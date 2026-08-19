package session

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/constants"
	entitymocks "github.com/alkaid/pitaya/v2/networkentity/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type sessionCoverageCache struct {
	CacheInterface
	fields map[string]string
	err    error
	key    string
	ttl    time.Duration
}

func (c *sessionCoverageCache) CacheTTL() time.Duration {
	return c.ttl
}

func (c *sessionCoverageCache) Hgetall(key string) (map[string]string, error) {
	c.key = key
	return c.fields, c.err
}

func newSessionWithCoverageCache(cache *sessionCoverageCache, uid string) *sessionImpl {
	pool := NewSessionPool().(*sessionPoolImpl)
	pool.SetClusterCache(cache)
	sess, _ := pool.NewSession(nil, false, uid)
	return sess.(*sessionImpl)
}

func TestSessionPoolCallbackRegistrationDeduplicatesAndRunsBindHooks(t *testing.T) {
	pool := NewSessionPool().(*sessionPoolImpl)
	var beforeCalls, afterCalls int
	beforeBind := func(context.Context, Session, map[string]string) error {
		beforeCalls++
		return nil
	}
	afterBind := func(context.Context, Session, map[string]string) error {
		afterCalls++
		return nil
	}
	afterBackendBind := func(context.Context, Session, string, string, map[string]string) error { return nil }
	afterBackendKick := func(context.Context, Session, string, string, map[string]string, CloseReason) error { return nil }
	onClose := func(Session, map[string]string, CloseReason) {}

	pool.OnSessionBind(beforeBind)
	pool.OnSessionBind(beforeBind)
	pool.OnAfterSessionBind(afterBind)
	pool.OnAfterSessionBind(afterBind)
	pool.OnAfterBindBackend(afterBackendBind)
	pool.OnAfterBindBackend(afterBackendBind)
	pool.OnAfterKickBackend(afterBackendKick)
	pool.OnAfterKickBackend(afterBackendKick)
	pool.OnSessionClose(onClose)
	pool.OnSessionClose(onClose)

	sess := newTestSession(pool, nil, true)
	require.NoError(t, sess.Bind(context.Background(), "user-callbacks", nil, nil))
	assert.Equal(t, 1, beforeCalls)
	assert.Equal(t, 1, afterCalls)
	assert.Len(t, pool.sessionBindCallbacks, 1)
	assert.Len(t, pool.afterBindCallbacks, 1)
	assert.Len(t, pool.afterBindBackendCallbacks, 1)
	assert.Len(t, pool.afterKickBackendCallbacks, 1)
	assert.Len(t, pool.GetSessionCloseCallbacks(), 1)
}

func TestInstallBackendCoordinatorOnDefaultPool(t *testing.T) {
	pool := NewSessionPool().(*sessionPoolImpl)
	coordinator := BackendCoordinator{
		Bind: func(context.Context, Session, string, string, map[string]string, BackendRollback) error {
			return nil
		},
		Kick: func(context.Context, Session, string, string, map[string]string, CloseReason, BackendRollback) error {
			return nil
		},
	}

	require.NoError(t, InstallBackendCoordinator(pool, coordinator))
	assert.NotNil(t, pool.backendCoordinator.Load())

	invalidPool := NewSessionPool().(*sessionPoolImpl)
	assert.PanicsWithValue(t, "session: backend coordinator requires bind and kick handlers", func() {
		invalidPool.SetBackendCoordinator(BackendCoordinator{})
	})
}

func TestSessionFrontendAndIPAccessors(t *testing.T) {
	ctrl := gomock.NewController(t)
	entity := entitymocks.NewMockNetworkEntity(ctrl)
	remoteIP := netip.MustParseAddr("198.51.100.4")
	entity.EXPECT().RemoteIP().Return(remoteIP)

	pool := NewSessionPool().(*sessionPoolImpl)
	assert.PanicsWithValue(t, "pitaya/session: cluster cache must not be nil", func() {
		pool.SetClusterCache(nil)
	})
	cache := &sessionCoverageCache{ttl: time.Minute}
	pool.SetClusterCache(cache)

	sess := newTestSession(pool, entity, true).(*sessionImpl)
	sess.SetFrontendData("frontend-1", 42)
	sess.SetIP("192.0.2.8")

	assert.Equal(t, "frontend-1", sess.GetFrontendID())
	assert.EqualValues(t, 42, sess.GetFrontendSessionID())
	assert.Equal(t, netip.MustParseAddr("192.0.2.8"), sess.RemoteIP())
	assert.Equal(t, "192.0.2.8", sess.RemoteIPText())
	assert.Equal(t, remoteIP, sess.RemoteIPWithoutCache())
	assert.Same(t, cache, sess.GetClusterStorage())
	assert.Empty(t, sess.ClusterStorageKey())

	require.NoError(t, sess.SetDataEncoded(nil))
	assert.Error(t, sess.SetDataEncoded([]byte("{")))
	sess.SetIP("not-an-ip")
	assert.False(t, sess.RemoteIP().IsValid())

	zeroValue := &sessionImpl{}
	assert.Empty(t, zeroValue.UID())
	assert.Zero(t, zeroValue.UIDInt())
	assert.False(t, zeroValue.Online())
	hash := FrontendSessHash("frontend-1", 42)
	assert.NotZero(t, hash)
	assert.NotEqual(t, hash, FrontendSessHash("frontend-1", 43))
}

func TestSessionInitialFromClusterRestoresBindingAndData(t *testing.T) {
	cache := &sessionCoverageCache{
		ttl: time.Minute,
		fields: map[string]string{
			fieldKeyBackends: `{"game":"game-1","chat":"chat-1"}`,
			fieldKeyData:     `{"coins":7}`,
			fieldKeyOnline:   "0",
		},
	}
	sess := newSessionWithCoverageCache(cache, "user-restore")

	require.NoError(t, sess.InitialFromCluster())
	assert.Equal(t, "pit:u:user-restore:", cache.key)
	assert.Equal(t, map[string]string{"chat": "chat-1", "game": "game-1"}, sess.GetBackends())
	assert.Equal(t, map[string]any{"coins": float64(7)}, sess.GetData())
	assert.JSONEq(t, `{"coins":7}`, string(sess.GetDataEncoded()))
}

func TestSessionInitialFromClusterFailurePaths(t *testing.T) {
	storageErr := errors.New("cache unavailable")
	tests := []struct {
		name    string
		cache   *sessionCoverageCache
		wantErr error
	}{
		{
			name:    "storage error",
			cache:   &sessionCoverageCache{ttl: time.Minute, err: storageErr},
			wantErr: storageErr,
		},
		{
			name:  "missing row is empty state",
			cache: &sessionCoverageCache{ttl: time.Minute, err: sql.ErrNoRows},
		},
		{
			name:    "invalid backends",
			cache:   &sessionCoverageCache{ttl: time.Minute, fields: map[string]string{fieldKeyBackends: "{"}},
			wantErr: errors.New("invalid JSON"),
		},
		{
			name:    "invalid data",
			cache:   &sessionCoverageCache{ttl: time.Minute, fields: map[string]string{fieldKeyData: "{"}},
			wantErr: errors.New("invalid JSON"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sess := newSessionWithCoverageCache(test.cache, "user-failure")
			err := sess.InitialFromCluster()
			if test.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			if errors.Is(test.wantErr, storageErr) {
				assert.ErrorIs(t, err, storageErr)
			} else {
				assert.Contains(t, err.Error(), "unexpected end of JSON input")
			}
		})
	}
}

func TestSessionObtainFromClusterRestoresCompleteSnapshot(t *testing.T) {
	cache := &sessionCoverageCache{
		ttl: time.Minute,
		fields: map[string]string{
			fieldKeyFrontendID:     "frontend-2",
			fieldKeyFrontendSessID: "84",
			fieldKeyBackends:       `{"game":"game-2"}`,
			fieldKeyData:           `{"level":9}`,
			fieldKeyIP:             "203.0.113.9",
			fieldKeyOnline:         "1",
		},
	}
	sess := newSessionWithCoverageCache(cache, "user-obtain")

	require.NoError(t, sess.ObtainFromCluster())
	assert.Equal(t, "frontend-2", sess.GetFrontendID())
	assert.EqualValues(t, 84, sess.GetFrontendSessionID())
	assert.Equal(t, map[string]string{"game": "game-2"}, sess.GetBackends())
	assert.Equal(t, map[string]any{"level": float64(9)}, sess.GetData())
	assert.Equal(t, "203.0.113.9", sess.RemoteIPText())
	assert.True(t, sess.Online())
}

func TestSessionObtainFromClusterClassifiesMissingAndStorageErrors(t *testing.T) {
	storageErr := errors.New("cache unavailable")
	tests := []struct {
		name  string
		cache *sessionCoverageCache
		want  error
	}{
		{name: "missing row", cache: &sessionCoverageCache{ttl: time.Minute, err: sql.ErrNoRows}, want: constants.ErrSessionNotFound},
		{name: "empty row", cache: &sessionCoverageCache{ttl: time.Minute, fields: map[string]string{}}, want: constants.ErrSessionNotFound},
		{name: "storage error", cache: &sessionCoverageCache{ttl: time.Minute, err: storageErr}, want: storageErr},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sess := newSessionWithCoverageCache(test.cache, "user-obtain-failure")
			assert.ErrorIs(t, sess.ObtainFromCluster(), test.want)
		})
	}
}
