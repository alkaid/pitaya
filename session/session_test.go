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

package session

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/networkentity"
	"github.com/alkaid/pitaya/v2/networkentity/mocks"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/proto"
)

var update = flag.Bool("update", false, "update .golden files")

type someStruct struct {
	A int
	B string
}

type unregisteredStruct struct{}

type mockAddr struct{}

func (ma *mockAddr) Network() string { return "tcp" }
func (ma *mockAddr) String() string  { return "192.0.2.1:25" }

func newTestSession(pool SessionPool, entity networkentity.NetworkEntity, frontend bool, uid ...string) Session {
	session, _ := pool.NewSession(entity, frontend, uid...)
	return session
}

func expectEntityCloseCounter(entity *mocks.MockNetworkEntity, count *atomic.Int64) {
	recordClose := func(map[string]string, ...int) error {
		count.Add(1)
		return nil
	}
	entity.EXPECT().Close(gomock.Any()).DoAndReturn(recordClose).AnyTimes()
	entity.EXPECT().Close(gomock.Any(), gomock.Any()).DoAndReturn(recordClose).AnyTimes()
}

type hgetallCache struct {
	CacheInterface
	fields map[string]string
	err    error
}

func (c *hgetallCache) Hgetall(string) (map[string]string, error) {
	return c.fields, c.err
}

func getEncodedEmptyMap() []byte {
	b, _ := json.Marshal(map[string]interface{}{})
	return b
}

func TestNewSessionIDService(t *testing.T) {
	t.Parallel()

	sessionIDService := newSessionIDService()
	assert.NotNil(t, sessionIDService)
	assert.EqualValues(t, 0, sessionIDService.sid)
}

func TestSessionIDServiceSessionID(t *testing.T) {
	t.Parallel()

	sessionIDService := newSessionIDService()
	sessionID := sessionIDService.sessionID()
	assert.EqualValues(t, 1, sessionID)
}

func TestCloseAll(t *testing.T) {
	var (
		entity      *mocks.MockNetworkEntity
		sessionPool = NewSessionPool().(*sessionPoolImpl)
	)

	tables := map[string]struct {
		sessions func() []Session
		mock     func()
	}{
		"test_close_many_sessions": {
			sessions: func() []Session {
				return []Session{
					newTestSession(sessionPool, entity, true, uuid.New().String()),
					newTestSession(sessionPool, entity, true, uuid.New().String()),
					newTestSession(sessionPool, entity, true, uuid.New().String()),
				}
			},
			mock: func() {
				entity.EXPECT().Close(nil).Times(3)
			},
		},

		"test_close_no_sessions": {
			sessions: func() []Session { return []Session{} },
			mock:     func() {},
		},
	}

	for name, table := range tables {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			entity = mocks.NewMockNetworkEntity(ctrl)
			for _, s := range table.sessions() {
				assert.NoError(t, sessionPool.StoreSessionLocal(s))
			}

			table.mock()

			sessionPool.CloseAll()
			assert.Zero(t, sessionPool.GetSessionCount())
			assert.Zero(t, sessionPool.GetUserCount())
		})
	}
}

func TestNew(t *testing.T) {
	tables := []struct {
		name     string
		frontend bool
		uid      string
	}{
		{"test_frontend", true, ""},
		{"test_backend", false, ""},
		{"test_frontend_with_uid", true, uuid.New().String()},
		{"test_backend_with_uid", false, uuid.New().String()},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			entity := mocks.NewMockNetworkEntity(ctrl)
			sessionPool := NewSessionPool().(*sessionPoolImpl)
			var ss *sessionImpl
			if table.uid != "" {
				ss = newTestSession(sessionPool, entity, table.frontend, table.uid).(*sessionImpl)
			} else {
				ss = newTestSession(sessionPool, entity, table.frontend).(*sessionImpl)
			}
			assert.NotZero(t, ss.id)
			assert.Equal(t, entity, ss.entity)
			assert.Empty(t, ss.data)
			assert.InDelta(t, time.Now().Unix(), ss.lastTime, 1)
			assert.Empty(t, ss.OnCloseCallbacks)
			assert.Equal(t, table.frontend, ss.IsFrontend)

			if len(table.uid) > 0 {
				assert.Equal(t, table.uid[0], ss.uid[0])
			}

			if table.frontend {
				val, ok := sessionPool.sessionsByID.Load(ss.id)
				assert.True(t, ok)
				assert.Equal(t, val, ss)
			}
		})
	}
}

func TestGetSessionByIDExists(t *testing.T) {
	t.Parallel()

	sessionPool := NewSessionPool()
	expectedSS := newTestSession(sessionPool, nil, true)
	ss := sessionPool.GetSessionByID(expectedSS.ID())
	assert.Equal(t, expectedSS, ss)
}

func TestGetSessionByIDDoenstExist(t *testing.T) {
	t.Parallel()
	sessionPool := NewSessionPool()
	ss := sessionPool.GetSessionByID(123456) // huge number to make sure no session with this id
	assert.Nil(t, ss)
}

func TestGetSessionByUIDExists(t *testing.T) {
	uid := uuid.New().String()

	sessionPool := NewSessionPool().(*sessionPoolImpl)
	expectedSS := newTestSession(sessionPool, nil, true, uid)
	assert.NoError(t, sessionPool.StoreSessionLocal(expectedSS))

	ss := sessionPool.GetSessionByUID(uid)
	assert.Equal(t, expectedSS, ss)
}

func TestGetSessionByUIDDoenstExist(t *testing.T) {
	t.Parallel()

	sessionPool := NewSessionPool()
	ss := sessionPool.GetSessionByUID(uuid.New().String())
	assert.Nil(t, ss)
}

func TestKick(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	entity := mocks.NewMockNetworkEntity(ctrl)
	sessionPool := NewSessionPool()
	ss := newTestSession(sessionPool, entity, true)
	c := context.Background()
	entity.EXPECT().Kick(c)
	entity.EXPECT().Close(nil)
	err := ss.Kick(c, nil)
	assert.NoError(t, err)
}

func TestKickClosesReentrantEntityOnce(t *testing.T) {
	ctrl := gomock.NewController(t)
	entity := mocks.NewMockNetworkEntity(ctrl)
	ctx := context.Background()
	entity.EXPECT().Kick(ctx)

	pool := NewSessionPool().(*sessionPoolImpl)
	var ss Session
	var closeCount atomic.Int64
	entity.EXPECT().Close(nil).DoAndReturn(func(map[string]string, ...int) error {
		closeCount.Add(1)
		ss.Close(nil)
		return nil
	}).AnyTimes()

	ss = newTestSession(pool, entity, true)
	uid := uuid.New().String()
	assert.NoError(t, ss.Bind(ctx, uid, nil, nil))

	assert.NoError(t, ss.Kick(ctx, nil))
	assert.EqualValues(t, 1, closeCount.Load())
	assert.Nil(t, pool.GetSessionByUID(uid))
	assert.Nil(t, pool.GetSessionByID(ss.ID()))
	assert.Zero(t, pool.GetSessionCount())
	assert.Zero(t, pool.GetUserCount())
}

func TestSessionUpdateEncodedData(t *testing.T) {
	tables := []struct {
		name string
		data map[string]interface{}
	}{
		{"testUpdateEncodedData_1", map[string]interface{}{"byte": []byte{1}}},
		{"testUpdateEncodedData_2", map[string]interface{}{"int": 1}},
		{"testUpdateEncodedData_3", map[string]interface{}{"struct": someStruct{A: 1, B: "aaa"}}},
		{"testUpdateEncodedData_4", map[string]interface{}{"string": "aaa"}},
		{"testUpdateEncodedData_5", map[string]interface{}{}},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool().(*sessionPoolImpl)
			ss := newTestSession(sessionPool, nil, false).(*sessionImpl)
			assert.NotNil(t, ss)

			ss.data = table.data
			err := ss.updateEncodedData()
			assert.NoError(t, err)

			gp := filepath.Join("fixtures", table.name+".golden")
			if *update {
				t.Log("updating golden file")
				helpers.WriteFile(t, gp, ss.encodedData)
			}
			data := helpers.ReadFile(t, gp)

			assert.Equal(t, data, ss.encodedData)
		})
	}
}

func TestSessionPush(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEntity := mocks.NewMockNetworkEntity(ctrl)
	sessionPool := NewSessionPool()
	ss := newTestSession(sessionPool, mockEntity, false)
	route := uuid.New().String()
	v := someStruct{A: 1, B: "aaa"}

	mockEntity.EXPECT().Push(route, v)
	err := ss.Push(route, v)
	assert.NoError(t, err)
}

func TestSessionResponseMID(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEntity := mocks.NewMockNetworkEntity(ctrl)
	sessionPool := NewSessionPool()
	ss := newTestSession(sessionPool, mockEntity, false)
	mid := uint(rand.Int())
	v := someStruct{A: 1, B: "aaa"}
	ctx := context.Background()

	mockEntity.EXPECT().ResponseMID(ctx, mid, v)
	err := ss.ResponseMID(ctx, mid, v)
	assert.NoError(t, err)
}

func TestSessionID(t *testing.T) {
	t.Parallel()

	sessionPool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(sessionPool, nil, false).(*sessionImpl)
	ss.id = int64(rand.Uint64())

	id := ss.ID()
	assert.Equal(t, ss.id, id)
}

func TestSessionUID(t *testing.T) {
	t.Parallel()

	sessionPool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(sessionPool, nil, false).(*sessionImpl)
	ss.uid = uuid.New().String()

	uid := ss.UID()
	assert.Equal(t, ss.uid, uid)
}

func TestSessionGetData(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name string
		data map[string]interface{}
	}{
		{"test_1", map[string]interface{}{"byte": []byte{1}, "string": "test", "int": 1}},
		{"test_2", map[string]interface{}{"byte": []byte{1}, "struct": someStruct{A: 1, B: "aaa"}, "int": 34}},
		{"test_3", map[string]interface{}{"string": "aaa"}},
		{"test_4", map[string]interface{}{}},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool().(*sessionPoolImpl)
			ss := newTestSession(sessionPool, nil, false).(*sessionImpl)
			ss.data = table.data

			data := ss.GetData()
			assert.Equal(t, ss.data, data)
		})
	}
}

func TestSessionSetData(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name string
		data map[string]interface{}
	}{
		{"testSessionSetData_1", map[string]interface{}{"byte": []byte{1}}},
		{"testSessionSetData_2", map[string]interface{}{"int": 1}},
		{"testSessionSetData_3", map[string]interface{}{"struct": someStruct{A: 1, B: "aaa"}}},
		{"testSessionSetData_4", map[string]interface{}{"string": "aaa"}},
		{"testSessionSetData_5", map[string]interface{}{}},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool().(*sessionPoolImpl)
			ss := newTestSession(sessionPool, nil, false).(*sessionImpl)
			err := ss.SetData(table.data)
			assert.NoError(t, err)
			assert.Equal(t, table.data, ss.data)

			gp := filepath.Join("fixtures", table.name+".golden")
			if *update {
				t.Log("updating golden file")
				helpers.WriteFile(t, gp, ss.encodedData)
			}
			encodedData := helpers.ReadFile(t, gp)
			assert.Equal(t, encodedData, ss.encodedData)
		})
	}
}

func TestSessionGetEncodedData(t *testing.T) {
	t.Parallel()
	tables := []struct {
		name string
	}{
		{"testUpdateEncodedData_1"},
		{"testUpdateEncodedData_2"},
		{"testUpdateEncodedData_3"},
		{"testUpdateEncodedData_4"},
		{"testUpdateEncodedData_5"},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool().(*sessionPoolImpl)
			ss := newTestSession(sessionPool, nil, false).(*sessionImpl)
			assert.NotNil(t, ss)

			gp := filepath.Join("fixtures", table.name+".golden")
			encodedData := helpers.ReadFile(t, gp)

			ss.encodedData = encodedData
			data := ss.GetDataEncoded()
			assert.Equal(t, ss.encodedData, data)
		})
	}
}

func TestSessionSetEncodedData(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name string
		data map[string]interface{}
	}{
		{"testUpdateEncodedData_2", map[string]interface{}{"int": float64(1)}},
		{"testUpdateEncodedData_3", map[string]interface{}{"struct": map[string]interface{}{"A": float64(1), "B": "aaa"}}},
		{"testUpdateEncodedData_4", map[string]interface{}{"string": "aaa"}},
		{"testUpdateEncodedData_5", map[string]interface{}{}},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool().(*sessionPoolImpl)
			ss := newTestSession(sessionPool, nil, false).(*sessionImpl)
			assert.NotNil(t, ss)

			gp := filepath.Join("fixtures", table.name+".golden")
			encodedData := helpers.ReadFile(t, gp)

			err := ss.SetDataEncoded(encodedData)
			assert.NoError(t, err)
			assert.Equal(t, encodedData, ss.encodedData)
			assert.Equal(t, table.data, ss.data)
		})
	}
}

func TestSessionSetFrontendData(t *testing.T) {
	t.Parallel()

	frontendID := uuid.New().String()
	frontendSessionID := int64(rand.Uint64())

	sessionPool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(sessionPool, nil, false).(*sessionImpl)
	assert.NotNil(t, ss)
	ss.SetFrontendData(frontendID, frontendSessionID)

	assert.Equal(t, frontendID, ss.frontendID)
	assert.Equal(t, frontendSessionID, ss.frontendSessionID)
}

func TestSessionBindFailsWithoutUID(t *testing.T) {
	t.Parallel()

	sessionPool := NewSessionPool()
	ss := newTestSession(sessionPool, nil, false)
	assert.NotNil(t, ss)

	err := ss.Bind(nil, "", nil, nil)
	assert.Equal(t, constants.ErrIllegalUID, err)
}

func TestSessionBindFailsIfAlreadyBound(t *testing.T) {
	t.Parallel()

	sessionPool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(sessionPool, nil, false).(*sessionImpl)
	ss.uid = uuid.New().String()
	assert.NotNil(t, ss)

	err := ss.Bind(nil, uuid.New().String(), nil, nil)
	assert.ErrorIs(t, err, constants.ErrSessionAlreadyBound)
}

func TestSessionBindRunsOnSessionBind(t *testing.T) {
	affectedVar := ""
	err := errors.New("some error occured")
	tables := []struct {
		name          string
		onSessionBind OnSessionBindFunc
		err           error
	}{
		{"successful_on_session_bind", func(ctx context.Context, s Session, _ map[string]string) error {
			affectedVar = s.UID()
			return nil
		}, nil},
		{"failed_on_session_bind", func(ctx context.Context, s Session, _ map[string]string) error { return err }, err},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			affectedVar = ""
			sessionPool := NewSessionPool().(*sessionPoolImpl)
			ss := newTestSession(sessionPool, nil, true)
			assert.NotNil(t, ss)

			sessionPool.OnSessionBind(table.onSessionBind)
			defer func() { sessionPool.sessionBindCallbacks = make([]OnSessionBindFunc, 0) }()

			uid := uuid.New().String()
			err := ss.Bind(nil, uid, nil, nil)

			if table.err != nil {
				assert.Equal(t, table.err, err)
				assert.Empty(t, affectedVar)
				assert.Empty(t, ss.UID())
			} else {
				assert.NoError(t, err)
				assert.Equal(t, uid, affectedVar)
				assert.Equal(t, uid, ss.UID())
			}
		})
	}
}

func TestSessionBindFrontend(t *testing.T) {
	sessionPool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(sessionPool, nil, true)
	assert.NotNil(t, ss)

	uid := uuid.New().String()
	err := ss.Bind(nil, uid, nil, nil)
	assert.NoError(t, err)
	assert.Equal(t, uid, ss.UID())

	val, ok := sessionPool.sessionsByUID.Load(uid)
	assert.True(t, ok)
	assert.Equal(t, val, ss)
}

func TestSessionBindFailurePreservesExistingBinding(t *testing.T) {
	pool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(pool, nil, true)
	uid := uuid.New().String()
	assert.NoError(t, ss.Bind(context.Background(), uid, nil, nil))

	bindErr := errors.New("bind callback failed")
	pool.OnSessionBind(func(context.Context, Session, map[string]string) error {
		return bindErr
	})
	err := ss.Bind(context.Background(), uid, nil, nil)

	assert.ErrorIs(t, err, bindErr)
	assert.Equal(t, uid, ss.UID())
	assert.Equal(t, ss, pool.GetSessionByUID(uid))
	assert.EqualValues(t, 1, pool.GetSessionCount())
	assert.EqualValues(t, 1, pool.GetUserCount())
}

func TestSessionBindRejectsReentrantAttempt(t *testing.T) {
	pool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(pool, nil, true)
	uid := uuid.New().String()
	var reentrantErr error
	pool.OnSessionBind(func(ctx context.Context, session Session, _ map[string]string) error {
		reentrantErr = session.Bind(ctx, uid, nil, nil)
		return nil
	})

	err := ss.Bind(context.Background(), uid, nil, nil)

	assert.NoError(t, err)
	assert.ErrorIs(t, reentrantErr, constants.ErrSessionAlreadyBound)
	assert.Equal(t, uid, ss.UID())
	assert.Equal(t, ss, pool.GetSessionByUID(uid))
	assert.EqualValues(t, 1, pool.GetSessionCount())
	assert.EqualValues(t, 1, pool.GetUserCount())
}

func TestSessionBindCallbackCannotStorePartialBinding(t *testing.T) {
	pool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(pool, nil, true)
	uid := uuid.New().String()
	bindErr := errors.New("bind callback failed")
	var storeErr error
	pool.OnSessionBind(func(_ context.Context, session Session, _ map[string]string) error {
		storeErr = pool.StoreSessionLocal(session)
		return bindErr
	})

	err := ss.Bind(context.Background(), uid, nil, nil)

	assert.ErrorIs(t, err, bindErr)
	assert.ErrorIs(t, storeErr, constants.ErrSessionAlreadyBound)
	assert.Empty(t, ss.UID())
	assert.Nil(t, pool.GetSessionByUID(uid))
	assert.Equal(t, ss, pool.GetSessionByID(ss.ID()))
	assert.EqualValues(t, 1, pool.GetSessionCount())
	assert.Zero(t, pool.GetUserCount())
}

func TestSessionBindCallbackCannotRemoveRegistration(t *testing.T) {
	pool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(pool, nil, true)
	uid := uuid.New().String()
	pool.OnSessionBind(func(_ context.Context, session Session, _ map[string]string) error {
		pool.RemoveSessionLocal(session)
		return nil
	})

	err := ss.Bind(context.Background(), uid, nil, nil)

	assert.NoError(t, err)
	assert.Equal(t, uid, ss.UID())
	assert.Equal(t, ss, pool.GetSessionByUID(uid))
	assert.Equal(t, ss, pool.GetSessionByID(ss.ID()))
	assert.EqualValues(t, 1, pool.GetSessionCount())
	assert.EqualValues(t, 1, pool.GetUserCount())
}

func TestSessionBindBackend(t *testing.T) {
	tables := []struct {
		name       string
		bindErr    error
		clusterErr error
		wantUID    bool
	}{
		{"successful_bind_in_front", nil, nil, true},
		{"failed_bind_in_front", errors.New("failed bind in front"), nil, false},
		{"failed_cluster_load_after_bind", nil, errors.New("failed cluster load"), true},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockEntity := mocks.NewMockNetworkEntity(ctrl)
			sessionPool := NewSessionPool().(*sessionPoolImpl)
			sessionPool.storage = &hgetallCache{fields: map[string]string{fieldKeyOnline: "1"}, err: table.clusterErr}
			ss := newTestSession(sessionPool, mockEntity, false).(*sessionImpl)
			assert.NotNil(t, ss)

			uid := uuid.New().String()
			frontendID := uuid.New().String()
			frontendSessionID := int64(42)
			ss.SetFrontendData(frontendID, frontendSessionID)
			expectedBindMsg := &protos.BindMsg{
				Uid: uid,
				Fid: frontendID,
				Sid: frontendSessionID,
				Session: &protos.Session{
					Data: ss.encodedData,
				},
			}
			ctx := context.Background()
			expectedRequestData, err := proto.Marshal(expectedBindMsg)
			assert.NoError(t, err)

			mockEntity.EXPECT().SendRequest(ctx, frontendID, constants.SessionBindRoute, expectedRequestData).Return(&protos.Response{}, table.bindErr)

			err = ss.Bind(ctx, uid, nil, nil)
			expectedErr := table.bindErr
			if expectedErr == nil {
				expectedErr = table.clusterErr
			}
			if expectedErr == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, expectedErr)
			}

			if table.wantUID {
				assert.Equal(t, uid, ss.uid)
			} else {
				assert.Empty(t, ss.uid)
			}

			_, ok := sessionPool.sessionsByUID.Load(uid)
			assert.False(t, ok)
		})
	}
}

func TestSessionOnCloseFailsIfBackend(t *testing.T) {
	t.Parallel()

	sessionPool := NewSessionPool()
	ss := newTestSession(sessionPool, nil, false)
	assert.NotNil(t, ss)

	err := ss.OnClose(nil)
	assert.Equal(t, constants.ErrOnCloseBackend, err)
}

func TestSessionOnClose(t *testing.T) {
	t.Parallel()

	sessionPool := NewSessionPool()
	ss := newTestSession(sessionPool, nil, true)
	assert.NotNil(t, ss)

	expected := false
	f := func() { expected = true }
	err := ss.OnClose(f)
	assert.NoError(t, err)
	assert.Len(t, ss.GetOnCloseCallbacks(), 1)

	ss.GetOnCloseCallbacks()[0]()
	assert.True(t, expected)
}

func TestSessionClose(t *testing.T) {
	tables := []struct {
		name string
		uid  string
	}{
		{"close", ""},
		{"close_bound", uuid.New().String()},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockEntity := mocks.NewMockNetworkEntity(ctrl)
			sessionPool := NewSessionPool().(*sessionPoolImpl)
			ss := newTestSession(sessionPool, mockEntity, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.uid != "" {
				ss.uid = table.uid
				assert.NoError(t, sessionPool.StoreSessionLocal(ss))
			}

			mockEntity.EXPECT().Close(nil)
			ss.Close(nil)

			_, ok := sessionPool.sessionsByID.Load(ss.id)
			assert.False(t, ok)

			if table.uid != "" {
				_, ok = sessionPool.sessionsByUID.Load(table.uid)
				assert.False(t, ok)
			}
			assert.Zero(t, sessionPool.GetSessionCount())
			assert.Zero(t, sessionPool.GetUserCount())
		})
	}
}

func TestSessionCloseAfterRebindPreservesReplacement(t *testing.T) {
	ctrl := gomock.NewController(t)
	oldEntity := mocks.NewMockNetworkEntity(ctrl)
	newEntity := mocks.NewMockNetworkEntity(ctrl)
	var oldCloseCount atomic.Int64
	expectEntityCloseCounter(oldEntity, &oldCloseCount)

	pool := NewSessionPool().(*sessionPoolImpl)
	uid := uuid.New().String()
	oldSession := newTestSession(pool, oldEntity, true)
	assert.NoError(t, oldSession.Bind(context.Background(), uid, nil, nil))

	newSession := newTestSession(pool, newEntity, true)
	assert.NoError(t, newSession.Bind(context.Background(), uid, nil, nil))
	assert.Equal(t, newSession, pool.GetSessionByUID(uid))
	assert.EqualValues(t, 1, pool.GetSessionCount())
	assert.EqualValues(t, 1, pool.GetUserCount())

	// The connection handler can still close the replaced session after Bind closed it.
	oldSession.Close(nil)

	assert.Equal(t, newSession, pool.GetSessionByUID(uid))
	assert.EqualValues(t, 1, pool.GetSessionCount())
	assert.EqualValues(t, 1, pool.GetUserCount())
	assert.EqualValues(t, 1, oldCloseCount.Load())
}

func TestSessionCloseAfterCallbackClearRemovesRegisteredUID(t *testing.T) {
	ctrl := gomock.NewController(t)
	entity := mocks.NewMockNetworkEntity(ctrl)
	entity.EXPECT().Close(nil)

	pool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(pool, entity, true)
	uid := uuid.New().String()
	assert.NoError(t, ss.Bind(context.Background(), uid, nil, nil))
	assert.NoError(t, ss.OnClose(ss.Clear))

	// Agent.Close runs these callbacks before HandlerService defers Session.Close.
	for _, callback := range ss.GetOnCloseCallbacks() {
		callback()
	}
	assert.Empty(t, ss.UID())
	ss.Close(nil)

	assert.Nil(t, pool.GetSessionByUID(uid))
	assert.Nil(t, pool.GetSessionByID(ss.ID()))
	assert.Zero(t, pool.GetSessionCount())
	assert.Zero(t, pool.GetUserCount())
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	t.Run("sequential", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		entity := mocks.NewMockNetworkEntity(ctrl)
		var closeCount atomic.Int64
		expectEntityCloseCounter(entity, &closeCount)

		pool := NewSessionPool().(*sessionPoolImpl)
		ss := newTestSession(pool, entity, true)
		assert.NoError(t, ss.Bind(context.Background(), uuid.New().String(), nil, nil))

		ss.Close(nil)
		ss.Close(nil)

		assert.Zero(t, pool.GetSessionCount())
		assert.Zero(t, pool.GetUserCount())
		assert.EqualValues(t, 1, closeCount.Load())
	})

	t.Run("concurrent", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		entity := mocks.NewMockNetworkEntity(ctrl)
		var closeCount atomic.Int64
		expectEntityCloseCounter(entity, &closeCount)

		pool := NewSessionPool().(*sessionPoolImpl)
		ss := newTestSession(pool, entity, true)
		assert.NoError(t, ss.Bind(context.Background(), uuid.New().String(), nil, nil))

		const closers = 32
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(closers)
		for range closers {
			go func() {
				defer wg.Done()
				<-start
				ss.Close(nil)
			}()
		}
		close(start)
		wg.Wait()

		assert.Zero(t, pool.GetSessionCount())
		assert.Zero(t, pool.GetUserCount())
		assert.EqualValues(t, 1, closeCount.Load())
	})
}

func TestSessionCloseWithoutUIDDoesNotChangeUserCount(t *testing.T) {
	ctrl := gomock.NewController(t)
	entity := mocks.NewMockNetworkEntity(ctrl)
	var closeCount atomic.Int64
	expectEntityCloseCounter(entity, &closeCount)

	pool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(pool, entity, true)
	ss.Close(nil)
	ss.Close(nil)

	assert.Zero(t, pool.GetSessionCount())
	assert.Zero(t, pool.GetUserCount())
	assert.EqualValues(t, 1, closeCount.Load())
}

func TestSessionBindConcurrentCloseDoesNotRestoreClosedSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	entity := mocks.NewMockNetworkEntity(ctrl)
	var closeCount atomic.Int64
	expectEntityCloseCounter(entity, &closeCount)

	pool := NewSessionPool().(*sessionPoolImpl)
	bindEntered := make(chan struct{})
	resumeBind := make(chan struct{})
	pool.OnSessionBind(func(context.Context, Session, map[string]string) error {
		close(bindEntered)
		<-resumeBind
		return nil
	})

	ss := newTestSession(pool, entity, true)
	uid := uuid.New().String()
	bindErr := make(chan error, 1)
	go func() {
		bindErr <- ss.Bind(context.Background(), uid, nil, nil)
	}()

	<-bindEntered
	ss.Close(nil)
	close(resumeBind)
	err := <-bindErr

	assert.ErrorIs(t, err, constants.ErrCloseClosedSession)
	assert.Nil(t, pool.GetSessionByUID(uid))
	assert.Nil(t, pool.GetSessionByID(ss.ID()))
	assert.Zero(t, pool.GetSessionCount())
	assert.Zero(t, pool.GetUserCount())
	assert.EqualValues(t, 1, closeCount.Load())
}

func TestCloseAllConcurrentRebindDoesNotRestoreClosedSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	oldEntity := mocks.NewMockNetworkEntity(ctrl)
	newEntity := mocks.NewMockNetworkEntity(ctrl)
	var oldCloseCount atomic.Int64
	var newCloseCount atomic.Int64
	expectEntityCloseCounter(oldEntity, &oldCloseCount)
	expectEntityCloseCounter(newEntity, &newCloseCount)

	pool := NewSessionPool().(*sessionPoolImpl)
	uid := uuid.New().String()
	oldSession := newTestSession(pool, oldEntity, true)
	assert.NoError(t, oldSession.Bind(context.Background(), uid, nil, nil))

	bindEntered := make(chan struct{})
	resumeBind := make(chan struct{})
	pool.OnSessionBind(func(context.Context, Session, map[string]string) error {
		close(bindEntered)
		<-resumeBind
		return nil
	})
	newSession := newTestSession(pool, newEntity, true)
	bindErr := make(chan error, 1)
	go func() {
		bindErr <- newSession.Bind(context.Background(), uid, nil, nil)
	}()

	<-bindEntered
	closeAllDone := make(chan struct{})
	go func() {
		pool.CloseAll()
		close(closeAllDone)
	}()
	<-closeAllDone
	close(resumeBind)
	err := <-bindErr

	assert.ErrorIs(t, err, constants.ErrCloseClosedSession)
	assert.Nil(t, pool.GetSessionByUID(uid))
	assert.Nil(t, pool.GetSessionByID(oldSession.ID()))
	assert.Nil(t, pool.GetSessionByID(newSession.ID()))
	assert.Zero(t, pool.GetSessionCount())
	assert.Zero(t, pool.GetUserCount())
	assert.EqualValues(t, 1, oldCloseCount.Load())
	assert.EqualValues(t, 1, newCloseCount.Load())
}

func TestRemoveSessionLocalPreservesReplacement(t *testing.T) {
	pool := NewSessionPool().(*sessionPoolImpl)
	uid := uuid.New().String()
	oldSession := newTestSession(pool, nil, false).(*sessionImpl)
	oldSession.uid = uid
	replacement := newTestSession(pool, nil, false).(*sessionImpl)
	replacement.uid = uid

	assert.NoError(t, pool.StoreSessionLocal(oldSession))
	assert.NoError(t, pool.StoreSessionLocal(oldSession))
	assert.NoError(t, pool.StoreSessionLocal(replacement))
	assert.Equal(t, replacement, pool.GetSessionByUID(uid))
	assert.EqualValues(t, 2, pool.GetSessionCount())
	assert.EqualValues(t, 1, pool.GetUserCount())

	pool.RemoveSessionLocal(oldSession)
	pool.RemoveSessionLocal(oldSession)

	assert.Equal(t, replacement, pool.GetSessionByUID(uid))
	assert.Nil(t, pool.GetSessionByID(oldSession.ID()))
	assert.Equal(t, replacement, pool.GetSessionByID(replacement.ID()))
	assert.EqualValues(t, 1, pool.GetSessionCount())
	assert.EqualValues(t, 1, pool.GetUserCount())
}

func TestStoreSessionLocalRejectsClosedSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	entity := mocks.NewMockNetworkEntity(ctrl)
	entity.EXPECT().Close(nil)

	pool := NewSessionPool().(*sessionPoolImpl)
	ss := newTestSession(pool, entity, true)
	uid := uuid.New().String()
	assert.NoError(t, ss.Bind(context.Background(), uid, nil, nil))
	ss.Close(nil)

	err := pool.StoreSessionLocal(ss)

	assert.ErrorIs(t, err, constants.ErrCloseClosedSession)
	assert.Nil(t, pool.GetSessionByUID(uid))
	assert.Nil(t, pool.GetSessionByID(ss.ID()))
	assert.Zero(t, pool.GetSessionCount())
	assert.Zero(t, pool.GetUserCount())
}

func TestStoreAndRemoveSessionLocalAreAtomic(t *testing.T) {
	t.Run("remove before store", func(t *testing.T) {
		pool := NewSessionPool().(*sessionPoolImpl)
		ss := newTestSession(pool, nil, false).(*sessionImpl)
		uid := uuid.New().String()
		ss.uid = uid

		pool.RemoveSessionLocal(ss)
		assert.Zero(t, pool.GetSessionCount())
		assert.Zero(t, pool.GetUserCount())

		assert.NoError(t, pool.StoreSessionLocal(ss))
		assert.Equal(t, ss, pool.GetSessionByUID(uid))
		assert.Equal(t, ss, pool.GetSessionByID(ss.ID()))
		assert.EqualValues(t, 1, pool.GetSessionCount())
		assert.EqualValues(t, 1, pool.GetUserCount())
	})

	t.Run("concurrent", func(t *testing.T) {
		const iterations = 100
		for i := range iterations {
			pool := NewSessionPool().(*sessionPoolImpl)
			ss := newTestSession(pool, nil, false).(*sessionImpl)
			uid := uuid.New().String()
			ss.uid = uid

			start := make(chan struct{})
			storeErr := make(chan error, 1)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				storeErr <- pool.StoreSessionLocal(ss)
			}()
			go func() {
				defer wg.Done()
				<-start
				pool.RemoveSessionLocal(ss)
			}()
			close(start)
			wg.Wait()

			assert.NoError(t, <-storeErr, "iteration %d", i)
			byUID := pool.GetSessionByUID(uid)
			byID := pool.GetSessionByID(ss.ID())
			assert.Equal(t, byUID == nil, byID == nil, "iteration %d", i)
			if byUID == nil {
				assert.Zero(t, pool.GetSessionCount(), "iteration %d", i)
				assert.Zero(t, pool.GetUserCount(), "iteration %d", i)
			} else {
				assert.Equal(t, ss, byUID, "iteration %d", i)
				assert.Equal(t, ss, byID, "iteration %d", i)
				assert.EqualValues(t, 1, pool.GetSessionCount(), "iteration %d", i)
				assert.EqualValues(t, 1, pool.GetUserCount(), "iteration %d", i)
			}
		}
	})
}

func TestSessionCloseFrontendWithSubscription(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	initialSubs := s.NumSubscriptions()

	conn, err := nats.Connect(fmt.Sprintf("nats://%s", s.Addr()))
	assert.NoError(t, err)
	defer conn.Close()

	subs, err := conn.Subscribe(uuid.New().String(), func(msg *nats.Msg) {})
	assert.NoError(t, err)

	helpers.ShouldEventuallyReturn(t, s.NumSubscriptions, initialSubs+1)
	helpers.ShouldEventuallyReturn(t, conn.NumSubscriptions, 1)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEntity := mocks.NewMockNetworkEntity(ctrl)
	sessionPool := NewSessionPool()
	ss := newTestSession(sessionPool, mockEntity, true)
	assert.NotNil(t, ss)
	ss.SetSubscriptions([]*nats.Subscription{subs})

	mockEntity.EXPECT().Close(nil)
	ss.Close(nil)
	ss.Close(nil)

	helpers.ShouldEventuallyReturn(t, s.NumSubscriptions, initialSubs)
	helpers.ShouldEventuallyReturn(t, conn.NumSubscriptions, 0)
}

func TestSessionRemoteAddr(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEntity := mocks.NewMockNetworkEntity(ctrl)
	sessionPool := NewSessionPool()
	ss := newTestSession(sessionPool, mockEntity, true)
	assert.NotNil(t, ss)

	expectedAddr := &mockAddr{}
	mockEntity.EXPECT().RemoteAddr().Return(expectedAddr)
	addr := ss.RemoteAddr()
	assert.Equal(t, expectedAddr, addr)
}

func TestSessionSet(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name   string
		val    interface{}
		errStr string
	}{
		{"ok", "val", ""},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)
			err := ss.Set("key", table.val)
			if table.errStr == "" {
				assert.NoError(t, err)
				val, ok := ss.data["key"]
				assert.True(t, ok)
				assert.Equal(t, table.val, val)
				assert.NotEmpty(t, ss.encodedData)
			}
		})
	}
}

func TestSessionRemove(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name string
		val  interface{}
	}{
		{"existent", "val"},
		{"unexistent", nil},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
				assert.NotEmpty(t, ss.encodedData)
			}

			err := ss.Remove("key")
			assert.NoError(t, err)
			assert.Empty(t, ss.data)

			expectedEncoded := getEncodedEmptyMap()
			assert.Equal(t, expectedEncoded, ss.encodedData)
		})
	}
}

func TestOnSessionBind(t *testing.T) {
	expected := false
	f := func(context.Context, Session, map[string]string) error {
		expected = true
		return nil
	}
	sessionPool := NewSessionPool().(*sessionPoolImpl)
	sessionPool.OnSessionBind(f)
	defer func() { sessionPool.sessionBindCallbacks = make([]OnSessionBindFunc, 0) }()
	assert.NotNil(t, sessionPool.OnSessionBind)

	sessionPool.sessionBindCallbacks[0](context.Background(), nil, nil)
	assert.True(t, expected)
}

func TestSessionHasKey(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name string
		val  interface{}
	}{
		{"existent", "val"},
		{"unexistent", nil},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			exists := ss.HasKey("key")
			assert.Equal(t, table.val != nil, exists)
		})
	}
}

func TestSessionGet(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name string
		val  interface{}
	}{
		{"existent", "val"},
		{"unexistent", nil},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Get("key")
			assert.Equal(t, table.val, val)
		})
	}
}

func TestSessionInt(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", 1, true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Int("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, 0, val)
			}
		})
	}
}

func TestSessionInt8(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", int8(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Int8("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, int8(0), val)
			}
		})
	}
}

func TestSessionInt16(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", int16(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Int16("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, int16(0), val)
			}
		})
	}
}

func TestSessionInt32(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", int32(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Int32("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, int32(0), val)
			}
		})
	}
}

func TestSessionInt64(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", int64(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Int64("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, int64(0), val)
			}
		})
	}
}

func TestSessionUint(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", uint(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Uint("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, uint(0), val)
			}
		})
	}
}

func TestSessionUint8(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", uint8(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Uint8("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, uint8(0), val)
			}
		})
	}
}

func TestSessionUint16(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", uint16(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Uint16("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, uint16(0), val)
			}
		})
	}
}

func TestSessionUint32(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", uint32(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Uint32("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, uint32(0), val)
			}
		})
	}
}

func TestSessionUint64(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", uint64(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Uint64("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, uint64(0), val)
			}
		})
	}
}

func TestSessionFloat32(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", float32(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Float32("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, float32(0), val)
			}
		})
	}
}

func TestSessionFloat64(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", float64(1), true},
		{"existent_wrong_type", "val", false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Float64("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, float64(0), val)
			}
		})
	}
}

func TestSessionString(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name         string
		val          interface{}
		shouldReturn bool
	}{
		{"existent", "val", true},
		{"existent_wrong_type", 1, false},
		{"unexistent", nil, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.String("key")
			if table.shouldReturn {
				assert.Equal(t, table.val, val)
			} else {
				assert.Equal(t, "", val)
			}
		})
	}
}

func TestSessionValue(t *testing.T) {
	t.Parallel()

	tables := []struct {
		name string
		val  interface{}
	}{
		{"existent", "val"},
		{"unexistent", nil},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
			assert.NotNil(t, ss)

			if table.val != nil {
				err := ss.Set("key", table.val)
				assert.NoError(t, err)
				assert.NotEmpty(t, ss.data)
			}

			val := ss.Value("key")
			assert.Equal(t, table.val, val)
		})
	}
}

func TestSessionPushToFrontFailsIfFrontend(t *testing.T) {
	t.Parallel()

	sessionPool := NewSessionPool()
	ss := newTestSession(sessionPool, nil, true)
	assert.NotNil(t, ss)

	err := ss.PushToFront(nil)
	assert.Equal(t, constants.ErrFrontSessionCantPushToFront, err)
}

func TestSessionPushToFront(t *testing.T) {
	t.Parallel()
	tables := []struct {
		name string
		err  error
	}{
		{"successful_request", nil},
		{"failed_request", errors.New("failed bind in front")},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockEntity := mocks.NewMockNetworkEntity(ctrl)
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, mockEntity, false).(*sessionImpl)
			assert.NotNil(t, ss)
			ss.Set("key", "val")
			uid := uuid.New().String()
			ss.uid = uid
			frontendID := uuid.New().String()
			ss.SetFrontendData(frontendID, 42)

			expectedSessionData := &protos.Session{
				Id:   ss.frontendSessionID,
				Uid:  uid,
				Data: ss.encodedData,
			}
			expectedRequestData, err := proto.Marshal(expectedSessionData)
			assert.NoError(t, err)
			ctx := context.Background()
			mockEntity.EXPECT().SendRequest(ctx, frontendID, constants.SessionPushRoute, expectedRequestData).Return(nil, table.err)

			err = ss.PushToFront(ctx)
			assert.Equal(t, table.err, err)
		})
	}
}

func TestSessionClear(t *testing.T) {
	t.Parallel()

	sessionPool := NewSessionPool()
	ss := newTestSession(sessionPool, nil, true).(*sessionImpl)
	assert.NotNil(t, ss)

	ss.uid = uuid.New().String()
	err := ss.Set("key", "val")
	assert.NoError(t, err)
	assert.NotEmpty(t, ss.data)
	assert.NotEmpty(t, ss.encodedData)

	ss.Clear()
	assert.Empty(t, ss.data)

	expectedEncoded := getEncodedEmptyMap()
	assert.Equal(t, expectedEncoded, ss.encodedData)
}

func TestSessionGetHandshakeData(t *testing.T) {
	t.Parallel()

	data1 := &HandshakeData{
		Sys: HandshakeClientData{
			Platform:    "macos",
			LibVersion:  "2.3.2",
			BuildNumber: "20",
			Version:     "14.0.2",
		},
		User: make(map[string]interface{}),
	}
	data2 := &HandshakeData{
		Sys: HandshakeClientData{
			Platform:    "windows",
			LibVersion:  "2.3.10",
			BuildNumber: "",
			Version:     "ahaha",
		},
		User: map[string]interface{}{
			"ababa": make(map[string]interface{}),
			"pepe":  1,
		},
	}
	tables := []struct {
		name string
		data *HandshakeData
	}{
		{"test_1", data1},
		{"test_2", data2},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, false).(*sessionImpl)

			assert.Nil(t, ss.GetHandshakeData())

			ss.handshakeData = table.data

			assert.Equal(t, ss.GetHandshakeData(), table.data)
		})
	}
}

func TestSessionSetHandshakeData(t *testing.T) {
	t.Parallel()

	data1 := &HandshakeData{
		Sys: HandshakeClientData{
			Platform:    "macos",
			LibVersion:  "2.3.2",
			BuildNumber: "20",
			Version:     "14.0.2",
		},
		User: make(map[string]interface{}),
	}
	data2 := &HandshakeData{
		Sys: HandshakeClientData{
			Platform:    "windows",
			LibVersion:  "2.3.10",
			BuildNumber: "",
			Version:     "ahaha",
		},
		User: map[string]interface{}{
			"ababa": make(map[string]interface{}),
			"pepe":  1,
		},
	}
	tables := []struct {
		name string
		data *HandshakeData
	}{
		{"testSessionSetData_1", data1},
		{"testSessionSetData_2", data2},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			ss := newTestSession(sessionPool, nil, false).(*sessionImpl)
			ss.SetHandshakeData(table.data)
			assert.Equal(t, table.data, ss.handshakeData)
		})
	}
}

func TestSessionPoolAddHandshakeValidator(t *testing.T) {
	fa := func(data *HandshakeData) error { return nil }
	fb := func(data *HandshakeData) error { return errors.New("error") }

	tables := []struct {
		name       string
		validators map[string]func(data *HandshakeData) error
		result     int
	}{
		{"add validator", map[string]func(data *HandshakeData) error{"fa": fa}, 1},
		{"add many validators", map[string]func(data *HandshakeData) error{"fa": fa, "fb": fb}, 2},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			for name, fun := range table.validators {
				sessionPool.AddHandshakeValidator(name, fun)
			}
			session := newTestSession(sessionPool, nil, false).(*sessionImpl)
			validators := session.GetHandshakeValidators()
			assert.Equal(t, len(validators), table.result)
		})
	}
}

func TestSessionValidateHandshake(t *testing.T) {
	fa := func(data *HandshakeData) error { return nil }
	fb := func(data *HandshakeData) error { return errors.New("error") }

	tables := []struct {
		name       string
		validators map[string]func(data *HandshakeData) error
		errStr     string
	}{
		{"one passing validator", map[string]func(data *HandshakeData) error{"fa": fa}, ""},
		{"one failing validator", map[string]func(data *HandshakeData) error{"fb": fb}, "failed to run 'fb'"},
		{"many validators all passing", map[string]func(data *HandshakeData) error{"fa": fa, "fb": fa}, ""},
		{"many validators one failing", map[string]func(data *HandshakeData) error{"fa": fa, "fb": fb}, "failed to run 'fb'"},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			sessionPool := NewSessionPool()
			for name, fun := range table.validators {
				sessionPool.AddHandshakeValidator(name, fun)
			}
			session := newTestSession(sessionPool, nil, false).(*sessionImpl)
			err := session.ValidateHandshake(nil)

			if table.errStr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), table.errStr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRequestsInFlightCopySharesLock(t *testing.T) {
	sess := newTestSession(NewSessionPool(), nil, false).(*sessionImpl)
	requests := sess.GetRequestsInFlight()

	requests.mu.Lock()
	acquired := sess.requestsInFlight.mu.TryLock()
	if acquired {
		sess.requestsInFlight.mu.Unlock()
	}
	requests.mu.Unlock()

	assert.False(t, acquired)
	sess.SetRequestInFlight("request-id", "route", true)
	assert.True(t, sess.HasRequestsInFlight())
	sess.SetRequestInFlight("request-id", "", false)
	assert.False(t, sess.HasRequestsInFlight())
}
