// Package co 线程池
package co

import (
	"context"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/goerrors/errors"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/util"
	"go.uber.org/zap"
)

// ErrInvalidUserID is returned when a user-pool API receives a non-positive ID.
var ErrInvalidUserID = errors.New("user id must be greater than zero")

// GoPersist 派发到常驻线程池 PersistGoPoolName
//
//	@template [T int | int32 | int64]
//	@param ctx
//	@param goID
//	@param task
//	@param opts
//	@return done
//	@return error
func GoPersist[T int | int32 | int64](ctx context.Context, goID T, task func(ctx context.Context), opts ...Option) (<-chan struct{}, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	return instance.pools[PersistGoPoolName].Go(ctx, int(goID), task, o.disableTimeoutWatch)
}

// GoWithID 派发任务到指定线程 若不指定线程池 WithPoolName, 则使用默认线程池 DefaultGoPoolName
//
//	@template [T int | int32 | int64]
//	@param ctx
//	@param goID
//	@param task
//	@param opts
//	@return done
//	@return error
func GoWithID[T int | int32 | int64](ctx context.Context, goID T, task func(ctx context.Context), opts ...Option) (<-chan struct{}, error) {
	o := &options{poolName: DefaultGoPoolName}
	for _, opt := range opts {
		opt(o)
	}
	return instance.pools[o.poolName].Go(ctx, int(goID), task, o.disableTimeoutWatch)
}

// WaitWithID 派发任务到指定线程并阻塞等待 若不指定线程池 WithPoolName, 则使用默认线程池 DefaultGoPoolName
//
//	@param ctx
//	@param goID
//	@param task
//	@param opts
//	@return error
func WaitWithID[T int | int32 | int64](ctx context.Context, goID T, task func(ctx context.Context), opts ...Option) error {
	o := &options{poolName: DefaultGoPoolName}
	for _, opt := range opts {
		opt(o)
	}
	return instance.pools[o.poolName].Wait(ctx, int(goID), task, o.disableTimeoutWatch)
}

// GoWithUser 派发到 UserGoPoolName 用户线程
//
//	@param ctx
//	@param uid
//	@param task
//	@return done
//	@return error
func GoWithUser(ctx context.Context, uid int64, task func(ctx context.Context)) (<-chan struct{}, error) {
	if uid <= 0 {
		ctx = normalizeContext(ctx)
		util.GetLoggerFromCtx(ctx).Error("uid invalid", zap.Int64("uid", uid))
		return nil, ErrInvalidUserID
	}
	return instance.pools[UserGoPoolName].Go(ctx, int(uid), task, false)
}

// WaitWithUser 派发到 UserGoPoolName 用户线程并阻塞等待
//
//	@param ctx
//	@param uid
//	@param task
//	@return error
func WaitWithUser(ctx context.Context, uid int64, task func(ctx context.Context)) error {
	if uid <= 0 {
		ctx = normalizeContext(ctx)
		util.GetLoggerFromCtx(ctx).Error("uid invalid", zap.Int64("uid", uid))
		return ErrInvalidUserID
	}
	return instance.pools[UserGoPoolName].Wait(ctx, int(uid), task, false)
}

// GoMain 派发到 PersistGoPoolName 常驻线程池的主线程,线程id为 MainThreadID
//
//	@param ctx
//	@param goID 线程ID
//	@param task
//	@return done
//	@return error
func GoMain(ctx context.Context, task func(ctx context.Context), opts ...Option) (<-chan struct{}, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	return instance.pools[PersistGoPoolName].Go(ctx, MainThreadID, task, o.disableTimeoutWatch)
}

// WaitMain 派发到 PersistGoPoolName 常驻线程池的主线程并阻塞等待,线程id为 MainThreadID
//
//	@param ctx
//	@param goID 线程ID
//	@param task
//	@return error
func WaitMain(ctx context.Context, task func(ctx context.Context), opts ...Option) error {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	return instance.pools[PersistGoPoolName].Wait(ctx, MainThreadID, task, o.disableTimeoutWatch)
}

// Submit submits a task to the stateless pool and reports whether it was accepted.
func Submit(task func()) error {
	if task == nil {
		return ErrNilTask
	}
	err := ants.Submit(task)
	if err != nil {
		logger.Zap.Error("submit task error", zap.Error(err))
		return err
	}
	return nil
}

// Go 从无状态线程池获取一个goroutine并派发任务
//
//	@param task
func Go(task func()) {
	_ = Submit(task)
}
