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
	"testing"

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/service"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/stretchr/testify/assert"
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

func TestStartupComponents(t *testing.T) {
	app := newTestAppForComponents(nil)

	app.Register(&MyComp{})
	app.RegisterRemote(&MyComp{})
	app.startupComponents()
	assert.Equal(t, true, app.handlerComp[0].comp.(*MyComp).running)
}

func TestShutdownComponents(t *testing.T) {
	app := newTestAppForComponents(nil)

	app.Register(&MyComp{})
	app.RegisterRemote(&MyComp{})
	app.startupComponents()

	app.shutdownComponents()
	assert.Equal(t, false, app.handlerComp[0].comp.(*MyComp).running)
}

func TestComponentLifecycleRunsOnceAcrossRegistries(t *testing.T) {
	app := newTestAppForComponents(nil)
	comp := &MyComp{}
	app.Register(comp)
	app.RegisterRemote(comp)

	app.startupComponents()
	assert.Equal(t, 1, comp.initCount)
	assert.Equal(t, 1, comp.afterInitCount)

	app.shutdownComponents()
	assert.Equal(t, 1, comp.beforeShutdownCount)
	assert.Equal(t, 1, comp.shutdownCount)
}

func TestComponentLifecyclePreservesRegistryOrder(t *testing.T) {
	app := newTestAppForComponents(nil)
	events := make([]string, 0, 8)
	app.Register(&MyComp{name: "handler", events: &events})
	app.RegisterRemote(&MyComp{name: "remote", events: &events})

	app.startupComponents()
	assert.Equal(t, []string{
		"handler.Init",
		"handler.AfterInit",
		"remote.Init",
		"remote.AfterInit",
	}, events)

	events = events[:0]
	app.shutdownComponents()
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
	app.Register(a)
	app.Register(b)
	app.Register(a)

	app.startupComponents()
	assert.Equal(t, []string{
		"a.Init",
		"b.Init",
		"a.AfterInit",
		"b.AfterInit",
	}, events)

	events = events[:0]
	app.shutdownComponents()
	assert.Equal(t, []string{
		"b.BeforeShutdown",
		"a.BeforeShutdown",
		"b.Shutdown",
		"a.Shutdown",
	}, events)
}
