package co

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/interfaces"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"go.uber.org/zap"
)

var _ interfaces.Module = (*StatefulPoolsModule)(nil)

const (
	SessionGoPoolName = "session"   // session线程池,仅框架内部使用
	UserGoPoolName    = "user"      // user线程池,仅框架内部使用
	DefaultGoPoolName = "default"   // 默认线程池,仅框架内部使用
	PersistGoPoolName = "persist"   // 常驻线程池,池子里的线程(运行时)常驻不销毁
	MainThreadID      = math.MaxInt // 主线程ID
)

var builtinPool = []string{
	SessionGoPoolName, UserGoPoolName, DefaultGoPoolName, PersistGoPoolName,
}

type StatefulPoolsModule struct {
	reporters     []metrics.Reporter
	pools         map[string]*StatefulPool
	cfgMap        map[string]config.GoPool
	reportPeriod  time.Duration
	monitorCancel context.CancelFunc
	monitorWG     sync.WaitGroup
}

var instance *StatefulPoolsModule

func NewStatefulPoolsModule(poolsCfg map[string]config.GoPool, reporters []metrics.Reporter, reportPeriod ...time.Duration) *StatefulPoolsModule {
	module := StatefulPoolsModule{
		reporters:    reporters,
		pools:        map[string]*StatefulPool{},
		cfgMap:       make(map[string]config.GoPool, len(poolsCfg)+len(builtinPool)),
		reportPeriod: resolveStatefulPoolMetricsPeriod(reportPeriod),
	}
	for name, pool := range poolsCfg {
		module.cfgMap[name] = resolvePoolConfig(name, pool)
	}
	for _, name := range builtinPool {
		if _, ok := module.cfgMap[name]; !ok {
			module.cfgMap[name] = resolvePoolConfig(name, config.GoPool{})
		}
	}
	instance = &module
	return &module
}

func resolvePoolConfig(name string, cfg config.GoPool) config.GoPool {
	cfg.Name = name
	if cfg.EnableRunningTaskEscape {
		cfg.DisablePurgeRunning = false
	} else if name != DefaultGoPoolName {
		cfg.DisablePurgeRunning = true
	}
	if name == PersistGoPoolName && cfg.Expire == 0 {
		cfg.Expire = time.Hour
	}
	return cfg
}

func (p *StatefulPoolsModule) Init() error {
	for _, cfg := range p.cfgMap {
		pool, err := NewStatefulPool(cfg, p.reporters)
		if err != nil {
			return err
		}
		p.pools[cfg.Name] = pool
		logger.Zap.Info("stateful pool init success", zap.String("name", cfg.Name))
	}
	return nil
}

func (p *StatefulPoolsModule) AfterInit() {
	if len(p.reporters) == 0 || p.monitorCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.monitorCancel = cancel
	for _, pool := range p.pools {
		p.monitorWG.Go(func() {
			pool.monitorMetrics(ctx, p.reportPeriod)
		})
	}
}

func (p *StatefulPoolsModule) BeforeShutdown() {
	if p.monitorCancel != nil {
		p.monitorCancel()
	}
}

func (p *StatefulPoolsModule) Shutdown() error {
	if p.monitorCancel != nil {
		p.monitorCancel()
		p.monitorWG.Wait()
	}
	return nil
}
