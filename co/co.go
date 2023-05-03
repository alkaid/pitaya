// Package co 线程池
package co

import (
	"context"

	"github.com/panjf2000/ants/v2"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/util"
	"go.uber.org/zap"
)

// GoWithPool 根据指定的线程池及goroutineID派发线程
//
//	@param ctx
//	@param poolName 指定线程池
//	@param goID 线程ID
//	@param task
func GoWithPool[T int | int32 | int64](ctx context.Context, poolName string, goID T, task func(ctx context.Context)) (done chan struct{}) {
	return instance.pools[poolName].Go(ctx, int(goID), task)
}

// WaitWithPool 根据指定的线程池及goroutineID派发线程并阻塞等待
//
//	@param ctx
//	@param poolName 指定线程池
//	@param goID 线程ID
//	@param task
func WaitWithPool[T int | int32 | int64](ctx context.Context, poolName string, goID T, task func(ctx context.Context)) {
	instance.pools[poolName].Wait(ctx, int(goID), task)
}

// GoWithID 根据指定goroutineID派发到默认线程池
//
//	@param ctx
//	@param goID 线程ID
//	@param task
func GoWithID[T int | int32 | int64](ctx context.Context, goID T, task func(ctx context.Context)) (done chan struct{}) {
	return instance.pools[DefaultGoPoolName].Go(ctx, int(goID), task)
}

// WaitWithID 根据指定的goroutineID派发到默认线程池并阻塞等待
//
//	@param ctx
//	@param goID 线程ID
//	@param task
func WaitWithID[T int | int32 | int64](ctx context.Context, goID T, task func(ctx context.Context)) {
	instance.pools[DefaultGoPoolName].Wait(ctx, int(goID), task)
}

// GoWithUser 派发到用户线程
//
//	@param ctx
//	@param uid
//	@param task
func GoWithUser(ctx context.Context, uid int64, task func(ctx context.Context)) (done chan struct{}) {
	if uid <= 0 {
		util.GetLoggerFromCtx(ctx).Error("uid invalid", zap.Int64("uid", uid))
		return
	}
	return instance.pools[UserGoPoolName].Go(ctx, int(uid), task)
}

// WaitWithUser 派发到用户线程并阻塞等待
//
//	@param ctx
//	@param uid
//	@param task
func WaitWithUser(ctx context.Context, uid int64, task func(ctx context.Context)) {
	if uid <= 0 {
		util.GetLoggerFromCtx(ctx).Error("uid invalid", zap.Int64("uid", uid))
		return
	}
	instance.pools[UserGoPoolName].Wait(ctx, int(uid), task)
}

// GoMain 根据派发到默认线程池的主线程,线程id为 MainThreadID
//
//	@param ctx
//	@param goID 线程ID
//	@param task
func GoMain(ctx context.Context, task func(ctx context.Context)) (done chan struct{}) {
	return instance.pools[DefaultGoPoolName].Go(ctx, MainThreadID, task)
}

// WaitMain 派发到默认线程池的主线程并阻塞等待,线程id为 MainThreadID
//
//	@param ctx
//	@param goID 线程ID
//	@param task
func WaitMain(ctx context.Context, task func(ctx context.Context)) {
	instance.pools[DefaultGoPoolName].Wait(ctx, MainThreadID, task)
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
