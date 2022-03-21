package co

import (
	"sync"
	"time"

	"github.com/panjf2000/ants/v2"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"go.uber.org/zap"
)

// TODO 后期考虑放到 pitaya/internal/co 去

const (
	GroupIdPitaya = "_pitaya"
)

var holder *HolderModule

type HolderModule struct {
	groups      map[string][]*Looper // Looper mapping
	pitayaGroup []*Looper
	Died        bool
	// 默认全局 Looper Group
	pitayaSingleLooper *Looper    // 默认全局单个 Looper
	gopool             *ants.Pool // 线程池,主要用于分离session线程
}

func NewHolder(config CoroutineConfig) *HolderModule {
	if holder == nil {
		holder = &HolderModule{
			groups: make(map[string][]*Looper),
		}
		gs, err := RegNewGroupWithConfig(GroupIdPitaya, &config)
		if err != nil {
			logger.Zap.Fatal("reg goroutine group failed", zap.String("gid", GroupIdPitaya))
		}
		holder.pitayaGroup = gs
		holder.pitayaSingleLooper = NewLooper(0, config.Buffers)
		p, err := ants.NewPool(ants.DefaultAntsPoolSize, ants.WithTaskBuffer(ants.DefaultStatefulTaskBuffer), ants.WithExpiryDuration(time.Hour))
		if err != nil {
			logger.Zap.Fatal("create ants pool error", zap.Error(err))
		}
		holder.gopool = p
	}
	return holder
}

func (h *HolderModule) start() error {
	if holder.Died {
		return constants.ErrClosedGroup
	}
	h.pitayaSingleLooper.Start()
	for _, schs := range h.groups {
		for _, sch := range schs {
			sch.Start()
		}
	}
	return nil
}

func (h *HolderModule) stop() {
	if holder.Died {
		logger.Zap.Error("", zap.Error(constants.ErrClosedGroup))
	}
	holder.Died = true
	var wg sync.WaitGroup
	go func() {
		wg.Add(1)
		h.pitayaSingleLooper.Exit()
		wg.Done()
	}()
	for _, schs := range h.groups {
		for _, sch := range schs {
			go func() {
				wg.Add(1)
				sch.Exit()
				wg.Done()
			}()
		}
	}
	h.gopool.Release()
	// ants.Release()
	wg.Wait()
}

// GoByID 根据指定的goroutineID派发线程
//  @receiver h
//  @param goID 若>0,派发到指定线程,否则随机派发
//  @param task
func GoByID(goID int, task func()) {
	if goID > 0 {
		holder.gopool.SubmitWithID(goID, task)
	} else {
		Go(task)
	}
}

// Go 从默认线程池获取一个goroutine并派发任务
//  @param task
func Go(task func()) {
	ants.Submit(task)
}

// Init was called to initialize the component.
func (h *HolderModule) Init() error {
	return h.start()
}

// AfterInit was called after the component is initialized.
func (h *HolderModule) AfterInit() {}

// BeforeShutdown was called before the component to shutdown.
func (h *HolderModule) BeforeShutdown() {
	h.stop()
}

// Shutdown was called to shutdown the component.
func (h *HolderModule) Shutdown() error {
	return nil
}
