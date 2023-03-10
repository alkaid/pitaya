package co

import (
	"context"
	"errors"

	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"go.uber.org/zap"
)

// loopInstance 全局默认 Looper
//  @return Asyncio
// func loopInstance() Asyncio {
// 	return holder.pitayaSingleLooper
// }

//
// // Async 全局 Looper 单例的 Looper.Async
// //  @param ctx 请使用调用链中已存在的Context传入,请勿自行生成
// //  @param task
// //  @return Waiter
// func Async(ctx context.Context, task TaskFun) Waiter {
// 	return loopInstance().Async(ctx, task)
// }
//
// // Wait [T any]  Waiter.Wait 的泛型包装
// //  @param ctx 请使用调用链中已存在的Context传入,请勿自行生成
// //  @param task
// //  @return T
// //  @return error
// func Wait[T any](ctx context.Context, task TaskFunT[T]) (T, error) {
// 	w := loopInstance().Async(ctx, func(ctx context.Context, coroutine Coroutine) (any, error) {
// 		return task(ctx)
// 	})
// 	var zero T
// 	if w == nil {
// 		err := constants.ErrLooperAsyncInCoroutine
// 		return zero, err
// 	}
// 	ret, err := w.Wait(ctx)
// 	if err != nil {
// 		return zero, err
// 	}
// 	result, ok := ret.(T)
// 	if !ok {
// 		return zero, constants.ErrConvertGenericType
// 	}
// 	return result, err
// }

// Await [T any] Coroutine.Await 的泛型包装,所属 Coroutine 从上下文获得
//
//	@param ctx 请使用调用链中已存在的Context传入,请勿自行生成
//	@param task
//	@return T
//	@return error
func Await[T any](ctx context.Context, task TaskFunT[T]) (T, error) {
	co := getCoFromCtx(ctx)
	var zero T
	// 不支持当前环境内无协程
	if co == nil {
		err := errors.New("don't call Await() without coroutine")
		logger.Zap.Error("", zap.Error(err))
		return zero, err
	}
	ret, err := co.Await(ctx, func(ctx context.Context, coroutine Coroutine) (any, error) {
		return task(ctx)
	})
	if err != nil {
		return zero, err
	}
	result, ok := ret.(T)
	if !ok {
		return zero, constants.ErrConvertGenericType
	}
	return result, err
}

// AwaitGo [T any] Coroutine.AwaitGo 的泛型包装,所属 Coroutine 从上下文获得
//
//	@param ctx 请使用调用链中已存在的Context传入,请勿自行生成
//	@param task
//	@return T
//	@return error
func AwaitGo[T any](ctx context.Context, task TaskFunT[T]) (T, error) {
	co := getCoFromCtx(ctx)
	var zero T
	// 不支持当前环境内无协程
	if co == nil {
		err := errors.New("don't call AwaitGo() without coroutine")
		logger.Zap.Error("", zap.Error(err))
		return zero, err
	}
	ret, err := co.AwaitGo(ctx, func(ctx context.Context, coroutine Coroutine) (any, error) {
		return task(ctx)
	})
	if err != nil {
		return zero, err
	}
	result, ok := ret.(T)
	if !ok {
		return zero, constants.ErrConvertGenericType
	}
	return result, err
}

// AwaitGoMulti Coroutine.AwaitGoMulti 的包装
//
//	@param ctx 请使用调用链中已存在的Context传入,请勿自行生成
//	@param tasks
//	@return []*Result
//	@return error
func AwaitGoMulti(ctx context.Context, tasks ...TaskFun) ([]*Result, error) {
	co := getCoFromCtx(ctx)
	// 不支持当前环境内无协程
	if co == nil {
		err := errors.New("don't call AwaitGo() without coroutine")
		logger.Zap.Error("", zap.Error(err))
		return nil, err
	}
	return co.AwaitGoMulti(ctx, tasks...)
}
