package cluster

import (
	"errors"
	"sync"

	nats "github.com/nats-io/nats.go"
)

var errNatsUserSubscriptionsClosed = errors.New("nats user subscriptions are closed")

type natsUserSubscriptionEntry struct {
	sessionID int64
	uid       string
	push      *nats.Subscription
	kick      *nats.Subscription
	ready     chan struct{}
	err       error
	active    bool
}

type natsUserSubscriptionRegistry struct {
	mu        sync.Mutex
	closed    bool
	entries   map[int64]*natsUserSubscriptionEntry
	createOps sync.WaitGroup
}

func newNatsUserSubscriptionRegistry() *natsUserSubscriptionRegistry {
	return &natsUserSubscriptionRegistry{
		entries: make(map[int64]*natsUserSubscriptionEntry),
	}
}

func (r *natsUserSubscriptionRegistry) acquire(
	sessionID int64,
	uid string,
	create func(*natsUserSubscriptionEntry) (*nats.Subscription, *nats.Subscription, error),
	drain func(*nats.Subscription),
) (bool, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false, errNatsUserSubscriptionsClosed
	}
	var replaced *natsUserSubscriptionEntry
	if current := r.entries[sessionID]; current != nil {
		if current.uid == uid {
			ready := current.ready
			r.mu.Unlock()
			<-ready
			if current.err != nil {
				return false, current.err
			}
			if !r.isActive(current) {
				return false, errNatsUserSubscriptionsClosed
			}
			return true, nil
		}

		// A different UID can only reach this point after an earlier bind attempt
		// for the same session rolled back. Replace that stale pair atomically so
		// the retry is not pinned to the failed UID.
		delete(r.entries, sessionID)
		current.active = false
		replaced = current
	}

	entry := &natsUserSubscriptionEntry{
		sessionID: sessionID,
		uid:       uid,
		ready:     make(chan struct{}),
	}
	r.entries[sessionID] = entry
	r.createOps.Add(1)
	r.mu.Unlock()
	r.drainReady(replaced, drain)

	push, kick, createErr := create(entry)

	r.mu.Lock()
	publish := createErr == nil && !r.closed && r.entries[sessionID] == entry
	if publish {
		entry.push = push
		entry.kick = kick
		entry.active = true
	} else {
		if r.entries[sessionID] == entry {
			delete(r.entries, sessionID)
		}
		if createErr == nil {
			createErr = errNatsUserSubscriptionsClosed
		}
	}
	entry.err = createErr
	close(entry.ready)
	r.mu.Unlock()

	if !publish {
		drain(push)
		drain(kick)
	}
	r.createOps.Done()
	return false, createErr
}

func (r *natsUserSubscriptionRegistry) remove(sessionID int64, drain func(*nats.Subscription)) {
	r.mu.Lock()
	entry := r.entries[sessionID]
	if entry != nil {
		delete(r.entries, sessionID)
		entry.active = false
	}
	r.mu.Unlock()
	if entry == nil {
		return
	}
	r.drainReady(entry, drain)
}

func (r *natsUserSubscriptionRegistry) drainReady(entry *natsUserSubscriptionEntry, drain func(*nats.Subscription)) {
	if entry == nil {
		return
	}
	select {
	case <-entry.ready:
		drain(entry.push)
		drain(entry.kick)
	default:
		// The creator observes removal before publishing and drains its own pair.
	}
}

func (r *natsUserSubscriptionRegistry) closeAdmission() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()

	// No createOps.Add can happen after closed becomes visible under the same lock.
	r.createOps.Wait()

	r.mu.Lock()
	for sessionID, entry := range r.entries {
		entry.active = false
		delete(r.entries, sessionID)
	}
	r.mu.Unlock()
}

func (r *natsUserSubscriptionRegistry) isActive(entry *natsUserSubscriptionEntry) bool {
	if entry == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && entry.active && r.entries[entry.sessionID] == entry
}

func (r *natsUserSubscriptionRegistry) activeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, entry := range r.entries {
		if entry.active {
			count++
		}
	}
	return count
}
