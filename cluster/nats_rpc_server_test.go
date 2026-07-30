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
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/helpers"
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
	sessionBindRemote func(context.Context, *protos.BindMsg) (*protos.Response, error)
	pushToUser        func(context.Context, *protos.Push) (*protos.Response, error)
	kickUser          func(context.Context, *protos.KickMsg) (*protos.KickAnswer, error)
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
		natsConnect        string
		messagesBufferSize int
		pushBufferSize     int
		err                error
	}{
		{"nats://localhost:2333", 10, 10, nil},
		{"nats://localhost:2333", 10, 0, constants.ErrNatsPushBufferSizeZero},
		{"nats://localhost:2333", 0, 10, constants.ErrNatsMessagesBufferSizeZero},
		{"", 10, 10, constants.ErrNoNatsConnectionString},
	}

	for _, table := range tables {
		t.Run(fmt.Sprintf("%s-%d-%d", table.natsConnect, table.messagesBufferSize, table.pushBufferSize), func(t *testing.T) {
			cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Nats
			cfg.Connect = table.natsConnect
			cfg.Buffer.Messages = table.messagesBufferSize
			cfg.Buffer.Push = table.pushBufferSize
			_, err := NewNatsRPCServer(cfg, getServer(), nil, nil, nil)
			assert.Equal(t, table.err, err)
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
		{"user1/messages", &protos.Request{Type: protos.RPCType_Sys, FrontendID: "bla", Msg: &protos.Msg{Id: 1, Reply: "ae"}}},
		{"user2/messages", &protos.Request{Type: protos.RPCType_User, FrontendID: "bla2", Msg: &protos.Msg{Id: 1}}},
	}

	go rpcServer.handleMessages()

	for _, table := range tables {
		t.Run(table.topic, func(t *testing.T) {
			subs, err := rpcServer.subscribe(table.topic, false)
			assert.NoError(t, err)
			assert.Equal(t, true, subs.IsValid())
			b, err := proto.Marshal(table.req)
			assert.NoError(t, err)

			mockMetricsReporter.EXPECT().ReportGauge(metrics.DroppedMessages, gomock.Any(), float64(0))
			mockMetricsReporter.EXPECT().ReportGauge(metrics.ChannelCapacity, gomock.Any(), gomock.Any()).Times(3)

			conn.Publish(table.topic, b)
			r := helpers.ShouldEventuallyReceive(t, rpcServer.unhandledReqCh).(*protos.Request)
			assert.Equal(t, table.req.FrontendID, r.FrontendID)
			assert.Equal(t, table.req.Msg.Id, r.Msg.Id)
		})
	}
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
	rpcServer := &NatsRPCServer{stopChan: make(chan bool)}
	require.NoError(t, rpcServer.Shutdown())
	require.NoError(t, rpcServer.Shutdown())
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
		{"test1", getChannel(sv.Type, sv.ID), &protos.Request{Type: protos.RPCType_Sys, FrontendID: "bla", Msg: &protos.Msg{Id: 1, Reply: "ae"}}},
		{"test2", getChannel(sv.Type, sv.ID), &protos.Request{Type: protos.RPCType_User, FrontendID: "bla2", Msg: &protos.Msg{Id: 1, Reply: "boa"}}},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			c := make(chan *nats.Msg)
			rpcServer.conn.ChanSubscribe(table.req.Msg.Reply, c)
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

	rpcServer.SetPitayaServer(&pitayaServerStub{
		sessionBindRemote: func(ctx context.Context, b *protos.BindMsg) (*protos.Response, error) {
			assert.Equal(t, bindMsg.Uid, b.Uid)
			assert.Equal(t, bindMsg.Fid, b.Fid)
			return &protos.Response{}, nil
		},
	})

	rpcServer.bindingsChan <- msg
	time.Sleep(30 * time.Millisecond)
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

	rpcServer.SetPitayaServer(&pitayaServerStub{
		pushToUser: func(ctx context.Context, p *protos.Push) (*protos.Response, error) {
			assert.Equal(t, push.Route, p.Route)
			assert.Equal(t, push.Uid, p.Uid)
			assert.Equal(t, push.Data, p.Data)
			return &protos.Response{}, nil
		},
	})

	rpcServer.userPushCh <- push
	time.Sleep(30 * time.Millisecond)
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

	rpcServer.SetPitayaServer(&pitayaServerStub{
		kickUser: func(ctx context.Context, p *protos.KickMsg) (*protos.KickAnswer, error) {
			assert.Equal(t, kick.UserId, p.UserId)
			return &protos.KickAnswer{}, nil
		},
	})

	rpcServer.userKickCh <- kick
	time.Sleep(30 * time.Millisecond)
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
