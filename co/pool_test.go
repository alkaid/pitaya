package co

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/metrics"
)

const poolTestTimeout = 3 * time.Second

func newTestStatefulPool(t *testing.T, name string, capacity int, options ...ants.Option) *StatefulPool {
	t.Helper()

	return newTestStatefulPoolWithConfig(t, config.GoPool{
		Name:                name,
		Expire:              time.Hour,
		RunningTaskTimeout:  time.Hour,
		DisablePurge:        true,
		DisablePurgeRunning: true,
		TaskBuffer:          8,
		DisableTimeoutWatch: true,
		TimeoutBuckets:      []time.Duration{time.Hour},
	}, nil, capacity, options...)
}

func newTestStatefulPoolWithConfig(
	t *testing.T,
	cfg config.GoPool,
	reporters []metrics.Reporter,
	capacity int,
	options ...ants.Option,
) *StatefulPool {
	t.Helper()

	p, err := NewStatefulPool(cfg, reporters, options...)
	if err != nil {
		t.Fatalf("create stateful pool: %v", err)
	}
	p.pool.Tune(capacity)
	t.Cleanup(p.pool.Release)
	return p
}

func mustSubmitPoolTask(
	t *testing.T,
	p *StatefulPool,
	ctx context.Context,
	goID int,
	task func(context.Context),
	disableTimeoutWatch bool,
) <-chan struct{} {
	t.Helper()

	done, err := p.Go(ctx, goID, task, disableTimeoutWatch)
	if err != nil {
		t.Fatalf("submit stateful pool task: %v", err)
	}
	if done == nil {
		t.Fatal("successful stateful pool submission returned nil done")
	}
	return done
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

func waitForPoolCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(poolTestTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		runtime.Gosched()
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
	done = append(done, mustSubmitPoolTask(t, p, ctx, 42, task(1), true))
	waitForPoolSignal(t, firstStarted, "first same-key task to start")
	done = append(done, mustSubmitPoolTask(t, p, ctx, 42, task(2), true))
	done = append(done, mustSubmitPoolTask(t, p, ctx, 42, task(3), true))
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

	firstDone := mustSubmitPoolTask(t, p, ctx, 1, task(0), true)
	waitForPoolSignal(t, started[0], "first key to start")
	secondDone := mustSubmitPoolTask(t, p, ctx, 2, task(1), true)
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
	var nestedErr error

	outerDone := mustSubmitPoolTask(t, p, ctx, 7, func(ctx context.Context) {
		nestedDone, nestedErr = p.Go(ctx, 7, func(context.Context) {
			close(nestedRan)
		}, true)
		close(nestedSubmitted)
	}, true)

	waitForPoolSignal(t, nestedSubmitted, "nested same-key task submission")
	if nestedErr != nil {
		t.Fatalf("submit nested same-key task: %v", nestedErr)
	}
	if nestedDone == nil {
		t.Fatal("successful nested submission returned nil done")
	}
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
	var waitErr error

	outerDone := mustSubmitPoolTask(t, outer, ctx, 1, func(ctx context.Context) {
		waitErr = inner.Wait(ctx, 1, func(context.Context) {
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
	if waitErr != nil {
		t.Fatalf("Wait returned error: %v", waitErr)
	}
	waitForPoolSignal(t, outerDone, "outer pool task completion")
}

func TestStatefulPoolDonePublishesTaskWrites(t *testing.T) {
	p := newTestStatefulPool(t, "done-publish", 1)
	var result int

	done := mustSubmitPoolTask(t, p, context.Background(), 1, func(context.Context) {
		result = 42
	}, true)
	waitForPoolSignal(t, done, "task write publication")

	if result != 42 {
		t.Fatalf("result after done closed = %d, want 42", result)
	}
}

func TestStatefulPoolRejectsClosedPool(t *testing.T) {
	p := newTestStatefulPool(t, "closed", 1)
	p.pool.Release()
	var ran atomic.Bool

	done, err := p.Go(context.Background(), 1, func(context.Context) {
		ran.Store(true)
	}, true)
	if !errors.Is(err, ants.ErrPoolClosed) {
		t.Fatalf("Go error = %v, want %v", err, ants.ErrPoolClosed)
	}
	if done != nil {
		t.Fatal("rejected submission returned non-nil done")
	}
	if err := p.Wait(context.Background(), 1, func(context.Context) {
		ran.Store(true)
	}, true); !errors.Is(err, ants.ErrPoolClosed) {
		t.Fatalf("Wait error = %v, want %v", err, ants.ErrPoolClosed)
	}
	if ran.Load() {
		t.Fatal("task ran after pool rejection")
	}
}

func TestStatefulPoolReturnsOverloadWithoutDone(t *testing.T) {
	p := newTestStatefulPoolWithConfig(t, config.GoPool{
		Name:                "overload",
		Expire:              time.Hour,
		RunningTaskTimeout:  time.Hour,
		DisablePurge:        true,
		DisablePurgeRunning: true,
		TaskBuffer:          1,
		DisableTimeoutWatch: true,
		TimeoutBuckets:      []time.Duration{time.Hour},
	}, nil, 1, ants.WithNonblocking(true))
	releaseFirst := make(chan struct{})
	firstStarted := make(chan struct{})
	var releaseOnce sync.Once
	unblockFirst := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(unblockFirst)

	firstDone := mustSubmitPoolTask(t, p, context.Background(), 1, func(context.Context) {
		close(firstStarted)
		<-releaseFirst
	}, true)
	waitForPoolSignal(t, firstStarted, "first overload task to start")
	secondDone := mustSubmitPoolTask(t, p, context.Background(), 1, func(context.Context) {}, true)

	done, err := p.Go(context.Background(), 1, func(context.Context) {}, true)
	if !errors.Is(err, ants.ErrPoolOverload) {
		t.Fatalf("Go error = %v, want %v", err, ants.ErrPoolOverload)
	}
	if done != nil {
		t.Fatal("overloaded submission returned non-nil done")
	}

	unblockFirst()
	waitForPoolSignal(t, firstDone, "first overload task completion")
	waitForPoolSignal(t, secondDone, "queued overload task completion")
}

func TestStatefulPoolRejectsPreCanceledContext(t *testing.T) {
	p := newTestStatefulPool(t, "pre-cancel", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var ran atomic.Bool

	done, err := p.Go(ctx, 1, func(context.Context) {
		ran.Store(true)
	}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Go error = %v, want %v", err, context.Canceled)
	}
	if done != nil {
		t.Fatal("pre-canceled submission returned non-nil done")
	}
	if ran.Load() {
		t.Fatal("pre-canceled task ran")
	}
}

func TestStatefulPoolWaitReturnsContextErrorAfterAcceptance(t *testing.T) {
	p := newTestStatefulPool(t, "wait-cancel", 1)
	ctx, cancel := context.WithCancel(context.Background())
	taskStarted := make(chan struct{})
	releaseTask := make(chan struct{})
	taskFinished := make(chan struct{})
	waitResult := make(chan error, 1)
	var releaseOnce sync.Once
	unblockTask := func() { releaseOnce.Do(func() { close(releaseTask) }) }
	t.Cleanup(unblockTask)

	go func() {
		waitResult <- p.Wait(ctx, 1, func(context.Context) {
			close(taskStarted)
			<-releaseTask
			close(taskFinished)
		}, true)
	}()
	waitForPoolSignal(t, taskStarted, "accepted task to start")
	cancel()

	select {
	case err := <-waitResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("timed out waiting for canceled Wait")
	}
	select {
	case <-taskFinished:
		t.Fatal("accepted task finished before it was released")
	default:
	}

	unblockTask()
	waitForPoolSignal(t, taskFinished, "accepted task completion after Wait cancellation")
}

func TestStatefulPoolAcceptedQueuedTaskRunsAfterContextCancellation(t *testing.T) {
	p := newTestStatefulPool(t, "queued-cancel", 1)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondRan := make(chan struct{})
	var releaseOnce sync.Once
	unblockFirst := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(unblockFirst)

	firstDone := mustSubmitPoolTask(t, p, context.Background(), 1, func(context.Context) {
		close(firstStarted)
		<-releaseFirst
	}, true)
	waitForPoolSignal(t, firstStarted, "first queued-cancel task to start")

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := mustSubmitPoolTask(t, p, ctx, 1, func(context.Context) {
		close(secondRan)
	}, true)
	cancel()
	select {
	case <-secondDone:
		t.Fatal("queued task completed before the running task was released")
	default:
	}

	unblockFirst()
	waitForPoolSignal(t, firstDone, "first queued-cancel task completion")
	waitForPoolSignal(t, secondRan, "queued task execution after context cancellation")
	waitForPoolSignal(t, secondDone, "queued task completion after context cancellation")
}

func TestStatefulPoolBlockingSubmitRemainsAcceptedAfterContextCancellation(t *testing.T) {
	p := newTestStatefulPoolWithConfig(t, config.GoPool{
		Name:                "blocking-submit-cancel",
		Expire:              time.Hour,
		RunningTaskTimeout:  time.Hour,
		DisablePurge:        true,
		DisablePurgeRunning: true,
		TaskBuffer:          1,
		DisableTimeoutWatch: true,
		TimeoutBuckets:      []time.Duration{time.Hour},
	}, nil, 1)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	fourthRan := make(chan struct{})
	var releaseOnce sync.Once
	unblockFirst := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(unblockFirst)

	firstDone := mustSubmitPoolTask(t, p, context.Background(), 1, func(context.Context) {
		close(firstStarted)
		<-releaseFirst
	}, true)
	waitForPoolSignal(t, firstStarted, "first blocking-submit task to start")
	secondDone := mustSubmitPoolTask(t, p, context.Background(), 1, func(context.Context) {}, true)
	thirdDone := mustSubmitPoolTask(t, p, context.Background(), 1, func(context.Context) {}, true)

	ctx, cancel := context.WithCancel(context.Background())
	type submissionResult struct {
		done <-chan struct{}
		err  error
	}
	fourthResult := make(chan submissionResult, 1)
	go func() {
		done, err := p.Go(ctx, 1, func(context.Context) {
			close(fourthRan)
		}, true)
		fourthResult <- submissionResult{done: done, err: err}
	}()
	waitForPoolCondition(t, "fourth submission to block", func() bool {
		return p.pool.Waiting() == 1
	})

	cancel()
	select {
	case result := <-fourthResult:
		t.Fatalf("blocked submission returned after context cancellation: %v", result.err)
	default:
	}
	if got := p.pool.Waiting(); got != 1 {
		t.Fatalf("waiting submissions after cancellation = %d, want 1", got)
	}

	unblockFirst()
	result := <-fourthResult
	if result.err != nil {
		t.Fatalf("blocking submission error = %v, want nil", result.err)
	}
	if result.done == nil {
		t.Fatal("accepted blocking submission returned nil done")
	}
	waitForPoolSignal(t, firstDone, "first blocking-submit task completion")
	waitForPoolSignal(t, secondDone, "second blocking-submit task completion")
	waitForPoolSignal(t, thirdDone, "third blocking-submit task completion")
	waitForPoolSignal(t, fourthRan, "canceled blocking submission execution")
	waitForPoolSignal(t, result.done, "canceled blocking submission completion")
}

type recordingAntsLogger struct {
	messages chan string
}

func (l *recordingAntsLogger) Printf(format string, args ...any) {
	select {
	case l.messages <- fmt.Sprintf(format, args...):
	default:
	}
}

func TestStatefulPoolPanicClosesDoneAndUsesAntsRecovery(t *testing.T) {
	panicLog := &recordingAntsLogger{messages: make(chan string, 1)}
	p := newTestStatefulPool(t, "panic", 1, ants.WithLogger(panicLog))
	waitReturned := make(chan struct{})

	go func() {
		p.Wait(context.Background(), 1, func(context.Context) {
			panic("task panic")
		}, true)
		close(waitReturned)
	}()
	waitForPoolSignal(t, waitReturned, "Wait to return after task panic")

	select {
	case message := <-panicLog.messages:
		if !strings.Contains(message, "task panic") {
			t.Fatalf("panic log = %q, want panic value", message)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("timed out waiting for ants panic recovery log")
	}

	followupDone := mustSubmitPoolTask(t, p, context.Background(), 1, func(context.Context) {}, true)
	waitForPoolSignal(t, followupDone, "same-key task after panic")
}

func TestStatefulPoolNilInputs(t *testing.T) {
	p := newTestStatefulPool(t, "nil-input", 1)
	taskCtx := make(chan context.Context, 1)

	done, err := p.Go(nil, 1, func(ctx context.Context) {
		taskCtx <- ctx
	}, true)
	if err != nil {
		t.Fatalf("Go with nil context: %v", err)
	}
	waitForPoolSignal(t, done, "nil-context task completion")
	select {
	case ctx := <-taskCtx:
		if ctx == nil {
			t.Fatal("task received nil context")
		}
	default:
		t.Fatal("task did not publish its normalized context")
	}

	done, err = p.Go(context.Background(), 2, nil, true)
	if !errors.Is(err, ErrNilTask) {
		t.Fatalf("nil task error = %v, want %v", err, ErrNilTask)
	}
	if done != nil {
		t.Fatal("nil task returned non-nil done")
	}
}

func TestUserPoolWrappersRejectInvalidUserID(t *testing.T) {
	done, err := GoWithUser(nil, 0, func(context.Context) {})
	if !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("GoWithUser error = %v, want %v", err, ErrInvalidUserID)
	}
	if done != nil {
		t.Fatal("invalid user ID returned non-nil done")
	}
	if err := WaitWithUser(nil, -1, func(context.Context) {}); !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("WaitWithUser error = %v, want %v", err, ErrInvalidUserID)
	}
}

func TestSubmitReturnsSubmissionErrors(t *testing.T) {
	if err := Submit(nil); !errors.Is(err, ErrNilTask) {
		t.Fatalf("Submit nil task error = %v, want %v", err, ErrNilTask)
	}

	ants.Release()
	t.Cleanup(ants.Reboot)
	var ran atomic.Bool
	if err := Submit(func() { ran.Store(true) }); !errors.Is(err, ants.ErrPoolClosed) {
		t.Fatalf("Submit closed pool error = %v, want %v", err, ants.ErrPoolClosed)
	}
	if ran.Load() {
		t.Fatal("stateless task ran after submission rejection")
	}
}

type deadlineSignalReporter struct {
	reported chan struct{}
	once     sync.Once
}

func (r *deadlineSignalReporter) ReportCount(name string, _ map[string]string, _ float64) error {
	if name == metrics.PoolGoDeadlines {
		r.once.Do(func() { close(r.reported) })
	}
	return nil
}

func (*deadlineSignalReporter) ReportGauge(string, map[string]string, float64) error {
	return nil
}

func (*deadlineSignalReporter) ReportHistogram(string, map[string]string, float64) error {
	return nil
}

func (*deadlineSignalReporter) ReportSummary(string, map[string]string, float64) error {
	return nil
}

func TestStatefulPoolTimeoutWatcherStartsAfterAcceptance(t *testing.T) {
	reporter := &deadlineSignalReporter{reported: make(chan struct{})}
	p := newTestStatefulPoolWithConfig(t, config.GoPool{
		Name:                "timeout-watch",
		Expire:              time.Hour,
		RunningTaskTimeout:  time.Hour,
		DisablePurge:        true,
		DisablePurgeRunning: true,
		TaskBuffer:          1,
		TimeoutBuckets:      []time.Duration{time.Millisecond},
	}, []metrics.Reporter{reporter}, 1)
	taskStarted := make(chan struct{})
	releaseTask := make(chan struct{})
	var releaseOnce sync.Once
	unblockTask := func() { releaseOnce.Do(func() { close(releaseTask) }) }
	t.Cleanup(unblockTask)

	done := mustSubmitPoolTask(t, p, context.Background(), 1, func(context.Context) {
		close(taskStarted)
		<-releaseTask
	}, false)
	waitForPoolSignal(t, taskStarted, "timeout-watched task to start")
	waitForPoolSignal(t, reporter.reported, "accepted task timeout report")
	unblockTask()
	waitForPoolSignal(t, done, "timeout-watched task completion")
}

func TestStatefulPoolRejectedSubmissionDoesNotStartTimeoutWatcher(t *testing.T) {
	reporter := &deadlineSignalReporter{reported: make(chan struct{})}
	p := newTestStatefulPoolWithConfig(t, config.GoPool{
		Name:                "rejected-timeout-watch",
		Expire:              time.Hour,
		RunningTaskTimeout:  time.Hour,
		DisablePurge:        true,
		DisablePurgeRunning: true,
		TaskBuffer:          1,
		TimeoutBuckets:      []time.Duration{time.Millisecond},
	}, []metrics.Reporter{reporter}, 1)
	p.pool.Release()

	done, err := p.Go(context.Background(), 1, func(context.Context) {}, false)
	if !errors.Is(err, ants.ErrPoolClosed) {
		t.Fatalf("Go error = %v, want %v", err, ants.ErrPoolClosed)
	}
	if done != nil {
		t.Fatal("rejected timeout-watched submission returned non-nil done")
	}

	select {
	case <-reporter.reported:
		t.Fatal("rejected submission started a timeout watcher")
	case <-time.After(50 * time.Millisecond):
	}
}
