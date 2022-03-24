package co

import (
	"context"
	"sync"

	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"go.uber.org/zap"
)

type schedulerSignal int

const (
	scheduler_None schedulerSignal = iota
	scheduler_Yield
	scheduler_Finished
)

// Looper Coroutine 协程的调度管理单元. Looper 迭代过程中同一时刻只会有一个协程执行
type Looper struct {
	id                int
	unhandleCoroutine chan *CoroutineImpl
	donePromise       chan *CoroutineImpl
	runningNum        int
	runningGuard      sync.Mutex
	chResume          chan struct{}
	running           bool
	coroutineAutoID   int
}

func getCoFromCtx(ctx context.Context) *CoroutineImpl {
	co := ctx.Value(constants.CoroutineCtxKey)
	if co != nil {
		coimpl, ok := co.(*CoroutineImpl)
		if ok {
			return coimpl
		}
	}
	return nil
}
func (l *Looper) async(ctx context.Context, parentCo *CoroutineImpl, task TaskFun) Waiter {
	if !l.running {
		logger.Zap.Warn("promise looper has shutdown")
		return nil
	}
	l.runningGuard.Lock()
	if l.runningNum > cap(l.unhandleCoroutine) {
		logger.Zap.Warn("promise looper buffer full", zap.Int("nums", l.runningNum))
	}
	co := newCoroutine(ctx, l, parentCo, task)
	l.coroutineAutoID++
	l.runningNum++
	l.runningGuard.Unlock()
	return co
}

// Async
//  @implement Asyncio.Async
//  @receiver l
//  @param ctx
//  @param task
//  @return Waiter
func (l *Looper) Async(ctx context.Context, task TaskFun) Waiter {
	currCo := getCoFromCtx(ctx)
	if currCo != nil {
		err := constants.ErrLooperAsyncInCoroutine
		logger.Zap.Error("", zap.Error(err))
		return nil
	}
	return l.async(ctx, nil, task)
}

func (l *Looper) postCoroutine(t *CoroutineImpl) {
	l.unhandleCoroutine <- t
}

func (l *Looper) runningCoroutineLen() int {
	l.runningGuard.Lock()
	defer l.runningGuard.Unlock()
	return l.runningNum
}

func (l *Looper) markFinished() {
	l.runningGuard.Lock()
	l.runningNum--
	l.runningGuard.Unlock()
}

func (l *Looper) pickCoroutine() *CoroutineImpl {
	return <-l.unhandleCoroutine
}

func (l *Looper) loop() {
	for {
		co := l.pickCoroutine()
		if co == nil {
			break
		}
		if co.GetStatus() == CoroutineStatusSuspend {
			// 通知任务线程恢复逻辑
			co.chResume <- struct{}{}
		} else {
			Go(func() {
				co.run()
				l.markFinished()
			})
		}
		// 等待完成或者挂起
		<-l.chResume
	}
}

func (l *Looper) Start() {
	l.running = true
	go l.loop()
}

func (l *Looper) Exit() {
	endChan := make(chan bool)
	go func() {
		for {
			l := l.runningCoroutineLen()
			if l == 0 {
				break
			}
		}
		endChan <- true
	}()
	<-endChan
}

func NewLooper(id int, coBuffer int) *Looper {
	return &Looper{
		id:                id,
		unhandleCoroutine: make(chan *CoroutineImpl, coBuffer),
		chResume:          make(chan struct{}),
	}
}
