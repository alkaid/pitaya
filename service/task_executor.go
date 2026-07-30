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
	errTaskExecutorDidNotComplete        = component.ErrTaskNotInvoked
	errTaskExecutorReturnedEarly         = component.ErrTaskExecutorReturnedEarly
	errTaskExecutorFailedAfterCompletion = errors.New("pitaya/task executor returned an error after completing task")
	errTaskExecutorInvokedMoreThanOnce   = component.ErrTaskAlreadyInvoked
)

const (
	taskInvocationPending uint32 = iota
	taskInvocationRunning
	taskInvocationCompleted
	taskInvocationClosed
	taskInvocationRunningClosed
	taskInvocationCompletedClosed
)

type taskInvocation struct {
	state      atomic.Uint32
	rejected   atomic.Uint32
	done       sync.WaitGroup
	defaultCtx context.Context
	task       component.Task
	taskErr    error
}

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
	invocation.done.Add(1)
	return invocation
}

func (i *taskInvocation) invoke(ctx context.Context) error {
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
		if i.rejected.CompareAndSwap(0, rejectionState) {
			logger.Zap.Warn(
				"pitaya/task executor rejected duplicate or late invocation",
				zap.Uint32("state", state),
				zap.Error(rejection),
			)
		}
		return rejection
	}

	defer func() {
		for {
			state := i.state.Load()
			completedState := uint32(taskInvocationCompleted)
			if state == taskInvocationRunningClosed {
				completedState = taskInvocationCompletedClosed
			}
			if i.state.CompareAndSwap(state, completedState) {
				break
			}
		}
		i.done.Done()
	}()

	if ctx == nil {
		ctx = i.defaultCtx
	}
	i.taskErr = invokeComponentTask(ctx, i.task)
	return nil
}

func executeTask(ctx context.Context, executor component.TaskExecutor, task component.Task) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if executor == nil {
		return task(ctx)
	}

	invocation := newTaskInvocation(ctx, task)
	executorErr := invokeTaskExecutor(ctx, executor, invocation.invoke)

	for {
		switch invocation.state.Load() {
		case taskInvocationPending:
			if !invocation.state.CompareAndSwap(taskInvocationPending, taskInvocationClosed) {
				continue
			}
			if executorErr != nil {
				return uninvokedTaskError{
					cause:   executorErr,
					skipped: isExplicitTaskSkip(executorErr),
				}
			}
			return errTaskExecutorDidNotComplete

		case taskInvocationRunning:
			if !invocation.state.CompareAndSwap(taskInvocationRunning, taskInvocationRunningClosed) {
				continue
			}
			invocation.done.Wait()
			return invocation.result(executorErr, true)

		case taskInvocationCompleted:
			if !invocation.state.CompareAndSwap(taskInvocationCompleted, taskInvocationCompletedClosed) {
				continue
			}
			return invocation.result(executorErr, false)

		case taskInvocationRunningClosed, taskInvocationCompletedClosed:
			invocation.done.Wait()
			return invocation.result(executorErr, true)

		default:
			return errTaskExecutorDidNotComplete
		}
	}
}

func (i *taskInvocation) result(executorErr error, returnedEarly bool) error {
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

func isUninvokedTaskSkip(err error) bool {
	var uninvoked uninvokedTaskError
	return errors.As(err, &uninvoked) && uninvoked.skipped
}

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
