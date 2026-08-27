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
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/util"
	"go.uber.org/zap"
)

type sessionIdentity struct {
	uid    string
	uidInt int64
	online bool
}

// sessionLifecycleOwner owns identity publication, bind/close admission and
// resources that must be detached exactly once when the session closes.
type sessionLifecycleOwner struct {
	lifecycleMutex   sync.RWMutex
	binding          bool
	closed           bool
	identity         atomic.Pointer[sessionIdentity]
	registeredUID    string
	lastTime         int64
	OnCloseCallbacks []func()
}

// GetOnCloseCallbacks ...
func (s *sessionImpl) GetOnCloseCallbacks() []func() {
	s.lifecycleMutex.RLock()
	defer s.lifecycleMutex.RUnlock()
	return append([]func(){}, s.OnCloseCallbacks...)
}

// SetOnCloseCallbacks ...
func (s *sessionImpl) SetOnCloseCallbacks(callbacks []func()) {
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	s.OnCloseCallbacks = append([]func(){}, callbacks...)
}

// UID returns uid that bind to current session
func (s *sessionImpl) UID() string {
	return s.identitySnapshot().uid
}

func (s *sessionImpl) UIDInt() int64 {
	return s.identitySnapshot().uidInt
}

// OnClose adds the function it receives to the callbacks that will be called
// when the session is closed
func (s *sessionImpl) OnClose(c func()) error {
	if !s.GetIsFrontend() {
		return constants.ErrOnCloseBackend
	}
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	s.OnCloseCallbacks = append(s.OnCloseCallbacks, c)
	return nil
}

// Close terminates current session, session related data will not be released,
// all related data should be cleared explicitly in Session closed callback
func (s *sessionImpl) Close(callback map[string]string, reason ...CloseReason) {
	_ = s.closeOnce(callback, reason...)
}

func (s *sessionImpl) closeOnce(callback map[string]string, reason ...CloseReason) error {
	uid, ok := s.beginClose()
	if !ok {
		return nil
	}
	logger.Zap.Debug("session close", zap.Int64("id", s.ID()), zap.String("uid", uid))
	s.pool.removeSessionLocal(uid, s.id, s)
	return s.entity.Close(callback, reason...)
}

func (s *sessionImpl) prepareBind(uid string) (bool, error) {
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	if s.closed {
		return false, constants.ErrCloseClosedSession
	}
	if s.binding {
		return false, fmt.Errorf("%w,uid=%s", constants.ErrSessionAlreadyBound, uid)
	}
	identity := s.identitySnapshot()
	if identity.uid != "" {
		if identity.uid != uid {
			return false, fmt.Errorf("%w,uid=%s", constants.ErrSessionAlreadyBound, uid)
		}
		s.binding = true
		return false, nil
	}
	identity.uid = uid
	identity.uidInt = util.ForceIdStrToInt(uid)
	s.publishIdentityLocked(identity)
	s.binding = true
	return true, nil
}

func (s *sessionImpl) finishBind(uid string, newBinding bool, succeeded bool) {
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	s.binding = false
	identity := s.identitySnapshot()
	if !succeeded && newBinding && !s.closed && identity.uid == uid {
		identity.uid = ""
		identity.uidInt = 0
		s.publishIdentityLocked(identity)
	}
}

func (s *sessionImpl) registerSessionByUID(uid string) (Session, bool, error) {
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	if s.closed {
		return nil, false, constants.ErrCloseClosedSession
	}
	identity := s.identitySnapshot()
	if identity.uid == "" {
		return nil, false, constants.ErrEmptyUID
	}
	if identity.uid != uid {
		return nil, false, fmt.Errorf("%w,uid=%s", constants.ErrSessionAlreadyBound, uid)
	}

	s.pool.sessionsMutex.Lock()
	defer s.pool.sessionsMutex.Unlock()
	if s.registeredUID != "" && s.registeredUID != uid {
		s.pool.removeSessionByUIDLocked(s.registeredUID, s)
	}
	previous, loaded := s.pool.storeSessionByUIDLocked(uid, s)
	s.registeredUID = uid
	return previous, loaded, nil
}

func (s *sessionImpl) isClosed() bool {
	s.lifecycleMutex.RLock()
	defer s.lifecycleMutex.RUnlock()
	return s.closed
}

func (s *sessionImpl) beginClose() (string, bool) {
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	if s.closed {
		return "", false
	}
	s.closed = true
	identity := s.identitySnapshot()
	identity.online = false
	s.publishIdentityLocked(identity)
	registeredUID := s.registeredUID
	s.registeredUID = ""
	return registeredUID, true
}

func (s *sessionImpl) setOnline(online bool) {
	s.lifecycleMutex.Lock()
	if !s.closed || !online {
		identity := s.identitySnapshot()
		if identity.online != online {
			identity.online = online
			s.publishIdentityLocked(identity)
		}
	}
	s.lifecycleMutex.Unlock()
}

func (s *sessionImpl) identitySnapshot() sessionIdentity {
	if identity := s.identity.Load(); identity != nil {
		return *identity
	}
	return sessionIdentity{}
}

// publishIdentityLocked publishes all hot-read identity fields as one version.
// The caller must hold lifecycleMutex for writes after construction.
func (s *sessionImpl) publishIdentityLocked(identity sessionIdentity) {
	s.identity.Store(&identity)
}

func (s *sessionImpl) lifecycleState() (string, int64, bool) {
	identity := s.identitySnapshot()
	return identity.uid, identity.uidInt, identity.online
}

func (s *sessionImpl) Online() bool {
	return s.identitySnapshot().online
}
