package component

import (
	"context"
	"errors"
)

var (
	// ErrTaskSkipped 表示执行器有意跳过本次任务，并且从未调用 task。
	// 典型场景是任务对应的业务对象已经销毁，不再需要执行该请求。
	ErrTaskSkipped = errors.New("pitaya/component: task skipped")
	// ErrTaskNotInvoked 表示执行器返回 nil，却没有调用 task。
	// 这属于执行器违反协议，而不是一次合法的任务跳过。
	ErrTaskNotInvoked = errors.New("pitaya/component: task was not invoked")
	// ErrTaskAlreadyInvoked 表示同一个 task 被调用了多次。
	// 框架只允许第一次调用进入真实 Handler，后续调用都会被拒绝。
	ErrTaskAlreadyInvoked = errors.New("pitaya/component: task already invoked")
	// ErrTaskInvocationClosed 表示 TaskExecutor.Execute 已返回后，又尝试调用之前保留的 task。
	// 此时请求的返回值可能已经交给后续流程，因此框架不会再执行真实 Handler。
	ErrTaskInvocationClosed = errors.New("pitaya/component: task invocation closed")
	// ErrTaskExecutorReturnedEarly 表示 task 尚未完成时，TaskExecutor.Execute 就提前返回。
	// 框架仍会等待已开始的 task 收敛，但会把这次执行标记为协议错误。
	ErrTaskExecutorReturnedEarly = errors.New("pitaya/component: task executor returned before task completed")
	// ErrTaskExecutorPanic 表示自定义执行器自身发生 panic。
	ErrTaskExecutorPanic = errors.New("pitaya/component: task executor panic")
	// ErrTaskPanic 表示执行器调用 task 时，框架封装的组件任务发生 panic。
	ErrTaskPanic = errors.New("pitaya/component: task panic")
)

// Task 是由框架持有实际执行权的组件调用入口。
//
// 执行器最多只能调用一次 Task，也不能在 Execute 返回后保存并延迟调用它。
// Task 的返回值只说明“这次调用是否被框架接受”；Handler 的业务错误仍由
// dispatcher 通过自己的返回值处理，不应被执行器解释为队列提交失败。
type Task func(ctx context.Context) error

// TaskExecutor 决定组件任务在哪个执行上下文中运行，例如用户串行队列或房间队列。
//
// 同一个 TaskExecutor 可能被多个请求并发调用，实现方必须自行保护内部状态。
// Execute 返回前必须满足以下二选一契约：
//   - 调用 task 恰好一次，并等待该调用完整结束；
//   - 完全不调用 task，返回 ErrTaskSkipped 或明确的队列提交错误。
//
// Execute 调用 task 后应原样返回 task 的错误。框架会额外校验重复调用、延迟调用
// 和提前返回，即使执行器忽略了 task 的错误，也不能绕过这些检查。
type TaskExecutor interface {
	Execute(ctx context.Context, task Task) error
}

// TaskExecutorFunc 把普通函数适配为 TaskExecutor，便于在注册组件时直接传入闭包。
type TaskExecutorFunc func(ctx context.Context, task Task) error

// Execute 实现 TaskExecutor，并把调用直接转发给被适配的函数。
func (f TaskExecutorFunc) Execute(ctx context.Context, task Task) error {
	return f(ctx, task)
}
