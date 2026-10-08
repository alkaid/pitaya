//go:build integration

package session

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRedisHashRoundTrip(t *testing.T) {
	addr := os.Getenv("PITAYA_TEST_REDIS_ADDR")
	require.NotEmpty(t, addr, "integration test requires a dedicated Redis address")
	conn := redis.NewClient(&redis.Options{Addr: addr})
	defer conn.Close()
	ctx := context.Background()
	key := "pitaya-test:" + uuid.NewString()
	defer conn.Del(ctx, key)
	cache := NewRedisCache(conn, time.Minute)
	fields := map[string]string{"uid": "123", "frontend": "frontend-1"}
	require.NoError(t, cache.HmsetCtx(ctx, key, fields))
	got, err := cache.HgetallCtx(ctx, key)
	require.NoError(t, err)
	require.Equal(t, fields, got)
	ttl, err := conn.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, time.Duration(-1), ttl, "HMSet must not add expiration")
	require.NoError(t, cache.Expire(key))
	require.NoError(t, cache.Hmset(key, map[string]string{"frontend": "frontend-2"}))
	got, err = cache.Hgetall(key)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"uid": "123", "frontend": "frontend-2"}, got)
	ttl, err = conn.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, time.Minute)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, cache.HmsetCtx(canceled, key, fields), context.Canceled)
}
