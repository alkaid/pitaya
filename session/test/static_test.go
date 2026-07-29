package test

import (
	"context"
	"testing"

	"github.com/alkaid/pitaya/v2/networkentity/mocks"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestStaticGetSessionByUID(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		sessionPool := session.NewSessionPool()
		expectedSession, isNew := sessionPool.NewSession(nil, true)
		require.True(t, isNew)

		uid := uuid.New().String()
		require.NoError(t, expectedSession.Bind(context.Background(), uid, nil, nil))

		actualSession := sessionPool.GetSessionByUID(uid)
		require.Equal(t, expectedSession, actualSession)
	})

	t.Run("Error", func(t *testing.T) {
		sessionPool := session.NewSessionPool()
		require.Nil(t, sessionPool.GetSessionByUID(uuid.New().String()))
	})
}

func TestStaticGetSessionByID(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		sessionPool := session.NewSessionPool()
		expectedSession, isNew := sessionPool.NewSession(nil, true)
		require.True(t, isNew)

		actualSession := sessionPool.GetSessionByID(expectedSession.ID())
		require.Equal(t, expectedSession, actualSession)
	})

	t.Run("Error", func(t *testing.T) {
		sessionPool := session.NewSessionPool()
		require.Nil(t, sessionPool.GetSessionByID(3))
	})
}

func TestStaticOnSessionBind(t *testing.T) {
	sessionPool := session.NewSessionPool()
	expectedSession, isNew := sessionPool.NewSession(nil, true)
	require.True(t, isNew)
	callbackData := map[string]string{"source": "test"}
	called := false

	sessionPool.OnSessionBind(func(_ context.Context, actualSession session.Session, actualCallbackData map[string]string) error {
		called = true
		require.Equal(t, expectedSession, actualSession)
		require.Equal(t, callbackData, actualCallbackData)
		return nil
	})

	require.NoError(t, expectedSession.Bind(context.Background(), uuid.New().String(), nil, callbackData))
	require.True(t, called)
}

func TestStaticOnAfterSessionBind(t *testing.T) {
	sessionPool := session.NewSessionPool()
	expectedSession, isNew := sessionPool.NewSession(nil, true)
	require.True(t, isNew)
	callbackData := map[string]string{"source": "test"}
	called := false

	sessionPool.OnAfterSessionBind(func(_ context.Context, actualSession session.Session, actualCallbackData map[string]string) error {
		called = true
		require.Equal(t, expectedSession, actualSession)
		require.Equal(t, callbackData, actualCallbackData)
		return nil
	})

	require.NoError(t, expectedSession.Bind(context.Background(), uuid.New().String(), nil, callbackData))
	require.True(t, called)
}

func TestStaticOnSessionClose(t *testing.T) {
	sessionPool := session.NewSessionPool()
	called := false
	sessionPool.OnSessionClose(func(session.Session, map[string]string, session.CloseReason) {
		called = true
	})

	callbacks := sessionPool.GetSessionCloseCallbacks()
	require.Len(t, callbacks, 1)
	callbacks[0](nil, nil, 0)
	require.True(t, called)
}

func TestStaticCloseAll(t *testing.T) {
	ctrl := gomock.NewController(t)
	entity := mocks.NewMockNetworkEntity(ctrl)
	entity.EXPECT().Close(nil)

	sessionPool := session.NewSessionPool()
	expectedSession, isNew := sessionPool.NewSession(entity, true)
	require.True(t, isNew)
	require.EqualValues(t, 1, sessionPool.GetSessionCount())

	sessionPool.CloseAll()
	require.Zero(t, sessionPool.GetSessionCount())
	require.Nil(t, sessionPool.GetSessionByID(expectedSession.ID()))
}
