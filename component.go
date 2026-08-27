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
	"fmt"
	"reflect"

	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/service"
)

type regComp struct {
	comp component.Component
	opts []component.Option
}

// Register register a component with options
func (app *App) Register(c component.Component, options ...component.Option) error {
	app.configurationMu.Lock()
	defer app.configurationMu.Unlock()
	if err := app.configurationAdmissionLocked(); err != nil {
		return err
	}
	app.handlerComp = append(app.handlerComp, regComp{c, options})
	return nil
}

// RegisterRemote register a remote component with options
func (app *App) RegisterRemote(c component.Component, options ...component.Option) error {
	app.configurationMu.Lock()
	defer app.configurationMu.Unlock()
	if err := app.configurationAdmissionLocked(); err != nil {
		return err
	}
	app.remoteComp = append(app.remoteComp, regComp{c, options})
	return nil
}

// RegisterSubscribe register a remote component with options
func (app *App) RegisterSubscribe(c component.Component, options ...component.Option) error {
	app.configurationMu.Lock()
	defer app.configurationMu.Unlock()
	if err := app.configurationAdmissionLocked(); err != nil {
		return err
	}
	options = append(options, component.WithSubscriber())
	app.remoteComp = append(app.remoteComp, regComp{c, options})
	return nil
}

func (app *App) LazyRegister(c component.Component, options ...component.Option) error {
	if app.handlerService == nil {
		return service.ErrHandlerServiceUnavailable
	}
	return app.handlerService.Register(c, options)
}
func (app *App) LazyRegisterRemote(c component.Component, options ...component.Option) error {
	if app.remoteService == nil {
		return service.ErrRemoteServiceUnavailable
	}
	return app.remoteService.Register(c, options)
}
func (app *App) RegisterInterceptor(serviceName string, interceptor *component.Interceptor) error {
	if app.handlerService == nil {
		return service.ErrHandlerServiceUnavailable
	}
	return app.handlerService.RegisterInterceptor(serviceName, interceptor)
}
func (app *App) RegisterRemoteInterceptor(serviceName string, interceptor *component.Interceptor) error {
	if app.remoteService == nil {
		return service.ErrRemoteServiceUnavailable
	}
	return app.remoteService.RegisterInterceptor(serviceName, interceptor)
}

// Value components can be non-comparable; compare their reference-bearing fields by identity.
func sameComponentValue(a, b reflect.Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	if a.Comparable() {
		return a.Equal(b)
	}

	switch a.Kind() {
	case reflect.Array:
		for i := 0; i < a.Len(); i++ {
			if !sameComponentValue(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() && b.IsNil()
		}
		return sameComponentValue(a.Elem(), b.Elem())
	case reflect.Map:
		return a.Pointer() == b.Pointer()
	case reflect.Slice:
		return a.Pointer() == b.Pointer() && a.Len() == b.Len() && a.Cap() == b.Cap()
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if !sameComponentValue(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func sameComponent(a, b component.Component) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sameComponentValue(reflect.ValueOf(a), reflect.ValueOf(b))
}

func uniqueComponents(group []regComp, seen *[]component.Component) []component.Component {
	components := make([]component.Component, 0)
	for _, registered := range group {
		comp := registered.comp
		duplicate := false
		for _, existing := range *seen {
			if sameComponent(existing, comp) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		*seen = append(*seen, comp)
		components = append(components, comp)
	}
	return components
}

func componentRegistrations(registrations []regComp) []service.ComponentRegistration {
	result := make([]service.ComponentRegistration, 0, len(registrations))
	for _, registration := range registrations {
		result = append(result, service.ComponentRegistration{
			Component: registration.comp,
			Options:   registration.opts,
		})
	}
	return result
}

func componentLifecycleName(comp component.Component) string {
	if comp == nil {
		return "<nil>"
	}
	typeOf := reflect.TypeOf(comp)
	for typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	if typeOf.Name() != "" {
		return typeOf.Name()
	}
	return reflect.TypeOf(comp).String()
}

func (app *App) startupComponents() error {
	if app.handlerService == nil && len(app.handlerComp) > 0 {
		return service.ErrHandlerServiceUnavailable
	}
	if app.remoteService == nil && len(app.remoteComp) > 0 {
		return service.ErrRemoteServiceUnavailable
	}

	var preparedHandlers *service.PreparedHandlerRegistrations
	var preparedRemotes *service.PreparedRemoteRegistrations
	var err error
	if len(app.handlerComp) > 0 {
		preparedHandlers, err = app.handlerService.PrepareRegistrations(componentRegistrations(app.handlerComp))
		if err != nil {
			return fmt.Errorf("preflight handler components: %w", err)
		}
	}
	if len(app.remoteComp) > 0 {
		preparedRemotes, err = app.remoteService.PrepareRegistrations(componentRegistrations(app.remoteComp))
		if err != nil {
			return fmt.Errorf("preflight remote components: %w", err)
		}
	}

	journal := app.ensureStartupJournal()
	seen := make([]component.Component, 0, len(app.handlerComp)+len(app.remoteComp))
	for groupIndex, group := range [][]regComp{app.handlerComp, app.remoteComp} {
		components := uniqueComponents(group, &seen)
		for _, comp := range components {
			if err := app.startupCancellationError(); err != nil {
				return err
			}
			name := componentLifecycleName(comp)
			if err := callLifecycleHook("component", name, "Init", comp.Init); err != nil {
				return err
			}
			journal.recordComponent(groupIndex, comp, name)
		}
		for _, comp := range components {
			if err := app.startupCancellationError(); err != nil {
				return err
			}
			name := componentLifecycleName(comp)
			if err := callLifecycleHook("component", name, "AfterInit", comp.AfterInit); err != nil {
				return err
			}
		}
	}

	if err := service.CommitPreparedComponentRegistrations(
		app.handlerService,
		preparedHandlers,
		app.remoteService,
		preparedRemotes,
	); err != nil {
		return fmt.Errorf("commit components: %w", err)
	}

	if app.handlerService != nil {
		app.handlerService.DumpServices()
	}
	if app.remoteService != nil {
		app.remoteService.DumpServices()
	}
	return nil
}

func (app *App) shutdownComponents() error {
	if app.startupJournal == nil {
		return nil
	}
	return app.startupJournal.shutdownComponents()
}
