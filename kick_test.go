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

package pitaya

import (
	"context"
	"testing"

	"github.com/alkaid/pitaya/v2/cluster"
	clustermocks "github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/session"
	sessionmocks "github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
)

func TestSendKickToUsersLocalSession(t *testing.T) {
	table := struct {
		uid1         string
		uid2         string
		frontendType string
		metadata     map[string]string
		reason       session.CloseReason
		err          error
	}{
		uuid.New().String(), uuid.New().String(), "connector",
		map[string]string{"source": "admin"}, session.CloseReasonKickManual, nil,
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s1 := sessionmocks.NewMockSession(ctrl)
	s2 := sessionmocks.NewMockSession(ctrl)
	s1.EXPECT().Kick(context.Background(), table.metadata, table.reason).Times(1).Return(table.err)
	s2.EXPECT().Kick(context.Background(), table.metadata, table.reason).Times(1).Return(table.err)

	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	mockSessionPool.EXPECT().GetSessionByUID(table.uid1).Return(s1).Times(1)
	mockSessionPool.EXPECT().GetSessionByUID(table.uid2).Return(s2).Times(1)

	app := &App{server: &cluster.Server{Frontend: true}, sessionPool: mockSessionPool}

	failedUids, err := app.SendKickToUsers([]string{table.uid1, table.uid2}, table.frontendType, table.metadata, table.reason)
	assert.Nil(t, failedUids)
	assert.NoError(t, err)
}

func TestSendKickToUsersFail(t *testing.T) {
	table := struct {
		uid1         string
		uid2         string
		frontendType string
		metadata     map[string]string
		reason       session.CloseReason
		err          error
	}{
		uuid.New().String(), uuid.New().String(), "connector",
		map[string]string{"source": "moderation"}, session.CloseReasonKickManual, constants.ErrKickingUsers,
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s1 := sessionmocks.NewMockSession(ctrl)
	s1.EXPECT().Kick(context.Background(), table.metadata, table.reason).Times(1).Return(nil)

	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	mockSessionPool.EXPECT().GetSessionByUID(table.uid1).Return(s1).Times(1)
	mockSessionPool.EXPECT().GetSessionByUID(table.uid2).Return(nil).Times(1)

	mockRPCClient := clustermocks.NewMockRPCClient(ctrl)
	mockRPCClient.EXPECT().SendKick(table.uid2, table.frontendType, &protos.KickMsg{
		UserId:   table.uid2,
		Metadata: table.metadata,
		Reason:   int32(table.reason),
	}).Return(table.err).Times(1)

	app := &App{
		server:      &cluster.Server{Frontend: true},
		sessionPool: mockSessionPool,
		rpcClient:   mockRPCClient,
	}

	failedUids, err := app.SendKickToUsers([]string{table.uid1, table.uid2}, table.frontendType, table.metadata, table.reason)
	assert.Len(t, failedUids, 1)
	assert.Equal(t, failedUids[0], table.uid2)
	assert.Equal(t, err, table.err)
}

func TestSendKickToUsersRemoteSession(t *testing.T) {
	tables := []struct {
		name         string
		uids         []string
		frontendType string
		metadata     map[string]string
		reason       session.CloseReason
		err          error
	}{
		{"success", []string{uuid.New().String(), uuid.New().String()}, "connector", map[string]string{"source": "admin"}, session.CloseReasonKickManual, nil},
		{"fail", []string{uuid.New().String(), uuid.New().String()}, "connector", map[string]string{"source": "moderation"}, session.CloseReasonKickManual, constants.ErrKickingUsers},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockRPCClient := clustermocks.NewMockRPCClient(ctrl)

			app := &App{
				server:      &cluster.Server{Frontend: true},
				sessionPool: session.NewSessionPool(),
				rpcClient:   mockRPCClient,
			}

			for _, uid := range table.uids {
				expectedKick := &protos.KickMsg{UserId: uid, Metadata: table.metadata, Reason: int32(table.reason)}
				mockRPCClient.EXPECT().SendKick(uid, table.frontendType, expectedKick).Return(table.err)
			}

			failedUids, err := app.SendKickToUsers(table.uids, table.frontendType, table.metadata, table.reason)
			assert.Equal(t, err, table.err)
			if table.err != nil {
				assert.NotNil(t, failedUids)
				assert.Equal(t, failedUids, table.uids)
			} else {
				assert.Nil(t, failedUids)
			}
		})
	}
}
