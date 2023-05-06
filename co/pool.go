package co

import (
	"context"
	"time"

	"github.com/alkaid/goerrors/errors"
	"github.com/panjf2000/ants/v2"
	"github.com/topfreegames/pitaya/v2/config"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/metrics"
	"github.com/topfreegames/pitaya/v2/util"
	"go.uber.org/zap"
)

const (
	DefaultStatefulPoolExpire     = time.Minute * 10
	DefaultStatefulPoolTaskBuffer = 100
)

var DefaultStatefulPoolTimeoutBuckets = []time.Duration{5 * time.Second, 10 * time.Second, 1 * time.Minute, 10 * time.Minute}

// StatefulPool 带状态的线程池
type StatefulPool struct {
	pool      *ants.Pool
	config    config.GoPool
	reporters []metrics.Reporter
}

func NewStatefulPool(config config.GoPool, reporters []metrics.Reporter) (*StatefulPool, error) {
	if config.Name == "" {
		return nil, errors.New("stateful pool name cannot be nil")
	}
	if config.Expire == 0 {
		config.Expire = DefaultStatefulPoolExpire
	}
	if config.TaskBuffer == 0 {
		config.TaskBuffer = DefaultStatefulPoolTaskBuffer
	}
	if len(config.TimeoutBuckets) == 0 {
		config.TimeoutBuckets = DefaultStatefulPoolTimeoutBuckets
	}
	p, err := ants.NewPool(ants.DefaultAntsPoolSize, ants.WithTaskBuffer(config.TaskBuffer), ants.WithExpiryDuration(config.Expire))
	if err != nil {
		return nil, err
	}
	return &StatefulPool{
		config:    config,
		reporters: reporters,
		pool:      p,
	}, nil
}

func (s *StatefulPool) Name() string {
	return s.config.Name
}

// Go 根据指定的goroutineID派发线程
//
//	@receiver h
//	@param goID 若>0,派发到指定线程,否则随机派发
//	@param task
func (s *StatefulPool) Go(ctx context.Context, goID int, task func(ctx context.Context)) (done chan struct{}) {
	done = make(chan struct{})
	logg := util.GetLoggerFromCtx(ctx).With(zap.String("pool", s.Name())).With(zap.Int("goID", goID))
	ctx = context.WithValue(ctx, constants.LoggerCtxKey, logg)
	taskWithDone := func() {
		task(ctx)
		close(done)
	}
	if !s.config.DisableTimeoutWatch {
		timeoutErr := errors.NewWithStack("goroutine timeout")
		Go(func() {
			for _, timeout := range s.config.TimeoutBuckets {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-time.After(timeout):
					logg.Error("", zap.Duration("timeout", timeout), zap.Error(timeoutErr))
					metrics.ReportPoolGoDeadlines(ctx, s.Name(), int(timeout/time.Second), s.reporters)
				}
			}
		})
	}
	logg.Debug("submit")
	err := s.pool.SubmitWithID(goID, taskWithDone)
	if err != nil {
		logg.Error("submit task with id error", zap.Error(err))
		return
	}
	return done
}

func (s *StatefulPool) Wait(ctx context.Context, goID int, task func(ctx context.Context)) {
	done := s.Go(ctx, goID, task)
	select {
	case <-done:
	case <-ctx.Done():
	}
}
