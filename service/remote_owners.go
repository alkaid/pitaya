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

package service

import (
	"fmt"
	"sync"

	goerrors "github.com/alkaid/goerrors/errors"
	"github.com/samber/lo"
	"k8s.io/utils/strings/slices"

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/docgenerator"
	"github.com/alkaid/pitaya/v2/logger"
	"go.uber.org/zap"
)

// remoteRegistryOwner owns the component registry and its synchronization.
// Handler execution only keeps a pointer snapshot, so registration never holds
// the lock while user code runs.
type remoteRegistryOwner struct {
	mu           sync.RWMutex
	services     map[string]*component.Service
	subscribers  map[string]*component.Service
	remotes      map[string]*component.Remote
	interceptors map[string]*component.Interceptor
}

func newRemoteRegistryOwner() remoteRegistryOwner {
	return remoteRegistryOwner{
		services:     make(map[string]*component.Service),
		subscribers:  make(map[string]*component.Service),
		remotes:      make(map[string]*component.Remote),
		interceptors: make(map[string]*component.Interceptor),
	}
}

func (o *remoteRegistryOwner) lookupInterceptor(service string) (*component.Interceptor, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	interceptor, ok := o.interceptors[service]
	return interceptor, ok
}

func (o *remoteRegistryOwner) lookupRemote(route string) (*component.Remote, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	remote, ok := o.remotes[route]
	return remote, ok
}

func (o *remoteRegistryOwner) remoteNames() []string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	names := make([]string, 0, len(o.remotes))
	for name := range o.remotes {
		names = append(names, name)
	}
	return names
}

func (o *remoteRegistryOwner) servicesSnapshot() map[string]*component.Service {
	o.mu.RLock()
	defer o.mu.RUnlock()
	services := make(map[string]*component.Service, len(o.services))
	for name, service := range o.services {
		services[name] = service
	}
	return services
}

// remoteListenerOwner owns listener publication. Callers receive snapshots so
// callbacks can register more listeners without deadlocking an active dispatch.
type remoteListenerOwner struct {
	mu                     sync.RWMutex
	remoteBindingListeners []cluster.RemoteBindingListener
	remoteSessionListeners []cluster.RemoteSessionListener
}

func (o *remoteListenerOwner) bindingSnapshot() []cluster.RemoteBindingListener {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return append([]cluster.RemoteBindingListener(nil), o.remoteBindingListeners...)
}

func (o *remoteListenerOwner) sessionSnapshot() []cluster.RemoteSessionListener {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return append([]cluster.RemoteSessionListener(nil), o.remoteSessionListeners...)
}

// AddRemoteBindingListener 添加绑定发生时的回调，内部使用，仅用于相同serverType间的广播
func (r *RemoteService) AddRemoteBindingListener(bindingListener cluster.RemoteBindingListener) {
	r.remoteListenerOwner.mu.Lock()
	defer r.remoteListenerOwner.mu.Unlock()
	r.remoteBindingListeners = append(r.remoteBindingListeners, bindingListener)
}

// AddRemoteSessionListener 添加session各个生命周期完成时的回调
func (r *RemoteService) AddRemoteSessionListener(sessionListener cluster.RemoteSessionListener) {
	r.remoteListenerOwner.mu.Lock()
	defer r.remoteListenerOwner.mu.Unlock()
	r.remoteSessionListeners = append(r.remoteSessionListeners, sessionListener)
}

func (r *RemoteService) GetRemoteSessionListener() []cluster.RemoteSessionListener {
	return r.remoteListenerOwner.sessionSnapshot()
}

func (r *RemoteService) GetRemoteBindingListener() []cluster.RemoteBindingListener {
	return r.remoteListenerOwner.bindingSnapshot()
}

// Register registers components
func (r *RemoteService) Register(comp component.Component, opts []component.Option) error {
	s := component.NewService(comp, opts)
	r.remoteRegistryOwner.mu.Lock()
	defer r.remoteRegistryOwner.mu.Unlock()
	services := lo.If(s.Options.Subscriber, r.subscribers).Else(r.services)

	if _, ok := services[s.Name]; ok {
		return goerrors.WithStack(fmt.Errorf("remote: service already defined: %s", s.Name))
	}

	if err := s.ExtractRemote(); err != nil {
		return err
	}

	services[s.Name] = s
	// register all remotes
	for name, remote := range s.Remotes {
		remoteName := fmt.Sprintf("%s.%s", s.Name, name)
		if _, ok := r.remotes[remoteName]; ok {
			logger.Zap.Warn("remote already registered, overwriting", zap.String("name", remoteName))
		}
		r.remotes[remoteName] = remote
	}
	if s.Options.Subscriber {
		// 注册订阅信息到服务发现，供后续同步订阅时使用
		for name := range s.Remotes {
			fork := s.Options.ForkAll
			if len(s.Options.ForkMethods) > 0 {
				fork = slices.Contains(s.Options.ForkMethods, name)
			}
			r.server.AddSubscribe(s.Name, name, fork)
		}
	}
	return nil
}

// RegisterInterceptor 注册拦截分发器,优先级别高于 component.Remote
//
//	@receiver r
//	@param serviceName
//	@param interceptor
func (r *RemoteService) RegisterInterceptor(serviceName string, interceptor *component.Interceptor) {
	r.remoteRegistryOwner.mu.Lock()
	defer r.remoteRegistryOwner.mu.Unlock()
	r.interceptors[serviceName] = interceptor
}

// DumpServices outputs all registered services
func (r *RemoteService) DumpServices() {
	for _, name := range r.remoteRegistryOwner.remoteNames() {
		logger.Log.Infof("registered remote %s", name)
	}
}

// Docs returns documentation for remotes
func (r *RemoteService) Docs(getPtrNames bool) (map[string]interface{}, error) {
	if r == nil {
		return map[string]interface{}{}, nil
	}
	return docgenerator.RemotesDocs(r.server.Type, r.remoteRegistryOwner.servicesSnapshot(), getPtrNames)
}
