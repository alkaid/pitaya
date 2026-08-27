package pitaya

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type readinessRPCClientStub struct {
	cluster.RPCClient
	ready bool
}

func (stub *readinessRPCClientStub) IsRunning() bool { return stub.ready }

type blockingReadinessRPCClientStub struct {
	cluster.RPCClient
	release <-chan struct{}
}

func (stub *blockingReadinessRPCClientStub) IsRunning() bool {
	<-stub.release
	return true
}

type opaqueReadinessRPCClientStub struct{ cluster.RPCClient }

type readinessRPCServerStub struct {
	cluster.RPCServer
	ready bool
}

func (stub *readinessRPCServerStub) IsRunning() bool { return stub.ready }

type readinessServiceDiscoveryStub struct {
	cluster.ServiceDiscovery
	ready bool
}

func (stub *readinessServiceDiscoveryStub) Running() bool { return stub.ready }

type readinessMetricsRecorder struct {
	mu     sync.Mutex
	gauges map[string]float64
}

type blockingReadinessMetricsReporter struct {
	*readinessMetricsRecorder
	once     sync.Once
	calls    atomic.Int32
	complete atomic.Int32
	started  chan struct{}
	release  <-chan struct{}
	finished chan struct{}
}

func (recorder *readinessMetricsRecorder) ReportCount(string, map[string]string, float64) error {
	return nil
}

func (recorder *readinessMetricsRecorder) ReportSummary(string, map[string]string, float64) error {
	return nil
}

func (recorder *readinessMetricsRecorder) ReportHistogram(string, map[string]string, float64) error {
	return nil
}

func (recorder *readinessMetricsRecorder) ReportGauge(metric string, tags map[string]string, value float64) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if metric == metrics.AppReadiness {
		recorder.gauges[tags["dependency"]] = value
	}
	return nil
}

func (reporter *blockingReadinessMetricsReporter) ReportGauge(metric string, tags map[string]string, value float64) error {
	reporter.calls.Add(1)
	reporter.once.Do(func() {
		close(reporter.started)
		<-reporter.release
	})
	err := reporter.readinessMetricsRecorder.ReportGauge(metric, tags, value)
	if reporter.complete.Add(1) == 6 {
		close(reporter.finished)
	}
	return err
}

func readyTestApp() *App {
	app := &App{
		rpcClient:        &readinessRPCClientStub{ready: true},
		rpcServer:        &readinessRPCServerStub{ready: true},
		serviceDiscovery: &readinessServiceDiscoveryStub{ready: true},
	}
	app.lifecycleState.Store(uint32(appStateRunning))
	return app
}

func TestReadinessReportsCompositeStateAndMetrics(t *testing.T) {
	app := readyTestApp()
	recorder := &readinessMetricsRecorder{gauges: make(map[string]float64)}
	app.metricsReporters = []metrics.Reporter{recorder}

	report := app.Readiness(context.Background())
	require.True(t, report.Ready)
	assert.Equal(t, ReadinessStatusReady, report.Checks[ReadinessLifecycle].Status)
	assert.Equal(t, ReadinessStatusReady, report.Checks[ReadinessRPCClient].Status)
	assert.Equal(t, ReadinessStatusReady, report.Checks[ReadinessRPCServer].Status)
	assert.Equal(t, ReadinessStatusReady, report.Checks[ReadinessServiceDiscovery].Status)
	assert.Equal(t, ReadinessStatusNotApplicable, report.Checks[ReadinessRedis].Status)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	require.Len(t, recorder.gauges, 6)
	for dependency, value := range recorder.gauges {
		assert.Equal(t, float64(1), value, dependency)
	}
}

func TestReadinessAcceptsNilContext(t *testing.T) {
	app := readyTestApp()

	report := app.Readiness(nil)

	require.True(t, report.Ready)
	assert.Equal(t, ReadinessStatusReady, report.Checks[ReadinessLifecycle].Status)
}

func TestReadinessIdentifiesEachDegradedDependency(t *testing.T) {
	tests := []struct {
		name       string
		dependency string
		degrade    func(*App)
	}{
		{
			name:       "lifecycle",
			dependency: ReadinessLifecycle,
			degrade: func(app *App) {
				app.lifecycleState.Store(uint32(appStateStopping))
			},
		},
		{
			name:       "rpc client",
			dependency: ReadinessRPCClient,
			degrade: func(app *App) {
				app.rpcClient.(*readinessRPCClientStub).ready = false
			},
		},
		{
			name:       "rpc server",
			dependency: ReadinessRPCServer,
			degrade: func(app *App) {
				app.rpcServer.(*readinessRPCServerStub).ready = false
			},
		},
		{
			name:       "service discovery",
			dependency: ReadinessServiceDiscovery,
			degrade: func(app *App) {
				app.serviceDiscovery.(*readinessServiceDiscoveryStub).ready = false
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := readyTestApp()
			test.degrade(app)
			report := app.Readiness(context.Background())
			require.False(t, report.Ready)
			assert.Equal(t, ReadinessStatusNotReady, report.Checks[test.dependency].Status)
			assert.NotEmpty(t, report.Checks[test.dependency].Error)
		})
	}
}

func TestReadinessReportsUnstartedGRPCServerAsNotReady(t *testing.T) {
	app := readyTestApp()
	app.rpcServer = &cluster.GRPCServer{}

	report := app.Readiness(context.Background())
	require.False(t, report.Ready)
	assert.Equal(t, ReadinessStatusNotReady, report.Checks[ReadinessRPCServer].Status)
	assert.Contains(t, report.Checks[ReadinessRPCServer].Error, "rpc_server is not running")
}

func TestReadinessBoundsRedisPingByParentDeadline(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr: "readiness.invalid:6379",
		Dialer: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	app := readyTestApp()
	app.redis = client
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	report := app.Readiness(ctx)

	require.False(t, report.Ready)
	assert.Equal(t, ReadinessStatusNotReady, report.Checks[ReadinessRedis].Status)
	assert.Less(t, time.Since(started), readinessCheckTimeout)
}

func TestReadinessBoundsBlockingStatusCheckByParentDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	app := readyTestApp()
	app.rpcClient = &blockingReadinessRPCClientStub{release: release}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()

	report := app.Readiness(ctx)

	require.False(t, report.Ready)
	assert.Equal(t, ReadinessStatusNotReady, report.Checks[ReadinessRPCClient].Status)
	assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	assert.Less(t, time.Since(started), readinessCheckTimeout)
}

func TestReadinessBoundsAndDeduplicatesBlockingMetricsReporter(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	reporter := &blockingReadinessMetricsReporter{
		readinessMetricsRecorder: &readinessMetricsRecorder{gauges: make(map[string]float64)},
		started:                  make(chan struct{}),
		release:                  release,
		finished:                 make(chan struct{}),
	}
	app := readyTestApp()
	app.metricsReporters = []metrics.Reporter{reporter}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan ReadinessReport, 1)
	go func() { result <- app.Readiness(ctx) }()

	select {
	case <-reporter.started:
	case <-time.After(time.Second):
		t.Fatal("blocking metrics reporter was not called")
	}
	cancel()

	select {
	case report := <-result:
		assert.True(t, report.Ready)
		report.Ready = false
		report.Checks[ReadinessLifecycle] = ReadinessCheck{Status: ReadinessStatusNotReady}
	case <-time.After(time.Second):
		t.Fatal("readiness did not honor parent cancellation while reporting metrics")
	}
	for range 20 {
		assert.True(t, app.Readiness(context.Background()).Ready)
	}
	assert.Equal(t, int32(1), reporter.calls.Load())

	releaseOnce.Do(func() { close(release) })
	select {
	case <-reporter.finished:
	case <-time.After(time.Second):
		t.Fatal("blocking metrics reporter did not exit after release")
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	require.Len(t, reporter.gauges, 6)
	assert.Equal(t, float64(1), reporter.gauges["overall"])
	assert.Equal(t, float64(1), reporter.gauges[ReadinessLifecycle])
}

func TestReadinessMarksDependencyWithoutStatusCapabilityNotApplicable(t *testing.T) {
	app := readyTestApp()
	app.rpcClient = &opaqueReadinessRPCClientStub{}

	report := app.Readiness(context.Background())

	require.True(t, report.Ready)
	assert.Equal(t, ReadinessStatusNotApplicable, report.Checks[ReadinessRPCClient].Status)
}

func TestModuleReadinessProbeTreatsTypedNilAsUnconfigured(t *testing.T) {
	var dependency *readinessRPCClientStub

	probe := moduleReadinessProbe(ReadinessRPCClient, dependency)

	assert.Equal(t, ReadinessRPCClient, probe.name)
	assert.False(t, probe.configured)
	assert.Nil(t, probe.check)
}

func TestAppLifecycleStateStringReportsUnknownValues(t *testing.T) {
	assert.Equal(t, "unknown", appLifecycleState(255).String())
}

func TestReadinessConcurrentCallsDoNotMutateDependencies(t *testing.T) {
	app := readyTestApp()
	const calls = 100
	var wg sync.WaitGroup
	wg.Add(calls)
	for range calls {
		go func() {
			defer wg.Done()
			require.True(t, app.Readiness(context.Background()).Ready)
		}()
	}
	wg.Wait()
	assert.True(t, app.rpcClient.(*readinessRPCClientStub).ready)
	assert.True(t, app.rpcServer.(*readinessRPCServerStub).ready)
	assert.True(t, app.serviceDiscovery.(*readinessServiceDiscoveryStub).ready)
}
