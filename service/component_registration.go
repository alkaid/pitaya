package service

import (
	"fmt"
	"reflect"

	"github.com/alkaid/pitaya/v2/component"
)

// ComponentRegistration describes one component service registration. It is
// primarily used by App startup to validate a complete batch before lifecycle
// hooks run.
type ComponentRegistration struct {
	Component component.Component
	Options   []component.Option
}

// PreparedHandlerRegistrations is an immutable, validated handler batch.
type PreparedHandlerRegistrations struct {
	services []*component.Service
}

// PreparedRemoteRegistrations is an immutable, validated remote batch.
type PreparedRemoteRegistrations struct {
	services []*component.Service
}

// CommitPreparedComponentRegistrations publishes startup handler and remote
// batches as one transaction. Both live registries are rechecked while their
// owners are held; publication starts only after every check succeeds.
func CommitPreparedComponentRegistrations(
	handler *HandlerService,
	handlers *PreparedHandlerRegistrations,
	remote *RemoteService,
	remotes *PreparedRemoteRegistrations,
) error {
	hasHandlers := handlers != nil && len(handlers.services) > 0
	hasRemotes := remotes != nil && len(remotes.services) > 0
	if hasHandlers && handler == nil {
		return ErrHandlerServiceUnavailable
	}
	if hasRemotes && remote == nil {
		return ErrRemoteServiceUnavailable
	}

	if hasHandlers {
		handler.registryMu.Lock()
		defer handler.registryMu.Unlock()
	}
	if hasRemotes {
		remote.remoteRegistryOwner.mu.Lock()
		defer remote.remoteRegistryOwner.mu.Unlock()
	}

	if hasHandlers {
		if err := handler.validatePreparedRegistrationsLocked(handlers); err != nil {
			return err
		}
	}
	if hasRemotes {
		if err := remote.validatePreparedRegistrationsLocked(remotes); err != nil {
			return err
		}
	}
	if hasHandlers {
		handler.commitPreparedRegistrationsLocked(handlers)
	}
	if hasRemotes {
		remote.commitPreparedRegistrationsLocked(remotes)
	}
	return nil
}

func validateRegistrationComponent(comp component.Component) error {
	if comp == nil {
		return fmt.Errorf("component is nil")
	}
	value := reflect.ValueOf(comp)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("component %T is nil", comp)
		}
	}
	return nil
}

// PrepareHandlerRegistrations performs reflection and batch validation without
// mutating a HandlerService or HandlerPool.
func PrepareHandlerRegistrations(registrations []ComponentRegistration) (*PreparedHandlerRegistrations, error) {
	prepared := &PreparedHandlerRegistrations{services: make([]*component.Service, 0, len(registrations))}
	names := make(map[string]struct{}, len(registrations))
	for index, registration := range registrations {
		if err := validateRegistrationComponent(registration.Component); err != nil {
			return nil, fmt.Errorf("prepare handler registration %d: %w", index, err)
		}
		service := component.NewService(registration.Component, registration.Options)
		if isReservedHandlerServiceName(service.Name) {
			return nil, fmt.Errorf("handler: service name %q is reserved", service.Name)
		}
		if _, duplicate := names[service.Name]; duplicate {
			return nil, fmt.Errorf("handler: service already defined in startup batch: %s", service.Name)
		}
		if err := service.ExtractHandler(); err != nil {
			return nil, err
		}
		names[service.Name] = struct{}{}
		prepared.services = append(prepared.services, service)
	}
	return prepared, nil
}

// PrepareRemoteRegistrations performs reflection and batch validation without
// mutating a RemoteService, Server, or subscriber registry.
func PrepareRemoteRegistrations(registrations []ComponentRegistration) (*PreparedRemoteRegistrations, error) {
	prepared := &PreparedRemoteRegistrations{services: make([]*component.Service, 0, len(registrations))}
	names := make(map[string]struct{}, len(registrations))
	for index, registration := range registrations {
		if err := validateRegistrationComponent(registration.Component); err != nil {
			return nil, fmt.Errorf("prepare remote registration %d: %w", index, err)
		}
		service := component.NewService(registration.Component, registration.Options)
		if _, duplicate := names[service.Name]; duplicate {
			return nil, fmt.Errorf("remote: service already defined in startup batch: %s", service.Name)
		}
		if err := service.ExtractRemote(); err != nil {
			return nil, err
		}
		names[service.Name] = struct{}{}
		prepared.services = append(prepared.services, service)
	}
	return prepared, nil
}
