// Copyright (c) nano Author and TFG Co. All Rights Reserved.
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

package pitaya

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/cluster"
	clustermocks "github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	metricsmocks "github.com/alkaid/pitaya/v2/metrics/mocks"
	"github.com/alkaid/pitaya/v2/metrics/models"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/service"
	"github.com/alkaid/pitaya/v2/session"
	sessionmocks "github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type builderIdempotencyStore struct {
	calls int
}

type resolvedGraphTestRPCServer struct{}

func (*resolvedGraphTestRPCServer) SetPitayaServer(protos.PitayaServer) {}
func (*resolvedGraphTestRPCServer) Subscribe(string, ...string) error   { return nil }
func (*resolvedGraphTestRPCServer) Init() error                         { return nil }
func (*resolvedGraphTestRPCServer) AfterInit()                          {}
func (*resolvedGraphTestRPCServer) BeforeShutdown()                     {}
func (*resolvedGraphTestRPCServer) Shutdown() error                     { return nil }

func replaceBuilderSessionPoolForTest(builder *Builder, pool session.SessionPool) {
	builder.SessionPool = pool
	if builder.ServerMode == Cluster {
		builder.RPCServer = &resolvedGraphTestRPCServer{}
	}
}

func (s *builderIdempotencyStore) Execute(
	_ context.Context,
	_, _ string,
	_ func(context.Context) (*protos.Response, error),
) (*protos.Response, error) {
	s.calls++
	return &protos.Response{Data: []byte("durable")}, nil
}

func TestDefaultBuilderBuildsWithoutReloadConfig(t *testing.T) {
	for _, table := range tables {
		t.Run(table.serverType, func(t *testing.T) {
			pitayaConfig := config.NewDefaultPitayaConfig()
			builder := NewDefaultBuilder(
				table.isFrontend,
				table.serverType,
				table.serverMode,
				table.serverMetadata,
				*pitayaConfig,
			)
			require.Nil(t, builder.conf)

			var built Pitaya
			require.NotPanics(t, func() {
				built = builder.Build()
			})
			require.NotNil(t, built)
			assert.Nil(t, built.(*App).conf)
		})
	}
}

func TestBuilderWithConfigsRegistersLoggerReload(t *testing.T) {
	for _, table := range tables {
		t.Run(table.serverType, func(t *testing.T) {
			conf := config.NewConfig(table.cfg)
			loaderCount := configLoaderCount(conf)
			builder := NewBuilderWithConfigs(
				table.isFrontend,
				table.serverType,
				table.serverMode,
				table.serverMetadata,
				conf,
			)

			built := builder.Build().(*App)
			assert.Same(t, conf, built.conf)
			assert.Equal(t, loaderCount+1, configLoaderCount(conf))
		})
	}
}

func configLoaderCount(conf *config.Config) int {
	return reflect.ValueOf(conf).Elem().FieldByName("loaders").Len()
}

func TestBuilderModeValidation(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()

	t.Run("standalone rejects RPC dependencies", func(t *testing.T) {
		builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *pitayaConfig)
		builder.RPCClient = clustermocks.NewMockRPCClient(gomock.NewController(t))

		assert.PanicsWithValue(t, "Standalone mode can't have RPC or service discovery instances", func() {
			builder.Build()
		})
	})

	t.Run("cluster requires every RPC dependency", func(t *testing.T) {
		builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *pitayaConfig)
		builder.RPCClient = nil

		assert.PanicsWithValue(t, "Cluster mode must have RPC and service discovery instances", func() {
			builder.Build()
		})
	})
}

func TestBuilderPassesRPCFanoutConcurrency(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	pitayaConfig.Cluster.RPC.FanoutConcurrency = 7

	builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *pitayaConfig)
	app := builder.Build().(*App)

	fanout := reflect.ValueOf(app.remoteService).Elem().FieldByName("fanout")
	configuredConcurrency := fanout.Elem().FieldByName("concurrency").Int()
	assert.Equal(t, int64(7), configuredConcurrency)
}

func TestBuilderPassesRPCIdempotencyStore(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *pitayaConfig)
	store := &builderIdempotencyStore{}
	builder.Config.Cluster.RPC.Idempotency.Enabled = false
	builder.IdempotencyStore = store

	app := builder.Build().(*App)
	builder.Server.SetOwnerEpoch(1)
	ctx := builder.Server.WithOwnerFencingMetadata(context.Background())
	ctx = pcontext.AddToPropagateCtx(ctx, constants.IdempotencyKey, "builder-store-1")
	response, err := app.remoteService.Call(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("durable"), response.Data)
	assert.Equal(t, 1, store.calls)
}

func TestBuilderConfiguresDefaultRedisRPCIdempotencyStore(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *pitayaConfig)

	assert.Nil(t, builder.IdempotencyStore, "nil means Build should resolve the configured default store")
	app := builder.Build().(*App)
	configuredStore := reflect.ValueOf(app.remoteService).Elem().FieldByName("idempotencyStore")
	require.False(t, configuredStore.IsNil())
	assert.Equal(t, reflect.TypeOf(&service.RedisIdempotencyStore{}), configuredStore.Elem().Type())
}

func TestBuilderResolvesRPCIdempotencyConfigAtBuild(t *testing.T) {
	t.Run("disable after construction", func(t *testing.T) {
		pitayaConfig := config.NewDefaultPitayaConfig()
		builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *pitayaConfig)
		builder.Config.Cluster.RPC.Idempotency.Enabled = false

		app := builder.Build().(*App)
		configuredStore := reflect.ValueOf(app.remoteService).Elem().FieldByName("idempotencyStore")
		assert.Equal(t, reflect.TypeOf(&service.MemoryIdempotencyStore{}), configuredStore.Elem().Type())
	})

	t.Run("enable and update options after construction", func(t *testing.T) {
		pitayaConfig := config.NewDefaultPitayaConfig()
		pitayaConfig.Cluster.RPC.Idempotency.Enabled = false
		builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *pitayaConfig)
		builder.Config.Cluster.RPC.Idempotency.Enabled = true
		builder.Config.Cluster.RPC.Idempotency.KeyPrefix = "tenant:commands:"
		builder.Config.Cluster.RPC.Idempotency.ResultTTL = 21 * time.Minute

		app := builder.Build().(*App)
		configuredStore := reflect.ValueOf(app.remoteService).Elem().FieldByName("idempotencyStore")
		assert.Equal(t, reflect.TypeOf(&service.RedisIdempotencyStore{}), configuredStore.Elem().Type())
		options := configuredStore.Elem().Elem().FieldByName("options")
		assert.Equal(t, "tenant:commands:", options.FieldByName("KeyPrefix").String())
		assert.Equal(t, 21*time.Minute, time.Duration(options.FieldByName("ResultTTL").Int()))
	})
}

func TestBuilderUsesMemoryRPCIdempotencyFallbackWhenDisabled(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	pitayaConfig.Cluster.RPC.Idempotency.Enabled = false
	builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *pitayaConfig)

	assert.Nil(t, builder.IdempotencyStore)
	app := builder.Build().(*App)
	configuredStore := reflect.ValueOf(app.remoteService).Elem().FieldByName("idempotencyStore")
	require.False(t, configuredStore.IsNil())
	assert.Equal(t, reflect.TypeOf(&service.MemoryIdempotencyStore{}), configuredStore.Elem().Type())
}

func TestBuilderReadsTrustedProxyConfigAtBuild(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	pitayaConfig.Acceptor.TrustedProxyCIDRs = []string{"10.0.0.0/8"}
	builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *pitayaConfig)
	builder.Config.Acceptor.TrustedProxyCIDRs = []string{"not-a-cidr"}

	assert.Panics(t, func() {
		builder.Build()
	})
}

func TestBuilderValidatesConfigBeforeConstructingDependencies(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	pitayaConfig.Metrics.Period = 0

	assert.Panics(t, func() {
		NewDefaultBuilder(false, "standalone", Standalone, nil, *pitayaConfig)
	})
}

func TestBuilderLoadsSessionRedisTLSBeforeConstructingClient(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	secretPath := filepath.Join(t.TempDir(), "tenant-secret-ca.pem")
	pitayaConfig.Storage.Redis.TLS = config.TLSConfig{
		Enabled: true,
		CAFile:  secretPath,
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		NewDefaultBuilder(false, "standalone", Standalone, nil, *pitayaConfig)
	}()
	require.NotNil(t, recovered)
	assert.Contains(t, fmt.Sprint(recovered), "pitaya.storage.redis.tls.cafile")
	assert.NotContains(t, fmt.Sprint(recovered), secretPath)
}

func TestBuilderRevalidatesMutatedConfigAtBuild(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *pitayaConfig)
	builder.Config.Metrics.Period = 0

	assert.Panics(t, func() {
		builder.Build()
	})
}

func TestBuilderRejectsNilMetricsReporter(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *pitayaConfig)
	builder.MetricsReporters = append(builder.MetricsReporters, nil)

	assert.PanicsWithError(t, "nil metrics reporter at index 0", func() {
		builder.Build()
	})
}

func TestBuilderFailsFastWhenRequiredPrometheusReporterCannotBeConstructed(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	cfg.Metrics.Prometheus.Enabled = true
	cfg.Metrics.Custom.Histograms = []*models.Histogram{{
		Subsystem: "handler",
		Name:      metrics.ResponseTime,
		Help:      "conflicts with the built-in response-time summary",
	}}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		NewBuilder(false, "prometheus-construction", Standalone, nil, *cfg)
	}()
	require.NotNil(t, recovered)
	require.Contains(t, fmt.Sprint(recovered), "configure required Prometheus exporter")
	require.Contains(t, fmt.Sprint(recovered), "pitaya_handler_response_time_ns")
}

func TestBuilderRejectsRemovingRequiredPrometheusReporterFromResolvedGraph(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	cfg.Metrics.Prometheus.Enabled = true
	builder := NewBuilder(false, "prometheus-resolved-graph", Standalone, nil, *cfg)
	builder.MetricsReporters = nil

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		builder.Build()
	}()
	require.NotNil(t, recovered)
	require.Contains(t, fmt.Sprint(recovered), "removed the reporter owned by the required Prometheus exporter")
}

func TestBuilderGraphRejectsDefaultDependenciesWithReplacedRootsBeforeSideEffects(t *testing.T) {
	tables := []struct {
		name       string
		mutate     func(t *testing.T, builder *Builder)
		wantFields []string
	}{
		{
			name: "session pool requires custom rpc server",
			mutate: func(t *testing.T, builder *Builder) {
				builder.SessionPool = sessionmocks.NewMockSessionPool(gomock.NewController(t))
			},
			wantFields: []string{"Builder.RPCServer", "Builder.SessionPool"},
		},
		{
			name: "server requires complete custom transport",
			mutate: func(_ *testing.T, builder *Builder) {
				builder.Server = cluster.NewServer("replacement", "cluster", false)
			},
			wantFields: []string{"Builder.RPCServer", "Builder.RPCClient", "Builder.ServiceDiscovery", "Builder.Server"},
		},
		{
			name: "metrics require custom nats client and server",
			mutate: func(t *testing.T, builder *Builder) {
				builder.MetricsReporters = []metrics.Reporter{metricsmocks.NewMockReporter(gomock.NewController(t))}
			},
			wantFields: []string{"Builder.RPCServer", "Builder.RPCClient", "Builder.MetricsReporters"},
		},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *config.NewDefaultPitayaConfig())
			hookCalled := false
			builder.AddPostBuildHook(func(Pitaya) { hookCalled = true })
			table.mutate(t, builder)

			panicValue := captureBuilderPanic(t, builder)

			message := fmt.Sprint(panicValue)
			assert.Contains(t, message, "invalid builder dependency graph")
			for _, field := range table.wantFields {
				assert.Contains(t, message, field)
			}
			assert.False(t, hookCalled)
			routerSD := reflect.ValueOf(builder.Router).Elem().FieldByName("serviceDiscovery")
			assert.True(t, routerSD.IsNil(), "graph preflight must run before Router mutation")
		})
	}
}

func TestBuilderGraphRejectsStaleDefaultSessionCacheInputs(t *testing.T) {
	t.Run("redis identity", func(t *testing.T) {
		builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *config.NewDefaultPitayaConfig())
		builder.Redis = nil

		message := fmt.Sprint(captureBuilderPanic(t, builder))
		assert.Contains(t, message, "Builder.Redis")
		assert.Contains(t, message, "Builder.SessionPool")
	})

	t.Run("redis construction config", func(t *testing.T) {
		builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *config.NewDefaultPitayaConfig())
		builder.Config.Storage.Redis.Addrs = []string{"redis.internal:6380"}

		message := fmt.Sprint(captureBuilderPanic(t, builder))
		assert.Contains(t, message, "Builder.Config.Storage.Redis")
	})

	t.Run("session cache ttl", func(t *testing.T) {
		builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *config.NewDefaultPitayaConfig())
		builder.Config.Session.CacheTTL += time.Minute

		message := fmt.Sprint(captureBuilderPanic(t, builder))
		assert.Contains(t, message, "Builder.Config.Session.CacheTTL")
	})
}

func TestBuilderGraphAcceptsSupportedDependencyReplacementGroups(t *testing.T) {
	t.Run("standalone session pool", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		customPool := sessionmocks.NewMockSessionPool(ctrl)
		builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *config.NewDefaultPitayaConfig())
		builder.SessionPool = customPool

		app := builder.Build().(*App)

		assert.Same(t, customPool, app.sessionPool)
	})

	t.Run("cluster session pool and rpc server", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		customPool := sessionmocks.NewMockSessionPool(ctrl)
		customRPCServer := clustermocks.NewMockRPCServer(ctrl)
		customRPCServer.EXPECT().SetPitayaServer(gomock.Any()).Times(1)
		builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *config.NewDefaultPitayaConfig())
		builder.SessionPool = customPool
		builder.RPCServer = customRPCServer

		app := builder.Build().(*App)

		assert.Same(t, customPool, app.sessionPool)
		assert.Same(t, customRPCServer, app.rpcServer)
	})

	t.Run("server and complete custom transport", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		customServer := cluster.NewServer("replacement", "cluster", false)
		customRPCClient := clustermocks.NewMockRPCClient(ctrl)
		customRPCServer := clustermocks.NewMockRPCServer(ctrl)
		customDiscovery := clustermocks.NewMockServiceDiscovery(ctrl)
		customRPCServer.EXPECT().SetPitayaServer(gomock.Any()).Times(1)
		builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *config.NewDefaultPitayaConfig())
		builder.Server = customServer
		builder.RPCClient = customRPCClient
		builder.RPCServer = customRPCServer
		builder.ServiceDiscovery = customDiscovery

		app := builder.Build().(*App)

		assert.Same(t, customServer, app.server)
		assert.Same(t, customRPCClient, app.rpcClient)
		assert.Same(t, customRPCServer, app.rpcServer)
		assert.Same(t, customDiscovery, app.serviceDiscovery)
	})
}

func TestBuilderBuildFreezesResolvedFieldAndSliceAssignments(t *testing.T) {
	ctrl := gomock.NewController(t)
	firstReporter := metricsmocks.NewMockReporter(ctrl)
	secondReporter := metricsmocks.NewMockReporter(ctrl)
	builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *config.NewDefaultPitayaConfig())
	builder.MetricsReporters = []metrics.Reporter{firstReporter}
	originalServer := builder.Server
	originalPool := builder.SessionPool
	originalHeartbeat := builder.Config.Heartbeat.Interval

	app := builder.Build().(*App)
	builder.Server = cluster.NewServer("replacement", "standalone", false)
	builder.SessionPool = sessionmocks.NewMockSessionPool(ctrl)
	builder.MetricsReporters[0] = secondReporter
	builder.Config.Heartbeat.Interval = time.Second

	assert.Same(t, originalServer, app.server)
	assert.Same(t, originalPool, app.sessionPool)
	require.Len(t, app.metricsReporters, 1)
	assert.Same(t, firstReporter, app.metricsReporters[0])
	assert.Equal(t, originalHeartbeat, app.config.Heartbeat.Interval)
}

func TestBuilderRejectsSecondBuild(t *testing.T) {
	builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *config.NewDefaultPitayaConfig())
	require.NotNil(t, builder.Build())

	panicValue := captureBuilderPanic(t, builder)

	assert.EqualError(t, panicValue.(error), "builder build already started")
}

func TestBuilderBuildAppReturnsConcreteApp(t *testing.T) {
	builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *config.NewDefaultPitayaConfig())
	app := builder.BuildApp()

	require.NotNil(t, app)
	assert.IsType(t, &App{}, app)
}

func TestBuilderRejectsGRPCForFullClusterAppCapabilities(t *testing.T) {
	builder := NewDefaultBuilder(false, "grpc-compatibility", Cluster, nil, *config.NewDefaultPitayaConfig())
	grpcServer, err := cluster.NewGRPCServer(
		builder.Config.Cluster.RPC.Server.Grpc,
		builder.Server,
		builder.MetricsReporters,
	)
	require.NoError(t, err)
	grpcClient, err := cluster.NewGRPCClient(
		builder.Config.Cluster.RPC.Client.Grpc,
		builder.Server,
		builder.MetricsReporters,
		nil,
		cluster.NewInfoRetriever(builder.Config.Cluster.Info),
	)
	require.NoError(t, err)
	builder.RPCClient = grpcClient
	builder.RPCServer = grpcServer

	panicValue := captureBuilderPanic(t, builder)
	message := fmt.Sprint(panicValue)
	assert.Contains(t, message, "invalid RPC transport capabilities")
	assert.Contains(t, message, "point-to-point")
	assert.Contains(t, message, "fanout,user-delivery,broker-subscription")
	assert.Contains(t, message, "gRPC is point-to-point compatibility only")
}

func TestBuilderRejectsConcurrentBuild(t *testing.T) {
	builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *config.NewDefaultPitayaConfig())
	hookStarted := make(chan struct{})
	releaseHook := make(chan struct{})
	builder.AddPostBuildHook(func(Pitaya) {
		close(hookStarted)
		<-releaseHook
	})

	firstResult := make(chan Pitaya, 1)
	firstPanic := make(chan any, 1)
	go func() {
		defer func() { firstPanic <- recover() }()
		firstResult <- builder.Build()
	}()
	<-hookStarted

	panicValue := captureBuilderPanic(t, builder)
	assert.EqualError(t, panicValue.(error), "builder build already started")
	close(releaseHook)
	require.Nil(t, <-firstPanic)
	require.NotNil(t, <-firstResult)
}

func TestWarnIgnoredHandlerConfigurationLogsOneDeprecation(t *testing.T) {
	core, observed := observer.New(zapcore.WarnLevel)
	previousLogger := logger.Zap
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousLogger })
	cfg := config.NewDefaultPitayaConfig()

	warnIgnoredHandlerConfiguration(*cfg)
	assert.Zero(t, observed.Len())

	cfg.Buffer.Handler.LocalProcess = -1
	cfg.Buffer.Handler.RemoteProcess = 999
	cfg.Concurrency.Handler.Dispatch = 0
	warnIgnoredHandlerConfiguration(*cfg)

	entries := observed.All()
	require.Len(t, entries, 1)
	assert.Equal(t, "deprecated handler buffer and dispatch configuration is ignored", entries[0].Message)
	fields := entries[0].ContextMap()
	assert.EqualValues(t, -1, fields["pitaya.buffer.handler.localprocess"])
	assert.EqualValues(t, 999, fields["pitaya.buffer.handler.remoteprocess"])
	assert.EqualValues(t, 0, fields["pitaya.concurrency.handler.dispatch"])
	assert.Len(t, fields["actual_capacity_owners"], 4)
}

func captureBuilderPanic(t *testing.T, builder *Builder) any {
	t.Helper()
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		builder.Build()
	}()
	require.NotNil(t, panicValue)
	return panicValue
}

func TestBuilderStartConfigModulePolicy(t *testing.T) {
	for _, mode := range []string{"snapshot", "reloadable"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestBuilderStartConfigModulePolicyHelper$")
			cmd.Env = append(os.Environ(), "PITAYA_BUILDER_START_HELPER="+mode)
			output, err := cmd.CombinedOutput()
			require.NoErrorf(t, err, "helper output:\n%s", output)
		})
	}
}

func TestBuilderStartConfigModulePolicyHelper(t *testing.T) {
	mode := os.Getenv("PITAYA_BUILDER_START_HELPER")
	if mode == "" {
		return
	}

	var app *App
	switch mode {
	case "snapshot":
		pitayaConfig := config.NewDefaultPitayaConfig()
		app = NewDefaultBuilder(false, mode, Standalone, nil, *pitayaConfig).Build().(*App)
	case "reloadable":
		app = NewBuilderWithConfigs(false, mode, Standalone, nil, config.NewConfig()).Build().(*App)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}

	app.onStarted = func() { app.dieChan <- true }
	app.Start()

	_, err := app.GetModule("configLoader")
	if mode == "reloadable" {
		assert.NoError(t, err)
	} else {
		assert.EqualError(t, err, "module with name configLoader not found")
	}
}

func TestPostBuildHooks(t *testing.T) {
	acc := acceptor.NewTCPAcceptor("0.0.0.0:0")
	for _, table := range tables {
		builderConfig := config.NewDefaultPitayaConfig()

		t.Run("with_post_build_hooks", func(t *testing.T) {
			called := false
			builder := newAppTestBuilder(table.isFrontend, table.serverType, table.serverMode, table.serverMetadata, *builderConfig)
			builder.AddAcceptor(acc)
			builder.AddPostBuildHook(func(app Pitaya) {
				called = true
			})
			app := builder.Build()

			assert.True(t, called)
			assert.NotNil(t, app)
		})

		t.Run("without_post_build_hooks", func(t *testing.T) {
			called := false
			builder := newAppTestBuilder(table.isFrontend, table.serverType, table.serverMode, table.serverMetadata, *builderConfig)
			builder.AddAcceptor(acc)
			app := builder.Build()

			assert.False(t, called)
			assert.NotNil(t, app)
		})
	}
}
