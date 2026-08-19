// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package timer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

type alwaysRunCondition struct{}

func (a *alwaysRunCondition) Check(time.Time) bool {
	return true
}

type neverRunCondition struct{}

func (a *neverRunCondition) Check(time.Time) bool {
	return false
}

type panicCondition struct{}

func (p *panicCondition) Check(time.Time) bool {
	panic("condition failed")
}

type panicLoggingValue struct{}

func (panicLoggingValue) MarshalLogObject(zapcore.ObjectEncoder) error {
	panic("panic value logging failed")
}

type panicLoggingCondition struct{}

func (panicLoggingCondition) Check(time.Time) bool {
	panic(panicLoggingValue{})
}

type timerCreatingCondition struct {
	manager *TimerManager
	count   int
	done    chan struct{}
}

func (c *timerCreatingCondition) Check(time.Time) bool {
	for range c.count {
		c.manager.NewTimerWithPolicy(func() {}, time.Hour, 1, ExecutionSerial)
	}
	close(c.done)
	return false
}

func startTestManager(t *testing.T, manager *TimerManager) (chan time.Time, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1024)
	done := make(chan struct{})
	go func() {
		manager.Run(ctx, ticks)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("timer manager did not stop")
		}
	})
	return ticks, cancel
}

func waitForTimer(t *testing.T, manager *TimerManager, timer *Timer) {
	t.Helper()
	require.Eventually(t, func() bool {
		value, ok := manager.timers.Load(timer.ID)
		return ok && value == timer
	}, time.Second, time.Millisecond)
}

func TestInit(t *testing.T) {
	t.Parallel()
	assert.NotNil(t, Manager.ChClosingTimer)
	assert.NotNil(t, Manager.ChCreatedTimer)
}

func TestNewTimerManagerEnforcesMinimumBacklog(t *testing.T) {
	t.Parallel()
	manager := NewTimerManager(1)
	assert.Equal(t, 16, cap(manager.ChClosingTimer))
	assert.Equal(t, 16, cap(manager.ChCreatedTimer))
}

func TestTimerManagerCreatesAndStopsTimerThroughOwner(t *testing.T) {
	t.Parallel()
	manager := NewTimerManager(16)
	startTestManager(t, manager)

	timer := manager.NewTimerWithPolicy(func() {}, time.Hour, LoopForever, ExecutionSerial)
	waitForTimer(t, manager, timer)

	timer.Stop()
	require.Eventually(t, func() bool {
		_, ok := manager.timers.Load(timer.ID)
		return !ok
	}, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&timer.closed))
}

func TestTimerManagerRunHasSingleOwner(t *testing.T) {
	t.Parallel()
	manager := NewTimerManager(16)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticksA := make(chan time.Time, 1)
	ticksB := make(chan time.Time, 1)
	var runners sync.WaitGroup
	for _, ticks := range []<-chan time.Time{ticksA, ticksB} {
		ticks := ticks
		runners.Add(1)
		go func() {
			defer runners.Done()
			manager.Run(ctx, ticks)
		}()
	}
	t.Cleanup(func() {
		cancel()
		runners.Wait()
	})

	ticksA <- time.Now()
	ticksB <- time.Now()
	require.Eventually(t, func() bool {
		return manager.Stats().Ticks == 1
	}, time.Second, time.Millisecond)
	assert.Never(t, func() bool {
		return manager.Stats().Ticks > 1
	}, 25*time.Millisecond, time.Millisecond, "standby Run caller consumed a tick without owning the manager")
}

func TestSetCondition(t *testing.T) {
	t.Parallel()
	manager := NewTimerManager(16)
	timer := manager.NewTimerWithPolicy(func() {}, time.Hour, LoopForever, ExecutionSerial)
	condition := &neverRunCondition{}
	timer.SetCondition(condition)
	assert.Same(t, condition, timer.getCondition())
}

func TestInvalidExecutionPolicyPanics(t *testing.T) {
	t.Parallel()
	manager := NewTimerManager(16)
	assert.PanicsWithValue(t, "pitaya/timer: invalid execution policy", func() {
		manager.NewTimerWithPolicy(func() {}, time.Second, 1, ExecutionPolicy(99))
	})
}

func TestPexec(t *testing.T) {
	t.Parallel()
	called := 0
	pexec(10, func() { called++ })
	assert.Equal(t, 1, called)

	assert.NotPanics(t, func() {
		pexec(20, func() { panic("boom") })
	})
}

func TestTimerManagerSerialPolicyPreventsReentryAndReportsLag(t *testing.T) {
	t.Parallel()
	manager := NewTimerManager(16)
	ticks, _ := startTestManager(t, manager)

	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	timer := manager.NewTimerWithPolicy(func() {
		switch calls.Add(1) {
		case 1:
			close(firstStarted)
		case 2:
			close(secondStarted)
		}
		<-release
	}, time.Millisecond, LoopForever, ExecutionSerial)
	timer.createAt = 0
	waitForTimer(t, manager, timer)

	ticks <- time.Now().Add(-20 * time.Millisecond)
	require.Eventually(t, func() bool {
		select {
		case <-firstStarted:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	ticks <- time.Now()
	select {
	case <-secondStarted:
		t.Fatal("serial timer callback reentered before the first callback returned")
	case <-time.After(25 * time.Millisecond):
	}

	close(release)
	require.Eventually(t, func() bool {
		select {
		case <-secondStarted:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		return manager.Stats().SerialOverruns > 0
	}, time.Second, time.Millisecond)
	stats := manager.Stats()
	assert.Equal(t, uint64(2), stats.Callbacks)
	assert.Greater(t, stats.MaxTickLag, time.Duration(0))
	assert.Greater(t, stats.MaxCallbackLag, time.Duration(0))
}

func TestTimerManagerSerialCallbackCanFillMutationQueue(t *testing.T) {
	t.Parallel()
	manager := NewTimerManager(16)
	ticks, _ := startTestManager(t, manager)

	created := make(chan []*Timer, 1)
	timer := manager.NewTimerWithPolicy(func() {
		nested := make([]*Timer, 0, cap(manager.ChCreatedTimer)+1)
		for range cap(manager.ChCreatedTimer) + 1 {
			nested = append(nested, manager.NewTimerWithPolicy(
				func() {}, time.Hour, 1, ExecutionSerial,
			))
		}
		created <- nested
	}, time.Millisecond, 1, ExecutionSerial)
	timer.createAt = 0
	waitForTimer(t, manager, timer)

	ticks <- time.Now()
	var nested []*Timer
	select {
	case nested = <-created:
	case <-time.After(time.Second):
		t.Fatal("serial callback deadlocked while filling the timer mutation queue")
	}

	require.Eventually(t, func() bool {
		for _, nestedTimer := range nested {
			if _, ok := manager.timers.Load(nestedTimer.ID); !ok {
				return false
			}
		}
		return true
	}, time.Second, time.Millisecond)
}

func TestTimerManagerAsyncPolicyAllowsObservableOverlap(t *testing.T) {
	t.Parallel()
	manager := NewTimerManager(16)
	ticks, _ := startTestManager(t, manager)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	timer := manager.NewTimerWithPolicy(func() {
		started <- struct{}{}
		<-release
	}, time.Millisecond, LoopForever, ExecutionAsync)
	timer.createAt = 0
	waitForTimer(t, manager, timer)

	ticks <- time.Now()
	ticks <- time.Now()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("async callback did not start")
		}
	}
	require.Eventually(t, func() bool {
		return manager.Stats().AsyncOverruns == 1
	}, time.Second, time.Millisecond)
	close(release)

	stats := manager.Stats()
	assert.Equal(t, uint64(2), stats.Callbacks)
	assert.Equal(t, uint64(1), stats.Overruns)
}

func TestTimerManagerSkipIfRunningDropsOverrun(t *testing.T) {
	t.Parallel()
	manager := NewTimerManager(16)
	ticks, _ := startTestManager(t, manager)

	started := make(chan struct{}, 2)
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	timer := manager.NewTimerWithPolicy(func() {
		call := calls.Add(1)
		started <- struct{}{}
		if call == 1 {
			<-releaseFirst
		}
	}, time.Millisecond, LoopForever, ExecutionSkipIfRunning)
	timer.createAt = 0
	waitForTimer(t, manager, timer)

	ticks <- time.Now()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first callback did not start")
	}
	ticks <- time.Now()
	require.Eventually(t, func() bool {
		return manager.Stats().SkippedOverruns == 1
	}, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), calls.Load())

	close(releaseFirst)
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&timer.running) == 0
	}, time.Second, time.Millisecond)
	ticks <- time.Now()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("callback did not run after the previous invocation completed")
	}
	assert.Equal(t, int32(2), calls.Load())
	assert.Equal(t, uint64(2), manager.Stats().Callbacks)
}

func TestTimerManagerConcurrentCreateCloseAndTick(t *testing.T) {
	manager := NewTimerManager(1024)
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1024)
	var owners sync.WaitGroup
	for range 4 {
		owners.Add(1)
		go func() {
			defer owners.Done()
			manager.Run(ctx, ticks)
		}()
	}

	var producers sync.WaitGroup
	for producer := range 8 {
		producer := producer
		producers.Add(1)
		go func() {
			defer producers.Done()
			for index := range 50 {
				policy := ExecutionPolicy((producer + index) % 3)
				timer := manager.NewTimerWithPolicy(func() {}, time.Nanosecond, 1, policy)
				if index%2 == 0 {
					timer.Stop()
				}
				timer.SetCondition(&neverRunCondition{})
				ticks <- time.Now()
			}
		}()
	}
	producers.Wait()
	require.Eventually(t, func() bool {
		return manager.Stats().Ticks > 0
	}, time.Second, time.Millisecond)
	cancel()
	owners.Wait()
}

func TestTimerManagerCallbackDrainIncludesConcurrentAdmission(t *testing.T) {
	t.Parallel()

	manager := NewTimerManager(16)
	manager.addCallback()
	drained := make(chan struct{})
	go func() {
		manager.waitForCallbacks()
		close(drained)
	}()

	manager.addCallback()
	manager.callbackDone()
	select {
	case <-drained:
		t.Fatal("callback drain returned while a concurrently admitted callback remained")
	case <-time.After(20 * time.Millisecond):
	}

	manager.callbackDone()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("callback drain did not finish after all callbacks completed")
	}
}

func TestTimerManagerShutdownDrainsAsyncCallbackMutations(t *testing.T) {
	manager := NewTimerManager(16)
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	runDone := make(chan struct{})
	go func() {
		manager.Run(ctx, ticks)
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("timer manager did not stop")
		}
	})

	callbackStarted := make(chan struct{})
	createMutations := make(chan struct{})
	created := make(chan []*Timer, 1)
	timer := manager.NewTimerWithPolicy(func() {
		close(callbackStarted)
		<-createMutations
		nested := make([]*Timer, 0, cap(manager.ChCreatedTimer)+1)
		for range cap(manager.ChCreatedTimer) + 1 {
			nested = append(nested, manager.NewTimerWithPolicy(
				func() {}, time.Hour, 1, ExecutionSerial,
			))
		}
		created <- nested
	}, time.Nanosecond, 1, ExecutionAsync)
	timer.createAt = 0
	waitForTimer(t, manager, timer)

	ticks <- time.Now()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("async callback did not start")
	}
	cancel()
	// Give the owner time to observe cancellation and enter its callback drain.
	time.Sleep(20 * time.Millisecond)
	close(createMutations)

	var nested []*Timer
	select {
	case nested = <-created:
	case <-time.After(time.Second):
		t.Fatal("shutdown deadlocked while an accepted callback filled the mutation queue")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("timer manager did not finish after the accepted callback returned")
	}
	for _, nestedTimer := range nested {
		_, ok := manager.timers.Load(nestedTimer.ID)
		assert.True(t, ok, "accepted mutation %d was not applied before shutdown", nestedTimer.ID)
	}
}

func TestCronDoesNotEscapeActiveRunLifecycle(t *testing.T) {
	manager := NewTimerManager(16)
	timerConfigMu.Lock()
	originalManager := Manager
	Manager = manager
	timerConfigMu.Unlock()
	t.Cleanup(func() {
		timerConfigMu.Lock()
		Manager = originalManager
		timerConfigMu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	runDone := make(chan struct{})
	go func() {
		manager.Run(ctx, ticks)
		close(runDone)
	}()
	var releaseOnce sync.Once
	release := make(chan struct{})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("timer manager did not stop")
		}
	})

	callbackStarted := make(chan struct{})
	timer := manager.NewTimerWithPolicy(func() {
		close(callbackStarted)
		<-release
	}, time.Nanosecond, 1, ExecutionSerial)
	timer.createAt = 0
	waitForTimer(t, manager, timer)

	cronDone := make(chan struct{})
	cronAccepted := make(chan bool, 1)
	go func() {
		cronAccepted <- TryCron()
		close(cronDone)
	}()

	select {
	case <-cronDone:
		// Rejection is valid while Run owns the manager. It must not start work.
		assert.False(t, <-cronAccepted)
		select {
		case <-callbackStarted:
			t.Fatal("Cron returned while its serial callback was still running")
		default:
		}
		cancel()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Fatal("timer manager did not stop after Cron was rejected")
		}
	case <-callbackStarted:
		cancel()
		returnedEarly := false
		select {
		case <-runDone:
			returnedEarly = true
		case <-time.After(25 * time.Millisecond):
		}
		releaseOnce.Do(func() { close(release) })
		select {
		case <-cronDone:
		case <-time.After(time.Second):
			t.Fatal("Cron did not finish after its callback returned")
		}
		assert.True(t, <-cronAccepted)
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Fatal("timer manager did not stop after Cron finished")
		}
		assert.False(t, returnedEarly, "Run returned while a Cron-owned serial callback was still using timer dependencies")
	case <-time.After(time.Second):
		t.Fatal("Cron neither rejected the competing call nor started its callback")
	}
}

func TestConditionPanicDoesNotStopManager(t *testing.T) {
	manager := NewTimerManager(16)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticks := make(chan time.Time, 2)
	runResult := make(chan any, 1)
	go func() {
		var recovered any
		defer func() { runResult <- recovered }()
		defer func() { recovered = recover() }()
		manager.Run(ctx, ticks)
	}()

	badTimer := manager.NewTimerWithPolicy(func() {}, time.Nanosecond, LoopForever, ExecutionSerial)
	badTimer.SetCondition(&panicCondition{})
	waitForTimer(t, manager, badTimer)
	ticks <- time.Now()
	select {
	case recovered := <-runResult:
		t.Fatalf("condition panic stopped the timer manager: %v", recovered)
	case <-time.After(100 * time.Millisecond):
	}

	callbackDone := make(chan struct{})
	goodTimer := manager.NewTimerWithPolicy(func() { close(callbackDone) }, time.Nanosecond, 1, ExecutionSerial)
	goodTimer.createAt = 0
	waitForTimer(t, manager, goodTimer)
	ticks <- time.Now()
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("timer manager did not execute a healthy timer after a condition panic")
	}
	cancel()
	select {
	case recovered := <-runResult:
		assert.Nil(t, recovered)
	case <-time.After(time.Second):
		t.Fatal("timer manager did not stop after cancellation")
	}
}

func TestConditionPanicValueCannotPanicDuringLogging(t *testing.T) {
	manager := NewTimerManager(16)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticks := make(chan time.Time, 2)
	runResult := make(chan any, 1)
	go func() {
		var recovered any
		defer func() { runResult <- recovered }()
		defer func() { recovered = recover() }()
		manager.Run(ctx, ticks)
	}()

	badTimer := manager.NewTimerWithPolicy(func() {}, time.Nanosecond, LoopForever, ExecutionSerial)
	badTimer.SetCondition(panicLoggingCondition{})
	waitForTimer(t, manager, badTimer)
	ticks <- time.Now()
	select {
	case recovered := <-runResult:
		t.Fatalf("logging a condition panic stopped the timer manager: %v", recovered)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case recovered := <-runResult:
		assert.Nil(t, recovered)
	case <-time.After(time.Second):
		t.Fatal("timer manager did not stop after cancellation")
	}
}

func TestConditionCanFillMutationQueueWithoutDeadlockingOwner(t *testing.T) {
	manager := NewTimerManager(16)
	ticks, cancel := startTestManager(t, manager)
	done := make(chan struct{})
	condition := &timerCreatingCondition{
		manager: manager,
		count:   cap(manager.ChCreatedTimer) + 1,
		done:    done,
	}
	conditionTimer := manager.NewTimerWithPolicy(func() {}, time.Nanosecond, LoopForever, ExecutionSerial)
	conditionTimer.SetCondition(condition)
	waitForTimer(t, manager, conditionTimer)
	ticks <- time.Now()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("condition deadlocked while creating timers")
	}
	cancel()
}

func TestCanceledStandbyDoesNotWaitForIndependentRunOwner(t *testing.T) {
	manager := NewTimerManager(16)
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerTicks := make(chan time.Time, 1)
	ownerDone := make(chan struct{})
	go func() {
		manager.Run(ownerCtx, ownerTicks)
		close(ownerDone)
	}()
	t.Cleanup(func() {
		cancelOwner()
		select {
		case <-ownerDone:
		case <-time.After(time.Second):
			t.Error("independent Run owner did not stop")
		}
	})

	callbackDone := make(chan struct{})
	ownedTimer := manager.NewTimerWithPolicy(func() { close(callbackDone) }, time.Nanosecond, 1, ExecutionSerial)
	ownedTimer.createAt = 0
	waitForTimer(t, manager, ownedTimer)
	ownerTicks <- time.Now()
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("first Run did not acquire manager ownership")
	}

	standbyCtx, cancelStandby := context.WithCancel(context.Background())
	standbyDone := make(chan struct{})
	go func() {
		manager.Run(standbyCtx, make(chan time.Time))
		close(standbyDone)
	}()
	cancelStandby()
	select {
	case <-standbyDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("canceled standby waited for an independent Run owner")
	}
}

func TestRunCancellationWaitsForPreexistingCronOwner(t *testing.T) {
	manager := NewTimerManager(16)
	timerConfigMu.Lock()
	originalManager := Manager
	Manager = manager
	timerConfigMu.Unlock()
	t.Cleanup(func() {
		timerConfigMu.Lock()
		Manager = originalManager
		timerConfigMu.Unlock()
	})

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	timer := manager.NewTimerWithPolicy(func() {
		close(callbackStarted)
		<-releaseCallback
	}, time.Nanosecond, 1, ExecutionSerial)
	timer.createAt = 0

	cronDone := make(chan bool, 1)
	go func() { cronDone <- TryCron() }()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("Cron did not acquire the idle manager")
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		manager.Run(ctx, make(chan time.Time))
		close(runDone)
	}()
	cancel()
	select {
	case <-runDone:
		t.Fatal("Run returned while a preexisting Cron owner was still executing")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseCallback)
	select {
	case accepted := <-cronDone:
		assert.True(t, accepted)
	case <-time.After(time.Second):
		t.Fatal("Cron did not return after its callback completed")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after the preexisting Cron owner completed")
	}
}

func TestSetTimerBacklog(t *testing.T) {
	timerConfigMu.Lock()
	originalBacklog, originalManager := timerBacklog, Manager
	timerBacklog = defaultTimerBacklog
	Manager = newTimerManager(defaultTimerBacklog)
	timerConfigMu.Unlock()
	t.Cleanup(func() {
		timerConfigMu.Lock()
		timerBacklog, Manager = originalBacklog, originalManager
		timerConfigMu.Unlock()
	})

	assert.Equal(t, defaultTimerBacklog, timerBacklog)
	SetTimerBacklog(10)
	assert.Equal(t, 16, timerBacklog)
	assert.Equal(t, 16, cap(Manager.ChCreatedTimer))
	assert.Equal(t, 16, cap(NewTimerManager(0).ChCreatedTimer))
	SetTimerBacklog(32)
	assert.Equal(t, 32, timerBacklog)
	assert.Equal(t, 32, cap(Manager.ChClosingTimer))
	assert.Equal(t, 32, cap(NewTimerManager(0).ChClosingTimer))

	Manager.NewTimerWithPolicy(func() {}, time.Hour, 1, ExecutionSerial)
	assert.PanicsWithValue(t, "pitaya/timer: timer backlog cannot change after manager use", func() {
		SetTimerBacklog(64)
	})
}
