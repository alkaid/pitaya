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

// Looper 事件循环体(Event looper).是 Coroutine 的调度管理单元.一个Looper所属的子协程 Coroutine 只在一个线程中运行
type Looper struct {
	id              int
	coroutineQueue  chan *CoroutineImpl
	runningNum      int
	runningGuard    sync.Mutex
	signalChan      chan schedulerSignal
	running         bool
	coroutineAutoID int
}

func (s *Looper) Async(ctx context.Context, task AsyncFun) {
	if !s.running {
		return
	}
	co := NewCoroutine(ctx, s, task)
	if s.runningNum > cap(s.coroutineQueue) {
		logger.Zap.Warn("coroutine buffer full", zap.Int("nums", s.runningNum))
	}
	s.runningGuard.Lock()
	co.id = s.coroutineAutoID
	s.coroutineAutoID++
	s.runningNum++
	s.runningGuard.Unlock()
	s.coroutineQueue <- co
}
func (s *Looper) Await(ctx context.Context, task AwaitFun) (any, error) {
	if !s.running {
		return nil, constants.ErrCloseClosedGroup
	}
	var futureResult any
	var futureError error
	done := make(chan struct{})
	Go(func() {
		// 包装后投递
		var asyncExec AsyncFun = func(ctx context.Context, co Coroutine) {
			futureResult, futureError = co.Await(task)
			close(done)
		}
		s.Async(ctx, asyncExec)

	})
	// 等待恢复逻辑
	<-done
	return futureResult, futureError
}

func (s *Looper) postCoroutine(t *CoroutineImpl) {
	s.coroutineQueue <- t
}

func (s *Looper) runningCoroutineLen() int {
	s.runningGuard.Lock()
	defer s.runningGuard.Unlock()
	return s.runningNum
}

func (s *Looper) markFinished() {
	s.runningGuard.Lock()
	s.runningNum--
	s.runningGuard.Unlock()
}

func (s *Looper) pickCoroutine() *CoroutineImpl {
	return <-s.coroutineQueue
}

func (s *Looper) signal(singal schedulerSignal) {
	s.signalChan <- singal
}

func (s *Looper) loop() {
	for {
		co := s.pickCoroutine()
		if co == nil {
			break
		}
		if co.NeedResume() {
			// 通知任务线程恢复逻辑
			co.signal(taskSignal_Resume)
		} else {
			Go(func() {
				co.executor(co.ctx, co)
				co.pending = false
				// 通知完成
				s.signal(scheduler_Finished)
				s.markFinished()
			})
		}
		// 等待完成或者挂起
		<-s.signalChan
	}
}

func (s *Looper) Start() {
	s.running = true
	go s.loop()
}

func (s *Looper) Exit() {
	endChan := make(chan bool)
	go func() {
		for {
			l := s.runningCoroutineLen()
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
		id:             id,
		coroutineQueue: make(chan *CoroutineImpl, coBuffer),
		signalChan:     make(chan schedulerSignal),
	}
}
