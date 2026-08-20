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
	"testing"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/metrics"
	metricsmocks "github.com/alkaid/pitaya/v2/metrics/mocks"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	sessionmocks "github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/proto"
)

func TestNewNatsRPCClient(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	sv := getServer()
	n, err := NewNatsRPCClient(cfg, sv, mockMetricsReporters, nil)
	assert.NoError(t, err)
	assert.NotNil(t, n)
	assert.Equal(t, sv, n.server)
	assert.Equal(t, mockMetricsReporters, n.metricsReporters)
	assert.False(t, n.running.Load())
}

func TestNatsRPCClientConfigure(t *testing.T) {
	t.Parallel()
	tables := []struct {
		name    string
		mutate  func(*config.NatsRPCClientConfig)
		want    string
		wantErr error
	}{
		{name: "valid"},
		{
			name:    "empty connect",
			mutate:  func(cfg *config.NatsRPCClientConfig) { cfg.Connect = " \t" },
			want:    "pitaya.cluster.rpc.client.nats.connect",
			wantErr: constants.ErrNoNatsConnectionString,
		},
		{
			name:   "connection timeout",
			mutate: func(cfg *config.NatsRPCClientConfig) { cfg.ConnectionTimeout = 0 },
			want:   "pitaya.cluster.rpc.client.nats.connectiontimeout",
		},
		{
			name:    "request timeout",
			mutate:  func(cfg *config.NatsRPCClientConfig) { cfg.RequestTimeout = -time.Second },
			want:    "pitaya.cluster.rpc.client.nats.requesttimeout",
			wantErr: constants.ErrNatsNoRequestTimeout,
		},
		{
			name:   "reconnect retries",
			mutate: func(cfg *config.NatsRPCClientConfig) { cfg.MaxReconnectionRetries = -2 },
			want:   "pitaya.cluster.rpc.client.nats.maxreconnectionretries",
		},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
			if table.mutate != nil {
				table.mutate(&cfg)
			}
			_, err := NewNatsRPCClient(cfg, getServer(), nil, nil)
			if table.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), table.want)
			if table.wantErr != nil {
				assert.ErrorIs(t, err, table.wantErr)
			}
		})
	}
}

func TestNatsRPCClientGetSubscribeChannel(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	sv := getServer()
	n, _ := NewNatsRPCClient(cfg, sv, nil, nil)
	assert.Equal(t, fmt.Sprintf("pitaya/servers/%s/%s", n.server.Type, n.server.ID), n.getSubscribeChannel())
}

func TestNatsRPCClientStop(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	sv := getServer()
	n, _ := NewNatsRPCClient(cfg, sv, nil, nil)
	// change it to true to ensure it goes to false
	n.running.Store(true)
	n.stop()
	assert.False(t, n.running.Load())
}

func TestNatsRPCClientInitShouldFailIfConnFails(t *testing.T) {
	t.Parallel()
	sv := getServer()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = "nats://localhost:1"
	rpcClient, _ := NewNatsRPCClient(cfg, sv, nil, nil)
	err := rpcClient.Init()
	assert.Error(t, err)
}

func TestNatsRPCClientInit(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	sv := getServer()

	rpcClient, _ := NewNatsRPCClient(cfg, sv, nil, nil)
	err := rpcClient.Init()
	assert.NoError(t, err)
	assert.True(t, rpcClient.running.Load())

	// should setup conn
	assert.NotNil(t, rpcClient.conn)
	assert.True(t, rpcClient.conn.IsConnected())
}

func TestNatsRPCClientTracksBrokerDisconnectAndRecovery(t *testing.T) {
	port := helpers.GetFreePort(t)
	currentServer := runTestNATSServerAtPort(t, port)
	defer func() {
		currentServer.Shutdown()
		currentServer.WaitForShutdown()
	}()

	dieChan := make(chan bool, 1)
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = fmt.Sprintf("nats://127.0.0.1:%d", port)
	cfg.MaxReconnectionRetries = 100
	rpcClient, err := NewNatsRPCClient(cfg, getServer(), nil, dieChan)
	require.NoError(t, err)
	require.NoError(t, rpcClient.Init())
	defer func() { require.NoError(t, rpcClient.Shutdown()) }()
	require.True(t, rpcClient.IsRunning())

	currentServer.Shutdown()
	currentServer.WaitForShutdown()
	require.Eventually(t, func() bool {
		return !rpcClient.IsRunning()
	}, 3*time.Second, time.Millisecond, "client did not leave readiness after broker disconnect")

	currentServer = runTestNATSServerAtPort(t, port)
	require.Eventually(t, rpcClient.IsRunning, 5*time.Second, 5*time.Millisecond, "client did not recover readiness after broker restart")
	require.NoError(t, rpcClient.Send("recovery.client", []byte("ok")))
	select {
	case <-dieChan:
		t.Fatal("recoverable broker disconnect triggered the terminal supervisor")
	default:
	}
}

func TestNatsRPCClientShutdownCancelsAndWaitsForInFlightCall(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	cfg.RequestTimeout = time.Minute
	rpcClient, err := NewNatsRPCClient(cfg, getServer(), nil, nil)
	require.NoError(t, err)
	require.NoError(t, rpcClient.Init())

	target := &Server{ID: "target", Type: "room"}
	requestReceived := make(chan *nats.Msg, 1)
	sub, err := rpcClient.conn.ChanSubscribe(getChannel(target.Type, target.ID), requestReceived)
	require.NoError(t, err)
	require.NoError(t, rpcClient.conn.Flush())

	callResult := make(chan error, 1)
	go func() {
		_, callErr := rpcClient.Call(
			context.Background(),
			protos.RPCType_User,
			route.NewRoute(target.Type, "service", "method"),
			nil,
			&message.Message{Type: message.Request, Route: "service.method"},
			target,
		)
		callResult <- callErr
	}()
	receiveFanout := func() {
		select {
		case <-requestReceived:
		case <-time.After(3 * time.Second):
			t.Fatal("in-flight call did not reach NATS")
		}
	}
	receiveFanout()
	require.NoError(t, sub.Unsubscribe())

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- rpcClient.Shutdown() }()
	select {
	case callErr := <-callResult:
		require.ErrorIs(t, callErr, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not cancel the in-flight call")
	}
	select {
	case shutdownErr := <-shutdownDone:
		require.NoError(t, shutdownErr)
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not wait for the canceled call")
	}
	assert.False(t, rpcClient.IsRunning())
	assert.True(t, rpcClient.conn.IsClosed())
	require.ErrorIs(t, rpcClient.Send("after.shutdown", nil), constants.ErrRPCClientNotInitialized)
	forkMessage := &message.Message{Type: message.Request, ID: 10}
	require.ErrorIs(
		t,
		rpcClient.Fork(context.Background(), route.NewRoute("room", "svc", "method"), nil, forkMessage),
		constants.ErrRPCClientNotInitialized,
	)
	assert.Equal(t, message.Request, forkMessage.Type)
}

func TestNatsRPCClientShutdownWaitsForTerminalCallback(t *testing.T) {
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

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	rpcClient, err := NewNatsRPCClient(cfg, getServer(), nil, nil)
	require.NoError(t, err)
	rpcClient.conn = conn
	rpcClient.terminalCallbackDone = terminalCallbackDone
	rpcClient.running.Store(true)

	rpcClient.BeforeShutdown()
	assert.True(t, rpcClient.running.Load())
	assert.False(t, conn.IsClosed())

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- rpcClient.Shutdown()
	}()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not start the terminal NATS callback")
	}
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned while the terminal NATS callback was running")
	default:
	}

	close(releaseCallback)
	released = true
	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after the terminal NATS callback exited")
	}
	assert.True(t, conn.IsClosed())
	require.NoError(t, rpcClient.Shutdown())
}

func TestNatsRPCClientBroadcastSessionBind(t *testing.T) {
	uid := "testuid123"
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	sv := getServer()

	rpcClient, _ := NewNatsRPCClient(cfg, sv, nil, nil)
	rpcClient.Init()

	subChan := make(chan *nats.Msg, 1)
	subs, err := rpcClient.conn.ChanSubscribe(GetBindBroadcastTopic(sv.Type), subChan)
	require.NoError(t, err)
	require.NoError(t, rpcClient.conn.Flush())

	err = rpcClient.BroadcastSessionBind(uid)
	assert.NoError(t, err)

	m := helpers.ShouldEventuallyReceive(t, subChan).(*nats.Msg)

	bMsg := &protos.BindMsg{}
	err = proto.Unmarshal(m.Data, bMsg)
	assert.NoError(t, err)

	assert.Equal(t, uid, bMsg.Uid)
	assert.Equal(t, sv.ID, bMsg.Fid)

	subs.Unsubscribe()
}

func TestNatsRPCClientSendKick(t *testing.T) {
	uid := "testuid"
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	sv := getServer()

	rpcClient, _ := NewNatsRPCClient(cfg, sv, nil, nil)
	err := rpcClient.Init()
	assert.NoError(t, err)

	kickChan := make(chan *nats.Msg, 1)
	subs, err := rpcClient.conn.ChanSubscribe(GetUserKickTopic(uid, sv.Type), kickChan)
	require.NoError(t, err)
	require.NoError(t, rpcClient.conn.Flush())

	kick := &protos.KickMsg{
		UserId: uid,
	}

	err = rpcClient.SendKick(uid, sv.Type, kick)
	assert.NoError(t, err)

	m := helpers.ShouldEventuallyReceive(t, kickChan).(*nats.Msg)

	actual := &protos.KickMsg{}
	err = proto.Unmarshal(m.Data, actual)
	assert.NoError(t, err)

	assert.Equal(t, kick.UserId, actual.UserId)
	err = subs.Unsubscribe()
	assert.NoError(t, err)
}

func TestNatsRPCClientSendPush(t *testing.T) {
	uid := "testuid123"
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	sv := getServer()

	rpcClient, _ := NewNatsRPCClient(cfg, sv, nil, nil)
	rpcClient.Init()

	subChan := make(chan *nats.Msg, 1)
	subs, err := rpcClient.conn.ChanSubscribe(GetUserMessagesTopic(uid, sv.Type), subChan)
	require.NoError(t, err)
	require.NoError(t, rpcClient.conn.Flush())

	push := &protos.Push{
		Route: "hellow",
		Uid:   uid,
		Data:  []byte{0x01},
	}

	err = rpcClient.SendPush(uid, sv, push)
	assert.NoError(t, err)

	m := helpers.ShouldEventuallyReceive(t, subChan).(*nats.Msg)

	actual := &protos.Push{}
	err = proto.Unmarshal(m.Data, actual)
	assert.NoError(t, err)

	assert.Equal(t, push.Route, actual.Route)
	assert.Equal(t, push.Uid, actual.Uid)
	assert.Equal(t, push.Data, actual.Data)

	subs.Unsubscribe()

}

func TestNatsRPCClientSendShouldFailIfNotRunning(t *testing.T) {
	config := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	sv := getServer()
	rpcClient, _ := NewNatsRPCClient(config, sv, nil, nil)
	err := rpcClient.Send("topic", []byte("data"))
	assert.ErrorIs(t, err, constants.ErrRPCClientNotInitialized)
}

func TestNatsRPCClientSend(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	sv := getServer()

	rpcClient, _ := NewNatsRPCClient(cfg, sv, nil, nil)
	rpcClient.Init()

	tables := []struct {
		name  string
		topic string
		data  []byte
	}{
		{"test1", getChannel(sv.Type, sv.ID), []byte("test1")},
		{"test2", getChannel(sv.Type, sv.ID), []byte("test2")},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			subChan := make(chan *nats.Msg, 1)
			subs, err := rpcClient.conn.ChanSubscribe(table.topic, subChan)
			require.NoError(t, err)
			require.NoError(t, rpcClient.conn.Flush())

			err = rpcClient.Send(table.topic, table.data)
			assert.NoError(t, err)

			r := helpers.ShouldEventuallyReceive(t, subChan).(*nats.Msg)
			assert.Equal(t, table.data, r.Data)
			subs.Unsubscribe()
		})
	}
}

func TestNatsRPCClientPublishAndForkPreserveWireSemantics(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	clientServer := getServer()
	rpcClient, err := NewNatsRPCClient(cfg, clientServer, nil, nil)
	require.NoError(t, err)
	require.NoError(t, rpcClient.Init())
	defer func() { assert.NoError(t, rpcClient.Shutdown()) }()

	rt := route.NewRoute("room", "svc", "publish")
	type observation struct {
		request *protos.Request
		err     error
	}
	observed := make(chan observation, 2)
	responseData, err := proto.Marshal(&protos.Response{Data: []byte("response")})
	require.NoError(t, err)
	sub, err := rpcClient.conn.Subscribe(getChannel(rt.SvType, rt.Method), func(msg *nats.Msg) {
		req := &protos.Request{}
		decodeErr := proto.Unmarshal(msg.Data, req)
		observed <- observation{request: req, err: decodeErr}
		if msg.Reply != "" {
			_ = msg.Respond(responseData)
		}
	})
	require.NoError(t, err)
	require.NoError(t, rpcClient.conn.Flush())

	publishCtx, cancelPublish := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelPublish()
	responses, err := rpcClient.Publish(
		publishCtx,
		protos.RPCType_Sys,
		rt,
		nil,
		&message.Message{Type: message.Request, ID: 42, Data: []byte("request")},
		1,
		time.Second,
	)
	require.NoError(t, err)
	require.Len(t, responses, 1)
	assert.Equal(t, []byte("response"), responses[0].Data)
	requestObservation := helpers.ShouldEventuallyReceive(t, observed).(observation)
	require.NoError(t, requestObservation.err)
	assert.Equal(t, protos.MsgType_MsgRequest, requestObservation.request.Msg.Type)
	assert.Equal(t, uint64(42), requestObservation.request.Msg.Id)
	assert.Equal(t, rt.String(), requestObservation.request.Msg.Route)

	responses, err = rpcClient.Publish(
		context.Background(),
		protos.RPCType_Sys,
		rt,
		nil,
		&message.Message{Type: message.Notify, Data: []byte("notify")},
		0,
	)
	require.NoError(t, err)
	assert.Nil(t, responses)
	notifyObservation := helpers.ShouldEventuallyReceive(t, observed).(observation)
	require.NoError(t, notifyObservation.err)
	assert.Equal(t, protos.MsgType_MsgNotify, notifyObservation.request.Msg.Type)
	assert.Equal(t, []byte("notify"), notifyObservation.request.Msg.Data)

	forkMessages := make(chan *nats.Msg, 1)
	forkSub, err := rpcClient.conn.ChanSubscribe(GetForkTopic(rt.SvType), forkMessages)
	require.NoError(t, err)
	require.NoError(t, rpcClient.conn.Flush())
	forkMessage := &message.Message{Type: message.Request, ID: 7, Data: []byte("fork")}
	require.NoError(t, rpcClient.Fork(nil, rt, nil, forkMessage))
	require.Equal(t, message.Notify, forkMessage.Type)

	wireFork := helpers.ShouldEventuallyReceive(t, forkMessages).(*nats.Msg)
	forkRequest := &protos.Request{}
	require.NoError(t, proto.Unmarshal(wireFork.Data, forkRequest))
	assert.Equal(t, protos.RPCType_User, forkRequest.Type)
	assert.Equal(t, protos.MsgType_MsgNotify, forkRequest.Msg.Type)
	assert.Equal(t, rt.String(), forkRequest.Msg.Route)
	forkCtx, err := pcontext.Decode(forkRequest.Metadata)
	require.NoError(t, err)
	assert.Equal(t, "1", pcontext.GetFromPropagateCtx(forkCtx, constants.IdempotencyFanoutKey))

	clientServer.SetOwnerEpoch(1)
	responses, err = rpcClient.Publish(
		context.Background(),
		protos.RPCType_Sys,
		rt,
		nil,
		&message.Message{Type: message.Notify, Data: []byte("fenced-publish")},
		0,
	)
	require.ErrorIs(t, err, constants.ErrRPCPublishRequiresTargetResolution)
	assert.Nil(t, responses)
	require.NoError(t, rpcClient.conn.Flush())
	select {
	case unexpected := <-observed:
		t.Fatalf("fenced Publish sent an unexpected request: %+v", unexpected)
	case <-time.After(100 * time.Millisecond):
	}

	fencedMessage := &message.Message{Type: message.Request, ID: 8, Data: []byte("fenced-fork")}
	err = rpcClient.Fork(context.Background(), rt, nil, fencedMessage)
	require.ErrorIs(t, err, constants.ErrRPCForkRequiresTargetResolution)
	assert.Equal(t, message.Request, fencedMessage.Type)
	require.NoError(t, rpcClient.conn.Flush())
	select {
	case unexpected := <-forkMessages:
		t.Fatalf("fenced Fork published an unexpected message: %q", unexpected.Data)
	case <-time.After(100 * time.Millisecond):
	}

	clientServer.SetOwnerEpoch(0)
	clientServer.InvalidateOwnerEpoch()
	responses, err = rpcClient.Publish(
		context.Background(),
		protos.RPCType_Sys,
		rt,
		nil,
		&message.Message{Type: message.Notify, Data: []byte("invalid-owner-publish")},
		0,
	)
	require.ErrorIs(t, err, constants.ErrRPCPublishRequiresTargetResolution)
	assert.Nil(t, responses)
	require.NoError(t, rpcClient.conn.Flush())
	select {
	case unexpected := <-observed:
		t.Fatalf("invalid-owner Publish sent an unexpected request: %+v", unexpected)
	case <-time.After(100 * time.Millisecond):
	}

	invalidOwnerMessage := &message.Message{Type: message.Request, ID: 9, Data: []byte("invalid-owner-fork")}
	err = rpcClient.Fork(context.Background(), rt, nil, invalidOwnerMessage)
	require.ErrorIs(t, err, constants.ErrRPCForkRequiresTargetResolution)
	assert.Equal(t, message.Request, invalidOwnerMessage.Type)
	require.NoError(t, rpcClient.conn.Flush())
	select {
	case unexpected := <-forkMessages:
		t.Fatalf("invalid-owner Fork published an unexpected message: %q", unexpected.Data)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, sub.Unsubscribe())
	require.NoError(t, forkSub.Unsubscribe())
}

func TestNatsRPCClientBuildRequest(t *testing.T) {
	config := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	sv := getServer()
	rpcClient, _ := NewNatsRPCClient(config, sv, nil, nil)

	rt := route.NewRoute("sv", "svc", "method")

	data := []byte("data")
	messageID := uint(123)
	sessionID := int64(1)
	uid := "uid"
	frontendID := "frontend-forwarded"
	data2 := []byte("data2")
	tables := []struct {
		name              string
		frontendServer    bool
		sessionFrontendID string
		rpcType           protos.RPCType
		route             *route.Route
		msg               *message.Message
		expected          *protos.Request
	}{
		{
			"test-frontend-request", true, "", protos.RPCType_Sys, rt,
			&message.Message{Type: message.Request, ID: messageID, Data: data},
			&protos.Request{
				Type: protos.RPCType_Sys,
				Msg: &protos.Msg{
					Route: rt.String(),
					Data:  data,
					Type:  protos.MsgType_MsgRequest,
					Id:    uint64(messageID),
				},
				FrontendID: sv.ID,
				Session: &protos.Session{
					Id:   sessionID,
					Uid:  uid,
					Data: data2,
				},
			},
		},
		{
			"test-rpc-sys-request", false, "", protos.RPCType_Sys, rt,
			&message.Message{Type: message.Request, ID: messageID, Data: data},
			&protos.Request{
				Type: protos.RPCType_Sys,
				Msg: &protos.Msg{
					Route: rt.String(),
					Data:  data,
					Type:  protos.MsgType_MsgRequest,
					Id:    uint64(messageID),
				},
				FrontendID: "",
				Session: &protos.Session{
					Id:   sessionID,
					Uid:  uid,
					Data: data2,
				},
			},
		},
		{
			"test-rpc-user-request", false, frontendID, protos.RPCType_User, rt,
			&message.Message{Type: message.Request, ID: messageID, Data: data},
			&protos.Request{
				Type: protos.RPCType_User,
				Session: &protos.Session{
					Id:   sessionID,
					Uid:  uid,
					Data: data2,
				},
				Msg: &protos.Msg{
					Route: rt.String(),
					Data:  data,
					Type:  protos.MsgType_MsgRequest,
				},
				FrontendID: frontendID,
			},
		},
		{
			"test-notify", false, "", protos.RPCType_Sys, rt,
			&message.Message{Type: message.Notify, ID: messageID, Data: data},
			&protos.Request{
				Type: protos.RPCType_Sys,
				Msg: &protos.Msg{
					Route: rt.String(),
					Data:  data,
					Type:  protos.MsgType_MsgNotify,
					Id:    0,
				},
				FrontendID: "",
				Session: &protos.Session{
					Id:   sessionID,
					Uid:  uid,
					Data: data2,
				},
			},
		},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)

			ss := sessionmocks.NewMockSession(ctrl)
			ss.EXPECT().GetFrontendID().Return(table.sessionFrontendID)
			ss.EXPECT().GetFrontendSessionID().Return(sessionID)
			ss.EXPECT().UID().Return(uid)
			ss.EXPECT().GetDataEncoded().Return(data2)
			ss.EXPECT().GetBackends().Return(nil)
			ss.EXPECT().RemoteIPText().Return("")

			rpcClient.server.Frontend = table.frontendServer
			req, err := buildRequest(context.Background(), table.rpcType, table.route.String(), ss, table.msg, rpcClient.server)
			assert.NoError(t, err)
			assert.NotNil(t, req.Metadata)
			req.Metadata = nil
			assert.True(t, proto.Equal(table.expected, req))
		})
	}
}

func TestNatsRPCClientCallShouldFailIfNotRunning(t *testing.T) {
	config := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	sv := getServer()
	rpcClient, _ := NewNatsRPCClient(config, sv, nil, nil)
	res, err := rpcClient.Call(
		context.Background(),
		protos.RPCType_Sys,
		route.NewRoute("sv", "svc", "method"),
		nil,
		&message.Message{},
		sv,
	)
	assert.ErrorIs(t, err, constants.ErrRPCClientNotInitialized)
	assert.Nil(t, res)
}

func TestNatsRPCClientCallOwnsTargetFencingMetadata(t *testing.T) {
	natsServer := helpers.GetTestNatsServer(t)
	defer natsServer.Shutdown()

	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", natsServer.Addr())
	rpcClient, err := NewNatsRPCClient(cfg, getServer(), nil, nil)
	require.NoError(t, err)
	require.NoError(t, rpcClient.Init())
	defer func() { require.NoError(t, rpcClient.Shutdown()) }()

	target := NewServer("room-fenced", "room", false)
	target.SetOwnerEpoch(42)
	requests := make(chan *nats.Msg, 1)
	sub, err := rpcClient.conn.ChanSubscribe(getChannel(target.Type, target.ID), requests)
	require.NoError(t, err)
	require.NoError(t, rpcClient.conn.Flush())
	defer func() { require.NoError(t, sub.Unsubscribe()) }()

	ctx := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.OwnerServerIDKey, "stale-target",
		constants.OwnerEpochKey, "1",
	)
	_, err = rpcClient.Call(
		ctx,
		protos.RPCType_User,
		route.NewRoute(target.Type, "svc", "notify"),
		nil,
		&message.Message{Type: message.Notify},
		target,
	)
	require.NoError(t, err)

	wire := helpers.ShouldEventuallyReceive(t, requests).(*nats.Msg)
	request := &protos.Request{}
	require.NoError(t, proto.Unmarshal(wire.Data, request))
	requestCtx, err := pcontext.Decode(request.Metadata)
	require.NoError(t, err)
	assert.Equal(t, target.ID, pcontext.GetFromPropagateCtx(requestCtx, constants.OwnerServerIDKey))
	assert.Equal(t, "42", pcontext.GetFromPropagateCtx(requestCtx, constants.OwnerEpochKey))

	target.InvalidateOwnerEpoch()
	_, err = rpcClient.Call(
		context.Background(),
		protos.RPCType_User,
		route.NewRoute(target.Type, "svc", "notify"),
		nil,
		&message.Message{Type: message.Notify},
		target,
	)
	require.ErrorIs(t, err, constants.ErrStaleOwnerEpoch)
	require.NoError(t, rpcClient.conn.Flush())
	select {
	case unexpected := <-requests:
		t.Fatalf("invalid target Call published an unexpected message: %q", unexpected.Data)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNatsRPCClientCall(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	sv := getServer()
	defer s.Shutdown()
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Nats
	cfg.Connect = fmt.Sprintf("nats://%s", s.Addr())
	cfg.RequestTimeout = time.Duration(300 * time.Millisecond)
	rpcClient, _ := NewNatsRPCClient(cfg, sv, nil, nil)
	rpcClient.Init()

	rt := route.NewRoute("sv", "svc", "method")

	sessionID := int64(1)
	uid := "uid"
	data2 := []byte("data2")

	msg := &message.Message{
		Type: message.Request,
		ID:   uint(123),
		Data: []byte("data"),
	}

	responseErr := apierrors.FromError(errors.New("nok"))
	tables := []struct {
		name     string
		response interface{}
		expected *protos.Response
		err      error
	}{
		{"test_error", &protos.Response{Data: []byte("nok"), Status: &responseErr.Status}, nil, errors.New("nok")},
		{"test_ok", &protos.Response{Data: []byte("ok")}, &protos.Response{Data: []byte("ok")}, nil},
		{"test_bad_response", []byte("invalid"), nil, errors.New("cannot parse invalid wire-format data")},
		{"test_bad_proto", &protos.Session{Id: 1, Uid: "snap"}, nil, errors.New("cannot parse invalid wire-format data")},
		{"test_no_response", nil, nil, constants.ErrRPCRequestTimeout},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
			assert.NoError(t, err)

			sv2 := getServer()
			sv2.Type = uuid.New().String()
			sv2.ID = uuid.New().String()
			subs, err := conn.Subscribe(getChannel(sv2.Type, sv2.ID), func(m *nats.Msg) {
				if table.response != nil {
					if val, ok := table.response.(*protos.Response); ok {
						b, _ := proto.Marshal(val)
						conn.Publish(m.Reply, b)
					} else if val, ok := table.response.(*protos.Session); ok {
						b, _ := proto.Marshal(val)
						conn.Publish(m.Reply, b)
					} else {
						conn.Publish(m.Reply, table.response.([]byte))
					}
				}
			})
			require.NoError(t, err)
			require.NoError(t, conn.Flush())

			ss := sessionmocks.NewMockSession(ctrl)
			ss.EXPECT().GetFrontendID().Return("frontend-forwarded").Times(1)
			ss.EXPECT().GetFrontendSessionID().Return(sessionID).Times(1)
			ss.EXPECT().UID().Return(uid).Times(1)
			ss.EXPECT().GetDataEncoded().Return(data2).Times(1)
			ss.EXPECT().GetBackends().Return(nil).Times(1)
			ss.EXPECT().RemoteIPText().Return("").Times(1)
			ss.EXPECT().SetRequestInFlight(gomock.Any(), gomock.Any(), gomock.Any()).Times(2)

			res, err := rpcClient.Call(context.Background(), protos.RPCType_Sys, rt, ss, msg, sv2)
			assert.True(t, proto.Equal(table.expected, res))
			if table.err != nil {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), table.err.Error())
			}
			err = subs.Unsubscribe()
			assert.NoError(t, err)
			conn.Close()
		})
	}
}
