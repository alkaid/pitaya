package cluster

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/util"
	"go.uber.org/zap"
)

const callbackQueueLimit = 64

type callbackWork struct {
	callback func()
	done     chan struct{}
}

// callbackDispatcher owns one FIFO callback lane. Producers only append under
// lock; exactly one lazily started run goroutine removes and executes work.
// stop closes admission and wakes blocked producers, but intentionally does not
// join the owner so a callback may call etcdServiceDiscovery.Shutdown itself.
type callbackDispatcher struct {
	lock      sync.Mutex
	notEmpty  *sync.Cond
	notFull   *sync.Cond
	queue     []callbackWork
	limit     int
	waiters   int
	started   bool
	stopping  bool
	drained   chan struct{}
	drainOnce sync.Once

	barrierWaiters atomic.Int32
}

type callbackDispatcherSnapshot struct {
	queued   int
	waiters  int
	started  bool
	stopping bool
}

func newCallbackDispatcher(limit int) *callbackDispatcher {
	if limit <= 0 {
		panic("cluster: callback dispatcher limit must be positive")
	}
	dispatcher := &callbackDispatcher{
		limit:   limit,
		drained: make(chan struct{}),
	}
	dispatcher.notEmpty = sync.NewCond(&dispatcher.lock)
	dispatcher.notFull = sync.NewCond(&dispatcher.lock)
	return dispatcher
}

func (d *callbackDispatcher) enqueue(callback func()) (<-chan struct{}, bool) {
	done := make(chan struct{})
	d.lock.Lock()
	// Backpressure is deliberate: topology callbacks are never dropped or
	// coalesced. The etcd watch client may buffer responses while this queue is
	// blocked, so listeners must still return promptly.
	for len(d.queue) >= d.limit && !d.stopping {
		d.waiters++
		d.notFull.Wait()
		d.waiters--
	}
	if d.stopping {
		d.lock.Unlock()
		return nil, false
	}
	d.queue = append(d.queue, callbackWork{callback: callback, done: done})
	if !d.started {
		d.started = true
		go d.run()
	}
	d.notEmpty.Signal()
	d.lock.Unlock()
	return done, true
}

func (d *callbackDispatcher) run() {
	defer d.drainOnce.Do(func() { close(d.drained) })
	for {
		d.lock.Lock()
		for len(d.queue) == 0 && !d.stopping {
			d.notEmpty.Wait()
		}
		if d.stopping && len(d.queue) == 0 {
			d.lock.Unlock()
			return
		}
		work := d.queue[0]
		d.queue[0] = callbackWork{}
		d.queue = d.queue[1:]
		d.notFull.Signal()
		d.lock.Unlock()

		d.executeWork(work)
	}
}

func (d *callbackDispatcher) executeWork(work callbackWork) {
	defer close(work.done)
	defer func() {
		if recovered := recover(); recovered != nil {
			util.LogRecoveredPanic(
				logger.Zap,
				"etcd callback work panicked",
				recovered,
				zap.String("callback_kind", "dispatcher_work"),
			)
		}
	}()
	if work.callback != nil {
		work.callback()
	}
}

func (d *callbackDispatcher) stop() {
	d.lock.Lock()
	d.stopping = true
	started := d.started
	d.notEmpty.Broadcast()
	d.notFull.Broadcast()
	d.lock.Unlock()
	if !started {
		d.drainOnce.Do(func() { close(d.drained) })
	}
}

func (d *callbackDispatcher) waitForBarrier(ctx context.Context, barrier <-chan struct{}, accepted bool) error {
	if !accepted {
		d.barrierWaiters.Add(1)
		defer d.barrierWaiters.Add(-1)
		select {
		case <-d.drained:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if barrier == nil {
		return nil
	}
	select {
	case <-barrier:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *callbackDispatcher) drainedSignal() <-chan struct{} {
	return d.drained
}

func (d *callbackDispatcher) snapshot() callbackDispatcherSnapshot {
	d.lock.Lock()
	defer d.lock.Unlock()
	return callbackDispatcherSnapshot{
		queued:   len(d.queue),
		waiters:  d.waiters,
		started:  d.started,
		stopping: d.stopping,
	}
}

func (d *callbackDispatcher) barrierWaiterCount() int32 {
	return d.barrierWaiters.Load()
}

func (sd *etcdServiceDiscovery) enqueueCallback(callback func()) (<-chan struct{}, bool) {
	return sd.watch.callbacks.enqueue(callback)
}

func (sd *etcdServiceDiscovery) enqueueLeaderCallback(callback func()) (<-chan struct{}, bool) {
	return sd.election.callbacks.enqueue(callback)
}

func (sd *etcdServiceDiscovery) stopCallbacks() {
	sd.watch.callbacks.stop()
}

func (sd *etcdServiceDiscovery) stopLeaderCallbacks() {
	sd.election.callbacks.stop()
}
