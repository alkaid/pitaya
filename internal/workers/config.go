package workers

import (
	"context"
	"fmt"
	"time"

	"github.com/gomodule/redigo/redis"
)

const (
	redisConnectTimeout = 5 * time.Second
	redisReadTimeout    = 5 * time.Second
	redisWriteTimeout   = 5 * time.Second
)

type Options struct {
	Address      string
	Username     string
	Password     string
	Database     string
	ProcessID    string
	Namespace    string
	PoolSize     int
	PoolInterval int
	DialOptions  []redis.DialOption
}

type WorkerConfig struct {
	processId    string
	Namespace    string
	PoolInterval int
	Pool         *redis.Pool
	Fetch        func(queue string) Fetcher
}

var Config *WorkerConfig

func Configure(options Options) {
	lockAccess()
	defer access.Unlock()
	if started {
		panic("Cannot configure workers while workers are running")
	}

	var namespace string

	if options.Address == "" {
		panic("Configure requires a 'Address' option, which identifies a Redis instance")
	}
	if options.ProcessID == "" {
		panic("Configure requires a 'ProcessID' option, which uniquely identifies this instance")
	}
	if options.PoolSize <= 0 {
		options.PoolSize = 1
	}
	if options.Namespace != "" {
		namespace = options.Namespace + ":"
	}
	if options.PoolInterval <= 0 {
		options.PoolInterval = 15
	}

	Config = &WorkerConfig{
		options.ProcessID,
		namespace,
		options.PoolInterval,
		GetConnectionPool(options),
		func(queue string) Fetcher {
			return NewFetch(queue, make(chan *Msg), make(chan bool))
		},
	}
}

func GetConnectionPool(options Options) *redis.Pool {
	dialOptions := append([]redis.DialOption(nil), options.DialOptions...)
	if options.Username != "" {
		dialOptions = append(dialOptions, redis.DialUsername(options.Username))
	}
	if options.Password != "" {
		dialOptions = append(dialOptions, redis.DialPassword(options.Password))
	}
	dialOptions = append(dialOptions,
		redis.DialConnectTimeout(redisConnectTimeout),
		redis.DialReadTimeout(redisReadTimeout),
		redis.DialWriteTimeout(redisWriteTimeout),
	)
	dial := func(ctx context.Context) (redis.Conn, error) {
		c, err := redis.DialContext(ctx, "tcp", options.Address, dialOptions...)
		if err != nil {
			return nil, err
		}
		if options.Database != "" {
			if _, err := redis.DoContext(c, ctx, "SELECT", options.Database); err != nil {
				if errClose := c.Close(); errClose != nil {
					return nil, fmt.Errorf("%w. failed to close connection: %s", err, errClose.Error())
				}
				return nil, err
			}
		}
		return c, nil
	}
	return &redis.Pool{
		MaxIdle:     options.PoolSize,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			return dial(context.Background())
		},
		DialContext: dial,
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			return err
		},
		TestOnBorrowContext: func(ctx context.Context, c redis.Conn, _ time.Time) error {
			_, err := redis.DoContext(c, ctx, "PING")
			return err
		},
	}
}
