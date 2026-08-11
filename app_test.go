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
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/pitaya/v2/session"

	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/router"
	"github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/alkaid/pitaya/v2/timer"
	"github.com/alkaid/viper"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/types/descriptorpb"
)

const appSubmissionErrorHelperEnv = "PITAYA_TEST_APP_SUBMISSION_ERROR"

var (
	tables = []struct {
		isFrontend     bool
		serverType     string
		serverMode     ServerMode
		serverMetadata map[string]string
		cfg            *viper.Viper
	}{
		{true, "sv1", Cluster, map[string]string{"name": "bla"}, viper.New()},
		{false, "sv2", Standalone, map[string]string{}, viper.New()},
	}
)

type backendCoordinatorTestPool struct {
	session.SessionPool
}

func (*backendCoordinatorTestPool) SetBackendCoordinator(session.BackendCoordinator) {}

func newAppTestBuilder(
	isFrontend bool,
	serverType string,
	serverMode ServerMode,
	serverMetadata map[string]string,
	pitayaConfig config.PitayaConfig,
) *Builder {
	return NewDefaultBuilder(isFrontend, serverType, serverMode, serverMetadata, pitayaConfig)
}

func newAppTestApp(
	isFrontend bool,
	serverType string,
	serverMode ServerMode,
	serverMetadata map[string]string,
	pitayaConfig config.PitayaConfig,
) Pitaya {
	return newAppTestBuilder(isFrontend, serverType, serverMode, serverMetadata, pitayaConfig).Build()
}

func TestMain(m *testing.M) {
	exit := m.Run()
	os.Exit(exit)
}

func TestNewApp(t *testing.T) {
	for _, table := range tables {
		t.Run(table.serverType, func(t *testing.T) {
			builder := NewBuilderWithConfigs(
				table.isFrontend,
				table.serverType,
				table.serverMode,
				table.serverMetadata,
				config.NewConfig(table.cfg),
			)
			app := builder.Build().(*App)
			assert.Equal(t, table.isFrontend, app.server.Frontend)
			assert.Equal(t, table.serverType, app.server.Type)
			assert.Equal(t, table.serverMode, app.serverMode)
			assert.Equal(t, table.serverMetadata, app.server.Metadata)
		})
	}
}

func TestAddAcceptor(t *testing.T) {
	acc := acceptor.NewTCPAcceptor("0.0.0.0:0")
	for _, table := range tables {
		t.Run(table.serverType, func(t *testing.T) {
			builderConfig := config.NewDefaultPitayaConfig()
			builder := newAppTestBuilder(table.isFrontend, table.serverType, table.serverMode, table.serverMetadata, *builderConfig)
			builder.AddAcceptor(acc)
			app := builder.Build().(*App)
			if table.isFrontend {
				assert.Equal(t, acc, app.acceptors[0])
			} else {
				assert.Equal(t, 0, len(app.acceptors))
			}
		})
	}
}

func TestGetDieChan(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	assert.Equal(t, app.dieChan, app.GetDieChan())
}

func TestGetSever(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	assert.Equal(t, app.server, app.GetServer())
}

func TestGetMetricsReporters(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	assert.Equal(t, app.metricsReporters, app.GetMetricsReporters())
}
func TestGetServerByID(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig)
	s, err := app.GetServerByID("id")
	assert.Nil(t, s)
	assert.ErrorIs(t, err, constants.ErrNoServerWithID)
}

func TestGetServersByType(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig)
	s, err := app.GetServersByType("id")
	assert.Nil(t, s)
	assert.EqualError(t, constants.ErrNoServersAvailableOfType, err.Error())
}

func TestSetHeartbeatInterval(t *testing.T) {
	inter := 35 * time.Millisecond
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	app.SetHeartbeatTime(inter)
	assert.Equal(t, inter, app.heartbeat)
}

func TestInitSysRemotes(t *testing.T) {
	ctrl := gomock.NewController(t)
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().OnSessionBind(gomock.Any()).Times(1)
	sessionPool.EXPECT().OnSessionClose(gomock.Any()).Times(1)

	app := newTestAppForComponents(&backendCoordinatorTestPool{SessionPool: sessionPool})
	app.initSysRemotes()
	assert.Empty(t, app.handlerComp)
	assert.Len(t, app.remoteComp, 1)
	app.startupComponents()
}

func TestSetDictionary(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)

	dict := map[string]uint16{"someroute": 12}
	err := app.SetDictionary(dict)
	assert.NoError(t, err)
	assert.Equal(t, dict, message.GetDictionary())

	app.running = true
	err = app.SetDictionary(dict)
	assert.EqualError(t, constants.ErrChangeDictionaryWhileRunning, err.Error())
}

func TestAddRoute(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	app.router = nil
	err := app.AddRoute("somesv", func(ctx context.Context, route *route.Route, payload []byte, servers map[string]*cluster.Server, session session.Session) (*cluster.Server, error) {
		return nil, nil
	})
	assert.EqualError(t, constants.ErrRouterNotInitialized, err.Error())

	app.router = router.New()
	err = app.AddRoute("somesv", func(ctx context.Context, route *route.Route, payload []byte, servers map[string]*cluster.Server, session session.Session) (*cluster.Server, error) {
		return nil, nil
	})
	assert.NoError(t, err)

	app.running = true
	err = app.AddRoute("somesv", func(ctx context.Context, route *route.Route, payload []byte, servers map[string]*cluster.Server, session session.Session) (*cluster.Server, error) {
		return nil, nil
	})
	assert.EqualError(t, constants.ErrChangeRouteWhileRunning, err.Error())
}

func TestShutdown(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	go func() {
		app.Shutdown()
	}()
	<-app.dieChan
}

func TestConfigureDefaultMetricsReporter(t *testing.T) {
	tables := []struct {
		enabled bool
	}{
		{true},
		{false},
	}

	for _, table := range tables {
		t.Run(fmt.Sprintf("%t", table.enabled), func(t *testing.T) {
			builderConfig := config.NewDefaultPitayaConfig()
			builderConfig.Metrics.Prometheus.Enabled = table.enabled
			builderConfig.Metrics.Statsd.Enabled = table.enabled
			app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
			// if statsd is enabled there are 2 metricsReporters, prometheus and statsd
			assert.Equal(t, table.enabled, len(app.metricsReporters) == 2)
		})
	}
}

func TestDefaultSD(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	assert.NotNil(t, app.serviceDiscovery)

	etcdSD, err := cluster.NewEtcdServiceDiscovery(config.NewDefaultPitayaConfig().Cluster.SD.Etcd, app.server, app.dieChan)
	assert.NoError(t, err)
	typeOfetcdSD := reflect.TypeOf(etcdSD)

	assert.Equal(t, typeOfetcdSD, reflect.TypeOf(app.serviceDiscovery))
}

func TestDefaultRPCServer(t *testing.T) {
	ctrl := gomock.NewController(t)

	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	assert.NotNil(t, app.rpcServer)

	sessionPool := mocks.NewMockSessionPool(ctrl)

	natsRPCServer, err := cluster.NewNatsRPCServer(config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats, app.server, nil, app.dieChan, sessionPool)
	assert.NoError(t, err)
	typeOfNatsRPCServer := reflect.TypeOf(natsRPCServer)

	assert.Equal(t, typeOfNatsRPCServer, reflect.TypeOf(app.rpcServer))
}

func TestDefaultRPCClient(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	assert.NotNil(t, app.rpcClient)

	natsRPCClient, err := cluster.NewNatsRPCClient(config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats, app.server, nil, app.dieChan)
	assert.NoError(t, err)
	typeOfNatsRPCClient := reflect.TypeOf(natsRPCClient)

	assert.Equal(t, typeOfNatsRPCClient, reflect.TypeOf(app.rpcClient))
}

func TestStartAndListenStandalone(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()

	acc := acceptor.NewTCPAcceptor("0.0.0.0:0")
	builder := newAppTestBuilder(true, "testtype", Standalone, map[string]string{}, *builderConfig)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)

	go func() {
		app.Start()
	}()
	helpers.ShouldEventuallyReturn(t, func() bool {
		return app.running
	}, true)

	assert.NotNil(t, app.handlerService)
	assert.NotNil(t, timer.GlobalTicker)
	// should be listening
	assert.NotEmpty(t, acc.GetAddr())
	helpers.ShouldEventuallyReturn(t, func() error {
		n, err := net.Dial("tcp", acc.GetAddr())
		defer n.Close()
		return err
	}, nil, 10*time.Millisecond, 100*time.Millisecond)
}

func TestStartAndListenCluster(t *testing.T) {
	es, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { es.Terminate(t) })

	ns := helpers.GetTestNatsServer(t)
	nsAddr := ns.Addr().String()

	builder := newAppTestBuilder(true, "testtype", Cluster, map[string]string{}, *config.NewDefaultPitayaConfig())

	var err error
	natsClientConfig := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	natsClientConfig.Connect = fmt.Sprintf("nats://%s", nsAddr)
	builder.RPCClient, err = cluster.NewNatsRPCClient(natsClientConfig, builder.Server, builder.MetricsReporters, builder.DieChan)
	if err != nil {
		panic(err.Error())
	}

	natsServerConfig := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	natsServerConfig.Connect = fmt.Sprintf("nats://%s", nsAddr)
	builder.RPCServer, err = cluster.NewNatsRPCServer(natsServerConfig, builder.Server, builder.MetricsReporters, builder.DieChan, builder.SessionPool)
	if err != nil {
		panic(err.Error())
	}

	etcdSD, err := cluster.NewEtcdServiceDiscovery(config.NewDefaultPitayaConfig().Cluster.SD.Etcd, builder.Server, builder.DieChan, cli)
	builder.ServiceDiscovery = etcdSD
	assert.NoError(t, err)
	acc := acceptor.NewTCPAcceptor("0.0.0.0:0")
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)

	appDone := make(chan struct{})
	go func() {
		defer close(appDone)
		app.Start()
	}()
	t.Cleanup(func() {
		app.Shutdown()
		select {
		case <-appDone:
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for app shutdown")
		}
	})
	helpers.ShouldEventuallyReturn(t, func() bool {
		return app.running
	}, true)

	assert.NotNil(t, app.handlerService)
	assert.NotNil(t, timer.GlobalTicker)
	// should be listening
	assert.NotEmpty(t, acc.GetAddr())
	helpers.ShouldEventuallyReturn(t, func() error {
		n, err := net.Dial("tcp", acc.GetAddr())
		defer n.Close()
		return err
	}, nil, 10*time.Millisecond, 100*time.Millisecond)
}

func TestGetSessionFromCtx(t *testing.T) {
	ctrl := gomock.NewController(t)
	ss := mocks.NewMockSession(ctrl)

	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *config.NewDefaultPitayaConfig())
	ctx := context.WithValue(context.Background(), constants.SessionCtxKey, ss)
	s := app.GetSessionFromCtx(ctx)
	assert.Equal(t, ss, s)
}

func TestAddMetricTagsToPropagateCtx(t *testing.T) {
	ctx := AddMetricTagsToPropagateCtx(context.Background(), map[string]string{
		"key": "value",
	})
	val := ctx.Value(constants.PropagateCtxKey)
	assert.Equal(t, map[string]interface{}{
		constants.MetricTagsKey: map[string]string{
			"key": "value",
		},
	}, val)
}

func TestAddToPropagateCtx(t *testing.T) {
	ctx := AddToPropagateCtx(context.Background(), "key", "val")
	val := ctx.Value(constants.PropagateCtxKey)
	assert.Equal(t, map[string]interface{}{"key": "val"}, val)
}

func TestGetFromPropagateCtx(t *testing.T) {
	ctx := AddToPropagateCtx(context.Background(), "key", "val")
	val := GetFromPropagateCtx(ctx, "key")
	assert.Equal(t, "val", val)
}

func TestExtractSpan(t *testing.T) {
	want := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1},
		SpanID:  trace.SpanID{2},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), want)
	extractedCtx, got := ExtractSpan(ctx)
	assert.Equal(t, want, got)
	assert.Equal(t, want, trace.SpanContextFromContext(extractedCtx))
}

func TestDescriptor(t *testing.T) {
	descriptors := make(map[string]*descriptorpb.FileDescriptorProto)
	err := Descriptor("protos.KickMsg", descriptors)
	assert.NoError(t, err)
	assert.NotEmpty(t, descriptors)

	err = Descriptor("kick.proto", descriptors)
	assert.EqualError(t, constants.ErrProtodescriptor, err.Error())
}

func expectedSysDocumentation(pointerNames bool) map[string]interface{} {
	type remoteTypes struct {
		input  string
		output string
	}

	specs := map[string]remoteTypes{
		"bindbackendsession":      {"*protos.BindBackendMsg", "*protos.Response"},
		"bindsession":             {"*protos.BindMsg", "*protos.Response"},
		"kick":                    {"*protos.KickMsg", "*protos.KickAnswer"},
		"kickbackend":             {"*protos.BindBackendMsg", "*protos.Response"},
		"pushsession":             {"*protos.Session", "*protos.Response"},
		"sessionbound":            {"*protos.BindMsg", "*protos.Response"},
		"sessionboundbackend":     {"*protos.BindBackendMsg", "*protos.Response"},
		"sessionboundbackendfork": {"*protos.BindBackendMsg", "*protos.Response"},
		"sessionboundfork":        {"*protos.BindMsg", "*protos.Response"},
		"sessionclosed":           {"*protos.KickMsg", "*protos.Response"},
		"sessionkickedbackend":    {"*protos.BindBackendMsg", "*protos.Response"},
	}
	remotes := make(map[string]interface{}, len(specs))
	for route, types := range specs {
		input := map[string]interface{}{}
		output := map[string]interface{}{}
		if pointerNames {
			input[types.input] = map[string]interface{}{}
			output[types.output] = map[string]interface{}{}
		}
		remotes["testtype.sys."+route] = map[string]interface{}{
			"input":  input,
			"output": []interface{}{output},
		}
	}
	return map[string]interface{}{
		"handlers": map[string]interface{}{},
		"remotes":  remotes,
	}
}

func TestDocumentation(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	app.startupComponents()
	doc, err := app.Documentation(false)
	assert.NoError(t, err)
	assert.Equal(t, expectedSysDocumentation(false), doc)
}

func TestDocumentationTrue(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	app.startupComponents()
	doc, err := app.Documentation(true)
	assert.NoError(t, err)
	assert.Equal(t, expectedSysDocumentation(true), doc)
}

func TestAddGRPCInfoToMetadata(t *testing.T) {
	t.Parallel()

	metadata := map[string]string{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}

	metadata = AddGRPCInfoToMetadata(metadata, "region", "host", "port", "external-host", "external-port")

	assert.Equal(t, map[string]string{
		"key1":                        "value1",
		"key2":                        "value2",
		"key3":                        "value3",
		constants.GRPCHostKey:         "host",
		constants.GRPCPortKey:         "port",
		constants.GRPCExternalHostKey: "external-host",
		constants.GRPCExternalPortKey: "external-port",
		constants.RegionKey:           "region",
	}, metadata)
}

func TestStartWorker(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)

	app.StartWorker()
	assert.True(t, app.worker.Started())
}

func TestAppStatelessStartupFailsFast(t *testing.T) {
	if os.Getenv(appSubmissionErrorHelperEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAppStatelessStartupFailsFast$")
		cmd.Env = append(os.Environ(), appSubmissionErrorHelperEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app submission helper failed: %v\n%s", err, output)
		}
		return
	}

	cfg := config.NewDefaultPitayaConfig()
	cfg.Concurrency.Handler.Dispatch = 0
	acc := acceptor.NewTCPAcceptor("127.0.0.1:0")
	builder := newAppTestBuilder(true, "testtype", Standalone, map[string]string{}, *cfg)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)
	comp := &MyComp{}
	app.Register(comp)
	recoverError := func(call func()) error {
		var panicValue any
		func() {
			defer func() { panicValue = recover() }()
			call()
		}()
		panicErr, ok := panicValue.(error)
		if !ok {
			t.Fatalf("panic = %v, want error", panicValue)
		}
		return panicErr
	}

	ants.Release()
	assert.True(t, errors.Is(recoverError(app.Start), ants.ErrPoolClosed))
	assert.False(t, app.running)
	assert.False(t, acc.IsRunning())
	assert.False(t, comp.running)
	assert.Equal(t, 1, comp.initCount)
	assert.Equal(t, 1, comp.afterInitCount)
	assert.Equal(t, 1, comp.beforeShutdownCount)
	assert.Equal(t, 1, comp.shutdownCount)

	assert.True(t, errors.Is(recoverError(app.StartWorker), ants.ErrPoolClosed))
	assert.False(t, app.worker.Started())
}

func TestRegisterRPCJob(t *testing.T) {
	t.Run("register_once", func(t *testing.T) {
		builderConfig := config.NewDefaultPitayaConfig()
		app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig)
		app.StartWorker()

		err := app.RegisterRPCJob(nil)
		assert.NoError(t, err)
	})

	t.Run("register_twice", func(t *testing.T) {
		builderConfig := config.NewDefaultPitayaConfig()
		app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig)
		app.StartWorker()

		err := app.RegisterRPCJob(nil)
		assert.NoError(t, err)

		err = app.RegisterRPCJob(nil)
		assert.Equal(t, constants.ErrRPCJobAlreadyRegistered, err)
	})
}
