package co

import (
	"context"
	"errors"
	"sync"

	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"go.uber.org/zap"
)

type taskSignal int

const (
	taskSignal_None taskSignal = iota
	taskSignal_Resume
)

// CoroutineStatus 协程状态
type CoroutineStatus int32

const (
	CoroutineStatusDead CoroutineStatus = iota
	CoroutineStatusReady
	CoroutineStatusRunning
	CoroutineStatusSuspend
)

// CoroutineImpl 协程,只在所属 Looper 的单线程里执行
//  @implement Coroutine
type CoroutineImpl struct {
	id       int
	ctx      context.Context
	chResume chan struct{}

	executor     TaskFun
	futureResult chan any
	futureErr    chan error

	result      any
	err         error
	looper      *Looper
	parent      *CoroutineImpl
	status      CoroutineStatus
	statusGuard sync.Mutex
}

// GetGoroutineID
//  @implement Coroutine.GetGoroutineID
//  @receiver c
//  @return int
func (c *CoroutineImpl) GetGoroutineID() int {
	return c.looper.id
}

// GetID
//  @implement Coroutine.GetID
//  @receiver c
//  @return int
func (c *CoroutineImpl) GetID() int {
	return c.id
}

// yield 挂起 交出控制权
//  @receiver c
func (c *CoroutineImpl) yield() {
	logger.Zap.Debug("co yield", zap.Int("coid", c.id))
	// 通知主线程调度 本协程挂起
	c.setStatus(CoroutineStatusSuspend)
	c.looper.chResume <- struct{}{}
	// 等待重新投递的自己被调度或子协程完成来通知恢复
	<-c.chResume
	c.setStatus(CoroutineStatusRunning)
	logger.Zap.Debug("co yield resume", zap.Int("coid", c.id))
}

// resume 尝试恢复,重新投递. 注意只能在goroutine中执行,请勿在coroutine中执行,参看 AwaitGo 中的调用
//  @receiver c
func (c *CoroutineImpl) resume() {
	logger.Zap.Debug("co resume", zap.Int("coid", c.id))
	// 恢复以后重新投递
	c.setStatus(CoroutineStatusSuspend)
	c.looper.unhandleCoroutine <- c
}

// safeExecTask 安全执行子任务,
//  @receiver c
//  @return ret
//  @return err
func (c *CoroutineImpl) safeExecTask() (ret any, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = errors.New("panic when coroutine run")
			logger.Zap.Error(err.Error(), zap.Any("panic", e))
		}
	}()
	ret, err = c.executor(c.ctx, c)
	return
}

func (c *CoroutineImpl) run() {

	c.setStatus(CoroutineStatusRunning)
	c.result, c.err = c.safeExecTask()
	if len(c.futureResult) == 0 {
		c.futureResult <- c.result
	}
	if len(c.futureErr) == 0 {
		c.futureErr <- c.err
	}
	c.setStatus(CoroutineStatusDead)
	// 通知主线程恢复
	if c.parent == nil {
		c.looper.chResume <- struct{}{}
	} else {
		// 有父协程 则应该通知父协程恢复
		c.parent.chResume <- struct{}{}
	}
}

// Wait
//  @implement Waiter.Wait
//  @receiver c
//  @param ctx
//  @return any
//  @return error
func (c *CoroutineImpl) Wait(ctx context.Context) (any, error) {
	currCo := getCoFromCtx(ctx)
	if currCo != c.parent {
		// err := errors.New("don't call Wait() outside coroutine's parent")
		err := errors.New("don't call Wait() inside coroutine")
		logger.Zap.Error("", zap.Error(err))
		return nil, err
	}
	if c.GetStatus() != CoroutineStatusDead {
		return <-c.futureResult, <-c.futureErr
	}
	return c.result, c.err
}

// Await
//  @implement Coroutine.Await
//  @receiver c
//  @param ctx
//  @param task
//  @return any
//  @return error
func (c *CoroutineImpl) Await(ctx context.Context, task TaskFun) (any, error) {
	// 只允许在coroutine自己task内部调用
	currCo := getCoFromCtx(ctx)
	if currCo != c {
		err := errors.New("must call Await() in coroutine self's task(TaskFun),otherwise maybe deadlock")
		logger.Zap.Error("", zap.Error(err))
		return nil, err
	}
	if c.GetStatus() != CoroutineStatusRunning {
		err := errors.New("can't call Await() outside coroutine's task(TaskFun),otherwise maybe deadlock")
		logger.Zap.Error("", zap.Error(err))
		return nil, err
	}
	// 线程内投递子任务
	var child *CoroutineImpl
	child = c.looper.async(c.ctx, c, task).(*CoroutineImpl)
	// 挂起等待子协程完成通知
	c.yield()
	return child.Wait(ctx)

}

// AwaitGo
//  @implement Coroutine.AwaitGo
//  @receiver c
//  @param ctx
//  @param task
//  @return any
//  @return error
func (c *CoroutineImpl) AwaitGo(ctx context.Context, task TaskFun) (any, error) {
	// 只允许在coroutine自己task内部调用
	currCo := getCoFromCtx(ctx)
	if currCo != c {
		err := errors.New("must call AwaitGo() in coroutine self's task(TaskFun),otherwise maybe deadlock")
		logger.Zap.Error("", zap.Error(err))
		return nil, err
	}
	if c.GetStatus() != CoroutineStatusRunning {
		err := errors.New("don't call AwaitGo() outside coroutine's task(TaskFun),otherwise maybe deadlock")
		logger.Zap.Warn("", zap.Error(err))
		return nil, err
	}
	var result any
	var err error
	Go(func() {
		result, err = task(c.ctx, c)
		// 耗时任务完成
		c.resume()
	})
	c.yield()
	return result, err
}

type Result struct {
	data any
	err  error
}

// AwaitGoMulti
//  @implement Coroutine.AwaitGoMulti
//  @receiver c
//  @param ctx
//  @param tasks
//  @return []*Result
//  @return error
func (c *CoroutineImpl) AwaitGoMulti(ctx context.Context, tasks ...TaskFun) ([]*Result, error) {
	// 只允许在coroutine自己task内部调用
	currCo := getCoFromCtx(ctx)
	if currCo != c {
		err := errors.New("must call AwaitGo() in coroutine self's tasks(TaskFun),otherwise maybe deadlock")
		logger.Zap.Error("", zap.Error(err))
		return nil, err
	}
	if c.GetStatus() != CoroutineStatusRunning {
		err := errors.New("don't call AwaitGo() outside coroutine's tasks(TaskFun),otherwise maybe deadlock")
		logger.Zap.Warn("", zap.Error(err))
		return nil, err
	}
	var rets = make([]*Result, len(tasks), len(tasks))
	wg := &sync.WaitGroup{}
	for i, t := range tasks {
		wg.Add(1)
		i := i
		t := t
		Go(func() {
			rets[i].data, rets[i].err = t(c.ctx, c)
			wg.Done()
		})

	}
	c.yield()
	wg.Wait()
	return rets, nil
}

func (c *CoroutineImpl) setStatus(status CoroutineStatus) {
	c.statusGuard.Lock()
	c.status = status
	c.statusGuard.Unlock()
}

// GetStatus
//  @implement Coroutine.GetStatus
//  @receiver c
//  @return CoroutineStatus
func (c *CoroutineImpl) GetStatus() CoroutineStatus {
	c.statusGuard.Lock()
	defer c.statusGuard.Unlock()
	return c.status
}

// newCoroutine 创建一个新协程并投递到 Looper 中
//  @param ctx
//  @param sch
//  @param parentCo
//  @param task
//  @return *CoroutineImpl
func newCoroutine(ctx context.Context, sch *Looper, parentCo *CoroutineImpl, task TaskFun) *CoroutineImpl {
	self := &CoroutineImpl{
		id:           sch.coroutineAutoID,
		chResume:     make(chan struct{}),
		ctx:          ctx,
		looper:       sch,
		executor:     task,
		futureResult: make(chan any, 1),
		futureErr:    make(chan error, 1),
		parent:       parentCo,
		status:       CoroutineStatusReady,
	}
	self.ctx = context.WithValue(ctx, constants.CoroutineCtxKey, self)
	sch.unhandleCoroutine <- self
	return self
}
