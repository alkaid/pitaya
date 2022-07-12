package co

import (
	"hash/crc32"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/panjf2000/ants/v2"
	"github.com/topfreegames/pitaya/v2/session"

	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"go.uber.org/zap"
)

// TODO 后期考虑放到 pitaya/internal/co 去

const (
	GroupIdPitaya = "_pitaya"
	MainThreadID  = math.MaxInt // 主线程ID
)

// LooperInstance 全局默认 Looper. 框架内部私有,若非特殊需求请勿直接调用
var LooperInstance Asyncio

var holder *HolderModule

type HolderModule struct {
	groups      map[string][]*Looper // Looper mapping
	pitayaGroup []*Looper
	Died        bool
	// 默认全局 Looper Group
	pitayaSingleLooper *Looper    // 默认全局单个 Looper,同 LooperInstance
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
		LooperInstance = holder.pitayaSingleLooper
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
	wg.Add(1)
	go func() {
		h.pitayaSingleLooper.Exit()
		wg.Done()
	}()
	for _, schs := range h.groups {
		for _, sch := range schs {
			wg.Add(1)
			go func() {
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
		err := holder.gopool.SubmitWithID(goID, task)
		if err != nil {
			logger.Zap.Error("submit task with id error", zap.Error(err), zap.Int("goID", goID))
			return
		}
	} else {
		Go(task)
	}
}

// GoByUID 根据uid派发任务线程
//  @param uid
//  @param task
func GoByUID(uid string, task func()) {
	if uid == "" {
		logger.Zap.Error("uid can not be empty")
	}
	goID, err := strconv.Atoi(uid)
	if err != nil {
		logger.Zap.Warn("can't atoi uid", zap.String("uid", uid), zap.Error(err))
		goID = int(crc32.ChecksumIEEE([]byte(uid)))
	}
	GoByID(goID, task)
}

// GoBySession 根据session数据决策派发任务线程
//  @param sess
//  @param task
func GoBySession(sess session.Session, task func()) {
	if sess.UID() != "" {
		GoByUID(sess.UID(), task)
		return
	}
	goID := 0
	if sess.ID() > 0 {
		goID = int(sess.ID())
	}
	GoByID(goID, task)
}

// Go 从默认线程池获取一个goroutine并派发任务
//  @param task
func Go(task func()) {
	err := ants.Submit(task)
	if err != nil {
		logger.Zap.Error("submit task error", zap.Error(err))
		return
	}
}

// GoMain 派发到主线程
//  @param task
func GoMain(task func()) {
	GoByID(MainThreadID, task)
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
