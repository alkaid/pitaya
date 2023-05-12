// Package co 线程池
package co

import (
	"context"

	"github.com/panjf2000/ants/v2"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/util"
	"go.uber.org/zap"
)

// GoPersist 派发到常驻线程池 PersistGoPoolName
//
//	@template [T int | int32 | int64]
//	@param ctx
//	@param goID
//	@param task
//	@param opts
//	@return done
func GoPersist[T int | int32 | int64](ctx context.Context, goID T, task func(ctx context.Context), opts ...Option) (done chan struct{}) {
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
func GoWithID[T int | int32 | int64](ctx context.Context, goID T, task func(ctx context.Context), opts ...Option) (done chan struct{}) {
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
func WaitWithID[T int | int32 | int64](ctx context.Context, goID T, task func(ctx context.Context), opts ...Option) {
	o := &options{poolName: DefaultGoPoolName}
	for _, opt := range opts {
		opt(o)
	}
	instance.pools[o.poolName].Wait(ctx, int(goID), task, o.disableTimeoutWatch)
}

// GoWithUser 派发到 UserGoPoolName 用户线程
//
//	@param ctx
//	@param uid
//	@param task
func GoWithUser(ctx context.Context, uid int64, task func(ctx context.Context)) (done chan struct{}) {
	if uid <= 0 {
		util.GetLoggerFromCtx(ctx).Error("uid invalid", zap.Int64("uid", uid))
		return
	}
	return instance.pools[UserGoPoolName].Go(ctx, int(uid), task, false)
}

// WaitWithUser 派发到 UserGoPoolName 用户线程并阻塞等待
//
//	@param ctx
//	@param uid
//	@param task
func WaitWithUser(ctx context.Context, uid int64, task func(ctx context.Context)) {
	if uid <= 0 {
		util.GetLoggerFromCtx(ctx).Error("uid invalid", zap.Int64("uid", uid))
		return
	}
	instance.pools[UserGoPoolName].Wait(ctx, int(uid), task, false)
}

// GoMain 派发到 PersistGoPoolName 常驻线程池的主线程,线程id为 MainThreadID
//
//	@param ctx
//	@param goID 线程ID
//	@param task
func GoMain(ctx context.Context, task func(ctx context.Context), opts ...Option) (done chan struct{}) {
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
func WaitMain(ctx context.Context, task func(ctx context.Context), opts ...Option) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	instance.pools[PersistGoPoolName].Wait(ctx, MainThreadID, task, o.disableTimeoutWatch)
}

// Go 从无状态线程池获取一个goroutine并派发任务
//
//	@param task
func Go(task func()) {
	err := ants.Submit(task)
	if err != nil {
		logger.Zap.Error("submit task error", zap.Error(err))
		return
	}
}
