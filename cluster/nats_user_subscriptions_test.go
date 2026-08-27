package cluster

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNatsUserSubscriptionRegistryConcurrentAcquireReusesPair(t *testing.T) {
	registry := newNatsUserSubscriptionRegistry()
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	var creates atomic.Int32
	create := func(*natsUserSubscriptionEntry) (*nats.Subscription, *nats.Subscription, error) {
		if creates.Add(1) == 1 {
			close(createStarted)
		}
		<-releaseCreate
		return nil, nil, nil
	}

	type result struct {
		reused bool
		err    error
	}
	results := make(chan result, 2)
	go func() {
		reused, err := registry.acquire(7, "user", create, func(*nats.Subscription) {})
		results <- result{reused: reused, err: err}
	}()
	<-createStarted
	go func() {
		reused, err := registry.acquire(7, "user", create, func(*nats.Subscription) {})
		results <- result{reused: reused, err: err}
	}()
	close(releaseCreate)

	first := <-results
	second := <-results
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	assert.NotEqual(t, first.reused, second.reused)
	assert.Equal(t, int32(1), creates.Load())
	assert.Equal(t, 1, registry.activeCount())
}

func TestNatsUserSubscriptionRegistryCloseWaitsForCreator(t *testing.T) {
	registry := newNatsUserSubscriptionRegistry()
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	acquireDone := make(chan error, 1)
	go func() {
		_, err := registry.acquire(9, "user", func(*natsUserSubscriptionEntry) (*nats.Subscription, *nats.Subscription, error) {
			close(createStarted)
			<-releaseCreate
			return nil, nil, nil
		}, func(*nats.Subscription) {})
		acquireDone <- err
	}()
	<-createStarted

	closeDone := make(chan struct{})
	go func() {
		registry.closeAdmission()
		close(closeDone)
	}()
	require.Eventually(t, func() bool {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		return registry.closed
	}, time.Second, time.Millisecond)
	select {
	case <-closeDone:
		t.Fatal("registry close returned before the admitted creator finished")
	default:
	}
	close(releaseCreate)
	require.ErrorIs(t, <-acquireDone, errNatsUserSubscriptionsClosed)
	<-closeDone
	assert.Equal(t, 0, registry.activeCount())
}

func TestNatsUserSubscriptionRegistryDrainsPartialCreate(t *testing.T) {
	registry := newNatsUserSubscriptionRegistry()
	wantErr := errors.New("kick subscribe failed")
	push := &nats.Subscription{}
	var drainedMu sync.Mutex
	var drained []*nats.Subscription

	_, err := registry.acquire(11, "user", func(*natsUserSubscriptionEntry) (*nats.Subscription, *nats.Subscription, error) {
		return push, nil, wantErr
	}, func(sub *nats.Subscription) {
		if sub == nil {
			return
		}
		drainedMu.Lock()
		drained = append(drained, sub)
		drainedMu.Unlock()
	})
	require.ErrorIs(t, err, wantErr)
	drainedMu.Lock()
	assert.Equal(t, []*nats.Subscription{push}, drained)
	drainedMu.Unlock()
	assert.Equal(t, 0, registry.activeCount())
}

func TestNatsUserSubscriptionRegistryReplacesRolledBackUIDForSameSession(t *testing.T) {
	registry := newNatsUserSubscriptionRegistry()
	oldPush := &nats.Subscription{}
	oldKick := &nats.Subscription{}
	newPush := &nats.Subscription{}
	newKick := &nats.Subscription{}
	var drained []*nats.Subscription
	drain := func(sub *nats.Subscription) {
		if sub != nil {
			drained = append(drained, sub)
		}
	}

	reused, err := registry.acquire(17, "old-user", func(*natsUserSubscriptionEntry) (*nats.Subscription, *nats.Subscription, error) {
		return oldPush, oldKick, nil
	}, drain)
	require.NoError(t, err)
	assert.False(t, reused)

	reused, err = registry.acquire(17, "new-user", func(*natsUserSubscriptionEntry) (*nats.Subscription, *nats.Subscription, error) {
		return newPush, newKick, nil
	}, drain)
	require.NoError(t, err)
	assert.False(t, reused)
	assert.Equal(t, []*nats.Subscription{oldPush, oldKick}, drained)
	assert.Equal(t, 1, registry.activeCount())

	registry.mu.Lock()
	entry := registry.entries[17]
	registry.mu.Unlock()
	require.NotNil(t, entry)
	assert.Equal(t, "new-user", entry.uid)
	assert.Same(t, newPush, entry.push)
	assert.Same(t, newKick, entry.kick)
}

func TestNatsUserSubscriptionRegistryRejectsClosedAdmission(t *testing.T) {
	registry := newNatsUserSubscriptionRegistry()
	registry.closeAdmission()

	reused, err := registry.acquire(21, "user", func(*natsUserSubscriptionEntry) (*nats.Subscription, *nats.Subscription, error) {
		t.Fatal("closed registry invoked create")
		return nil, nil, nil
	}, func(*nats.Subscription) {})
	assert.False(t, reused)
	assert.ErrorIs(t, err, errNatsUserSubscriptionsClosed)
	assert.True(t, registry.isActive(nil))
}

func TestNatsUserSubscriptionRegistryPropagatesCompletedReuseFailure(t *testing.T) {
	registry := newNatsUserSubscriptionRegistry()
	wantErr := errors.New("subscription create failed")
	ready := make(chan struct{})
	close(ready)
	registry.entries[22] = &natsUserSubscriptionEntry{
		sessionID: 22,
		uid:       "user",
		ready:     ready,
		err:       wantErr,
	}

	reused, err := registry.acquire(22, "user", func(*natsUserSubscriptionEntry) (*nats.Subscription, *nats.Subscription, error) {
		t.Fatal("completed failed entry invoked create")
		return nil, nil, nil
	}, func(*nats.Subscription) {})
	assert.False(t, reused)
	assert.ErrorIs(t, err, wantErr)

	registry.entries[22].err = nil
	reused, err = registry.acquire(22, "user", func(*natsUserSubscriptionEntry) (*nats.Subscription, *nats.Subscription, error) {
		t.Fatal("inactive entry invoked create")
		return nil, nil, nil
	}, func(*nats.Subscription) {})
	assert.False(t, reused)
	assert.ErrorIs(t, err, errNatsUserSubscriptionsClosed)
}
