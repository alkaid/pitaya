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
	"errors"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/cluster"
	clustermocks "github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/service"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type bindingEvent struct {
	kind       string
	uid        string
	frontendID string
	serverType string
	serverID   string
}

type recordingBindingListener struct {
	events chan bindingEvent
}

func (l *recordingBindingListener) OnUserBind(uid, frontendID string) {
	l.events <- bindingEvent{kind: "frontend", uid: uid, frontendID: frontendID}
}

func (l *recordingBindingListener) OnUserBindBackend(uid, serverType, serverID string) {
	l.events <- bindingEvent{kind: "backend", uid: uid, serverType: serverType, serverID: serverID}
}

type sessionEvent struct {
	kind       string
	uid        string
	frontendID string
	serverType string
	serverID   string
	metadata   map[string]string
}

type recordingSessionListener struct {
	events chan sessionEvent
}

func (l *recordingSessionListener) OnUserBound(_ context.Context, uid, frontendID string, metadata map[string]string) {
	l.events <- sessionEvent{kind: "bound", uid: uid, frontendID: frontendID, metadata: metadata}
}

func (l *recordingSessionListener) OnUserDisconnected(_ context.Context, uid string, metadata map[string]string) {
	l.events <- sessionEvent{kind: "disconnected", uid: uid, metadata: metadata}
}

func (l *recordingSessionListener) OnUserBoundBackend(_ context.Context, uid, serverType, serverID string, metadata map[string]string) {
	l.events <- sessionEvent{kind: "backend-bound", uid: uid, serverType: serverType, serverID: serverID, metadata: metadata}
}

func (l *recordingSessionListener) OnUserUnboundBackend(_ context.Context, uid, serverType, serverID string, metadata map[string]string) {
	l.events <- sessionEvent{kind: "backend-unbound", uid: uid, serverType: serverType, serverID: serverID, metadata: metadata}
}

func receiveBindingEvent(t *testing.T, events <-chan bindingEvent) bindingEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for binding listener")
		return bindingEvent{}
	}
}

func receiveSessionEvent(t *testing.T, events <-chan sessionEvent) sessionEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for session listener")
		return sessionEvent{}
	}
}

func TestBindBackendSessionFrontend(t *testing.T) {
	uid := "user-frontend"
	msg := &protos.BindBackendMsg{
		Uid:      uid,
		Btype:    "game",
		Bid:      "game-1",
		Metadata: map[string]string{"source": "test"},
	}

	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		sess := mocks.NewMockSession(ctrl)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(uid).Return(sess)
		sess.EXPECT().SetBackendID(msg.Btype, msg.Bid)
		sess.EXPECT().FlushBackendData().Return(nil)

		sys := NewSys(pool, cluster.NewServer("frontend-1", "frontend", true), nil, nil, nil)
		response, err := sys.BindBackendSession(context.Background(), msg)

		require.NoError(t, err)
		require.NotNil(t, response)
	})

	t.Run("missing session", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(uid).Return(nil)

		sys := NewSys(pool, cluster.NewServer("frontend-1", "frontend", true), nil, nil, nil)
		response, err := sys.BindBackendSession(context.Background(), msg)

		require.Nil(t, response)
		assertSessionNotFound(t, err, map[string]string{"uid": uid, "bid": msg.Bid, "btype": msg.Btype})
	})

	t.Run("flush failure", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		flushErr := errors.New("flush backend failed")
		sess := mocks.NewMockSession(ctrl)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(uid).Return(sess)
		sess.EXPECT().SetBackendID(msg.Btype, msg.Bid)
		sess.EXPECT().FlushBackendData().Return(flushErr)

		sys := NewSys(pool, cluster.NewServer("frontend-1", "frontend", true), nil, nil, nil)
		response, err := sys.BindBackendSession(context.Background(), msg)

		require.Nil(t, response)
		assert.ErrorIs(t, err, flushErr)
	})
}

func TestBindBackendSessionBackend(t *testing.T) {
	server := cluster.NewServer("game-1", "game", false)
	msg := &protos.BindBackendMsg{
		Uid:      "user-backend",
		Btype:    server.Type,
		Bid:      server.ID,
		Metadata: map[string]string{"source": "test"},
	}

	t.Run("existing session", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		sess := mocks.NewMockSession(ctrl)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(msg.Uid).Return(sess)
		sess.EXPECT().BindBackend(gomock.Any(), msg.Btype, msg.Bid, msg.Metadata).Return(nil)

		sys := NewSys(pool, server, nil, nil, nil)
		response, err := sys.BindBackendSession(context.Background(), msg)

		require.NoError(t, err)
		assert.Equal(t, []byte("ack"), response.Data)
	})

	t.Run("session restored from context", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		sess := mocks.NewMockSession(ctrl)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(msg.Uid).Return(nil)
		sess.EXPECT().UID().Return(msg.Uid)
		sess.EXPECT().BindBackend(gomock.Any(), msg.Btype, msg.Bid, msg.Metadata).Return(nil)
		ctx := context.WithValue(context.Background(), constants.SessionCtxKey, sess)

		sys := NewSys(pool, server, nil, nil, nil)
		response, err := sys.BindBackendSession(ctx, msg)

		require.NoError(t, err)
		assert.Equal(t, []byte("ack"), response.Data)
	})

	t.Run("context uid mismatch", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		sess := mocks.NewMockSession(ctrl)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(msg.Uid).Return(nil)
		sess.EXPECT().UID().Return("another-user").Times(2)
		ctx := context.WithValue(context.Background(), constants.SessionCtxKey, sess)

		sys := NewSys(pool, server, nil, nil, nil)
		response, err := sys.BindBackendSession(ctx, msg)

		require.Nil(t, response)
		assertSessionNotFound(t, err, map[string]string{
			"uid": msg.Uid, "sessUid": "another-user", "bid": msg.Bid, "btype": msg.Btype,
		})
	})

	t.Run("missing session", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(msg.Uid).Return(nil)

		sys := NewSys(pool, server, nil, nil, nil)
		response, err := sys.BindBackendSession(context.Background(), msg)

		require.Nil(t, response)
		assertSessionNotFound(t, err, map[string]string{"uid": msg.Uid, "bid": msg.Bid, "btype": msg.Btype})
	})

	t.Run("bind failure", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		bindErr := errors.New("bind backend failed")
		sess := mocks.NewMockSession(ctrl)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(msg.Uid).Return(sess)
		sess.EXPECT().BindBackend(gomock.Any(), msg.Btype, msg.Bid, msg.Metadata).Return(bindErr)

		sys := NewSys(pool, server, nil, nil, nil)
		response, err := sys.BindBackendSession(context.Background(), msg)

		require.Nil(t, response)
		assert.ErrorIs(t, err, bindErr)
	})

	t.Run("wrong target", func(t *testing.T) {
		sys := NewSys(nil, server, nil, nil, nil)
		response, err := sys.BindBackendSession(context.Background(), &protos.BindBackendMsg{
			Uid: msg.Uid, Btype: msg.Btype, Bid: "game-2",
		})

		require.Nil(t, response)
		assert.ErrorIs(t, err, constants.ErrIllegalBindBackendID)
	})
}

func TestKickBackendBackend(t *testing.T) {
	server := cluster.NewServer("game-1", "game", false)
	msg := &protos.BindBackendMsg{
		Uid:      "user-backend",
		Btype:    server.Type,
		Bid:      server.ID,
		Metadata: map[string]string{"source": "test"},
	}

	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		sess := mocks.NewMockSession(ctrl)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(msg.Uid).Return(sess)
		sess.EXPECT().KickBackend(gomock.Any(), msg.Btype, msg.Metadata).Return(nil)

		sys := NewSys(pool, server, nil, nil, nil)
		response, err := sys.KickBackend(context.Background(), msg)

		require.NoError(t, err)
		assert.Equal(t, []byte("ack"), response.Data)
	})

	t.Run("kick failure", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		kickErr := errors.New("kick backend failed")
		sess := mocks.NewMockSession(ctrl)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(msg.Uid).Return(sess)
		sess.EXPECT().KickBackend(gomock.Any(), msg.Btype, msg.Metadata).Return(kickErr)

		sys := NewSys(pool, server, nil, nil, nil)
		response, err := sys.KickBackend(context.Background(), msg)

		assert.Equal(t, []byte("ack"), response.Data)
		assert.ErrorIs(t, err, kickErr)
	})

	t.Run("missing session", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		pool := mocks.NewMockSessionPool(ctrl)
		pool.EXPECT().GetSessionByUID(msg.Uid).Return(nil)

		sys := NewSys(pool, server, nil, nil, nil)
		response, err := sys.KickBackend(context.Background(), msg)

		require.Nil(t, response)
		assert.ErrorIs(t, err, constants.ErrSessionNotFound)
	})

	t.Run("wrong target", func(t *testing.T) {
		sys := NewSys(nil, server, nil, nil, nil)
		response, err := sys.KickBackend(context.Background(), &protos.BindBackendMsg{
			Uid: msg.Uid, Btype: "chat", Bid: server.ID,
		})

		require.Nil(t, response)
		assert.ErrorIs(t, err, constants.ErrIllegalBindBackendID)
	})
}

func TestGetSessionFromCtx(t *testing.T) {
	ctrl := gomock.NewController(t)
	sess := mocks.NewMockSession(ctrl)
	sys := &Sys{}

	assert.Nil(t, sys.getSessionFromCtx(context.Background()))
	ctx := context.WithValue(context.Background(), constants.SessionCtxKey, sess)
	assert.Same(t, sess, sys.getSessionFromCtx(ctx))
}

func TestSysSessionCloseCallbackNotifiesClusterDeterministically(t *testing.T) {
	ctrl := gomock.NewController(t)
	server := cluster.NewServer("frontend-1", "frontend", true)
	discovery := clustermocks.NewMockServiceDiscovery(ctrl)
	discovery.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{})

	var onClose session.OnSessionCloseFunc
	pool := mocks.NewMockSessionPool(ctrl)
	pool.EXPECT().OnSessionBind(gomock.Any())
	pool.EXPECT().OnSessionClose(gomock.Any()).Do(func(callback session.OnSessionCloseFunc) {
		onClose = callback
	})
	var coordinator session.BackendCoordinator
	remoteService := service.NewRemoteService(nil, nil, discovery, nil, nil, nil, nil, server, pool, nil, nil, nil)
	sys := NewSys(&backendCoordinatorPool{SessionPool: pool, coordinator: &coordinator}, server, nil, nil, remoteService)
	sys.Init()
	require.NotNil(t, onClose)

	sess := mocks.NewMockSession(ctrl)
	sess.EXPECT().ID().Return(int64(7)).AnyTimes()
	sess.EXPECT().UID().Return("user-1").AnyTimes()
	sess.EXPECT().FlushOnline().Return(nil)
	onClose(sess, map[string]string{"reason": "test"}, session.CloseReasonNormal)
}

func TestSysListenerSuccessPaths(t *testing.T) {
	coModule := co.NewStatefulPoolsModule(nil, nil)
	require.NoError(t, coModule.Init())
	t.Cleanup(func() { require.NoError(t, coModule.Shutdown()) })

	ctrl := gomock.NewController(t)
	uid := "12345"
	metadata := map[string]string{"source": "test"}
	frontendID := "frontend-1"
	serverType := "game"
	serverID := "game-1"

	bindingListener := &recordingBindingListener{events: make(chan bindingEvent, 2)}
	sessionListener := &recordingSessionListener{events: make(chan sessionEvent, 4)}
	remoteService := &service.RemoteService{}
	remoteService.AddRemoteBindingListener(bindingListener)
	remoteService.AddRemoteSessionListener(sessionListener)

	sess := mocks.NewMockSession(ctrl)
	wrapped := &compareRemoveSession{Session: sess, remove: func(gotType, gotID string) bool {
		assert.Equal(t, serverType, gotType)
		assert.Equal(t, serverID, gotID)
		return true
	}}
	pool := mocks.NewMockSessionPool(ctrl)
	pool.EXPECT().GetSessionByUID(uid).Return(sess)
	sess.EXPECT().SetFrontendData(frontendID, int64(7))
	pool.EXPECT().GetSessionByUID(uid).Return(sess)
	sess.EXPECT().SetBackendID(serverType, serverID)
	pool.EXPECT().GetSessionByUID(uid).Return(wrapped)

	sys := NewSys(pool, cluster.NewServer("chat-1", "chat", false), nil, nil, remoteService)

	response, err := sys.SessionBoundFork(context.Background(), &protos.BindMsg{Uid: uid, Fid: frontendID})
	require.NoError(t, err)
	assert.Equal(t, []byte("ack"), response.Data)
	assert.Equal(t, bindingEvent{kind: "frontend", uid: uid, frontendID: frontendID}, receiveBindingEvent(t, bindingListener.events))

	response, err = sys.SessionBound(context.Background(), &protos.BindMsg{Uid: uid, Fid: frontendID, Sid: 7, Metadata: metadata})
	require.NoError(t, err)
	assert.Equal(t, []byte("ack"), response.Data)
	assert.Equal(t, sessionEvent{kind: "bound", uid: uid, frontendID: frontendID, metadata: metadata}, receiveSessionEvent(t, sessionListener.events))

	response, err = sys.SessionClosed(context.Background(), &protos.KickMsg{UserId: uid, Metadata: metadata})
	require.NoError(t, err)
	assert.Equal(t, []byte("ack"), response.Data)
	assert.Equal(t, sessionEvent{kind: "disconnected", uid: uid, metadata: metadata}, receiveSessionEvent(t, sessionListener.events))

	backendMsg := &protos.BindBackendMsg{Uid: uid, Btype: serverType, Bid: serverID, Metadata: metadata}
	response, err = sys.SessionBoundBackendFork(context.Background(), backendMsg)
	require.NoError(t, err)
	assert.Equal(t, []byte("ack"), response.Data)
	assert.Equal(t, bindingEvent{kind: "backend", uid: uid, serverType: serverType, serverID: serverID}, receiveBindingEvent(t, bindingListener.events))

	response, err = sys.SessionBoundBackend(context.Background(), backendMsg)
	require.NoError(t, err)
	assert.Equal(t, []byte("ack"), response.Data)
	assert.Equal(t, sessionEvent{kind: "backend-bound", uid: uid, serverType: serverType, serverID: serverID, metadata: metadata}, receiveSessionEvent(t, sessionListener.events))

	response, err = sys.SessionKickedBackend(context.Background(), backendMsg)
	require.NoError(t, err)
	assert.Equal(t, []byte("ack"), response.Data)
	assert.Equal(t, sessionEvent{kind: "backend-unbound", uid: uid, serverType: serverType, serverID: serverID, metadata: metadata}, receiveSessionEvent(t, sessionListener.events))
	require.NoError(t, co.WaitWithUser(context.Background(), int64(12345), func(context.Context) {}))
}

var (
	_ cluster.RemoteBindingListener = (*recordingBindingListener)(nil)
	_ cluster.RemoteSessionListener = (*recordingSessionListener)(nil)
	_ session.Session               = (*compareRemoveSession)(nil)
)
