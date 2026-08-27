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
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/pitaya/v2/session"

	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/interfaces"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/metrics/models"
	acceptormocks "github.com/alkaid/pitaya/v2/mocks"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/router"
	"github.com/alkaid/pitaya/v2/service"
	"github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/alkaid/pitaya/v2/timer"
	"github.com/alkaid/viper"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

type forceCloseTrackingPool struct {
	session.SessionPool
	forceCloseCalled atomic.Bool
}

func (p *forceCloseTrackingPool) ForceCloseAll() {
	p.forceCloseCalled.Store(true)
	p.SessionPool.ForceCloseAll()
}

type unexpectedExitAcceptor struct {
	ready    chan error
	connChan chan acceptor.PlayerConn
}

type readyGateAcceptor struct {
	name             string
	connChan         chan acceptor.PlayerConn
	readyGate        chan struct{}
	readyDelivered   chan struct{}
	serveStarted     chan struct{}
	releaseReadyOnce sync.Once
	connChanCalls    atomic.Int32
}

type initOrderAcceptor struct {
	ready        chan error
	serveChecked chan error
	connChan     chan acceptor.PlayerConn
	check        func() error
}

type blockingStopAcceptor struct {
	*unexpectedExitAcceptor
	stopStarted chan struct{}
	releaseStop chan struct{}
	stopDone    chan struct{}
}

type readyMarkerObserverModule struct {
	MyMod
	zapLogger *zap.Logger
}

type LifecycleBarrierComponent struct {
	MyComp
	initEntered chan struct{}
	releaseInit chan struct{}
}

func (component *LifecycleBarrierComponent) Init() {
	close(component.initEntered)
	<-component.releaseInit
	component.MyComp.Init()
}

type FailingLifecycleBarrierComponent struct {
	MyComp
	initEntered chan struct{}
	releaseInit chan struct{}
	failure     error
}

func (component *FailingLifecycleBarrierComponent) Init() {
	close(component.initEntered)
	<-component.releaseInit
	panic(component.failure)
}

type ShutdownBarrierComponent struct {
	MyComp
	beforeShutdownEntered chan struct{}
	releaseShutdown       chan struct{}
}

func (component *ShutdownBarrierComponent) BeforeShutdown() {
	close(component.beforeShutdownEntered)
	<-component.releaseShutdown
	component.MyComp.BeforeShutdown()
}

func (m *readyMarkerObserverModule) AfterInit() {
	logger.Zap = m.zapLogger
}

func newReadyGateAcceptor(name string) *readyGateAcceptor {
	return &readyGateAcceptor{
		name:           name,
		connChan:       make(chan acceptor.PlayerConn),
		readyGate:      make(chan struct{}),
		readyDelivered: make(chan struct{}),
		serveStarted:   make(chan struct{}),
	}
}

func newInitOrderAcceptor(check func() error) *initOrderAcceptor {
	return &initOrderAcceptor{
		ready:        make(chan error, 1),
		serveChecked: make(chan error, 1),
		connChan:     make(chan acceptor.PlayerConn),
		check:        check,
	}
}

func (a *initOrderAcceptor) Serve(ctx context.Context) error {
	err := a.check()
	a.serveChecked <- err
	close(a.serveChecked)
	a.ready <- err
	close(a.ready)
	if err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func (a *initOrderAcceptor) Ready() <-chan error                   { return a.ready }
func (*initOrderAcceptor) Stop()                                   {}
func (*initOrderAcceptor) GetAddr() string                         { return "init-order" }
func (a *initOrderAcceptor) GetConnChan() chan acceptor.PlayerConn { return a.connChan }
func (*initOrderAcceptor) EnableProxyProtocol()                    {}
func (*initOrderAcceptor) IsRunning() bool                         { return true }
func (*initOrderAcceptor) GetConfiguredAddress() string            { return "init-order" }

func (a *readyGateAcceptor) releaseReady() {
	a.releaseReadyOnce.Do(func() {
		close(a.readyGate)
	})
}

func (a *readyGateAcceptor) Serve(ctx context.Context) error {
	close(a.serveStarted)
	<-ctx.Done()
	return nil
}

func (a *readyGateAcceptor) Ready() <-chan error {
	ready := make(chan error)
	go func() {
		<-a.readyGate
		ready <- nil
		close(ready)
		close(a.readyDelivered)
	}()
	return ready
}

func (*readyGateAcceptor) Stop()             {}
func (a *readyGateAcceptor) GetAddr() string { return a.name }
func (a *readyGateAcceptor) GetConnChan() chan acceptor.PlayerConn {
	a.connChanCalls.Add(1)
	return a.connChan
}
func (*readyGateAcceptor) EnableProxyProtocol()           {}
func (*readyGateAcceptor) IsRunning() bool                { return true }
func (a *readyGateAcceptor) GetConfiguredAddress() string { return a.name }

func newUnexpectedExitAcceptor() *unexpectedExitAcceptor {
	return &unexpectedExitAcceptor{
		ready:    make(chan error, 1),
		connChan: make(chan acceptor.PlayerConn),
	}
}

func (a *unexpectedExitAcceptor) Serve(context.Context) error {
	a.ready <- nil
	return nil
}

func (a *unexpectedExitAcceptor) Ready() <-chan error                   { return a.ready }
func (*unexpectedExitAcceptor) Stop()                                   {}
func (*unexpectedExitAcceptor) GetAddr() string                         { return "" }
func (a *unexpectedExitAcceptor) GetConnChan() chan acceptor.PlayerConn { return a.connChan }
func (*unexpectedExitAcceptor) EnableProxyProtocol()                    {}
func (*unexpectedExitAcceptor) IsRunning() bool                         { return false }
func (*unexpectedExitAcceptor) GetConfiguredAddress() string            { return "early-exit" }

func (a *blockingStopAcceptor) Stop() {
	close(a.stopStarted)
	defer close(a.stopDone)
	<-a.releaseStop
}

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
	sessionPool.EXPECT().OnAfterSessionBind(gomock.Any()).Times(1)
	sessionPool.EXPECT().OnSessionClose(gomock.Any()).Times(1)

	app := newTestAppForComponents(&backendCoordinatorTestPool{SessionPool: sessionPool})
	app.initSysRemotes()
	assert.Empty(t, app.handlerComp)
	assert.Len(t, app.remoteComp, 1)
	require.NoError(t, app.startupComponents())
}

func TestSetDictionary(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)

	sequence := appDictionarySequence.Add(1)
	route := fmt.Sprintf("someroute-%d", sequence)
	code := uint16(50000 + sequence)
	dict := map[string]uint16{route: code}
	expected := message.GetDictionary()
	if expected == nil {
		expected = make(map[string]uint16)
	}
	expected[route] = code
	err := app.SetDictionary(dict)
	assert.NoError(t, err)
	assert.Equal(t, expected, message.GetDictionary())

	app.lifecycleState.Store(uint32(appStateStarting))
	err = app.SetDictionary(dict)
	assert.ErrorIs(t, err, constants.ErrAppConfigurationLocked)
}

var appDictionarySequence atomic.Uint32

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

	app.lifecycleState.Store(uint32(appStateStarting))
	err = app.AddRoute("somesv", func(ctx context.Context, route *route.Route, payload []byte, servers map[string]*cluster.Server, session session.Session) (*cluster.Server, error) {
		return nil, nil
	})
	assert.ErrorIs(t, err, constants.ErrAppConfigurationLocked)
}

func TestShutdown(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			app.Shutdown()
		}()
	}
	wg.Wait()

	select {
	case <-app.stopCh:
	case <-time.After(time.Second):
		t.Fatal("shutdown signal was not published")
	}
}

func TestShutdownToleratesClosedLegacyDieChannel(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
	close(app.dieChan)
	assert.NotPanics(t, app.Shutdown)
}

func TestAppConfigurationAdmissionByLifecycleState(t *testing.T) {
	states := []appLifecycleState{
		appStateCreated,
		appStateStarting,
		appStateRunning,
		appStateStopping,
		appStateStopped,
	}
	for _, state := range states {
		t.Run(state.String(), func(t *testing.T) {
			app := newTestAppForComponents(nil)
			app.router = router.New()
			app.modulesMap = make(map[string]interfaces.Module)
			app.lifecycleState.Store(uint32(state))

			assertResult := func(err error) {
				t.Helper()
				if state == appStateCreated {
					require.NoError(t, err)
					return
				}
				require.ErrorIs(t, err, constants.ErrAppConfigurationLocked)
			}

			assertResult(app.Register(&MyComp{}, component.WithName("handler")))
			assertResult(app.RegisterRemote(&MyComp{}, component.WithName("remote")))
			assertResult(app.RegisterSubscribe(&MyComp{}, component.WithName("subscriber")))
			assertResult(app.RegisterModule(&MyMod{}, "module"))
			assertResult(app.OnStarted(func() {}))
			assertResult(app.AddRoute("backend", nil))
		})
	}
}

func TestAppRegisterAndStartShareOneAdmissionDecision(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		cfg := config.NewDefaultPitayaConfig()
		app := newAppTestApp(false, "registration-race", Standalone, nil, *cfg).(*App)
		registered := &MyComp{}
		registerDone := make(chan error, 1)
		startDone := make(chan any, 1)

		app.configurationMu.Lock()
		go func() {
			registerDone <- app.Register(registered, component.WithName("race"))
		}()
		go func() {
			defer func() { startDone <- recover() }()
			app.Start()
		}()
		app.configurationMu.Unlock()

		registerErr := <-registerDone
		require.Eventually(t, app.IsRunning, 5*time.Second, time.Millisecond)
		app.Shutdown()
		select {
		case recovered := <-startDone:
			require.Nil(t, recovered)
		case <-time.After(5 * time.Second):
			t.Fatal("App did not finish after registration race")
		}

		if registerErr == nil {
			assert.Equal(t, 1, registered.initCount)
			assert.Equal(t, 1, registered.beforeShutdownCount)
			assert.Equal(t, 1, registered.shutdownCount)
		} else {
			require.ErrorIs(t, registerErr, constants.ErrAppConfigurationLocked)
			assert.Zero(t, registered.initCount)
			assert.Zero(t, registered.beforeShutdownCount)
			assert.Zero(t, registered.shutdownCount)
		}
	}
}

func TestAppConcurrentStartAndShutdownDuringStarting(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	app := newAppTestApp(false, "lifecycle-race", Standalone, nil, *cfg).(*App)
	barrier := &LifecycleBarrierComponent{
		initEntered: make(chan struct{}),
		releaseInit: make(chan struct{}),
	}
	require.NoError(t, app.Register(barrier, component.WithName("barrier")))

	firstStart := make(chan any, 1)
	go func() {
		defer func() { firstStart <- recover() }()
		app.Start()
	}()
	select {
	case <-barrier.initEntered:
	case recovered := <-firstStart:
		t.Fatalf("App.Start exited before component barrier: %v", recovered)
	case <-time.After(time.Second):
		t.Fatal("component Init did not reach barrier")
	}
	require.Equal(t, appStateStarting, app.state())

	secondStart := make(chan any, 1)
	go func() {
		defer func() { secondStart <- recover() }()
		app.Start()
	}()
	select {
	case recovered := <-secondStart:
		require.ErrorIs(t, recovered.(error), constants.ErrAppAlreadyStarted)
	case <-time.After(time.Second):
		t.Fatal("concurrent Start did not fail")
	}

	shutdownDone := make(chan struct{})
	go func() {
		app.Shutdown()
		close(shutdownDone)
	}()
	require.Eventually(t, func() bool {
		app.configurationMu.Lock()
		defer app.configurationMu.Unlock()
		return app.stopRequested
	}, time.Second, time.Millisecond)

	close(barrier.releaseInit)
	select {
	case recovered := <-firstStart:
		require.Nil(t, recovered)
	case <-time.After(5 * time.Second):
		t.Fatal("startup owner did not finish rollback")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not join startup rollback")
	}

	require.Equal(t, appStateStopped, app.state())
	assert.Equal(t, 1, barrier.initCount)
	assert.Equal(t, 1, barrier.beforeShutdownCount)
	assert.Equal(t, 1, barrier.shutdownCount)
	assert.ErrorIs(t, app.Register(&MyComp{}), constants.ErrAppConfigurationLocked)
	assert.PanicsWithValue(t, constants.ErrAppAlreadyStarted, app.Start)
}

func TestAppStartupFailureAndShutdownBeforeStartAreTerminal(t *testing.T) {
	t.Run("startup failure", func(t *testing.T) {
		cfg := config.NewDefaultPitayaConfig()
		app := newAppTestApp(true, "invalid-start", Standalone, nil, *cfg).(*App)
		assert.Panics(t, app.Start)
		require.Equal(t, appStateStopped, app.state())
		assert.PanicsWithValue(t, constants.ErrAppAlreadyStarted, app.Start)
	})

	t.Run("shutdown before start", func(t *testing.T) {
		cfg := config.NewDefaultPitayaConfig()
		app := newAppTestApp(false, "shutdown-before-start", Standalone, nil, *cfg).(*App)
		app.Shutdown()
		require.Equal(t, appStateStopped, app.state())
		assert.PanicsWithValue(t, constants.ErrAppAlreadyStarted, app.Start)
		assert.NotPanics(t, app.Shutdown)
	})
}

func TestAppStartupFailureIsNotMaskedByConcurrentShutdown(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	app := newAppTestApp(false, "startup-failure-shutdown", Standalone, nil, *cfg).(*App)
	initialized := &MyComp{}
	startupFailure := errors.New("startup failure")
	failing := &FailingLifecycleBarrierComponent{
		initEntered: make(chan struct{}),
		releaseInit: make(chan struct{}),
		failure:     startupFailure,
	}
	require.NoError(t, app.Register(initialized, component.WithName("initialized")))
	require.NoError(t, app.Register(failing, component.WithName("failing")))

	startDone := make(chan any, 1)
	go func() {
		defer func() { startDone <- recover() }()
		app.Start()
	}()
	select {
	case <-failing.initEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("failing component did not reach Init barrier")
	}

	shutdownDone := make(chan struct{})
	go func() {
		app.Shutdown()
		close(shutdownDone)
	}()
	require.Eventually(t, func() bool {
		app.configurationMu.Lock()
		defer app.configurationMu.Unlock()
		return app.stopRequested
	}, time.Second, time.Millisecond)
	close(failing.releaseInit)

	select {
	case recovered := <-startDone:
		require.ErrorIs(t, recovered.(error), startupFailure)
	case <-time.After(5 * time.Second):
		t.Fatal("startup owner did not surface the initialization failure")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not join startup rollback")
	}
	require.Equal(t, appStateStopped, app.state())
	assert.Equal(t, 1, initialized.initCount)
	assert.Equal(t, 1, initialized.beforeShutdownCount)
	assert.Equal(t, 1, initialized.shutdownCount)
}

func TestAppOnStartedCanShutdownSynchronously(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	app := newAppTestApp(false, "on-started-shutdown", Standalone, nil, *cfg).(*App)
	callbackDone := make(chan struct{})
	require.NoError(t, app.OnStarted(func() {
		app.Shutdown()
		close(callbackDone)
	}))

	startDone := make(chan any, 1)
	go func() {
		defer func() { startDone <- recover() }()
		app.Start()
	}()

	select {
	case <-callbackDone:
	case <-time.After(5 * time.Second):
		t.Fatal("OnStarted callback deadlocked in Shutdown")
	}
	select {
	case recovered := <-startDone:
		require.Nil(t, recovered)
	case <-time.After(5 * time.Second):
		t.Fatal("App did not finish teardown after OnStarted requested shutdown")
	}
	require.Equal(t, appStateStopped, app.state())
}

func TestAppShutdownDuringActiveOnStartedReturnsAfterSignaling(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	app := newAppTestApp(false, "on-started-external-shutdown", Standalone, nil, *cfg).(*App)
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseCallback:
		default:
			close(releaseCallback)
		}
	})
	require.NoError(t, app.OnStarted(func() {
		close(callbackStarted)
		<-releaseCallback
	}))

	startDone := make(chan any, 1)
	go func() {
		defer func() { startDone <- recover() }()
		app.Start()
	}()
	select {
	case <-callbackStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("OnStarted callback did not begin")
	}

	shutdownDone := make(chan struct{})
	go func() {
		app.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown waited for an active OnStarted callback")
	}
	assert.Equal(t, appStateStopping, app.state())
	select {
	case <-startDone:
		t.Fatal("Start returned before the active OnStarted callback exited")
	default:
	}

	close(releaseCallback)
	select {
	case recovered := <-startDone:
		require.Nil(t, recovered)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not finish teardown after the OnStarted callback exited")
	}
	require.Equal(t, appStateStopped, app.state())
}

func TestAppConcurrentShutdownHasOneTeardownOwner(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	app := newAppTestApp(false, "concurrent-shutdown", Standalone, nil, *cfg).(*App)
	barrier := &ShutdownBarrierComponent{
		beforeShutdownEntered: make(chan struct{}),
		releaseShutdown:       make(chan struct{}),
	}
	require.NoError(t, app.Register(barrier, component.WithName("shutdown-barrier")))

	startDone := make(chan any, 1)
	go func() {
		defer func() { startDone <- recover() }()
		app.Start()
	}()
	require.Eventually(t, app.IsRunning, 5*time.Second, time.Millisecond)

	const shutdownCalls = 32
	var shutdownWG sync.WaitGroup
	shutdownWG.Add(shutdownCalls)
	allShutdownDone := make(chan struct{})
	for range shutdownCalls {
		go func() {
			defer shutdownWG.Done()
			app.Shutdown()
		}()
	}
	go func() {
		shutdownWG.Wait()
		close(allShutdownDone)
	}()

	select {
	case <-barrier.beforeShutdownEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown owner did not reach component barrier")
	}
	select {
	case <-allShutdownDone:
		t.Fatal("concurrent Shutdown returned before lifecycle completion")
	default:
	}
	close(barrier.releaseShutdown)
	select {
	case <-allShutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Shutdown callers did not join lifecycle completion")
	}
	select {
	case recovered := <-startDone:
		require.Nil(t, recovered)
	case <-time.After(5 * time.Second):
		t.Fatal("Start owner did not observe completed shutdown")
	}

	require.Equal(t, appStateStopped, app.state())
	assert.Equal(t, 1, barrier.initCount)
	assert.Equal(t, 1, barrier.beforeShutdownCount)
	assert.Equal(t, 1, barrier.shutdownCount)
}

func TestInternalDependencyFailureStopsAppWhenLegacyEventIsObserved(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	builder := newAppTestBuilder(false, "dependency-failure", Standalone, nil, *builderConfig)
	app := builder.Build().(*App)
	started := make(chan struct{})
	app.OnStarted(func() { close(started) })

	appDone := make(chan interface{}, 1)
	go func() {
		defer func() { appDone <- recover() }()
		app.Start()
	}()
	defer app.Shutdown()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("App did not start")
	}

	legacyObserved := make(chan bool, 1)
	go func() { legacyObserved <- <-app.GetDieChan() }()
	builder.dependencyDieChan <- true

	select {
	case recovered := <-appDone:
		assert.Nil(t, recovered)
	case <-time.After(5 * time.Second):
		t.Fatal("internal dependency failure did not stop App")
	}
	select {
	case observed := <-legacyObserved:
		assert.True(t, observed)
	case <-time.After(time.Second):
		t.Fatal("internal dependency failure was not mirrored to the legacy channel")
	}
}

func TestLegacyDieChannelStopsApp(t *testing.T) {
	builderConfig := config.NewDefaultPitayaConfig()
	builder := newAppTestBuilder(false, "legacy-die", Standalone, nil, *builderConfig)
	app := builder.Build().(*App)
	started := make(chan struct{})
	app.OnStarted(func() { close(started) })

	appDone := make(chan interface{}, 1)
	go func() {
		defer func() { appDone <- recover() }()
		app.Start()
	}()
	defer app.Shutdown()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("App did not start")
	}
	builder.DieChan <- true

	select {
	case recovered := <-appDone:
		assert.Nil(t, recovered)
	case <-time.After(5 * time.Second):
		t.Fatal("legacy die event did not stop App")
	}
}

func TestStopAcceptorsReportsBlockedDispatcher(t *testing.T) {
	app := &App{dispatchersStopped: true}
	app.acceptCtx, app.cancelAccept = context.WithCancel(context.Background())

	release := make(chan struct{})
	app.dispatcherWG.Add(1)
	go func() {
		defer app.dispatcherWG.Done()
		<-release
	}()

	err := app.stopAcceptorsWithin(20 * time.Millisecond)
	assert.ErrorIs(t, err, ErrDispatcherShutdownTimeout)
	assert.NotErrorIs(t, err, ErrAcceptorShutdownTimeout)

	close(release)
	app.dispatcherWG.Wait()
}

func TestStopAcceptorsReportsBlockedAcceptor(t *testing.T) {
	app := &App{acceptorsStopped: true, dispatchersStopped: true}
	app.acceptCtx, app.cancelAccept = context.WithCancel(context.Background())

	release := make(chan struct{})
	app.acceptorWG.Add(1)
	go func() {
		defer app.acceptorWG.Done()
		<-release
	}()

	err := app.stopAcceptorsWithin(20 * time.Millisecond)
	assert.ErrorIs(t, err, ErrAcceptorShutdownTimeout)
	assert.NotErrorIs(t, err, ErrDispatcherShutdownTimeout)

	close(release)
	app.acceptorWG.Wait()
}

func TestStopAcceptorsReportsBlockingStop(t *testing.T) {
	acc := &blockingStopAcceptor{
		unexpectedExitAcceptor: newUnexpectedExitAcceptor(),
		stopStarted:            make(chan struct{}),
		releaseStop:            make(chan struct{}),
		stopDone:               make(chan struct{}),
	}
	app := &App{
		acceptors:          []acceptor.Acceptor{acc},
		acceptorsStopped:   true,
		dispatchersStopped: true,
	}
	app.acceptCtx, app.cancelAccept = context.WithCancel(context.Background())

	startedAt := time.Now()
	err := app.stopAcceptorsWithin(20 * time.Millisecond)
	assert.ErrorIs(t, err, ErrAcceptorShutdownTimeout)
	assert.NotErrorIs(t, err, ErrDispatcherShutdownTimeout)
	assert.Less(t, time.Since(startedAt), time.Second, "blocking Stop bypassed the shutdown deadline")
	select {
	case <-acc.stopStarted:
	default:
		t.Fatal("Acceptor.Stop was not called")
	}

	close(acc.releaseStop)
	select {
	case <-acc.stopDone:
	case <-time.After(time.Second):
		t.Fatal("blocking Acceptor.Stop did not exit after release")
	}
	app.acceptorWG.Wait()
}

func TestStopConnectionsWaitsForEveryAdmittedHandler(t *testing.T) {
	ctrl := gomock.NewController(t)
	conn := acceptormocks.NewMockPlayerConn(ctrl)
	closeCalled := make(chan struct{})
	conn.EXPECT().Close().DoAndReturn(func() error {
		close(closeCalled)
		return nil
	})

	app := &App{connections: make(map[*trackedPlayerConn]struct{})}
	tracked := &trackedPlayerConn{PlayerConn: conn}
	require.True(t, app.trackConnection(tracked))

	shutdownDone := make(chan struct{})
	go func() {
		app.stopConnectionsAndWait()
		close(shutdownDone)
	}()

	select {
	case <-closeCalled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not close the admitted connection")
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown crossed the handler barrier before the admitted handler exited")
	default:
	}

	late := &trackedPlayerConn{PlayerConn: conn}
	assert.False(t, app.trackConnection(late), "shutdown accepted a late connection handler")
	app.untrackConnection(tracked)
	app.handlerWG.Done()

	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after the admitted handler exited")
	}
}

func TestRuntimeQuiescenceTimeoutSkipsFinalSessionSweep(t *testing.T) {
	pool := &forceCloseTrackingPool{SessionPool: session.NewSessionPool()}
	app := newTestAppForComponents(pool)
	started := make(chan struct{})
	release := make(chan struct{})
	app.OnStarted(func() {
		close(started)
		<-release
	})
	app.startBackgroundTasks()
	<-started

	err := app.quiesceRuntimeBefore(time.Now().Add(20 * time.Millisecond))
	require.ErrorIs(t, err, ErrRuntimeShutdownTimeout)
	assert.False(t, pool.forceCloseCalled.Load(), "timed-out quiescence must not start the final session sweep")

	close(release)
	backgroundDone := make(chan struct{})
	go func() {
		app.backgroundWG.Wait()
		close(backgroundDone)
	}()
	select {
	case <-backgroundDone:
	case <-time.After(time.Second):
		t.Fatal("onStarted callback did not exit after release")
	}
	assert.False(t, pool.forceCloseCalled.Load(), "late barrier completion must not start the final session sweep")
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

func TestPrometheusExporterBindFailurePreventsAppReadiness(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, occupied.Close()) })

	cfg := config.NewDefaultPitayaConfig()
	cfg.Metrics.Prometheus.Enabled = true
	cfg.Metrics.Prometheus.Port = occupied.Addr().(*net.TCPAddr).Port
	acceptorStarted := atomic.Bool{}
	acc := newInitOrderAcceptor(func() error {
		acceptorStarted.Store(true)
		return nil
	})
	builder := newAppTestBuilder(true, "prometheus-bind-failure", Standalone, nil, *cfg)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)
	laterModule := &MyMod{name: "must-not-start"}
	require.NoError(t, app.RegisterModule(laterModule, "must-not-start"))

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		app.Start()
	}()
	require.NotNil(t, recovered)
	require.Contains(t, fmt.Sprint(recovered), "bind prometheus exporter port")
	require.False(t, app.IsRunning())
	require.False(t, laterModule.running)
	require.False(t, acceptorStarted.Load())
	select {
	case <-acc.serveChecked:
		t.Fatal("acceptor Serve ran after required Prometheus bind failed")
	default:
	}
}

func TestPrometheusExporterLifecycleIncludesPrivateAndDefaultRegistries(t *testing.T) {
	port := reserveTCPPort(t)
	defaultCollector := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pitaya_app_default_registry_probe",
		Help: "App Prometheus default registry probe",
	})
	require.NoError(t, prometheus.Register(defaultCollector))
	t.Cleanup(func() { prometheus.Unregister(defaultCollector) })
	defaultCollector.Set(2)

	cfg := config.NewDefaultPitayaConfig()
	cfg.Metrics.Prometheus.Enabled = true
	cfg.Metrics.Prometheus.Port = port
	cfg.Metrics.Custom.Gauges = append(cfg.Metrics.Custom.Gauges, &models.Gauge{
		Subsystem: "test",
		Name:      "app_private_registry_probe",
		Help:      "App Prometheus private registry probe",
	})
	app := newAppTestApp(false, "prometheus-lifecycle", Standalone, nil, *cfg).(*App)
	reporter, ok := app.metricsReporters[0].(*metrics.PrometheusReporter)
	require.True(t, ok)
	require.NoError(t, reporter.ReportGauge("app_private_registry_probe", nil, 1))

	started := make(chan struct{})
	app.OnStarted(func() { close(started) })
	startResult := make(chan any, 1)
	go func() {
		defer func() { startResult <- recover() }()
		app.Start()
	}()
	t.Cleanup(app.Shutdown)
	select {
	case <-started:
	case result := <-startResult:
		t.Fatalf("App.Start exited before Prometheus readiness: %v", result)
	case <-time.After(5 * time.Second):
		t.Fatal("App did not become ready with Prometheus enabled")
	}

	_, exporterPort, err := net.SplitHostPort(app.prometheusExporter.Addr())
	require.NoError(t, err)
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + exporterPort + "/metrics")
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, string(body), "pitaya_test_app_private_registry_probe")
	require.Contains(t, string(body), "pitaya_app_default_registry_probe")

	addr := app.prometheusExporter.Addr()
	app.Shutdown()
	select {
	case result := <-startResult:
		require.Nil(t, result)
	case <-time.After(5 * time.Second):
		t.Fatal("App did not stop its Prometheus exporter")
	}
	app.Shutdown()
	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err, "App Shutdown did not release the Prometheus port")
	require.NoError(t, listener.Close())
}

func reserveTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
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
	externalReady := acc.Ready()

	appDone := make(chan struct{})
	go func() {
		defer close(appDone)
		app.Start()
	}()
	assert.NoError(t, <-externalReady)
	t.Cleanup(func() {
		app.Shutdown()
		select {
		case <-appDone:
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for standalone app shutdown")
		}
	})
	helpers.ShouldEventuallyReturn(t, func() bool {
		return app.IsRunning()
	}, true)

	assert.NotNil(t, app.handlerService)
	assert.NotNil(t, timer.GlobalTicker)
	// should be listening
	assert.NotEmpty(t, acc.GetAddr())
	helpers.ShouldEventuallyReturn(t, func() error {
		n, err := net.Dial("tcp", acc.GetAddr())
		if err == nil {
			defer n.Close()
		}
		return err
	}, nil, 10*time.Millisecond, 100*time.Millisecond)

	addr := acc.GetAddr()
	var shutdowns sync.WaitGroup
	for i := 0; i < 16; i++ {
		shutdowns.Add(1)
		go func() {
			defer shutdowns.Done()
			app.Shutdown()
		}()
	}
	shutdowns.Wait()
	select {
	case <-appDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for standalone app shutdown")
	}

	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("acceptor still accepted connections after App.Start returned")
	}
}

func TestAppIgnoresLegacyHandlerDispatchSettingsAndRunsTimerOwner(t *testing.T) {
	originalPrecision := timer.Precision
	SetTimerPrecision(time.Millisecond)
	t.Cleanup(func() { SetTimerPrecision(originalPrecision) })

	cfg := config.NewDefaultPitayaConfig()
	cfg.Buffer.Handler.LocalProcess = -1
	cfg.Buffer.Handler.RemoteProcess = 999
	cfg.Concurrency.Handler.Dispatch = -99
	app := newAppTestApp(false, "legacy-handler-config", Standalone, nil, *cfg).(*App)
	started := make(chan struct{})
	app.OnStarted(func() { close(started) })
	startResult := make(chan any, 1)
	go func() {
		defer func() { startResult <- recover() }()
		app.Start()
	}()

	select {
	case <-started:
	case result := <-startResult:
		t.Fatalf("App.Start exited before readiness: %v", result)
	case <-time.After(5 * time.Second):
		t.Fatal("App did not start with ignored legacy handler settings")
	}

	fired := make(chan struct{}, 1)
	NewAfterTimer(time.Millisecond, func() { fired <- struct{}{} })
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("the App-managed timer dispatcher did not execute a callback")
	}

	app.Shutdown()
	select {
	case result := <-startResult:
		require.Nil(t, result)
	case <-time.After(5 * time.Second):
		t.Fatal("App did not stop after legacy handler settings test")
	}
}

func TestAppInitializesComponentsAndModulesBeforeStartingAcceptors(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	cfg.Concurrency.Handler.Dispatch = 1
	component := &MyComp{}
	module := &MyMod{name: "init-order"}
	acc := newInitOrderAcceptor(func() error {
		if !component.running {
			return errors.New("component was not initialized before acceptor Serve")
		}
		if !module.running {
			return errors.New("module was not initialized before acceptor Serve")
		}
		return nil
	})

	builder := newAppTestBuilder(true, "init-order", Standalone, nil, *cfg)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)
	app.Register(component)
	require.NoError(t, app.RegisterModule(module, "init-order"))

	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		app.Start()
	}()

	require.NoError(t, <-acc.serveChecked)
	require.Eventually(t, app.IsRunning, time.Second, time.Millisecond)
	app.Shutdown()
	select {
	case panicValue := <-done:
		require.Nil(t, panicValue)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for app shutdown")
	}
}

func capturePanicError(t *testing.T, call func()) error {
	t.Helper()
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		call()
	}()
	if panicValue == nil {
		t.Fatal("call did not panic")
	}
	err, ok := panicValue.(error)
	if !ok {
		t.Fatalf("panic = %#v, want error", panicValue)
	}
	return err
}

func requireAcceptorNotStarted(t *testing.T, acceptor *initOrderAcceptor) {
	t.Helper()
	select {
	case err := <-acceptor.serveChecked:
		t.Fatalf("acceptor Serve unexpectedly ran: %v", err)
	default:
	}
}

func TestValidateStartConfigurationRejectsInvalidServerTopology(t *testing.T) {
	tests := []struct {
		name      string
		server    *cluster.Server
		acceptors []acceptor.Acceptor
		wantText  string
	}{
		{name: "missing server", wantText: "server is not configured"},
		{
			name:      "backend with acceptor",
			server:    &cluster.Server{Frontend: false},
			acceptors: []acceptor.Acceptor{nil},
			wantText:  "acceptors are not allowed on backend servers",
		},
		{
			name:     "frontend without acceptor",
			server:   &cluster.Server{Frontend: true},
			wantText: "frontend servers should have at least one configured acceptor",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := &App{server: test.server, acceptors: test.acceptors}
			require.EqualError(t, app.validateStartConfiguration(), test.wantText)
		})
	}

	require.NoError(t, (&App{server: &cluster.Server{Frontend: false}}).validateStartConfiguration())
}

func TestAppComponentPreflightFailurePreventsAllStartupSideEffects(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	cfg.Concurrency.Handler.Dispatch = 1
	acc := newInitOrderAcceptor(func() error { return nil })
	builder := newAppTestBuilder(true, "component-preflight", Standalone, nil, *cfg)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)
	first := &MyComp{}
	second := &MyComp{}
	module := &LifecycleFaultModule{MyMod: MyMod{name: "user-module"}}
	app.Register(first)
	app.Register(second)
	require.NoError(t, app.RegisterModule(module, "user-module"))

	err := capturePanicError(t, app.Start)
	require.ErrorContains(t, err, "service already defined in startup batch")
	assert.Zero(t, first.initCount)
	assert.Zero(t, second.initCount)
	assert.False(t, module.running)
	assert.False(t, app.IsRunning())
	requireAcceptorNotStarted(t, acc)
}

func TestAppFrameworkModuleConflictFailsBeforeComponentInit(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	cfg.Concurrency.Handler.Dispatch = 1
	acc := newInitOrderAcceptor(func() error { return nil })
	builder := newAppTestBuilder(true, "module-preflight", Standalone, nil, *cfg)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)
	component := &MyComp{}
	app.Register(component)
	require.NoError(t, app.RegisterModule(&MyMod{name: "conflict"}, "statefulGoPool"))

	err := capturePanicError(t, app.Start)
	require.ErrorContains(t, err, "module with name statefulGoPool already exists")
	assert.Zero(t, component.initCount)
	assert.False(t, app.IsRunning())
	requireAcceptorNotStarted(t, acc)
}

func TestAppRemoteComponentWithoutRemoteServiceFailsBeforeInit(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	cfg.Concurrency.Handler.Dispatch = 1
	acc := newInitOrderAcceptor(func() error { return nil })
	builder := newAppTestBuilder(true, "remote-preflight", Standalone, nil, *cfg)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)
	component := &MyComp{}
	app.RegisterRemote(component)

	err := capturePanicError(t, app.Start)
	require.ErrorIs(t, err, service.ErrRemoteServiceUnavailable)
	assert.Zero(t, component.initCount)
	assert.False(t, app.IsRunning())
	requireAcceptorNotStarted(t, acc)
}

func TestAppComponentInitPanicRollsBackBeforeAcceptorStartup(t *testing.T) {
	initErr := errors.New("component init panic")
	cfg := config.NewDefaultPitayaConfig()
	cfg.Concurrency.Handler.Dispatch = 1
	acc := newInitOrderAcceptor(func() error { return nil })
	builder := newAppTestBuilder(true, "component-rollback", Standalone, nil, *cfg)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)
	first := &LifecycleFaultComp{MyComp: MyComp{name: "first"}}
	failing := &LifecycleFaultComp{MyComp: MyComp{name: "failing"}, initPanic: initErr}
	app.Register(first, component.WithName("first"))
	app.Register(failing, component.WithName("failing"))

	err := capturePanicError(t, app.Start)
	require.ErrorIs(t, err, initErr)
	assert.Equal(t, 1, first.beforeShutdownCount)
	assert.Equal(t, 1, first.shutdownCount)
	assert.Zero(t, failing.shutdownCount)
	assert.False(t, failing.running)
	assert.False(t, app.IsRunning())
	requireAcceptorNotStarted(t, acc)
}

func TestAppModuleInitErrorRollsBackComponentsAndInitializedModules(t *testing.T) {
	initErr := errors.New("module init error")
	cleanupErr := errors.New("component shutdown panic")
	cfg := config.NewDefaultPitayaConfig()
	cfg.Concurrency.Handler.Dispatch = 1
	acc := newInitOrderAcceptor(func() error { return nil })
	builder := newAppTestBuilder(true, "module-rollback", Standalone, nil, *cfg)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)
	component := &LifecycleFaultComp{MyComp: MyComp{}, shutdownPanic: cleanupErr}
	module := &LifecycleFaultModule{MyMod: MyMod{name: "failing-module"}, initErr: initErr}
	app.Register(component)
	require.NoError(t, app.RegisterModule(module, "failing-module"))

	err := capturePanicError(t, app.Start)
	require.ErrorIs(t, err, initErr)
	require.ErrorIs(t, err, cleanupErr)
	assert.Equal(t, 1, component.beforeShutdownCount)
	assert.Equal(t, 1, component.shutdownCount)
	assert.Zero(t, module.shutdownCount, "failed Init module owns its partial cleanup")
	assert.False(t, app.IsRunning())
	requireAcceptorNotStarted(t, acc)
}

func TestAppWaitsForAllAcceptorsBeforeStartingConnectionReaders(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	cfg.Concurrency.Handler.Dispatch = 1
	first := newReadyGateAcceptor("first")
	second := newReadyGateAcceptor("second")
	builder := newAppTestBuilder(true, "readiness-order", Standalone, nil, *cfg)
	builder.AddAcceptor(first)
	builder.AddAcceptor(second)
	app := builder.Build().(*App)

	startResult := make(chan any, 1)
	go func() {
		defer func() {
			startResult <- recover()
		}()
		app.Start()
	}()

	var stopOnce sync.Once
	stopApp := func() {
		stopOnce.Do(func() {
			first.releaseReady()
			second.releaseReady()
			app.Shutdown()
			select {
			case panicValue := <-startResult:
				if panicValue != nil {
					t.Errorf("App.Start panicked: %v", panicValue)
				}
			case <-time.After(5 * time.Second):
				t.Error("timed out waiting for app shutdown")
			}
		})
	}
	t.Cleanup(stopApp)

	for _, started := range []<-chan struct{}{first.serveStarted, second.serveStarted} {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("acceptor Serve did not start")
		}
	}

	first.releaseReady()
	select {
	case <-first.readyDelivered:
	case <-time.After(time.Second):
		t.Fatal("first acceptor readiness was not consumed")
	}
	require.Never(t, func() bool {
		return first.connChanCalls.Load() != 0 || second.connChanCalls.Load() != 0
	}, 200*time.Millisecond, 5*time.Millisecond, "connection reader started before every acceptor was ready")
	assert.False(t, app.IsRunning())

	second.releaseReady()
	select {
	case <-second.readyDelivered:
	case <-time.After(time.Second):
		t.Fatal("second acceptor readiness was not consumed")
	}
	require.Eventually(t, func() bool {
		return first.connChanCalls.Load() == 1 && second.connChanCalls.Load() == 1
	}, time.Second, 5*time.Millisecond, "connection readers did not start after all acceptors became ready")
	require.Eventually(t, app.IsRunning, time.Second, 5*time.Millisecond)

	stopApp()
}

func TestAppPublishesReadyMarkerOnlyAfterAcceptorsReady(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	previousLogger := logger.Zap
	t.Cleanup(func() { logger.Zap = previousLogger })

	cfg := config.NewDefaultPitayaConfig()
	cfg.Concurrency.Handler.Dispatch = 1
	acc := newReadyGateAcceptor("ready-marker")
	builder := newAppTestBuilder(true, "ready-marker", Standalone, nil, *cfg)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)
	require.NoError(t, app.RegisterModule(&readyMarkerObserverModule{
		MyMod:     MyMod{name: "ready-marker-observer"},
		zapLogger: zap.New(core),
	}, "ready-marker-observer"))

	startResult := make(chan any, 1)
	go func() {
		defer func() { startResult <- recover() }()
		app.Start()
	}()

	var stopOnce sync.Once
	stopApp := func() {
		stopOnce.Do(func() {
			acc.releaseReady()
			app.Shutdown()
			select {
			case panicValue := <-startResult:
				if panicValue != nil {
					t.Errorf("App.Start panicked: %v", panicValue)
				}
			case <-time.After(5 * time.Second):
				t.Error("timed out waiting for app shutdown")
			}
		})
	}
	t.Cleanup(stopApp)

	select {
	case <-acc.serveStarted:
	case <-time.After(time.Second):
		t.Fatal("acceptor Serve did not start")
	}
	require.Never(t, func() bool {
		return observed.FilterMessage("all modules started!").Len() != 0
	}, 100*time.Millisecond, 5*time.Millisecond, "readiness marker was published before the acceptor was ready")

	acc.releaseReady()
	require.Eventually(t, func() bool {
		return observed.FilterMessage("all modules started!").Len() == 1
	}, time.Second, 5*time.Millisecond, "readiness marker was not published after the acceptor became ready")
	require.True(t, app.IsRunning())

	stopApp()
}

func TestAppStartsAndStopsWSSAcceptor(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	acc := acceptor.NewWSAcceptor(
		"127.0.0.1:0",
		"acceptor/fixtures/server.crt",
		"acceptor/fixtures/server.key",
	)
	builder := newAppTestBuilder(true, "wss", Standalone, nil, *cfg)
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.Start()
	}()
	t.Cleanup(func() {
		app.Shutdown()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for WSS app shutdown")
		}
	})
	helpers.ShouldEventuallyReturn(t, app.IsRunning, true)

	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}} // #nosec G402 -- test fixture certificate
	conn, _, err := dialer.Dial("wss://"+acc.GetAddr(), nil)
	assert.NoError(t, err)
	if conn != nil {
		assert.NoError(t, conn.Close())
	}

	addr := acc.GetAddr()
	app.Shutdown()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WSS app shutdown")
	}
	plainConn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		plainConn.Close()
		t.Fatal("WSS listener remained open after shutdown")
	}
}

func TestAppRejectsAcceptorThatStopsAfterReportingReadiness(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig()
	builder := newAppTestBuilder(true, "early-exit", Standalone, nil, *cfg)
	builder.AddAcceptor(newUnexpectedExitAcceptor())
	app := builder.Build().(*App)
	started := make(chan struct{})
	require.NoError(t, app.OnStarted(func() { close(started) }))

	panicResult := make(chan any, 1)
	go func() {
		defer func() { panicResult <- recover() }()
		app.Start()
	}()

	select {
	case recovered := <-panicResult:
		require.NotNil(t, recovered)
		assert.Contains(t, fmt.Sprint(recovered), "stopped after reporting readiness")
	case <-time.After(5 * time.Second):
		app.Shutdown()
		t.Fatal("App did not reject the stopped acceptor")
	}
	assert.False(t, app.IsRunning())
	select {
	case <-started:
		t.Fatal("OnStarted ran for an acceptor that had already stopped")
	default:
	}
}

func TestSignalStopSerializesWithRunningPublication(t *testing.T) {
	app := &App{stopCh: make(chan struct{})}
	app.configurationMu.Lock()

	entered := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		close(entered)
		app.signalStop()
		close(returned)
	}()
	<-entered

	select {
	case <-returned:
		app.configurationMu.Unlock()
		t.Fatal("signalStop bypassed the running publication lock")
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-app.stopCh:
		app.configurationMu.Unlock()
		t.Fatal("signalStop closed stopCh while running publication was locked")
	default:
	}

	app.configurationMu.Unlock()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("signalStop did not complete after running publication unlocked")
	}
	select {
	case <-app.stopCh:
	default:
		t.Fatal("signalStop returned without closing stopCh")
	}
}

func TestBeginStartInitializesZeroValueLifecycleState(t *testing.T) {
	app := &App{}

	app.beginStart()
	t.Cleanup(app.cancelStartup)

	assert.Equal(t, appStateStarting, app.state())
	assert.NotNil(t, app.startupCtx)
	assert.NotNil(t, app.startDone)
	assert.NotNil(t, app.lifecycleDone)
	assert.NotNil(t, app.startupJournal)
}

func TestPublishRunningRejectsEveryPublishedStopSource(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*App, context.CancelFunc)
	}{
		{
			name: "stop requested",
			configure: func(app *App, _ context.CancelFunc) {
				app.stopRequested = true
			},
		},
		{
			name: "startup canceled",
			configure: func(_ *App, cancel context.CancelFunc) {
				cancel()
			},
		},
		{
			name: "stop channel closed",
			configure: func(app *App, _ context.CancelFunc) {
				close(app.stopCh)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			startupCtx, cancelStartup := context.WithCancel(context.Background())
			t.Cleanup(cancelStartup)
			app := &App{
				startupCtx: startupCtx,
				startDone:  make(chan struct{}),
				stopCh:     make(chan struct{}),
			}
			test.configure(app, cancelStartup)

			assert.False(t, app.publishRunning())
			assert.Equal(t, appStateCreated, app.state())
			select {
			case <-app.startDone:
				t.Fatal("failed running publication closed startDone")
			default:
			}
		})
	}
}

func TestPublishRunningCommitsStateBeforeStartingCallback(t *testing.T) {
	startupCtx, cancelStartup := context.WithCancel(context.Background())
	t.Cleanup(cancelStartup)
	started := make(chan appLifecycleState, 1)
	app := &App{
		startupCtx: startupCtx,
		startDone:  make(chan struct{}),
		stopCh:     make(chan struct{}),
	}
	app.onStarted = func() { started <- app.state() }

	require.True(t, app.publishRunning())
	select {
	case state := <-started:
		assert.Equal(t, appStateRunning, state)
	case <-time.After(time.Second):
		t.Fatal("OnStarted callback did not run")
	}
	select {
	case <-app.startDone:
	default:
		t.Fatal("successful running publication did not close startDone")
	}

	app.cancelBackground()
	app.backgroundWG.Wait()
}

func TestFinishRuntimeShutdownHandlesCompletedTerminalPaths(t *testing.T) {
	t.Run("already stopped", func(t *testing.T) {
		lifecycleDone := make(chan struct{})
		close(lifecycleDone)
		app := &App{lifecycleDone: lifecycleDone}
		app.lifecycleState.Store(uint32(appStateStopped))

		app.finishRuntimeShutdown(nil, nil)

		assert.Equal(t, appStateStopped, app.state())
	})

	t.Run("existing shutdown owner", func(t *testing.T) {
		lifecycleDone := make(chan struct{})
		close(lifecycleDone)
		app := &App{
			lifecycleDone:   lifecycleDone,
			shutdownStarted: true,
		}
		app.lifecycleState.Store(uint32(appStateStopping))

		app.finishRuntimeShutdown(nil, nil)

		assert.True(t, app.shutdownStarted)
		assert.Equal(t, appStateStopping, app.state())
	})
}

func TestShutdownInitializesZeroValueCompletionChannels(t *testing.T) {
	app := &App{
		stopCh:  make(chan struct{}),
		dieChan: make(chan bool, 1),
	}

	app.Shutdown()

	assert.True(t, app.stopRequested)
	assert.Equal(t, appStateStopped, app.state())
	for name, channel := range map[string]<-chan struct{}{
		"start":     app.startDone,
		"lifecycle": app.lifecycleDone,
		"stop":      app.stopCh,
	} {
		select {
		case <-channel:
		default:
			t.Fatalf("%s completion channel is still open", name)
		}
	}
	select {
	case <-app.dieChan:
	default:
		t.Fatal("Shutdown did not preserve the legacy die signal")
	}
}

func TestListenRejectsPreCanceledStartup(t *testing.T) {
	startupCtx, cancelStartup := context.WithCancel(context.Background())
	cancelStartup()
	app := &App{startupCtx: startupCtx}

	err := app.listen()

	require.ErrorIs(t, err, context.Canceled)
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
	builder.RPCClient, err = cluster.NewNatsRPCClient(natsClientConfig, builder.Server, builder.MetricsReporters, builder.dependencyDieChan)
	if err != nil {
		panic(err.Error())
	}

	natsServerConfig := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	natsServerConfig.Connect = fmt.Sprintf("nats://%s", nsAddr)
	builder.RPCServer, err = cluster.NewNatsRPCServer(natsServerConfig, builder.Server, builder.MetricsReporters, builder.dependencyDieChan, builder.SessionPool)
	if err != nil {
		panic(err.Error())
	}

	etcdSD, err := cluster.NewEtcdServiceDiscovery(config.NewDefaultPitayaConfig().Cluster.SD.Etcd, builder.Server, builder.dependencyDieChan, cli)
	builder.ServiceDiscovery = etcdSD
	assert.NoError(t, err)
	acc := acceptor.NewTCPAcceptor("0.0.0.0:0")
	builder.AddAcceptor(acc)
	app := builder.Build().(*App)
	started := make(chan struct{})
	app.OnStarted(func() { close(started) })

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
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for app startup")
	}

	assert.NotNil(t, app.handlerService)
	assert.NotNil(t, timer.GlobalTicker)
	// should be listening
	assert.NotEmpty(t, acc.GetAddr())
	helpers.ShouldEventuallyReturn(t, func() error {
		n, err := net.Dial("tcp", acc.GetAddr())
		if err == nil {
			defer n.Close()
		}
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
	tags := map[string]string{
		"key": "value",
	}
	ctx := AddMetricTagsToPropagateCtx(context.Background(), tags)
	tags["key"] = "mutated"
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
	require.NoError(t, app.startupComponents())
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
	t.Cleanup(app.worker.Stop)
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
	cfg.Concurrency.Handler.Dispatch = 1
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
	assert.False(t, app.IsRunning())
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
		app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
		app.StartWorker()
		t.Cleanup(app.worker.Stop)

		err := app.RegisterRPCJob(nil)
		assert.NoError(t, err)
	})

	t.Run("register_twice", func(t *testing.T) {
		builderConfig := config.NewDefaultPitayaConfig()
		app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *builderConfig).(*App)
		app.StartWorker()
		t.Cleanup(app.worker.Stop)

		err := app.RegisterRPCJob(nil)
		assert.NoError(t, err)

		err = app.RegisterRPCJob(nil)
		assert.Equal(t, constants.ErrRPCJobAlreadyRegistered, err)
	})
}
