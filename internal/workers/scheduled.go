package workers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"
)

const moveScheduledJobLua = `
if not redis.call("ZSCORE", KEYS[1], ARGV[1]) then
  return 0
end
redis.call("LPUSH", KEYS[2], ARGV[2])
redis.call("ZREM", KEYS[1], ARGV[1])
return 1
`

type scheduled struct {
	keys     []string
	stopCh   chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func (s *scheduled) start(ctx context.Context, pool *redis.Pool, namespace string, poolInterval int) {
	go func() {
		defer close(s.done)
		for {
			select {
			case <-s.stopCh:
				return
			default:
			}

			if !s.poll(ctx, pool, namespace) {
				return
			}

			timer := time.NewTimer(time.Duration(poolInterval) * time.Second)
			select {
			case <-s.stopCh:
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
		}
	}()
}

func (s *scheduled) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

func (s *scheduled) wait() {
	<-s.done
}

func (s *scheduled) poll(ctx context.Context, pool *redis.Pool, namespace string) bool {
	conn, err := pool.GetContext(ctx)
	if err != nil {
		return !errors.Is(err, context.Canceled)
	}
	defer conn.Close()

	now := nowToSecondsWithNanoPrecision()

	for _, key := range s.keys {
		key = namespace + key
		for {
			select {
			case <-s.stopCh:
				return false
			default:
			}
			messages, err := redis.Strings(redis.DoContext(conn, ctx, "zrangebyscore", key, "-inf", now, "limit", 0, 1))
			if errors.Is(err, context.Canceled) {
				return false
			}

			if len(messages) == 0 {
				break
			}

			message, err := NewMsg(messages[0])
			if err != nil {
				Logger.Errorln("failed to create scheduled message", err)
				break
			}
			queue, err := message.Get("queue").String()
			if err != nil {
				Logger.Errorln("failed to read scheduled message queue", err)
				break
			}
			queue = strings.TrimPrefix(queue, namespace)
			message.Set("enqueued_at", nowToSecondsWithNanoPrecision())
			_, err = redis.DoContext(
				conn,
				ctx,
				"eval",
				moveScheduledJobLua,
				2,
				key,
				namespace+"queue:"+queue,
				messages[0],
				message.ToJson(),
			)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return false
				}
				break
			}
		}
	}

	return true
}

func newScheduled(keys ...string) *scheduled {
	return &scheduled{
		keys:   keys,
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
}
