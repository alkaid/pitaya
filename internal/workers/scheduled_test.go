package workers

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/require"
)

type atomicMoveBackend struct {
	mu                sync.Mutex
	original          string
	zsetHasJob        bool
	queuePayload      string
	zsetKey           string
	queueKey          string
	applyBeforeCancel bool
	evalStarted       chan struct{}
	applied           chan struct{}
	startOnce         sync.Once
	applyOnce         sync.Once
}

type atomicMoveRedisConn struct {
	backend *atomicMoveBackend
}

func (c *atomicMoveRedisConn) Close() error { return nil }
func (c *atomicMoveRedisConn) Err() error   { return nil }
func (c *atomicMoveRedisConn) Flush() error { return nil }
func (c *atomicMoveRedisConn) Receive() (any, error) {
	return nil, redis.ErrNil
}
func (c *atomicMoveRedisConn) ReceiveContext(ctx context.Context) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *atomicMoveRedisConn) Send(string, ...any) error { return nil }
func (c *atomicMoveRedisConn) Do(commandName string, args ...any) (any, error) {
	return c.DoContext(context.Background(), commandName, args...)
}

func (c *atomicMoveRedisConn) DoContext(ctx context.Context, commandName string, args ...any) (any, error) {
	b := c.backend
	switch strings.ToLower(commandName) {
	case "zrangebyscore":
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.zsetHasJob {
			return []any{}, nil
		}
		return []any{b.original}, nil
	case "eval":
		b.mu.Lock()
		b.zsetKey, _ = args[2].(string)
		b.queueKey, _ = args[3].(string)
		original, _ := args[4].(string)
		payload, _ := args[5].(string)
		b.mu.Unlock()
		b.startOnce.Do(func() { close(b.evalStarted) })
		if b.applyBeforeCancel {
			b.mu.Lock()
			if b.zsetHasJob && original == b.original {
				b.zsetHasJob = false
				b.queuePayload = payload
			}
			b.mu.Unlock()
			b.applyOnce.Do(func() { close(b.applied) })
		}
		<-ctx.Done()
		return nil, ctx.Err()
	default:
		return nil, nil
	}
}

func TestScheduledMoveIsAtomicAcrossCancellationAndAmbiguousReturn(t *testing.T) {
	const original = `{"jid":"scheduled-1","queue":"rpc","args":{}}`
	tests := []struct {
		name               string
		applyBeforeCancel  bool
		wantInSortedSet    bool
		wantInDestinationQ bool
	}{
		{
			name:               "cancel before eval applies",
			wantInSortedSet:    true,
			wantInDestinationQ: false,
		},
		{
			name:               "cancel after eval applies but before reply",
			applyBeforeCancel:  true,
			wantInSortedSet:    false,
			wantInDestinationQ: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &atomicMoveBackend{
				original:          original,
				zsetHasJob:        true,
				applyBeforeCancel: tt.applyBeforeCancel,
				evalStarted:       make(chan struct{}),
				applied:           make(chan struct{}),
			}
			pool := &redis.Pool{
				MaxIdle: 1,
				Dial: func() (redis.Conn, error) {
					return &atomicMoveRedisConn{backend: backend}, nil
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan bool, 1)
			go func() {
				done <- newScheduled(RETRY_KEY).poll(ctx, pool, "ns:")
			}()

			select {
			case <-backend.evalStarted:
			case <-time.After(time.Second):
				t.Fatal("scheduler did not issue the atomic Redis move")
			}
			if tt.applyBeforeCancel {
				select {
				case <-backend.applied:
				case <-time.After(time.Second):
					t.Fatal("server-side atomic move did not apply")
				}
			}
			cancel()
			select {
			case keepPolling := <-done:
				require.False(t, keepPolling)
			case <-time.After(time.Second):
				t.Fatal("scheduler did not return after cancellation")
			}

			backend.mu.Lock()
			inSortedSet := backend.zsetHasJob
			inDestinationQueue := backend.queuePayload != ""
			zsetKey := backend.zsetKey
			queueKey := backend.queueKey
			backend.mu.Unlock()
			require.Equal(t, tt.wantInSortedSet, inSortedSet)
			require.Equal(t, tt.wantInDestinationQ, inDestinationQueue)
			require.NotEqual(t, inSortedSet, inDestinationQueue, "job must remain in exactly one Redis location")
			require.Equal(t, "ns:"+RETRY_KEY, zsetKey)
			require.Equal(t, "ns:queue:rpc", queueKey)
		})
	}
}
