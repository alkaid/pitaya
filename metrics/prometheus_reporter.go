// Copyright (c) TFG Co. All Rights Reserved.
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

package metrics

import (
	"errors"
	"fmt"
	"sort"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/metrics/models"
	"github.com/prometheus/client_golang/prometheus"
)

const prometheusNamespace = "pitaya"

type prometheusMetricKind string

const (
	prometheusCounter   prometheusMetricKind = "counter"
	prometheusGauge     prometheusMetricKind = "gauge"
	prometheusSummary   prometheusMetricKind = "summary"
	prometheusHistogram prometheusMetricKind = "histogram"
)

type prometheusCollectorRegistration struct {
	collector prometheus.Collector
	fqName    string
	kind      prometheusMetricKind
}

type prometheusCollectorSet struct {
	reporter   *PrometheusReporter
	counters   map[string]prometheusCollectorRegistration
	gauges     map[string]prometheusCollectorRegistration
	summaries  map[string]prometheusCollectorRegistration
	histograms map[string]prometheusCollectorRegistration
}

func newPrometheusCollectorSet(reporter *PrometheusReporter) *prometheusCollectorSet {
	return &prometheusCollectorSet{
		reporter:   reporter,
		counters:   make(map[string]prometheusCollectorRegistration),
		gauges:     make(map[string]prometheusCollectorRegistration),
		summaries:  make(map[string]prometheusCollectorRegistration),
		histograms: make(map[string]prometheusCollectorRegistration),
	}
}

func (s *prometheusCollectorSet) addCounter(subsystem, name string, collector *prometheus.CounterVec) {
	s.reporter.countReportersMap[name] = collector
	s.counters[name] = prometheusCollectorRegistration{
		collector: collector,
		fqName:    prometheus.BuildFQName(prometheusNamespace, subsystem, name),
		kind:      prometheusCounter,
	}
}

func (s *prometheusCollectorSet) addGauge(subsystem, name string, collector *prometheus.GaugeVec) {
	s.reporter.gaugeReportersMap[name] = collector
	s.gauges[name] = prometheusCollectorRegistration{
		collector: collector,
		fqName:    prometheus.BuildFQName(prometheusNamespace, subsystem, name),
		kind:      prometheusGauge,
	}
}

func (s *prometheusCollectorSet) addSummary(subsystem, name string, collector *prometheus.SummaryVec) {
	s.reporter.summaryReportersMap[name] = collector
	s.summaries[name] = prometheusCollectorRegistration{
		collector: collector,
		fqName:    prometheus.BuildFQName(prometheusNamespace, subsystem, name),
		kind:      prometheusSummary,
	}
}

func (s *prometheusCollectorSet) addHistogram(subsystem, name string, collector *prometheus.HistogramVec) {
	s.reporter.histogramReportersMap[name] = collector
	s.histograms[name] = prometheusCollectorRegistration{
		collector: collector,
		fqName:    prometheus.BuildFQName(prometheusNamespace, subsystem, name),
		kind:      prometheusHistogram,
	}
}

func (s *prometheusCollectorSet) registrations() []prometheusCollectorRegistration {
	registrations := make([]prometheusCollectorRegistration, 0,
		len(s.counters)+len(s.gauges)+len(s.summaries)+len(s.histograms))
	for _, collectors := range []map[string]prometheusCollectorRegistration{
		s.counters,
		s.gauges,
		s.summaries,
		s.histograms,
	} {
		names := make([]string, 0, len(collectors))
		for name := range collectors {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			registrations = append(registrations, collectors[name])
		}
	}
	return registrations
}

func validatePrometheusMetricNames(registrations []prometheusCollectorRegistration) error {
	owners := make(map[string]prometheusCollectorRegistration)
	for _, registration := range registrations {
		for _, expositionName := range registration.expositionNames() {
			if existing, exists := owners[expositionName]; exists {
				return fmt.Errorf(
					"prometheus metric name collision on %q between %s %q and %s %q",
					expositionName,
					existing.kind,
					existing.fqName,
					registration.kind,
					registration.fqName,
				)
			}
			owners[expositionName] = registration
		}
	}
	return nil
}

func (r prometheusCollectorRegistration) expositionNames() []string {
	names := []string{r.fqName}
	switch r.kind {
	case prometheusSummary:
		return append(names, r.fqName+"_count", r.fqName+"_sum")
	case prometheusHistogram:
		return append(names, r.fqName+"_count", r.fqName+"_sum", r.fqName+"_bucket")
	default:
		return names
	}
}

// PrometheusReporter reports metrics to prometheus
type PrometheusReporter struct {
	serverType            string
	game                  string
	countReportersMap     map[string]*prometheus.CounterVec
	summaryReportersMap   map[string]*prometheus.SummaryVec
	histogramReportersMap map[string]*prometheus.HistogramVec
	gaugeReportersMap     map[string]*prometheus.GaugeVec
	additionalLabels      map[string]string
}

func (p *PrometheusReporter) registerCustomMetrics(
	constLabels map[string]string,
	additionalLabelsKeys []string,
	spec *models.CustomMetricsSpec,
	collectorSet *prometheusCollectorSet,
) error {
	histogramNames := make(map[string]struct{}, len(spec.Histograms))
	for _, histogram := range spec.Histograms {
		if _, exists := histogramNames[histogram.Name]; exists {
			return fmt.Errorf("duplicate custom histogram name %q", histogram.Name)
		}
		histogramNames[histogram.Name] = struct{}{}
	}

	for _, summary := range spec.Summaries {
		collectorSet.addSummary(summary.Subsystem, summary.Name, prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Namespace:   prometheusNamespace,
				Subsystem:   summary.Subsystem,
				Name:        summary.Name,
				Help:        summary.Help,
				Objectives:  summary.Objectives,
				ConstLabels: constLabels,
			},
			append(additionalLabelsKeys, summary.Labels...),
		))
	}

	for _, histogram := range spec.Histograms {
		collectorSet.addHistogram(histogram.Subsystem, histogram.Name, prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace:   prometheusNamespace,
				Subsystem:   histogram.Subsystem,
				Name:        histogram.Name,
				Help:        histogram.Help,
				Buckets:     histogram.Buckets,
				ConstLabels: constLabels,
			},
			append(additionalLabelsKeys, histogram.Labels...),
		))
	}

	for _, gauge := range spec.Gauges {
		collectorSet.addGauge(gauge.Subsystem, gauge.Name, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace:   prometheusNamespace,
				Subsystem:   gauge.Subsystem,
				Name:        gauge.Name,
				Help:        gauge.Help,
				ConstLabels: constLabels,
			},
			append(additionalLabelsKeys, gauge.Labels...),
		))
	}

	for _, counter := range spec.Counters {
		collectorSet.addCounter(counter.Subsystem, counter.Name, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace:   prometheusNamespace,
				Subsystem:   counter.Subsystem,
				Name:        counter.Name,
				Help:        counter.Help,
				ConstLabels: constLabels,
			},
			append(additionalLabelsKeys, counter.Labels...),
		))
	}

	return nil
}

func (p *PrometheusReporter) registerMetrics(
	constLabels, additionalLabels map[string]string,
	spec *models.CustomMetricsSpec,
	registerer prometheus.Registerer,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = recoveredPrometheusError("configure prometheus metrics", recovered)
		}
	}()
	if spec == nil {
		return errors.New("custom prometheus metrics spec must not be nil")
	}
	constLabels = cloneLabelMap(constLabels)
	additionalLabels = cloneLabelMap(additionalLabels)

	constLabels["game"] = p.game
	constLabels["serverType"] = p.serverType

	p.additionalLabels = additionalLabels
	collectorSet := newPrometheusCollectorSet(p)
	additionalLabelsKeys := make([]string, 0, len(additionalLabels))
	for key := range additionalLabels {
		additionalLabelsKeys = append(additionalLabelsKeys, key)
	}
	sort.Strings(additionalLabelsKeys)

	if err := p.registerCustomMetrics(constLabels, additionalLabelsKeys, spec, collectorSet); err != nil {
		return err
	}

	// HandlerResponseTimeMs summary
	collectorSet.addSummary("handler", ResponseTime, prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "handler",
			Name:        ResponseTime,
			Help:        "the time to process a msg in nanoseconds",
			Objectives:  map[float64]float64{0.7: 0.02, 0.95: 0.005, 0.99: 0.001},
			ConstLabels: constLabels,
		},
		append([]string{"route", "status", "type", "code"}, additionalLabelsKeys...),
	))

	collectorSet.addCounter("gopool", PoolGoDeadlines, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "gopool",
			Name:        PoolGoDeadlines,
			Help:        "the number of goroutines by exceeded timeout",
			ConstLabels: constLabels,
		},
		append([]string{"route", "pool", "second"}, additionalLabelsKeys...),
	))

	collectorSet.addGauge("gopool", PoolWorkers, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "gopool",
			Name:        PoolWorkers,
			Help:        "the number of managed, escaped, and total workers in a stateful pool",
			ConstLabels: constLabels,
		},
		append([]string{"pool", "state"}, additionalLabelsKeys...),
	))

	collectorSet.addGauge("gopool", PoolWaitingTasks, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "gopool",
			Name:        PoolWaitingTasks,
			Help:        "the number of Submit calls currently blocked by a stateful pool",
			ConstLabels: constLabels,
		},
		append([]string{"pool"}, additionalLabelsKeys...),
	))

	collectorSet.addGauge("gopool", PoolEscapeBudgetLimit, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "gopool",
			Name:        PoolEscapeBudgetLimit,
			Help:        "the configured global and per-ID escape-worker limits",
			ConstLabels: constLabels,
		},
		append([]string{"pool", "scope"}, additionalLabelsKeys...),
	))

	collectorSet.addGauge("gopool", PoolEscapeBudgetExhausted, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "gopool",
			Name:        PoolEscapeBudgetExhausted,
			Help:        "the number of IDs whose replacement is blocked by an escape budget",
			ConstLabels: constLabels,
		},
		append([]string{"pool", "scope"}, additionalLabelsKeys...),
	))

	collectorSet.addGauge("gopool", PoolEscapeEventsDropped, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "gopool",
			Name:        PoolEscapeEventsDropped,
			Help:        "the lifetime number of best-effort escape events dropped by a stateful pool",
			ConstLabels: constLabels,
		},
		append([]string{"pool"}, additionalLabelsKeys...),
	))

	collectorSet.addCounter("gopool", PoolEscapeEvents, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "gopool",
			Name:        PoolEscapeEvents,
			Help:        "the number of stateful-pool escape transitions",
			ConstLabels: constLabels,
		},
		append([]string{"pool", "event", "reason"}, additionalLabelsKeys...),
	))

	// ProcessDelay summary
	collectorSet.addSummary("handler", ProcessDelay, prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "handler",
			Name:        ProcessDelay,
			Help:        "the delay to start processing a msg in nanoseconds",
			Objectives:  map[float64]float64{0.7: 0.02, 0.95: 0.005, 0.99: 0.001},
			ConstLabels: constLabels,
		},
		append([]string{"route", "type"}, additionalLabelsKeys...),
	))

	// ConnectedClients gauge
	collectorSet.addGauge("acceptor", ConnectedClients, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "acceptor",
			Name:        ConnectedClients,
			Help:        "the number of clients connected right now",
			ConstLabels: constLabels,
		},
		additionalLabelsKeys,
	))

	collectorSet.addGauge("service_discovery", CountServers, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "service_discovery",
			Name:        CountServers,
			Help:        "the number of discovered servers by service discovery",
			ConstLabels: constLabels,
		},
		append([]string{"type"}, additionalLabelsKeys...),
	))

	collectorSet.addGauge("channel", ChannelCapacity, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "channel",
			Name:        ChannelCapacity,
			Help:        "the available capacity of the channel",
			ConstLabels: constLabels,
		},
		append([]string{"channel"}, additionalLabelsKeys...),
	))

	collectorSet.addGauge("rpc_server", DroppedMessages, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "rpc_server",
			Name:        DroppedMessages,
			Help:        "the number of rpc server dropped messages (messages that are not handled)",
			ConstLabels: constLabels,
		},
		additionalLabelsKeys,
	))

	for _, metric := range []struct {
		name string
		help string
	}{
		{name: ActiveUserSubscriptionPairs, help: "the number of active NATS user subscription pairs"},
		{name: UserSubscriptionStaleOwnerDrops, help: "the number of NATS user deliveries rejected for stale session ownership"},
		{name: UserSubscriptionCreateRollbacks, help: "the number of partially-created NATS user subscription pairs rolled back"},
		{name: UserSubscriptionDuplicateBindReuse, help: "the number of duplicate session binds that reused a NATS user subscription pair"},
		{name: UserMessageUIDMismatches, help: "the number of NATS user messages rejected for subject and payload UID mismatch"},
	} {
		collectorSet.addGauge("rpc_server", metric.name, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace:   prometheusNamespace,
				Subsystem:   "rpc_server",
				Name:        metric.name,
				Help:        metric.help,
				ConstLabels: constLabels,
			},
			additionalLabelsKeys,
		))
	}

	collectorSet.addGauge("sys", Goroutines, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "sys",
			Name:        Goroutines,
			Help:        "the current number of goroutines",
			ConstLabels: constLabels,
		},
		additionalLabelsKeys,
	))

	collectorSet.addGauge("sys", HeapSize, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "sys",
			Name:        HeapSize,
			Help:        "the current heap size",
			ConstLabels: constLabels,
		},
		additionalLabelsKeys,
	))

	collectorSet.addGauge("sys", HeapObjects, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "sys",
			Name:        HeapObjects,
			Help:        "the current number of allocated heap objects",
			ConstLabels: constLabels,
		},
		additionalLabelsKeys,
	))

	collectorSet.addGauge("app", AppReadiness, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "app",
			Name:        AppReadiness,
			Help:        "whether the app and each fixed dependency are ready",
			ConstLabels: constLabels,
		},
		append([]string{"dependency"}, additionalLabelsKeys...),
	))

	collectorSet.addGauge("worker", WorkerJobsRetry, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "worker",
			Name:        WorkerJobsRetry,
			Help:        "the current number of job retries",
			ConstLabels: constLabels,
		},
		additionalLabelsKeys,
	))

	collectorSet.addGauge("worker", WorkerQueueSize, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "worker",
			Name:        WorkerQueueSize,
			Help:        "the current queue size",
			ConstLabels: constLabels,
		},
		append([]string{"queue"}, additionalLabelsKeys...),
	))

	collectorSet.addGauge("worker", WorkerJobsTotal, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "worker",
			Name:        WorkerJobsTotal,
			Help:        "the total executed jobs",
			ConstLabels: constLabels,
		},
		append([]string{"status"}, additionalLabelsKeys...),
	))

	collectorSet.addCounter("acceptor", ExceededRateLimiting, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   prometheusNamespace,
			Subsystem:   "acceptor",
			Name:        ExceededRateLimiting,
			Help:        "the number of blocked requests by exceeded rate limiting",
			ConstLabels: constLabels,
		},
		additionalLabelsKeys,
	))

	registrations := collectorSet.registrations()
	if err := validatePrometheusMetricNames(registrations); err != nil {
		return err
	}

	for i, registration := range registrations {
		if err := registerer.Register(registration.collector); err != nil {
			for registered := i - 1; registered >= 0; registered-- {
				registerer.Unregister(registrations[registered].collector)
			}
			return fmt.Errorf("register prometheus metric: %w", err)
		}
	}
	return nil
}

func cloneLabelMap(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func newPrometheusReporter(
	serverType string,
	config config.MetricsConfig,
	metricsSpecs *models.CustomMetricsSpec,
	registerer prometheus.Registerer,
) (*PrometheusReporter, error) {
	reporter := &PrometheusReporter{
		serverType:            serverType,
		game:                  config.Game,
		countReportersMap:     make(map[string]*prometheus.CounterVec),
		histogramReportersMap: make(map[string]*prometheus.HistogramVec),
		summaryReportersMap:   make(map[string]*prometheus.SummaryVec),
		gaugeReportersMap:     make(map[string]*prometheus.GaugeVec),
	}
	if err := reporter.registerMetrics(config.ConstLabels, config.AdditionalLabels, metricsSpecs, registerer); err != nil {
		return nil, err
	}
	return reporter, nil
}

// NewPrometheusReporter creates a reporter registered only in a private
// registry. It does not open a listener or mutate Prometheus globals.
func NewPrometheusReporter(
	serverType string,
	metricsConfig config.MetricsConfig,
	metricsSpecs models.CustomMetricsSpec,
) (*PrometheusReporter, *prometheus.Registry, error) {
	registry := prometheus.NewRegistry()
	reporter, err := newPrometheusReporter(
		serverType,
		metricsConfig,
		&metricsSpecs,
		registry,
	)
	if err != nil {
		return nil, nil, err
	}
	return reporter, registry, nil
}

// ReportSummary reports a summary metric
func (p *PrometheusReporter) ReportSummary(metric string, labels map[string]string, value float64) (err error) {
	defer recoverPrometheusReport(metric, prometheusSummary, &err)
	sum := p.summaryReportersMap[metric]
	if sum != nil {
		observer, err := sum.GetMetricWith(p.withDefaultLabels(labels))
		if err != nil {
			return fmt.Errorf("invalid labels for prometheus summary %q: %w", metric, err)
		}
		observer.Observe(value)
		return nil
	}
	return constants.ErrMetricNotKnown
}

// ReportHistogram reports a histogram metric
func (p *PrometheusReporter) ReportHistogram(metric string, labels map[string]string, value float64) (err error) {
	defer recoverPrometheusReport(metric, prometheusHistogram, &err)
	hist := p.histogramReportersMap[metric]
	if hist != nil {
		observer, err := hist.GetMetricWith(p.withDefaultLabels(labels))
		if err != nil {
			return fmt.Errorf("invalid labels for prometheus histogram %q: %w", metric, err)
		}
		observer.Observe(value)
		return nil
	}
	return constants.ErrMetricNotKnown
}

// ReportCount reports a summary metric
func (p *PrometheusReporter) ReportCount(metric string, labels map[string]string, count float64) (err error) {
	defer recoverPrometheusReport(metric, prometheusCounter, &err)
	cnt := p.countReportersMap[metric]
	if cnt != nil {
		counter, err := cnt.GetMetricWith(p.withDefaultLabels(labels))
		if err != nil {
			return fmt.Errorf("invalid labels for prometheus counter %q: %w", metric, err)
		}
		counter.Add(count)
		return nil
	}
	return constants.ErrMetricNotKnown
}

// ReportGauge reports a gauge metric
func (p *PrometheusReporter) ReportGauge(metric string, labels map[string]string, value float64) (err error) {
	defer recoverPrometheusReport(metric, prometheusGauge, &err)
	g := p.gaugeReportersMap[metric]
	if g != nil {
		gauge, err := g.GetMetricWith(p.withDefaultLabels(labels))
		if err != nil {
			return fmt.Errorf("invalid labels for prometheus gauge %q: %w", metric, err)
		}
		gauge.Set(value)
		return nil
	}
	return constants.ErrMetricNotKnown
}

func recoverPrometheusReport(metric string, kind prometheusMetricKind, err *error) {
	if recovered := recover(); recovered != nil {
		*err = recoveredPrometheusError(fmt.Sprintf("report prometheus %s %q", kind, metric), recovered)
	}
}

func recoveredPrometheusError(operation string, recovered any) error {
	return fmt.Errorf("%s: recovered panic of type %T", operation, recovered)
}

// withDefaultLabels returns a caller-independent label set. GetMetricWith uses
// the Vec's declared variable labels as the allowlist and reports missing or
// unexpected labels as errors instead of panicking.
func (p *PrometheusReporter) withDefaultLabels(labels map[string]string) prometheus.Labels {
	completed := make(prometheus.Labels, len(labels)+len(p.additionalLabels))
	for key, value := range labels {
		completed[key] = value
	}
	for key, defaultVal := range p.additionalLabels {
		if _, ok := completed[key]; !ok {
			completed[key] = defaultVal
		}
	}

	return completed
}
