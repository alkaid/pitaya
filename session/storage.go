package session

import (
	"context"
	"github.com/go-redis/redis/v8"
	"time"
)

//StorageInterface
//  @Description:
type StorageInterface interface {
	Set(key string, value string) error
	Get(key string) (string, error)
	Expire(key string) error
}
type RedisStorage struct {
	client redis.Cmdable
	ttl    time.Duration
}

func NewRedisStorage(client redis.Cmdable, defaultTTL time.Duration) *RedisStorage {
	return &RedisStorage{
		client: client,
		ttl:    defaultTTL,
	}
}

func (r RedisStorage) Set(key string, value string) error {
	return r.client.Set(context.Background(), key, value, r.ttl).Err()
}
func (r RedisStorage) Get(key string) (string, error) {
	return r.client.Get(context.Background(), key).Result()
}
func (r RedisStorage) Expire(key string) error {
	return r.client.Expire(context.Background(), key, r.ttl).Err()
}
