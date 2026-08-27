package pitaya

import (
	"context"
	"fmt"
	"time"

	"github.com/alkaid/pitaya/v2/metrics"
)

const readinessCheckTimeout = 250 * time.Millisecond

const (
	ReadinessLifecycle        = "lifecycle"
	ReadinessRPCClient        = "rpc_client"
	ReadinessRPCServer        = "rpc_server"
	ReadinessServiceDiscovery = "service_discovery"
	ReadinessRedis            = "redis"
)

type ReadinessStatus string

const (
	ReadinessStatusReady         ReadinessStatus = "ready"
	ReadinessStatusNotReady      ReadinessStatus = "not_ready"
	ReadinessStatusNotApplicable ReadinessStatus = "not_applicable"
)

type ReadinessCheck struct {
	Status ReadinessStatus `json:"status"`
	Error  string          `json:"error,omitempty"`
}

type ReadinessReport struct {
	Ready  bool                      `json:"ready"`
	Checks map[string]ReadinessCheck `json:"checks"`
}

type ReadinessProvider interface {
	Readiness(context.Context) ReadinessReport
}

type readinessProbe struct {
	name       string
	configured bool
	check      func(context.Context) error
}

type readinessResult struct {
	name  string
	check ReadinessCheck
}

func (app *App) Readiness(ctx context.Context) ReadinessReport {
	if ctx == nil {
		ctx = context.Background()
	}
	checkCtx, cancel := context.WithTimeout(ctx, readinessCheckTimeout)
	defer cancel()

	probes := []readinessProbe{
		{
			name:       ReadinessLifecycle,
			configured: true,
			check: func(context.Context) error {
				if state := app.state(); state != appStateRunning {
					return fmt.Errorf("app lifecycle is %s", state)
				}
				return nil
			},
		},
		moduleReadinessProbe(ReadinessRPCClient, app.rpcClient),
		moduleReadinessProbe(ReadinessRPCServer, app.rpcServer),
		moduleReadinessProbe(ReadinessServiceDiscovery, app.serviceDiscovery),
		{
			name:       ReadinessRedis,
			configured: !isNilDependency(app.redis),
			check: func(ctx context.Context) error {
				return app.redis.Ping(ctx).Err()
			},
		},
	}

	report := ReadinessReport{
		Ready:  true,
		Checks: make(map[string]ReadinessCheck, len(probes)),
	}
	results := make(chan readinessResult, len(probes))
	pending := make(map[string]struct{}, len(probes))
	for _, probe := range probes {
		probe := probe
		if !probe.configured {
			report.Checks[probe.name] = ReadinessCheck{Status: ReadinessStatusNotApplicable}
			continue
		}
		pending[probe.name] = struct{}{}
		go func() {
			result := readinessResult{
				name:  probe.name,
				check: ReadinessCheck{Status: ReadinessStatusReady},
			}
			if err := probe.check(checkCtx); err != nil {
				result.check.Status = ReadinessStatusNotReady
				result.check.Error = err.Error()
			}
			results <- result
		}()
	}

	for len(pending) > 0 {
		select {
		case result := <-results:
			if _, waiting := pending[result.name]; !waiting {
				continue
			}
			delete(pending, result.name)
			report.Checks[result.name] = result.check
			if result.check.Status == ReadinessStatusNotReady {
				report.Ready = false
			}
		case <-checkCtx.Done():
			for name := range pending {
				report.Checks[name] = ReadinessCheck{
					Status: ReadinessStatusNotReady,
					Error:  checkCtx.Err().Error(),
				}
				delete(pending, name)
			}
			report.Ready = false
		}
	}
	if len(app.metricsReporters) == 0 {
		return report
	}
	if !app.readinessReportInFlight.CompareAndSwap(false, true) {
		return report
	}
	metricsReport := ReadinessReport{
		Ready:  report.Ready,
		Checks: make(map[string]ReadinessCheck, len(report.Checks)),
	}
	for name, check := range report.Checks {
		metricsReport.Checks[name] = check
	}
	reported := make(chan struct{})
	go func() {
		defer app.readinessReportInFlight.Store(false)
		defer close(reported)
		app.reportReadiness(metricsReport)
	}()
	select {
	case <-reported:
	case <-checkCtx.Done():
	}
	return report
}

func moduleReadinessProbe(name string, dependency any) readinessProbe {
	probe := readinessProbe{name: name}
	if isNilDependency(dependency) {
		return probe
	}
	switch running := dependency.(type) {
	case interface{ IsRunning() bool }:
		probe.configured = true
		probe.check = func(context.Context) error {
			if !running.IsRunning() {
				return fmt.Errorf("%s is not running", name)
			}
			return nil
		}
	case interface{ Running() bool }:
		probe.configured = true
		probe.check = func(context.Context) error {
			if !running.Running() {
				return fmt.Errorf("%s is not running", name)
			}
			return nil
		}
	}
	return probe
}

func (app *App) reportReadiness(report ReadinessReport) {
	metrics.ReportAppReadiness(app.metricsReporters, "overall", report.Ready)
	for _, name := range []string{
		ReadinessLifecycle,
		ReadinessRPCClient,
		ReadinessRPCServer,
		ReadinessServiceDiscovery,
		ReadinessRedis,
	} {
		check := report.Checks[name]
		metrics.ReportAppReadiness(app.metricsReporters, name, check.Status != ReadinessStatusNotReady)
	}
}

func (state appLifecycleState) String() string {
	switch state {
	case appStateCreated:
		return "created"
	case appStateStarting:
		return "starting"
	case appStateRunning:
		return "running"
	case appStateStopping:
		return "stopping"
	case appStateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}
