package component

import (
	"context"
	"errors"
)

var (
	// ErrTaskSkipped reports that an executor intentionally did not run a task.
	ErrTaskSkipped = errors.New("pitaya/component: task skipped")
	// ErrTaskNotInvoked reports that an executor returned nil without running a task.
	ErrTaskNotInvoked = errors.New("pitaya/component: task was not invoked")
	// ErrTaskAlreadyInvoked reports a duplicate task invocation.
	ErrTaskAlreadyInvoked = errors.New("pitaya/component: task already invoked")
	// ErrTaskInvocationClosed reports a task invocation attempted after Execute returned.
	ErrTaskInvocationClosed = errors.New("pitaya/component: task invocation closed")
	// ErrTaskExecutorReturnedEarly reports that Execute returned while a task was running.
	ErrTaskExecutorReturnedEarly = errors.New("pitaya/component: task executor returned before task completed")
	// ErrTaskExecutorPanic reports a panic raised by an executor.
	ErrTaskExecutorPanic = errors.New("pitaya/component: task executor panic")
	// ErrTaskPanic reports a panic raised while a custom executor invokes a component task.
	ErrTaskPanic = errors.New("pitaya/component: task panic")
)

// Task is a framework-owned component invocation. An executor must call it at
// most once and must not retain it after Execute returns. Its error only reports
// that the invocation was rejected; handler errors remain owned by the dispatcher.
type Task func(ctx context.Context) error

// TaskExecutor controls where a component invocation runs. Execute may be
// called concurrently and implementations must synchronize their own state.
// Before returning, Execute must either run task exactly once to completion or
// return ErrTaskSkipped or a submission error without running task. It should
// propagate the error returned by task.
type TaskExecutor interface {
	Execute(ctx context.Context, task Task) error
}

// TaskExecutorFunc adapts a function to TaskExecutor.
type TaskExecutorFunc func(ctx context.Context, task Task) error

// Execute implements TaskExecutor.
func (f TaskExecutorFunc) Execute(ctx context.Context, task Task) error {
	return f(ctx, task)
}
