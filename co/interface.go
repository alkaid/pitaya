package co

import "context"

type AwaitFun func(ctx context.Context, co Coroutine) (any, error)
type AsyncFun func(ctx context.Context, co Coroutine)

type Coroutine interface {
	AsyncIO
	Await(awaitFun AwaitFun) (any, error)
	// GetGoroutineID 获取现所在线程ID
	//  @return int
	GetGoroutineID() int
	// GetID 获取自己的ID
	//  @return int
	GetID() int
}

type AsyncIO interface {
	Async(ctx context.Context, exec AsyncFun)
}
