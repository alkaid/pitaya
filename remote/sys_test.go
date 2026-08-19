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

package remote

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/cluster"
	clustermocks "github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/service"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func assertSessionNotFound(t *testing.T, err error, metadata map[string]string) {
	t.Helper()

	var apiErr *apierrors.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, protos.ErrSessionNotFound().GetCode(), apiErr.GetCode())
	assert.Equal(t, protos.ErrSessionNotFound().GetReason(), apiErr.GetReason())
	assert.Equal(t, metadata, apiErr.GetMetadata())
}

type compareRemoveSession struct {
	session.Session
	remove func(serverType, serverID string) bool
}

func (s *compareRemoveSession) RemoveBackendIDIfEqual(serverType, serverID string) bool {
	return s.remove(serverType, serverID)
}

type backendCoordinatorPool struct {
	session.SessionPool
	coordinator *session.BackendCoordinator
}

func (p *backendCoordinatorPool) SetBackendCoordinator(coordinator session.BackendCoordinator) {
	*p.coordinator = coordinator
}

type standaloneSessionCache struct {
	session.CacheInterface
	loaded map[string]string
	writes map[string]string
}

func (c *standaloneSessionCache) Hgetall(string) (map[string]string, error) {
	return c.loaded, nil
}

func (c *standaloneSessionCache) Hmset(_ string, fields map[string]string) error {
	c.writes = fields
	return nil
}

func (c *standaloneSessionCache) CacheTTL() time.Duration {
	return time.Minute
}

func TestStandaloneFrontendBindSkipsClusterBroadcast(t *testing.T) {
	t.Parallel()

	pool := session.NewSessionPool()
	cache := &standaloneSessionCache{
		loaded: map[string]string{},
	}
	pool.SetClusterCache(cache)

	server := cluster.NewServer("frontend-1", "connector", true)
	NewSys(pool, server, nil, nil, nil).Init()

	sess, isNew := pool.NewSession(nil, true)
	require.True(t, isNew)

	customData := map[string]any{"source": "standalone"}
	require.NoError(t, sess.Bind(context.Background(), "user-1", customData, nil))

	assert.Same(t, sess, pool.GetSessionByUID("user-1"))
	assert.Equal(t, customData, sess.GetData())
	require.NotNil(t, cache.writes)
	assert.Equal(t, "1", cache.writes["o"])
}

func TestBindSession(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	id := int64(1)
	uid := uuid.New().String()
	customData := map[string]interface{}{
		"hello": "test",
	}
	d, err := json.Marshal(customData)
	require.NoError(t, err)
	metadata := map[string]string{"source": "test"}

	ss := mocks.NewMockSession(ctrl)

	sessionPool := mocks.NewMockSessionPool(ctrl)
	gomock.InOrder(
		sessionPool.EXPECT().GetSessionByID(id).Return(ss),
		ss.EXPECT().SetDataEncoded(d).Return(nil),
		ss.EXPECT().GetData().Return(customData),
		ss.EXPECT().Bind(nil, uid, customData, metadata).Return(nil),
	)

	s := NewSys(sessionPool, nil, nil, nil, nil)
	msg := &protos.BindMsg{
		Uid: uid,
		Fid: "frontend-1",
		Sid: id,
		Session: &protos.Session{
			Id:   id,
			Uid:  uid,
			Data: d,
		},
		Metadata: metadata,
	}

	res, err := s.BindSession(nil, msg)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []byte("ack"), res.Data)
}

func TestBindSessionShouldErrorIfNotExists(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	uid := uuid.New().String()
	d, err := json.Marshal(map[string]interface{}{
		"hello": "test",
	})
	require.NoError(t, err)

	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().GetSessionByID(gomock.Any()).Return(nil)

	s := NewSys(sessionPool, nil, nil, nil, nil)
	msg := &protos.BindMsg{
		Uid: uid,
		Fid: "frontend-1",
		Sid: 133,
		Session: &protos.Session{
			Id:   133,
			Uid:  uid,
			Data: d,
		},
	}
	res, err := s.BindSession(nil, msg)
	assert.Nil(t, res)
	assertSessionNotFound(t, err, map[string]string{
		"uid": uid,
		"sid": "133",
		"fid": "frontend-1",
	})
}

func TestBindSessionShouldErrorIfAlreadyBound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	id := int64(1)
	uid := uuid.New().String()
	customData := map[string]interface{}{
		"hello": "test",
	}
	d, err := json.Marshal(customData)
	require.NoError(t, err)
	metadata := map[string]string{"source": "test"}
	msg := &protos.BindMsg{
		Uid: uid,
		Fid: "frontend-1",
		Sid: id,
		Session: &protos.Session{
			Id:   id,
			Uid:  uid,
			Data: d,
		},
		Metadata: metadata,
	}

	ss := mocks.NewMockSession(ctrl)
	sessionPool := mocks.NewMockSessionPool(ctrl)
	gomock.InOrder(
		sessionPool.EXPECT().GetSessionByID(id).Return(ss),
		ss.EXPECT().SetDataEncoded(d).Return(nil),
		ss.EXPECT().GetData().Return(customData),
		ss.EXPECT().Bind(nil, uid, customData, metadata).Return(nil),
		sessionPool.EXPECT().GetSessionByID(id).Return(ss),
		ss.EXPECT().SetDataEncoded(d).Return(nil),
		ss.EXPECT().GetData().Return(customData),
		ss.EXPECT().Bind(nil, uid, customData, metadata).Return(constants.ErrSessionAlreadyBound),
	)

	s := NewSys(sessionPool, nil, nil, nil, nil)
	res, err := s.BindSession(nil, msg)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []byte("ack"), res.Data)

	res, err = s.BindSession(nil, msg)
	assert.Nil(t, res)
	assert.EqualError(t, constants.ErrSessionAlreadyBound, err.Error())
}

func TestPushSession(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	id := int64(1)
	uid := uuid.New().String()
	d, err := json.Marshal(map[string]interface{}{
		"hello":   "test",
		"hello22": 2,
	})
	data := &protos.Session{
		Id:   id,
		Uid:  uid,
		Data: d,
	}
	require.NoError(t, err)

	ss := mocks.NewMockSession(ctrl)
	sessionPool := mocks.NewMockSessionPool(ctrl)
	gomock.InOrder(
		sessionPool.EXPECT().GetSessionByID(id).Return(ss),
		ss.EXPECT().UID().Return(uid),
		ss.EXPECT().SetDataEncoded(d).Return(nil),
		ss.EXPECT().FlushUserData().Return(nil),
	)

	s := NewSys(sessionPool, nil, nil, nil, nil)
	res, err := s.PushSession(nil, data)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []byte("ack"), res.Data)
}

func TestPushSessionShouldFailIfSessionDoesntExists(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	uid := uuid.New().String()
	d, err := json.Marshal(map[string]interface{}{
		"hello":   "test",
		"hello22": 2,
	})
	require.NoError(t, err)
	data := &protos.Session{
		Id:   343,
		Uid:  uid,
		Data: d,
	}

	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().GetSessionByID(data.Id).Return(nil).Times(1)

	s := NewSys(sessionPool, nil, nil, nil, nil)
	res, err := s.PushSession(nil, data)
	assert.Nil(t, res)
	assertSessionNotFound(t, err, map[string]string{
		"uid": uid,
		"sid": "343",
	})
}

func TestKick(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	uid := uuid.New().String()
	reason := session.CloseReasonKickManual
	metadata := map[string]string{"source": "test"}

	ss := mocks.NewMockSession(ctrl)
	ss.EXPECT().Kick(nil, metadata, reason).Return(nil)

	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().GetSessionByUID(uid).Return(ss).Times(1)

	s := NewSys(sessionPool, nil, nil, nil, nil)

	res, err := s.Kick(nil, &protos.KickMsg{UserId: uid, Reason: int32(reason), Metadata: metadata})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Kicked)
}

func TestKickSessionShouldFailIfSessionDoesntExists(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	uid := uuid.New().String()
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().GetSessionByUID(uid).Return(nil).Times(1)

	s := NewSys(sessionPool, nil, nil, nil, nil)
	res, err := s.Kick(nil, &protos.KickMsg{UserId: uid})
	require.NotNil(t, res)
	assert.False(t, res.Kicked)
	assertSessionNotFound(t, err, map[string]string{
		"uid":    uid,
		"reason": "0",
	})
}

func TestBindBackendRollbackUsesBindingIdentity(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	server := cluster.NewServer("game-1", "game", false)
	rollbackErr := errors.New("store session failed")
	uid := uuid.New().String()

	var coordinator session.BackendCoordinator
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().OnSessionBind(gomock.Any())
	sessionPool.EXPECT().OnSessionClose(gomock.Any())

	sys := NewSys(&backendCoordinatorPool{SessionPool: sessionPool, coordinator: &coordinator}, server, nil, nil, nil)
	sys.Init()
	require.NotNil(t, coordinator.Bind)

	ss := mocks.NewMockSession(ctrl)
	ss.EXPECT().UID().Return(uid).AnyTimes()
	ss.EXPECT().ID().Return(int64(1)).AnyTimes()
	sessionPool.EXPECT().GetSessionByUID(uid).Return(nil)
	sessionPool.EXPECT().StoreSessionLocal(ss).Return(rollbackErr)
	gomock.InOrder(
		ss.EXPECT().FlushBackendData().Return(nil),
		sessionPool.EXPECT().RemoveSessionLocal(ss),
	)
	var rollbackCalls int
	rollback := session.BackendRollback(func() bool {
		rollbackCalls++
		return true
	})

	err := coordinator.Bind(context.Background(), ss, server.Type, server.ID, nil, rollback)

	assert.ErrorIs(t, err, rollbackErr)
	assert.Equal(t, 1, rollbackCalls)
}

func TestBindBackendRetryContinuesForRetainedSession(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	server := cluster.NewServer("game-1", "game", false)
	uid := uuid.New().String()
	sendErr := errors.New("frontend unavailable")

	var coordinator session.BackendCoordinator
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().OnSessionBind(gomock.Any())
	sessionPool.EXPECT().OnSessionClose(gomock.Any())

	sys := NewSys(&backendCoordinatorPool{SessionPool: sessionPool, coordinator: &coordinator}, server, nil, nil, nil)
	sys.Init()
	require.NotNil(t, coordinator.Bind)

	ss := mocks.NewMockSession(ctrl)
	ss.EXPECT().UID().Return(uid).AnyTimes()
	ss.EXPECT().ID().Return(int64(1)).AnyTimes()
	sessionPool.EXPECT().GetSessionByUID(uid).Return(ss)
	ss.EXPECT().SendRequestToFrontend(gomock.Any(), constants.SessionBindBackendRoute, gomock.Any()).Return(nil, sendErr)
	gomock.InOrder(
		ss.EXPECT().FlushBackendData().Return(nil),
		sessionPool.EXPECT().RemoveSessionLocal(ss),
	)
	var rollbackCalls int

	err := coordinator.Bind(context.Background(), ss, server.Type, server.ID, nil, func() bool {
		rollbackCalls++
		return true
	})

	assert.ErrorIs(t, err, sendErr)
	assert.Equal(t, 1, rollbackCalls)
}

func TestBindBackendRemoteRPCSuccessStopsSourceCoordinator(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	source := cluster.NewServer("chat-1", "chat", false)
	target := cluster.NewServer("game-1", "game", false)
	uid := uuid.New().String()

	var coordinator session.BackendCoordinator
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().OnSessionBind(gomock.Any())
	sessionPool.EXPECT().OnSessionClose(gomock.Any())
	serviceDiscovery := clustermocks.NewMockServiceDiscovery(ctrl)
	rpcClient := clustermocks.NewMockRPCClient(ctrl)
	remoteService := service.NewRemoteService(rpcClient, nil, serviceDiscovery, nil, nil, nil, nil, source, sessionPool, nil, nil, nil)

	sys := NewSys(&backendCoordinatorPool{SessionPool: sessionPool, coordinator: &coordinator}, source, nil, nil, remoteService)
	sys.Init()
	require.NotNil(t, coordinator.Bind)

	ss := mocks.NewMockSession(ctrl)
	ss.EXPECT().UID().Return(uid).AnyTimes()
	serviceDiscovery.EXPECT().GetServer(target.ID).Return(target, nil)
	rpcClient.EXPECT().Call(gomock.Any(), protos.RPCType_User, gomock.Any(), ss, gomock.Any(), target).Return(&protos.Response{}, nil)
	var rollbackCalls int

	err := coordinator.Bind(context.Background(), ss, target.Type, target.ID, nil, func() bool {
		rollbackCalls++
		return true
	})

	require.NoError(t, err)
	assert.Zero(t, rollbackCalls)
}

func TestBindBackendRollbackFlushFailurePreservesLocalSession(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	server := cluster.NewServer("game-1", "game", false)
	uid := uuid.New().String()
	sendErr := errors.New("frontend unavailable")
	flushErr := errors.New("flush backend data failed")

	var coordinator session.BackendCoordinator
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().OnSessionBind(gomock.Any())
	sessionPool.EXPECT().OnSessionClose(gomock.Any())

	sys := NewSys(&backendCoordinatorPool{SessionPool: sessionPool, coordinator: &coordinator}, server, nil, nil, nil)
	sys.Init()
	require.NotNil(t, coordinator.Bind)

	ss := mocks.NewMockSession(ctrl)
	ss.EXPECT().UID().Return(uid).AnyTimes()
	ss.EXPECT().ID().Return(int64(1)).AnyTimes()
	sessionPool.EXPECT().GetSessionByUID(uid).Return(nil)
	sessionPool.EXPECT().StoreSessionLocal(ss).Return(nil)
	ss.EXPECT().SendRequestToFrontend(gomock.Any(), constants.SessionBindBackendRoute, gomock.Any()).Return(nil, sendErr)
	ss.EXPECT().FlushBackendData().Return(flushErr)
	var rollbackCalls int

	err := coordinator.Bind(context.Background(), ss, server.Type, server.ID, nil, func() bool {
		rollbackCalls++
		return true
	})

	assert.ErrorIs(t, err, sendErr)
	assert.ErrorIs(t, err, flushErr)
	assert.Equal(t, 1, rollbackCalls)
}

func TestBindBackendRejectsDifferentSessionOwner(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	server := cluster.NewServer("game-1", "game", false)
	uid := uuid.New().String()

	var coordinator session.BackendCoordinator
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().OnSessionBind(gomock.Any())
	sessionPool.EXPECT().OnSessionClose(gomock.Any())

	sys := NewSys(&backendCoordinatorPool{SessionPool: sessionPool, coordinator: &coordinator}, server, nil, nil, nil)
	sys.Init()
	require.NotNil(t, coordinator.Bind)

	ss := mocks.NewMockSession(ctrl)
	ss.EXPECT().UID().Return(uid).AnyTimes()
	existing := mocks.NewMockSession(ctrl)
	existing.EXPECT().GetBackendID(server.Type).Return("game-0")
	sessionPool.EXPECT().GetSessionByUID(uid).Return(existing)

	err := coordinator.Bind(context.Background(), ss, server.Type, server.ID, nil, func() bool {
		t.Fatal("coordinator must leave rollback to Session.BindBackend")
		return false
	})

	assert.ErrorIs(t, err, constants.ErrSessionAlreadyBound)
}

func TestKickBackendCallbackFailureDoesNotRemoveLocalSession(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	server := cluster.NewServer("game-1", "game", false)
	uid := uuid.New().String()
	sendErr := errors.New("frontend unavailable")
	flushErr := errors.New("flush backend data failed")

	var coordinator session.BackendCoordinator
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().OnSessionBind(gomock.Any())
	sessionPool.EXPECT().OnSessionClose(gomock.Any())

	sys := NewSys(&backendCoordinatorPool{SessionPool: sessionPool, coordinator: &coordinator}, server, nil, nil, nil)
	sys.Init()
	require.NotNil(t, coordinator.Kick)

	ss := mocks.NewMockSession(ctrl)
	ss.EXPECT().UID().Return(uid).AnyTimes()
	ss.EXPECT().ID().Return(int64(1)).AnyTimes()
	var removeCalls int
	sessionPool.EXPECT().RemoveSessionLocal(ss).Do(func(session.Session) {
		removeCalls++
	}).AnyTimes()
	ss.EXPECT().SendRequestToFrontend(gomock.Any(), constants.KickBackendRoute, gomock.Any()).Return(nil, sendErr)
	ss.EXPECT().FlushBackendData().Return(flushErr)

	err := coordinator.Kick(context.Background(), ss, server.Type, server.ID, nil, session.CloseReasonNormal, func() bool { return true })

	assert.ErrorIs(t, err, flushErr)
	assert.Zero(t, removeCalls)
}

func TestKickBackendFrontendRemovesMatchingBinding(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	uid := uuid.New().String()
	msg := &protos.BindBackendMsg{Uid: uid, Btype: "game", Bid: "game-1"}

	ss := mocks.NewMockSession(ctrl)
	wrapped := &compareRemoveSession{Session: ss, remove: func(serverType, serverID string) bool {
		assert.Equal(t, msg.Btype, serverType)
		assert.Equal(t, msg.Bid, serverID)
		return true
	}}
	ss.EXPECT().FlushBackendData().Return(nil)
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().GetSessionByUID(uid).Return(wrapped)

	sys := NewSys(sessionPool, cluster.NewServer("frontend-1", "frontend", true), nil, nil, nil)
	res, err := sys.KickBackend(context.Background(), msg)

	require.NoError(t, err)
	require.NotNil(t, res)
}

func TestKickBackendFrontendPreservesMismatchedBinding(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	uid := uuid.New().String()
	msg := &protos.BindBackendMsg{Uid: uid, Btype: "game", Bid: "game-1"}

	ss := mocks.NewMockSession(ctrl)
	wrapped := &compareRemoveSession{Session: ss, remove: func(serverType, serverID string) bool {
		assert.Equal(t, msg.Btype, serverType)
		assert.Equal(t, msg.Bid, serverID)
		return false
	}}
	ss.EXPECT().FlushBackendData().Return(nil)
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().GetSessionByUID(uid).Return(wrapped)

	sys := NewSys(sessionPool, cluster.NewServer("frontend-1", "frontend", true), nil, nil, nil)
	res, err := sys.KickBackend(context.Background(), msg)

	require.NoError(t, err)
	require.NotNil(t, res)
}

func TestSessionKickedBackendUsesBindingIdentity(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	// The synchronous binding mutation happens before listener submission is rejected.
	uid := ""
	msg := &protos.BindBackendMsg{Uid: uid, Btype: "game", Bid: "game-1"}

	ss := mocks.NewMockSession(ctrl)
	wrapped := &compareRemoveSession{Session: ss, remove: func(serverType, serverID string) bool {
		assert.Equal(t, msg.Btype, serverType)
		assert.Equal(t, msg.Bid, serverID)
		return false
	}}
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().GetSessionByUID(uid).Return(wrapped)

	sys := NewSys(sessionPool, cluster.NewServer("chat-1", "chat", false), nil, nil, &service.RemoteService{})
	res, err := sys.SessionKickedBackend(context.Background(), msg)

	require.ErrorIs(t, err, co.ErrInvalidUserID)
	require.Nil(t, res)
}

func TestSysPropagatesStatefulListenerSubmissionErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	sessionPool := mocks.NewMockSessionPool(ctrl)
	sessionPool.EXPECT().GetSessionByUID("0").Return(nil).Times(1)
	sys := NewSys(
		sessionPool,
		cluster.NewServer("frontend-1", "frontend", true),
		nil,
		nil,
		&service.RemoteService{},
	)

	tests := []struct {
		name string
		call func() (*protos.Response, error)
	}{
		{name: "session bound", call: func() (*protos.Response, error) {
			return sys.SessionBound(context.Background(), &protos.BindMsg{Uid: "0"})
		}},
		{name: "session closed", call: func() (*protos.Response, error) {
			return sys.SessionClosed(context.Background(), &protos.KickMsg{UserId: "0"})
		}},
		{name: "backend bound fork", call: func() (*protos.Response, error) {
			return sys.SessionBoundBackendFork(context.Background(), &protos.BindBackendMsg{Uid: "0"})
		}},
		{name: "backend bound", call: func() (*protos.Response, error) {
			return sys.SessionBoundBackend(context.Background(), &protos.BindBackendMsg{Uid: "0"})
		}},
		{name: "backend kicked", call: func() (*protos.Response, error) {
			return sys.SessionKickedBackend(context.Background(), &protos.BindBackendMsg{Uid: "0"})
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := test.call()
			require.ErrorIs(t, err, co.ErrInvalidUserID)
			assert.Equal(t, 500, apierrors.Code(err))
			assert.Equal(t, "task submission failed", apierrors.FromError(err).Status.Message)
			assert.NotContains(t, apierrors.FromError(err).Status.Message, co.ErrInvalidUserID.Error())
			require.Nil(t, response)
		})
	}
}

func TestSysPropagatesStatelessListenerSubmissionErrors(t *testing.T) {
	ants.Release()
	t.Cleanup(ants.Reboot)
	sys := NewSys(nil, nil, nil, nil, &service.RemoteService{})

	response, err := sys.SessionClosed(context.Background(), &protos.KickMsg{})
	require.ErrorIs(t, err, ants.ErrPoolClosed)
	assert.Equal(t, 503, apierrors.Code(err))
	assert.Equal(t, "task submission unavailable", apierrors.FromError(err).Status.Message)
	require.Nil(t, response)

	response, err = sys.SessionBoundBackendFork(context.Background(), &protos.BindBackendMsg{})
	require.ErrorIs(t, err, ants.ErrPoolClosed)
	assert.Equal(t, 503, apierrors.Code(err))
	assert.Equal(t, "task submission unavailable", apierrors.FromError(err).Status.Message)
	require.Nil(t, response)
}
