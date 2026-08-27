package pitaya

import (
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/metrics/models"
	"go.uber.org/zap"
)

// CreatePrometheusReporter creates the process-wide compatibility reporter.
//
// Deprecated: use the default Builder-managed Prometheus reporter/exporter.
// All callers share one process owner and must coordinate a single
// ShutdownPrometheusReporter call after their references stop reporting.
func CreatePrometheusReporter(serverType string, config config.MetricsConfig, customSpecs models.CustomMetricsSpec) (*metrics.PrometheusReporter, error) {
	logger.Sugar.Infof("prometheus is enabled, configuring reporter on port %d", config.Prometheus.Port)
	prometheus, err := metrics.GetPrometheusReporter(serverType, config, customSpecs)
	if err != nil {
		logger.Zap.Error("failed to start prometheus metrics reporter, skipping ", zap.Error(err))
	}
	return prometheus, err
}

// ShutdownPrometheusReporter stops the process-wide compatibility exporter.
// It is idempotent.
func ShutdownPrometheusReporter() error {
	return metrics.ShutdownPrometheusReporter()
}

func newManagedPrometheus(
	serverType string,
	config config.MetricsConfig,
	customSpecs models.CustomMetricsSpec,
	failureSignal chan<- bool,
) (*metrics.PrometheusReporter, *metrics.PrometheusExporter, error) {
	reporter, registry, err := metrics.NewPrometheusReporter(serverType, config, customSpecs)
	if err != nil {
		return nil, nil, err
	}
	exporter, err := metrics.NewPrometheusExporter(config.Prometheus.Port, registry, failureSignal)
	if err != nil {
		return nil, nil, err
	}
	return reporter, exporter, nil
}

// CreateStatsdReporter create a Statsd reporter instance
func CreateStatsdReporter(serverType string, config config.MetricsConfig) (*metrics.StatsdReporter, error) {
	logger.Sugar.Infof(
		"statsd is enabled, configuring the metrics reporter with host: %s",
		config.Statsd.Host,
	)
	metricsReporter, err := metrics.NewStatsdReporter(
		config,
		serverType,
	)
	if err != nil {
		logger.Zap.Error("failed to start statds metrics reporter, skipping", zap.Error(err))
	}
	return metricsReporter, err
}
