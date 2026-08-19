package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type evalCmdRecorder struct {
	redis.Cmdable

	ctx    context.Context
	script string
	keys   []string
	args   []interface{}
	result interface{}
	err    error
}

func (r *evalCmdRecorder) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	r.ctx = ctx
	r.script = script
	r.keys = append([]string(nil), keys...)
	r.args = append([]interface{}(nil), args...)
	if r.err != nil {
		return redis.NewCmdResult(nil, r.err)
	}
	if err := ctx.Err(); err != nil {
		return redis.NewCmdResult(nil, err)
	}
	result := r.result
	if result == nil {
		result = int64(1)
	}
	return redis.NewCmdResult(result, nil)
}

func TestRedisCacheHmsetCtxForwardsArguments(t *testing.T) {
	t.Parallel()

	type contextKey string
	ctx := context.WithValue(context.Background(), contextKey("request"), "request-id")
	fields := map[string]string{
		"online":     "1",
		"frontendID": "frontend-1",
	}
	recorder := &evalCmdRecorder{}
	cache := NewRedisCache(recorder, time.Minute)

	err := cache.HmsetCtx(ctx, "session-key", fields)

	require.NoError(t, err)
	assert.Equal(t, ctx, recorder.ctx)
	assert.Equal(t, writeHashWithTTLScript, recorder.script)
	assert.Equal(t, []string{"session-key"}, recorder.keys)
	assert.Equal(t, []interface{}{int64(60_000), "frontendID", "frontend-1", "online", "1"}, recorder.args)
}

func TestRedisCacheHsetCtxWritesValueAndTTLInOneEval(t *testing.T) {
	t.Parallel()

	recorder := &evalCmdRecorder{}
	cache := NewRedisCache(recorder, 1500*time.Millisecond)

	err := cache.HsetCtx(context.Background(), "session-key", "online", "1")

	require.NoError(t, err)
	assert.Equal(t, writeHashWithTTLScript, recorder.script)
	assert.Equal(t, []string{"session-key"}, recorder.keys)
	assert.Equal(t, []interface{}{int64(1500), "online", "1"}, recorder.args)
}

func TestRedisCacheHsetnxCtxReturnsResultAndRefreshesTTLInOneEval(t *testing.T) {
	t.Parallel()

	recorder := &evalCmdRecorder{result: int64(0)}
	cache := NewRedisCache(recorder, 1500*time.Millisecond)

	inserted, err := cache.HsetnxCtx(context.Background(), "session-key", "online", "1")

	require.NoError(t, err)
	assert.False(t, inserted)
	assert.Equal(t, writeHashNXWithTTLScript, recorder.script)
	assert.Equal(t, []string{"session-key"}, recorder.keys)
	assert.Equal(t, []interface{}{int64(1500), "online", "1"}, recorder.args)
}

func TestRedisCacheHmsetCtxReturnsRedisError(t *testing.T) {
	t.Parallel()

	redisErr := errors.New("redis unavailable")
	recorder := &evalCmdRecorder{err: redisErr}
	cache := NewRedisCache(recorder, time.Minute)

	err := cache.HmsetCtx(context.Background(), "session-key", map[string]string{"online": "1"})

	assert.ErrorIs(t, err, redisErr)
}

func TestRedisCacheHmsetCtxReturnsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := &evalCmdRecorder{}
	cache := NewRedisCache(recorder, time.Minute)

	err := cache.HmsetCtx(ctx, "session-key", map[string]string{"online": "1"})

	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, ctx, recorder.ctx)
}

func TestNewRedisCacheRejectsNonPositiveTTL(t *testing.T) {
	t.Parallel()

	for _, ttl := range []time.Duration{0, -time.Second} {
		assert.PanicsWithValue(t, "pitaya/session: cache TTL must be greater than zero", func() {
			NewRedisCache(nil, ttl)
		})
	}
}

func TestNewRedisCacheRoundsPositiveTTLToRedisPrecision(t *testing.T) {
	t.Parallel()

	cache := NewRedisCache(nil, time.Nanosecond)
	assert.Equal(t, time.Millisecond, cache.CacheTTL())
}

type cacheTTLStub struct {
	CacheInterface
	ttl time.Duration
}

func (c cacheTTLStub) CacheTTL() time.Duration {
	return c.ttl
}

func TestSessionPoolRejectsCacheWithoutPositiveTTL(t *testing.T) {
	t.Parallel()

	pool := NewSessionPool()
	for _, ttl := range []time.Duration{0, -time.Second} {
		assert.PanicsWithValue(t, "pitaya/session: cache TTL must be greater than zero", func() {
			pool.SetClusterCache(cacheTTLStub{ttl: ttl})
		})
	}
}

type hmsetCacheRecorder struct {
	CacheInterface

	key    string
	fields map[string]string
	err    error
}

type hsetCall struct {
	key   string
	field string
	value string
}

type hsetCacheRecorder struct {
	CacheInterface
	calls []hsetCall
}

func (r *hsetCacheRecorder) Hset(key, field, value string) error {
	r.calls = append(r.calls, hsetCall{key: key, field: field, value: value})
	return nil
}

func (r *hmsetCacheRecorder) Hmset(key string, fields map[string]string) error {
	r.key = key
	r.fields = fields
	return r.err
}

func TestFlushFrontendDataWritesCompleteSessionHash(t *testing.T) {
	t.Parallel()

	recorder := &hmsetCacheRecorder{}
	pool := NewSessionPool().(*sessionPoolImpl)
	pool.storage = recorder
	session := &sessionImpl{
		pool: pool,
		sessionDataOwner: sessionDataOwner{
			frontendID:        "frontend-1",
			frontendSessionID: 42,
			ip:                "192.0.2.1",
			encodedData:       []byte(`{"coins":7}`),
		},
	}
	session.identity.Store(&sessionIdentity{uid: "user-42", uidInt: 42, online: true})

	err := session.FlushFrontendData()

	require.NoError(t, err)
	assert.Equal(t, "pit:u:user-42:", recorder.key)
	assert.Equal(t, map[string]string{
		fieldKeyOnline:         "1",
		fieldKeyFrontendID:     "frontend-1",
		fieldKeyFrontendSessID: "42",
		fieldKeyIP:             "192.0.2.1",
		fieldKeyData:           `{"coins":7}`,
	}, recorder.fields)
}

func TestSessionSingleFieldFlushesUseOwnedSnapshots(t *testing.T) {
	t.Parallel()

	recorder := &hsetCacheRecorder{}
	pool := NewSessionPool().(*sessionPoolImpl)
	pool.storage = recorder
	sess := &sessionImpl{
		pool: pool,
		sessionDataOwner: sessionDataOwner{
			encodedData: []byte(`{"coins":7}`),
		},
	}
	sess.identity.Store(&sessionIdentity{uid: "user-42", uidInt: 42, online: true})
	sess.SetBackendID("chat", "backend-1")

	require.NoError(t, sess.FlushOnline())
	require.NoError(t, sess.FlushBackendData())
	require.NoError(t, sess.FlushUserData())
	require.Equal(t, []hsetCall{
		{key: "pit:u:user-42:", field: fieldKeyOnline, value: "1"},
		{key: "pit:u:user-42:", field: fieldKeyBackends, value: `{"chat":"backend-1"}`},
		{key: "pit:u:user-42:", field: fieldKeyData, value: `{"coins":7}`},
	}, recorder.calls)
}
