package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics/models"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

const prometheusExporterShutdownTimeout = 5 * time.Second

// PrometheusExporter owns the HTTP listener for one Prometheus reporter.
// Construction has no network side effects; Init performs the readiness bind.
type PrometheusExporter struct {
	port          int
	handler       http.Handler
	failureSignal chan<- bool

	mu          sync.Mutex
	listener    net.Listener
	server      *http.Server
	serveDone   chan struct{}
	serveErr    error
	initialized bool
	started     bool
	stopping    bool

	shutdownOnce sync.Once
	shutdownErr  error
}

// NewPrometheusExporter creates an App-managed exporter without opening a port.
func NewPrometheusExporter(
	port int,
	pitayaGatherer prometheus.Gatherer,
	failureSignal chan<- bool,
) (*PrometheusExporter, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("prometheus exporter port must be between 0 and 65535, got %d", port)
	}
	if pitayaGatherer == nil {
		return nil, errors.New("prometheus exporter requires a Pitaya gatherer")
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(
		prometheus.Gatherers{pitayaGatherer, prometheus.DefaultGatherer},
		promhttp.HandlerOpts{ErrorHandling: promhttp.HTTPErrorOnError},
	))
	return &PrometheusExporter{
		port:          port,
		handler:       mux,
		failureSignal: failureSignal,
	}, nil
}

// Init synchronously binds the exporter port. A bind failure prevents App
// readiness because the exporter is a required module when enabled.
func (e *PrometheusExporter) Init() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.initialized {
		return errors.New("prometheus exporter is already initialized")
	}
	if e.stopping {
		return errors.New("prometheus exporter is already shutting down")
	}

	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(e.port)))
	if err != nil {
		return fmt.Errorf("bind prometheus exporter port %d: %w", e.port, err)
	}
	e.listener = listener
	e.server = &http.Server{
		Handler:           e.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	e.serveDone = make(chan struct{})
	e.initialized = true
	return nil
}

// AfterInit starts serving on the listener acquired by Init.
func (e *PrometheusExporter) AfterInit() {
	e.mu.Lock()
	if !e.initialized {
		e.mu.Unlock()
		panic("prometheus exporter AfterInit called before Init")
	}
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	server := e.server
	listener := e.listener
	serveDone := e.serveDone
	e.mu.Unlock()

	go e.serve(server, listener, serveDone)
}

func (e *PrometheusExporter) serve(server *http.Server, listener net.Listener, serveDone chan struct{}) {
	err := server.Serve(listener)
	e.mu.Lock()
	e.serveErr = err
	stopping := e.stopping
	close(serveDone)
	e.mu.Unlock()

	if stopping {
		return
	}
	logger.Zap.Error("prometheus exporter stopped unexpectedly", zap.Error(err))
	if e.failureSignal != nil {
		select {
		case e.failureSignal <- true:
		default:
		}
	}
}

// BeforeShutdown stops accepting new scrape connections. Active requests are
// drained by Shutdown.
func (e *PrometheusExporter) BeforeShutdown() {
	e.stopAdmission()
}

func (e *PrometheusExporter) stopAdmission() {
	e.mu.Lock()
	e.stopping = true
	listener := e.listener
	e.mu.Unlock()
	if listener == nil {
		return
	}
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		e.mu.Lock()
		e.shutdownErr = errors.Join(e.shutdownErr, fmt.Errorf("close prometheus listener: %w", err))
		e.mu.Unlock()
	}
}

// Shutdown drains active scrapes within a fixed bound and releases the port.
// It is safe to call repeatedly.
func (e *PrometheusExporter) Shutdown() error {
	e.shutdownOnce.Do(func() {
		e.stopAdmission()

		e.mu.Lock()
		server := e.server
		started := e.started
		serveDone := e.serveDone
		e.mu.Unlock()
		if server == nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), prometheusExporterShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr := server.Close()
			if errors.Is(closeErr, http.ErrServerClosed) {
				closeErr = nil
			}
			e.mu.Lock()
			e.shutdownErr = errors.Join(e.shutdownErr, fmt.Errorf("shutdown prometheus HTTP server: %w", err), closeErr)
			e.mu.Unlock()
		}

		if started {
			select {
			case <-serveDone:
			case <-ctx.Done():
				e.mu.Lock()
				e.shutdownErr = errors.Join(e.shutdownErr, fmt.Errorf("wait for prometheus exporter: %w", ctx.Err()))
				e.mu.Unlock()
			}
		}
	})

	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownErr
}

// Addr returns the bound listener address, or an empty string before Init.
func (e *PrometheusExporter) Addr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.listener == nil {
		return ""
	}
	return e.listener.Addr().String()
}

var legacyPrometheusOwner struct {
	sync.Mutex
	reporter *PrometheusReporter
	exporter *PrometheusExporter
}

// GetPrometheusReporter gets the process-wide compatibility reporter.
//
// Deprecated: use NewPrometheusReporter with an App-managed
// PrometheusExporter. Every caller of this function shares one process owner
// and must coordinate a single ShutdownPrometheusReporter call.
func GetPrometheusReporter(
	serverType string,
	metricsConfig config.MetricsConfig,
	metricsSpecs models.CustomMetricsSpec,
) (*PrometheusReporter, error) {
	legacyPrometheusOwner.Lock()
	defer legacyPrometheusOwner.Unlock()
	if legacyPrometheusOwner.reporter != nil {
		return legacyPrometheusOwner.reporter, nil
	}

	reporter, registry, err := NewPrometheusReporter(serverType, metricsConfig, metricsSpecs)
	if err != nil {
		return nil, err
	}
	exporter, err := NewPrometheusExporter(metricsConfig.Prometheus.Port, registry, nil)
	if err != nil {
		return nil, err
	}
	if err := exporter.Init(); err != nil {
		return nil, err
	}
	exporter.AfterInit()
	legacyPrometheusOwner.reporter = reporter
	legacyPrometheusOwner.exporter = exporter
	return reporter, nil
}

// ShutdownPrometheusReporter stops the process-wide compatibility exporter.
// It is idempotent and must only be called after all shared reporter references
// have stopped using the process owner.
func ShutdownPrometheusReporter() error {
	legacyPrometheusOwner.Lock()
	defer legacyPrometheusOwner.Unlock()
	if legacyPrometheusOwner.exporter == nil {
		return nil
	}
	err := legacyPrometheusOwner.exporter.Shutdown()
	legacyPrometheusOwner.reporter = nil
	legacyPrometheusOwner.exporter = nil
	return err
}
