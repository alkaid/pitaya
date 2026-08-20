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

package session

import (
	"sync"

	"github.com/pkg/errors"
)

type backendMutationToken struct {
	serverType string
	id         string
	generation uint64
}

// backendBindings owns the backend binding map and the mutation generations
// used to make coordinator rollbacks conditional on the mutation they started.
// Its zero value is ready for use.
type backendBindings struct {
	mu          sync.RWMutex
	backends    map[string]string
	generations map[string]uint64
}

func (s *sessionImpl) GetBackends() map[string]string {
	return s.backendBindings.snapshot()
}

func (s *sessionImpl) GetBackendID(svrType string) string {
	return s.backendBindings.get(svrType)
}

func (b *backendBindings) snapshot() map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make(map[string]string, len(b.backends))
	for serverType, id := range b.backends {
		result[serverType] = id
	}
	return result
}

func (b *backendBindings) get(serverType string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.backends[serverType]
}

func (b *backendBindings) ensureLocked() {
	if b.backends == nil {
		b.backends = make(map[string]string)
	}
	if b.generations == nil {
		b.generations = make(map[string]uint64)
	}
}

func (b *backendBindings) advanceGenerationLocked(serverType string) uint64 {
	b.ensureLocked()
	b.generations[serverType]++
	return b.generations[serverType]
}

func (b *backendBindings) setIfAbsent(serverType string, id string) (string, backendMutationToken, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	if backendID := b.backends[serverType]; backendID != "" {
		return backendID, backendMutationToken{}, false
	}
	generation := b.advanceGenerationLocked(serverType)
	b.backends[serverType] = id
	return "", backendMutationToken{serverType: serverType, id: id, generation: generation}, true
}

func (s *sessionImpl) setBackendIDIfAbsent(svrType string, id string) (string, backendMutationToken, bool) {
	return s.backendBindings.setIfAbsent(svrType, id)
}

func (b *backendBindings) take(serverType string) (backendMutationToken, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	backendID := b.backends[serverType]
	if backendID == "" {
		return backendMutationToken{}, false
	}
	generation := b.advanceGenerationLocked(serverType)
	delete(b.backends, serverType)
	return backendMutationToken{serverType: serverType, id: backendID, generation: generation}, true
}

func (s *sessionImpl) takeBackendID(svrType string) (backendMutationToken, bool) {
	return s.backendBindings.take(svrType)
}

func (b *backendBindings) removeIfToken(token backendMutationToken) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	if b.generations[token.serverType] != token.generation || b.backends[token.serverType] != token.id {
		return false
	}
	b.advanceGenerationLocked(token.serverType)
	delete(b.backends, token.serverType)
	return true
}

func (s *sessionImpl) removeBackendIDIfToken(token backendMutationToken) bool {
	return s.backendBindings.removeIfToken(token)
}

func (b *backendBindings) restoreIfToken(token backendMutationToken) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	if b.generations[token.serverType] != token.generation || b.backends[token.serverType] != "" {
		return false
	}
	b.advanceGenerationLocked(token.serverType)
	b.backends[token.serverType] = token.id
	return true
}

func (s *sessionImpl) restoreBackendIDIfToken(token backendMutationToken) bool {
	return s.backendBindings.restoreIfToken(token)
}

func (b *backendBindings) removeIfEqual(serverType string, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	if backendID, ok := b.backends[serverType]; !ok || backendID != id {
		return false
	}
	b.advanceGenerationLocked(serverType)
	delete(b.backends, serverType)
	return true
}

func (s *sessionImpl) RemoveBackendIDIfEqual(svrType string, id string) bool {
	return s.backendBindings.removeIfEqual(svrType, id)
}

// CompareAndRemoveBackendID performs an atomic identity-aware backend removal.
// Sessions without the narrow capability return an error; there is no racy
// GetBackendID/RemoveBackendID fallback.
func CompareAndRemoveBackendID(s Session, svrType string, id string) (bool, error) {
	remover, ok := s.(interface {
		RemoveBackendIDIfEqual(svrType string, id string) bool
	})
	if !ok {
		return false, errors.Errorf("session %T does not support atomic backend removal", s)
	}
	return remover.RemoveBackendIDIfEqual(svrType, id), nil
}

func (s *sessionImpl) SetBackends(bs map[string]string) {
	s.backendBindings.replace(bs)
}

func (b *backendBindings) replace(backends map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()

	next := make(map[string]string, len(backends))
	affected := make(map[string]struct{}, len(b.generations)+len(backends))
	// Replacing the whole map invalidates in-flight mutations for types whose
	// slots are currently empty as well.
	for svrType := range b.generations {
		affected[svrType] = struct{}{}
	}
	for svrType := range b.backends {
		affected[svrType] = struct{}{}
	}
	for svrType, id := range backends {
		next[svrType] = id
		affected[svrType] = struct{}{}
	}
	for svrType := range affected {
		b.advanceGenerationLocked(svrType)
	}
	b.backends = next
}

func (s *sessionImpl) SetBackendID(svrType string, id string) {
	s.backendBindings.set(svrType, id)
}

func (b *backendBindings) set(serverType string, id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	b.advanceGenerationLocked(serverType)
	b.backends[serverType] = id
}

func (s *sessionImpl) RemoveBackendID(svrType string) {
	s.backendBindings.remove(svrType)
}

func (b *backendBindings) remove(serverType string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	b.advanceGenerationLocked(serverType)
	delete(b.backends, serverType)
}
