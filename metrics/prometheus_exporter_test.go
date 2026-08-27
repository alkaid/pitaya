package metrics

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/metrics/models"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestPrometheusExporterLifecycleRegistryAndShutdown(t *testing.T) {
	defaultCollector := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pitaya_exporter_default_lifecycle_probe",
		Help: "default registry exporter lifecycle probe",
	})
	require.NoError(t, prometheus.Register(defaultCollector))
	t.Cleanup(func() { prometheus.Unregister(defaultCollector) })
	defaultCollector.Set(2)

	reporter, registry, err := NewPrometheusReporter("frontend", emptyMetricsConfig(), models.CustomMetricsSpec{
		Gauges: []*models.Gauge{{
			Subsystem: "test",
			Name:      "exporter_private_lifecycle_probe",
			Help:      "private registry exporter lifecycle probe",
		}},
	})
	require.NoError(t, err)
	require.NoError(t, reporter.ReportGauge("exporter_private_lifecycle_probe", nil, 1))

	failures := make(chan bool, 1)
	exporter, err := NewPrometheusExporter(0, registry, failures)
	require.NoError(t, err)
	require.Empty(t, exporter.Addr())
	require.NoError(t, exporter.Init())
	require.NotEmpty(t, exporter.Addr())
	exporter.AfterInit()

	status, body := scrapeExporter(t, exporter)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, "pitaya_test_exporter_private_lifecycle_probe")
	require.Contains(t, body, "pitaya_exporter_default_lifecycle_probe")

	addr := exporter.Addr()
	require.NoError(t, exporter.Shutdown())
	require.NoError(t, exporter.Shutdown())
	select {
	case <-failures:
		t.Fatal("normal Prometheus shutdown emitted a dependency failure")
	default:
	}

	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err, "Prometheus port was not released")
	require.NoError(t, listener.Close())
}

func TestPrometheusExporterBindFailureIsSynchronous(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, occupied.Close()) })
	port := occupied.Addr().(*net.TCPAddr).Port

	exporter, err := NewPrometheusExporter(port, prometheus.NewRegistry(), nil)
	require.NoError(t, err)
	err = exporter.Init()
	require.Error(t, err)
	require.Contains(t, err.Error(), "bind prometheus exporter port")
	require.Empty(t, exporter.Addr())
}

func TestPrometheusExporterValidatesConstructionAndLifecycle(t *testing.T) {
	for _, port := range []int{-1, 65536} {
		_, err := NewPrometheusExporter(port, prometheus.NewRegistry(), nil)
		require.ErrorContains(t, err, "port must be between 0 and 65535")
	}
	_, err := NewPrometheusExporter(0, nil, nil)
	require.ErrorContains(t, err, "requires a Pitaya gatherer")

	stopped, err := NewPrometheusExporter(0, prometheus.NewRegistry(), nil)
	require.NoError(t, err)
	require.PanicsWithValue(t, "prometheus exporter AfterInit called before Init", stopped.AfterInit)
	stopped.BeforeShutdown()
	require.ErrorContains(t, stopped.Init(), "already shutting down")
	require.NoError(t, stopped.Shutdown())
	require.NoError(t, stopped.Shutdown())

	started, err := NewPrometheusExporter(0, prometheus.NewRegistry(), nil)
	require.NoError(t, err)
	require.NoError(t, started.Init())
	require.ErrorContains(t, started.Init(), "already initialized")
	started.AfterInit()
	started.AfterInit()
	require.NoError(t, started.Shutdown())
}

type closeErrorListener struct {
	net.Listener
	err error
}

func (l *closeErrorListener) Close() error {
	return l.err
}

func TestPrometheusExporterPreservesListenerCloseErrors(t *testing.T) {
	want := errors.New("close listener failed")
	exporter := &PrometheusExporter{
		listener: &closeErrorListener{err: want},
	}

	exporter.BeforeShutdown()
	require.ErrorIs(t, exporter.Shutdown(), want)
	require.ErrorIs(t, exporter.Shutdown(), want)
}

func TestPrometheusExporterShutdownForcesBlockedScrapeAfterTimeout(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() {
		releaseOnce.Do(func() { close(release) })
	}
	t.Cleanup(releaseHandler)

	exporter := &PrometheusExporter{
		port: 0,
		handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(entered)
			<-release
		}),
	}
	require.NoError(t, exporter.Init())
	exporter.AfterInit()

	requestDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 2 * prometheusExporterShutdownTimeout}
		response, err := client.Get("http://" + exporter.Addr())
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- err
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("blocking scrape did not reach the exporter")
	}

	err := exporter.Shutdown()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	releaseHandler()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("forced scrape connection did not exit")
	}
}

func TestPrometheusExporterInstancesDoNotUseDefaultServeMux(t *testing.T) {
	originalMux := http.DefaultServeMux
	http.DefaultServeMux = http.NewServeMux()
	t.Cleanup(func() { http.DefaultServeMux = originalMux })

	exporters := make([]*PrometheusExporter, 0, 2)
	for range 2 {
		reporter, registry, err := NewPrometheusReporter("frontend", emptyMetricsConfig(), models.CustomMetricsSpec{})
		require.NoError(t, err)
		require.NotNil(t, reporter)
		exporter, err := NewPrometheusExporter(0, registry, nil)
		require.NoError(t, err)
		require.NoError(t, exporter.Init())
		exporter.AfterInit()
		exporters = append(exporters, exporter)
	}
	t.Cleanup(func() {
		for _, exporter := range exporters {
			require.NoError(t, exporter.Shutdown())
		}
	})

	for _, exporter := range exporters {
		status, _ := scrapeExporter(t, exporter)
		require.Equal(t, http.StatusOK, status)
	}
	recorder := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestPrometheusExporterReportsDuplicateFQNameAcrossGatherers(t *testing.T) {
	const metricName = "pitaya_exporter_duplicate_probe"
	defaultCollector := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: metricName,
		Help: "default duplicate probe",
	})
	require.NoError(t, prometheus.Register(defaultCollector))
	t.Cleanup(func() { prometheus.Unregister(defaultCollector) })
	defaultCollector.Set(1)

	reporter, registry, err := NewPrometheusReporter("frontend", emptyMetricsConfig(), models.CustomMetricsSpec{
		Gauges: []*models.Gauge{{
			Name: "exporter_duplicate_probe",
			Help: "private duplicate probe",
		}},
	})
	require.NoError(t, err)
	require.NoError(t, reporter.ReportGauge("exporter_duplicate_probe", nil, 1))
	exporter, err := NewPrometheusExporter(0, registry, nil)
	require.NoError(t, err)
	require.NoError(t, exporter.Init())
	exporter.AfterInit()
	t.Cleanup(func() { require.NoError(t, exporter.Shutdown()) })

	status, body := scrapeExporter(t, exporter)
	require.Equal(t, http.StatusInternalServerError, status)
	require.Contains(t, body, metricName)
}

func TestPrometheusExporterUnexpectedServeExitSignalsDependencyFailure(t *testing.T) {
	failures := make(chan bool, 1)
	exporter, err := NewPrometheusExporter(0, prometheus.NewRegistry(), failures)
	require.NoError(t, err)
	require.NoError(t, exporter.Init())
	exporter.AfterInit()

	exporter.mu.Lock()
	listener := exporter.listener
	exporter.mu.Unlock()
	require.NoError(t, listener.Close())
	select {
	case <-failures:
	case <-time.After(time.Second):
		t.Fatal("unexpected Prometheus Serve exit did not signal dependency failure")
	}
	require.NoError(t, exporter.Shutdown())
}

func TestLegacyPrometheusReporterHasOneImmediateProcessOwner(t *testing.T) {
	require.NoError(t, ShutdownPrometheusReporter())
	t.Cleanup(func() { require.NoError(t, ShutdownPrometheusReporter()) })

	cfg := emptyMetricsConfig()
	cfg.Prometheus = &config.PrometheusConfig{Enabled: true, Port: 0}
	spec := models.CustomMetricsSpec{Gauges: []*models.Gauge{{
		Subsystem: "test",
		Name:      "legacy_exporter_probe",
		Help:      "legacy exporter probe",
	}}}
	first, err := GetPrometheusReporter("frontend", cfg, spec)
	require.NoError(t, err)
	second, err := GetPrometheusReporter("ignored", cfg, models.CustomMetricsSpec{})
	require.NoError(t, err)
	require.Same(t, first, second)
	require.NoError(t, first.ReportGauge("legacy_exporter_probe", nil, 1))

	legacyPrometheusOwner.Lock()
	exporter := legacyPrometheusOwner.exporter
	legacyPrometheusOwner.Unlock()
	status, body := scrapeExporter(t, exporter)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, "pitaya_test_legacy_exporter_probe")
	addr := exporter.Addr()

	require.NoError(t, ShutdownPrometheusReporter())
	require.NoError(t, ShutdownPrometheusReporter())
	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
}

func TestLegacyPrometheusReporterPropagatesSetupErrors(t *testing.T) {
	require.NoError(t, ShutdownPrometheusReporter())
	t.Cleanup(func() { require.NoError(t, ShutdownPrometheusReporter()) })

	_, err := GetPrometheusReporter("frontend", emptyMetricsConfig(), models.CustomMetricsSpec{
		Histograms: []*models.Histogram{
			{Subsystem: "alpha", Name: "duplicate", Help: "alpha", Buckets: []float64{1}},
			{Subsystem: "beta", Name: "duplicate", Help: "beta", Buckets: []float64{2}},
		},
	})
	require.ErrorContains(t, err, "duplicate custom histogram name")

	invalidPort := emptyMetricsConfig()
	invalidPort.Prometheus = &config.PrometheusConfig{Enabled: true, Port: 65536}
	_, err = GetPrometheusReporter("frontend", invalidPort, models.CustomMetricsSpec{})
	require.ErrorContains(t, err, "port must be between 0 and 65535")

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, occupied.Close()) })
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port
	bindConflict := emptyMetricsConfig()
	bindConflict.Prometheus = &config.PrometheusConfig{Enabled: true, Port: occupiedPort}
	_, err = GetPrometheusReporter("frontend", bindConflict, models.CustomMetricsSpec{})
	require.ErrorContains(t, err, "bind prometheus exporter port")

	legacyPrometheusOwner.Lock()
	reporter := legacyPrometheusOwner.reporter
	exporter := legacyPrometheusOwner.exporter
	legacyPrometheusOwner.Unlock()
	require.Nil(t, reporter)
	require.Nil(t, exporter)
}

func scrapeExporter(t *testing.T, exporter *PrometheusExporter) (int, string) {
	t.Helper()
	_, port, err := net.SplitHostPort(exporter.Addr())
	require.NoError(t, err)
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + port + "/metrics")
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, strings.TrimSpace(string(body))
}
