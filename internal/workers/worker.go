package workers

import (
	"sync"
	"sync/atomic"
	"time"
)

type worker struct {
	manager    *manager
	stop       chan struct{}
	done       chan struct{}
	stopOnce   sync.Once
	currentMu  sync.RWMutex
	currentMsg *Msg
	startedAt  int64
}

func (w *worker) start() {
	go w.work(w.manager.fetch.Messages())
}

func (w *worker) quit() {
	w.stopWork()
	w.wait()
}

func (w *worker) stopWork() {
	w.stopOnce.Do(func() {
		close(w.stop)
	})
}

func (w *worker) wait() {
	<-w.done
}

func (w *worker) work(messages chan *Msg) {
	defer close(w.done)
	for {
		select {
		case message := <-messages:
			atomic.StoreInt64(&w.startedAt, time.Now().UTC().Unix())
			w.currentMu.Lock()
			w.currentMsg = message
			w.currentMu.Unlock()

			if w.process(message) {
				confirmation := &confirmation{
					message: message,
					done:    make(chan struct{}),
				}
				w.manager.confirm <- confirmation
				<-confirmation.done
			}

			atomic.StoreInt64(&w.startedAt, 0)
			w.currentMu.Lock()
			w.currentMsg = nil
			w.currentMu.Unlock()

			// Attempt to tell fetcher we're finished.
			// Can be used when the fetcher has slept due
			// to detecting an empty queue to requery the
			// queue immediately if we finish work.
			select {
			case w.manager.fetch.FinishedWork() <- true:
			default:
			}
		case w.manager.fetch.Ready() <- true:
			// Signaled to fetcher that we're
			// ready to accept a message
		case <-w.stop:
			return
		}
	}
}

func (w *worker) process(message *Msg) (acknowledge bool) {
	acknowledge = true

	defer func() {
		if recovered := recover(); recovered != nil {
			if outcome, ok := recovered.(*retryPanicOutcome); ok {
				acknowledge = outcome.acknowledge
			}
		}
	}()

	return w.manager.mids.call(w.manager.queueName(), message, func() {
		w.manager.job(message)
	})
}

func (w *worker) processing() bool {
	return atomic.LoadInt64(&w.startedAt) > 0
}

func (w *worker) current() (*Msg, int64) {
	w.currentMu.RLock()
	defer w.currentMu.RUnlock()
	return w.currentMsg, atomic.LoadInt64(&w.startedAt)
}

func newWorker(m *manager) *worker {
	return &worker{
		manager: m,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}
