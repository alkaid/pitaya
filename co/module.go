package co

import (
	"math"
	"time"

	"github.com/topfreegames/pitaya/v2/config"
	"github.com/topfreegames/pitaya/v2/interfaces"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/metrics"
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
	reporters []metrics.Reporter
	pools     map[string]*StatefulPool
	cfgMap    map[string]config.GoPool
}

var instance *StatefulPoolsModule

func NewStatefulPoolsModule(poolsCfg map[string]config.GoPool, reporters []metrics.Reporter) *StatefulPoolsModule {
	module := StatefulPoolsModule{
		reporters: reporters,
		pools:     map[string]*StatefulPool{},
		cfgMap:    poolsCfg,
	}
	for name, pool := range module.cfgMap {
		pool.Name = name
		module.cfgMap[name] = pool
	}
	if module.cfgMap == nil {
		module.cfgMap = map[string]config.GoPool{}
	}
	// 若没有配置内建线程池,要加上
	for _, name := range builtinPool {
		_, ok := module.cfgMap[name]
		if !ok {
			module.cfgMap[name] = config.GoPool{Name: name}
		}
	}
	module.cfgMap[PersistGoPoolName] = config.GoPool{Name: PersistGoPoolName, DisablePurgeRunning: true, Expire: time.Hour}
	instance = &module
	return &module
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
}

func (p *StatefulPoolsModule) BeforeShutdown() {
}

func (p *StatefulPoolsModule) Shutdown() error {
	return nil
}
