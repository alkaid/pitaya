package pitaya

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/interfaces"
)

type startupComponent struct {
	component component.Component
	name      string
}

type startupJournal struct {
	mu sync.Mutex

	componentGroups [2][]startupComponent
	components      []startupComponent
	modules         []moduleWrapper
	ticker          *time.Ticker
	runtimeStarted  bool

	modulesBeforeDone     bool
	modulesBeforeErr      error
	modulesShutdownDone   bool
	modulesShutdownErr    error
	componentsClosed      bool
	componentsShutdownErr error
}

func (app *App) ensureStartupJournal() *startupJournal {
	if app.startupJournal == nil {
		app.startupJournal = newStartupJournal()
	}
	return app.startupJournal
}

func newStartupJournal() *startupJournal {
	return &startupJournal{}
}

func (j *startupJournal) recordComponent(group int, comp component.Component, name string) {
	entry := startupComponent{component: comp, name: name}
	j.mu.Lock()
	j.componentGroups[group] = append(j.componentGroups[group], entry)
	j.components = append(j.components, entry)
	j.mu.Unlock()
}

func (j *startupJournal) recordModule(module moduleWrapper) {
	j.mu.Lock()
	j.modules = append(j.modules, module)
	j.mu.Unlock()
}

func (j *startupJournal) setTicker(ticker *time.Ticker) {
	j.mu.Lock()
	j.ticker = ticker
	j.mu.Unlock()
}

func (j *startupJournal) stopTicker() {
	j.mu.Lock()
	ticker := j.ticker
	j.ticker = nil
	j.mu.Unlock()
	if ticker != nil {
		ticker.Stop()
	}
}

func (j *startupJournal) markRuntimeStarted() {
	j.mu.Lock()
	j.runtimeStarted = true
	j.mu.Unlock()
}

func (j *startupJournal) hasRuntimeStarted() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.runtimeStarted
}

func recoveredLifecycleError(ownerKind, ownerName, hook string, recovered any) error {
	if recoveredErr, ok := recovered.(error); ok {
		return fmt.Errorf("%s %q %s panicked: %w", ownerKind, ownerName, hook, recoveredErr)
	}
	return fmt.Errorf("%s %q %s panicked: %v", ownerKind, ownerName, hook, recovered)
}

func callLifecycleHook(ownerKind, ownerName, hook string, call func()) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = recoveredLifecycleError(ownerKind, ownerName, hook, recovered)
		}
	}()
	call()
	return nil
}

func callModuleInit(name string, module interfaces.Module) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = recoveredLifecycleError("module", name, "Init", recovered)
		}
	}()
	if err := module.Init(); err != nil {
		return fmt.Errorf("module %q Init: %w", name, err)
	}
	return nil
}

func callModuleShutdown(name string, module interfaces.Module) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = recoveredLifecycleError("module", name, "Shutdown", recovered)
		}
	}()
	if err := module.Shutdown(); err != nil {
		return fmt.Errorf("module %q Shutdown: %w", name, err)
	}
	return nil
}

func (j *startupJournal) beforeShutdownModules() error {
	j.mu.Lock()
	if j.modulesBeforeDone {
		err := j.modulesBeforeErr
		j.mu.Unlock()
		return err
	}
	j.modulesBeforeDone = true
	modules := append([]moduleWrapper(nil), j.modules...)
	j.mu.Unlock()

	var hookErrors []error
	for index := len(modules) - 1; index >= 0; index-- {
		module := modules[index]
		if err := callLifecycleHook("module", module.name, "BeforeShutdown", module.module.BeforeShutdown); err != nil {
			hookErrors = append(hookErrors, err)
		}
	}
	joined := errors.Join(hookErrors...)
	j.mu.Lock()
	j.modulesBeforeErr = joined
	j.mu.Unlock()
	return joined
}

func (j *startupJournal) shutdownModules() error {
	beforeErr := j.beforeShutdownModules()
	j.mu.Lock()
	if j.modulesShutdownDone {
		err := errors.Join(beforeErr, j.modulesShutdownErr)
		j.mu.Unlock()
		return err
	}
	j.modulesShutdownDone = true
	modules := append([]moduleWrapper(nil), j.modules...)
	j.mu.Unlock()

	var hookErrors []error
	for index := len(modules) - 1; index >= 0; index-- {
		module := modules[index]
		if err := callModuleShutdown(module.name, module.module); err != nil {
			hookErrors = append(hookErrors, err)
		}
	}
	joined := errors.Join(hookErrors...)
	j.mu.Lock()
	j.modulesShutdownErr = joined
	j.mu.Unlock()
	return errors.Join(beforeErr, joined)
}

func (j *startupJournal) closeComponents(groups [2][]startupComponent) error {
	var hookErrors []error
	for _, group := range groups {
		for index := len(group) - 1; index >= 0; index-- {
			entry := group[index]
			if err := callLifecycleHook("component", entry.name, "BeforeShutdown", entry.component.BeforeShutdown); err != nil {
				hookErrors = append(hookErrors, err)
			}
		}
		for index := len(group) - 1; index >= 0; index-- {
			entry := group[index]
			if err := callLifecycleHook("component", entry.name, "Shutdown", entry.component.Shutdown); err != nil {
				hookErrors = append(hookErrors, err)
			}
		}
	}
	return errors.Join(hookErrors...)
}

func (j *startupJournal) shutdownComponents() error {
	j.mu.Lock()
	if j.componentsClosed {
		err := j.componentsShutdownErr
		j.mu.Unlock()
		return err
	}
	j.componentsClosed = true
	groups := [2][]startupComponent{
		append([]startupComponent(nil), j.componentGroups[0]...),
		append([]startupComponent(nil), j.componentGroups[1]...),
	}
	j.mu.Unlock()

	joined := j.closeComponents(groups)
	j.mu.Lock()
	j.componentsShutdownErr = joined
	j.mu.Unlock()
	return joined
}

func (j *startupJournal) rollbackComponents() error {
	j.mu.Lock()
	if j.componentsClosed {
		err := j.componentsShutdownErr
		j.mu.Unlock()
		return err
	}
	j.componentsClosed = true
	components := append([]startupComponent(nil), j.components...)
	j.mu.Unlock()

	joined := j.closeComponents([2][]startupComponent{components, nil})
	j.mu.Lock()
	j.componentsShutdownErr = joined
	j.mu.Unlock()
	return joined
}
