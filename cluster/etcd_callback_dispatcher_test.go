package cluster

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitCallbackWork(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for callback work")
	}
}

func TestCallbackDispatcherSingleOwnerPreservesFIFO(t *testing.T) {
	t.Parallel()
	dispatcher := newCallbackDispatcher(32)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(release)

	var active atomic.Int32
	var maxActive atomic.Int32
	var orderLock sync.Mutex
	order := make([]int, 0, 17)
	callback := func(index int, block bool) func() {
		return func() {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				maximum := maxActive.Load()
				if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
					break
				}
			}
			orderLock.Lock()
			order = append(order, index)
			orderLock.Unlock()
			if block {
				close(firstStarted)
				<-releaseFirst
			}
		}
	}

	firstDone, accepted := dispatcher.enqueue(callback(0, true))
	require.True(t, accepted)
	<-firstStarted
	done := []<-chan struct{}{firstDone}
	for index := 1; index <= 16; index++ {
		workDone, accepted := dispatcher.enqueue(callback(index, false))
		require.True(t, accepted)
		done = append(done, workDone)
	}
	release()
	for _, workDone := range done {
		waitCallbackWork(t, workDone)
	}

	expected := make([]int, 17)
	for index := range expected {
		expected[index] = index
	}
	assert.Equal(t, expected, order)
	assert.Equal(t, int32(1), maxActive.Load(), "one lane must never execute callbacks concurrently")
	state := dispatcher.snapshot()
	assert.True(t, state.started)
	assert.False(t, state.stopping)
	assert.Zero(t, state.queued)
	assert.Zero(t, state.waiters)

	dispatcher.stop()
	waitCallbackWork(t, dispatcher.drainedSignal())
	assert.True(t, dispatcher.snapshot().stopping)
}

func TestCallbackDispatcherStopWakesProducerAndDrainsAcceptedWork(t *testing.T) {
	t.Parallel()
	dispatcher := newCallbackDispatcher(1)
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)

	blockerDone, accepted := dispatcher.enqueue(func() {
		close(callbackStarted)
		<-releaseCallback
	})
	require.True(t, accepted)
	<-callbackStarted
	queuedDone, accepted := dispatcher.enqueue(nil)
	require.True(t, accepted)

	type enqueueResult struct {
		done     <-chan struct{}
		accepted bool
	}
	producerResult := make(chan enqueueResult, 1)
	go func() {
		done, accepted := dispatcher.enqueue(nil)
		producerResult <- enqueueResult{done: done, accepted: accepted}
	}()
	require.Eventually(t, func() bool {
		return dispatcher.snapshot().waiters == 1
	}, time.Second, time.Millisecond)

	dispatcher.stop()
	select {
	case result := <-producerResult:
		assert.False(t, result.accepted)
		assert.Nil(t, result.done)
	case <-time.After(time.Second):
		t.Fatal("stopping dispatcher did not wake blocked producer")
	}
	lateDone, lateAccepted := dispatcher.enqueue(nil)
	assert.False(t, lateAccepted)
	assert.Nil(t, lateDone)
	select {
	case <-dispatcher.drainedSignal():
		t.Fatal("dispatcher drained before accepted callbacks completed")
	default:
	}

	release()
	waitCallbackWork(t, blockerDone)
	waitCallbackWork(t, queuedDone)
	waitCallbackWork(t, dispatcher.drainedSignal())
	state := dispatcher.snapshot()
	assert.True(t, state.stopping)
	assert.Zero(t, state.queued)
	assert.Zero(t, state.waiters)
}

func TestCallbackDispatcherPanicClosesBarrierAndContinues(t *testing.T) {
	dispatcher := newCallbackDispatcher(4)
	panicDone, accepted := dispatcher.enqueue(func() { panic("listener failure") })
	require.True(t, accepted)
	var nextCalls atomic.Int64
	nextDone, accepted := dispatcher.enqueue(func() { nextCalls.Add(1) })
	require.True(t, accepted)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, dispatcher.waitForBarrier(ctx, panicDone, true))
	require.NoError(t, dispatcher.waitForBarrier(ctx, nextDone, true))
	assert.Equal(t, int64(1), nextCalls.Load())

	dispatcher.stop()
	require.NoError(t, dispatcher.waitForBarrier(ctx, nil, false))
}

type callbackTestSDListener struct {
	add    func(*Server)
	remove func(*Server)
	modify func(*Server, *Server)
}

func (l callbackTestSDListener) AddServer(server *Server) {
	l.add(server)
}

func (l callbackTestSDListener) RemoveServer(server *Server) {
	l.remove(server)
}

func (l callbackTestSDListener) ModifyServer(server, old *Server) {
	l.modify(server, old)
}

func TestTopologyListenerPanicDoesNotSkipRemainingListeners(t *testing.T) {
	dispatcher := newCallbackDispatcher(8)
	var callsMu sync.Mutex
	calls := make([]Action, 0, 3)
	panicking := callbackTestSDListener{
		add:    func(*Server) { panic("add") },
		remove: func(*Server) { panic("remove") },
		modify: func(*Server, *Server) { panic("modify") },
	}
	recording := callbackTestSDListener{
		add: func(*Server) {
			callsMu.Lock()
			calls = append(calls, ADD)
			callsMu.Unlock()
		},
		remove: func(*Server) {
			callsMu.Lock()
			calls = append(calls, DEL)
			callsMu.Unlock()
		},
		modify: func(*Server, *Server) {
			callsMu.Lock()
			calls = append(calls, Modify)
			callsMu.Unlock()
		},
	}
	sd := &etcdServiceDiscovery{watch: etcdWatchOwner{
		callbacks: dispatcher,
		listeners: []SDListener{panicking, recording},
	}}
	server := &Server{ID: "new", Type: "backend"}
	oldServer := &Server{ID: "old", Type: "backend"}
	sd.notifyListeners(ADD, server)
	sd.notifyListeners(DEL, server)
	sd.notifyListeners(Modify, server, oldServer)
	barrier, accepted := sd.enqueueCallback(nil)
	require.True(t, accepted)
	waitCallbackWork(t, barrier)

	callsMu.Lock()
	assert.Equal(t, []Action{ADD, DEL, Modify}, calls)
	callsMu.Unlock()
	dispatcher.stop()
	waitCallbackWork(t, dispatcher.drainedSignal())
}

func TestLeaderListenerPanicDoesNotStopLaterGenerations(t *testing.T) {
	dispatcher := newCallbackDispatcher(4)
	sd := &etcdServiceDiscovery{election: etcdElectionOwner{
		callbacks:  dispatcher,
		generation: 1,
	}}
	sd.OnLeaderChange(func(string) { panic("leader listener") })
	require.True(t, sd.notifyLeaderChange(1, "leader-1", false))
	barrier, accepted := sd.enqueueLeaderCallback(nil)
	require.True(t, accepted)
	waitCallbackWork(t, barrier)
	assert.Equal(t, "leader-1", sd.LeaderID())

	called := make(chan string, 1)
	sd.OnLeaderChange(func(leader string) { called <- leader })
	require.True(t, sd.activateLeaderGeneration(2))
	require.True(t, sd.notifyLeaderChange(2, "leader-2", false))
	barrier, accepted = sd.enqueueLeaderCallback(nil)
	require.True(t, accepted)
	waitCallbackWork(t, barrier)
	assert.Equal(t, "leader-2", <-called)
	assert.Equal(t, "leader-2", sd.LeaderID())

	dispatcher.stop()
	waitCallbackWork(t, dispatcher.drainedSignal())
}
