package co

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestStatefulPoolsModuleConsumesEscapeEventsWithoutReporters(t *testing.T) {
	core, observed := observer.New(zapcore.ErrorLevel)
	previousLogger := logger.Zap
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousLogger })

	module := NewStatefulPoolsModule(map[string]config.GoPool{
		DefaultGoPoolName: {
			Expire:                 time.Hour,
			RunningTaskTimeout:     time.Millisecond,
			TaskBuffer:             2,
			MaxEscapedWorkers:      1,
			MaxEscapedWorkersPerID: 1,
			DisableTimeoutWatch:    true,
		},
	}, nil, time.Hour)
	if err := module.Init(); err != nil {
		t.Fatalf("init stateful pools module: %v", err)
	}
	module.AfterInit()
	t.Cleanup(func() { cleanupStatefulPoolsModule(module) })
	module.BeforeShutdown()

	pool := module.pools[DefaultGoPoolName]
	releaseFirst := newF12TaskRelease(t)
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	firstDone := mustSubmitPoolTask(t, pool, context.Background(), 1, func(context.Context) {
		close(firstStarted)
		<-releaseFirst.ch
	}, true)
	waitForPoolSignal(t, firstStarted, "no-reporter first task start")
	secondDone := mustSubmitPoolTask(t, pool, context.Background(), 1, func(context.Context) {
		close(secondStarted)
	}, true)
	waitForPoolSignal(t, secondStarted, "no-reporter replacement task start")

	waitForPoolCondition(t, "no-reporter escape log", func() bool {
		return observed.FilterMessage("stateful pool worker escaped").Len() == 1
	})
	entry := observed.FilterMessage("stateful pool worker escaped").All()[0]
	fields := entry.ContextMap()
	if fields["pool"] != DefaultGoPoolName || fields["goID"] != int64(1) {
		t.Fatalf("unexpected no-reporter escape fields: %+v", fields)
	}

	releaseFirst.close()
	waitForPoolSignal(t, firstDone, "no-reporter first task completion")
	waitForPoolSignal(t, secondDone, "no-reporter replacement task completion")
	shutdownStatefulPoolsModule(t, module)
}

func TestStatefulPoolsModuleReconcilesDroppedEventsWithoutReporters(t *testing.T) {
	core, observed := observer.New(zapcore.ErrorLevel)
	previousLogger := logger.Zap
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousLogger })

	module := NewStatefulPoolsModule(map[string]config.GoPool{
		DefaultGoPoolName: {
			Expire:                 time.Hour,
			RunningTaskTimeout:     time.Millisecond,
			TaskBuffer:             2,
			MaxEscapedWorkers:      1,
			MaxEscapedWorkersPerID: 1,
			DisableTimeoutWatch:    true,
		},
	}, nil, time.Hour)
	if err := module.Init(); err != nil {
		t.Fatalf("init stateful pools module: %v", err)
	}
	t.Cleanup(func() { cleanupStatefulPoolsModule(module) })

	pool := module.pools[DefaultGoPoolName]
	// Each cycle emits worker-escaped and escaped-worker-exited. Keep the
	// consumer stopped until more than the fixed 64-event channel can hold.
	for cycle := 0; cycle < 33; cycle++ {
		releaseBlocked := newF12TaskRelease(t)
		blockedStarted := make(chan struct{})
		blockedDone := mustSubmitPoolTask(t, pool, context.Background(), 1, func(context.Context) {
			close(blockedStarted)
			<-releaseBlocked.ch
		}, true)
		waitForPoolSignal(t, blockedStarted, "dropped-event blocked task start")
		replacementDone := mustSubmitPoolTask(t, pool, context.Background(), 1, func(context.Context) {}, true)
		waitForPoolSignal(t, replacementDone, "dropped-event replacement completion")

		releaseBlocked.close()
		waitForPoolSignal(t, blockedDone, "dropped-event escaped task completion")
		waitForPoolCondition(t, "dropped-event escape state recovery", func() bool {
			return pool.pool.EscapeSnapshot().Total == 0
		})
	}
	snapshot := pool.pool.EscapeSnapshot()
	if snapshot.DroppedEvents == 0 {
		t.Fatalf("escape event channel did not report a drop: %+v", snapshot)
	}

	module.AfterInit()
	waitForPoolCondition(t, "authoritative dropped-event reconcile log", func() bool {
		return observed.FilterMessage("stateful pool escape events dropped").Len() == 1
	})
	fields := observed.FilterMessage("stateful pool escape events dropped").All()[0].ContextMap()
	if fields["pool"] != DefaultGoPoolName || fields["droppedDelta"] != snapshot.DroppedEvents ||
		fields["droppedTotal"] != snapshot.DroppedEvents {
		t.Fatalf("unexpected authoritative dropped-event fields: %+v", fields)
	}
	shutdownStatefulPoolsModule(t, module)
}

func TestStatefulPoolsModuleShutdownWaitsForConsumerBeforeRelease(t *testing.T) {
	pool := newTestStatefulPool(t, "shutdown-order", 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	module := &StatefulPoolsModule{
		pools:         map[string]*StatefulPool{pool.Name(): pool},
		monitorCancel: cancel,
	}
	poolClosedBeforeConsumerExit := make(chan bool, 1)
	module.monitorWG.Go(func() {
		<-ctx.Done()
		poolClosedBeforeConsumerExit <- pool.pool.IsClosed()
	})

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- module.Shutdown() }()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("shutdown stateful pools module: %v", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("module shutdown did not wait for a cancelable event consumer")
	}
	if wasClosed := <-poolClosedBeforeConsumerExit; wasClosed {
		t.Fatal("pool was released before the event consumer exited")
	}
	if !pool.pool.IsClosed() {
		t.Fatal("module shutdown returned without releasing its pool")
	}
}

func TestStatefulPoolsModuleShutdownDoesNotWaitForBlockedTask(t *testing.T) {
	pool := newTestStatefulPool(t, "shutdown-blocked-task", 1)
	module := &StatefulPoolsModule{pools: map[string]*StatefulPool{pool.Name(): pool}}
	releaseTask := make(chan struct{})
	var releaseOnce sync.Once
	unblockTask := func() { releaseOnce.Do(func() { close(releaseTask) }) }
	t.Cleanup(unblockTask)
	started := make(chan struct{})
	done := mustSubmitPoolTask(t, pool, context.Background(), 1, func(context.Context) {
		close(started)
		<-releaseTask
	}, true)
	waitForPoolSignal(t, started, "blocked shutdown task start")

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- module.Shutdown() }()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("shutdown stateful pools module: %v", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("module shutdown waited for a blocked accepted task")
	}
	if !pool.pool.IsClosed() {
		t.Fatal("module shutdown returned without starting pool release")
	}
	select {
	case <-done:
		t.Fatal("pool release unexpectedly completed the blocked task")
	default:
	}
	unblockTask()
	waitForPoolSignal(t, done, "blocked task completion after shutdown")
}

func shutdownStatefulPoolsModule(t *testing.T, module *StatefulPoolsModule) {
	t.Helper()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- module.Shutdown() }()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("shutdown stateful pools module: %v", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("timed out shutting down stateful pools module")
	}
}

func cleanupStatefulPoolsModule(module *StatefulPoolsModule) {
	if module.monitorCancel != nil {
		module.monitorCancel()
	}
	consumersDone := make(chan struct{})
	go func() {
		module.monitorWG.Wait()
		close(consumersDone)
	}()
	timer := time.NewTimer(poolTestTimeout)
	select {
	case <-consumersDone:
		timer.Stop()
	case <-timer.C:
	}
	for _, pool := range module.pools {
		pool.pool.Release()
	}
}
