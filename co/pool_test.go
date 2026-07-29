package co

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
)

const poolTestTimeout = 3 * time.Second

func newTestStatefulPool(t *testing.T, name string, capacity int) *StatefulPool {
	t.Helper()

	p, err := NewStatefulPool(config.GoPool{
		Name:                name,
		Expire:              time.Hour,
		RunningTaskTimeout:  time.Hour,
		DisablePurge:        true,
		DisablePurgeRunning: true,
		TaskBuffer:          8,
		DisableTimeoutWatch: true,
		TimeoutBuckets:      []time.Duration{time.Hour},
	}, nil)
	if err != nil {
		t.Fatalf("create stateful pool: %v", err)
	}
	p.pool.Tune(capacity)
	t.Cleanup(p.pool.Release)
	return p
}

func waitForPoolSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()

	timer := time.NewTimer(poolTestTimeout)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", description)
	}
}

func enterPoolTask(running, maximum *atomic.Int32) func() {
	current := running.Add(1)
	for {
		previous := maximum.Load()
		if current <= previous || maximum.CompareAndSwap(previous, current) {
			break
		}
	}
	return func() {
		running.Add(-1)
	}
}

func TestStatefulPoolSameKeyRunsFIFOAndSerially(t *testing.T) {
	p := newTestStatefulPool(t, "same-key", 2)
	ctx := context.Background()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	unblockFirst := func() {
		releaseOnce.Do(func() { close(releaseFirst) })
	}
	t.Cleanup(unblockFirst)

	var running atomic.Int32
	var maximum atomic.Int32
	var orderMu sync.Mutex
	order := make([]int, 0, 3)
	task := func(sequence int) func(context.Context) {
		return func(context.Context) {
			leave := enterPoolTask(&running, &maximum)
			defer leave()

			orderMu.Lock()
			order = append(order, sequence)
			orderMu.Unlock()
			if sequence == 1 {
				close(firstStarted)
				<-releaseFirst
			}
		}
	}

	done := make([]<-chan struct{}, 0, 3)
	done = append(done, p.Go(ctx, 42, task(1), true))
	waitForPoolSignal(t, firstStarted, "first same-key task to start")
	done = append(done, p.Go(ctx, 42, task(2), true))
	done = append(done, p.Go(ctx, 42, task(3), true))
	unblockFirst()
	for _, taskDone := range done {
		waitForPoolSignal(t, taskDone, "same-key task completion")
	}

	orderMu.Lock()
	gotOrder := append([]int(nil), order...)
	orderMu.Unlock()
	if want := []int{1, 2, 3}; !reflect.DeepEqual(gotOrder, want) {
		t.Fatalf("same-key start order = %v, want %v", gotOrder, want)
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum same-key concurrency = %d, want 1", got)
	}
}

func TestStatefulPoolDifferentKeysRunInParallel(t *testing.T) {
	p := newTestStatefulPool(t, "different-keys", 2)
	ctx := context.Background()
	releaseTasks := make(chan struct{})
	var releaseOnce sync.Once
	unblockTasks := func() {
		releaseOnce.Do(func() { close(releaseTasks) })
	}
	t.Cleanup(unblockTasks)

	started := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var running atomic.Int32
	var maximum atomic.Int32
	task := func(index int) func(context.Context) {
		return func(context.Context) {
			leave := enterPoolTask(&running, &maximum)
			defer leave()
			close(started[index])
			<-releaseTasks
		}
	}

	firstDone := p.Go(ctx, 1, task(0), true)
	waitForPoolSignal(t, started[0], "first key to start")
	secondDone := p.Go(ctx, 2, task(1), true)
	waitForPoolSignal(t, started[1], "second key to start while first is blocked")
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum different-key concurrency = %d, want 2", got)
	}

	unblockTasks()
	waitForPoolSignal(t, firstDone, "first different-key task completion")
	waitForPoolSignal(t, secondDone, "second different-key task completion")
}

func TestStatefulPoolTaskCanQueueAnotherTaskForSameKey(t *testing.T) {
	p := newTestStatefulPool(t, "nested", 1)
	ctx := context.Background()
	nestedSubmitted := make(chan struct{})
	nestedRan := make(chan struct{})
	var nestedDone <-chan struct{}

	outerDone := p.Go(ctx, 7, func(ctx context.Context) {
		nestedDone = p.Go(ctx, 7, func(context.Context) {
			close(nestedRan)
		}, true)
		close(nestedSubmitted)
	}, true)

	waitForPoolSignal(t, nestedSubmitted, "nested same-key task submission")
	waitForPoolSignal(t, outerDone, "outer same-key task completion")
	waitForPoolSignal(t, nestedRan, "nested same-key task execution")
	waitForPoolSignal(t, nestedDone, "nested same-key task completion")
}

func TestStatefulPoolWaitAcrossPools(t *testing.T) {
	outer := newTestStatefulPool(t, "outer", 1)
	inner := newTestStatefulPool(t, "inner", 1)
	ctx := context.Background()
	innerStarted := make(chan struct{})
	releaseInner := make(chan struct{})
	waitReturned := make(chan struct{})
	var releaseOnce sync.Once
	unblockInner := func() {
		releaseOnce.Do(func() { close(releaseInner) })
	}
	t.Cleanup(unblockInner)

	outerDone := outer.Go(ctx, 1, func(ctx context.Context) {
		inner.Wait(ctx, 1, func(context.Context) {
			close(innerStarted)
			<-releaseInner
		}, true)
		close(waitReturned)
	}, true)

	waitForPoolSignal(t, innerStarted, "inner pool task to start")
	select {
	case <-waitReturned:
		t.Fatal("Wait returned before the inner task completed")
	default:
	}
	unblockInner()
	waitForPoolSignal(t, waitReturned, "Wait to return after inner task completion")
	waitForPoolSignal(t, outerDone, "outer pool task completion")
}
