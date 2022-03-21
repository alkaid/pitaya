package co

import (
	"context"
	"errors"
	"sync"

	"github.com/topfreegames/pitaya/v2/logger"
	"go.uber.org/zap"
)

type taskSignal int

const (
	taskSignal_None taskSignal = iota
	taskSignal_Resume
)

type CoroutineImpl struct {
	id         int
	ctx        context.Context
	signalChan chan taskSignal

	executor AsyncFun

	needResumeGuard sync.Mutex
	needResume      bool
	futureResult    any
	futureError     error
	pending         bool
	looper          *Looper
}

func (c *CoroutineImpl) GetGoroutineID() int {
	return c.looper.id
}
func (c *CoroutineImpl) GetID() int {
	return c.id
}

func (c *CoroutineImpl) safeCallAsync(task AsyncFun) {
	defer func() {
		if err := recover(); err != nil {
			logger.Zap.Error("Call coroutine async function panic", zap.Any("panic", err))
		}
	}()
	task(c.ctx, c)
}

func (c *CoroutineImpl) safeCallAwait(task AwaitFun) (ret any, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = errors.New("Call coroutine await function panic")
			logger.Zap.Error(err.Error(), zap.Any("panic", err))
		}
	}()
	return task(c.ctx, c)
}

// Async 在该 cointf.Coroutine 所属线程上继续调度新的执行逻辑
//  @receiver c
//  @param ctx
//  @param exec
func (c *CoroutineImpl) Async(ctx context.Context, exec AsyncFun) {
	c.looper.Async(ctx, exec)
}

func (c *CoroutineImpl) Await(task AwaitFun) (any, error) {
	// TODO 暂时不考虑并行 executor的情况
	// 并行执行耗时任务
	var futureResult any
	var futureError error
	Go(func() {
		if !c.pending {
			// 包装后重新投递
			var asyncExec AsyncFun = func(ctx context.Context, co Coroutine) {
				futureResult, futureError = co.Await(task)
				c.signal(taskSignal_Resume)
			}
			c.looper.Async(c.ctx, asyncExec)
			return
		}
		// TODO 考虑是要用 safeCallAwait 执行
		futureResult, futureError = task(c.ctx, c)

		// 耗时任务完成
		c.needResumeGuard.Lock()
		c.needResume = true
		c.needResumeGuard.Unlock()

		c.looper.postCoroutine(c)
	})

	if c.pending {
		// 通知调度 本线程挂起
		c.looper.signal(scheduler_Yield)
	}

	// 等待恢复逻辑
	<-c.signalChan
	return futureResult, futureError
}

func (c *CoroutineImpl) NeedResume() bool {
	c.needResumeGuard.Lock()
	defer c.needResumeGuard.Unlock()
	return c.needResume
}

func (c *CoroutineImpl) signal(s taskSignal) {
	c.signalChan <- s
}

func NewCoroutine(ctx context.Context, sch *Looper, task AsyncFun) *CoroutineImpl {
	self := &CoroutineImpl{
		signalChan: make(chan taskSignal),
		ctx:        ctx,
		looper:     sch,
		executor:   task,
		pending:    true,
	}

	return self
}
