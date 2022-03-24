package co

import "context"

type AwaitFun func(ctx context.Context, coroutine Coroutine) (any, error)
type AsyncFun func(ctx context.Context, coroutine Coroutine)
type TaskFun func(ctx context.Context, coroutine Coroutine) (any, error)
type TaskFunT[T any] func(ctx context.Context) (T, error)

// Coroutine 协程,只在所属 Looper 的单线程里执行
type Coroutine interface {
	// Await 阻塞等待新任务完成
	//  (和 AwaitGo 不同,新任务和当前任务在同一线程,主要用于有读写共享资源的任务以防止竞态问题)
	//  具体流程:在当前 Coroutine 所属的 Looper 中投递新任务(包装成新协程)并挂起等待,会交出控制权给 Looper 直到子task执行完成才恢复
	//  @param ctx 请使用调用链中已存在的Context传入,请勿自行生成
	//  @param task
	//  @return any
	//  @return error
	Await(ctx context.Context, task TaskFun) (any, error)
	// AwaitGo 阻塞等待新任务完成
	//	//  (和 Await 不同,新任务和当前任务不在同一线程,主要用于不可能有竞态问题的子任务,比如DB操作)
	//	//  具体流程:在当前 Coroutine 新起线程执行新任务并挂起等待,会交出控制权给 Looper 直到子task执行完成才恢复
	//  @param ctx 请使用调用链中已存在的Context传入,请勿自行生成
	//  @param task
	//  @return any
	//  @return error
	AwaitGo(ctx context.Context, task TaskFun) (any, error)
	// AwaitGoMulti AwaitGo 的多任务并行提交版本
	//  @param ctx 请使用调用链中已存在的Context传入,请勿自行生成
	//  @param tasks
	//  @return []*Result
	//  @return error
	AwaitGoMulti(ctx context.Context, tasks ...TaskFun) ([]*Result, error)
	// GetGoroutineID 获取所属 Looper 的线程ID
	//  @return int
	GetGoroutineID() int
	// GetID 获取协程ID
	//  @return int
	GetID() int
	// GetStatus 获取当前状态
	//  @return CoroutineStatus
	GetStatus() CoroutineStatus
}

// Waiter 可阻塞 Coroutine
type Waiter interface {
	// Wait 阻塞等待 谨慎使用,会阻塞当前线程或协程.(暂时禁止在协程中使用,会报错)
	//  @param ctx 请使用调用链中已存在的Context传入,请勿自行生成
	//  @return any
	//  @return error
	Wait(ctx context.Context) (any, error)
}

// Asyncio 单线程多协程调度管理者 Looper
type Asyncio interface {
	// Async 异步包装任务为 Coroutine 并投递到 Looper 里
	//  @param ctx 请使用调用链中已存在的Context传入,请勿自行生成
	//  @param task
	//  @return Waiter
	Async(ctx context.Context, task TaskFun) Waiter
}
