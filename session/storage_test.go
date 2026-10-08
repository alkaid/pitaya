package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

type hashWriter struct {
	redis.Cmdable
	ctx    context.Context
	key    string
	values []interface{}
	err    error
}

func (w *hashWriter) HMSet(ctx context.Context, key string, values ...interface{}) *redis.BoolCmd {
	w.ctx, w.key, w.values = ctx, key, values
	cmd := redis.NewBoolCmd(ctx)
	cmd.SetErr(w.err)
	return cmd
}
func TestRedisCacheHmsetForwardsFieldsAndContext(t *testing.T) {
	writer := &hashWriter{}
	cache := NewRedisCache(writer, time.Minute)
	fields := map[string]string{"uid": "123", "frontend": "frontend-1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, cache.HmsetCtx(ctx, "session:123", fields))
	require.Same(t, ctx, writer.ctx)
	require.Equal(t, "session:123", writer.key)
	require.Len(t, writer.values, 1)
	require.EqualValues(t, fields, writer.values[0])
	require.Equal(t, time.Minute, cache.CacheTTL())
	writer.err = errors.New("redis failure")
	require.ErrorIs(t, cache.Hmset("session:123", fields), writer.err)
}
