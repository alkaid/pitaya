package session

import (
	"context"
	"time"

	"github.com/go-redis/redis/v8"
)

// CacheInterface
//
//	@Description:
type CacheInterface interface {
	Set(key string, value string) error
	Get(key string) (string, error)
	Expire(key string) error
	CacheTTL() time.Duration
}
type RedisCache struct {
	client redis.Cmdable
	ttl    time.Duration
}

func NewRedisCache(client redis.Cmdable, defaultTTL time.Duration) *RedisCache {
	return &RedisCache{
		client: client,
		ttl:    defaultTTL,
	}
}

func (r RedisCache) Set(key string, value string) error {
	return r.client.Set(context.Background(), key, value, r.ttl).Err()
}
func (r RedisCache) Get(key string) (string, error) {
	return r.client.Get(context.Background(), key).Result()
}
func (r RedisCache) Expire(key string) error {
	return r.client.Expire(context.Background(), key, r.ttl).Err()
}
func (r RedisCache) CacheTTL() time.Duration {
	return r.ttl
}
