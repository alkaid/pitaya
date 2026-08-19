package workers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/require"
)

type retryFailureBackend struct {
	zaddAttempted chan struct{}
	zaddOnce      sync.Once
	zaddCalls     atomic.Int32
	zaddErr       error
	lremCalls     atomic.Int32
}

type retryFailureConn struct {
	backend *retryFailureBackend
}

func (c *retryFailureConn) Close() error { return nil }
func (c *retryFailureConn) Err() error   { return nil }
func (c *retryFailureConn) Flush() error { return nil }
func (c *retryFailureConn) Receive() (any, error) {
	return nil, redis.ErrNil
}
func (c *retryFailureConn) ReceiveContext(context.Context) (any, error) {
	return nil, redis.ErrNil
}
func (c *retryFailureConn) Send(string, ...any) error { return nil }

func (c *retryFailureConn) Do(commandName string, _ ...any) (any, error) {
	switch strings.ToLower(commandName) {
	case "lrange", "zrangebyscore":
		return []any{}, nil
	case "brpoplpush":
		return nil, redis.ErrNil
	case "exec":
		return []any{}, nil
	case "zadd":
		c.backend.zaddCalls.Add(1)
		if c.backend.zaddAttempted != nil {
			c.backend.zaddOnce.Do(func() { close(c.backend.zaddAttempted) })
		}
		if c.backend.zaddErr != nil {
			return nil, c.backend.zaddErr
		}
		return 1, nil
	case "lrem":
		c.backend.lremCalls.Add(1)
		return 1, nil
	default:
		return nil, nil
	}
}

func (c *retryFailureConn) DoContext(ctx context.Context, commandName string, args ...any) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return c.Do(commandName, args...)
	}
}

func TestRetryEnqueueFailureDoesNotAcknowledgeOriginalJob(t *testing.T) {
	configureIdleRuntime(t)
	backend := &retryFailureBackend{
		zaddAttempted: make(chan struct{}),
		zaddErr:       errors.New("retry zadd failed"),
	}
	Config.Pool = &redis.Pool{
		MaxIdle: 4,
		Dial: func() (redis.Conn, error) {
			return &retryFailureConn{backend: backend}, nil
		},
	}

	Process("rpc", func(*Msg) { panic("rpc failed") }, 1)
	manager := managers["rpc"]
	Start()

	message, err := NewMsg(`{"jid":"retry-failure","retry":true,"args":{}}`)
	require.NoError(t, err)
	go func() { manager.fetch.Messages() <- message }()

	select {
	case <-backend.zaddAttempted:
	case <-time.After(time.Second):
		t.Fatal("retry middleware did not attempt to enqueue the failed job")
	}
	require.Eventually(t, func() bool {
		return !manager.workers[0].processing()
	}, time.Second, time.Millisecond)
	require.Zero(t, backend.lremCalls.Load(), "retry enqueue failure acknowledged the original job")
}

func TestQuitCancellationDoesNotAcknowledgeWhenRetryEnqueueFails(t *testing.T) {
	configureIdleRuntime(t)
	backend := &retryFailureBackend{
		zaddAttempted: make(chan struct{}),
		zaddErr:       errors.New("retry zadd failed"),
	}
	Config.Pool = &redis.Pool{
		MaxIdle: 4,
		Dial: func() (redis.Conn, error) {
			return &retryFailureConn{backend: backend}, nil
		},
	}

	jobStarted := make(chan struct{})
	Process("rpc", func(*Msg) {
		close(jobStarted)
		<-runtimeCtx.Done()
		panic("rpc canceled")
	}, 1)
	manager := managers["rpc"]
	Start()

	message, err := NewMsg(`{"jid":"retry-shutdown","retry":true,"args":{}}`)
	require.NoError(t, err)
	go func() { manager.fetch.Messages() <- message }()

	select {
	case <-jobStarted:
	case <-time.After(time.Second):
		t.Fatal("worker did not accept the job")
	}

	quitDone := make(chan struct{})
	go func() {
		Quit()
		close(quitDone)
	}()

	select {
	case <-backend.zaddAttempted:
	case <-time.After(time.Second):
		t.Fatal("canceled job did not attempt retry enqueue")
	}
	select {
	case <-quitDone:
	case <-time.After(time.Second):
		t.Fatal("Quit did not complete after the canceled job declined acknowledgement")
	}
	require.Zero(t, backend.lremCalls.Load(), "shutdown acknowledged a canceled job whose retry enqueue failed")
}

func TestRetryEnqueueSuccessAcknowledgesOriginalJob(t *testing.T) {
	configureIdleRuntime(t)
	backend := &retryFailureBackend{zaddAttempted: make(chan struct{})}
	Config.Pool = &redis.Pool{
		MaxIdle: 4,
		Dial: func() (redis.Conn, error) {
			return &retryFailureConn{backend: backend}, nil
		},
	}

	Process("rpc", func(*Msg) { panic("rpc failed") }, 1)
	manager := managers["rpc"]
	Start()

	message, err := NewMsg(`{"jid":"retry-success","retry":true,"args":{}}`)
	require.NoError(t, err)
	go func() { manager.fetch.Messages() <- message }()

	select {
	case <-backend.zaddAttempted:
	case <-time.After(time.Second):
		t.Fatal("retry middleware did not enqueue the failed job")
	}
	require.Eventually(t, func() bool {
		return backend.lremCalls.Load() == 1
	}, time.Second, time.Millisecond)
	require.Equal(t, int32(1), backend.zaddCalls.Load())
}

func TestFractionalRetryMaxRandAcknowledgesOnlyAfterRetryEnqueue(t *testing.T) {
	configureIdleRuntime(t)
	backend := &retryFailureBackend{zaddAttempted: make(chan struct{})}
	Config.Pool = &redis.Pool{
		MaxIdle: 4,
		Dial: func() (redis.Conn, error) {
			return &retryFailureConn{backend: backend}, nil
		},
	}

	Process("rpc", func(*Msg) { panic("rpc failed") }, 1)
	manager := managers["rpc"]
	Start()

	message, err := NewMsg(`{"jid":"retry-fractional-jitter","retry":true,"retry_options":{"max_rand":0.5},"args":{}}`)
	require.NoError(t, err)
	go func() { manager.fetch.Messages() <- message }()

	select {
	case <-backend.zaddAttempted:
	case <-time.After(time.Second):
		t.Fatal("fractional retry jitter prevented retry enqueue")
	}
	require.Eventually(t, func() bool {
		return backend.lremCalls.Load() == 1
	}, time.Second, time.Millisecond)
	require.Equal(t, int32(1), backend.zaddCalls.Load())
}

func TestRetryDisabledAcknowledgesOriginalJob(t *testing.T) {
	configureIdleRuntime(t)
	backend := &retryFailureBackend{}
	Config.Pool = &redis.Pool{
		MaxIdle: 4,
		Dial: func() (redis.Conn, error) {
			return &retryFailureConn{backend: backend}, nil
		},
	}

	Process("rpc", func(*Msg) { panic("rpc failed") }, 1)
	manager := managers["rpc"]
	Start()

	message, err := NewMsg(`{"jid":"retry-disabled","retry":false,"args":{}}`)
	require.NoError(t, err)
	go func() { manager.fetch.Messages() <- message }()

	require.Eventually(t, func() bool {
		return backend.lremCalls.Load() == 1
	}, time.Second, time.Millisecond)
	require.Zero(t, backend.zaddCalls.Load())
}
