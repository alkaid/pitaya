package co

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/pitaya/v2/config"
)

type f12TaskRelease struct {
	ch   chan struct{}
	once sync.Once
}

func newF12TaskRelease(t *testing.T) *f12TaskRelease {
	t.Helper()
	release := &f12TaskRelease{ch: make(chan struct{})}
	t.Cleanup(release.close)
	return release
}

func (r *f12TaskRelease) close() {
	r.once.Do(func() { close(r.ch) })
}

func TestStatefulPoolUsesBoundedDefaults(t *testing.T) {
	pool, err := NewStatefulPool(config.GoPool{
		Name:                "bounded-defaults",
		DisablePurge:        true,
		DisableTimeoutWatch: true,
	}, nil)
	if err != nil {
		t.Fatalf("NewStatefulPool: %v", err)
	}
	t.Cleanup(pool.pool.Release)

	if pool.config.Expire != 10*time.Minute {
		t.Fatalf("Expire = %v, want 10m", pool.config.Expire)
	}
	snapshot := pool.pool.EscapeSnapshot()
	if snapshot.GlobalBudget != 128 || snapshot.PerIDBudget != 1 {
		t.Fatalf("escape budgets = %d/%d, want 128/1", snapshot.GlobalBudget, snapshot.PerIDBudget)
	}
}

func TestStatefulPoolRunningTaskTimeoutDefaultsToExpire(t *testing.T) {
	const expire = 37 * time.Minute
	pool, err := NewStatefulPool(config.GoPool{
		Name:                "legacy-running-timeout",
		Expire:              expire,
		DisablePurge:        true,
		DisableTimeoutWatch: true,
	}, nil)
	if err != nil {
		t.Fatalf("NewStatefulPool: %v", err)
	}
	t.Cleanup(pool.pool.Release)

	if pool.config.RunningTaskTimeout != expire {
		t.Fatalf("RunningTaskTimeout = %v, want legacy Expire %v", pool.config.RunningTaskTimeout, expire)
	}
}

func TestStatefulPoolsModulePreservesLegacyBuiltinDefaults(t *testing.T) {
	module := NewStatefulPoolsModule(nil, nil)

	for _, name := range []string{SessionGoPoolName, UserGoPoolName, DefaultGoPoolName} {
		if module.cfgMap[name].DisablePurgeRunning {
			t.Errorf("pool %q disables running-task escape by default", name)
		}
	}
	if !module.cfgMap[PersistGoPoolName].DisablePurgeRunning {
		t.Error("persist pool unexpectedly allows running-task escape")
	}
	if module.cfgMap[PersistGoPoolName].Expire != time.Hour {
		t.Fatalf("persist Expire = %v, want 1h", module.cfgMap[PersistGoPoolName].Expire)
	}
}

func TestStatefulPoolsModulePreservesEscapeDefaultsAndExplicitStrictMode(t *testing.T) {
	configured := map[string]config.GoPool{
		"room": {
			Expire:     5 * time.Minute,
			TaskBuffer: 1000,
		},
		SessionGoPoolName: {
			Expire: 90 * time.Minute,
		},
		UserGoPoolName: {
			Expire:              2 * time.Hour,
			DisablePurgeRunning: true,
		},
		PersistGoPoolName: {
			Expire:                 3 * time.Hour,
			MaxEscapedWorkers:      7,
			MaxEscapedWorkersPerID: 2,
		},
	}
	module := NewStatefulPoolsModule(configured, nil)

	room := module.cfgMap["room"]
	if room.Expire != 5*time.Minute || room.TaskBuffer != 1000 || room.DisablePurgeRunning {
		t.Fatalf("custom room pool lost the legacy escape default: %+v", room)
	}
	session := module.cfgMap[SessionGoPoolName]
	if session.Expire != 90*time.Minute || session.DisablePurgeRunning {
		t.Fatalf("partial session config lost the legacy escape default: %+v", session)
	}
	user := module.cfgMap[UserGoPoolName]
	if user.Name != UserGoPoolName || user.Expire != 2*time.Hour || !user.DisablePurgeRunning {
		t.Fatalf("explicit strict user config was not preserved: %+v", user)
	}
	persist := module.cfgMap[PersistGoPoolName]
	if persist.Name != PersistGoPoolName || persist.Expire != 3*time.Hour ||
		!persist.DisablePurgeRunning || persist.MaxEscapedWorkers != 7 ||
		persist.MaxEscapedWorkersPerID != 2 {
		t.Fatalf("explicit persist config was not preserved: %+v", persist)
	}
	if configured["room"].Name != "" || configured[SessionGoPoolName].Name != "" ||
		configured[UserGoPoolName].Name != "" ||
		configured[PersistGoPoolName].Name != "" {
		t.Fatal("NewStatefulPoolsModule mutated caller-owned config")
	}
}

func TestStatefulPoolRejectsNegativeEscapeBudgets(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.GoPool
	}{
		{
			name: "global",
			cfg: config.GoPool{
				Name:              "negative-global",
				MaxEscapedWorkers: -1,
			},
		},
		{
			name: "per-ID",
			cfg: config.GoPool{
				Name:                   "negative-per-id",
				MaxEscapedWorkersPerID: -1,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool, err := NewStatefulPool(test.cfg, nil)
			if pool != nil {
				pool.pool.Release()
				t.Fatal("negative escape budget returned a pool")
			}
			if !errors.Is(err, ants.ErrInvalidPoolWithIDEscapeBudget) {
				t.Fatalf("error = %v, want %v", err, ants.ErrInvalidPoolWithIDEscapeBudget)
			}
		})
	}
}

func TestStatefulPoolStrictModeDoesNotEscape(t *testing.T) {
	pool := newTestStatefulPoolWithConfig(t, config.GoPool{
		Name:                   "room",
		Expire:                 time.Hour,
		RunningTaskTimeout:     5 * time.Millisecond,
		MaxEscapedWorkers:      1,
		MaxEscapedWorkersPerID: 1,
		DisablePurgeRunning:    true,
		TaskBuffer:             2,
		DisableTimeoutWatch:    true,
	}, nil, 1)
	if !pool.config.DisablePurgeRunning {
		t.Fatal("custom room pool did not resolve to strict mode")
	}
	releaseFirst := newF12TaskRelease(t)
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})

	firstDone := mustSubmitPoolTask(t, pool, context.Background(), 1, func(context.Context) {
		close(firstStarted)
		<-releaseFirst.ch
	}, true)
	waitForPoolSignal(t, firstStarted, "strict first task start")
	secondDone := mustSubmitPoolTask(t, pool, context.Background(), 1, func(context.Context) {
		close(secondStarted)
	}, true)

	escapeControl := newTestStatefulPoolWithConfig(t, config.GoPool{
		Name:                   "room-escape-control",
		Expire:                 time.Hour,
		RunningTaskTimeout:     5 * time.Millisecond,
		MaxEscapedWorkers:      1,
		MaxEscapedWorkersPerID: 1,
		TaskBuffer:             2,
		DisableTimeoutWatch:    true,
	}, nil, 1)
	controlRelease := newF12TaskRelease(t)
	controlFirstStarted := make(chan struct{})
	controlSecondStarted := make(chan struct{})
	controlFirstDone := mustSubmitPoolTask(t, escapeControl, context.Background(), 1, func(context.Context) {
		close(controlFirstStarted)
		<-controlRelease.ch
	}, true)
	waitForPoolSignal(t, controlFirstStarted, "escape control first task start")
	controlSecondDone := mustSubmitPoolTask(t, escapeControl, context.Background(), 1, func(context.Context) {
		close(controlSecondStarted)
	}, true)
	waitForPoolSignal(t, controlSecondStarted, "escape control replacement start")

	select {
	case <-secondStarted:
		t.Fatal("strict pool ran the same ID concurrently")
	default:
	}
	if snapshot := pool.pool.EscapeSnapshot(); snapshot.Total != 0 || len(snapshot.ByID) != 0 {
		t.Fatalf("strict pool escaped a worker: %+v", snapshot)
	}
	controlRelease.close()
	waitForPoolSignal(t, controlFirstDone, "escape control first task completion")
	waitForPoolSignal(t, controlSecondDone, "escape control replacement completion")

	releaseFirst.close()
	waitForPoolSignal(t, firstDone, "strict first task completion")
	waitForPoolSignal(t, secondStarted, "strict second task serial start")
	waitForPoolSignal(t, secondDone, "strict second task completion")
}

func TestStatefulPoolGlobalEscapeBudget(t *testing.T) {
	pool := newTestStatefulPoolWithConfig(t, config.GoPool{
		Name:                   "global-budget",
		Expire:                 time.Hour,
		RunningTaskTimeout:     5 * time.Millisecond,
		MaxEscapedWorkers:      2,
		MaxEscapedWorkersPerID: 1,
		TaskBuffer:             2,
		DisableTimeoutWatch:    true,
	}, nil, 3)

	releases := make(map[int]*f12TaskRelease, 3)
	firstDone := make(map[int]<-chan struct{}, 3)
	secondDone := make(map[int]<-chan struct{}, 3)
	replacements := make(chan int, 3)
	for id := 1; id <= 3; id++ {
		release := newF12TaskRelease(t)
		releases[id] = release
		started := make(chan struct{})
		firstDone[id] = mustSubmitPoolTask(t, pool, context.Background(), id, func(context.Context) {
			close(started)
			<-release.ch
		}, true)
		waitForPoolSignal(t, started, "global-budget first task start")
		taskID := id
		secondDone[id] = mustSubmitPoolTask(t, pool, context.Background(), id, func(context.Context) {
			replacements <- taskID
		}, true)
	}

	waitForPoolCondition(t, "global escape budget exhaustion", func() bool {
		snapshot := pool.pool.EscapeSnapshot()
		if snapshot.Total != 2 {
			return false
		}
		for _, reason := range snapshot.ExhaustedByID {
			if reason&ants.PoolWithIDEscapeGlobalBudgetExhausted != 0 {
				return true
			}
		}
		return false
	})

	started := make(map[int]bool, 3)
	for len(started) < 2 {
		select {
		case id := <-replacements:
			started[id] = true
		case <-time.After(poolTestTimeout):
			t.Fatal("timed out waiting for the first two replacements")
		}
	}
	select {
	case id := <-replacements:
		t.Fatalf("replacement for ID %d exceeded global escape budget", id)
	case <-time.After(20 * time.Millisecond):
	}

	snapshot := pool.pool.EscapeSnapshot()
	var releasedID int
	for id := range snapshot.ByID {
		releasedID = id
		break
	}
	if releasedID == 0 {
		t.Fatalf("no escaped ID in snapshot: %+v", snapshot)
	}
	releases[releasedID].close()

	select {
	case id := <-replacements:
		if started[id] {
			t.Fatalf("replacement for ID %d ran twice", id)
		}
		started[id] = true
	case <-time.After(poolTestTimeout):
		t.Fatal("timed out waiting for replacement after global budget recovery")
	}
	if len(started) != 3 {
		t.Fatalf("replacement IDs = %v, want all three", started)
	}

	for _, release := range releases {
		release.close()
	}
	for id := 1; id <= 3; id++ {
		waitForPoolSignal(t, firstDone[id], "global-budget first task completion")
		waitForPoolSignal(t, secondDone[id], "global-budget replacement completion")
	}
	waitForPoolCondition(t, "global escaped workers to exit", func() bool {
		return pool.pool.EscapeSnapshot().Total == 0
	})
}

func TestStatefulPoolPerIDEscapeBudget(t *testing.T) {
	pool := newTestStatefulPoolWithConfig(t, config.GoPool{
		Name:                   "per-id-budget",
		Expire:                 time.Hour,
		RunningTaskTimeout:     5 * time.Millisecond,
		MaxEscapedWorkers:      4,
		MaxEscapedWorkersPerID: 1,
		TaskBuffer:             3,
		DisableTimeoutWatch:    true,
	}, nil, 1)
	releaseFirst := newF12TaskRelease(t)
	releaseSecond := newF12TaskRelease(t)
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	thirdStarted := make(chan struct{})

	firstDone := mustSubmitPoolTask(t, pool, context.Background(), 1, func(context.Context) {
		close(firstStarted)
		<-releaseFirst.ch
	}, true)
	waitForPoolSignal(t, firstStarted, "per-ID first task start")
	secondDone := mustSubmitPoolTask(t, pool, context.Background(), 1, func(context.Context) {
		close(secondStarted)
		<-releaseSecond.ch
	}, true)
	thirdDone := mustSubmitPoolTask(t, pool, context.Background(), 1, func(context.Context) {
		close(thirdStarted)
	}, true)
	waitForPoolSignal(t, secondStarted, "per-ID second task after first escape")

	waitForPoolCondition(t, "per-ID escape budget exhaustion", func() bool {
		snapshot := pool.pool.EscapeSnapshot()
		return snapshot.Total == 1 &&
			snapshot.ExhaustedByID[1]&ants.PoolWithIDEscapePerIDBudgetExhausted != 0
	})
	select {
	case <-thirdStarted:
		t.Fatal("third same-ID task started while per-ID escape budget was exhausted")
	case <-time.After(20 * time.Millisecond):
	}

	releaseFirst.close()
	waitForPoolSignal(t, firstDone, "per-ID first escaped task completion")
	waitForPoolSignal(t, thirdStarted, "per-ID third task after budget recovery")
	waitForPoolSignal(t, thirdDone, "per-ID third task completion")
	releaseSecond.close()
	waitForPoolSignal(t, secondDone, "per-ID second escaped task completion")
	waitForPoolCondition(t, "per-ID escaped workers to exit", func() bool {
		return pool.pool.EscapeSnapshot().Total == 0
	})
}
