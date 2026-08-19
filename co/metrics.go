package co

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"go.uber.org/zap"
)

const (
	defaultStatefulPoolMetricsPeriod   = 15 * time.Second
	statefulPoolMetricsReconcilePeriod = 5 * time.Minute
)

const (
	poolWorkerStateManaged = "managed"
	poolWorkerStateEscaped = "escaped"
	poolWorkerStateTotal   = "total"

	poolEscapeScopeGlobal = "global"
	poolEscapeScopePerID  = "per_id"
)

type poolMonitorState struct {
	exhaustedByID     map[int]ants.PoolWithIDEscapeBudgetReason
	globalExhausted   int
	perIDExhausted    int
	globalBudgetLimit int
	perIDBudgetLimit  int
	droppedEvents     uint64
}

func resolveStatefulPoolMetricsPeriod(periods []time.Duration) time.Duration {
	if len(periods) == 0 || periods[0] <= 0 {
		return defaultStatefulPoolMetricsPeriod
	}
	return periods[0]
}

func newPoolMonitorState(snapshot ants.PoolWithIDEscapeSnapshot) *poolMonitorState {
	state := &poolMonitorState{}
	state.reconcile(snapshot)
	return state
}

func (s *poolMonitorState) reconcile(snapshot ants.PoolWithIDEscapeSnapshot) uint64 {
	previousDroppedEvents := s.droppedEvents
	s.exhaustedByID = make(map[int]ants.PoolWithIDEscapeBudgetReason, len(snapshot.ExhaustedByID))
	s.globalExhausted = 0
	s.perIDExhausted = 0
	s.globalBudgetLimit = snapshot.GlobalBudget
	s.perIDBudgetLimit = snapshot.PerIDBudget
	s.droppedEvents = snapshot.DroppedEvents
	for id, reason := range snapshot.ExhaustedByID {
		s.setReason(id, reason)
	}
	if snapshot.DroppedEvents <= previousDroppedEvents {
		return 0
	}
	return snapshot.DroppedEvents - previousDroppedEvents
}

func (s *poolMonitorState) apply(event ants.PoolWithIDEscapeEvent) {
	if event.GlobalBudget != 0 {
		s.globalBudgetLimit = event.GlobalBudget
	}
	if event.PerIDBudget != 0 {
		s.perIDBudgetLimit = event.PerIDBudget
	}

	switch event.Type {
	case ants.PoolWithIDEscapeBudgetExhausted:
		s.setReason(event.ID, s.exhaustedByID[event.ID]|event.BudgetReason)
	case ants.PoolWithIDWorkerEscaped:
		// A successful replacement means the previously exhausted status recovered.
		s.setReason(event.ID, 0)
	}
}

func (s *poolMonitorState) setReason(id int, reason ants.PoolWithIDEscapeBudgetReason) {
	previous := s.exhaustedByID[id]
	if previous&ants.PoolWithIDEscapeGlobalBudgetExhausted != 0 {
		s.globalExhausted--
	}
	if previous&ants.PoolWithIDEscapePerIDBudgetExhausted != 0 {
		s.perIDExhausted--
	}

	if reason == 0 {
		delete(s.exhaustedByID, id)
		return
	}
	s.exhaustedByID[id] = reason
	if reason&ants.PoolWithIDEscapeGlobalBudgetExhausted != 0 {
		s.globalExhausted++
	}
	if reason&ants.PoolWithIDEscapePerIDBudgetExhausted != 0 {
		s.perIDExhausted++
	}
}

func (s *StatefulPool) monitorMetrics(ctx context.Context, period time.Duration) {
	state := &poolMonitorState{}
	s.reconcileMonitorState(state, s.pool.EscapeSnapshot())

	var reportTicker *time.Ticker
	var reportC <-chan time.Time
	if len(s.reporters) > 0 {
		reportTicker = time.NewTicker(period)
		reportC = reportTicker.C
		defer reportTicker.Stop()
	}
	// EscapeEvents is best effort and budget recovery has no event of its own.
	// Periodically reconcile from the authoritative snapshot so gauges cannot
	// remain stale forever after a dropped event or a silent recovery.
	snapshotReconcileTicker := time.NewTicker(statefulPoolMetricsReconcilePeriod)
	defer snapshotReconcileTicker.Stop()

	for {
		select {
		case event := <-s.pool.EscapeEvents():
			state.apply(event)
			s.logEscapeEvent(event)
			s.reportEscapeEvent(event)
		case <-reportC:
			s.reportMetrics(state)
		case <-snapshotReconcileTicker.C:
			s.reconcileMonitorState(state, s.pool.EscapeSnapshot())
		case <-ctx.Done():
			s.reconcileMonitorState(state, s.pool.EscapeSnapshot())
			return
		}
	}
}

func (s *StatefulPool) reconcileMonitorState(
	state *poolMonitorState,
	snapshot ants.PoolWithIDEscapeSnapshot,
) {
	if droppedDelta := state.reconcile(snapshot); droppedDelta > 0 {
		logger.Zap.Error("stateful pool escape events dropped",
			zap.String("pool", s.Name()),
			zap.Uint64("droppedDelta", droppedDelta),
			zap.Uint64("droppedTotal", snapshot.DroppedEvents))
	}
	s.reportMetrics(state)
}

func (s *StatefulPool) logEscapeEvent(event ants.PoolWithIDEscapeEvent) {
	if event.Type != ants.PoolWithIDWorkerEscaped && event.Type != ants.PoolWithIDEscapeBudgetExhausted {
		return
	}
	fields := []zap.Field{
		zap.String("pool", s.Name()),
		zap.Int("goID", event.ID),
		zap.Uint64("generation", event.Generation),
		zap.Int("escapedTotal", event.Total),
		zap.Int("escapedByID", event.ByID),
		zap.Int("globalBudget", event.GlobalBudget),
		zap.Int("perIDBudget", event.PerIDBudget),
	}

	switch event.Type {
	case ants.PoolWithIDWorkerEscaped:
		logger.Zap.Error("stateful pool worker escaped", fields...)
	case ants.PoolWithIDEscapeBudgetExhausted:
		logger.Zap.Error("stateful pool escape budget exhausted",
			append(fields, zap.String("reason", poolEscapeReasonName(event.BudgetReason)))...)
	}
}

func (s *StatefulPool) reportMetrics(state *poolMonitorState) {
	if len(s.reporters) == 0 {
		return
	}
	managed := s.pool.Running()
	escaped := s.pool.Escaped()
	total := s.pool.TotalWorkers()
	poolTags := map[string]string{"pool": s.Name()}

	s.reportGauge(metrics.PoolWorkers, map[string]string{
		"pool":  s.Name(),
		"state": poolWorkerStateManaged,
	}, float64(managed))
	s.reportGauge(metrics.PoolWorkers, map[string]string{
		"pool":  s.Name(),
		"state": poolWorkerStateEscaped,
	}, float64(escaped))
	s.reportGauge(metrics.PoolWorkers, map[string]string{
		"pool":  s.Name(),
		"state": poolWorkerStateTotal,
	}, float64(total))
	s.reportGauge(metrics.PoolWaitingTasks, poolTags, float64(s.pool.Waiting()))
	s.reportGauge(metrics.PoolEscapeEventsDropped, poolTags, float64(s.pool.DroppedEscapeEvents()))
	s.reportGauge(metrics.PoolEscapeBudgetLimit, map[string]string{
		"pool":  s.Name(),
		"scope": poolEscapeScopeGlobal,
	}, float64(state.globalBudgetLimit))
	s.reportGauge(metrics.PoolEscapeBudgetLimit, map[string]string{
		"pool":  s.Name(),
		"scope": poolEscapeScopePerID,
	}, float64(state.perIDBudgetLimit))
	s.reportGauge(metrics.PoolEscapeBudgetExhausted, map[string]string{
		"pool":  s.Name(),
		"scope": poolEscapeScopeGlobal,
	}, float64(state.globalExhausted))
	s.reportGauge(metrics.PoolEscapeBudgetExhausted, map[string]string{
		"pool":  s.Name(),
		"scope": poolEscapeScopePerID,
	}, float64(state.perIDExhausted))
}

func (s *StatefulPool) reportEscapeEvent(event ants.PoolWithIDEscapeEvent) {
	if len(s.reporters) == 0 {
		return
	}
	tags := map[string]string{
		"pool":   s.Name(),
		"event":  poolEscapeEventName(event.Type),
		"reason": poolEscapeReasonName(event.BudgetReason),
	}
	for _, reporter := range s.reporters {
		if reporter == nil {
			s.logReporterError(metrics.PoolEscapeEvents, reporter, errors.New("nil metrics reporter"))
			continue
		}
		if err := reporter.ReportCount(metrics.PoolEscapeEvents, tags, 1); err != nil {
			s.logReporterError(metrics.PoolEscapeEvents, reporter, err)
		}
	}
}

func (s *StatefulPool) reportGauge(name string, tags map[string]string, value float64) {
	for _, reporter := range s.reporters {
		if reporter == nil {
			s.logReporterError(name, reporter, errors.New("nil metrics reporter"))
			continue
		}
		if err := reporter.ReportGauge(name, tags, value); err != nil {
			s.logReporterError(name, reporter, err)
		}
	}
}

func (s *StatefulPool) logReporterError(metric string, reporter metrics.Reporter, err error) {
	logger.Zap.Error("stateful pool metric reporter failed",
		zap.String("pool", s.Name()),
		zap.String("metric", metric),
		zap.String("reporter", fmt.Sprintf("%T", reporter)),
		zap.Error(err),
	)
}

func poolEscapeEventName(eventType ants.PoolWithIDEscapeEventType) string {
	switch eventType {
	case ants.PoolWithIDWorkerEscaped:
		return "worker_escaped"
	case ants.PoolWithIDEscapedWorkerExited:
		return "escaped_worker_exited"
	case ants.PoolWithIDEscapeBudgetExhausted:
		return "budget_exhausted"
	default:
		return "unknown"
	}
}

func poolEscapeReasonName(reason ants.PoolWithIDEscapeBudgetReason) string {
	switch reason {
	case 0:
		return "none"
	case ants.PoolWithIDEscapeGlobalBudgetExhausted:
		return poolEscapeScopeGlobal
	case ants.PoolWithIDEscapePerIDBudgetExhausted:
		return poolEscapeScopePerID
	case ants.PoolWithIDEscapeGlobalBudgetExhausted | ants.PoolWithIDEscapePerIDBudgetExhausted:
		return "global_and_per_id"
	default:
		return "unknown"
	}
}
