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
	"testing"

	clustermocks "github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/interfaces"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type MyMod struct {
	component.Base
	running bool
	name    string
}

type valueModule struct{}

func (valueModule) Init() error     { return nil }
func (valueModule) AfterInit()      {}
func (valueModule) BeforeShutdown() {}
func (valueModule) Shutdown() error { return nil }

type startupCancelModule struct {
	MyMod
	cancel         context.CancelFunc
	afterInitCount int
}

func (m *startupCancelModule) Init() error {
	if err := m.MyMod.Init(); err != nil {
		return err
	}
	m.cancel()
	return nil
}

func (m *startupCancelModule) AfterInit() {
	m.afterInitCount++
}

type LifecycleFaultModule struct {
	MyMod
	initErr             error
	initPanic           error
	afterInitPanic      error
	beforeShutdownPanic error
	shutdownErr         error
	shutdownPanic       error
	afterInitCount      int
	beforeCount         int
	shutdownCount       int
}

func (m *LifecycleFaultModule) Init() error {
	if m.initPanic != nil {
		m.running = false
		panic(m.initPanic)
	}
	if m.initErr != nil {
		m.running = false
		return m.initErr
	}
	return m.MyMod.Init()
}

func (m *LifecycleFaultModule) AfterInit() {
	m.afterInitCount++
	if m.afterInitPanic != nil {
		panic(m.afterInitPanic)
	}
}

func (m *LifecycleFaultModule) BeforeShutdown() {
	m.beforeCount++
	if m.beforeShutdownPanic != nil {
		panic(m.beforeShutdownPanic)
	}
}

func (m *LifecycleFaultModule) Shutdown() error {
	m.shutdownCount++
	m.running = false
	if m.shutdownPanic != nil {
		panic(m.shutdownPanic)
	}
	return m.shutdownErr
}

var modulesOrder []string

func (m *MyMod) Init() error {
	m.running = true
	modulesOrder = append(modulesOrder, m.name)
	return nil
}

func (m *MyMod) Shutdown() error {
	m.running = false
	modulesOrder = append(modulesOrder, "shutdown:"+m.name)
	return nil
}

func (m *MyMod) BeforeShutdown() {
	modulesOrder = append(modulesOrder, "before:"+m.name)
}

func TestRegisterModule(t *testing.T) {
	b := &MyMod{}

	config := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *config).(*App)

	err := app.RegisterModule(b, "mod")
	assert.NoError(t, err)
	assert.Equal(t, 1, len(app.modulesMap))
	assert.Equal(t, b, app.modulesMap["mod"])
	assert.Equal(t, 1, len(app.modulesArr))
	assert.Equal(t, "mod", app.modulesArr[0].name)
	assert.Equal(t, b, app.modulesArr[0].module)
	err = app.RegisterModule(b, "mod")
	assert.Error(t, err)
}

func TestRegisterModuleBeforeRejectsAfterStartupBegins(t *testing.T) {
	app := newRuntimeModuleTestApp(Standalone)
	app.lifecycleState.Store(uint32(appStateStarting))

	err := app.RegisterModuleBefore(&MyMod{name: "late"}, "late")

	require.ErrorIs(t, err, constants.ErrAppConfigurationLocked)
	assert.NotContains(t, app.modulesMap, "late")
}

func TestGetModule(t *testing.T) {
	b := &MyMod{}

	config := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *config)

	app.RegisterModule(b, "mod")
	m, err := app.GetModule("mod")
	assert.NoError(t, err)
	assert.Equal(t, b, m)

	m, err = app.GetModule("mmm")
	assert.Error(t, err)
}

func TestStartupModules(t *testing.T) {
	modulesOrder = []string{}
	app := newAppTestApp(true, "testtype", Standalone, map[string]string{}, *config.NewDefaultPitayaConfig()).(*App)

	err := app.RegisterModule(&MyMod{name: "mod1"}, "mod1")
	assert.NoError(t, err)
	err = app.RegisterModuleBefore(&MyMod{name: "mod2"}, "mod2")
	assert.NoError(t, err)
	err = app.RegisterModuleBefore(&MyMod{name: "mod3"}, "mod3")
	assert.NoError(t, err)
	err = app.RegisterModuleAfter(&MyMod{name: "mod4"}, "mod4")
	assert.NoError(t, err)

	assert.NoError(t, app.startModules())
	assert.Equal(t, true, app.modulesMap["mod1"].(*MyMod).running)
	assert.Equal(t, true, app.modulesMap["mod2"].(*MyMod).running)
	assert.Equal(t, true, app.modulesMap["mod3"].(*MyMod).running)
	assert.Equal(t, true, app.modulesMap["mod4"].(*MyMod).running)
	assert.Equal(t, []string{"mod3", "mod2", "mod1", "mod4"}, modulesOrder)
}

func TestStartModulesHonorsCancellationAtBothLifecyclePhases(t *testing.T) {
	t.Run("before init", func(t *testing.T) {
		startupCtx, cancelStartup := context.WithCancel(context.Background())
		cancelStartup()
		module := &LifecycleFaultModule{MyMod: MyMod{name: "not-started"}}
		app := &App{
			startupCtx:     startupCtx,
			startupJournal: newStartupJournal(),
			modulesArr: []moduleWrapper{
				{name: "not-started", module: module},
			},
		}

		err := app.startModules()

		require.ErrorIs(t, err, context.Canceled)
		assert.False(t, module.running)
		assert.Zero(t, module.afterInitCount)
		assert.Empty(t, app.startupJournal.modules)
	})

	t.Run("before after init", func(t *testing.T) {
		startupCtx, cancelStartup := context.WithCancel(context.Background())
		module := &startupCancelModule{
			MyMod:  MyMod{name: "cancel-after-init"},
			cancel: cancelStartup,
		}
		app := &App{
			startupCtx:     startupCtx,
			startupJournal: newStartupJournal(),
			modulesArr: []moduleWrapper{
				{name: "cancel-after-init", module: module},
			},
		}

		err := app.startModules()

		require.ErrorIs(t, err, context.Canceled)
		assert.True(t, module.running)
		assert.Zero(t, module.afterInitCount)
		require.Len(t, app.startupJournal.modules, 1)
		require.NoError(t, app.shutdownModules())
		assert.False(t, module.running)
	})
}

func TestShutdownModules(t *testing.T) {
	modulesOrder = []string{}
	app := newAppTestApp(true, "testtype", Standalone, map[string]string{}, *config.NewDefaultPitayaConfig()).(*App)

	err := app.RegisterModule(&MyMod{name: "mod1"}, "mod1")
	assert.NoError(t, err)
	err = app.RegisterModuleBefore(&MyMod{name: "mod2"}, "mod2")
	assert.NoError(t, err)
	err = app.RegisterModuleBefore(&MyMod{name: "mod3"}, "mod3")
	assert.NoError(t, err)
	err = app.RegisterModuleAfter(&MyMod{name: "mod4"}, "mod4")
	assert.NoError(t, err)

	assert.NoError(t, app.startModules())

	modulesOrder = []string{}
	assert.NoError(t, app.shutdownModules())
	assert.Equal(t, false, app.modulesMap["mod1"].(*MyMod).running)
	assert.Equal(t, false, app.modulesMap["mod2"].(*MyMod).running)
	assert.Equal(t, false, app.modulesMap["mod3"].(*MyMod).running)
	assert.Equal(t, false, app.modulesMap["mod4"].(*MyMod).running)
	assert.Equal(t, []string{
		"before:mod4", "before:mod1", "before:mod2", "before:mod3",
		"shutdown:mod4", "shutdown:mod1", "shutdown:mod2", "shutdown:mod3",
	}, modulesOrder)
}

func TestStartupModuleInitErrorRollsBackOnlySuccessfulModules(t *testing.T) {
	initErr := errors.New("module init failed")
	app := newAppTestApp(true, "testtype", Standalone, map[string]string{}, *config.NewDefaultPitayaConfig()).(*App)
	first := &LifecycleFaultModule{MyMod: MyMod{name: "first"}}
	failing := &LifecycleFaultModule{MyMod: MyMod{name: "failing"}, initErr: initErr}
	notStarted := &LifecycleFaultModule{MyMod: MyMod{name: "not-started"}}
	require.NoError(t, app.RegisterModule(first, "first"))
	require.NoError(t, app.RegisterModule(failing, "failing"))
	require.NoError(t, app.RegisterModule(notStarted, "not-started"))

	err := app.startModules()
	require.ErrorIs(t, err, initErr)
	require.NoError(t, app.shutdownModules())
	assert.Equal(t, 1, first.beforeCount)
	assert.Equal(t, 1, first.shutdownCount)
	assert.Equal(t, 0, failing.beforeCount)
	assert.Equal(t, 0, failing.shutdownCount)
	assert.False(t, failing.running, "failed Init must clean its own partial state")
	assert.False(t, notStarted.running)
}

func TestStartupModuleAfterInitPanicRollsBackEveryInitializedModule(t *testing.T) {
	afterErr := errors.New("module after init failed")
	app := newAppTestApp(true, "testtype", Standalone, map[string]string{}, *config.NewDefaultPitayaConfig()).(*App)
	first := &LifecycleFaultModule{MyMod: MyMod{name: "first"}}
	failing := &LifecycleFaultModule{MyMod: MyMod{name: "failing"}, afterInitPanic: afterErr}
	last := &LifecycleFaultModule{MyMod: MyMod{name: "last"}}
	require.NoError(t, app.RegisterModule(first, "first"))
	require.NoError(t, app.RegisterModule(failing, "failing"))
	require.NoError(t, app.RegisterModule(last, "last"))

	err := app.startModules()
	require.ErrorIs(t, err, afterErr)
	require.NoError(t, app.shutdownModules())
	for _, module := range []*LifecycleFaultModule{first, failing, last} {
		assert.Equal(t, 1, module.beforeCount)
		assert.Equal(t, 1, module.shutdownCount)
		assert.False(t, module.running)
	}
	assert.Equal(t, 1, first.afterInitCount)
	assert.Equal(t, 1, failing.afterInitCount)
	assert.Zero(t, last.afterInitCount)
}

func TestStartupModuleRollbackJoinsCleanupFailuresAndContinues(t *testing.T) {
	beforeErr := errors.New("module before shutdown failed")
	shutdownErr := errors.New("module shutdown failed")
	panicErr := errors.New("module shutdown panic")
	app := newAppTestApp(true, "testtype", Standalone, map[string]string{}, *config.NewDefaultPitayaConfig()).(*App)
	first := &LifecycleFaultModule{MyMod: MyMod{name: "first"}, shutdownErr: shutdownErr}
	second := &LifecycleFaultModule{MyMod: MyMod{name: "second"}, beforeShutdownPanic: beforeErr}
	third := &LifecycleFaultModule{MyMod: MyMod{name: "third"}, shutdownPanic: panicErr}
	for _, registration := range []struct {
		name   string
		module *LifecycleFaultModule
	}{{"first", first}, {"second", second}, {"third", third}} {
		require.NoError(t, app.RegisterModule(registration.module, registration.name))
	}
	require.NoError(t, app.startModules())

	err := app.shutdownModules()
	require.ErrorIs(t, err, beforeErr)
	require.ErrorIs(t, err, shutdownErr)
	require.ErrorIs(t, err, panicErr)
	for _, module := range []*LifecycleFaultModule{first, second, third} {
		assert.Equal(t, 1, module.beforeCount)
		assert.Equal(t, 1, module.shutdownCount)
	}
}

func newRuntimeModuleTestApp(mode ServerMode) *App {
	return &App{
		config:     *config.NewDefaultPitayaConfig(),
		serverMode: mode,
		modulesMap: make(map[string]interfaces.Module),
	}
}

func configureClusterRuntimeModuleTestApp(app *App) {
	app.rpcServer = &clustermocks.MockRPCServer{}
	app.rpcClient = &clustermocks.MockRPCClient{}
	app.serviceDiscovery = &clustermocks.MockServiceDiscovery{}
	app.remoteService = &service.RemoteService{}
}

func TestIsNilModuleHandlesNilInterfacesTypedNilAndValues(t *testing.T) {
	require.True(t, isNilModule(nil))
	var typedNil *LifecycleFaultModule
	require.True(t, isNilModule(typedNil))
	require.False(t, isNilModule(valueModule{}))
}

func TestPrepareRuntimeModulesRejectsMalformedUserModules(t *testing.T) {
	tests := []struct {
		name     string
		modules  []moduleWrapper
		wantText string
	}{
		{
			name:     "empty name",
			modules:  []moduleWrapper{{module: valueModule{}}},
			wantText: "registered module has an empty name",
		},
		{
			name: "typed nil",
			modules: []moduleWrapper{{
				name:   "typed-nil",
				module: (*LifecycleFaultModule)(nil),
			}},
			wantText: `module "typed-nil" is nil`,
		},
		{
			name: "duplicate name",
			modules: []moduleWrapper{
				{name: "duplicate", module: valueModule{}},
				{name: "duplicate", module: valueModule{}},
			},
			wantText: "module with name duplicate already exists",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := newRuntimeModuleTestApp(Standalone)
			app.modulesArr = test.modules

			_, err := app.prepareRuntimeModules()
			require.EqualError(t, err, test.wantText)
		})
	}
}

func TestPrepareRuntimeModulesRequiresCompleteClusterDependencies(t *testing.T) {
	tests := []struct {
		name     string
		remove   func(*App)
		wantText string
	}{
		{
			name:     "rpc server",
			remove:   func(app *App) { app.rpcServer = nil },
			wantText: `cluster module "rpcServer" is not configured`,
		},
		{
			name:     "rpc client",
			remove:   func(app *App) { app.rpcClient = nil },
			wantText: `cluster module "rpcClient" is not configured`,
		},
		{
			name:     "service discovery",
			remove:   func(app *App) { app.serviceDiscovery = nil },
			wantText: `cluster module "serviceDiscovery" is not configured`,
		},
		{
			name:     "remote service",
			remove:   func(app *App) { app.remoteService = nil },
			wantText: "cluster remote service is not configured",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := newRuntimeModuleTestApp(Cluster)
			configureClusterRuntimeModuleTestApp(app)
			test.remove(app)

			_, err := app.prepareRuntimeModules()
			require.EqualError(t, err, test.wantText)
		})
	}
}

func TestPrepareRuntimeModulesRejectsEveryFrameworkNameConflict(t *testing.T) {
	tests := []struct {
		name      string
		mode      ServerMode
		configure func(*App)
	}{
		{name: "rpcServer", mode: Cluster, configure: configureClusterRuntimeModuleTestApp},
		{name: "rpcClient", mode: Cluster, configure: configureClusterRuntimeModuleTestApp},
		{name: "serviceDiscovery", mode: Cluster, configure: configureClusterRuntimeModuleTestApp},
		{name: "uniqueSession", mode: Cluster, configure: configureClusterRuntimeModuleTestApp},
		{
			name: "configLoader",
			configure: func(app *App) {
				app.conf = &config.Config{}
			},
		},
		{name: "statefulGoPool"},
		{
			name: "prometheusExporter",
			configure: func(app *App) {
				app.prometheusExporter = &metrics.PrometheusExporter{}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := newRuntimeModuleTestApp(test.mode)
			if test.configure != nil {
				test.configure(app)
			}
			module := valueModule{}
			app.modulesArr = []moduleWrapper{{name: test.name, module: module}}
			app.modulesMap[test.name] = module

			_, err := app.prepareRuntimeModules()
			require.EqualError(t, err, "module with name "+test.name+" already exists")
		})
	}
}

func TestCommitRuntimeModulesRejectsInvalidFactoriesWithoutMutation(t *testing.T) {
	t.Run("nil plan", func(t *testing.T) {
		app := newRuntimeModuleTestApp(Standalone)
		require.NoError(t, app.commitRuntimeModules(nil))
		require.Empty(t, app.modulesArr)
	})

	t.Run("factory panic", func(t *testing.T) {
		app := newRuntimeModuleTestApp(Standalone)
		factoryErr := errors.New("factory panic")
		prepared := &preparedRuntimeModules{specs: []runtimeModuleSpec{{
			name: "panicking",
			factory: func() interfaces.Module {
				panic(factoryErr)
			},
		}}}

		err := app.commitRuntimeModules(prepared)
		require.ErrorIs(t, err, factoryErr)
		require.Empty(t, app.modulesArr)
		require.Empty(t, app.modulesMap)
	})

	t.Run("typed nil factory result", func(t *testing.T) {
		app := newRuntimeModuleTestApp(Standalone)
		prepared := &preparedRuntimeModules{specs: []runtimeModuleSpec{{
			name: "typed-nil",
			factory: func() interfaces.Module {
				return (*LifecycleFaultModule)(nil)
			},
		}}}

		err := app.commitRuntimeModules(prepared)
		require.EqualError(t, err, `framework module "typed-nil" is nil`)
		require.Empty(t, app.modulesArr)
		require.Empty(t, app.modulesMap)
	})
}

func TestCommitRuntimeModulesRollsBackRegistrationAndCallbackFailures(t *testing.T) {
	t.Run("registration conflict", func(t *testing.T) {
		app := newRuntimeModuleTestApp(Standalone)
		original := valueModule{}
		app.modulesArr = []moduleWrapper{{name: "duplicate", module: original}}
		app.modulesMap["duplicate"] = original
		prepared := &preparedRuntimeModules{specs: []runtimeModuleSpec{
			{name: "first", position: runtimeModuleBefore, factory: func() interfaces.Module { return valueModule{} }},
			{name: "duplicate", position: runtimeModuleAfter, factory: func() interfaces.Module { return valueModule{} }},
		}}

		err := app.commitRuntimeModules(prepared)
		require.ErrorContains(t, err, `register framework module "duplicate"`)
		require.Equal(t, []moduleWrapper{{name: "duplicate", module: original}}, app.modulesArr)
		require.Equal(t, map[string]interfaces.Module{"duplicate": original}, app.modulesMap)
	})

	t.Run("after-register panic", func(t *testing.T) {
		app := newRuntimeModuleTestApp(Standalone)
		callbackErr := errors.New("after-register panic")
		prepared := &preparedRuntimeModules{specs: []runtimeModuleSpec{{
			name:    "callback",
			factory: func() interfaces.Module { return valueModule{} },
			afterRegister: func(interfaces.Module) {
				panic(callbackErr)
			},
		}}}

		err := app.commitRuntimeModules(prepared)
		require.ErrorIs(t, err, callbackErr)
		require.Empty(t, app.modulesArr)
		require.Empty(t, app.modulesMap)
	})
}

func TestShutdownModulesWithoutStartupJournalIsANoop(t *testing.T) {
	app := newRuntimeModuleTestApp(Standalone)
	require.NoError(t, app.shutdownModules())
	require.NoError(t, app.shutdownModules())
}
