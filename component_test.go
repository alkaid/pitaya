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

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/service"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type MyComp struct {
	component.Base
	running             bool
	initCount           int
	afterInitCount      int
	beforeShutdownCount int
	shutdownCount       int
	name                string
	events              *[]string
}

type NonComparableComp struct {
	*MyComp
	marker []int
}

type LifecycleFaultComp struct {
	MyComp
	initPanic           error
	afterInitPanic      error
	beforeShutdownPanic error
	shutdownPanic       error
}

type AfterInitHookComp struct {
	MyComp
	afterInitHook func()
}

func (c *AfterInitHookComp) AfterInit() {
	c.MyComp.AfterInit()
	if c.afterInitHook != nil {
		c.afterInitHook()
	}
}

func (c *LifecycleFaultComp) Init() {
	c.MyComp.Init()
	if c.initPanic != nil {
		// Init did not commit, so the component owns partial cleanup.
		c.running = false
		panic(c.initPanic)
	}
}

func (c *LifecycleFaultComp) AfterInit() {
	c.MyComp.AfterInit()
	if c.afterInitPanic != nil {
		panic(c.afterInitPanic)
	}
}

func (c *LifecycleFaultComp) BeforeShutdown() {
	c.MyComp.BeforeShutdown()
	if c.beforeShutdownPanic != nil {
		panic(c.beforeShutdownPanic)
	}
}

func (c *LifecycleFaultComp) Shutdown() {
	c.MyComp.Shutdown()
	if c.shutdownPanic != nil {
		panic(c.shutdownPanic)
	}
}

func (m *MyComp) record(event string) {
	if m.events != nil {
		*m.events = append(*m.events, m.name+"."+event)
	}
}

func (m *MyComp) Init() {
	m.running = true
	m.initCount++
	m.record("Init")
}

func (m *MyComp) AfterInit() {
	m.afterInitCount++
	m.record("AfterInit")
}

func (m *MyComp) BeforeShutdown() {
	m.beforeShutdownCount++
	m.record("BeforeShutdown")
}

func (m *MyComp) Shutdown() {
	m.running = false
	m.shutdownCount++
	m.record("Shutdown")
}

func (m *MyComp) Handler(context.Context) {}

func (m *MyComp) Remote(context.Context, *protos.Request) (*protos.Response, error) {
	return &protos.Response{}, nil
}

func newTestAppForComponents(sessionPool session.SessionPool) *App {
	if sessionPool == nil {
		sessionPool = session.NewSessionPool()
	}
	server := &cluster.Server{ID: "test-id", Type: "testtype", Frontend: true}
	handlerPool := service.NewHandlerPool()
	handlerHooks := pipeline.NewHandlerHooks()
	remoteService := service.NewRemoteService(
		nil, nil, nil, nil, nil, nil, nil, server, sessionPool,
		pipeline.NewRemoteHooks(), handlerHooks, handlerPool,
	)
	handlerService := service.NewHandlerService(
		nil, nil, 0, 0, server, remoteService, nil, nil, handlerHooks, handlerPool,
	)
	return &App{
		server:         server,
		sessionPool:    sessionPool,
		remoteService:  remoteService,
		handlerService: handlerService,
		handlerComp:    make([]regComp, 0),
		remoteComp:     make([]regComp, 0),
	}
}

func TestRegister(t *testing.T) {
	app := newTestAppForComponents(nil)
	b := &component.Base{}
	app.Register(b)
	assert.Equal(t, 1, len(app.handlerComp))
	assert.Equal(t, regComp{b, nil}, app.handlerComp[0])
}

func TestRegisterRemote(t *testing.T) {
	app := newTestAppForComponents(nil)
	before := app.remoteComp
	b := &component.Base{}
	app.RegisterRemote(b)
	assert.Equal(t, len(before)+1, len(app.remoteComp))
	assert.Equal(t, regComp{b, nil}, app.remoteComp[len(before)])
}

func runtimeTestInterceptor() *component.Interceptor {
	return &component.Interceptor{InterceptorFun: func(context.Context, route.Route, []byte) (proto.Message, error) {
		return nil, nil
	}}
}

func TestRuntimeRegistrationReturnsErrors(t *testing.T) {
	app := newTestAppForComponents(nil)

	require.NoError(t, app.LazyRegister(&MyComp{}))
	assert.ErrorContains(t, app.LazyRegister(&MyComp{}), "service already defined")
	require.NoError(t, app.LazyRegisterRemote(&MyComp{}))
	assert.ErrorContains(t, app.LazyRegisterRemote(&MyComp{}), "service already defined")

	assert.Error(t, app.RegisterInterceptor("", runtimeTestInterceptor()))
	assert.Error(t, app.RegisterInterceptor("handler", nil))
	assert.Error(t, app.RegisterInterceptor("handler", &component.Interceptor{}))
	require.NoError(t, app.RegisterInterceptor("handler", runtimeTestInterceptor()))
	require.NoError(t, app.RegisterRemoteInterceptor("sys", runtimeTestInterceptor()))
	assert.Error(t, app.RegisterRemoteInterceptor("remote.method", runtimeTestInterceptor()))
}

func TestRuntimeRegistrationReportsMissingCapabilities(t *testing.T) {
	handlerless := newTestAppForComponents(nil)
	handlerless.handlerService = nil
	assert.ErrorIs(t, handlerless.LazyRegister(&MyComp{}), service.ErrHandlerServiceUnavailable)
	assert.ErrorIs(t, handlerless.RegisterInterceptor("handler", runtimeTestInterceptor()), service.ErrHandlerServiceUnavailable)

	remoteless := newTestAppForComponents(nil)
	remoteless.remoteService = nil
	assert.ErrorIs(t, remoteless.LazyRegisterRemote(&MyComp{}), service.ErrRemoteServiceUnavailable)
	assert.ErrorIs(t, remoteless.RegisterRemoteInterceptor("remote", runtimeTestInterceptor()), service.ErrRemoteServiceUnavailable)
}

func TestStartupComponents(t *testing.T) {
	app := newTestAppForComponents(nil)

	app.Register(&MyComp{})
	app.RegisterRemote(&MyComp{})
	require.NoError(t, app.startupComponents())
	assert.Equal(t, true, app.handlerComp[0].comp.(*MyComp).running)
}

func TestShutdownComponents(t *testing.T) {
	app := newTestAppForComponents(nil)

	app.Register(&MyComp{})
	app.RegisterRemote(&MyComp{})
	require.NoError(t, app.startupComponents())

	require.NoError(t, app.shutdownComponents())
	assert.Equal(t, false, app.handlerComp[0].comp.(*MyComp).running)
}

func TestComponentLifecycleRunsOnceAcrossRegistries(t *testing.T) {
	app := newTestAppForComponents(nil)
	comp := &MyComp{}
	app.Register(comp)
	app.RegisterRemote(comp)

	require.NoError(t, app.startupComponents())
	assert.Equal(t, 1, comp.initCount)
	assert.Equal(t, 1, comp.afterInitCount)

	require.NoError(t, app.shutdownComponents())
	assert.Equal(t, 1, comp.beforeShutdownCount)
	assert.Equal(t, 1, comp.shutdownCount)
}

func TestNonComparableComponentLifecycleRunsOnceAcrossRegistries(t *testing.T) {
	app := newTestAppForComponents(nil)
	comp := NonComparableComp{
		MyComp: &MyComp{},
		marker: []int{1},
	}
	app.Register(comp)
	app.RegisterRemote(comp)

	require.NoError(t, app.startupComponents())
	assert.Equal(t, 1, comp.initCount)
	assert.Equal(t, 1, comp.afterInitCount)

	require.NoError(t, app.shutdownComponents())
	assert.Equal(t, 1, comp.beforeShutdownCount)
	assert.Equal(t, 1, comp.shutdownCount)
}

func TestDistinctNonComparableComponentLifecyclesBothRun(t *testing.T) {
	app := newTestAppForComponents(nil)
	first := NonComparableComp{MyComp: &MyComp{}, marker: []int{1}}
	second := NonComparableComp{MyComp: &MyComp{}, marker: []int{1}}
	app.Register(first, component.WithName("first"))
	app.Register(second, component.WithName("second"))

	require.NoError(t, app.startupComponents())
	require.NoError(t, app.shutdownComponents())

	assert.Equal(t, 1, first.initCount)
	assert.Equal(t, 1, first.shutdownCount)
	assert.Equal(t, 1, second.initCount)
	assert.Equal(t, 1, second.shutdownCount)
}

func TestComponentLifecyclePreservesRegistryOrder(t *testing.T) {
	app := newTestAppForComponents(nil)
	events := make([]string, 0, 8)
	app.Register(&MyComp{name: "handler", events: &events})
	app.RegisterRemote(&MyComp{name: "remote", events: &events})

	require.NoError(t, app.startupComponents())
	assert.Equal(t, []string{
		"handler.Init",
		"handler.AfterInit",
		"remote.Init",
		"remote.AfterInit",
	}, events)

	events = events[:0]
	require.NoError(t, app.shutdownComponents())
	assert.Equal(t, []string{
		"handler.BeforeShutdown",
		"handler.Shutdown",
		"remote.BeforeShutdown",
		"remote.Shutdown",
	}, events)
}

func TestComponentLifecycleReversesUniqueRegistryOrder(t *testing.T) {
	app := newTestAppForComponents(nil)
	events := make([]string, 0, 8)
	a := &MyComp{name: "a", events: &events}
	b := &MyComp{name: "b", events: &events}
	app.Register(a, component.WithName("a"))
	app.Register(b, component.WithName("b"))

	require.NoError(t, app.startupComponents())
	assert.Equal(t, []string{
		"a.Init",
		"b.Init",
		"a.AfterInit",
		"b.AfterInit",
	}, events)

	events = events[:0]
	require.NoError(t, app.shutdownComponents())
	assert.Equal(t, []string{
		"b.BeforeShutdown",
		"a.BeforeShutdown",
		"b.Shutdown",
		"a.Shutdown",
	}, events)
}

func TestStartupComponentPreflightRejectsDuplicateBeforeInit(t *testing.T) {
	app := newTestAppForComponents(nil)
	first := &MyComp{}
	second := &MyComp{}
	app.Register(first)
	app.Register(second)

	err := app.startupComponents()
	require.ErrorContains(t, err, "service already defined in startup batch")
	assert.Zero(t, first.initCount)
	assert.Zero(t, second.initCount)
	handlers, docsErr := app.handlerService.Docs(false)
	require.NoError(t, docsErr)
	assert.Empty(t, handlers)
}

func TestStartupComponentCommitIsAtomicAcrossHandlerAndRemoteRegistries(t *testing.T) {
	app := newTestAppForComponents(nil)
	startupHandler := &AfterInitHookComp{}
	startupHandler.afterInitHook = func() {
		require.NoError(t, app.LazyRegisterRemote(&MyComp{}, component.WithName("remote-conflict")))
	}
	app.Register(startupHandler, component.WithName("startup-handler"))
	app.RegisterRemote(&MyComp{}, component.WithName("remote-conflict"))

	err := app.startupComponents()
	require.ErrorContains(t, err, "remote: service already defined")
	handlers, docsErr := app.handlerService.Docs(false)
	require.NoError(t, docsErr)
	assert.Empty(t, handlers, "a failed cross-registry commit must not publish the handler batch")
}

func TestStartupComponentInitPanicRollsBackOnlySuccessfulInit(t *testing.T) {
	initErr := errors.New("component init failed")
	events := make([]string, 0, 8)
	app := newTestAppForComponents(nil)
	first := &LifecycleFaultComp{MyComp: MyComp{name: "first", events: &events}}
	failing := &LifecycleFaultComp{MyComp: MyComp{name: "failing", events: &events}, initPanic: initErr}
	notStarted := &LifecycleFaultComp{MyComp: MyComp{name: "not-started", events: &events}}
	app.Register(first, component.WithName("first"))
	app.Register(failing, component.WithName("failing"))
	app.Register(notStarted, component.WithName("not-started"))

	err := app.startupComponents()
	require.ErrorIs(t, err, initErr)
	require.NoError(t, app.startupJournal.rollbackComponents())
	assert.Equal(t, 1, first.shutdownCount)
	assert.Equal(t, 0, failing.shutdownCount)
	assert.False(t, failing.running, "failing Init fixture must clean its own partial state")
	assert.Equal(t, 0, notStarted.initCount)
	assert.Equal(t, []string{
		"first.Init",
		"failing.Init",
		"first.BeforeShutdown",
		"first.Shutdown",
	}, events)
}

func TestStartupComponentAfterInitPanicRollsBackAllInitializedInReverse(t *testing.T) {
	afterErr := errors.New("component after init failed")
	events := make([]string, 0, 16)
	app := newTestAppForComponents(nil)
	first := &LifecycleFaultComp{MyComp: MyComp{name: "first", events: &events}}
	failing := &LifecycleFaultComp{MyComp: MyComp{name: "failing", events: &events}, afterInitPanic: afterErr}
	last := &LifecycleFaultComp{MyComp: MyComp{name: "last", events: &events}}
	app.Register(first, component.WithName("first"))
	app.Register(failing, component.WithName("failing"))
	app.Register(last, component.WithName("last"))

	err := app.startupComponents()
	require.ErrorIs(t, err, afterErr)
	require.NoError(t, app.startupJournal.rollbackComponents())
	assert.Equal(t, []string{
		"first.Init", "failing.Init", "last.Init",
		"first.AfterInit", "failing.AfterInit",
		"last.BeforeShutdown", "failing.BeforeShutdown", "first.BeforeShutdown",
		"last.Shutdown", "failing.Shutdown", "first.Shutdown",
	}, events)
}

func TestStartupComponentRollbackContinuesAfterCleanupPanics(t *testing.T) {
	beforeErr := errors.New("before shutdown failed")
	shutdownErr := errors.New("shutdown failed")
	app := newTestAppForComponents(nil)
	first := &LifecycleFaultComp{MyComp: MyComp{name: "first"}, shutdownPanic: shutdownErr}
	second := &LifecycleFaultComp{MyComp: MyComp{name: "second"}, beforeShutdownPanic: beforeErr}
	app.Register(first, component.WithName("first"))
	app.Register(second, component.WithName("second"))
	require.NoError(t, app.startupComponents())

	err := app.startupJournal.rollbackComponents()
	require.ErrorIs(t, err, beforeErr)
	require.ErrorIs(t, err, shutdownErr)
	assert.Equal(t, 1, first.beforeShutdownCount)
	assert.Equal(t, 1, first.shutdownCount)
	assert.Equal(t, 1, second.beforeShutdownCount)
	assert.Equal(t, 1, second.shutdownCount)
}
