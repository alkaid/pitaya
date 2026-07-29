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
	"reflect"

	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/logger"
	"go.uber.org/zap"
)

type regComp struct {
	comp component.Component
	opts []component.Option
}

// Register register a component with options
func (app *App) Register(c component.Component, options ...component.Option) {
	app.handlerComp = append(app.handlerComp, regComp{c, options})
}

// RegisterRemote register a remote component with options
func (app *App) RegisterRemote(c component.Component, options ...component.Option) {
	app.remoteComp = append(app.remoteComp, regComp{c, options})
}

// RegisterSubscribe register a remote component with options
func (app *App) RegisterSubscribe(c component.Component, options ...component.Option) {
	options = append(options, component.WithSubscriber())
	app.remoteComp = append(app.remoteComp, regComp{c, options})
}

func (app *App) LazyRegister(c component.Component, options ...component.Option) {
	if err := app.handlerService.Register(c, options); err != nil {
		logger.Zap.Error("Failed to lazy register handler", zap.Error(err))
	}
}
func (app *App) LazyRegisterRemote(c component.Component, options ...component.Option) {
	if err := app.remoteService.Register(c, options); err != nil {
		logger.Zap.Error("Failed to lazy register remote", zap.Error(err))
	}
}
func (app *App) RegisterInterceptor(serviceName string, interceptor *component.Interceptor) {
	app.handlerService.RegisterInterceptor(serviceName, interceptor)
}
func (app *App) RegisterRemoteInterceptor(serviceName string, interceptor *component.Interceptor) {
	app.remoteService.RegisterInterceptor(serviceName, interceptor)
}

func uniqueComponents(group []regComp, seen map[component.Component]struct{}) []component.Component {
	components := make([]component.Component, 0)
	for _, registered := range group {
		comp := registered.comp
		if reflect.TypeOf(comp).Comparable() {
			if _, ok := seen[comp]; ok {
				continue
			}
			seen[comp] = struct{}{}
		}
		components = append(components, comp)
	}
	return components
}

func (app *App) startupComponents() {
	seen := make(map[component.Component]struct{})
	for _, group := range [][]regComp{app.handlerComp, app.remoteComp} {
		components := uniqueComponents(group, seen)
		for _, comp := range components {
			comp.Init()
		}
		for _, comp := range components {
			comp.AfterInit()
		}
	}

	// register all components
	for _, c := range app.handlerComp {
		if err := app.handlerService.Register(c.comp, c.opts); err != nil {
			logger.Zap.Error("Failed to register handler", zap.Error(err))
		}
	}

	// register all remote components
	for _, c := range app.remoteComp {
		if app.remoteService == nil {
			logger.Zap.Warn("registered a remote component but remoteService is not running! skipping...")
		} else {
			if err := app.remoteService.Register(c.comp, c.opts); err != nil {
				logger.Zap.Error("Failed to register remote", zap.Error(err))
			}
		}
	}

	app.handlerService.DumpServices()
	if app.remoteService != nil {
		app.remoteService.DumpServices()
	}
}

func (app *App) shutdownComponents() {
	seen := make(map[component.Component]struct{})
	for _, group := range [][]regComp{app.handlerComp, app.remoteComp} {
		components := uniqueComponents(group, seen)
		for i := len(components) - 1; i >= 0; i-- {
			components[i].BeforeShutdown()
		}
		for i := len(components) - 1; i >= 0; i-- {
			components[i].Shutdown()
		}
	}
}
