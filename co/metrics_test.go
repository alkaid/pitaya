package co

import (
	"context"
	"maps"
	"testing"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type recordedPoolMetric struct {
	name  string
	tags  map[string]string
	value float64
}

type recordingPoolReporter struct {
	counts []recordedPoolMetric
	gauges []recordedPoolMetric
}

func (r *recordingPoolReporter) ReportCount(name string, tags map[string]string, value float64) error {
	r.counts = append(r.counts, recordedPoolMetric{name: name, tags: maps.Clone(tags), value: value})
	return nil
}

func (r *recordingPoolReporter) ReportGauge(name string, tags map[string]string, value float64) error {
	r.gauges = append(r.gauges, recordedPoolMetric{name: name, tags: maps.Clone(tags), value: value})
	return nil
}

func (r *recordingPoolReporter) ReportHistogram(string, map[string]string, float64) error {
	return nil
}

func (r *recordingPoolReporter) ReportSummary(string, map[string]string, float64) error {
	return nil
}

func TestPoolMonitorStateReconcilesEscapeBudgets(t *testing.T) {
	both := ants.PoolWithIDEscapeGlobalBudgetExhausted | ants.PoolWithIDEscapePerIDBudgetExhausted
	state := newPoolMonitorState(ants.PoolWithIDEscapeSnapshot{
		GlobalBudget: 32,
		PerIDBudget:  10,
		ExhaustedByID: map[int]ants.PoolWithIDEscapeBudgetReason{
			1: ants.PoolWithIDEscapeGlobalBudgetExhausted,
			2: ants.PoolWithIDEscapePerIDBudgetExhausted,
			3: both,
		},
	})
	assertPoolBudgetState(t, state, 2, 2, 32, 10)

	state.apply(ants.PoolWithIDEscapeEvent{
		Type:         ants.PoolWithIDEscapeBudgetExhausted,
		ID:           4,
		BudgetReason: ants.PoolWithIDEscapeGlobalBudgetExhausted,
		GlobalBudget: 40,
		PerIDBudget:  11,
	})
	state.apply(ants.PoolWithIDEscapeEvent{
		Type:         ants.PoolWithIDEscapeBudgetExhausted,
		ID:           4,
		BudgetReason: ants.PoolWithIDEscapePerIDBudgetExhausted,
	})
	assertPoolBudgetState(t, state, 3, 3, 40, 11)

	state.apply(ants.PoolWithIDEscapeEvent{Type: ants.PoolWithIDWorkerEscaped, ID: 3})
	assertPoolBudgetState(t, state, 2, 2, 40, 11)

	state.reconcile(ants.PoolWithIDEscapeSnapshot{GlobalBudget: 41, PerIDBudget: 12})
	assertPoolBudgetState(t, state, 0, 0, 41, 12)
}

func TestStatefulPoolReportsPoolWithIDMetrics(t *testing.T) {
	reporter := &recordingPoolReporter{}
	pool, err := NewStatefulPool(config.GoPool{
		Name:                "test",
		DisablePurge:        true,
		DisableTimeoutWatch: true,
	}, []metrics.Reporter{reporter})
	if err != nil {
		t.Fatalf("NewStatefulPool: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	done := pool.Go(context.Background(), 1, func(context.Context) {
		close(started)
		<-release
	}, true)
	<-started
	t.Cleanup(func() {
		close(release)
		<-done
		pool.pool.Release()
	})

	state := newPoolMonitorState(pool.pool.EscapeSnapshot())
	state.setReason(1, ants.PoolWithIDEscapeGlobalBudgetExhausted)
	state.setReason(2, ants.PoolWithIDEscapePerIDBudgetExhausted)
	pool.reportMetrics(state)

	assertPoolGauge(t, reporter, metrics.PoolWorkers, map[string]string{
		"pool": "test", "state": poolWorkerStateManaged,
	}, 1)
	assertPoolGauge(t, reporter, metrics.PoolWorkers, map[string]string{
		"pool": "test", "state": poolWorkerStateEscaped,
	}, 0)
	assertPoolGauge(t, reporter, metrics.PoolWorkers, map[string]string{
		"pool": "test", "state": poolWorkerStateTotal,
	}, 1)
	assertPoolGauge(t, reporter, metrics.PoolWaitingTasks, map[string]string{"pool": "test"}, 0)
	assertPoolGauge(t, reporter, metrics.PoolEscapeEventsDropped, map[string]string{"pool": "test"}, 0)
	assertPoolGauge(t, reporter, metrics.PoolEscapeBudgetLimit, map[string]string{
		"pool": "test", "scope": poolEscapeScopeGlobal,
	}, float64(ants.DefaultAntsPoolSize/4))
	assertPoolGauge(t, reporter, metrics.PoolEscapeBudgetLimit, map[string]string{
		"pool": "test", "scope": poolEscapeScopePerID,
	}, 10)
	assertPoolGauge(t, reporter, metrics.PoolEscapeBudgetExhausted, map[string]string{
		"pool": "test", "scope": poolEscapeScopeGlobal,
	}, 1)
	assertPoolGauge(t, reporter, metrics.PoolEscapeBudgetExhausted, map[string]string{
		"pool": "test", "scope": poolEscapeScopePerID,
	}, 1)

	pool.reportEscapeEvent(ants.PoolWithIDEscapeEvent{
		Type:         ants.PoolWithIDEscapeBudgetExhausted,
		BudgetReason: ants.PoolWithIDEscapeGlobalBudgetExhausted | ants.PoolWithIDEscapePerIDBudgetExhausted,
	})
	assertPoolCount(t, reporter, metrics.PoolEscapeEvents, map[string]string{
		"pool": "test", "event": "budget_exhausted", "reason": "global_and_per_id",
	}, 1)
}

func TestStatefulPoolLogsEscapeErrors(t *testing.T) {
	core, observed := observer.New(zapcore.ErrorLevel)
	previousLogger := logger.Zap
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousLogger })

	pool := &StatefulPool{config: config.GoPool{Name: "test"}}
	pool.logEscapeEvent(ants.PoolWithIDEscapeEvent{
		Type:         ants.PoolWithIDWorkerEscaped,
		ID:           42,
		Generation:   3,
		Total:        2,
		ByID:         1,
		GlobalBudget: 32,
		PerIDBudget:  10,
	})
	pool.logEscapeEvent(ants.PoolWithIDEscapeEvent{
		Type:         ants.PoolWithIDEscapeBudgetExhausted,
		ID:           43,
		Generation:   3,
		Total:        32,
		ByID:         10,
		BudgetReason: ants.PoolWithIDEscapeGlobalBudgetExhausted,
		GlobalBudget: 32,
		PerIDBudget:  10,
	})

	entries := observed.All()
	if len(entries) != 2 {
		t.Fatalf("got %d error logs, want 2: %+v", len(entries), entries)
	}
	if entries[0].Message != "stateful pool worker escaped" {
		t.Fatalf("unexpected escape log message: %q", entries[0].Message)
	}
	escapeFields := entries[0].ContextMap()
	if escapeFields["pool"] != "test" || escapeFields["goID"] != int64(42) ||
		escapeFields["generation"] != uint64(3) || escapeFields["escapedTotal"] != int64(2) {
		t.Fatalf("unexpected escape log fields: %+v", escapeFields)
	}
	if entries[1].Message != "stateful pool escape budget exhausted" {
		t.Fatalf("unexpected budget log message: %q", entries[1].Message)
	}
	if reason := entries[1].ContextMap()["reason"]; reason != poolEscapeScopeGlobal {
		t.Fatalf("unexpected budget exhaustion reason: %v", reason)
	}
}

func assertPoolBudgetState(
	t *testing.T,
	state *poolMonitorState,
	globalExhausted, perIDExhausted, globalLimit, perIDLimit int,
) {
	t.Helper()
	if state.globalExhausted != globalExhausted || state.perIDExhausted != perIDExhausted ||
		state.globalBudgetLimit != globalLimit || state.perIDBudgetLimit != perIDLimit {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func assertPoolGauge(
	t *testing.T,
	reporter *recordingPoolReporter,
	name string,
	tags map[string]string,
	want float64,
) {
	t.Helper()
	assertRecordedPoolMetric(t, reporter.gauges, name, tags, want)
}

func assertPoolCount(
	t *testing.T,
	reporter *recordingPoolReporter,
	name string,
	tags map[string]string,
	want float64,
) {
	t.Helper()
	assertRecordedPoolMetric(t, reporter.counts, name, tags, want)
}

func assertRecordedPoolMetric(
	t *testing.T,
	reports []recordedPoolMetric,
	name string,
	tags map[string]string,
	want float64,
) {
	t.Helper()
	for _, report := range reports {
		if report.name == name && maps.Equal(report.tags, tags) {
			if report.value != want {
				t.Fatalf("metric %s%v = %v, want %v", name, tags, report.value, want)
			}
			return
		}
	}
	t.Fatalf("metric %s%v not reported; got %+v", name, tags, reports)
}
