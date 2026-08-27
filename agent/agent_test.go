// Copyright (c) nano Author and TFG Co. All Rights Reserved.
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

package agent

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/acceptor"
	codecmocks "github.com/alkaid/pitaya/v2/conn/codec/mocks"
	"github.com/alkaid/pitaya/v2/conn/message"
	messagemocks "github.com/alkaid/pitaya/v2/conn/message/mocks"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/metrics"
	metricsmocks "github.com/alkaid/pitaya/v2/metrics/mocks"
	"github.com/alkaid/pitaya/v2/mocks"
	serializemocks "github.com/alkaid/pitaya/v2/serialize/mocks"
	"github.com/alkaid/pitaya/v2/session"
	sessionmocks "github.com/alkaid/pitaya/v2/session/mocks"
	netutil "github.com/alkaid/pitaya/v2/util/net"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type mockAddr struct{}

func (m *mockAddr) Network() string { return "" }
func (m *mockAddr) String() string  { return "remote-string" }

func newAgentMockConn(ctrl *gomock.Controller) *mocks.MockPlayerConn {
	conn := mocks.NewMockPlayerConn(ctrl)
	conn.EXPECT().RemoteAddr().Return(&net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 4321,
	}).AnyTimes()
	return conn
}

type sessionCloseObservation struct {
	online     bool
	registered bool
}

type agentCacheTTLStub struct {
	session.CacheInterface
	ttl time.Duration
}

func (c *agentCacheTTLStub) CacheTTL() time.Duration { return c.ttl }

func heartbeatAndHandshakeMocks(mockEncoder *codecmocks.MockPacketEncoder) []byte {
	heartbeatPacket := []byte("heartbeat")
	heartbeatAndHandshakeMocksWithResponse(mockEncoder, heartbeatPacket, nil)
	return heartbeatPacket
}

func heartbeatAndHandshakeMocksWithResponse(mockEncoder *codecmocks.MockPacketEncoder, heartbeatResponse []byte, onHeartbeat func([]byte)) {
	// heartbeat and handshake if not set by another test
	mockEncoder.EXPECT().Encode(packet.Type(packet.Handshake), gomock.Not(gomock.Nil())).AnyTimes()
	mockEncoder.EXPECT().Encode(packet.Type(packet.Heartbeat), gomock.Nil()).Return(heartbeatResponse, nil).AnyTimes()
	mockEncoder.EXPECT().Encode(packet.Type(packet.Heartbeat), gomock.Not(gomock.Nil())).DoAndReturn(
		func(_ packet.Type, data []byte) ([]byte, error) {
			if onHeartbeat != nil {
				onHeartbeat(data)
			}
			return heartbeatResponse, nil
		},
	).AnyTimes()
	mockEncoder.EXPECT().Encode(packet.Type(packet.HeartbeatAck), gomock.Nil()).AnyTimes()
}

func getCtxWithRequestKeys() context.Context {
	ctx := pcontext.AddToPropagateCtx(context.Background(), constants.StartTimeKey, time.Now().UnixNano())
	return pcontext.AddToPropagateCtx(ctx, constants.RouteKey, "route")
}

func waitForChannelClose(t *testing.T, ch <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed")
		}
	case <-time.After(timeout):
		t.Fatal("timed out waiting for channel to close")
	}
}

func TestAgentClusterCacheKeepAliveStopsOnAgentDeath(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockSession := sessionmocks.NewMockSession(ctrl)
	mockSession.EXPECT().GetClusterStorage().Return(&agentCacheTTLStub{ttl: time.Hour})
	died := make(chan struct{})
	close(died)
	agent := &agentImpl{
		Session:              mockSession,
		chDie:                died,
		chStopKeepCacheAlive: make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		agent.keepClusterCacheAlive()
		close(done)
	}()
	waitForChannelClose(t, done, time.Second)
}

func TestNewAgent(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName().Return("factory-one").Times(1)

	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	mockDecoder := codecmocks.NewMockPacketDecoder(ctrl)
	dieChan := make(chan bool)
	hbTime := time.Second

	mockConn := newAgentMockConn(ctrl)

	packetData := &agentPacketData{
		heartbeat:              []byte("factory-one-heartbeat"),
		heartbeatAck:           []byte("factory-one-heartbeat-ack"),
		handshakeResponse:      []byte("factory-one-handshake"),
		handshakeErrorResponse: []byte("factory-one-handshake-error"),
	}
	gomock.InOrder(
		mockEncoder.EXPECT().Encode(packet.Type(packet.Handshake), gomock.Not(gomock.Nil())).Return(packetData.handshakeResponse, nil),
		mockEncoder.EXPECT().Encode(packet.Type(packet.Heartbeat), gomock.Nil()).Return(packetData.heartbeat, nil),
		mockEncoder.EXPECT().Encode(packet.Type(packet.HeartbeatAck), gomock.Nil()).Return(packetData.heartbeatAck, nil),
		mockEncoder.EXPECT().Encode(packet.Type(packet.Handshake), gomock.Not(gomock.Nil())).Return(packetData.handshakeErrorResponse, nil),
	)
	messageEncoder := message.NewMessagesEncoder(false)

	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
	sessionPool := session.NewSessionPool()
	serverID := uuid.New().String()

	mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any()).Times(2)
	factory := NewAgentFactory(dieChan, mockDecoder, mockEncoder, mockSerializer, hbTime, messageEncoder, 10, sessionPool, mockMetricsReporters, serverID).(*agentFactoryImpl)
	ag := factory.CreateAgent(mockConn).(*agentImpl)
	assert.NotNil(t, ag)
	assert.IsType(t, make(chan struct{}), ag.chDie)
	assert.IsType(t, make(chan pendingWrite), ag.chSend)
	assert.IsType(t, make(chan struct{}), ag.chStopHeartbeat)
	assert.IsType(t, make(chan struct{}), ag.chStopWrite)
	assert.Equal(t, dieChan, ag.appDieChan)
	assert.Equal(t, 10, ag.messagesBufferSize)
	assert.Equal(t, mockConn, ag.conn)
	assert.Equal(t, mockDecoder, ag.decoder)
	assert.Equal(t, mockEncoder, ag.encoder)
	assert.Equal(t, hbTime, ag.heartbeatTimeout)
	assert.InDelta(t, time.Now().Unix(), ag.lastAt, 1)
	assert.Equal(t, mockSerializer, ag.serializer)
	assert.Equal(t, mockMetricsReporters, ag.metricsReporters)
	assert.Equal(t, packetData, ag.packetData)
	assert.Equal(t, constants.StatusStart, ag.state)
	assert.NotNil(t, ag.Session)
	assert.True(t, ag.Session.GetIsFrontend())
	assert.Equal(t, serverID, ag.Session.GetFrontendID())
	assert.Equal(t, netip.MustParseAddr("127.0.0.1"), ag.Session.RemoteIP())

	secondAgent := factory.CreateAgent(nil).(*agentImpl)
	require.Same(t, ag.packetData, secondAgent.packetData)

	secondSerializer := serializemocks.NewMockSerializer(ctrl)
	secondSerializer.EXPECT().GetName().Return("factory-two").Times(1)
	secondEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	secondPacketData := &agentPacketData{
		heartbeat:              []byte("factory-two-heartbeat"),
		heartbeatAck:           []byte("factory-two-heartbeat-ack"),
		handshakeResponse:      []byte("factory-two-handshake"),
		handshakeErrorResponse: []byte("factory-two-handshake-error"),
	}
	gomock.InOrder(
		secondEncoder.EXPECT().Encode(packet.Type(packet.Handshake), gomock.Not(gomock.Nil())).Return(secondPacketData.handshakeResponse, nil),
		secondEncoder.EXPECT().Encode(packet.Type(packet.Heartbeat), gomock.Nil()).Return(secondPacketData.heartbeat, nil),
		secondEncoder.EXPECT().Encode(packet.Type(packet.HeartbeatAck), gomock.Nil()).Return(secondPacketData.heartbeatAck, nil),
		secondEncoder.EXPECT().Encode(packet.Type(packet.Handshake), gomock.Not(gomock.Nil())).Return(secondPacketData.handshakeErrorResponse, nil),
	)
	secondFactory := NewAgentFactory(nil, nil, secondEncoder, secondSerializer, 2*hbTime, message.NewMessagesEncoder(false), 1, session.NewSessionPool(), nil, "second").(*agentFactoryImpl)
	thirdAgent := secondFactory.CreateAgent(nil).(*agentImpl)
	require.NotSame(t, ag.packetData, thirdAgent.packetData)
	require.Equal(t, secondPacketData, thirdAgent.packetData)
}

func TestAgentFactoryPacketDataInitializationFailureIsSticky(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	initErr := errors.New("encode static packet")
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName().Return("serializer").Times(1)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	mockEncoder.EXPECT().Encode(packet.Type(packet.Handshake), gomock.Not(gomock.Nil())).Return(nil, initErr).Times(1)

	factory := NewAgentFactory(
		nil,
		nil,
		mockEncoder,
		mockSerializer,
		time.Second,
		message.NewMessagesEncoder(false),
		1,
		session.NewSessionPool(),
		nil,
		"",
	)

	for range 2 {
		require.PanicsWithError(t, initErr.Error(), func() {
			factory.CreateAgent(nil)
		})
	}
}

func TestAgentFactoryPacketDataConcurrentInitialization(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName().Return("serializer").Times(1)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	packetData := &agentPacketData{
		heartbeat:              []byte("heartbeat"),
		heartbeatAck:           []byte("heartbeat-ack"),
		handshakeResponse:      []byte("handshake"),
		handshakeErrorResponse: []byte("handshake-error"),
	}
	gomock.InOrder(
		mockEncoder.EXPECT().Encode(packet.Type(packet.Handshake), gomock.Not(gomock.Nil())).Return(packetData.handshakeResponse, nil),
		mockEncoder.EXPECT().Encode(packet.Type(packet.Heartbeat), gomock.Nil()).Return(packetData.heartbeat, nil),
		mockEncoder.EXPECT().Encode(packet.Type(packet.HeartbeatAck), gomock.Nil()).Return(packetData.heartbeatAck, nil),
		mockEncoder.EXPECT().Encode(packet.Type(packet.Handshake), gomock.Not(gomock.Nil())).Return(packetData.handshakeErrorResponse, nil),
	)

	const agentCount = 32
	factory := NewAgentFactory(
		nil,
		nil,
		mockEncoder,
		mockSerializer,
		time.Second,
		message.NewMessagesEncoder(false),
		1,
		session.NewSessionPool(),
		nil,
		"",
	)
	agents := make([]*agentImpl, agentCount)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(agentCount)
	for i := range agents {
		go func() {
			defer wg.Done()
			<-start
			agents[i] = factory.CreateAgent(nil).(*agentImpl)
		}()
	}
	close(start)
	wg.Wait()

	for _, agent := range agents {
		require.Same(t, agents[0].packetData, agent.packetData)
	}
	require.Equal(t, packetData, agents[0].packetData)
}

func TestAgentRemoteIPUsesConfiguredTrustedProxies(t *testing.T) {
	trusted, err := netutil.ParseTrustedProxies([]string{"10.0.0.0/8"})
	require.NoError(t, err)
	req := &http.Request{
		RemoteAddr: "10.0.0.2:443",
		Header: http.Header{
			"X-Forwarded-For": []string{"198.51.100.7"},
		},
	}
	wsConn, err := acceptor.NewWSConn(nil, req)
	require.NoError(t, err)

	ag := &agentImpl{conn: wsConn, trustedProxies: trusted}
	require.Equal(t, netip.MustParseAddr("198.51.100.7"), ag.RemoteIP())

	ag.trustedProxies = netutil.TrustedProxies{}
	require.Equal(t, netip.MustParseAddr("10.0.0.2"), ag.RemoteIP())
}

func TestAgentRemoteIPFromSocketAddress(t *testing.T) {
	for _, table := range []struct {
		name     string
		addr     net.Addr
		want     netip.Addr
		wrapWith func(net.Conn) net.Conn
	}{
		{
			name: "ipv4 tcp",
			addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 3010},
			want: netip.MustParseAddr("192.0.2.10"),
		},
		{
			name: "ipv6 tcp",
			addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::10"), Port: 3010},
			want: netip.MustParseAddr("2001:db8::10"),
		},
		{
			name: "mapped ipv4",
			addr: &net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.11"), Port: 3010},
			want: netip.MustParseAddr("192.0.2.11"),
		},
		{
			name: "tls tcp",
			addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::12"), Port: 3010},
			want: netip.MustParseAddr("2001:db8::12"),
			wrapWith: func(conn net.Conn) net.Conn {
				return tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12})
			},
		},
		{
			name: "wrapped address",
			addr: &customMockAddr{network: "tcp", str: "[2001:db8::13]:3010"},
			want: netip.MustParseAddr("2001:db8::13"),
		},
		{name: "invalid wrapped address", addr: &customMockAddr{network: "tcp", str: "invalid"}},
		{name: "non-IP wrapped host", addr: &customMockAddr{network: "tcp", str: "not-an-ip:3010"}},
		{name: "nil address"},
	} {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			conn := mocks.NewMockPlayerConn(ctrl)
			conn.EXPECT().RemoteAddr().Return(table.addr)
			var remote net.Conn = conn
			if table.wrapWith != nil {
				remote = table.wrapWith(remote)
			}
			ag := &agentImpl{conn: remote}
			assert.Equal(t, table.want, ag.RemoteIP())
		})
	}
}

func TestOutboundCompletionNilAndClosedQueuePathsAreNoOps(t *testing.T) {
	var completion *outboundCompletion
	assert.NotPanics(t, func() { completion.finish(nil) })

	ag := &agentImpl{
		chSend:      make(chan pendingWrite),
		chStopWrite: make(chan struct{}),
	}
	close(ag.chSend)

	ag.write()
	select {
	case <-ag.writerDone:
	default:
		t.Fatal("writer did not finish after its outbound queue closed")
	}
	assert.NotPanics(t, func() { ag.failQueuedWrites(brokenPipeError()) })
}

func TestAgentFactoryConstructorsPreserveDefaultAndTrustedPolicies(t *testing.T) {
	constructorType := reflect.TypeOf(NewAgentFactory)
	require.False(t, constructorType.IsVariadic())
	require.Equal(t, 10, constructorType.NumIn())

	trusted, err := netutil.ParseTrustedProxies([]string{"10.0.0.0/8"})
	require.NoError(t, err)

	defaultFactory := NewAgentFactory(nil, nil, nil, nil, 0, nil, 0, nil, nil, "").(*agentFactoryImpl)
	require.False(t, defaultFactory.trustedProxies.Contains(netip.MustParseAddr("10.1.2.3")))

	trustedFactory := NewAgentFactoryWithTrustedProxies(
		nil, nil, nil, nil, 0, nil, 0, nil, nil, "", trusted,
	).(*agentFactoryImpl)
	require.True(t, trustedFactory.trustedProxies.Contains(netip.MustParseAddr("10.1.2.3")))
}

func TestKick(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	mockDecoder := codecmocks.NewMockPacketDecoder(ctrl)
	dieChan := make(chan bool)
	hbTime := time.Second

	mockConn := newAgentMockConn(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockEncoder.EXPECT().Encode(packet.Type(packet.Kick), gomock.Any()).Do(
		func(typ packet.Type, d []byte) {
			assert.EqualValues(t, packet.Kick, typ)
			assert.EqualValues(t, session.CloseReasonKickRebind, binary.BigEndian.Uint32(d))
		}).Return([]byte("kick"), nil)
	mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	mockConn.EXPECT().Write(gomock.Any()).Return(0, nil)
	messageEncoder := message.NewMessagesEncoder(false)

	mockSerializer.EXPECT().GetName()

	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, mockDecoder, mockEncoder, mockSerializer, hbTime, 10, dieChan, messageEncoder, nil, sessionPool, "")
	c := context.Background()
	err := ag.Kick(c)
	assert.NoError(t, err)
}

func TestAgentSend(t *testing.T) {
	tables := []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"failure", brokenPipeError()},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockSerializer := serializemocks.NewMockSerializer(ctrl)
			mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
			heartbeatAndHandshakeMocks(mockEncoder)
			mockDecoder := codecmocks.NewMockPacketDecoder(ctrl)
			dieChan := make(chan bool)
			hbTime := time.Second
			messageEncoder := message.NewMessagesEncoder(false)

			mockConn := newAgentMockConn(ctrl)
			mockSerializer.EXPECT().GetName()
			sessionPool := session.NewSessionPool()
			ag := newAgent(mockConn, mockDecoder, mockEncoder, mockSerializer, hbTime, 10, dieChan, messageEncoder, nil, sessionPool, "").(*agentImpl)
			assert.NotNil(t, ag)

			if table.err != nil {
				close(ag.chSend)
			}
			pm := pendingMessage{}

			expectedBytes := []byte("ok")

			mockSerializer.EXPECT().Marshal(nil).Return(expectedBytes, nil)
			mockEncoder.EXPECT().Encode(packet.Type(packet.Data), gomock.Any()).Return(expectedBytes, nil)

			err := ag.send(pm)
			assert.Equal(t, table.err, err)

			expectedWrite := pendingWrite{
				ctx:  nil,
				data: expectedBytes,
				err:  nil,
			}

			if table.err == nil {
				recv := helpers.ShouldEventuallyReceive(t, ag.chSend).(pendingWrite)
				assert.Equal(t, expectedWrite.ctx, recv.ctx)
				assert.Equal(t, expectedWrite.data, recv.data)
				assert.Equal(t, expectedWrite.err, recv.err)
				ag.finishPendingWrite(recv, nil)
			}
		})
	}
}

func TestAgentSendSerializeErr(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockConn := mocks.NewMockPlayerConn(ctrl)
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	messageEncoder := message.NewMessagesEncoder(false)
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
	sessionPool := session.NewSessionPool()
	ss, _ := sessionPool.NewSession(nil, true)
	ag := &agentImpl{ // avoid heartbeat and handshake to fully test serialize
		conn:               mockConn,
		chSend:             make(chan pendingWrite, 10),
		messagesBufferSize: 10,
		encoder:            mockEncoder,
		heartbeatTimeout:   time.Second,
		lastAt:             time.Now().Unix(),
		serializer:         mockSerializer,
		messageEncoder:     messageEncoder,
		metricsReporters:   mockMetricsReporters,
		Session:            ss,
	}

	ctx := getCtxWithRequestKeys()
	var wg sync.WaitGroup
	wg.Add(1)
	mockMetricsReporters[0].(*metricsmocks.MockReporter).
		EXPECT().
		ReportSummary(metrics.ResponseTime, gomock.Any(), gomock.Any()).
		Do(func(string, map[string]string, float64) { wg.Done() })

	expected := pendingMessage{
		ctx:     ctx,
		typ:     message.Response,
		route:   uuid.New().String(),
		mid:     uint(rand.Int()),
		payload: someStruct{A: "bla"},
	}
	expectedErr := errors.New("noo")
	mockSerializer.EXPECT().Marshal(expected.payload).Return(nil, expectedErr)

	expectedBT := []byte("bla")
	expectedAPIError := apierrors.FromError(expectedErr)
	mockSerializer.EXPECT().Marshal(&expectedAPIError.Status).Return(expectedBT, nil)
	m := &message.Message{
		Type:  expected.typ,
		Data:  expectedBT,
		Route: expected.route,
		ID:    expected.mid,
		Err:   false,
	}

	em, err := messageEncoder.Encode(m)
	assert.NoError(t, err)
	expectedPacket := []byte("packet")
	mockEncoder.EXPECT().Encode(gomock.Any(), em).Return(expectedPacket, nil)

	mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	mockConn.EXPECT().Write(expectedPacket).Return(len(expectedPacket), nil)
	go ag.write()
	mockMetricsReporter.EXPECT().ReportGauge(gomock.Any(), gomock.Any(), gomock.Any())
	ag.send(expected)
	wg.Wait()

}

func TestAgentPushFailsIfClosedAgent(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()

	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	messageEncoder := message.NewMessagesEncoder(false)

	sessionPool := session.NewSessionPool()
	ag := newAgent(nil, nil, mockEncoder, mockSerializer, time.Second, 10, nil, messageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)
	ag.state = constants.StatusClosed
	err := ag.Push("", nil)
	assert.Equal(t, brokenPipeError(), err)
}

func TestAgentPushStruct(t *testing.T) {
	tables := []struct {
		name string
		data interface{}
		err  error
	}{
		{"success_struct", &someStruct{A: "ok"}, nil},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockSerializer := serializemocks.NewMockSerializer(ctrl)
			mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
			heartbeatAndHandshakeMocks(mockEncoder)
			mockDecoder := codecmocks.NewMockPacketDecoder(ctrl)
			dieChan := make(chan bool)
			hbTime := time.Second
			messageEncoder := message.NewMessagesEncoder(false)
			mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
			mockConn := newAgentMockConn(ctrl)
			mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
			mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any())
			mockSerializer.EXPECT().GetName()
			sessionPool := session.NewSessionPool()
			ag := newAgent(mockConn, mockDecoder, mockEncoder, mockSerializer, hbTime, 10, dieChan, messageEncoder, mockMetricsReporters, sessionPool, "").(*agentImpl)
			assert.NotNil(t, ag)

			expectedBytes := []byte("hello")
			msg := &message.Message{
				Type:  message.Push,
				Route: uuid.New().String(),
				Data:  expectedBytes,
			}
			em, err := messageEncoder.Encode(msg)
			assert.NoError(t, err)
			mockSerializer.EXPECT().Marshal(table.data).Return(expectedBytes, nil)
			mockEncoder.EXPECT().Encode(packet.Type(packet.Data), em).Return(expectedBytes, nil)
			expectedWrite := pendingWrite{ctx: nil, data: expectedBytes, err: nil}

			if table.err != nil {
				close(ag.chSend)
			}

			mockMetricsReporter.EXPECT().ReportGauge(metrics.ChannelCapacity, gomock.Any(), float64(10))
			err = ag.Push(msg.Route, table.data)
			assert.Equal(t, table.err, err)

			if table.err == nil {
				recvData := helpers.ShouldEventuallyReceive(t, ag.chSend).(pendingWrite)
				assert.Equal(t, expectedWrite.ctx, recvData.ctx)
				assert.Equal(t, expectedWrite.data, recvData.data)
				assert.Equal(t, expectedWrite.err, recvData.err)
				ag.finishPendingWrite(recvData, nil)
			}
		})
	}
}

func TestAgentPush(t *testing.T) {
	tables := []struct {
		name string
		data []byte
		err  error
	}{
		{"success_raw", []byte("ok"), nil},
		{"failure", []byte("ok"), brokenPipeError()},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockSerializer := serializemocks.NewMockSerializer(ctrl)
			mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
			heartbeatAndHandshakeMocks(mockEncoder)
			mockDecoder := codecmocks.NewMockPacketDecoder(ctrl)
			dieChan := make(chan bool)
			hbTime := time.Second
			messageEncoder := message.NewMessagesEncoder(false)
			mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
			mockConn := newAgentMockConn(ctrl)
			mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
			mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any())
			mockSerializer.EXPECT().GetName()
			sessionPool := session.NewSessionPool()
			ag := newAgent(mockConn, mockDecoder, mockEncoder, mockSerializer, hbTime, 10, dieChan, messageEncoder, mockMetricsReporters, sessionPool, "").(*agentImpl)
			assert.NotNil(t, ag)

			expectedBytes := []byte("hello")
			msg := &message.Message{
				Type:  message.Push,
				Route: uuid.New().String(),
				Data:  table.data,
			}
			em, err := messageEncoder.Encode(msg)
			assert.NoError(t, err)
			mockEncoder.EXPECT().Encode(packet.Type(packet.Data), em).Return(expectedBytes, nil)
			expectedWrite := pendingWrite{ctx: nil, data: expectedBytes, err: nil}

			if table.err != nil {
				close(ag.chSend)
			}

			mockMetricsReporter.EXPECT().ReportGauge(metrics.ChannelCapacity, gomock.Any(), float64(10))
			err = ag.Push(msg.Route, table.data)
			assert.Equal(t, table.err, err)

			if table.err == nil {
				recvData := helpers.ShouldEventuallyReceive(t, ag.chSend).(pendingWrite)
				assert.Equal(t, expectedWrite.ctx, recvData.ctx)
				assert.Equal(t, expectedWrite.data, recvData.data)
				assert.Equal(t, expectedWrite.err, recvData.err)
				ag.finishPendingWrite(recvData, nil)
			}
		})
	}
}

func TestAgentPushFullChannel(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockDecoder := codecmocks.NewMockPacketDecoder(ctrl)
	dieChan := make(chan bool)
	hbTime := time.Second
	messageEncoder := message.NewMessagesEncoder(false)
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockConn := newAgentMockConn(ctrl)
	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
	mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any())
	mockSerializer.EXPECT().GetName()
	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, mockDecoder, mockEncoder, mockSerializer, hbTime, 0, dieChan, messageEncoder, mockMetricsReporters, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)

	mockMetricsReporter.EXPECT().ReportGauge(metrics.ChannelCapacity, gomock.Any(), float64(0))

	msg := &message.Message{
		Route: "route",
		Data:  []byte("data"),
		Type:  message.Push,
	}
	em, err := messageEncoder.Encode(msg)
	assert.NoError(t, err)

	mockEncoder.EXPECT().Encode(packet.Type(packet.Data), em)
	go func() {
		err := ag.Push(msg.Route, []byte("data"))
		assert.NoError(t, err)
	}()
	pWrite := helpers.ShouldEventuallyReceive(t, ag.chSend).(pendingWrite)
	ag.finishPendingWrite(pWrite, nil)
}

func TestAgentResponseMIDFailsIfClosedAgent(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().Return(false)
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()

	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
	mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any())
	sessionPool := session.NewSessionPool()
	ag := newAgent(nil, nil, mockEncoder, mockSerializer, time.Second, 10, nil, mockMessageEncoder, mockMetricsReporters, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)
	ag.state = constants.StatusClosed

	ctx := getCtxWithRequestKeys()
	mockMetricsReporter.EXPECT().ReportSummary(metrics.ResponseTime, gomock.Any(), gomock.Any())
	err := ag.ResponseMID(ctx, 1, nil)
	assert.Equal(t, brokenPipeError(), err)
}

func TestAgentResponseMID(t *testing.T) {
	tables := []struct {
		name   string
		mid    uint
		data   interface{}
		msgErr bool
		err    error
	}{
		{"success_raw", uint(rand.Int()), []byte("ok"), false, nil},
		{"success_raw_msg_err", uint(rand.Int()), []byte("ok"), true, nil},
		{"success_struct", uint(rand.Int()), &someStruct{A: "ok"}, false, nil},
		{"failure_empty_mid", 0, []byte("ok"), false, constants.ErrSessionOnNotify},
		{"failure_send", uint(rand.Int()), []byte("ok"), false, brokenPipeError()},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
			mockSerializer := serializemocks.NewMockSerializer(ctrl)
			mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
			heartbeatAndHandshakeMocks(mockEncoder)
			mockDecoder := codecmocks.NewMockPacketDecoder(ctrl)
			dieChan := make(chan bool)
			hbTime := time.Second
			messageEncoder := message.NewMessagesEncoder(false)

			mockConn := newAgentMockConn(ctrl)
			mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
			mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any())
			mockSerializer.EXPECT().GetName()
			sessionPool := session.NewSessionPool()
			ag := newAgent(mockConn, mockDecoder, mockEncoder, mockSerializer, hbTime, 10, dieChan, messageEncoder, mockMetricsReporters, sessionPool, "").(*agentImpl)
			assert.NotNil(t, ag)

			ctx := getCtxWithRequestKeys()
			if table.mid != 0 {
				mockEncoder.EXPECT().Encode(gomock.Any(), gomock.Any()).Return([]byte("ok!"), nil)
				mockMetricsReporter.EXPECT().ReportGauge(metrics.ChannelCapacity, gomock.Any(), float64(10))
			}
			if table.err != nil {
				mockMetricsReporter.EXPECT().ReportSummary(metrics.ResponseTime, gomock.Any(), gomock.Any())
			}
			if table.mid != 0 {
				if table.err != nil {
					close(ag.chSend)
				}
			}
			if reflect.TypeOf(table.data) != reflect.TypeOf([]byte{}) {
				mockSerializer.EXPECT().Marshal(table.data).Return([]byte("ok"), nil)
			}
			expected := pendingWrite{ctx: ctx, data: []byte("ok!"), err: nil}
			var err error
			if table.msgErr {
				err = ag.ResponseMID(ctx, table.mid, table.data, table.msgErr)
			} else {
				err = ag.ResponseMID(ctx, table.mid, table.data)
			}
			assert.Equal(t, table.err, err)

			if table.err == nil {
				recv := helpers.ShouldEventuallyReceive(t, ag.chSend).(pendingWrite)
				assert.Equal(t, expected.ctx, recv.ctx)
				assert.Equal(t, expected.data, recv.data)
				if table.msgErr {
					assert.NotNil(t, recv.err)
				}
			}
		})
	}
}

func TestAgentResponseMIDFullChannel(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockDecoder := codecmocks.NewMockPacketDecoder(ctrl)
	dieChan := make(chan bool)
	hbTime := time.Second
	messageEncoder := message.NewMessagesEncoder(false)
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockConn := newAgentMockConn(ctrl)
	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
	mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any())
	mockSerializer.EXPECT().GetName()
	mockEncoder.EXPECT().Encode(packet.Type(packet.Data), gomock.Any())
	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, mockDecoder, mockEncoder, mockSerializer, hbTime, 0, dieChan, messageEncoder, mockMetricsReporters, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)
	mockMetricsReporters[0].(*metricsmocks.MockReporter).EXPECT().ReportGauge(metrics.ChannelCapacity, gomock.Any(), float64(0))
	go func() {
		err := ag.ResponseMID(nil, 1, []byte("data"))
		assert.NoError(t, err)
	}()
	pWrite := helpers.ShouldEventuallyReceive(t, ag.chSend).(pendingWrite)
	ag.finishPendingWrite(pWrite, nil)
}

func TestAgentCloseFailsIfAlreadyClosed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().AnyTimes()
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()

	sessionPool := session.NewSessionPool()
	ag := newAgent(nil, nil, mockEncoder, mockSerializer, time.Second, 10, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)
	ag.state = constants.StatusClosed
	err := ag.Close(nil)
	assert.Equal(t, constants.ErrCloseClosedSession, err)
}

func TestAgentClose(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockConn := newAgentMockConn(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().AnyTimes()
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()

	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)

	expected := false
	f := func() { expected = true }
	err := ag.Session.OnClose(f)
	assert.NoError(t, err)

	mockConn.EXPECT().Close()
	err = ag.Close(nil)
	assert.NoError(t, err)
	assert.Equal(t, ag.state, constants.StatusClosed)
	assert.True(t, expected)
	waitForChannelClose(t, ag.chStopWrite, 500*time.Millisecond)
	waitForChannelClose(t, ag.chStopHeartbeat, 500*time.Millisecond)
	waitForChannelClose(t, ag.chDie, 500*time.Millisecond)
}

func TestAgentCloseRejectsWaitingAndLateOutboundItems(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockConn := newAgentMockConn(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any()).Times(2)
	mockMetricsReporter.EXPECT().ReportGauge(metrics.ChannelCapacity, gomock.Any(), gomock.Any()).Times(3)
	mockMetricsReporter.EXPECT().ReportSummary(metrics.ResponseTime, gomock.Any(), gomock.Any()).Times(1)
	mockEncoder.EXPECT().Encode(packet.Type(packet.Data), gomock.Any()).Return([]byte("packet"), nil).Times(3)
	mockConn.EXPECT().Close().Return(nil)

	ag := newAgent(
		mockConn,
		nil,
		mockEncoder,
		mockSerializer,
		time.Hour,
		1,
		nil,
		message.NewMessagesEncoder(false),
		[]metrics.Reporter{mockMetricsReporter},
		session.NewSessionPool(),
		"",
	).(*agentImpl)

	require.NoError(t, ag.Push("route", []byte("first")))
	responseResult := make(chan error, 1)
	go func() {
		responseResult <- ag.ResponseMID(getCtxWithRequestKeys(), 1, []byte("second"))
	}()
	require.Eventually(t, func() bool {
		return ag.outboundAdmissionN.Load() == 1
	}, time.Second, time.Millisecond)

	require.NoError(t, ag.Close(nil))
	assert.ErrorIs(t, <-responseResult, constants.ErrBrokenPipe)
	assert.Zero(t, ag.outboundAdmissionN.Load())
	assert.Zero(t, ag.outboundItemN.Load())
	select {
	case <-ag.chSend:
		t.Fatal("Close left an accepted outbound item in the queue")
	default:
	}

	err := ag.send(pendingMessage{typ: message.Push, payload: []byte("late")})
	assert.ErrorIs(t, err, constants.ErrBrokenPipe)
	assert.ErrorIs(t, ag.SendHeartbeatResponse(0), constants.ErrBrokenPipe)
}

func TestAgentRemoteAddr(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockConn := mocks.NewMockPlayerConn(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().Return(false)
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()

	expected := &mockAddr{}
	mockConn.EXPECT().RemoteAddr().Return(expected).AnyTimes()
	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "")
	assert.NotNil(t, ag)

	addr := ag.RemoteAddr()
	assert.Equal(t, expected, addr)
}

func TestAgentString(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockConn := mocks.NewMockPlayerConn(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().Return(false)
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()

	mockConn.EXPECT().RemoteAddr().Return(&mockAddr{}).AnyTimes()
	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)

	expected := fmt.Sprintf("Remote=remote-string, LastTime=%d", ag.lastAt)
	str := ag.String()
	assert.Equal(t, expected, str)
}

func TestAgentGetStatus(t *testing.T) {
	tables := []struct {
		name   string
		status int32
	}{
		{"start", constants.StatusStart},
		{"closed", constants.StatusClosed},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockConn := newAgentMockConn(ctrl)
			mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
			heartbeatAndHandshakeMocks(mockEncoder)
			mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
			mockMessageEncoder.EXPECT().IsCompressionEnabled().Return(false)

			mockSerializer := serializemocks.NewMockSerializer(ctrl)
			mockSerializer.EXPECT().GetName()

			sessionPool := session.NewSessionPool()
			ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
			assert.NotNil(t, ag)

			ag.state = table.status

			status := ag.GetStatus()
			assert.Equal(t, table.status, status)
		})
	}
}

func TestAgentSetLastAt(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().Return(false)
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()

	sessionPool := session.NewSessionPool()
	ag := newAgent(nil, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)

	ag.lastAt = 0
	ag.SetLastAt()
	assert.InDelta(t, time.Now().Unix(), ag.lastAt, 1)
}

func TestAgentSetStatus(t *testing.T) {
	tables := []struct {
		name   string
		status int32
	}{
		{"start", constants.StatusStart},
		{"closed", constants.StatusClosed},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
			heartbeatAndHandshakeMocks(mockEncoder)
			mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
			mockMessageEncoder.EXPECT().IsCompressionEnabled().Return(false)
			mockSerializer := serializemocks.NewMockSerializer(ctrl)
			mockSerializer.EXPECT().GetName()

			sessionPool := session.NewSessionPool()
			ag := newAgent(nil, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
			assert.NotNil(t, ag)

			ag.SetStatus(table.status)
			assert.Equal(t, table.status, ag.state)
		})
	}
}

func TestAgentStatusTransitionsAreAtomicAndClosedIsTerminal(t *testing.T) {
	ag := &agentImpl{state: constants.StatusStart}

	assert.True(t, TransitionStatus(ag, constants.StatusStart, constants.StatusHandshakeValidated))
	assert.Equal(t, constants.StatusHandshakeValidated, ag.GetStatus())
	assert.False(t, TransitionStatus(ag, constants.StatusStart, constants.StatusWorking))
	assert.True(t, TransitionStatus(ag, constants.StatusHandshakeValidated, constants.StatusWorking))

	ag.SetStatus(constants.StatusClosed)
	ag.SetStatus(constants.StatusWorking)
	assert.Equal(t, constants.StatusClosed, ag.GetStatus())
	assert.False(t, TransitionStatus(ag, constants.StatusClosed, constants.StatusWorking))
}

func TestAgentCloseWinsConcurrentHandshakeAck(t *testing.T) {
	const attempts = 100
	for range attempts {
		ag := &agentImpl{state: constants.StatusHandshakeValidated}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			ag.SetStatus(constants.StatusClosed)
		}()
		go func() {
			defer wg.Done()
			<-start
			TransitionStatus(ag, constants.StatusHandshakeValidated, constants.StatusWorking)
		}()
		close(start)
		wg.Wait()
		assert.Equal(t, constants.StatusClosed, ag.GetStatus())
	}
}

func TestOnSessionClosed(t *testing.T) {
	ctrl := gomock.NewController(t)

	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().Return(false)
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()
	sessionPool := session.NewSessionPool()
	ag := newAgent(nil, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)

	ss, _ := sessionPool.NewSession(nil, true)

	expected := false
	f := func() { expected = true }
	err := ss.OnClose(f)
	assert.NoError(t, err)

	assert.NotPanics(t, func() { ag.onSessionClosed(ss, nil) })
	assert.True(t, expected)
}

func TestOnSessionClosedRecoversIfPanic(t *testing.T) {
	ctrl := gomock.NewController(t)

	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().Return(false)
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()
	sessionPool := session.NewSessionPool()
	ag := newAgent(nil, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)

	ss, _ := sessionPool.NewSession(nil, true)

	expected := false
	f := func() {
		expected = true
		panic("oh noes")
	}
	err := ss.OnClose(f)
	assert.NoError(t, err)

	assert.NotPanics(t, func() { ag.onSessionClosed(ss, nil) })
	assert.True(t, expected)
}

func TestAgentSendHandshakeResponse(t *testing.T) {
	tables := []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"failure", errors.New("handshake failed")},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockConn := newAgentMockConn(ctrl)
			mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
			heartbeatAndHandshakeMocks(mockEncoder)
			mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
			mockMessageEncoder.EXPECT().IsCompressionEnabled().AnyTimes()
			mockSerializer := serializemocks.NewMockSerializer(ctrl)
			mockSerializer.EXPECT().GetName()

			sessionPool := session.NewSessionPool()
			ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
			assert.NotNil(t, ag)

			mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
			mockConn.EXPECT().Write(ag.packetData.handshakeResponse).Return(0, table.err)
			err := ag.SendHandshakeResponse()
			assert.Equal(t, table.err, err)
		})
	}
}

func TestAgentSendHeartbeatResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockConn := newAgentMockConn(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().AnyTimes()
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()

	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Second, 1, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)

	err := ag.SendHeartbeatResponse(0)
	assert.NoError(t, err)
	pWrite := helpers.ShouldEventuallyReceive(t, ag.chSend).(pendingWrite)
	assert.NotNil(t, pWrite.ctx)
	assert.Equal(t, ag.packetData.heartbeatAck, pWrite.data)
	ag.finishPendingWrite(pWrite, nil)

	const timestamp = int64(123456789)
	timestampData := make([]byte, 8)
	binary.BigEndian.PutUint64(timestampData, uint64(timestamp))
	encodedHeartbeat := []byte("encoded heartbeat")
	mockEncoder.EXPECT().Encode(packet.Type(packet.HeartbeatAck), timestampData).Return(encodedHeartbeat, nil)

	err = ag.SendHeartbeatResponse(timestamp)
	assert.NoError(t, err)
	pWrite = helpers.ShouldEventuallyReceive(t, ag.chSend).(pendingWrite)
	assert.NotNil(t, pWrite.ctx)
	assert.Equal(t, encodedHeartbeat, pWrite.data)
	ag.finishPendingWrite(pWrite, nil)
}

func TestAnswerWithError(t *testing.T) {
	tables := []struct {
		name          string
		getPayloadErr error
		resErr        error
		err           error
	}{
		{"success", nil, nil, nil},
		{"failure_get_payload", errors.New("serialize err"), nil, errors.New("serialize err")},
		{"failure_response_mid", nil, errors.New("responsemid err"), errors.New("responsemid err")},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockSerializer := serializemocks.NewMockSerializer(ctrl)
			mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
			heartbeatAndHandshakeMocks(mockEncoder)
			messageEncoder := message.NewMessagesEncoder(false)
			mockSerializer.EXPECT().GetName()
			sessionPool := session.NewSessionPool()
			ag := newAgent(nil, nil, mockEncoder, mockSerializer, time.Second, 1, nil, messageEncoder, nil, sessionPool, "").(*agentImpl)
			assert.NotNil(t, ag)

			mockSerializer.EXPECT().Marshal(gomock.Any()).Return(nil, table.getPayloadErr)
			if table.getPayloadErr == nil {
				mockEncoder.EXPECT().Encode(packet.Type(packet.Data), gomock.Any())
			}
			ag.AnswerWithError(nil, uint(rand.Int()), errors.New("something went wrong"))
			if table.err == nil {
				pWrite := helpers.ShouldEventuallyReceive(t, ag.chSend).(pendingWrite)
				ag.finishPendingWrite(pWrite, nil)
			}
		})
	}
}

func TestAgentHeartbeatClosesUnhandshakenConnectionAfterProgressiveTimeout(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatPacket := []byte("heartbeat")
	encodedHeartbeats := make(chan int64, 2)
	heartbeatAndHandshakeMocksWithResponse(mockEncoder, heartbeatPacket, func(data []byte) {
		encodedHeartbeats <- int64(binary.BigEndian.Uint64(data))
	})
	mockConn := newAgentMockConn(ctrl)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().AnyTimes()
	mockSerializer.EXPECT().GetName()
	sessionPool := session.NewSessionPool()
	callbackState := make(chan sessionCloseObservation, 1)
	sessionPool.OnSessionClose(func(s session.Session, _ map[string]string, _ session.CloseReason) {
		callbackState <- sessionCloseObservation{
			online:     s.Online(),
			registered: sessionPool.GetSessionByID(s.ID()) != nil,
		}
	})
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, 1*time.Second, 1, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)
	require.Equal(t, constants.StatusStart, ag.GetStatus())

	mockConn.EXPECT().Close().MaxTimes(1)

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ag.heartbeatLoop(ticks)
	}()

	lastAt := time.Unix(ag.lastAt, 0)
	for _, tick := range []time.Time{lastAt.Add(time.Second), lastAt.Add(2 * time.Second)} {
		ticks <- tick
		pWrite := helpers.ShouldEventuallyReceive(t, ag.chSend).(pendingWrite)
		assert.Equal(t, heartbeatPacket, pWrite.data)
		assert.Equal(t, tick.UnixMilli(), helpers.ShouldEventuallyReceive(t, encodedHeartbeats).(int64))
		ag.finishPendingWrite(pWrite, nil)
	}

	ticks <- lastAt.Add(3 * time.Second)
	waitForChannelClose(t, done, time.Second)
	waitForChannelClose(t, ag.chDie, time.Second)
	assert.Equal(t, constants.StatusClosed, ag.GetStatus())
	observed := helpers.ShouldEventuallyReceive(t, callbackState).(sessionCloseObservation)
	assert.False(t, observed.online)
	assert.False(t, observed.registered)
}

func TestAgentHeartbeatExitsIfConnError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatPacket := []byte("heartbeat")
	heartbeatAndHandshakeMocksWithResponse(mockEncoder, heartbeatPacket, nil)
	mockConn := newAgentMockConn(ctrl)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockMessageEncoder.EXPECT().IsCompressionEnabled().AnyTimes()
	mockSerializer.EXPECT().GetName()
	sessionPool := session.NewSessionPool()
	callbackState := make(chan sessionCloseObservation, 1)
	sessionPool.OnSessionClose(func(s session.Session, _ map[string]string, _ session.CloseReason) {
		callbackState <- sessionCloseObservation{
			online:     s.Online(),
			registered: sessionPool.GetSessionByID(s.ID()) != nil,
		}
	})
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, 1*time.Second, 1, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)

	mockConn.EXPECT().Close().MaxTimes(1)
	mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	mockConn.EXPECT().Write(heartbeatPacket).Return(0, errors.New("connection failed"))

	ticks := make(chan time.Time)
	heartbeatDone := make(chan struct{})
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		ag.write()
	}()
	go func() {
		defer close(heartbeatDone)
		ag.heartbeatLoop(ticks)
	}()

	ticks <- time.Unix(ag.lastAt, 0).Add(time.Second)

	waitForChannelClose(t, ag.chDie, time.Second)
	waitForChannelClose(t, writeDone, time.Second)
	waitForChannelClose(t, heartbeatDone, time.Second)
	observed := helpers.ShouldEventuallyReceive(t, callbackState).(sessionCloseObservation)
	assert.False(t, observed.online)
	assert.False(t, observed.registered)
}

func TestAgentHeartbeatExitsOnStopHeartbeat(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockConn := newAgentMockConn(ctrl)
	messageEncoder := message.NewMessagesEncoder(false)

	mockConn.EXPECT().Close().MaxTimes(1)

	mockSerializer.EXPECT().GetName()
	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, 1*time.Second, 1, nil, messageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)

	ticks := make(chan time.Time)
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		ag.heartbeatLoop(ticks)
	}()
	<-started

	assert.NoError(t, ag.Close(nil))
	waitForChannelClose(t, done, time.Second)
}

func TestAgentWriteChSend(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	mockConn := mocks.NewMockPlayerConn(ctrl)
	messageEncoder := message.NewMessagesEncoder(false)
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
	ag := &agentImpl{ // avoid heartbeat and handshake to fully test serialize
		conn:             mockConn,
		chSend:           make(chan pendingWrite, 10),
		encoder:          mockEncoder,
		heartbeatTimeout: time.Second,
		lastAt:           time.Now().Unix(),
		serializer:       mockSerializer,
		messageEncoder:   messageEncoder,
		metricsReporters: mockMetricsReporters,
	}
	ctx := getCtxWithRequestKeys()
	reported := make(chan struct{})
	mockMetricsReporters[0].(*metricsmocks.MockReporter).EXPECT().ReportSummary(metrics.ResponseTime, gomock.Any(), gomock.Any()).
		Do(func(string, map[string]string, float64) { close(reported) })

	expectedPacket := []byte("final")

	mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	mockConn.EXPECT().Write(expectedPacket).Return(len(expectedPacket), nil)
	go ag.write()
	ag.chSend <- pendingWrite{ctx: ctx, data: expectedPacket, err: nil}
	waitForChannelClose(t, reported, time.Second)
}

func TestAgentHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockConn := newAgentMockConn(ctrl)
	messageEncoder := message.NewMessagesEncoder(false)
	mockSerializer.EXPECT().GetName()
	sessionPool := session.NewSessionPool()
	sessionPool.SetClusterCache(session.NewRedisCache(nil, 10*time.Second))
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Minute, 1, nil, messageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)
	assert.True(t, TransitionStatus(ag, constants.StatusStart, constants.StatusHandshakeValidated))
	assert.True(t, TransitionStatus(ag, constants.StatusHandshakeValidated, constants.StatusWorking))
	ag.SetLastAt()

	expectedBytes := []byte("bla")

	mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	var wg sync.WaitGroup
	wg.Add(1)

	mockConn.EXPECT().Write(expectedBytes).Return(0, nil).Do(func(d []byte) {
		wg.Done()
	})

	// Session.Close on method exit
	mockConn.EXPECT().Close().MaxTimes(1)

	ag.chSend <- pendingWrite{ctx: nil, data: expectedBytes, err: nil}

	go ag.Handle()

	wg.Wait()
	ag.Session.Close(nil)
	waitForChannelClose(t, ag.chDie, 5*time.Second)
}

func TestAgentHandleWaitsForAdmittedRuntimeTasks(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockConn := newAgentMockConn(ctrl)
	messageEncoder := message.NewMessagesEncoder(false)
	sessionPool := session.NewSessionPool()
	sessionPool.SetClusterCache(session.NewRedisCache(nil, time.Hour))
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Hour, 1, nil, messageEncoder, nil, sessionPool, "").(*agentImpl)

	expectedBytes := []byte("blocked write")
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseWrite) }) })

	mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	mockConn.EXPECT().Write(expectedBytes).DoAndReturn(func([]byte) (int, error) {
		close(writeStarted)
		<-releaseWrite
		return len(expectedBytes), nil
	})
	mockConn.EXPECT().Close().Return(nil).MaxTimes(1)

	require.NoError(t, ag.enqueueOutbound(pendingWrite{data: expectedBytes}))
	handleDone := make(chan struct{})
	go func() {
		ag.Handle()
		close(handleDone)
	}()

	waitForChannelClose(t, writeStarted, time.Second)
	closeDone := make(chan struct{})
	go func() {
		ag.Session.Close(nil)
		close(closeDone)
	}()

	select {
	case <-handleDone:
		t.Fatal("Handle returned while an admitted write task was still running")
	default:
	}
	select {
	case <-closeDone:
		t.Fatal("Close returned while an admitted write item was still running")
	default:
	}

	releaseOnce.Do(func() { close(releaseWrite) })
	<-closeDone
	waitForChannelClose(t, handleDone, time.Second)
}

func TestAgentHandleAllowsHandshakeBeyondSingleHeartbeatInterval(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockConn := newAgentMockConn(ctrl)
	messageEncoder := message.NewMessagesEncoder(false)
	mockSerializer.EXPECT().GetName()
	mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil).AnyTimes()
	mockConn.EXPECT().Write(gomock.Any()).Return(0, nil).AnyTimes()
	mockConn.EXPECT().Close().Return(nil).MaxTimes(1)

	sessionPool := session.NewSessionPool()
	sessionPool.SetClusterCache(session.NewRedisCache(nil, 10*time.Second))
	heartbeatTimeout := 100 * time.Millisecond
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, heartbeatTimeout, 1, nil, messageEncoder, nil, sessionPool, "").(*agentImpl)

	go ag.Handle()
	timer := time.NewTimer(heartbeatTimeout + heartbeatTimeout/2)
	defer timer.Stop()
	select {
	case <-ag.chDie:
		t.Fatal("agent closed before a delayed handshake ACK could arrive")
	case <-timer.C:
	}

	require.True(t, TransitionStatus(ag, constants.StatusStart, constants.StatusHandshakeValidated))
	require.True(t, TransitionStatus(ag, constants.StatusHandshakeValidated, constants.StatusWorking))
	ag.SetLastAt()

	ag.Session.Close(nil)
	waitForChannelClose(t, ag.chDie, time.Second)
	assert.Equal(t, constants.StatusClosed, ag.GetStatus())
}

func TestNatsRPCServerReportMetrics(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockDecoder := codecmocks.NewMockPacketDecoder(ctrl)
	dieChan := make(chan bool)
	hbTime := time.Second
	messageEncoder := message.NewMessagesEncoder(false)
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
	mockConn := newAgentMockConn(ctrl)
	mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any())
	mockSerializer.EXPECT().GetName()
	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, mockDecoder, mockEncoder, mockSerializer, hbTime, 10, dieChan, messageEncoder, mockMetricsReporters, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)

	ag.messagesBufferSize = 0

	ag.chSend <- pendingWrite{}

	mockMetricsReporter.EXPECT().ReportGauge(metrics.ChannelCapacity, gomock.Any(), float64(-1)) // because buffersize is 0 and chan sz is 1
	ag.reportChannelSize()
}

type customMockAddr struct{ network, str string }

func (m *customMockAddr) Network() string { return m.network }
func (m *customMockAddr) String() string  { return m.str }

func TestIPVersion(t *testing.T) {
	tables := []struct {
		addr      string
		ipVersion string
	}{
		{"127.0.0.1:80", constants.IPv4},
		{"1.2.3.4:3333", constants.IPv4},
		{"::1:3333", constants.IPv6},
		{"2001:db8:0000:1:1:1:1:1:3333", constants.IPv6},
	}

	for _, table := range tables {
		t.Run("test_"+table.addr, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockConn := mocks.NewMockPlayerConn(ctrl)
			mockAddr := &customMockAddr{str: table.addr}

			mockConn.EXPECT().RemoteAddr().Return(mockAddr)
			a := &agentImpl{conn: mockConn}

			assert.Equal(t, table.ipVersion, a.IPVersion())
		})
	}
}

func TestClusterCacheKeepAliveIntervalIsAlwaysPositive(t *testing.T) {
	assert.Equal(t, time.Nanosecond, clusterCacheKeepAliveInterval(time.Nanosecond))
	assert.Equal(t, 10*time.Millisecond, clusterCacheKeepAliveInterval(100*time.Millisecond))
}
