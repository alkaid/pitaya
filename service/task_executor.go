package service

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/route"
	"go.uber.org/zap"
)

var (
	// 下面几个内部别名用于在 result 中描述具体的执行器违约行为，同时保留公开错误供调用方 errors.Is。
	errTaskExecutorDidNotComplete        = component.ErrTaskNotInvoked
	errTaskExecutorReturnedEarly         = component.ErrTaskExecutorReturnedEarly
	errTaskExecutorFailedAfterCompletion = errors.New("pitaya/task executor returned an error after completing task")
	errTaskExecutorInvokedMoreThanOnce   = component.ErrTaskAlreadyInvoked
)

// taskInvocation 的正常状态转换为：
//
//	Pending -> Running -> Completed -> CompletedClosed
//
// Execute 未调用 task 就返回时：Pending -> Closed。
// Execute 在 task 运行中提前返回时：Running -> RunningClosed -> CompletedClosed。
// Closed 后不再接受新的调用；带 Closed 后缀的状态也让延迟调用可以和普通重复调用区分开。
const (
	taskInvocationPending         uint32 = iota // task 尚未被任何调用者领取；也是 atomic.Uint32 的零值
	taskInvocationRunning                       // 唯一合法调用者已经领取 task，真实组件任务正在执行
	taskInvocationCompleted                     // 真实组件任务已执行完成，但 executeTask 尚未封闭调用窗口
	taskInvocationClosed                        // Execute 未执行 task 就已返回，调用窗口已关闭
	taskInvocationRunningClosed                 // Execute 已提前返回，但已开始的 task 仍在收敛
	taskInvocationCompletedClosed               // task 已完成，且调用窗口已关闭
)

// taskInvocation 是框架包在 component.Task 外的一次性调用门。
// 它把执行器控制的调度过程与 dispatcher 持有的 Handler 返回值隔离开。
type taskInvocation struct {
	state atomic.Uint32 // 上述 taskInvocation* 状态；CAS 保证只有一个调用者能进入真实任务
	// rejected 记录第一次非法调用：0 表示没有；1 表示重复调用；2 表示关闭后的延迟调用。
	// 即使执行器忽略 invoke 返回的错误，executeTask 仍能发现执行期间发生过重复调用。
	rejected atomic.Uint32
	// done 初始计数为 1，只由成功领取任务的调用者 Done。
	// 当 Execute 提前返回时，executeTask 用它等待真实任务结束并建立 happens-before。
	done       sync.WaitGroup
	defaultCtx context.Context // 执行器给 task 传 nil context 时使用原始请求 context
	task       component.Task  // dispatcher 提供的真实组件任务
	taskErr    error           // 真实任务的执行错误；只由唯一合法调用者写入，收敛后由 executeTask 读取
}

// uninvokedTaskError 表示执行器在从未启动 task 的前提下返回了错误。
// skipped 单独保存“这是一次明确跳过”的分类，防止任务已经开始后返回的同名错误被误判为跳过。
type uninvokedTaskError struct {
	cause   error
	skipped bool
}

func (e uninvokedTaskError) Error() string {
	return e.cause.Error()
}

func (e uninvokedTaskError) Unwrap() error {
	return e.cause
}

func newTaskInvocation(ctx context.Context, task component.Task) *taskInvocation {
	invocation := &taskInvocation{defaultCtx: ctx, task: task}
	// 计数只对应唯一一次可能成功的 task 调用。未调用 task 的路径不会 Wait，因此无需 Done。
	invocation.done.Add(1)
	return invocation
}

// invoke 是实际交给自定义 TaskExecutor 的受控任务入口。
// 执行器可以同步、异步或并发调用它，但只有第一次 Pending -> Running 的 CAS 能执行真实任务。
func (i *taskInvocation) invoke(ctx context.Context) error {
	// 先原子领取执行权。CAS 失败说明状态刚被其他 goroutine 改变，需要重新读取后再分类。
	for {
		state := i.state.Load()
		if state == taskInvocationPending {
			if !i.state.CompareAndSwap(taskInvocationPending, taskInvocationRunning) {
				continue
			}
			break
		}

		rejection := component.ErrTaskAlreadyInvoked
		rejectionState := uint32(1)
		if state == taskInvocationClosed || state == taskInvocationRunningClosed || state == taskInvocationCompletedClosed {
			rejection = component.ErrTaskInvocationClosed
			rejectionState = 2
		}
		// 每次非法调用都会得到明确错误，但同一次 invocation 最多记录一条警告，避免错误执行器放大日志。
		if i.rejected.CompareAndSwap(0, rejectionState) {
			logger.Zap.Warn(
				"pitaya/task executor rejected duplicate or late invocation",
				zap.Uint32("state", state),
				zap.Error(rejection),
			)
		}
		return rejection
	}

	// 无论真实任务正常返回还是 panic 被转换成 error，都必须发布完成状态并唤醒等待方。
	defer func() {
		for {
			state := i.state.Load()
			completedState := uint32(taskInvocationCompleted)
			// executeTask 可能已经观察到 Running 并把调用窗口封闭，此时保留 Closed 标记。
			if state == taskInvocationRunningClosed {
				completedState = taskInvocationCompletedClosed
			}
			if i.state.CompareAndSwap(state, completedState) {
				break
			}
		}
		// taskErr 的写入发生在 Done 之前；等待方返回后可以安全读取它。
		i.done.Done()
	}()

	// nil context 不代表丢弃请求上下文，而是沿用调用 executeTask 时保存的默认值。
	if ctx == nil {
		ctx = i.defaultCtx
	}
	// invokeComponentTask 把 task panic 转成 error。这里返回 nil 只表示“本次 invoke 被接受”；
	// taskErr 由外层统一汇总，避免执行器把 Handler 错误误当成调度失败。
	i.taskErr = invokeComponentTask(ctx, i.task)
	return nil
}

// executeTask 执行组件任务并检查自定义 TaskExecutor 是否遵守一次性完成协议。
func executeTask(ctx context.Context, executor component.TaskExecutor, task component.Task) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// 未配置执行器时保留原来的直接调用热路径：没有 invocation 分配、atomic 或 WaitGroup。
	if executor == nil {
		return task(ctx)
	}

	invocation := newTaskInvocation(ctx, task)
	// Execute 在当前 goroutine 中运行；其中的 panic 会被转换为 ErrTaskExecutorPanic。
	executorErr := invokeTaskExecutor(ctx, executor, invocation.invoke)

	// Execute 已经返回。下面根据 task 的实际进度封闭调用窗口，并生成最终协议结果。
	for {
		switch invocation.state.Load() {
		case taskInvocationPending:
			// 执行器完全没有调用 task。先关闭入口，阻止它在返回后再异步补调。
			if !invocation.state.CompareAndSwap(taskInvocationPending, taskInvocationClosed) {
				continue
			}
			if executorErr != nil {
				// 只有 task 从未启动时，执行器错误才可能被解释为合法跳过或队列提交失败。
				return uninvokedTaskError{
					cause:   executorErr,
					skipped: isExplicitTaskSkip(executorErr),
				}
			}
			return errTaskExecutorDidNotComplete

		case taskInvocationRunning:
			// Execute 在 task 完成前返回。封闭入口后必须等待，避免 Handler 继续竞写 resp/err/params。
			if !invocation.state.CompareAndSwap(taskInvocationRunning, taskInvocationRunningClosed) {
				continue
			}
			invocation.done.Wait()
			return invocation.result(executorErr, true)

		case taskInvocationCompleted:
			// 正常完成路径。封闭入口后，执行器保存的 task 即使稍后被调用也只会得到 Closed 错误。
			if !invocation.state.CompareAndSwap(taskInvocationCompleted, taskInvocationCompletedClosed) {
				continue
			}
			return invocation.result(executorErr, false)

		case taskInvocationRunningClosed, taskInvocationCompletedClosed:
			// 防御并发状态变化：确保已开始的任务真正结束，再读取 taskErr 并汇总结果。
			invocation.done.Wait()
			return invocation.result(executorErr, true)

		default:
			return errTaskExecutorDidNotComplete
		}
	}
}

// result 把任务错误、执行器错误和协议违约合并为一个可被 errors.Is/errors.As 检查的错误。
func (i *taskInvocation) result(executorErr error, returnedEarly bool) error {
	// 最常见的成功路径不构造切片，也不调用 errors.Join。
	if !returnedEarly && i.rejected.Load() == 0 && executorErr == nil && i.taskErr == nil {
		return nil
	}
	result := make([]error, 0, 4)
	if returnedEarly {
		result = append(result, errTaskExecutorReturnedEarly)
	}
	if i.rejected.Load() == 1 {
		result = append(result, errTaskExecutorInvokedMoreThanOnce)
	}
	if executorErr != nil {
		if !returnedEarly {
			result = append(result, errTaskExecutorFailedAfterCompletion)
		}
		result = append(result, executorErr)
	}
	if i.taskErr != nil {
		result = append(result, i.taskErr)
	}
	return errors.Join(result...)
}

// invokeTaskExecutor 隔离自定义执行器的 panic，避免扩散到 RPC dispatcher 并终止服务进程。
func invokeTaskExecutor(ctx context.Context, executor component.TaskExecutor, task component.Task) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Zap.Error(
				"panic in pitaya task executor",
				zap.Any("panic", recovered),
				zap.ByteString("stack", debug.Stack()),
			)
			err = fmt.Errorf("%w: %v", component.ErrTaskExecutorPanic, recovered)
		}
	}()
	return executor.Execute(ctx, task)
}

// invokeComponentTask 隔离组件任务的 panic。Handler 的普通业务错误不在这里格式化，仍交给上层处理。
func invokeComponentTask(ctx context.Context, task func(context.Context) error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Zap.Error(
				"panic in pitaya component task",
				zap.Any("panic", recovered),
				zap.ByteString("stack", debug.Stack()),
			)
			err = fmt.Errorf("%w: %v", component.ErrTaskPanic, recovered)
		}
	}()
	return task(ctx)
}

// acknowledgeSkippedTask 判断一次未执行的 Notify 是否可以直接确认。
// Request 必须把跳过映射成响应错误；Notify 没有业务响应体，因此记录日志并返回 ack，避免 NATS request 超时。
func acknowledgeSkippedTask(msgType interface{}, executionErr error, rt *route.Route, dispatch string) bool {
	if !isUninvokedTaskSkip(executionErr) {
		return false
	}
	typ, err := getMsgType(msgType)
	if err != nil || typ != message.Notify {
		return false
	}
	logger.Zap.Info(
		"pitaya/task executor skipped notify",
		zap.String("route", rt.Short()),
		zap.String("dispatch", dispatch),
	)
	return true
}

// normalizeTaskExecutionError 把内部执行器错误收敛为稳定的对外状态，避免泄露队列或 panic 细节：
// 明确跳过为 503，任务启动前的取消为 499，超时为 504，其余协议错误统一为 500。
func normalizeTaskExecutionError(executionErr error) error {
	if isUninvokedTaskSkip(executionErr) {
		return apierrors.ServiceUnavailable("task_skipped", "component task skipped", "").WithCause(executionErr)
	}

	var uninvoked uninvokedTaskError
	if errors.As(executionErr, &uninvoked) {
		switch {
		case isExplicitTaskAPIError(uninvoked.cause):
			return executionErr
		case errors.Is(uninvoked.cause, context.Canceled):
			return apierrors.ClientClosed("task_canceled", "component task canceled", "").WithCause(executionErr)
		case errors.Is(uninvoked.cause, context.DeadlineExceeded):
			return apierrors.GatewayTimeout("task_timeout", "component task timed out", "").WithCause(executionErr)
		}
	}

	return apierrors.InternalServer("task_executor_failed", "component task execution failed", "").WithCause(executionErr)
}

// isUninvokedTaskSkip 只接受“task 从未启动”这一事实已经由 uninvokedTaskError 固定的跳过。
func isUninvokedTaskSkip(err error) bool {
	var uninvoked uninvokedTaskError
	return errors.As(err, &uninvoked) && uninvoked.skipped
}

// isExplicitTaskSkip 沿错误包装链识别执行器明确返回的 ErrTaskSkipped。
func isExplicitTaskSkip(err error) bool {
	for err != nil {
		if err == component.ErrTaskSkipped {
			return true
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}

// isExplicitTaskAPIError 只保留执行器在任务启动前明确返回的单一业务 API 错误。
// 多错误 Join 不视为明确业务状态，避免夹带的内部错误被错误地暴露到 wire response。
func isExplicitTaskAPIError(err error) bool {
	for err != nil {
		if _, ok := err.(*apierrors.Error); ok {
			return true
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}
