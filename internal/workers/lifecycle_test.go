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

type idleRedisBackend struct {
	pollSeen chan struct{}
	pollOnce sync.Once
}

type idleRedisConn struct {
	backend *idleRedisBackend
}

func (c *idleRedisConn) Close() error { return nil }
func (c *idleRedisConn) Err() error   { return nil }
func (c *idleRedisConn) Flush() error { return nil }
func (c *idleRedisConn) Receive() (any, error) {
	return nil, redis.ErrNil
}
func (c *idleRedisConn) ReceiveContext(context.Context) (any, error) {
	return nil, redis.ErrNil
}
func (c *idleRedisConn) Send(string, ...any) error { return nil }

func (c *idleRedisConn) Do(commandName string, _ ...any) (any, error) {
	switch strings.ToLower(commandName) {
	case "lrange":
		return []any{}, nil
	case "zrangebyscore":
		c.backend.pollOnce.Do(func() { close(c.backend.pollSeen) })
		return []any{}, nil
	case "brpoplpush":
		return nil, redis.ErrNil
	default:
		return nil, nil
	}
}

func (c *idleRedisConn) DoContext(ctx context.Context, commandName string, args ...any) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return c.Do(commandName, args...)
	}
}

type blockingRedisBackend struct {
	saddStarted       chan struct{}
	zrangeStarted     chan struct{}
	lrangeStarted     chan struct{}
	releaseSadd       chan struct{}
	saddOnce          sync.Once
	zrangeOnce        sync.Once
	lrangeOnce        sync.Once
	activeContextIO   atomic.Int32
	finishedContextIO atomic.Int32
}

func newBlockingRedisBackend() *blockingRedisBackend {
	return &blockingRedisBackend{
		saddStarted:   make(chan struct{}),
		zrangeStarted: make(chan struct{}),
		lrangeStarted: make(chan struct{}),
		releaseSadd:   make(chan struct{}),
	}
}

type blockingRedisConn struct {
	backend *blockingRedisBackend
}

func (c *blockingRedisConn) Close() error { return nil }
func (c *blockingRedisConn) Err() error   { return nil }
func (c *blockingRedisConn) Flush() error { return nil }
func (c *blockingRedisConn) Receive() (any, error) {
	return nil, redis.ErrNil
}
func (c *blockingRedisConn) ReceiveContext(ctx context.Context) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestTryFetchMessageReturnsAfterPoolDialFailure(t *testing.T) {
	dialErr := errors.New("synthetic Redis dial failure")
	dialed := false
	fetcher := &fetch{
		ctx: context.Background(),
		pool: &redis.Pool{
			Dial: func() (redis.Conn, error) {
				dialed = true
				return nil, dialErr
			},
		},
	}

	fetcher.tryFetchMessage()
	require.True(t, dialed)
}

type inprogressRedisBackend struct {
	mu           sync.Mutex
	queued       string
	inprogress   []string
	brpopCalls   int
	acknowledged chan struct{}
	secondPoll   chan struct{}
	ackOnce      sync.Once
	pollOnce     sync.Once
}

func newInprogressRedisBackend(message string) *inprogressRedisBackend {
	return &inprogressRedisBackend{
		queued:       message,
		acknowledged: make(chan struct{}),
		secondPoll:   make(chan struct{}),
	}
}

func (b *inprogressRedisBackend) inprogressCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.inprogress)
}

type inprogressRedisConn struct {
	backend *inprogressRedisBackend
}

func (c *inprogressRedisConn) Close() error { return nil }
func (c *inprogressRedisConn) Err() error   { return nil }
func (c *inprogressRedisConn) Flush() error { return nil }
func (c *inprogressRedisConn) Receive() (any, error) {
	return nil, redis.ErrNil
}
func (c *inprogressRedisConn) ReceiveContext(ctx context.Context) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *inprogressRedisConn) Send(string, ...any) error { return nil }
func (c *inprogressRedisConn) Do(commandName string, args ...any) (any, error) {
	return c.do(context.Background(), commandName, args...)
}
func (c *inprogressRedisConn) DoContext(ctx context.Context, commandName string, args ...any) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return c.do(ctx, commandName, args...)
	}
}

func (c *inprogressRedisConn) do(_ context.Context, commandName string, args ...any) (any, error) {
	b := c.backend
	switch strings.ToLower(commandName) {
	case "lrange":
		b.mu.Lock()
		messages := make([]any, len(b.inprogress))
		for i, message := range b.inprogress {
			messages[i] = message
		}
		b.mu.Unlock()
		return messages, nil
	case "brpoplpush":
		b.mu.Lock()
		b.brpopCalls++
		call := b.brpopCalls
		message := b.queued
		if message != "" {
			b.queued = ""
			b.inprogress = append(b.inprogress, message)
		}
		b.mu.Unlock()
		if call >= 2 {
			b.pollOnce.Do(func() { close(b.secondPoll) })
		}
		if message == "" {
			return nil, redis.ErrNil
		}
		return message, nil
	case "lrem":
		original, _ := args[2].(string)
		b.mu.Lock()
		remaining := b.inprogress[:0]
		for _, message := range b.inprogress {
			if message != original {
				remaining = append(remaining, message)
			}
		}
		b.inprogress = remaining
		b.mu.Unlock()
		b.ackOnce.Do(func() { close(b.acknowledged) })
		return 1, nil
	case "zrangebyscore":
		return []any{}, nil
	default:
		return nil, nil
	}
}

type ackBlackholeBackend struct {
	started        chan struct{}
	allStarted     chan struct{}
	startOnce      sync.Once
	allStartedOnce sync.Once
	expected       int32
	startedCount   atomic.Int32
	active         atomic.Int32
	maxActive      atomic.Int32
	mu             sync.Mutex
	deadlines      []time.Time
}

type ackBlackholeRedisConn struct {
	backend *ackBlackholeBackend
}

func (c *ackBlackholeRedisConn) Close() error { return nil }
func (c *ackBlackholeRedisConn) Err() error   { return nil }
func (c *ackBlackholeRedisConn) Flush() error { return nil }
func (c *ackBlackholeRedisConn) Receive() (any, error) {
	return nil, redis.ErrNil
}
func (c *ackBlackholeRedisConn) ReceiveContext(ctx context.Context) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *ackBlackholeRedisConn) Send(string, ...any) error { return nil }
func (c *ackBlackholeRedisConn) Do(commandName string, _ ...any) (any, error) {
	switch strings.ToLower(commandName) {
	case "lrange", "zrangebyscore":
		return []any{}, nil
	case "brpoplpush":
		return nil, redis.ErrNil
	default:
		return nil, nil
	}
}
func (c *ackBlackholeRedisConn) DoContext(ctx context.Context, commandName string, args ...any) (any, error) {
	if strings.EqualFold(commandName, "lrem") {
		deadline, ok := ctx.Deadline()
		if !ok {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		c.backend.mu.Lock()
		c.backend.deadlines = append(c.backend.deadlines, deadline)
		c.backend.mu.Unlock()
		active := c.backend.active.Add(1)
		defer c.backend.active.Add(-1)
		for {
			maximum := c.backend.maxActive.Load()
			if active <= maximum || c.backend.maxActive.CompareAndSwap(maximum, active) {
				break
			}
		}
		c.backend.startOnce.Do(func() { close(c.backend.started) })
		if c.backend.startedCount.Add(1) == c.backend.expected {
			c.backend.allStartedOnce.Do(func() { close(c.backend.allStarted) })
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return c.Do(commandName, args...)
	}
}
func (c *blockingRedisConn) Send(string, ...any) error { return nil }

func (c *blockingRedisConn) Do(commandName string, _ ...any) (any, error) {
	switch strings.ToLower(commandName) {
	case "sadd":
		c.backend.saddOnce.Do(func() { close(c.backend.saddStarted) })
		<-c.backend.releaseSadd
		return 1, nil
	case "lrange", "zrangebyscore":
		return []any{}, nil
	default:
		return nil, nil
	}
}

func (c *blockingRedisConn) DoContext(ctx context.Context, commandName string, args ...any) (any, error) {
	var started chan struct{}
	var once *sync.Once
	switch strings.ToLower(commandName) {
	case "lrange":
		started, once = c.backend.lrangeStarted, &c.backend.lrangeOnce
	case "zrangebyscore":
		started, once = c.backend.zrangeStarted, &c.backend.zrangeOnce
	default:
		return c.Do(commandName, args...)
	}

	c.backend.activeContextIO.Add(1)
	defer func() {
		c.backend.activeContextIO.Add(-1)
		c.backend.finishedContextIO.Add(1)
	}()
	once.Do(func() { close(started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func configureIdleRuntime(t *testing.T) *idleRedisBackend {
	t.Helper()
	Quit()
	require.NoError(t, ResetManagers())

	Configure(Options{
		Address:      "127.0.0.1:1",
		ProcessID:    "lifecycle-test",
		PoolSize:     4,
		PoolInterval: 15,
	})
	backend := &idleRedisBackend{pollSeen: make(chan struct{})}
	Config.Pool = &redis.Pool{
		MaxIdle: 4,
		Dial: func() (redis.Conn, error) {
			return &idleRedisConn{backend: backend}, nil
		},
	}
	t.Cleanup(func() {
		Quit()
		require.NoError(t, ResetManagers())
	})
	return backend
}

func TestQuitJoinsSchedulerFetcherManagerAndWorkers(t *testing.T) {
	backend := configureIdleRuntime(t)
	Process("rpc", func(*Msg) {}, 2)
	manager := managers["rpc"]
	fetcher, ok := manager.fetch.(*fetch)
	require.True(t, ok)

	Start()
	scheduler := schedule
	select {
	case <-backend.pollSeen:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not start polling")
	}

	done := make(chan struct{})
	go func() {
		Quit()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Quit did not interrupt the scheduler idle wait")
	}

	requireClosed(t, scheduler.done, "scheduler")
	requireClosed(t, manager.done, "manager")
	requireClosed(t, fetcher.done, "fetcher")
	for _, worker := range manager.workers {
		requireClosed(t, worker.done, "worker")
	}
}

func TestQuitWaitsForAcceptedJob(t *testing.T) {
	configureIdleRuntime(t)
	started := make(chan struct{})
	release := make(chan struct{})
	Process("rpc", func(*Msg) {
		close(started)
		<-release
	}, 1)
	manager := managers["rpc"]
	Start()

	message, err := NewMsg(`{"jid":"job-1","args":{}}`)
	require.NoError(t, err)
	go func() { manager.fetch.Messages() <- message }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not accept the job")
	}

	done := make(chan struct{})
	go func() {
		Quit()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Quit returned before the accepted job completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Quit did not return after the accepted job completed")
	}
}

func TestQuitAndResetDoesNotRestartPreviousManagers(t *testing.T) {
	configureIdleRuntime(t)
	Process("old", func(*Msg) {}, 1)
	Start()
	QuitAndReset()
	require.Empty(t, managers)

	backend := &idleRedisBackend{pollSeen: make(chan struct{})}
	Configure(Options{
		Address:      "127.0.0.1:1",
		ProcessID:    "second-runtime",
		PoolSize:     2,
		PoolInterval: 15,
	})
	Config.Pool = &redis.Pool{
		MaxIdle: 2,
		Dial: func() (redis.Conn, error) {
			return &idleRedisConn{backend: backend}, nil
		},
	}
	Process("new", func(*Msg) {}, 1)
	Start()
	QuitAndReset()
	require.Empty(t, managers)
}

func TestAcceptedJobCanEnqueueWhileQuitWaits(t *testing.T) {
	configureIdleRuntime(t)
	startedJob := make(chan struct{})
	allowEnqueue := make(chan struct{})
	enqueueResult := make(chan error, 1)
	Process("rpc", func(*Msg) {
		close(startedJob)
		<-allowEnqueue
		_, err := EnqueueWithOptions("rpc", "pitaya.rpc", map[string]any{}, EnqueueOptions{})
		enqueueResult <- err
	}, 1)
	manager := managers["rpc"]
	Start()

	message, err := NewMsg(`{"jid":"job-2","args":{}}`)
	require.NoError(t, err)
	go func() { manager.fetch.Messages() <- message }()
	select {
	case <-startedJob:
	case <-time.After(time.Second):
		t.Fatal("worker did not accept the job")
	}

	quitDone := make(chan struct{})
	go func() {
		Quit()
		close(quitDone)
	}()
	require.Eventually(t, func() bool {
		access.RLock()
		defer access.RUnlock()
		return stopping
	}, time.Second, time.Millisecond)
	close(allowEnqueue)
	select {
	case err := <-enqueueResult:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("accepted job deadlocked enqueueing during Quit")
	}
	select {
	case <-quitDone:
	case <-time.After(time.Second):
		t.Fatal("Quit did not return after the accepted job completed")
	}
}

func TestEnqueueRedisIODoesNotHoldLifecycleLock(t *testing.T) {
	configureIdleRuntime(t)
	backend := newBlockingRedisBackend()
	Config.Pool = &redis.Pool{
		MaxIdle: 4,
		Dial: func() (redis.Conn, error) {
			return &blockingRedisConn{backend: backend}, nil
		},
	}
	Start()

	enqueueDone := make(chan error, 1)
	go func() {
		_, err := EnqueueWithOptions("rpc", "pitaya.rpc", map[string]any{}, EnqueueOptions{})
		enqueueDone <- err
	}()
	select {
	case <-backend.saddStarted:
	case <-time.After(time.Second):
		t.Fatal("enqueue did not enter Redis I/O")
	}

	quitDone := make(chan struct{})
	go func() {
		Quit()
		close(quitDone)
	}()
	select {
	case <-quitDone:
	case <-time.After(time.Second):
		t.Fatal("Quit was blocked by enqueue Redis I/O holding the lifecycle lock")
	}
	access.RLock()
	require.False(t, started)
	require.False(t, stopping)
	access.RUnlock()

	close(backend.releaseSadd)
	select {
	case err := <-enqueueDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("enqueue did not return after Redis I/O was released")
	}
}

func TestQuitCancelsAndJoinsBlockingRedisIO(t *testing.T) {
	configureIdleRuntime(t)
	backend := newBlockingRedisBackend()
	Config.Pool = &redis.Pool{
		MaxIdle: 4,
		Dial: func() (redis.Conn, error) {
			return &blockingRedisConn{backend: backend}, nil
		},
	}
	Process("rpc", func(*Msg) {}, 1)
	manager := managers["rpc"]
	fetcher := manager.fetch.(*fetch)
	Start()
	scheduler := schedule

	select {
	case <-backend.lrangeStarted:
	case <-time.After(time.Second):
		t.Fatal("fetcher did not enter blocking Redis I/O")
	}
	select {
	case <-backend.zrangeStarted:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not enter blocking Redis I/O")
	}
	require.Equal(t, int32(2), backend.activeContextIO.Load())

	quitDone := make(chan struct{})
	go func() {
		Quit()
		close(quitDone)
	}()
	select {
	case <-quitDone:
	case <-time.After(time.Second):
		t.Fatal("Quit did not cancel and join blocking Redis I/O")
	}

	require.Zero(t, backend.activeContextIO.Load())
	require.Equal(t, int32(2), backend.finishedContextIO.Load())
	requireClosed(t, scheduler.done, "scheduler")
	requireClosed(t, fetcher.done, "fetcher")
	requireClosed(t, manager.done, "manager")
}

func TestShutdownAcknowledgesCompletedInProgressJobBeforeRestart(t *testing.T) {
	configureIdleRuntime(t)
	oldTimeout := workerCleanupTimeout
	workerCleanupTimeout = 10 * time.Millisecond
	t.Cleanup(func() { workerCleanupTimeout = oldTimeout })
	const original = `{"jid":"job-in-progress","queue":"rpc","args":{}}`
	backend := newInprogressRedisBackend(original)
	Config.Pool = &redis.Pool{
		MaxIdle: 4,
		Dial: func() (redis.Conn, error) {
			return &inprogressRedisConn{backend: backend}, nil
		},
	}

	jobStarted := make(chan struct{})
	releaseJob := make(chan struct{})
	replayed := make(chan struct{})
	var startedOnce sync.Once
	var replayOnce sync.Once
	var calls atomic.Int32
	Process("rpc", func(*Msg) {
		call := calls.Add(1)
		startedOnce.Do(func() { close(jobStarted) })
		if call > 1 {
			replayOnce.Do(func() { close(replayed) })
		}
		<-releaseJob
	}, 1)
	Start()

	select {
	case <-jobStarted:
	case <-time.After(time.Second):
		t.Fatal("worker did not accept the Redis in-progress job")
	}
	require.Equal(t, 1, backend.inprogressCount())

	quitDone := make(chan struct{})
	go func() {
		Quit()
		close(quitDone)
	}()
	require.Eventually(t, func() bool {
		access.RLock()
		defer access.RUnlock()
		return stopping
	}, time.Second, time.Millisecond)
	<-time.After(3 * workerCleanupTimeout)
	close(releaseJob)

	select {
	case <-backend.acknowledged:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not acknowledge the completed in-progress job")
	}
	select {
	case <-quitDone:
	case <-time.After(time.Second):
		t.Fatal("Quit did not wait for the shutdown acknowledgement")
	}
	require.Zero(t, backend.inprogressCount())

	Start()
	select {
	case <-backend.secondPoll:
	case <-time.After(time.Second):
		t.Fatal("restarted fetcher did not finish scanning the in-progress list")
	}
	select {
	case <-replayed:
		t.Fatal("acknowledged job was replayed after restart")
	default:
	}
	require.Equal(t, int32(1), calls.Load())
	Quit()
}

func TestQuitRunsBoundedAcknowledgementsConcurrently(t *testing.T) {
	configureIdleRuntime(t)
	oldTimeout := workerCleanupTimeout
	workerCleanupTimeout = 40 * time.Millisecond
	t.Cleanup(func() { workerCleanupTimeout = oldTimeout })

	const workerCount = 4
	backend := &ackBlackholeBackend{
		started:    make(chan struct{}),
		allStarted: make(chan struct{}),
		expected:   workerCount,
	}
	Config.Pool = &redis.Pool{
		MaxIdle: 8,
		Dial: func() (redis.Conn, error) {
			return &ackBlackholeRedisConn{backend: backend}, nil
		},
	}

	jobsStarted := make(chan struct{})
	releaseJobs := make(chan struct{})
	var startOnce sync.Once
	var startedJobs atomic.Int32
	Process("rpc", func(*Msg) {
		if startedJobs.Add(1) == workerCount {
			startOnce.Do(func() { close(jobsStarted) })
		}
		<-releaseJobs
	}, workerCount)
	manager := managers["rpc"]
	Start()

	for i := 0; i < workerCount; i++ {
		message, err := NewMsg(`{"jid":"blackhole","args":{}}`)
		require.NoError(t, err)
		go func() { manager.fetch.Messages() <- message }()
	}
	select {
	case <-jobsStarted:
	case <-time.After(time.Second):
		t.Fatal("workers did not accept all jobs")
	}

	quitStarted := time.Now()
	quitDone := make(chan struct{})
	go func() {
		Quit()
		close(quitDone)
	}()
	require.Eventually(t, func() bool {
		access.RLock()
		defer access.RUnlock()
		return stopping
	}, time.Second, time.Millisecond)
	close(releaseJobs)
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("shutdown acknowledgement did not enter Redis I/O")
	}
	select {
	case <-backend.allStarted:
	case <-time.After(3 * workerCleanupTimeout):
		t.Fatal("shutdown acknowledgements were not started concurrently")
	}
	select {
	case <-quitDone:
	case <-time.After(3 * workerCleanupTimeout):
		t.Fatal("Quit serialized per-job acknowledgement cleanup budgets")
	}
	require.Less(t, time.Since(quitStarted), 3*workerCleanupTimeout)
	require.Zero(t, backend.active.Load())
	require.Equal(t, int32(workerCount), backend.maxActive.Load())

	backend.mu.Lock()
	deadlines := append([]time.Time(nil), backend.deadlines...)
	backend.mu.Unlock()
	require.Len(t, deadlines, workerCount)
}

func requireClosed(t *testing.T, ch <-chan struct{}, owner string) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatalf("%s goroutine was not joined", owner)
	}
}
