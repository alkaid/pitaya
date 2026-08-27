// Copyright (c) TFG Co. All Rights Reserved.
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

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/interfaces"
	"github.com/alkaid/pitaya/v2/logger"
	mods "github.com/alkaid/pitaya/v2/modules"
)

type moduleWrapper struct {
	module interfaces.Module
	name   string
}

type sessionModuleWrapper struct {
	module interfaces.SessionModule
	name   string
}

// RegisterModule registers a module, by default it register after registered modules
func (app *App) RegisterModule(module interfaces.Module, name string) error {
	return app.RegisterModuleAfter(module, name)
}

// RegisterModuleAfter registers a module after all registered modules
func (app *App) RegisterModuleAfter(module interfaces.Module, name string) error {
	if err := app.alreadyRegistered(name); err != nil {
		return err
	}

	app.modulesMap[name] = module
	app.modulesArr = append(app.modulesArr, moduleWrapper{
		module: module,
		name:   name,
	})

	if sm, ok := module.(interfaces.SessionModule); ok {
		app.sessionModulesArr = append(app.sessionModulesArr, sessionModuleWrapper{
			module: sm,
			name:   name,
		})
	}

	return nil
}

// RegisterModuleBefore registers a module before all registered modules
func (app *App) RegisterModuleBefore(module interfaces.Module, name string) error {
	if err := app.alreadyRegistered(name); err != nil {
		return err
	}

	app.modulesMap[name] = module
	app.modulesArr = append([]moduleWrapper{
		{
			module: module,
			name:   name,
		},
	}, app.modulesArr...)

	if sm, ok := module.(interfaces.SessionModule); ok {
		app.sessionModulesArr = append([]sessionModuleWrapper{
			{
				module: sm,
				name:   name,
			},
		}, app.sessionModulesArr...)
	}

	return nil
}

// GetModule gets a module with a name
func (app *App) GetModule(name string) (interfaces.Module, error) {
	if m, ok := app.modulesMap[name]; ok {
		return m, nil
	}
	return nil, fmt.Errorf("module with name %s not found", name)
}

func (app *App) alreadyRegistered(name string) error {
	if _, ok := app.modulesMap[name]; ok {
		return fmt.Errorf("module with name %s already exists", name)
	}

	return nil
}

type runtimeModulePosition uint8

const (
	runtimeModuleBefore runtimeModulePosition = iota
	runtimeModuleAfter
)

type runtimeModuleSpec struct {
	name          string
	position      runtimeModulePosition
	factory       func() interfaces.Module
	afterRegister func(interfaces.Module)
}

type preparedRuntimeModules struct {
	specs []runtimeModuleSpec
}

func isNilModule(module interfaces.Module) bool {
	if module == nil {
		return true
	}
	value := reflect.ValueOf(module)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (app *App) prepareRuntimeModules() (*preparedRuntimeModules, error) {
	names := make(map[string]struct{}, len(app.modulesArr)+6)
	for _, module := range app.modulesArr {
		if module.name == "" {
			return nil, fmt.Errorf("registered module has an empty name")
		}
		if isNilModule(module.module) {
			return nil, fmt.Errorf("module %q is nil", module.name)
		}
		if _, duplicate := names[module.name]; duplicate {
			return nil, fmt.Errorf("module with name %s already exists", module.name)
		}
		names[module.name] = struct{}{}
	}

	prepared := &preparedRuntimeModules{specs: make([]runtimeModuleSpec, 0, 6)}
	add := func(spec runtimeModuleSpec) error {
		if spec.name == "" {
			return fmt.Errorf("framework module has an empty name")
		}
		if _, duplicate := names[spec.name]; duplicate {
			return fmt.Errorf("module with name %s already exists", spec.name)
		}
		names[spec.name] = struct{}{}
		prepared.specs = append(prepared.specs, spec)
		return nil
	}

	if app.serverMode == Cluster {
		for name, module := range map[string]interfaces.Module{
			"rpcServer":        app.rpcServer,
			"rpcClient":        app.rpcClient,
			"serviceDiscovery": app.serviceDiscovery,
		} {
			if isNilModule(module) {
				return nil, fmt.Errorf("cluster module %q is not configured", name)
			}
		}
		if app.remoteService == nil {
			return nil, fmt.Errorf("cluster remote service is not configured")
		}
		if err := add(runtimeModuleSpec{
			name: "rpcServer", position: runtimeModuleBefore,
			factory: func() interfaces.Module { return app.rpcServer },
		}); err != nil {
			return nil, err
		}
		if err := add(runtimeModuleSpec{
			name: "rpcClient", position: runtimeModuleBefore,
			factory: func() interfaces.Module { return app.rpcClient },
		}); err != nil {
			return nil, err
		}
		if err := add(runtimeModuleSpec{
			name: "serviceDiscovery", position: runtimeModuleAfter,
			factory: func() interfaces.Module { return app.serviceDiscovery },
		}); err != nil {
			return nil, err
		}
		if err := add(runtimeModuleSpec{
			name: "uniqueSession", position: runtimeModuleAfter,
			factory: func() interfaces.Module {
				return mods.NewUniqueSession(app.server, app.rpcServer, app.rpcClient, app.sessionPool)
			},
			afterRegister: func(module interfaces.Module) {
				app.remoteService.AddRemoteBindingListener(module.(*mods.UniqueSession))
			},
		}); err != nil {
			return nil, err
		}
	}
	if app.conf != nil {
		if err := add(runtimeModuleSpec{
			name: "configLoader", position: runtimeModuleBefore,
			factory: func() interfaces.Module { return config.NewConfigModule(app.conf) },
		}); err != nil {
			return nil, err
		}
	}
	if err := add(runtimeModuleSpec{
		name: "statefulGoPool", position: runtimeModuleBefore,
		factory: func() interfaces.Module {
			return co.NewStatefulPoolsModule(app.config.GoPools, app.metricsReporters, app.config.Metrics.Period)
		},
	}); err != nil {
		return nil, err
	}
	// Register this last among the "before" modules because RegisterModuleBefore
	// prepends entries. The required exporter must bind before every other
	// module initializes so its failure blocks readiness without partial startup.
	if app.prometheusExporter != nil {
		if err := add(runtimeModuleSpec{
			name: "prometheusExporter", position: runtimeModuleBefore,
			factory: func() interfaces.Module { return app.prometheusExporter },
		}); err != nil {
			return nil, err
		}
	}
	return prepared, nil
}

func (app *App) commitRuntimeModules(prepared *preparedRuntimeModules) (err error) {
	if prepared == nil {
		return nil
	}
	built := make([]moduleWrapper, 0, len(prepared.specs))
	defer func() {
		if recovered := recover(); recovered != nil {
			err = recoveredLifecycleError("runtime module plan", "framework", "build", recovered)
		}
	}()
	for _, spec := range prepared.specs {
		module := spec.factory()
		if isNilModule(module) {
			return fmt.Errorf("framework module %q is nil", spec.name)
		}
		built = append(built, moduleWrapper{module: module, name: spec.name})
	}

	originalModules := append([]moduleWrapper(nil), app.modulesArr...)
	originalSessionModules := append([]sessionModuleWrapper(nil), app.sessionModulesArr...)
	originalMap := make(map[string]interfaces.Module, len(app.modulesMap))
	for name, module := range app.modulesMap {
		originalMap[name] = module
	}
	committed := false
	defer func() {
		if !committed {
			app.modulesArr = originalModules
			app.sessionModulesArr = originalSessionModules
			app.modulesMap = originalMap
		}
	}()

	for index, spec := range prepared.specs {
		module := built[index].module
		if spec.position == runtimeModuleBefore {
			err = app.RegisterModuleBefore(module, spec.name)
		} else {
			err = app.RegisterModuleAfter(module, spec.name)
		}
		if err != nil {
			return fmt.Errorf("register framework module %q: %w", spec.name, err)
		}
	}
	for index, spec := range prepared.specs {
		if spec.afterRegister != nil {
			spec.afterRegister(built[index].module)
		}
	}
	if grpcClient, ok := app.rpcClient.(*cluster.GRPCClient); ok && app.serverMode == Cluster {
		app.serviceDiscovery.AddListener(grpcClient)
	}
	committed = true
	return nil
}

// startModules starts all modules in order
func (app *App) startModules() error {
	logger.Zap.Debug("initializing all modules")
	modules := append([]moduleWrapper(nil), app.modulesArr...)
	journal := app.ensureStartupJournal()
	for _, modWrapper := range modules {
		logger.Sugar.Debugf("initializing module: %s", modWrapper.name)
		if err := callModuleInit(modWrapper.name, modWrapper.module); err != nil {
			return err
		}
		journal.recordModule(modWrapper)
	}

	for _, modWrapper := range modules {
		if err := callLifecycleHook("module", modWrapper.name, "AfterInit", modWrapper.module.AfterInit); err != nil {
			return err
		}
		logger.Sugar.Infof("module: %s successfully loaded", modWrapper.name)
	}
	return nil
}

func (app *App) startModuleSessionDraining() {
	for i := len(app.sessionModulesArr) - 1; i >= 0; i-- {
		name := app.sessionModulesArr[i].name
		mod := app.sessionModulesArr[i].module

		logger.Log.Debugf("start session draining on module: %s", name)
		mod.StartSessionDraining()
	}
}

func (app *App) maxModuleSessionCount() int64 {
	count := int64(0)
	for _, mod := range app.sessionModulesArr {
		c := mod.module.SessionCount()
		if c > count {
			count = c
		}
	}
	return count
}

// beforeShutdownModules closes module-owned admission and waits for module
// workers while the remaining module resources are still available.
func (app *App) beforeShutdownModules() error {
	app.beforeModulesOnce.Do(func() {
		if app.startupJournal != nil {
			app.beforeModulesErr = app.startupJournal.beforeShutdownModules()
		}
	})
	return app.beforeModulesErr
}

// shutdownModules releases all modules in reverse order after runtime work has
// drained. Calling it directly preserves the complete two-phase lifecycle.
func (app *App) shutdownModules() error {
	beforeErr := app.beforeShutdownModules()
	if app.startupJournal == nil {
		return beforeErr
	}
	shutdownErr := app.startupJournal.shutdownModules()
	if shutdownErr != nil {
		logger.Zap.Warn("one or more modules failed during shutdown")
	}
	return shutdownErr
}
