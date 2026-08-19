// Copyright (c) nano Author and TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package timer

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/util"
)

const (
	defaultTimerBacklog = 1 << 8

	// LoopForever is a constant indicating that timer should loop forever.
	LoopForever = -1
)

var (
	timerConfigMu sync.RWMutex
	timerBacklog  = defaultTimerBacklog
)

// ExecutionPolicy defines what happens when a callback takes longer than its
// timer interval.
type ExecutionPolicy uint8

const (
	// ExecutionSerial is the default. The manager waits for the callback and does
	// not consume another tick until it returns. Create and close mutations remain
	// serviceable so callbacks can safely manage timers.
	ExecutionSerial ExecutionPolicy = iota
	// ExecutionAsync starts every eligible callback in its own goroutine and
	// explicitly permits callbacks for the same timer to overlap.
	ExecutionAsync
	// ExecutionSkipIfRunning starts callbacks asynchronously but drops an
	// occurrence while a callback for the same timer is still running.
	ExecutionSkipIfRunning
)

func (p ExecutionPolicy) String() string {
	switch p {
	case ExecutionSerial:
		return "serial"
	case ExecutionAsync:
		return "async"
	case ExecutionSkipIfRunning:
		return "skip-if-running"
	default:
		return "unknown"
	}
}

func validateExecutionPolicy(policy ExecutionPolicy) {
	switch policy {
	case ExecutionSerial, ExecutionAsync, ExecutionSkipIfRunning:
		return
	default:
		panic("pitaya/timer: invalid execution policy")
	}
}

type (
	// Func represents a function which will be called periodically.
	Func func()

	// Condition represents a checker that returns true when cron job needs to execute.
	Condition interface {
		Check(now time.Time) bool
	}

	// Timer represents a cron job.
	Timer struct {
		ID       int64         // timer id
		fn       Func          // function that execute
		createAt int64         // timer create time
		interval time.Duration // execution interval
		elapse   int64         // total elapse time
		closed   int32         // is timer closed
		counter  int           // counter
		manager  *TimerManager
		policy   ExecutionPolicy
		running  int32

		conditionMu sync.RWMutex
		condition   Condition // condition to cron job execution
	}
)

// ManagerStats is a lock-free snapshot of timer scheduling health. Tick lag is
// the delay between a ticker event and owner consumption. Callback lag is the
// delay between an interval timer's due time and callback admission.
type ManagerStats struct {
	Ticks           uint64
	Callbacks       uint64
	Overruns        uint64
	SerialOverruns  uint64
	AsyncOverruns   uint64
	SkippedOverruns uint64
	LastTickLag     time.Duration
	MaxTickLag      time.Duration
	LastCallbackLag time.Duration
	MaxCallbackLag  time.Duration
}

type managerStats struct {
	ticks           atomic.Uint64
	callbacks       atomic.Uint64
	overruns        atomic.Uint64
	serialOverruns  atomic.Uint64
	asyncOverruns   atomic.Uint64
	skippedOverruns atomic.Uint64
	lastTickLag     atomic.Int64
	maxTickLag      atomic.Int64
	lastCallbackLag atomic.Int64
	maxCallbackLag  atomic.Int64
}

type managerOwnerKind uint8

const (
	managerOwnerNone managerOwnerKind = iota
	managerOwnerRun
	managerOwnerCron
)

// TimerManager owns timer creation, closing, and tick consumption. Run uses an
// ownership token so multiple App dispatchers may call it while only one of them
// mutates timer scheduling state at a time.
type TimerManager struct {
	incrementID int64
	timers      sync.Map

	// ChClosingTimer and ChCreatedTimer remain exported for source compatibility.
	// New code should use Timer.Stop and NewTimerWithPolicy.
	ChClosingTimer chan int64
	ChCreatedTimer chan *Timer

	owner        chan struct{}
	ownerStateMu sync.Mutex
	ownerKind    managerOwnerKind
	ownerDone    chan struct{}
	tickMu       sync.Mutex
	used         atomic.Bool
	callbackMu   sync.Mutex
	callbackCV   *sync.Cond
	callbackWake chan struct{}
	callbacks    int
	stats        managerStats
}

var (
	// Manager is the process-wide manager for all timers.
	Manager = newTimerManager(defaultTimerBacklog)

	// Precision indicates the precision of timer, default is time.Second.
	Precision = time.Second

	// GlobalTicker represents the global ticker used by the App timer owner.
	GlobalTicker *time.Ticker
)

// NewTimerManager creates an isolated timer manager. A single Run owner consumes
// all of its create, close, and tick events.
func NewTimerManager(backlog int) *TimerManager {
	if backlog == 0 {
		timerConfigMu.RLock()
		backlog = timerBacklog
		timerConfigMu.RUnlock()
	}
	return newTimerManager(backlog)
}

func newTimerManager(backlog int) *TimerManager {
	if backlog < 16 {
		backlog = 16
	}
	m := &TimerManager{
		ChClosingTimer: make(chan int64, backlog),
		ChCreatedTimer: make(chan *Timer, backlog),
		owner:          make(chan struct{}, 1),
		callbackWake:   make(chan struct{}, 1),
	}
	m.callbackCV = sync.NewCond(&m.callbackMu)
	m.owner <- struct{}{}
	return m
}

// CreateAt returns the timer creation time in nanoseconds.
func (t *Timer) CreateAt() int64 {
	return t.createAt
}

// SetCondition sets the condition used for verifying when the cron job should run.
func (t *Timer) SetCondition(condition Condition) {
	t.conditionMu.Lock()
	t.condition = condition
	t.conditionMu.Unlock()
}

func (t *Timer) getCondition() Condition {
	t.conditionMu.RLock()
	condition := t.condition
	t.conditionMu.RUnlock()
	return condition
}

// Stop turns off a timer. A callback that is already running is allowed to finish.
func (t *Timer) Stop() {
	if !atomic.CompareAndSwapInt32(&t.closed, 0, 1) {
		return
	}

	manager := t.manager
	if manager == nil {
		manager = processManager()
	}
	// Stop must remain non-blocking. If the owner queue is full, the owner removes
	// the closed timer during its next tick.
	select {
	case manager.ChClosingTimer <- t.ID:
	default:
	}
}

// Run consumes create, close, and tick events until ctx is canceled. At most one
// caller owns a TimerManager at a time; other callers wait as warm standbys.
func (m *TimerManager) Run(ctx context.Context, ticks <-chan time.Time) {
	m.used.Store(true)
	select {
	case <-ctx.Done():
		// A compatibility Cron is outside the App dispatcher lifecycle, so a
		// canceled standby must wait for it. Another Run owns its own lifecycle
		// and must not keep an independently canceled standby alive.
		m.waitForCronOwner()
		return
	case <-m.owner:
		m.markOwner(managerOwnerRun)
	}

	defer func() {
		m.waitForCallbacksDrainingMutations()
		m.releaseOwner()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-m.ChCreatedTimer:
			m.addTimer(t)
		case id := <-m.ChClosingTimer:
			m.removeTimer(id)
		case tick, ok := <-ticks:
			if !ok {
				return
			}
			m.tick(tick, time.Now())
		}
	}
}

// NewTimer creates a serial cron job managed by the process-wide Manager.
func NewTimer(fn Func, interval time.Duration, counter int) *Timer {
	return NewTimerWithPolicy(fn, interval, counter, ExecutionSerial)
}

// NewTimerWithPolicy creates a cron job with an explicit overrun policy.
func NewTimerWithPolicy(fn Func, interval time.Duration, counter int, policy ExecutionPolicy) *Timer {
	return processManager().NewTimerWithPolicy(fn, interval, counter, policy)
}

// NewTimerWithPolicy creates and queues a timer owned by m.
func (m *TimerManager) NewTimerWithPolicy(fn Func, interval time.Duration, counter int, policy ExecutionPolicy) *Timer {
	if fn == nil {
		panic("pitaya/timer: nil timer function")
	}
	if interval <= 0 {
		panic("pitaya/timer: non-positive interval")
	}
	validateExecutionPolicy(policy)
	m.used.Store(true)

	id := atomic.AddInt64(&m.incrementID, 1)
	t := &Timer{
		ID:       id,
		fn:       fn,
		createAt: time.Now().UnixNano(),
		interval: interval,
		elapse:   int64(interval),
		counter:  counter,
		manager:  m,
		policy:   policy,
	}
	m.ChCreatedTimer <- t
	return t
}

// AddTimer queues t for creation by its manager owner.
func AddTimer(t *Timer) {
	if t == nil {
		panic("pitaya/timer: nil timer")
	}
	manager := t.manager
	if manager == nil {
		manager = processManager()
		t.manager = manager
	}
	manager.used.Store(true)
	manager.ChCreatedTimer <- t
}

// RemoveTimer queues id for removal by the process-wide manager owner.
func RemoveTimer(id int64) {
	manager := processManager()
	manager.used.Store(true)
	manager.ChClosingTimer <- id
}

func (m *TimerManager) addTimer(t *Timer) {
	if t == nil || atomic.LoadInt32(&t.closed) != 0 {
		return
	}
	m.timers.Store(t.ID, t)
}

func (m *TimerManager) removeTimer(id int64) {
	if value, ok := m.timers.Load(id); ok {
		atomic.StoreInt32(&value.(*Timer).closed, 1)
	}
	m.timers.Delete(id)
}

// Cron executes one process-wide timer tick when no Run or Cron caller owns the
// manager. App runtime code should use Run. Callers that need to observe a
// rejected compatibility tick should use TryCron.
func Cron() {
	TryCron()
}

// TryCron executes one process-wide timer tick and reports whether it acquired
// manager ownership. It returns false instead of racing an active Run owner.
func TryCron() bool {
	now := time.Now()
	manager := processManager()
	manager.used.Store(true)
	manager.ownerStateMu.Lock()
	select {
	case <-manager.owner:
	default:
		manager.ownerStateMu.Unlock()
		return false
	}
	manager.ownerKind = managerOwnerCron
	manager.ownerDone = make(chan struct{})
	manager.ownerStateMu.Unlock()
	defer manager.releaseOwner()
	manager.drainMutations()
	manager.tick(now, now)
	return true
}

func (m *TimerManager) markOwner(kind managerOwnerKind) {
	m.ownerStateMu.Lock()
	m.ownerKind = kind
	m.ownerDone = make(chan struct{})
	m.ownerStateMu.Unlock()
}

func (m *TimerManager) releaseOwner() {
	m.ownerStateMu.Lock()
	done := m.ownerDone
	m.ownerKind = managerOwnerNone
	m.ownerDone = nil
	m.owner <- struct{}{}
	if done != nil {
		close(done)
	}
	m.ownerStateMu.Unlock()
}

func (m *TimerManager) waitForCronOwner() {
	m.ownerStateMu.Lock()
	var done <-chan struct{}
	if m.ownerKind == managerOwnerCron {
		done = m.ownerDone
	}
	m.ownerStateMu.Unlock()
	if done != nil {
		<-done
	}
}

func (m *TimerManager) drainMutations() {
	for {
		select {
		case t := <-m.ChCreatedTimer:
			m.addTimer(t)
		case id := <-m.ChClosingTimer:
			m.removeTimer(id)
		default:
			return
		}
	}
}

func (m *TimerManager) tick(tickAt, handledAt time.Time) {
	m.tickMu.Lock()
	defer m.tickMu.Unlock()

	m.stats.ticks.Add(1)
	tickLag := handledAt.Sub(tickAt)
	if tickLag < 0 {
		tickLag = 0
	}
	m.recordLag(&m.stats.lastTickLag, &m.stats.maxTickLag, tickLag)

	nowUnix := handledAt.UnixNano()
	m.timers.Range(func(idInterface, timerInterface interface{}) bool {
		t := timerInterface.(*Timer)
		if atomic.LoadInt32(&t.closed) != 0 || t.counter == 0 {
			m.removeTimer(idInterface.(int64))
			return true
		}

		if condition := t.getCondition(); condition != nil {
			if m.checkCondition(t, condition, handledAt) {
				m.execute(t)
			}
			return true
		}

		dueAt := t.createAt + t.elapse
		if dueAt > nowUnix {
			return true
		}

		callbackLag := time.Duration(nowUnix - dueAt)
		m.recordLag(&m.stats.lastCallbackLag, &m.stats.maxCallbackLag, callbackLag)
		t.elapse += int64(t.interval)
		if m.execute(t) && t.counter != LoopForever && t.counter > 0 {
			t.counter--
			if t.counter == 0 {
				m.removeTimer(t.ID)
			}
		}
		return true
	})
}

func (m *TimerManager) checkCondition(t *Timer, condition Condition, now time.Time) (ready bool) {
	type result struct {
		ready     bool
		recovered any
	}
	results := make(chan result, 1)
	go func() {
		conditionResult := result{}
		defer func() {
			conditionResult.recovered = recover()
			results <- conditionResult
		}()
		conditionResult.ready = condition.Check(now)
	}()

	for {
		select {
		case conditionResult := <-results:
			if conditionResult.recovered != nil {
				m.removeTimer(t.ID)
				util.LogRecoveredPanic(
					logger.Zap,
					"timer condition panicked",
					conditionResult.recovered,
					zap.Int64("TimerID", t.ID),
				)
				return false
			}
			return conditionResult.ready
		case created := <-m.ChCreatedTimer:
			m.addTimer(created)
		case id := <-m.ChClosingTimer:
			m.removeTimer(id)
		}
	}
}

func (m *TimerManager) execute(t *Timer) bool {
	switch t.policy {
	case ExecutionSerial:
		m.stats.callbacks.Add(1)
		started := time.Now()
		m.runSerial(t)
		if elapsed := time.Since(started); elapsed > t.interval {
			m.recordOverrun(ExecutionSerial)
		}
		return true

	case ExecutionAsync:
		if atomic.AddInt32(&t.running, 1) > 1 {
			m.recordOverrun(ExecutionAsync)
		}
		m.stats.callbacks.Add(1)
		m.addCallback()
		go m.runAsync(t)
		return true

	case ExecutionSkipIfRunning:
		if !atomic.CompareAndSwapInt32(&t.running, 0, 1) {
			m.recordOverrun(ExecutionSkipIfRunning)
			return false
		}
		m.stats.callbacks.Add(1)
		m.addCallback()
		go m.runAsync(t)
		return true

	default:
		panic("pitaya/timer: invalid execution policy")
	}
}

// runSerial preserves serial callback semantics while allowing the scheduling
// owner to unblock timer mutations issued by the callback itself.
func (m *TimerManager) runSerial(t *Timer) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		pexec(t.ID, t.fn)
	}()

	for {
		select {
		case <-done:
			return
		default:
		}

		select {
		case created := <-m.ChCreatedTimer:
			m.addTimer(created)
		case id := <-m.ChClosingTimer:
			m.removeTimer(id)
		case <-done:
			return
		}
	}
}

func (m *TimerManager) runAsync(t *Timer) {
	defer m.callbackDone()
	defer atomic.AddInt32(&t.running, -1)
	pexec(t.ID, t.fn)
}

func (m *TimerManager) addCallback() {
	m.callbackMu.Lock()
	m.callbacks++
	m.callbackMu.Unlock()
	m.signalCallbackChange()
}

func (m *TimerManager) callbackDone() {
	m.callbackMu.Lock()
	m.callbacks--
	if m.callbacks == 0 {
		m.callbackCV.Broadcast()
	}
	m.callbackMu.Unlock()
	m.signalCallbackChange()
}

func (m *TimerManager) signalCallbackChange() {
	select {
	case m.callbackWake <- struct{}{}:
	default:
	}
}

func (m *TimerManager) waitForCallbacks() {
	m.callbackMu.Lock()
	for m.callbacks > 0 {
		m.callbackCV.Wait()
	}
	m.callbackMu.Unlock()
}

func (m *TimerManager) waitForCallbacksDrainingMutations() {
	for {
		m.callbackMu.Lock()
		pending := m.callbacks
		m.callbackMu.Unlock()
		if pending == 0 {
			// The last callback has completed all of its sends. Apply mutations that
			// were accepted just before callbackDone before releasing ownership.
			m.drainMutations()
			return
		}

		select {
		case created := <-m.ChCreatedTimer:
			m.addTimer(created)
		case id := <-m.ChClosingTimer:
			m.removeTimer(id)
		case <-m.callbackWake:
		}
	}
}

func (m *TimerManager) recordOverrun(policy ExecutionPolicy) {
	m.stats.overruns.Add(1)
	switch policy {
	case ExecutionSerial:
		m.stats.serialOverruns.Add(1)
	case ExecutionAsync:
		m.stats.asyncOverruns.Add(1)
	case ExecutionSkipIfRunning:
		m.stats.skippedOverruns.Add(1)
	}
}

func (m *TimerManager) recordLag(last, maximum *atomic.Int64, lag time.Duration) {
	value := int64(lag)
	last.Store(value)
	for {
		current := maximum.Load()
		if value <= current || maximum.CompareAndSwap(current, value) {
			return
		}
	}
}

// Stats returns the current scheduling and overrun counters.
func (m *TimerManager) Stats() ManagerStats {
	return ManagerStats{
		Ticks:           m.stats.ticks.Load(),
		Callbacks:       m.stats.callbacks.Load(),
		Overruns:        m.stats.overruns.Load(),
		SerialOverruns:  m.stats.serialOverruns.Load(),
		AsyncOverruns:   m.stats.asyncOverruns.Load(),
		SkippedOverruns: m.stats.skippedOverruns.Load(),
		LastTickLag:     time.Duration(m.stats.lastTickLag.Load()),
		MaxTickLag:      time.Duration(m.stats.maxTickLag.Load()),
		LastCallbackLag: time.Duration(m.stats.lastCallbackLag.Load()),
		MaxCallbackLag:  time.Duration(m.stats.maxCallbackLag.Load()),
	}
}

// SetTimerBacklog configures the process-wide manager and the default used by
// NewTimerManager(0). It must be called before the process-wide manager is run
// or used to create, add, remove, or dispatch a timer.
func SetTimerBacklog(c int) {
	if c < 16 {
		c = 16
	}

	timerConfigMu.Lock()
	defer timerConfigMu.Unlock()
	if Manager != nil && (Manager.used.Load() || len(Manager.ChCreatedTimer) > 0 || len(Manager.ChClosingTimer) > 0) {
		panic("pitaya/timer: timer backlog cannot change after manager use")
	}
	timerBacklog = c
	Manager = newTimerManager(c)
}

func processManager() *TimerManager {
	timerConfigMu.RLock()
	manager := Manager
	timerConfigMu.RUnlock()
	return manager
}

// execute job function with panic protection.
func pexec(id int64, fn Func) {
	defer func() {
		if recovered := recover(); recovered != nil {
			util.LogRecoveredPanic(
				logger.Zap,
				"timer callback panicked",
				recovered,
				zap.Int64("TimerID", id),
			)
		}
	}()

	fn()
}
