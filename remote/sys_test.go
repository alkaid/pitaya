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
	"encoding/json"
	"testing"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/protos"
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
