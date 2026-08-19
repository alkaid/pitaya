package session

import (
	"context"
	"sort"
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
	Hget(key, field string) (string, error)
	HgetCtx(ctx context.Context, key, field string) (val string, err error)
	Hgetall(key string) (map[string]string, error)
	HgetallCtx(ctx context.Context, key string) (val map[string]string, err error)
	Hset(key, field, value string) error
	HsetCtx(ctx context.Context, key, field, value string) error
	Hsetnx(key, field, value string) (bool, error)
	HsetnxCtx(ctx context.Context, key, field, value string) (val bool, err error)
	Hmset(key string, fieldsAndValues map[string]string) error
	HmsetCtx(ctx context.Context, key string, fieldsAndValues map[string]string) error
}
type RedisCache struct {
	conn redis.Cmdable
	ttl  time.Duration
}

const writeHashWithTTLScript = `
local ttl = tonumber(ARGV[1])
if not ttl or ttl <= 0 then
	return redis.error_reply("ERR session cache TTL must be positive")
end
redis.call("HSET", KEYS[1], unpack(ARGV, 2))
redis.call("PEXPIRE", KEYS[1], ttl)
return 1
`

const writeHashNXWithTTLScript = `
local ttl = tonumber(ARGV[1])
if not ttl or ttl <= 0 then
	return redis.error_reply("ERR session cache TTL must be positive")
end
local inserted = redis.call("HSETNX", KEYS[1], ARGV[2], ARGV[3])
redis.call("PEXPIRE", KEYS[1], ttl)
return inserted
`

func NewRedisCache(client redis.Cmdable, defaultTTL time.Duration) *RedisCache {
	if defaultTTL <= 0 {
		panic("pitaya/session: cache TTL must be greater than zero")
	}
	// Redis expiry commands use millisecond precision. Preserve the positive-TTL
	// contract for sub-millisecond callers instead of rounding the key to expiry 0.
	if defaultTTL < time.Millisecond {
		defaultTTL = time.Millisecond
	}
	return &RedisCache{
		conn: client,
		ttl:  defaultTTL,
	}
}

func (r RedisCache) Set(key string, value string) error {
	return r.conn.Set(context.Background(), key, value, r.ttl).Err()
}
func (r RedisCache) Get(key string) (string, error) {
	return r.conn.Get(context.Background(), key).Result()
}
func (r RedisCache) Expire(key string) error {
	return r.conn.Expire(context.Background(), key, r.ttl).Err()
}
func (r RedisCache) CacheTTL() time.Duration {
	return r.ttl
}

// Hget is the implementation of redis hget command.
func (r RedisCache) Hget(key, field string) (string, error) {
	return r.HgetCtx(context.Background(), key, field)
}

// HgetCtx is the implementation of redis hget command.
func (r RedisCache) HgetCtx(ctx context.Context, key, field string) (val string, err error) {
	val, err = r.conn.HGet(ctx, key, field).Result()
	return
}

// Hgetall is the implementation of redis hgetall command.
func (r RedisCache) Hgetall(key string) (map[string]string, error) {
	return r.HgetallCtx(context.Background(), key)
}

// HgetallCtx is the implementation of redis hgetall command.
func (r RedisCache) HgetallCtx(ctx context.Context, key string) (val map[string]string, err error) {
	val, err = r.conn.HGetAll(ctx, key).Result()
	return
}

// Hset is the implementation of redis hset command.
func (r RedisCache) Hset(key, field, value string) error {
	return r.HsetCtx(context.Background(), key, field, value)
}

// HsetCtx is the implementation of redis hset command.
func (r RedisCache) HsetCtx(ctx context.Context, key, field, value string) error {
	return r.writeHashWithTTL(ctx, key, field, value)
}

// Hsetnx is the implementation of redis hsetnx command.
func (r RedisCache) Hsetnx(key, field, value string) (bool, error) {
	return r.HsetnxCtx(context.Background(), key, field, value)
}

// HsetnxCtx is the implementation of redis hsetnx command.
func (r RedisCache) HsetnxCtx(ctx context.Context, key, field, value string) (val bool, err error) {
	result := r.conn.Eval(ctx, writeHashNXWithTTLScript, []string{key}, r.ttl.Milliseconds(), field, value)
	inserted, err := result.Int64()
	if err != nil {
		return false, err
	}
	return inserted == 1, nil
}

// Hmset is the implementation of redis hmset command.
func (r RedisCache) Hmset(key string, fieldsAndValues map[string]string) error {
	return r.HmsetCtx(context.Background(), key, fieldsAndValues)
}

// HmsetCtx is the implementation of redis hmset command.
func (r RedisCache) HmsetCtx(ctx context.Context, key string, fieldsAndValues map[string]string) error {
	fields := make([]string, 0, len(fieldsAndValues))
	for field := range fieldsAndValues {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	values := make([]interface{}, 0, len(fields)*2)
	for _, field := range fields {
		values = append(values, field, fieldsAndValues[field])
	}
	return r.writeHashWithTTL(ctx, key, values...)
}

func (r RedisCache) writeHashWithTTL(ctx context.Context, key string, fieldsAndValues ...interface{}) error {
	ttlMillis := r.ttl.Milliseconds()
	args := make([]interface{}, 1, len(fieldsAndValues)+1)
	args[0] = ttlMillis
	args = append(args, fieldsAndValues...)
	return r.conn.Eval(ctx, writeHashWithTTLScript, []string{key}, args...).Err()
}
