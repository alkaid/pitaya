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
	"golang.org/x/exp/slices"
)

const (
	DefaultStatefulPoolExpire     = time.Minute * 10
	DefaultStatefulPoolTaskBuffer = 100
)

var DefaultStatefulPoolTimeoutBuckets = []time.Duration{5 * time.Second, 10 * time.Second, 1 * time.Minute, 10 * time.Minute}

// StatefulPool 带状态的线程池
type StatefulPool struct {
	pool      *ants.PoolWithID
	config    config.GoPool
	reporters []metrics.Reporter
}

func NewStatefulPool(config config.GoPool, reporters []metrics.Reporter, options ...ants.Option) (*StatefulPool, error) {
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
	options = append(options,
		ants.WithTaskBuffer(config.TaskBuffer),
		ants.WithExpiryDuration(config.Expire),
		ants.WithDisablePurgeRunning(config.DisablePurgeRunning),
		ants.WithDisablePurge(config.DisablePurge))
	p, err := ants.NewPoolWithID(ants.DefaultAntsPoolSize, options...)
	if err != nil {
		return nil, err
	}
	slices.Sort(config.TimeoutBuckets)
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
//	@param goID
//	@param task
func (s *StatefulPool) Go(ctx context.Context, goID int, task func(ctx context.Context), disableTimeoutWatch bool) (done chan struct{}) {
	done = make(chan struct{})
	fields := util.LogFieldsFromCtx(ctx)
	fields = append(fields, zap.String("pool", s.Name()), zap.Int("goID", goID))
	logg := util.GetLoggerFromCtx(ctx).With(fields...)
	ctx = context.WithValue(ctx, constants.LoggerCtxKey, logg)
	taskWithDone := func() {
		task(ctx)
		close(done)
	}
	submitted := false
	if !s.config.DisableTimeoutWatch && !disableTimeoutWatch {
		timeoutErr := errors.NewWithStack("goroutine timeout")
		Go(func() {
			for _, timeout := range s.config.TimeoutBuckets {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-time.After(timeout):
					logg.Error("", zap.Duration("timeout", timeout), zap.Bool("submitted", submitted), zap.Error(timeoutErr))
					metrics.ReportPoolGoDeadlines(ctx, s.Name(), int(timeout/time.Second), s.reporters)
				}
			}
		})
	}
	logg.Debug("submit")
	err := s.pool.Submit(goID, taskWithDone)
	if err != nil {
		logg.Error("submit task with id error", zap.Error(err))
		return
	}
	submitted = true
	return done
}

func (s *StatefulPool) Wait(ctx context.Context, goID int, task func(ctx context.Context), disableTimeoutWatch bool) {
	done := s.Go(ctx, goID, task, disableTimeoutWatch)
	select {
	case <-done:
	case <-ctx.Done():
	}
}
