package session

import (
	"context"
	"errors"
	"testing"

	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type hmsetCmdRecorder struct {
	redis.Cmdable

	ctx    context.Context
	key    string
	values []interface{}
	err    error
}

func (r *hmsetCmdRecorder) HMSet(ctx context.Context, key string, values ...interface{}) *redis.BoolCmd {
	r.ctx = ctx
	r.key = key
	r.values = values
	if r.err != nil {
		return redis.NewBoolResult(false, r.err)
	}
	if err := ctx.Err(); err != nil {
		return redis.NewBoolResult(false, err)
	}
	return redis.NewBoolResult(true, nil)
}

func TestRedisCacheHmsetCtxForwardsArguments(t *testing.T) {
	t.Parallel()

	type contextKey string
	ctx := context.WithValue(context.Background(), contextKey("request"), "request-id")
	fields := map[string]string{
		"online":     "1",
		"frontendID": "frontend-1",
	}
	recorder := &hmsetCmdRecorder{}
	cache := NewRedisCache(recorder, 0)

	err := cache.HmsetCtx(ctx, "session-key", fields)

	require.NoError(t, err)
	assert.Equal(t, ctx, recorder.ctx)
	assert.Equal(t, "session-key", recorder.key)
	require.Len(t, recorder.values, 1)
	assert.Equal(t, fields, recorder.values[0])
}

func TestRedisCacheHmsetCtxReturnsRedisError(t *testing.T) {
	t.Parallel()

	redisErr := errors.New("redis unavailable")
	recorder := &hmsetCmdRecorder{err: redisErr}
	cache := NewRedisCache(recorder, 0)

	err := cache.HmsetCtx(context.Background(), "session-key", map[string]string{"online": "1"})

	assert.ErrorIs(t, err, redisErr)
}

func TestRedisCacheHmsetCtxReturnsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := &hmsetCmdRecorder{}
	cache := NewRedisCache(recorder, 0)

	err := cache.HmsetCtx(ctx, "session-key", map[string]string{"online": "1"})

	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, ctx, recorder.ctx)
}

type hmsetCacheRecorder struct {
	CacheInterface

	key    string
	fields map[string]string
	err    error
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
		uid:               "user-42",
		pool:              pool,
		online:            true,
		frontendID:        "frontend-1",
		frontendSessionID: 42,
		ip:                "192.0.2.1",
		encodedData:       []byte(`{"coins":7}`),
	}

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
