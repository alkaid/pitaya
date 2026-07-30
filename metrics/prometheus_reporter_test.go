package metrics

import (
	"errors"
	"maps"
	"testing"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/metrics/models"
	"github.com/prometheus/client_golang/prometheus"
)

var errPrometheusRegistration = errors.New("injected prometheus registration failure")

type failingPrometheusRegisterer struct {
	registry      *prometheus.Registry
	failAt        int
	registerCalls int
	unregisters   int
}

func (r *failingPrometheusRegisterer) Register(collector prometheus.Collector) error {
	r.registerCalls++
	if r.registerCalls == r.failAt {
		return errPrometheusRegistration
	}
	return r.registry.Register(collector)
}

func (r *failingPrometheusRegisterer) MustRegister(collectors ...prometheus.Collector) {
	r.registry.MustRegister(collectors...)
}

func (r *failingPrometheusRegisterer) Unregister(collector prometheus.Collector) bool {
	if r.registry.Unregister(collector) {
		r.unregisters++
		return true
	}
	return false
}

func TestPrometheusReporterRegistersAndGathersStatefulPoolMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	reporter, err := newPrometheusReporter("frontend", config.MetricsConfig{
		Game:             "cards",
		ConstLabels:      map[string]string{"environment": "test"},
		AdditionalLabels: map[string]string{"region": "local"},
	}, &models.CustomMetricsSpec{}, registry)
	if err != nil {
		t.Fatalf("create prometheus reporter: %v", err)
	}

	reports := []struct {
		name       string
		metricType string
		labels     map[string]string
		value      float64
		report     func() error
	}{
		{
			name:       PoolWorkers,
			metricType: "GAUGE",
			labels:     map[string]string{"pool": "default", "state": "managed"},
			value:      3,
			report: func() error {
				return reporter.ReportGauge(PoolWorkers, map[string]string{"pool": "default", "state": "managed"}, 3)
			},
		},
		{
			name:       PoolWaitingTasks,
			metricType: "GAUGE",
			labels:     map[string]string{"pool": "default"},
			value:      4,
			report: func() error {
				return reporter.ReportGauge(PoolWaitingTasks, map[string]string{"pool": "default"}, 4)
			},
		},
		{
			name:       PoolEscapeBudgetLimit,
			metricType: "GAUGE",
			labels:     map[string]string{"pool": "default", "scope": "global"},
			value:      128,
			report: func() error {
				return reporter.ReportGauge(PoolEscapeBudgetLimit, map[string]string{"pool": "default", "scope": "global"}, 128)
			},
		},
		{
			name:       PoolEscapeBudgetExhausted,
			metricType: "GAUGE",
			labels:     map[string]string{"pool": "default", "scope": "per_id"},
			value:      2,
			report: func() error {
				return reporter.ReportGauge(PoolEscapeBudgetExhausted, map[string]string{"pool": "default", "scope": "per_id"}, 2)
			},
		},
		{
			name:       PoolEscapeEventsDropped,
			metricType: "GAUGE",
			labels:     map[string]string{"pool": "default"},
			value:      7,
			report: func() error {
				return reporter.ReportGauge(PoolEscapeEventsDropped, map[string]string{"pool": "default"}, 7)
			},
		},
		{
			name:       PoolEscapeEvents,
			metricType: "COUNTER",
			labels: map[string]string{
				"pool": "default", "event": "worker_escaped", "reason": "none",
			},
			value: 5,
			report: func() error {
				return reporter.ReportCount(PoolEscapeEvents, map[string]string{
					"pool": "default", "event": "worker_escaped", "reason": "none",
				}, 5)
			},
		},
	}
	for _, report := range reports {
		if err := report.report(); err != nil {
			t.Fatalf("report %s: %v", report.name, err)
		}
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather prometheus metrics: %v", err)
	}
	baseLabels := map[string]string{
		"environment": "test",
		"game":        "cards",
		"region":      "local",
		"serverType":  "frontend",
	}
	for _, report := range reports {
		labels := maps.Clone(baseLabels)
		maps.Copy(labels, report.labels)
		name := "pitaya_gopool_" + report.name
		foundFamily := false
		foundMetric := false
		for _, family := range families {
			if family.GetName() != name {
				continue
			}
			foundFamily = true
			if got := family.GetType().String(); got != report.metricType {
				t.Fatalf("metric family %q type = %v, want %v", name, got, report.metricType)
			}
			for _, metric := range family.Metric {
				gotLabels := make(map[string]string, len(metric.Label))
				for _, pair := range metric.Label {
					gotLabels[pair.GetName()] = pair.GetValue()
				}
				if !maps.Equal(gotLabels, labels) {
					continue
				}
				var got float64
				switch report.metricType {
				case "COUNTER":
					got = metric.GetCounter().GetValue()
				case "GAUGE":
					got = metric.GetGauge().GetValue()
				default:
					t.Fatalf("unsupported metric type %q", report.metricType)
				}
				if got != report.value {
					t.Fatalf("metric %q%v = %v, want %v", name, labels, got, report.value)
				}
				foundMetric = true
				break
			}
			break
		}
		if !foundFamily {
			t.Fatalf("metric family %q not gathered", name)
		}
		if !foundMetric {
			t.Fatalf("metric %q with labels %v not gathered", name, labels)
		}
	}
}

func TestPrometheusReporterReturnsDuplicateRegistrationError(t *testing.T) {
	registry := prometheus.NewRegistry()
	cfg := config.MetricsConfig{
		Game:             "cards",
		ConstLabels:      map[string]string{},
		AdditionalLabels: map[string]string{},
	}
	spec := &models.CustomMetricsSpec{}
	if _, err := newPrometheusReporter("frontend", cfg, spec, registry); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	_, err := newPrometheusReporter("frontend", cfg, spec, registry)
	if err == nil {
		t.Fatal("duplicate registration unexpectedly succeeded")
	}
	var alreadyRegistered prometheus.AlreadyRegisteredError
	if !errors.As(err, &alreadyRegistered) {
		t.Fatalf("duplicate registration error = %T %v, want prometheus.AlreadyRegisteredError", err, err)
	}
}

func TestPrometheusReporterRollsBackPartialRegistration(t *testing.T) {
	registry := prometheus.NewRegistry()
	registerer := &failingPrometheusRegisterer{registry: registry, failAt: 2}
	cfg := config.MetricsConfig{
		Game:             "cards",
		ConstLabels:      map[string]string{},
		AdditionalLabels: map[string]string{},
	}
	spec := &models.CustomMetricsSpec{}
	if _, err := newPrometheusReporter("frontend", cfg, spec, registerer); !errors.Is(err, errPrometheusRegistration) {
		t.Fatalf("registration error = %v, want %v", err, errPrometheusRegistration)
	}
	if registerer.unregisters != 1 {
		t.Fatalf("unregistered collectors = %d, want 1", registerer.unregisters)
	}
	if _, err := newPrometheusReporter("frontend", cfg, spec, registry); err != nil {
		t.Fatalf("registration after rollback: %v", err)
	}
}
