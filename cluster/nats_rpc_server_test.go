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

package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/helpers"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/metrics"
	metricsmocks "github.com/alkaid/pitaya/v2/metrics/mocks"
	"github.com/alkaid/pitaya/v2/protos"
	sessionmocks "github.com/alkaid/pitaya/v2/session/mocks"
	nats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/proto"
)

const statelessSubmissionErrorHelperEnv = "PITAYA_TEST_STATELESS_SUBMISSION_ERROR"

func TestGoWithRequestRejectsInvalidSubmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := make(chan struct{}, 1)

	err := GoWithRequest(ctx, &protos.Request{Session: &protos.Session{Uid: "42"}}, func(context.Context) {
		ran <- struct{}{}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("GoWithRequest error = %v, want %v", err, context.Canceled)
	}
	select {
	case <-ran:
		t.Fatal("pre-canceled request task ran")
	default:
	}

	err = GoWithRequest(context.Background(), nil, nil)
	if !errors.Is(err, co.ErrNilTask) {
		t.Fatalf("nil task error = %v, want %v", err, co.ErrNilTask)
	}
}

func TestGoWithRequestPropagatesStatelessSubmissionError(t *testing.T) {
	if os.Getenv(statelessSubmissionErrorHelperEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestGoWithRequestPropagatesStatelessSubmissionError$")
		cmd.Env = append(os.Environ(), statelessSubmissionErrorHelperEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("stateless submission helper failed: %v\n%s", err, output)
		}
		return
	}

	ants.Release()
	var ran bool

	err := GoWithRequest(context.Background(), nil, func(context.Context) {
		ran = true
	})
	if !errors.Is(err, ants.ErrPoolClosed) {
		t.Fatalf("GoWithRequest error = %v, want %v", err, ants.ErrPoolClosed)
	}
	if ran {
		t.Fatal("stateless task ran after submission rejection")
	}
}

func TestNormalizeTaskSubmissionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
	}{
		{name: "canceled", err: context.Canceled, code: 499},
		{name: "deadline", err: context.DeadlineExceeded, code: 504},
		{name: "closed", err: ants.ErrPoolClosed, code: 503},
		{name: "overload", err: ants.ErrPoolOverload, code: 503},
		{name: "nil task", err: co.ErrNilTask, code: 500},
		{name: "invalid user ID", err: co.ErrInvalidUserID, code: 500},
		{name: "unknown", err: errors.New("internal detail"), code: 500},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := NormalizeTaskSubmissionError(test.err)
			assert.Equal(t, test.code, apierrors.Code(got))
			assert.ErrorIs(t, got, test.err)
			assert.NotContains(t, apierrors.FromError(got).Status.Message, test.err.Error())
		})
	}
}

func TestNatsRPCServerMarshalResponseNormalizesNil(t *testing.T) {
	rpcServer := &NatsRPCServer{}

	payload, err := rpcServer.marshalResponse(nil)

	require.Error(t, err)
	response := &protos.Response{}
	require.NoError(t, proto.Unmarshal(payload, response))
	require.NotNil(t, response.Status)
	assert.Equal(t, int32(500), response.Status.Code)
}

func TestNatsRPCServerPreservesReliableMetadataInCallContext(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)
	natsServer := helpers.GetTestNatsServer(t)
	t.Cleanup(natsServer.Shutdown)
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", natsServer.Addr()), nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	rpcServer.conn = conn

	contexts := make(chan context.Context, 1)
	rpcServer.SetPitayaServer(&pitayaServerStub{call: func(ctx context.Context, _ *protos.Request) (*protos.Response, error) {
		contexts <- ctx
		return &protos.Response{}, nil
	}})
	reply := nats.NewInbox()
	sub, err := conn.SubscribeSync(reply)
	require.NoError(t, err)
	require.NoError(t, conn.Flush())
	processDone := make(chan struct{})
	go func() {
		defer close(processDone)
		rpcServer.processMessages(0)
	}()
	t.Cleanup(func() {
		close(rpcServer.GetUnhandledRequestsChannel())
		<-processDone
	})

	const jobID = "worker-jid-1"
	digest := strings.Repeat("a", 64)
	metadata, err := pcontext.Encode(pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.IdempotencyKey, jobID,
		rpccontext.ReliableJobKey, jobID,
		rpccontext.ReliableJobMetadataDigestKey, digest,
	))
	require.NoError(t, err)
	rpcServer.GetUnhandledRequestsChannel() <- &protos.Request{
		Metadata: metadata,
		Msg: &protos.Msg{
			Type:  protos.MsgType_MsgRequest,
			Route: "svc.method",
			Reply: reply,
		},
	}
	_, err = sub.NextMsg(time.Second)
	require.NoError(t, err)

	select {
	case received := <-contexts:
		assert.Equal(t, jobID, pcontext.GetFromPropagateCtx(received, constants.IdempotencyKey))
		assert.Equal(t, jobID, pcontext.GetFromPropagateCtx(received, rpccontext.ReliableJobKey))
		assert.Equal(t, digest, pcontext.GetFromPropagateCtx(received, rpccontext.ReliableJobMetadataDigestKey))
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for NATS RPC context")
	}
}

func TestNatsRPCServerRepliesAfterCallPanicAndContinues(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)

	natsServer := helpers.GetTestNatsServer(t)
	t.Cleanup(natsServer.Shutdown)
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", natsServer.Addr()), nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	rpcServer.conn = conn

	var calls atomic.Int32
	rpcServer.SetPitayaServer(&pitayaServerStub{call: func(context.Context, *protos.Request) (*protos.Response, error) {
		switch calls.Add(1) {
		case 1:
			panic("boom")
		case 2:
			return nil, errors.New("cached panic")
		default:
			return &protos.Response{Data: []byte("ok")}, nil
		}
	}})

	reply := nats.NewInbox()
	sub, err := conn.SubscribeSync(reply)
	require.NoError(t, err)
	require.NoError(t, conn.Flush())

	processDone := make(chan struct{})
	go func() {
		defer close(processDone)
		rpcServer.processMessages(0)
	}()
	t.Cleanup(func() {
		close(rpcServer.GetUnhandledRequestsChannel())
		<-processDone
	})

	send := func(id uint64) *protos.Response {
		t.Helper()
		rpcServer.GetUnhandledRequestsChannel() <- &protos.Request{
			Metadata: []byte("{}"),
			Msg: &protos.Msg{
				Id:    id,
				Type:  protos.MsgType_MsgRequest,
				Route: "svc.method",
				Reply: reply,
			},
		}
		msg, err := sub.NextMsg(time.Second)
		require.NoError(t, err)
		response := &protos.Response{}
		require.NoError(t, proto.Unmarshal(msg.Data, response))
		return response
	}

	for _, id := range []uint64{1, 2} {
		response := send(id)
		require.NotNil(t, response.Status)
		assert.Equal(t, int32(500), response.Status.Code)
	}
	response := send(3)
	assert.Nil(t, response.Status)
	assert.Equal(t, []byte("ok"), response.Data)
}

func TestNatsRPCServerPublishesSubmissionErrorResponse(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)

	natsServer := helpers.GetTestNatsServer(t)
	t.Cleanup(natsServer.Shutdown)
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", natsServer.Addr()), nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	rpcServer.conn = conn

	reply := nats.NewInbox()
	sub, err := conn.SubscribeSync(reply)
	require.NoError(t, err)
	require.NoError(t, conn.Flush())

	processDone := make(chan struct{})
	go func() {
		defer close(processDone)
		rpcServer.processMessages(0)
	}()
	t.Cleanup(func() {
		close(rpcServer.GetUnhandledRequestsChannel())
		<-processDone
	})

	rpcServer.GetUnhandledRequestsChannel() <- &protos.Request{
		Session:  &protos.Session{Uid: "0"},
		Metadata: []byte("{}"),
		Msg: &protos.Msg{
			Id:    1,
			Type:  protos.MsgType_MsgRequest,
			Route: "svc.method",
			Reply: reply,
		},
	}

	msg, err := sub.NextMsg(3 * time.Second)
	require.NoError(t, err)
	response := &protos.Response{}
	require.NoError(t, proto.Unmarshal(msg.Data, response))
	require.NotNil(t, response.Status)
	assert.Equal(t, int32(500), response.Status.Code)
	assert.Equal(t, "task submission failed", response.Status.Message)
	assert.NotContains(t, response.Status.Message, co.ErrInvalidUserID.Error())
}

type funcPtrMatcher struct {
	ptr uintptr
}

type pitayaServerStub struct {
	protos.UnimplementedPitayaServer
	call              func(context.Context, *protos.Request) (*protos.Response, error)
	sessionBindRemote func(context.Context, *protos.BindMsg) (*protos.Response, error)
	pushToUser        func(context.Context, *protos.Push) (*protos.Response, error)
	kickUser          func(context.Context, *protos.KickMsg) (*protos.KickAnswer, error)
}

func (s *pitayaServerStub) Call(ctx context.Context, req *protos.Request) (*protos.Response, error) {
	if s.call == nil {
		return s.UnimplementedPitayaServer.Call(ctx, req)
	}
	return s.call(ctx, req)
}

func (s *pitayaServerStub) SessionBindRemote(ctx context.Context, msg *protos.BindMsg) (*protos.Response, error) {
	return s.sessionBindRemote(ctx, msg)
}

func (s *pitayaServerStub) PushToUser(ctx context.Context, push *protos.Push) (*protos.Response, error) {
	return s.pushToUser(ctx, push)
}

func (s *pitayaServerStub) KickUser(ctx context.Context, kick *protos.KickMsg) (*protos.KickAnswer, error) {
	return s.kickUser(ctx, kick)
}

func newFuncPtrMatcher(x interface{}) *funcPtrMatcher {
	if reflect.ValueOf(x).Kind() != reflect.Func {
		panic("funcPtrMatcher only accepts functions as arguments")
	}
	return &funcPtrMatcher{
		ptr: reflect.ValueOf(x).Pointer(),
	}
}

func (m funcPtrMatcher) Matches(x interface{}) bool {
	v := reflect.ValueOf(x)
	switch v.Kind() {
	case reflect.Func:
		ptr := reflect.ValueOf(x).Pointer()
		return ptr == m.ptr
	default:
		return false
	}
}

func (m funcPtrMatcher) String() string {
	return fmt.Sprintf("has address %d", m.ptr)
}

func TestNewNatsRPCServer(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	n, err := NewNatsRPCServer(cfg, sv, mockMetricsReporters, nil, mockSessionPool)
	assert.NoError(t, err)
	assert.NotNil(t, n)
	assert.Equal(t, sv, n.server)
	assert.Equal(t, mockMetricsReporters, n.metricsReporters)
}

func TestNatsRPCServerConfigure(t *testing.T) {
	t.Parallel()
	tables := []struct {
		name    string
		mutate  func(*config.NatsRPCServerConfig)
		want    string
		wantErr error
	}{
		{name: "valid"},
		{
			name:    "empty connect",
			mutate:  func(cfg *config.NatsRPCServerConfig) { cfg.Connect = " \n" },
			want:    "pitaya.cluster.rpc.server.nats.connect",
			wantErr: constants.ErrNoNatsConnectionString,
		},
		{
			name:   "connection timeout",
			mutate: func(cfg *config.NatsRPCServerConfig) { cfg.ConnectionTimeout = 0 },
			want:   "pitaya.cluster.rpc.server.nats.connectiontimeout",
		},
		{
			name:   "request timeout",
			mutate: func(cfg *config.NatsRPCServerConfig) { cfg.RequestTimeout = -time.Second },
			want:   "pitaya.cluster.rpc.server.nats.requesttimeout",
		},
		{
			name:   "reconnect retries",
			mutate: func(cfg *config.NatsRPCServerConfig) { cfg.MaxReconnectionRetries = -2 },
			want:   "pitaya.cluster.rpc.server.nats.maxreconnectionretries",
		},
		{
			name:    "parallel services",
			mutate:  func(cfg *config.NatsRPCServerConfig) { cfg.Services = 2 },
			want:    "pitaya.cluster.rpc.server.nats.services",
			wantErr: constants.ErrNatsRPCServicesUnsupported,
		},
		{
			name:    "negative messages buffer",
			mutate:  func(cfg *config.NatsRPCServerConfig) { cfg.Buffer.Messages = -1 },
			want:    "pitaya.cluster.rpc.server.nats.buffer.messages",
			wantErr: constants.ErrNatsMessagesBufferSizeZero,
		},
		{
			name:    "zero push buffer",
			mutate:  func(cfg *config.NatsRPCServerConfig) { cfg.Buffer.Push = 0 },
			want:    "pitaya.cluster.rpc.server.nats.buffer.push",
			wantErr: constants.ErrNatsPushBufferSizeZero,
		},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
			if table.mutate != nil {
				table.mutate(&cfg)
			}
			rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
			if table.want == "" {
				require.NoError(t, err)
				assert.Equal(t, cfg.Buffer.Messages, rpcServer.messagesBufferSize)
				assert.Equal(t, cfg.Buffer.Messages, cap(rpcServer.subChan))
				assert.Equal(t, cfg.Buffer.Messages, cap(rpcServer.bindingsChan))
				assert.Equal(t, cfg.Buffer.Messages, cap(rpcServer.userKickCh))
				return
			}
			require.Error(t, err)
			assert.Nil(t, rpcServer)
			assert.Contains(t, err.Error(), table.want)
			if table.wantErr != nil {
				assert.ErrorIs(t, err, table.wantErr)
			}
		})
	}
}

func TestNatsRPCServerGetUserMessagesTopic(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "pitaya/connector/user/bla/push", GetUserMessagesTopic("bla", "connector"))
	assert.Equal(t, "pitaya/game/user/123bla/push", GetUserMessagesTopic("123bla", "game"))
	assert.Equal(t, "pitaya/connector/user/1/push", GetUserMessagesTopic("1", "connector"))
}

func TestNatsRPCServerGetUserKickTopic(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "pitaya/connector/user/0/kick", GetUserKickTopic("0", "connector"))
	assert.Equal(t, "pitaya/game/user/1/kick", GetUserKickTopic("1", "game"))
	assert.Equal(t, "pitaya/connector/user/10/kick", GetUserKickTopic("10", "connector"))
	assert.Equal(t, "pitaya/game/user/11/kick", GetUserKickTopic("11", "game"))
}

func TestNatsRPCServerGetUnhandledRequestsChannel(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	n, _ := NewNatsRPCServer(cfg, sv, nil, nil, nil)
	assert.NotNil(t, n.GetUnhandledRequestsChannel())
	assert.IsType(t, make(chan *protos.Request), n.GetUnhandledRequestsChannel())
}

func TestNatsRPCServerGetBindingsChannel(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	n, _ := NewNatsRPCServer(cfg, sv, nil, nil, nil)
	assert.Equal(t, n.bindingsChan, n.GetBindingsChannel())
}

func TestNatsRPCServerOnSessionBind(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()

	ctrl := gomock.NewController(t)
	mockSession := sessionmocks.NewMockSession(ctrl)
	mockSession.EXPECT().UID().Return("uid").Times(2)
	mockSession.EXPECT().SetSubscriptions(gomock.Len(2)).Times(1)

	rpcServer, _ := NewNatsRPCServer(cfg, sv, nil, nil, nil)
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	assert.NoError(t, err)
	rpcServer.conn = conn
	err = rpcServer.onSessionBind(context.Background(), mockSession, nil)
	assert.NoError(t, err)
	assert.NotNil(t, rpcServer.userKickCh)
}

func TestNatsRPCServerSubscribeToBindingsChannel(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	rpcServer, _ := NewNatsRPCServer(cfg, sv, nil, nil, nil)
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	assert.NoError(t, err)
	rpcServer.conn = conn
	err = rpcServer.subscribeToBindingsChannel()
	assert.NoError(t, err)
	dt := []byte("somedata")
	conn.Publish(GetBindBroadcastTopic(sv.Type), dt)
	msg := helpers.ShouldEventuallyReceive(t, rpcServer.GetBindingsChannel()).(*nats.Msg)
	assert.Equal(t, msg.Data, dt)
}

func TestNatsRPCServerSubscribeUserKickChannel(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	rpcServer, _ := NewNatsRPCServer(cfg, sv, nil, nil, nil)
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	assert.NoError(t, err)
	rpcServer.conn = conn
	sub, err := rpcServer.subscribeToUserKickChannel("someuid", sv.Type)
	assert.NoError(t, err)
	assert.NotNil(t, sub)
	kick := &protos.KickMsg{UserId: "randomid"}
	dt, err := proto.Marshal(kick)
	assert.NoError(t, err)
	err = conn.Publish(GetUserKickTopic("someuid", sv.Type), dt)
	assert.NoError(t, err)
	msg := helpers.ShouldEventuallyReceive(t, rpcServer.getUserKickChannel()).(*protos.KickMsg)
	assert.Equal(t, msg.UserId, kick.UserId)
}

func TestNatsRPCServerGetUserPushChannel(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	n, _ := NewNatsRPCServer(cfg, sv, nil, nil, nil)
	assert.NotNil(t, n.getUserPushChannel())
	assert.IsType(t, make(chan *protos.Push), n.getUserPushChannel())
}

func TestNatsRPCServerGetUserKickChannel(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	n, _ := NewNatsRPCServer(cfg, sv, nil, nil, nil)
	assert.NotNil(t, n.getUserKickChannel())
	assert.IsType(t, make(chan *protos.KickMsg), n.getUserKickChannel())
}

func TestNatsRPCServerSubscribeToUserMessages(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	rpcServer, _ := NewNatsRPCServer(cfg, sv, nil, nil, nil)
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	assert.NoError(t, err)
	rpcServer.conn = conn
	tables := []struct {
		uid    string
		svType string
		msg    []byte
	}{
		{"user1", "conn", []byte("msg1")},
		{"user2", "game", []byte("")},
		{"u", "conn", []byte("000")},
	}

	for _, table := range tables {
		t.Run(table.uid, func(t *testing.T) {
			subs, err := rpcServer.subscribeToUserMessages(table.uid, table.svType)
			assert.NoError(t, err)
			assert.Equal(t, true, subs.IsValid())
			conn.Publish(GetUserMessagesTopic(table.uid, table.svType), table.msg)
			helpers.ShouldEventuallyReceive(t, rpcServer.userPushCh)
		})
	}
}

func TestNatsRPCServerSubscribe(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	rpcServer, _ := NewNatsRPCServer(cfg, sv, nil, nil, nil)
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	assert.NoError(t, err)
	rpcServer.conn = conn
	tables := []struct {
		topic string
		msg   []byte
	}{
		{"user1/messages", []byte("msg1")},
		{"user2/messages", []byte("")},
		{"u/messages", []byte("000")},
	}

	for _, table := range tables {
		t.Run(table.topic, func(t *testing.T) {
			subs, err := rpcServer.subscribe(table.topic, false)
			assert.NoError(t, err)
			assert.Equal(t, true, subs.IsValid())
			conn.Publish(table.topic, table.msg)
			r := helpers.ShouldEventuallyReceive(t, rpcServer.subChan).(*nats.Msg)
			assert.Equal(t, table.msg, r.Data)
		})
	}
}

func TestNatsRPCServerHandleMessages(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}

	rpcServer, _ := NewNatsRPCServer(cfg, sv, mockMetricsReporters, nil, nil)
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	assert.NoError(t, err)
	rpcServer.conn = conn
	tables := []struct {
		topic string
		req   *protos.Request
	}{
		{"user1/messages", &protos.Request{Type: protos.RPCType_Sys, FrontendID: "bla", Msg: &protos.Msg{Id: 1, Route: "svc.method", Reply: "ae"}}},
		{"user2/messages", &protos.Request{Type: protos.RPCType_User, FrontendID: "bla2", Msg: &protos.Msg{Id: 1, Route: "svc.method", Reply: "boa"}}},
	}

	for i, table := range tables {
		sub, err := rpcServer.subscribe(table.topic, false)
		require.NoError(t, err)
		require.True(t, sub.IsValid())
		if i == 0 {
			rpcServer.sub = sub
		}
	}

	pumpDone := make(chan struct{})
	go func() {
		rpcServer.handleMessages()
		close(pumpDone)
	}()
	defer func() {
		require.NoError(t, rpcServer.Shutdown())
		select {
		case <-pumpDone:
		case <-time.After(3 * time.Second):
			t.Error("nats rpc message pump did not stop")
		}
	}()

	for _, table := range tables {
		t.Run(table.topic, func(t *testing.T) {
			b, err := proto.Marshal(table.req)
			assert.NoError(t, err)

			mockMetricsReporter.EXPECT().ReportGauge(metrics.DroppedMessages, gomock.Any(), float64(0))
			mockMetricsReporter.EXPECT().ReportGauge(metrics.ChannelCapacity, gomock.Any(), gomock.Any()).Times(3)

			require.NoError(t, conn.PublishRequest(table.topic, table.req.Msg.Reply, b))
			r := helpers.ShouldEventuallyReceive(t, rpcServer.unhandledReqCh).(*protos.Request)
			assert.Equal(t, table.req.FrontendID, r.FrontendID)
			assert.Equal(t, table.req.Msg.Id, r.Msg.Id)
		})
	}
}

func TestNatsRPCServerHandleMessagesRejectsMalformedRequestsAndContinues(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)

	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	require.NoError(t, err)
	rpcServer.conn = conn

	const topic = "malformed/messages"
	rpcServer.sub, err = rpcServer.subscribe(topic, false)
	require.NoError(t, err)

	pumpDone := make(chan struct{})
	go func() {
		rpcServer.handleMessages()
		close(pumpDone)
	}()
	defer func() {
		require.NoError(t, rpcServer.Shutdown())
		select {
		case <-pumpDone:
		case <-time.After(3 * time.Second):
			t.Error("nats rpc message pump did not stop")
		}
	}()

	require.NoError(t, conn.Publish(topic, nil))
	missingMsg, err := proto.Marshal(&protos.Request{Type: protos.RPCType_User})
	require.NoError(t, err)
	require.NoError(t, conn.Publish(topic, missingMsg))

	valid := &protos.Request{
		Type:       protos.RPCType_Sys,
		FrontendID: "frontend-1",
		Msg: &protos.Msg{
			Id:    7,
			Route: "svc.method",
			Type:  protos.MsgType_MsgRequest,
		},
	}
	payload, err := proto.Marshal(valid)
	require.NoError(t, err)
	require.NoError(t, conn.PublishRequest(topic, "valid.reply", payload))

	received := helpers.ShouldEventuallyReceive(t, rpcServer.unhandledReqCh).(*protos.Request)
	assert.Equal(t, valid.FrontendID, received.FrontendID)
	assert.Equal(t, valid.Msg.Route, received.Msg.Route)
	assert.Equal(t, "valid.reply", received.Msg.Reply)
	assert.Equal(t, uint64(2), rpcServer.malformedRequests.Load())
}

func TestNatsRPCServerHandleMessagesSignalsAppDieWhenSubscriptionChannelCloses(t *testing.T) {
	dieChan := make(chan bool, 1)
	rpcServer := &NatsRPCServer{
		appDieChan:     dieChan,
		stopChan:       make(chan bool),
		subChan:        make(chan *nats.Msg),
		bindingsChan:   make(chan *nats.Msg),
		unhandledReqCh: make(chan *protos.Request),
	}
	rpcServer.running.Store(true)

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		rpcServer.handleMessages()
	}()
	close(rpcServer.subChan)

	select {
	case <-dieChan:
	case <-time.After(time.Second):
		t.Fatal("unexpected message pump exit did not signal app failure")
	}
	select {
	case <-pumpDone:
	case <-time.After(time.Second):
		t.Fatal("message pump did not return after its subscription channel closed")
	}
	assert.False(t, rpcServer.IsRunning(), "failed message pump remained ready")
}

func TestNatsRPCServerAddInboundSubscriptionDoesNotHoldInboundLock(t *testing.T) {
	rpcServer := &NatsRPCServer{}
	subscribeErr := errors.New("subscribe failed")
	lockWasAvailable := false

	_, err := rpcServer.addInboundSubscription(func() (*nats.Subscription, error) {
		lockWasAvailable = rpcServer.inboundMu.TryLock()
		if lockWasAvailable {
			rpcServer.inboundMu.Unlock()
		}
		return nil, subscribeErr
	})

	require.ErrorIs(t, err, subscribeErr)
	assert.True(t, lockWasAvailable, "subscribe closure ran while inboundMu was held")
}

func TestNatsRPCServerShutdownWaitsForInFlightInboundRegistration(t *testing.T) {
	rpcServer := &NatsRPCServer{stopChan: make(chan bool)}
	registrationStarted := make(chan struct{})
	releaseRegistration := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRegistration) }) }
	defer release()

	registrationErr := errors.New("subscribe failed")
	registrationDone := make(chan error, 1)
	go func() {
		_, err := rpcServer.addInboundSubscription(func() (*nats.Subscription, error) {
			close(registrationStarted)
			<-releaseRegistration
			return nil, registrationErr
		})
		registrationDone <- err
	}()
	<-registrationStarted

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- rpcServer.Shutdown()
	}()
	require.Eventually(t, func() bool {
		rpcServer.inboundMu.Lock()
		defer rpcServer.inboundMu.Unlock()
		return rpcServer.inboundClosed
	}, time.Second, time.Millisecond, "shutdown did not close inbound admission")

	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before the in-flight registration completed: %v", err)
	default:
	}
	closureCalled := false
	_, err := rpcServer.addInboundSubscription(func() (*nats.Subscription, error) {
		closureCalled = true
		return nil, nil
	})
	require.ErrorIs(t, err, errNatsRPCServerStopping)
	assert.False(t, closureCalled, "subscription closure ran after admission closed")

	release()
	require.ErrorIs(t, <-registrationDone, registrationErr)
	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after the in-flight registration completed")
	}
}

func TestNatsRPCServerShutdownDoesNotDependOnSubscriptionClosedHandler(t *testing.T) {
	natsServer := helpers.GetTestNatsServer(t)
	defer natsServer.Shutdown()

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)
	rpcServer.conn, err = setupNatsConn(fmt.Sprintf("nats://%s", natsServer.Addr()), nil)
	require.NoError(t, err)
	defer rpcServer.conn.Close()

	sub, err := rpcServer.addInboundSubscription(func() (*nats.Subscription, error) {
		return rpcServer.conn.Subscribe("shutdown.closed-handler", func(*nats.Msg) {})
	})
	require.NoError(t, err)
	require.NoError(t, rpcServer.conn.Flush())
	rpcServer.inboundMu.Lock()
	tracked := rpcServer.inboundSubscriptions[sub]
	rpcServer.inboundMu.Unlock()
	require.NotNil(t, tracked)

	userClosedHandlerCalled := make(chan struct{}, 1)
	sub.SetClosedHandler(func(string) {
		userClosedHandlerCalled <- struct{}{}
	})
	rpcServer.handlerMu.Lock()
	rpcServer.consumersStarted = true
	rpcServer.handlerMu.Unlock()

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- rpcServer.Shutdown()
	}()
	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		tracked.complete()
		select {
		case <-shutdownDone:
		case <-time.After(time.Second):
		}
		t.Fatal("Shutdown depended on the replaceable subscription closed handler")
	}
	select {
	case <-userClosedHandlerCalled:
	case <-time.After(time.Second):
		t.Fatal("replacement subscription closed handler was not called")
	}
}

func TestNatsRPCServerShutdownAfterConnectionCloseReleasesInboundCallback(t *testing.T) {
	natsServer := helpers.GetTestNatsServer(t)
	defer natsServer.Shutdown()
	url := fmt.Sprintf("nats://%s", natsServer.Addr())

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	cfg.Buffer.Push = 1
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)
	rpcServer.conn, err = setupNatsConn(url, nil)
	require.NoError(t, err)

	_, err = rpcServer.subscribeToUserMessages("closed-connection-user", rpcServer.server.Type)
	require.NoError(t, err)
	require.NoError(t, rpcServer.conn.Flush())
	publisher, err := setupNatsConn(url, nil)
	require.NoError(t, err)
	defer publisher.Close()

	rpcServer.userPushCh <- &protos.Push{Uid: "buffer-filler"}
	payload, err := proto.Marshal(&protos.Push{Uid: "closed-connection-user"})
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(
		GetUserMessagesTopic("closed-connection-user", rpcServer.server.Type),
		payload,
	))
	require.NoError(t, publisher.Flush())
	require.Eventually(t, func() bool {
		rpcServer.inboundMu.Lock()
		defer rpcServer.inboundMu.Unlock()
		return rpcServer.inboundCallbacks == 1
	}, time.Second, time.Millisecond, "inbound callback did not block on the full local buffer")

	// Exercise the graceful branch: an already closed connection cannot emit the
	// subscription status that branch normally uses as its completion barrier.
	rpcServer.handlerMu.Lock()
	rpcServer.consumersStarted = true
	rpcServer.handlerMu.Unlock()
	rpcServer.conn.Close()

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- rpcServer.Shutdown()
	}()
	select {
	case shutdownErr := <-shutdownDone:
		require.NoError(t, shutdownErr)
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown waited for a subscription status after the connection closed")
	}

	rpcServer.inboundMu.Lock()
	callbacks := rpcServer.inboundCallbacks
	callbacksClosed := rpcServer.inboundCallbacksClosed
	rpcServer.inboundMu.Unlock()
	assert.Zero(t, callbacks)
	assert.True(t, callbacksClosed)
	assert.False(t, rpcServer.beginInboundCallback(), "late callback entered after shutdown")
}

func TestNatsRPCServerShutdownWaitsForChannelDeliveryToStop(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	dieChan := make(chan bool, 1)
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, dieChan, nil)
	require.NoError(t, err)

	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	rpcServer.conn, err = setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	require.NoError(t, err)
	defer rpcServer.conn.Close()
	publisher, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	require.NoError(t, err)
	defer publisher.Close()

	const topic = "shutdown-drain/messages"
	rpcServer.sub, err = rpcServer.subscribe(topic, false)
	require.NoError(t, err)
	require.NoError(t, rpcServer.subscribeToBindingsChannel())
	require.NoError(t, rpcServer.conn.Flush())
	rpcServer.SetPitayaServer(&pitayaServerStub{
		sessionBindRemote: func(context.Context, *protos.BindMsg) (*protos.Response, error) {
			return &protos.Response{}, nil
		},
	})
	require.NoError(t, rpcServer.startHandler(rpcServer.processSessionBindings))

	payload, err := proto.Marshal(&protos.Request{
		Type: protos.RPCType_User,
		Msg: &protos.Msg{
			Type:  protos.MsgType_MsgNotify,
			Route: "svc.method",
		},
	})
	require.NoError(t, err)
	bindPayload, err := proto.Marshal(&protos.BindMsg{Uid: "shutdown-user", Fid: "frontend-1"})
	require.NoError(t, err)

	pumpDone := make(chan struct{})
	go func() {
		rpcServer.handleMessages()
		close(pumpDone)
	}()

	for range cfg.Buffer.Messages + 1 {
		require.NoError(t, publisher.Publish(topic, payload))
	}
	require.NoError(t, publisher.Flush())
	require.Eventually(t, func() bool {
		return len(rpcServer.subChan) > 0
	}, 3*time.Second, time.Millisecond)

	keepPublishing := make(chan struct{})
	publisherDone := make(chan struct{})
	go func() {
		defer close(publisherDone)
		for {
			select {
			case <-keepPublishing:
				return
			default:
				_ = publisher.Publish(topic, payload)
				_ = publisher.Publish(GetBindBroadcastTopic(rpcServer.server.Type), bindPayload)
			}
		}
	}()

	require.NoError(t, rpcServer.Shutdown())
	select {
	case <-pumpDone:
	case <-time.After(5 * time.Second):
		close(keepPublishing)
		<-publisherDone
		t.Fatal("nats rpc message pump did not finish draining")
	}
	close(keepPublishing)
	<-publisherDone
	assert.True(t, rpcServer.conn.IsClosed())
	select {
	case <-dieChan:
		t.Fatal("normal nats rpc shutdown signaled app failure")
	default:
	}
}

func TestNatsRPCServerShutdownDrainsBufferedTransportMessages(t *testing.T) {
	natsServer := helpers.GetTestNatsServer(t)
	defer natsServer.Shutdown()

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", natsServer.Addr())
	cfg.Buffer.Messages = 8
	cfg.Buffer.Push = 8

	ctrl := gomock.NewController(t)
	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, mockSessionPool)
	require.NoError(t, err)
	mockSessionPool.EXPECT().OnSessionBind(newFuncPtrMatcher(rpcServer.onSessionBind))

	releaseCallbacks := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseCallbacks) })
	}
	var taskMuLocked bool
	defer func() {
		release()
		if taskMuLocked {
			rpcServer.taskMu.Unlock()
		}
		_ = rpcServer.Shutdown()
	}()

	requestCalls := make(chan string, 2)
	bindingCalls := make(chan string, 2)
	pushCalls := make(chan string, 2)
	kickCalls := make(chan string, 2)
	bindingStarted := make(chan struct{})
	pushStarted := make(chan struct{})
	kickStarted := make(chan struct{})
	var bindingOnce sync.Once
	var pushOnce sync.Once
	var kickOnce sync.Once
	rpcServer.SetPitayaServer(&pitayaServerStub{
		call: func(_ context.Context, req *protos.Request) (*protos.Response, error) {
			requestCalls <- fmt.Sprintf("%d:%s", req.GetMsg().GetId(), req.GetMsg().GetType())
			return &protos.Response{}, nil
		},
		sessionBindRemote: func(_ context.Context, msg *protos.BindMsg) (*protos.Response, error) {
			bindingOnce.Do(func() {
				close(bindingStarted)
				<-releaseCallbacks
			})
			bindingCalls <- msg.GetUid()
			return &protos.Response{}, nil
		},
		pushToUser: func(_ context.Context, push *protos.Push) (*protos.Response, error) {
			pushOnce.Do(func() {
				close(pushStarted)
				<-releaseCallbacks
			})
			pushCalls <- push.GetUid()
			return &protos.Response{}, nil
		},
		kickUser: func(_ context.Context, kick *protos.KickMsg) (*protos.KickAnswer, error) {
			kickOnce.Do(func() {
				close(kickStarted)
				<-releaseCallbacks
			})
			kickCalls <- kick.GetUserId()
			return &protos.KickAnswer{}, nil
		},
	})
	require.NoError(t, rpcServer.Init())

	marshalBinding := func(uid string) *nats.Msg {
		data, marshalErr := proto.Marshal(&protos.BindMsg{Uid: uid, Fid: "frontend-1"})
		require.NoError(t, marshalErr)
		return &nats.Msg{Data: data}
	}
	rpcServer.bindingsChan <- marshalBinding("binding-1")
	rpcServer.userPushCh <- &protos.Push{Uid: "push-1"}
	rpcServer.userKickCh <- &protos.KickMsg{UserId: "kick-1"}
	select {
	case <-bindingStarted:
	case <-time.After(time.Second):
		t.Fatal("binding consumer did not start")
	}
	select {
	case <-pushStarted:
	case <-time.After(time.Second):
		t.Fatal("push consumer did not start")
	}
	select {
	case <-kickStarted:
	case <-time.After(time.Second):
		t.Fatal("kick consumer did not start")
	}
	rpcServer.bindingsChan <- marshalBinding("binding-2")
	rpcServer.userPushCh <- &protos.Push{Uid: "push-2"}
	rpcServer.userKickCh <- &protos.KickMsg{UserId: "kick-2"}

	marshalRequest := func(id uint64, msgType protos.MsgType, reply string) *nats.Msg {
		data, marshalErr := proto.Marshal(&protos.Request{
			Type:     protos.RPCType_User,
			Metadata: []byte("{}"),
			Msg: &protos.Msg{
				Id:    id,
				Type:  msgType,
				Route: "svc.method",
			},
		})
		require.NoError(t, marshalErr)
		return &nats.Msg{Data: data, Reply: reply}
	}
	rpcServer.taskMu.Lock()
	taskMuLocked = true
	rpcServer.subChan <- marshalRequest(1, protos.MsgType_MsgRequest, "shutdown.buffered.reply")
	rpcServer.subChan <- marshalRequest(2, protos.MsgType_MsgNotify, "")
	require.Eventually(t, func() bool {
		return len(rpcServer.subChan) == 0
	}, time.Second, time.Millisecond, "request dispatcher did not reach the task-admission gate")

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- rpcServer.Shutdown()
	}()
	select {
	case <-rpcServer.transportDraining:
	case <-time.After(3 * time.Second):
		t.Error("shutdown did not start the private transport drain phase")
	}
	assert.False(t, rpcServer.conn.IsClosed(), "connection closed before buffered work reached its consumers")

	release()
	rpcServer.taskMu.Unlock()
	taskMuLocked = false
	select {
	case shutdownErr := <-shutdownDone:
		require.NoError(t, shutdownErr)
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not finish after buffered work was released")
	}

	drainStrings := func(ch <-chan string) []string {
		var values []string
		for {
			select {
			case value := <-ch:
				values = append(values, value)
			default:
				return values
			}
		}
	}
	assert.ElementsMatch(t, []string{
		fmt.Sprintf("1:%s", protos.MsgType_MsgRequest),
		fmt.Sprintf("2:%s", protos.MsgType_MsgNotify),
	}, drainStrings(requestCalls))
	assert.ElementsMatch(t, []string{"binding-1", "binding-2"}, drainStrings(bindingCalls))
	assert.ElementsMatch(t, []string{"push-1", "push-2"}, drainStrings(pushCalls))
	assert.ElementsMatch(t, []string{"kick-1", "kick-2"}, drainStrings(kickCalls))
	select {
	case binding := <-rpcServer.GetBindingsChannel():
		t.Fatalf("public bindings channel exposed an internal drain value: %v", binding)
	default:
	}
	select {
	case request, ok := <-rpcServer.GetUnhandledRequestsChannel():
		assert.False(t, ok, "public request channel exposed an internal drain value: %v", request)
	default:
		t.Fatal("public request channel was not closed after its owned consumer drained")
	}
	assert.Empty(t, rpcServer.subChan)
	assert.Empty(t, rpcServer.bindingsChan)
	assert.Empty(t, rpcServer.userPushCh)
	assert.Empty(t, rpcServer.userKickCh)
}

func TestNatsRPCServerConsumersPreferSignaledTransportDrain(t *testing.T) {
	draining := make(chan struct{})
	close(draining)

	t.Run("binding", func(t *testing.T) {
		payload, err := proto.Marshal(&protos.BindMsg{Uid: "drained-binding"})
		require.NoError(t, err)
		called := make(chan string, 1)
		rpcServer := &NatsRPCServer{
			stopChan:          make(chan bool),
			transportDraining: draining,
			bindingsChan:      make(chan *nats.Msg, 1),
		}
		rpcServer.SetPitayaServer(&pitayaServerStub{
			sessionBindRemote: func(_ context.Context, msg *protos.BindMsg) (*protos.Response, error) {
				called <- msg.GetUid()
				return &protos.Response{}, nil
			},
		})
		rpcServer.bindingsChan <- &nats.Msg{Data: payload}

		rpcServer.processSessionBindings()

		assert.Equal(t, "drained-binding", helpers.ShouldEventuallyReceive(t, called))
	})

	t.Run("push", func(t *testing.T) {
		called := make(chan string, 1)
		rpcServer := &NatsRPCServer{
			stopChan:          make(chan bool),
			transportDraining: draining,
			userPushCh:        make(chan *protos.Push, 1),
		}
		rpcServer.SetPitayaServer(&pitayaServerStub{
			pushToUser: func(_ context.Context, push *protos.Push) (*protos.Response, error) {
				called <- push.GetUid()
				return &protos.Response{}, nil
			},
		})
		rpcServer.userPushCh <- &protos.Push{Uid: "drained-push"}

		rpcServer.processPushes()

		assert.Equal(t, "drained-push", helpers.ShouldEventuallyReceive(t, called))
	})

	t.Run("kick", func(t *testing.T) {
		called := make(chan string, 1)
		rpcServer := &NatsRPCServer{
			stopChan:          make(chan bool),
			transportDraining: draining,
			userKickCh:        make(chan *protos.KickMsg, 1),
		}
		rpcServer.SetPitayaServer(&pitayaServerStub{
			kickUser: func(_ context.Context, kick *protos.KickMsg) (*protos.KickAnswer, error) {
				called <- kick.GetUserId()
				return &protos.KickAnswer{}, nil
			},
		})
		rpcServer.userKickCh <- &protos.KickMsg{UserId: "drained-kick"}

		rpcServer.processKick()

		assert.Equal(t, "drained-kick", helpers.ShouldEventuallyReceive(t, called))
	})
}

func TestNatsRPCServerShutdownWaitsForInboundCallbackBeforeLocalDrain(t *testing.T) {
	natsServer := helpers.GetTestNatsServer(t)
	defer natsServer.Shutdown()
	url := fmt.Sprintf("nats://%s", natsServer.Addr())

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	cfg.Connect = url
	cfg.Buffer.Push = 1
	ctrl := gomock.NewController(t)
	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, mockSessionPool)
	require.NoError(t, err)
	mockSessionPool.EXPECT().OnSessionBind(newFuncPtrMatcher(rpcServer.onSessionBind))

	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseCallback) })
	}
	defer func() {
		release()
		_ = rpcServer.Shutdown()
	}()

	processed := make(chan string, 3)
	callbackStarted := make(chan struct{})
	var callbackOnce sync.Once
	rpcServer.SetPitayaServer(&pitayaServerStub{
		pushToUser: func(_ context.Context, push *protos.Push) (*protos.Response, error) {
			callbackOnce.Do(func() {
				close(callbackStarted)
				<-releaseCallback
			})
			processed <- push.GetUid()
			return &protos.Response{}, nil
		},
	})
	require.NoError(t, rpcServer.Init())

	userSub, err := rpcServer.subscribeToUserMessages("callback-user", rpcServer.server.Type)
	require.NoError(t, err)
	require.NoError(t, rpcServer.conn.Flush())
	publisher, err := setupNatsConn(url, nil)
	require.NoError(t, err)
	defer publisher.Close()

	rpcServer.userPushCh <- &protos.Push{Uid: "in-progress"}
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("push consumer did not start")
	}
	rpcServer.userPushCh <- &protos.Push{Uid: "buffered"}
	payload, err := proto.Marshal(&protos.Push{Uid: "nats-callback"})
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(GetUserMessagesTopic("callback-user", rpcServer.server.Type), payload))
	require.NoError(t, publisher.Flush())
	require.Eventually(t, func() bool {
		pending, _, pendingErr := userSub.Pending()
		return pendingErr == nil && pending == 1
	}, time.Second, time.Millisecond, "NATS callback did not block on the full local channel")

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- rpcServer.Shutdown()
	}()
	require.Eventually(t, userSub.IsDraining, time.Second, time.Millisecond)
	select {
	case <-rpcServer.transportDraining:
		t.Fatal("local drain started before the inbound callback completed")
	default:
	}

	release()
	select {
	case shutdownErr := <-shutdownDone:
		require.NoError(t, shutdownErr)
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not finish after the inbound callback completed")
	}

	var got []string
	for {
		select {
		case uid := <-processed:
			got = append(got, uid)
		default:
			assert.ElementsMatch(t, []string{"in-progress", "buffered", "nats-callback"}, got)
			return
		}
	}
}

func TestNatsRPCServerShutdownStopsSessionBindingHandlerWithoutClosingDeliveryChannel(t *testing.T) {
	rpcServer := &NatsRPCServer{
		stopChan:     make(chan bool),
		bindingsChan: make(chan *nats.Msg, 1),
	}
	require.NoError(t, rpcServer.startHandler(rpcServer.processSessionBindings))
	require.NoError(t, rpcServer.Shutdown())
	require.NotPanics(t, func() {
		rpcServer.bindingsChan <- &nats.Msg{}
	})
}

func TestNatsRPCServerShutdownCancelsSaturatedUserCallbacks(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	url := fmt.Sprintf("nats://%s", s.Addr())

	tests := []struct {
		name      string
		topic     string
		message   proto.Message
		subscribe func(*NatsRPCServer) (*nats.Subscription, error)
		fill      func(*NatsRPCServer)
	}{
		{
			name:    "push",
			topic:   GetUserMessagesTopic("shutdown-user", "frontend"),
			message: &protos.Push{Uid: "shutdown-user"},
			subscribe: func(ns *NatsRPCServer) (*nats.Subscription, error) {
				return ns.subscribeToUserMessages("shutdown-user", "frontend")
			},
			fill: func(ns *NatsRPCServer) {
				ns.userPushCh <- &protos.Push{Uid: "buffer-filler"}
			},
		},
		{
			name:    "kick",
			topic:   GetUserKickTopic("shutdown-user", "frontend"),
			message: &protos.KickMsg{UserId: "shutdown-user"},
			subscribe: func(ns *NatsRPCServer) (*nats.Subscription, error) {
				return ns.subscribeToUserKickChannel("shutdown-user", "frontend")
			},
			fill: func(ns *NatsRPCServer) {
				ns.userKickCh <- &protos.KickMsg{UserId: "buffer-filler"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
			cfg.Buffer.Messages = 1
			cfg.Buffer.Push = 1
			rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
			require.NoError(t, err)

			rpcServer.conn, err = setupNatsConn(url, nil, nats.DrainTimeout(25*time.Millisecond))
			require.NoError(t, err)
			defer rpcServer.conn.Close()
			publisher, err := setupNatsConn(url, nil)
			require.NoError(t, err)
			defer publisher.Close()

			sub, err := tt.subscribe(rpcServer)
			require.NoError(t, err)
			require.NoError(t, rpcServer.conn.Flush())
			require.NoError(t, rpcServer.startHandler(rpcServer.handleMessages))

			tt.fill(rpcServer)
			payload, err := proto.Marshal(tt.message)
			require.NoError(t, err)
			require.NoError(t, publisher.Publish(tt.topic, payload))
			require.NoError(t, publisher.Flush())
			require.Eventually(t, func() bool {
				pending, _, pendingErr := sub.Pending()
				return pendingErr == nil && pending == 1
			}, time.Second, time.Millisecond, "NATS callback did not block on the saturated buffer")

			shutdownDone := make(chan error, 1)
			go func() {
				shutdownDone <- rpcServer.Shutdown()
			}()
			select {
			case err := <-shutdownDone:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("Shutdown timed out with a saturated user callback")
			}
			require.NotErrorIs(t, rpcServer.conn.LastError(), nats.ErrDrainTimeout)
		})
	}
}

func TestValidateNatsRPCRequestRejectsInvalidStructure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		req   *protos.Request
		reply string
	}{
		{
			name:  "missing msg",
			req:   &protos.Request{Type: protos.RPCType_User},
			reply: "reply",
		},
		{
			name:  "unknown rpc type",
			req:   &protos.Request{Type: protos.RPCType(99), Msg: &protos.Msg{Route: "svc.method"}},
			reply: "reply",
		},
		{
			name:  "non inbound message type",
			req:   &protos.Request{Msg: &protos.Msg{Route: "svc.method", Type: protos.MsgType_MsgResponse}},
			reply: "reply",
		},
		{
			name:  "empty route",
			req:   &protos.Request{Msg: &protos.Msg{}},
			reply: "reply",
		},
		{
			name: "negative session id",
			req: &protos.Request{
				Session: &protos.Session{Id: -1},
				Msg:     &protos.Msg{Route: "svc.method"},
			},
			reply: "reply",
		},
		{
			name:  "missing request reply",
			req:   &protos.Request{Msg: &protos.Msg{Route: "svc.method"}},
			reply: "",
		},
		{
			name:  "wildcard request reply",
			req:   &protos.Request{Msg: &protos.Msg{Route: "svc.method"}},
			reply: "reply.*",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload, err := proto.Marshal(tt.req)
			require.NoError(t, err)
			_, err = validateNatsRPCRequest(&nats.Msg{Data: payload, Reply: tt.reply})
			assert.Error(t, err)
		})
	}
}

func TestValidateNatsRPCRequestAcceptsRouteLongerThanClientWireLimit(t *testing.T) {
	routeName := strings.Repeat("s", 1<<8) + ".method"
	payload, err := proto.Marshal(&protos.Request{
		Type: protos.RPCType_User,
		Msg: &protos.Msg{
			Type:  protos.MsgType_MsgRequest,
			Route: routeName,
		},
	})
	require.NoError(t, err)

	req, err := validateNatsRPCRequest(&nats.Msg{Data: payload, Reply: "reply"})
	require.NoError(t, err)
	assert.Equal(t, routeName, req.GetMsg().GetRoute())
}

func TestNatsRPCServerInitShouldFailIfConnFails(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	cfg.Connect = "nats://localhost:1"
	sv := getServer()

	ctrl := gomock.NewController(t)
	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	rpcServer, _ := NewNatsRPCServer(cfg, sv, nil, nil, mockSessionPool)
	//mockSessionPool.EXPECT().OnSessionBind(rpcServer.onSessionBind)
	err := rpcServer.Init()
	assert.Error(t, err)
}

func TestNatsRPCServerTracksBrokerDisconnectRecoveryAndResumesPump(t *testing.T) {
	port := helpers.GetFreePort(t)
	currentServer := runTestNATSServerAtPort(t, port)
	defer func() {
		currentServer.Shutdown()
		currentServer.WaitForShutdown()
	}()

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	cfg.Connect = fmt.Sprintf("nats://127.0.0.1:%d", port)
	cfg.MaxReconnectionRetries = 100
	dieChan := make(chan bool, 1)
	ctrl := gomock.NewController(t)
	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, dieChan, mockSessionPool)
	require.NoError(t, err)
	mockSessionPool.EXPECT().OnSessionBind(newFuncPtrMatcher(rpcServer.onSessionBind))

	called := make(chan struct{}, 1)
	rpcServer.SetPitayaServer(&pitayaServerStub{call: func(context.Context, *protos.Request) (*protos.Response, error) {
		called <- struct{}{}
		return &protos.Response{}, nil
	}})
	require.NoError(t, rpcServer.Init())
	defer func() { require.NoError(t, rpcServer.Shutdown()) }()
	require.True(t, rpcServer.IsRunning())

	currentServer.Shutdown()
	currentServer.WaitForShutdown()
	require.Eventually(t, func() bool {
		return !rpcServer.IsRunning()
	}, 3*time.Second, time.Millisecond, "server did not leave readiness after broker disconnect")

	currentServer = runTestNATSServerAtPort(t, port)
	require.Eventually(t, rpcServer.IsRunning, 5*time.Second, 5*time.Millisecond, "server did not recover readiness after broker restart")

	payload, err := proto.Marshal(&protos.Request{
		Type:     protos.RPCType_User,
		Metadata: []byte("{}"),
		Msg: &protos.Msg{
			Type:  protos.MsgType_MsgNotify,
			Route: "service.method",
		},
	})
	require.NoError(t, err)
	require.NoError(t, rpcServer.conn.Publish(getChannel(rpcServer.server.Type, rpcServer.server.ID), payload))
	require.NoError(t, rpcServer.conn.Flush())
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("message pump did not resume after broker recovery")
	}
	select {
	case <-dieChan:
		t.Fatal("recoverable broker disconnect triggered the terminal supervisor")
	default:
	}
}

func TestNatsRPCServerInitPropagatesHandlerSubmissionError(t *testing.T) {
	if os.Getenv(statelessSubmissionErrorHelperEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNatsRPCServerInitPropagatesHandlerSubmissionError$")
		cmd.Env = append(os.Environ(), statelessSubmissionErrorHelperEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("nats init submission helper failed: %v\n%s", err, output)
		}
		return
	}

	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	ctrl := gomock.NewController(t)
	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, mockSessionPool)
	require.NoError(t, err)

	ants.Release()
	err = rpcServer.Init()
	require.ErrorIs(t, err, ants.ErrPoolClosed)
	require.NotNil(t, rpcServer.conn)
	assert.True(t, rpcServer.conn.IsClosed())
}

func TestNatsRPCServerShutdownIsIdempotent(t *testing.T) {
	rpcServer := &NatsRPCServer{
		stopChan:   make(chan bool),
		userPushCh: make(chan *protos.Push),
		userKickCh: make(chan *protos.KickMsg),
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- rpcServer.Shutdown()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	_, pushOpen := <-rpcServer.userPushCh
	_, kickOpen := <-rpcServer.userKickCh
	assert.False(t, pushOpen, "Shutdown left the push delivery channel open")
	assert.False(t, kickOpen, "Shutdown left the kick delivery channel open")
}

func TestNatsRPCServerGracefulShutdownCancelsHandlerLifecycleAfterWait(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)
	rpcServer.handlerMu.Lock()
	rpcServer.consumersStarted = true
	rpcServer.handlerMu.Unlock()

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	require.NoError(t, rpcServer.startHandler(func() {
		close(handlerStarted)
		<-releaseHandler
	}))
	<-handlerStarted

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- rpcServer.Shutdown() }()
	select {
	case <-rpcServer.runtimeContext().Done():
		t.Fatal("graceful Shutdown canceled the lifecycle while an accepted handler was running")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseHandler)
	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("graceful Shutdown did not return after the accepted handler exited")
	}
	select {
	case <-rpcServer.runtimeContext().Done():
	default:
		t.Fatal("graceful Shutdown did not cancel the lifecycle after handlers exited")
	}
}

func TestNatsRPCServerGracefulShutdownPreservesAcceptedDeliveryContext(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)
	rpcServer.handlerMu.Lock()
	rpcServer.consumersStarted = true
	rpcServer.handlerMu.Unlock()

	deliveryStarted := make(chan struct{})
	releaseDelivery := make(chan struct{})
	deliveryResult := make(chan error, 1)
	rpcServer.SetPitayaServer(&pitayaServerStub{
		pushToUser: func(ctx context.Context, _ *protos.Push) (*protos.Response, error) {
			close(deliveryStarted)
			select {
			case <-ctx.Done():
				deliveryResult <- ctx.Err()
			case <-releaseDelivery:
				deliveryResult <- ctx.Err()
			}
			return &protos.Response{}, nil
		},
	})
	require.NoError(t, rpcServer.startHandler(rpcServer.processPushes))
	rpcServer.userPushCh <- &protos.Push{Uid: "accepted-user"}
	<-deliveryStarted

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- rpcServer.Shutdown() }()
	select {
	case err := <-deliveryResult:
		t.Fatalf("graceful Shutdown canceled an accepted delivery before it completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseDelivery)
	require.NoError(t, <-deliveryResult)
	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("graceful Shutdown did not return after the accepted delivery completed")
	}
}

func TestNatsRPCServerSupervisesUnexpectedConsumerExit(t *testing.T) {
	tests := []struct {
		name     string
		consumer func()
	}{
		{name: "panic", consumer: func() { panic("boom") }},
		{name: "return", consumer: func() {}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
			appDie := make(chan bool, 1)
			rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, appDie, nil)
			require.NoError(t, err)
			rpcServer.running.Store(true)
			t.Cleanup(func() { require.NoError(t, rpcServer.Shutdown()) })

			require.NoError(t, rpcServer.startConsumer("test", test.consumer))
			select {
			case <-appDie:
			case <-time.After(time.Second):
				t.Fatal("unexpected consumer exit did not signal app shutdown")
			}
			assert.False(t, rpcServer.IsRunning())
		})
	}
}

func TestNatsRPCServerShutdownWaitsForOwnedHandlers(t *testing.T) {
	rpcServer := &NatsRPCServer{stopChan: make(chan bool)}
	started := make(chan struct{})
	release := make(chan struct{})
	require.NoError(t, rpcServer.startHandler(func() {
		close(started)
		<-release
	}))
	<-started

	shutdownResult := make(chan error, 1)
	go func() {
		shutdownResult <- rpcServer.Shutdown()
	}()
	select {
	case <-rpcServer.stopChan:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not publish the stop signal")
	}
	select {
	case <-shutdownResult:
		t.Fatal("Shutdown returned while an owned handler was running")
	default:
	}

	close(release)
	select {
	case err := <-shutdownResult:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after the owned handler exited")
	}
}

func TestNatsRPCServerBeforeShutdownWaitsForTerminalCallback(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseCallback)
		}
	}()
	conn, terminalCallbackDone, err := setupTrackedNatsConn(
		fmt.Sprintf("nats://%s", s.Addr()),
		nil,
		func() {
			close(callbackStarted)
			<-releaseCallback
		},
	)
	require.NoError(t, err)
	defer conn.Close()

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)
	rpcServer.conn = conn
	rpcServer.terminalCallbackDone = terminalCallbackDone
	require.NoError(t, rpcServer.startHandler(rpcServer.handleMessages))

	beforeDone := make(chan struct{})
	go func() {
		rpcServer.BeforeShutdown()
		close(beforeDone)
	}()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("BeforeShutdown did not start the terminal NATS callback")
	}
	select {
	case <-beforeDone:
		t.Fatal("BeforeShutdown returned while the terminal NATS callback was running")
	default:
	}

	close(releaseCallback)
	released = true
	select {
	case <-beforeDone:
	case <-time.After(time.Second):
		t.Fatal("BeforeShutdown did not return after the terminal NATS callback exited")
	}
	assert.True(t, conn.IsClosed())
}

func TestNatsRPCServerBeforeShutdownStopsAndWaitsForOwnedHandlers(t *testing.T) {
	rpcServer := &NatsRPCServer{
		stopChan:   make(chan bool),
		userPushCh: make(chan *protos.Push),
		userKickCh: make(chan *protos.KickMsg),
	}
	require.NoError(t, rpcServer.startHandler(rpcServer.processPushes))
	require.NoError(t, rpcServer.startHandler(rpcServer.processKick))

	started := make(chan struct{})
	release := make(chan struct{})
	require.NoError(t, rpcServer.startHandler(func() {
		close(started)
		<-release
	}))
	<-started

	beforeDone := make(chan struct{})
	go func() {
		rpcServer.BeforeShutdown()
		close(beforeDone)
	}()
	select {
	case <-rpcServer.stopChan:
	case <-time.After(time.Second):
		t.Fatal("BeforeShutdown did not publish the stop signal")
	}
	select {
	case <-beforeDone:
		t.Fatal("BeforeShutdown returned while an owned handler was running")
	default:
	}

	close(release)
	select {
	case <-beforeDone:
	case <-time.After(time.Second):
		t.Fatal("BeforeShutdown did not return after owned handlers exited")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- rpcServer.Shutdown()
	}()
	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Shutdown was not idempotent after BeforeShutdown")
	}
}

func TestNatsRPCServerBeforeShutdownWaitsForAcceptedRequestTasks(t *testing.T) {
	rpcServer := &NatsRPCServer{stopChan: make(chan bool)}
	started := make(chan struct{})
	release := make(chan struct{})
	require.NoError(t, rpcServer.submitRequestTask(context.Background(), nil, func(context.Context) {
		close(started)
		<-release
	}))
	<-started

	beforeDone := make(chan struct{})
	go func() {
		rpcServer.BeforeShutdown()
		close(beforeDone)
	}()
	select {
	case <-rpcServer.stopChan:
	case <-time.After(time.Second):
		t.Fatal("BeforeShutdown did not publish the stop signal")
	}
	select {
	case <-beforeDone:
		t.Fatal("BeforeShutdown returned while an accepted request task was running")
	default:
	}
	require.ErrorIs(t, rpcServer.submitRequestTask(context.Background(), nil, func(context.Context) {}), errNatsRPCServerStopping)

	close(release)
	select {
	case <-beforeDone:
	case <-time.After(time.Second):
		t.Fatal("BeforeShutdown did not return after the accepted request task exited")
	}
}

func TestNatsRPCServerShutdownKeepsConnectionOpenUntilAcceptedRequestTasksFinish(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	require.NoError(t, err)
	defer conn.Close()

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)
	rpcServer.conn = conn

	handlerExited := make(chan struct{})
	require.NoError(t, rpcServer.startHandler(func() {
		rpcServer.handleMessages()
		close(handlerExited)
	}))

	taskStarted := make(chan struct{})
	releaseTask := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseTask) }) }
	defer release()
	publishResult := make(chan error, 1)
	require.NoError(t, rpcServer.submitRequestTask(context.Background(), nil, func(context.Context) {
		close(taskStarted)
		<-releaseTask
		publishResult <- conn.Publish("shutdown.accepted.response", []byte("ok"))
	}))
	<-taskStarted

	shutdownResult := make(chan error, 1)
	go func() {
		shutdownResult <- rpcServer.Shutdown()
	}()
	select {
	case <-handlerExited:
	case <-time.After(time.Second):
		t.Fatal("message handler did not exit after shutdown started")
	}
	assert.False(t, conn.IsClosed(), "accepted request tasks must finish before the connection drains")

	release()
	require.NoError(t, <-publishResult)
	select {
	case err := <-shutdownResult:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after the accepted request task finished")
	}
	assert.True(t, conn.IsClosed())
}

func TestNatsRPCServerBeforeShutdownCancelsAcceptedRequestTaskContext(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	rpcServer, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
	require.NoError(t, err)
	started := make(chan struct{})
	canceled := make(chan struct{})
	require.NoError(t, rpcServer.submitRequestTask(context.Background(), nil, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(canceled)
	}))
	<-started

	beforeDone := make(chan struct{})
	go func() {
		rpcServer.BeforeShutdown()
		close(beforeDone)
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("BeforeShutdown did not cancel the accepted request task context")
	}
	select {
	case <-beforeDone:
	case <-time.After(time.Second):
		t.Fatal("BeforeShutdown did not return after the canceled request task exited")
	}
}

func TestNatsRPCServerInit(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	ctrl := gomock.NewController(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	sv := getServer()

	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	rpcServer, _ := NewNatsRPCServer(cfg, sv, nil, nil, mockSessionPool)
	mockSessionPool.EXPECT().OnSessionBind(newFuncPtrMatcher(rpcServer.onSessionBind))
	err := rpcServer.Init()
	assert.NoError(t, err)
	// should setup conn
	assert.NotNil(t, rpcServer.conn)
	assert.True(t, rpcServer.conn.IsConnected())
	// should subscribe
	assert.True(t, rpcServer.sub.IsValid())
	//should handle messages
	tables := []struct {
		name  string
		topic string
		req   *protos.Request
	}{
		{"test1", getChannel(sv.Type, sv.ID), &protos.Request{Type: protos.RPCType_Sys, FrontendID: "bla", Msg: &protos.Msg{Id: 1, Route: "svc.method", Reply: "ae"}}},
		{"test2", getChannel(sv.Type, sv.ID), &protos.Request{Type: protos.RPCType_User, FrontendID: "bla2", Msg: &protos.Msg{Id: 1, Route: "svc.method", Reply: "boa"}}},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			c := make(chan *nats.Msg, 1)
			_, err := rpcServer.conn.ChanSubscribe(table.req.Msg.Reply, c)
			require.NoError(t, err)
			require.NoError(t, rpcServer.conn.Flush())
			rpcServer.unhandledReqCh <- table.req
			r := helpers.ShouldEventuallyReceive(t, c).(*nats.Msg)
			assert.NotNil(t, r.Data)
		})
	}
}

func TestNatsRPCServerProcessBindings(t *testing.T) {
	ctrl := gomock.NewController(t)
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	sv := getServer()
	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	rpcServer, _ := NewNatsRPCServer(cfg, sv, nil, nil, mockSessionPool)
	mockSessionPool.EXPECT().OnSessionBind(newFuncPtrMatcher(rpcServer.onSessionBind))
	err := rpcServer.Init()

	assert.NoError(t, err)

	defer ctrl.Finish()

	bindMsg := &protos.BindMsg{
		Uid: "testuid",
		Fid: "testfid",
	}

	bindData, err := proto.Marshal(bindMsg)
	assert.NoError(t, err)

	msg := &nats.Msg{
		Data: bindData,
	}

	unmarshalledMsg := &protos.BindMsg{}
	_ = proto.Unmarshal(bindData, unmarshalledMsg)
	processed := make(chan *protos.BindMsg, 1)

	rpcServer.SetPitayaServer(&pitayaServerStub{
		sessionBindRemote: func(ctx context.Context, b *protos.BindMsg) (*protos.Response, error) {
			processed <- b
			return &protos.Response{}, nil
		},
	})

	rpcServer.bindingsChan <- msg
	got := helpers.ShouldEventuallyReceive(t, processed).(*protos.BindMsg)
	assert.Equal(t, bindMsg.Uid, got.Uid)
	assert.Equal(t, bindMsg.Fid, got.Fid)
}

func TestNatsRPCServerProcessPushes(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	ctrl := gomock.NewController(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	sv := getServer()
	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	rpcServer, _ := NewNatsRPCServer(cfg, sv, nil, nil, mockSessionPool)
	mockSessionPool.EXPECT().OnSessionBind(newFuncPtrMatcher(rpcServer.onSessionBind))
	err := rpcServer.Init()

	assert.NoError(t, err)

	defer ctrl.Finish()

	push := &protos.Push{
		Route: "someroute",
		Uid:   "someuid",
		Data:  []byte{0x01},
	}
	processed := make(chan *protos.Push, 1)

	rpcServer.SetPitayaServer(&pitayaServerStub{
		pushToUser: func(ctx context.Context, p *protos.Push) (*protos.Response, error) {
			processed <- p
			return &protos.Response{}, nil
		},
	})

	rpcServer.userPushCh <- push
	got := helpers.ShouldEventuallyReceive(t, processed).(*protos.Push)
	assert.Equal(t, push.Route, got.Route)
	assert.Equal(t, push.Uid, got.Uid)
	assert.Equal(t, push.Data, got.Data)
}

func TestNatsRPCServerProcessKick(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	ctrl := gomock.NewController(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	sv := getServer()
	mockSessionPool := sessionmocks.NewMockSessionPool(ctrl)
	rpcServer, _ := NewNatsRPCServer(cfg, sv, nil, nil, mockSessionPool)
	mockSessionPool.EXPECT().OnSessionBind(newFuncPtrMatcher(rpcServer.onSessionBind))
	err := rpcServer.Init()

	assert.NoError(t, err)

	defer ctrl.Finish()

	kick := &protos.KickMsg{
		UserId: "someuid",
	}
	processed := make(chan *protos.KickMsg, 1)

	rpcServer.SetPitayaServer(&pitayaServerStub{
		kickUser: func(ctx context.Context, p *protos.KickMsg) (*protos.KickAnswer, error) {
			processed <- p
			return &protos.KickAnswer{}, nil
		},
	})

	rpcServer.userKickCh <- kick
	got := helpers.ShouldEventuallyReceive(t, processed).(*protos.KickMsg)
	assert.Equal(t, kick.UserId, got.UserId)
}

func TestNatsRPCServerReportMetrics(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
	sv := getServer()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}

	rpcServer, _ := NewNatsRPCServer(cfg, sv, mockMetricsReporters, nil, nil)
	rpcServer.dropped = 100
	rpcServer.messagesBufferSize = 100
	rpcServer.pushBufferSize = 100

	rpcServer.subChan <- &nats.Msg{}
	rpcServer.bindingsChan <- &nats.Msg{}
	rpcServer.userPushCh <- &protos.Push{}

	mockMetricsReporter.EXPECT().ReportGauge(metrics.DroppedMessages, gomock.Any(), float64(rpcServer.dropped))
	mockMetricsReporter.EXPECT().ReportGauge(metrics.ChannelCapacity, gomock.Any(), float64(99)).Times(3)
	rpcServer.reportMetrics()
}
