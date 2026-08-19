package metrics

import (
	"errors"
	"maps"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/metrics/models"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
)

var errPrometheusRegistration = errors.New("injected prometheus registration failure")

type panicFormattingMetricValue struct{}

func (panicFormattingMetricValue) Error() string  { panic("metric panic Error called") }
func (panicFormattingMetricValue) String() string { panic("metric panic String called") }

func TestRecoverPrometheusReportDoesNotFormatRecoveredValue(t *testing.T) {
	var reportErr error
	require.NotPanics(t, func() {
		func() {
			defer recoverPrometheusReport("unsafe", prometheusCounter, &reportErr)
			panic(panicFormattingMetricValue{})
		}()
	})
	require.Error(t, reportErr)
	require.Contains(t, reportErr.Error(), "metrics.panicFormattingMetricValue")
}

type failingPrometheusRegisterer struct {
	registry      *prometheus.Registry
	failAt        int
	registerCalls int
	unregisters   int
}

type descriptorFailingPrometheusRegisterer struct {
	registry         *prometheus.Registry
	failMetricName   string
	failed           bool
	registerAttempts []prometheus.Collector
	registered       []prometheus.Collector
	unregistered     []prometheus.Collector
}

func (r *descriptorFailingPrometheusRegisterer) Register(collector prometheus.Collector) error {
	r.registerAttempts = append(r.registerAttempts, collector)
	if !r.failed && collectorDescribesMetric(collector, r.failMetricName) {
		r.failed = true
		return errPrometheusRegistration
	}
	if err := r.registry.Register(collector); err != nil {
		return err
	}
	r.registered = append(r.registered, collector)
	return nil
}

func (r *descriptorFailingPrometheusRegisterer) MustRegister(collectors ...prometheus.Collector) {
	r.registry.MustRegister(collectors...)
}

func (r *descriptorFailingPrometheusRegisterer) Unregister(collector prometheus.Collector) bool {
	if r.registry.Unregister(collector) {
		r.unregistered = append(r.unregistered, collector)
		return true
	}
	return false
}

func collectorDescribesMetric(collector prometheus.Collector, metricName string) bool {
	descriptions := make(chan *prometheus.Desc, 16)
	collector.Describe(descriptions)
	close(descriptions)

	want := `fqName: "` + metricName + `"`
	for description := range descriptions {
		if strings.Contains(description.String(), want) {
			return true
		}
	}
	return false
}

func assertNoPrometheusRegistrationAttempts(t *testing.T, registerer *descriptorFailingPrometheusRegisterer) {
	t.Helper()
	if got := len(registerer.registerAttempts); got != 0 {
		t.Fatalf("prometheus registration attempts = %d, want 0", got)
	}
}

func assertPrometheusRollbackIsExactReverse(t *testing.T, registerer *descriptorFailingPrometheusRegisterer) {
	t.Helper()
	if got, want := len(registerer.unregistered), len(registerer.registered); got != want {
		t.Fatalf("unregistered collectors = %d, want %d", got, want)
	}
	for i, collector := range registerer.unregistered {
		want := registerer.registered[len(registerer.registered)-1-i]
		if collector != want {
			t.Fatalf("unregistered collector %d does not match registered collector %d", i, len(registerer.registered)-1-i)
		}
	}
}

func collectorsDescribeMetric(collectors []prometheus.Collector, metricName string) bool {
	for _, collector := range collectors {
		if collectorDescribesMetric(collector, metricName) {
			return true
		}
	}
	return false
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

func TestPrometheusReporterCustomHistogramGatherPreservesResponseTimeSummary(t *testing.T) {
	registry := prometheus.NewRegistry()
	reporter, err := newPrometheusReporter("frontend", config.MetricsConfig{
		Game:             "cards",
		ConstLabels:      map[string]string{"environment": "test"},
		AdditionalLabels: map[string]string{"region": "local"},
	}, &models.CustomMetricsSpec{
		Histograms: []*models.Histogram{{
			Subsystem: "match",
			Name:      "users_rating_level",
			Help:      "the number of users in each rating level bucket",
			Buckets:   []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			Labels:    []string{"ground"},
		}},
	}, registry)
	if err != nil {
		t.Fatalf("create prometheus reporter: %v", err)
	}

	for _, value := range []float64{3, 7} {
		if err := reporter.ReportHistogram(
			"users_rating_level",
			map[string]string{"ground": "101"},
			value,
		); err != nil {
			t.Fatalf("report custom histogram: %v", err)
		}
	}
	if err := reporter.ReportSummary(ResponseTime, map[string]string{
		"route": "match.join", "status": "ok", "type": "request", "code": "0",
	}, 123); err != nil {
		t.Fatalf("report response-time summary: %v", err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather prometheus metrics: %v", err)
	}

	var foundHistogram, foundResponseTime bool
	for _, family := range families {
		switch family.GetName() {
		case "pitaya_match_users_rating_level":
			foundHistogram = true
			if got := family.GetType().String(); got != "HISTOGRAM" {
				t.Fatalf("custom metric type = %q, want HISTOGRAM", got)
			}
			if len(family.Metric) != 1 {
				t.Fatalf("custom histogram series = %d, want 1", len(family.Metric))
			}
			metric := family.Metric[0]
			gotLabels := make(map[string]string, len(metric.Label))
			for _, pair := range metric.Label {
				gotLabels[pair.GetName()] = pair.GetValue()
			}
			wantLabels := map[string]string{
				"environment": "test",
				"game":        "cards",
				"ground":      "101",
				"region":      "local",
				"serverType":  "frontend",
			}
			if !maps.Equal(gotLabels, wantLabels) {
				t.Fatalf("custom histogram labels = %v, want %v", gotLabels, wantLabels)
			}
			histogram := metric.GetHistogram()
			if histogram == nil {
				t.Fatal("custom histogram payload is nil")
			}
			if got := histogram.GetSampleCount(); got != 2 {
				t.Fatalf("custom histogram count = %d, want 2", got)
			}
			if got := histogram.GetSampleSum(); got != 10 {
				t.Fatalf("custom histogram sum = %v, want 10", got)
			}
			wantBucketCounts := []uint64{0, 0, 1, 1, 1, 1, 2, 2, 2, 2}
			if len(histogram.Bucket) != len(wantBucketCounts) {
				t.Fatalf("custom histogram buckets = %d, want %d", len(histogram.Bucket), len(wantBucketCounts))
			}
			for i, bucket := range histogram.Bucket {
				if got, want := bucket.GetUpperBound(), float64(i+1); got != want {
					t.Fatalf("custom histogram bucket %d upper bound = %v, want %v", i, got, want)
				}
				if got, want := bucket.GetCumulativeCount(), wantBucketCounts[i]; got != want {
					t.Fatalf("custom histogram bucket %d count = %d, want %d", i, got, want)
				}
			}
		case "pitaya_handler_response_time_ns":
			foundResponseTime = true
			if got := family.GetType().String(); got != "SUMMARY" {
				t.Fatalf("response-time metric type = %q, want SUMMARY", got)
			}
			if len(family.Metric) != 1 {
				t.Fatalf("response-time series = %d, want 1", len(family.Metric))
			}
			summary := family.Metric[0].GetSummary()
			if summary == nil {
				t.Fatal("response-time summary payload is nil")
			}
			if got := summary.GetSampleCount(); got != 1 {
				t.Fatalf("response-time count = %d, want 1", got)
			}
			if got := summary.GetSampleSum(); got != 123 {
				t.Fatalf("response-time sum = %v, want 123", got)
			}
			if got := len(summary.Quantile); got != 3 {
				t.Fatalf("response-time quantiles = %d, want 3", got)
			}
			if family.Metric[0].GetHistogram() != nil {
				t.Fatal("response-time summary unexpectedly contains histogram buckets")
			}
		}
	}
	if !foundHistogram {
		t.Fatal("custom histogram metric family was not gathered")
	}
	if !foundResponseTime {
		t.Fatal("response-time summary metric family was not gathered")
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/metrics", nil)
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("prometheus scrape status = %d, want 200", recorder.Code)
	}
	scrape := recorder.Body.String()
	if strings.Contains(scrape, "pitaya_handler_response_time_ns_bucket") {
		t.Fatal("response-time summary scrape unexpectedly exposes histogram buckets")
	}
	if !strings.Contains(scrape, "pitaya_handler_response_time_ns{") ||
		!strings.Contains(scrape, `quantile="0.95"`) {
		t.Fatal("response-time summary scrape is missing its quantile series")
	}
}

func TestPrometheusReporterCustomSummaryAndHistogramWithDifferentFQNamesCoexist(t *testing.T) {
	registry := prometheus.NewRegistry()
	reporter, err := newPrometheusReporter("frontend", emptyMetricsConfig(), &models.CustomMetricsSpec{
		Summaries: []*models.Summary{{
			Subsystem: "summary",
			Name:      "shared_name",
			Help:      "custom summary",
		}},
		Histograms: []*models.Histogram{{
			Subsystem: "histogram",
			Name:      "shared_name",
			Help:      "custom histogram",
			Buckets:   []float64{1, 2},
		}},
	}, registry)
	if err != nil {
		t.Fatalf("create prometheus reporter: %v", err)
	}
	if err := reporter.ReportSummary("shared_name", map[string]string{}, 1); err != nil {
		t.Fatalf("report custom summary: %v", err)
	}
	if err := reporter.ReportHistogram("shared_name", map[string]string{}, 2); err != nil {
		t.Fatalf("report custom histogram: %v", err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather prometheus metrics: %v", err)
	}
	wantTypes := map[string]string{
		"pitaya_summary_shared_name":   "SUMMARY",
		"pitaya_histogram_shared_name": "HISTOGRAM",
	}
	for _, family := range families {
		wantType, ok := wantTypes[family.GetName()]
		if !ok {
			continue
		}
		if got := family.GetType().String(); got != wantType {
			t.Fatalf("metric family %q type = %q, want %q", family.GetName(), got, wantType)
		}
		delete(wantTypes, family.GetName())
	}
	if len(wantTypes) != 0 {
		t.Fatalf("metric families not gathered: %v", wantTypes)
	}
}

func TestPrometheusReporterRejectsDuplicateCustomHistogramRawName(t *testing.T) {
	registry := prometheus.NewRegistry()
	registerer := &descriptorFailingPrometheusRegisterer{registry: registry}
	_, err := newPrometheusReporter("frontend", emptyMetricsConfig(), &models.CustomMetricsSpec{
		Histograms: []*models.Histogram{
			{Subsystem: "alpha", Name: "duplicate", Help: "alpha", Buckets: []float64{1}},
			{Subsystem: "beta", Name: "duplicate", Help: "beta", Buckets: []float64{2}},
		},
	}, registerer)
	if err == nil {
		t.Fatal("duplicate custom histogram raw name unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), `duplicate custom histogram name "duplicate"`) {
		t.Fatalf("duplicate custom histogram error = %v", err)
	}
	assertNoPrometheusRegistrationAttempts(t, registerer)
	assertRegistryEmpty(t, registry)
}

func TestPrometheusReporterRejectsCustomHistogramConflictingWithResponseTimeSummary(t *testing.T) {
	registry := prometheus.NewRegistry()
	registerer := &descriptorFailingPrometheusRegisterer{registry: registry}
	_, err := newPrometheusReporter("frontend", emptyMetricsConfig(), &models.CustomMetricsSpec{
		Histograms: []*models.Histogram{{
			Subsystem: "handler",
			Name:      ResponseTime,
			Help:      "the time to process a msg in nanoseconds",
			Buckets:   []float64{1, 5, 10},
			Labels:    []string{"route", "status", "type", "code"},
		}},
	}, registerer)
	if err == nil {
		t.Fatal("custom histogram conflicting with response-time summary unexpectedly succeeded")
	}
	assertNoPrometheusRegistrationAttempts(t, registerer)
	assertRegistryEmpty(t, registry)
}

func TestPrometheusReporterRejectsCustomSummaryAndHistogramWithSameFQName(t *testing.T) {
	registry := prometheus.NewRegistry()
	registerer := &descriptorFailingPrometheusRegisterer{registry: registry}
	_, err := newPrometheusReporter("frontend", emptyMetricsConfig(), &models.CustomMetricsSpec{
		Summaries: []*models.Summary{{
			Subsystem: "match",
			Name:      "shared_name",
			Help:      "shared metric",
		}},
		Histograms: []*models.Histogram{{
			Subsystem: "match",
			Name:      "shared_name",
			Help:      "shared metric",
			Buckets:   []float64{1, 2},
		}},
	}, registerer)
	if err == nil {
		t.Fatal("custom summary and histogram with the same FQName unexpectedly succeeded")
	}
	assertNoPrometheusRegistrationAttempts(t, registerer)
	assertRegistryEmpty(t, registry)
}

func TestPrometheusReporterRejectsCustomHistogramMagicSuffixCollisions(t *testing.T) {
	for _, suffix := range []string{"_bucket", "_count", "_sum"} {
		t.Run(suffix, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			registerer := &descriptorFailingPrometheusRegisterer{registry: registry}
			_, err := newPrometheusReporter("frontend", emptyMetricsConfig(), &models.CustomMetricsSpec{
				Histograms: []*models.Histogram{{
					Subsystem: "match",
					Name:      "latency",
					Help:      "match latency",
					Buckets:   []float64{1, 2},
				}},
				Gauges: []*models.Gauge{{
					Subsystem: "match",
					Name:      "latency" + suffix,
					Help:      "conflicting metric",
				}},
			}, registerer)
			if err == nil {
				t.Fatalf("custom histogram magic suffix collision %q unexpectedly succeeded", suffix)
			}
			assertNoPrometheusRegistrationAttempts(t, registerer)
			assertRegistryEmpty(t, registry)
		})
	}
}

func TestPrometheusReporterRejectsCustomSummaryMagicSuffixCollisions(t *testing.T) {
	for _, suffix := range []string{"_count", "_sum"} {
		t.Run(suffix, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			registerer := &descriptorFailingPrometheusRegisterer{registry: registry}
			_, err := newPrometheusReporter("frontend", emptyMetricsConfig(), &models.CustomMetricsSpec{
				Summaries: []*models.Summary{{
					Subsystem: "match",
					Name:      "latency",
					Help:      "match latency",
				}},
				Gauges: []*models.Gauge{{
					Subsystem: "match",
					Name:      "latency" + suffix,
					Help:      "conflicting metric",
				}},
			}, registerer)
			if err == nil {
				t.Fatalf("custom summary magic suffix collision %q unexpectedly succeeded", suffix)
			}
			assertNoPrometheusRegistrationAttempts(t, registerer)
			assertRegistryEmpty(t, registry)
		})
	}
}

func TestPrometheusReporterAllowsSummaryBucketSuffixMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	reporter, err := newPrometheusReporter("frontend", emptyMetricsConfig(), &models.CustomMetricsSpec{
		Summaries: []*models.Summary{{
			Subsystem: "match",
			Name:      "latency",
			Help:      "match latency",
		}},
		Gauges: []*models.Gauge{{
			Subsystem: "match",
			Name:      "latency_bucket",
			Help:      "standalone bucket metric",
		}},
	}, registry)
	if err != nil {
		t.Fatalf("create reporter with allowed summary bucket suffix metric: %v", err)
	}
	if err := reporter.ReportSummary("latency", map[string]string{}, 1); err != nil {
		t.Fatalf("report summary: %v", err)
	}
	if err := reporter.ReportGauge("latency_bucket", map[string]string{}, 2); err != nil {
		t.Fatalf("report bucket-suffixed gauge: %v", err)
	}
	if _, err := registry.Gather(); err != nil {
		t.Fatalf("gather allowed summary bucket suffix metric: %v", err)
	}
}

func TestPrometheusReporterRejectsDifferentRawNameWithResponseTimeFQName(t *testing.T) {
	registry := prometheus.NewRegistry()
	registerer := &descriptorFailingPrometheusRegisterer{registry: registry}
	_, err := newPrometheusReporter("frontend", emptyMetricsConfig(), &models.CustomMetricsSpec{
		Histograms: []*models.Histogram{{
			Subsystem: "handler_response",
			Name:      "time_ns",
			Help:      "the time to process a msg in nanoseconds",
			Buckets:   []float64{1, 5, 10},
			Labels:    []string{"route", "status", "type", "code"},
		}},
	}, registerer)
	if err == nil {
		t.Fatal("custom histogram with response-time FQName unexpectedly succeeded")
	}
	assertNoPrometheusRegistrationAttempts(t, registerer)
	assertRegistryEmpty(t, registry)
}

func TestPrometheusReporterPreservesPreRegisteredCollectorOnCustomHistogramConflict(t *testing.T) {
	registry := prometheus.NewRegistry()
	cfg := config.MetricsConfig{
		Game:             "cards",
		ConstLabels:      map[string]string{"environment": "test"},
		AdditionalLabels: map[string]string{"region": "local"},
	}
	spec := &models.CustomMetricsSpec{
		Histograms: []*models.Histogram{{
			Subsystem: "match",
			Name:      "users_rating_level",
			Help:      "existing histogram",
			Buckets:   []float64{1, 2},
			Labels:    []string{"ground"},
		}},
	}
	existing := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "pitaya",
		Subsystem: "match",
		Name:      "users_rating_level",
		Help:      "existing histogram",
		ConstLabels: prometheus.Labels{
			"environment": "test",
			"game":        "cards",
			"serverType":  "frontend",
		},
		Buckets: []float64{1, 2},
	}, []string{"region", "ground"})
	existing.WithLabelValues("local", "101").Observe(2)
	registry.MustRegister(existing)

	_, err := newPrometheusReporter("frontend", cfg, spec, registry)
	if err == nil {
		t.Fatal("registration conflicting with pre-registered histogram unexpectedly succeeded")
	}

	families, gatherErr := registry.Gather()
	if gatherErr != nil {
		t.Fatalf("gather registry after failed registration: %v", gatherErr)
	}
	foundExisting := false
	for _, family := range families {
		if family.GetName() != "pitaya_match_users_rating_level" {
			continue
		}
		foundExisting = true
		if len(family.Metric) != 1 || family.Metric[0].GetHistogram().GetSampleCount() != 1 {
			t.Fatalf("pre-registered histogram changed after rollback: %v", family)
		}
	}
	if !foundExisting {
		t.Fatal("pre-registered histogram was removed by rollback")
	}
	if !registry.Unregister(existing) {
		t.Fatal("pre-registered histogram was not preserved for explicit unregister")
	}
	if _, err := newPrometheusReporter("frontend", cfg, spec, registry); err != nil {
		t.Fatalf("registration in same registry after removing existing collector: %v", err)
	}
}

func TestPrometheusReporterRollsBackHistogramRegistrationByIdentityAndRetries(t *testing.T) {
	spec := &models.CustomMetricsSpec{
		Histograms: []*models.Histogram{
			{
				Subsystem: "match",
				Name:      "a_first_histogram",
				Help:      "first histogram",
				Buckets:   []float64{1, 2},
			},
			{
				Subsystem: "match",
				Name:      "b_second_histogram",
				Help:      "second histogram",
				Buckets:   []float64{10, 20},
			},
		},
	}
	registry := prometheus.NewRegistry()
	registerer := &descriptorFailingPrometheusRegisterer{
		registry:       registry,
		failMetricName: "pitaya_match_b_second_histogram",
	}
	if _, err := newPrometheusReporter("frontend", emptyMetricsConfig(), spec, registerer); !errors.Is(err, errPrometheusRegistration) {
		t.Fatalf("registration error = %v, want %v", err, errPrometheusRegistration)
	}
	if !registerer.failed {
		t.Fatal("second histogram did not reach the registration plan")
	}
	if !collectorsDescribeMetric(registerer.registered, "pitaya_match_a_first_histogram") {
		t.Fatal("first histogram was not registered before second histogram failed")
	}
	if !collectorsDescribeMetric(registerer.unregistered, "pitaya_match_a_first_histogram") {
		t.Fatal("first histogram was not rolled back after second histogram failed")
	}
	assertPrometheusRollbackIsExactReverse(t, registerer)
	assertRegistryEmpty(t, registry)
	if _, err := newPrometheusReporter("frontend", emptyMetricsConfig(), spec, registry); err != nil {
		t.Fatalf("registration after rollback: %v", err)
	}
}

func TestPrometheusReporterReturnsUnknownForUnconfiguredResponseTimeHistogram(t *testing.T) {
	reporter, err := newPrometheusReporter(
		"frontend",
		emptyMetricsConfig(),
		&models.CustomMetricsSpec{},
		prometheus.NewRegistry(),
	)
	if err != nil {
		t.Fatalf("create prometheus reporter: %v", err)
	}
	err = reporter.ReportHistogram(ResponseTime, map[string]string{
		"route": "match.join", "status": "ok", "type": "request", "code": "0",
	}, 123)
	if !errors.Is(err, constants.ErrMetricNotKnown) {
		t.Fatalf("response-time histogram error = %v, want %v", err, constants.ErrMetricNotKnown)
	}
}

func TestPrometheusReporterReturnsUnknownForEveryMetricKind(t *testing.T) {
	reporter := &PrometheusReporter{
		countReportersMap:     map[string]*prometheus.CounterVec{},
		summaryReportersMap:   map[string]*prometheus.SummaryVec{},
		histogramReportersMap: map[string]*prometheus.HistogramVec{},
		gaugeReportersMap:     map[string]*prometheus.GaugeVec{},
	}
	tests := []struct {
		name   string
		report func() error
	}{
		{name: "counter", report: func() error { return reporter.ReportCount("unknown", nil, 1) }},
		{name: "summary", report: func() error { return reporter.ReportSummary("unknown", nil, 1) }},
		{name: "histogram", report: func() error { return reporter.ReportHistogram("unknown", nil, 1) }},
		{name: "gauge", report: func() error { return reporter.ReportGauge("unknown", nil, 1) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.report(); !errors.Is(err, constants.ErrMetricNotKnown) {
				t.Fatalf("report error = %v, want %v", err, constants.ErrMetricNotKnown)
			}
		})
	}
}

func TestPrometheusReporterRejectsUnexpectedLabelsWithoutPanicking(t *testing.T) {
	reporter, err := newPrometheusReporter(
		"frontend",
		emptyMetricsConfig(),
		&models.CustomMetricsSpec{Histograms: []*models.Histogram{{
			Subsystem: "custom",
			Name:      "latency",
			Help:      "custom latency",
			Labels:    []string{"kind"},
		}}},
		prometheus.NewRegistry(),
	)
	if err != nil {
		t.Fatalf("create prometheus reporter: %v", err)
	}

	tests := []struct {
		name   string
		report func() error
	}{
		{
			name: "summary",
			report: func() error {
				return reporter.ReportSummary(ResponseTime, map[string]string{
					"route": "match.join", "status": "ok", "type": "request", "code": "0", "typo": "value",
				}, 1)
			},
		},
		{
			name: "histogram",
			report: func() error {
				return reporter.ReportHistogram("latency", map[string]string{"kind": "rpc", "typo": "value"}, 1)
			},
		},
		{
			name: "counter",
			report: func() error {
				return reporter.ReportCount(PoolGoDeadlines, map[string]string{
					"route": "match.join", "pool": "default", "second": "1", "typo": "value",
				}, 1)
			},
		},
		{
			name: "gauge",
			report: func() error {
				return reporter.ReportGauge(ConnectedClients, map[string]string{"typo": "value"}, 1)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var reportErr error
			if func() (panicked bool) {
				defer func() { panicked = recover() != nil }()
				reportErr = test.report()
				return false
			}() {
				t.Fatal("reporting unexpected labels panicked")
			}
			if reportErr == nil {
				t.Fatal("reporting unexpected labels returned nil error")
			}
		})
	}
}

func TestPrometheusReporterAddsDefaultLabelsWithoutMutatingInput(t *testing.T) {
	cfg := emptyMetricsConfig()
	cfg.AdditionalLabels = map[string]string{"region": "unknown"}
	reporter, err := newPrometheusReporter("frontend", cfg, &models.CustomMetricsSpec{}, prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("create prometheus reporter: %v", err)
	}

	labels := map[string]string{}
	if err := reporter.ReportGauge(ConnectedClients, labels, 1); err != nil {
		t.Fatalf("report gauge with default label: %v", err)
	}
	if len(labels) != 0 {
		t.Fatalf("reporting mutated caller labels: %#v", labels)
	}
}

func TestPrometheusReporterReturnsReservedLabelConfigurationErrors(t *testing.T) {
	tests := []struct {
		name string
		spec *models.CustomMetricsSpec
	}{
		{
			name: "summary quantile label",
			spec: &models.CustomMetricsSpec{Summaries: []*models.Summary{{
				Subsystem: "custom", Name: "latency", Help: "latency", Labels: []string{"quantile"},
			}}},
		},
		{
			name: "additional quantile label",
			spec: &models.CustomMetricsSpec{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := emptyMetricsConfig()
			if test.name == "additional quantile label" {
				cfg.AdditionalLabels = map[string]string{"quantile": "unknown"}
			}
			var reporter *PrometheusReporter
			var err error
			panicked := false
			func() {
				defer func() { panicked = recover() != nil }()
				reporter, err = newPrometheusReporter("frontend", cfg, test.spec, prometheus.NewRegistry())
			}()
			if panicked {
				t.Fatal("reserved prometheus label panicked")
			}
			if err == nil || reporter != nil {
				t.Fatalf("reporter, error = %v, %v; want nil, non-nil", reporter, err)
			}
		})
	}
}

func TestPrometheusReporterConvertsLazyHistogramPanicsToErrors(t *testing.T) {
	tests := []struct {
		name      string
		labels    []string
		buckets   []float64
		reportMap map[string]string
	}{
		{name: "reserved le label", labels: []string{"le"}, reportMap: map[string]string{"le": "1"}},
		{name: "unordered buckets", buckets: []float64{2, 1}, reportMap: map[string]string{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reporter, err := newPrometheusReporter(
				"frontend",
				emptyMetricsConfig(),
				&models.CustomMetricsSpec{Histograms: []*models.Histogram{{
					Subsystem: "custom",
					Name:      "latency",
					Help:      "latency",
					Labels:    test.labels,
					Buckets:   test.buckets,
				}}},
				prometheus.NewRegistry(),
			)
			if err != nil {
				t.Fatalf("create prometheus reporter: %v", err)
			}

			var reportErr error
			panicked := false
			func() {
				defer func() { panicked = recover() != nil }()
				reportErr = reporter.ReportHistogram("latency", test.reportMap, 1)
			}()
			if panicked {
				t.Fatal("invalid histogram configuration panicked while reporting")
			}
			if reportErr == nil {
				t.Fatal("invalid histogram configuration returned nil error")
			}
		})
	}
}

func emptyMetricsConfig() config.MetricsConfig {
	return config.MetricsConfig{
		Game:             "cards",
		ConstLabels:      map[string]string{},
		AdditionalLabels: map[string]string{},
	}
}

func assertRegistryEmpty(t *testing.T, registry *prometheus.Registry) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather registry: %v", err)
	}
	if len(families) != 0 {
		t.Fatalf("registry contains %d metric families after rollback", len(families))
	}
}
