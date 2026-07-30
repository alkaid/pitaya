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
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
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
	netutil "github.com/alkaid/pitaya/v2/util/net"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type mockAddr struct{}

func (m *mockAddr) Network() string { return "" }
func (m *mockAddr) String() string  { return "remote-string" }

type sessionCloseObservation struct {
	online     bool
	registered bool
}

func heartbeatAndHandshakeMocks(mockEncoder *codecmocks.MockPacketEncoder) {
	heartbeatAndHandshakeMocksWithResponse(mockEncoder, hbd, nil)
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

func TestNewAgent(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName().Times(2)

	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	mockDecoder := codecmocks.NewMockPacketDecoder(ctrl)
	dieChan := make(chan bool)
	hbTime := time.Second

	mockConn := mocks.NewMockPlayerConn(ctrl)

	mockEncoder.EXPECT().Encode(gomock.Any(), gomock.Not(gomock.Nil())).Do(
		func(typ packet.Type, d []byte) {
			// cannot compare inside the expect because they are equivalent but not equal
			assert.EqualValues(t, packet.Handshake, typ)
		}).Times(2)
	mockEncoder.EXPECT().Encode(gomock.Any(), gomock.Nil()).Do(
		func(typ packet.Type, d []byte) {
			assert.EqualValues(t, packet.Heartbeat, typ)
		})
	mockEncoder.EXPECT().Encode(gomock.Any(), gomock.Nil()).Do(
		func(typ packet.Type, d []byte) {
			assert.EqualValues(t, packet.HeartbeatAck, typ)
		})
	messageEncoder := message.NewMessagesEncoder(false)

	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
	sessionPool := session.NewSessionPool()
	serverID := uuid.New().String()

	mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any())
	ag := newAgent(mockConn, mockDecoder, mockEncoder, mockSerializer, hbTime, 10, dieChan, messageEncoder, mockMetricsReporters, sessionPool, serverID).(*agentImpl)
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
	assert.Equal(t, constants.StatusStart, ag.state)
	assert.NotNil(t, ag.Session)
	assert.True(t, ag.Session.GetIsFrontend())
	assert.Equal(t, serverID, ag.Session.GetFrontendID())

	// second call should no call hdb encode
	mockMetricsReporter.EXPECT().ReportGauge(metrics.ConnectedClients, gomock.Any(), gomock.Any())
	ag = newAgent(nil, nil, mockEncoder, mockSerializer, hbTime, 10, dieChan, messageEncoder, mockMetricsReporters, sessionPool, serverID).(*agentImpl)
	assert.NotNil(t, ag)
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

	mockConn := mocks.NewMockPlayerConn(ctrl)
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
		{"failure", apierrors.ClientClosed("pitaya.BrokenPipe", constants.ErrBrokenPipe.Error(), "")},
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

			mockConn := mocks.NewMockPlayerConn(ctrl)
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
				assert.Equal(t, expectedWrite, recv)
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
	assert.Equal(t, apierrors.ClientClosed("ErrBrokenPipe", "agent push error", "").WithCause(constants.ErrBrokenPipe), err)
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
			mockConn := mocks.NewMockPlayerConn(ctrl)
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
				assert.Equal(t, expectedWrite, recvData)
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
		{"failure", []byte("ok"), apierrors.ClientClosed("pitaya.BrokenPipe", constants.ErrBrokenPipe.Error(), "")},
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
			mockConn := mocks.NewMockPlayerConn(ctrl)
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
				assert.Equal(t, expectedWrite, recvData)
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
	mockConn := mocks.NewMockPlayerConn(ctrl)
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
	helpers.ShouldEventuallyReceive(t, ag.chSend)
}

func TestAgentResponseMIDFailsIfClosedAgent(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
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
	err := ag.ResponseMID(ctx, 1, nil)
	assert.Equal(t, apierrors.ClientClosed("ErrBrokenPipe", constants.ErrBrokenPipe.Error(), "").WithCause(constants.ErrBrokenPipe), err)
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
		{"failure_send", uint(rand.Int()), []byte("ok"), false,
			apierrors.ClientClosed("pitaya.BrokenPipe", constants.ErrBrokenPipe.Error(), "")},
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

			mockConn := mocks.NewMockPlayerConn(ctrl)
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
	mockConn := mocks.NewMockPlayerConn(ctrl)
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
	helpers.ShouldEventuallyReceive(t, ag.chSend)
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

	mockConn := mocks.NewMockPlayerConn(ctrl)
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

	mockConn.EXPECT().RemoteAddr()
	mockConn.EXPECT().Close()
	err = ag.Close(nil)
	assert.NoError(t, err)
	assert.Equal(t, ag.state, constants.StatusClosed)
	assert.True(t, expected)
	waitForChannelClose(t, ag.chStopWrite, 500*time.Millisecond)
	waitForChannelClose(t, ag.chStopHeartbeat, 500*time.Millisecond)
	waitForChannelClose(t, ag.chDie, 500*time.Millisecond)
}

func TestAgentRemoteAddr(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockConn := mocks.NewMockPlayerConn(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()

	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "")
	assert.NotNil(t, ag)

	expected := &mockAddr{}
	mockConn.EXPECT().RemoteAddr().Return(expected)
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
	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().GetName()

	sessionPool := session.NewSessionPool()
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)

	mockConn.EXPECT().RemoteAddr().Return(&mockAddr{})
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

			mockConn := mocks.NewMockPlayerConn(ctrl)
			mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
			heartbeatAndHandshakeMocks(mockEncoder)
			mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)

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

func TestOnSessionClosed(t *testing.T) {
	ctrl := gomock.NewController(t)

	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatAndHandshakeMocks(mockEncoder)
	mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
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

			mockConn := mocks.NewMockPlayerConn(ctrl)
			mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
			heartbeatAndHandshakeMocks(mockEncoder)
			mockMessageEncoder := messagemocks.NewMockEncoder(ctrl)
			mockMessageEncoder.EXPECT().IsCompressionEnabled().AnyTimes()
			mockSerializer := serializemocks.NewMockSerializer(ctrl)
			mockSerializer.EXPECT().GetName()

			sessionPool := session.NewSessionPool()
			ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, time.Second, 0, nil, mockMessageEncoder, nil, sessionPool, "")
			assert.NotNil(t, ag)

			mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
			mockConn.EXPECT().Write(hrd).Return(0, table.err)
			err := ag.SendHandshakeResponse()
			assert.Equal(t, table.err, err)
		})
	}
}

func TestAgentSendHeartbeatResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockConn := mocks.NewMockPlayerConn(ctrl)
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
	assert.Equal(t, hbAck, pWrite.data)

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
				helpers.ShouldEventuallyReceive(t, ag.chSend)
			}
		})
	}
}

func TestAgentHeartbeat(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSerializer := serializemocks.NewMockSerializer(ctrl)
	mockEncoder := codecmocks.NewMockPacketEncoder(ctrl)
	heartbeatPacket := []byte("heartbeat")
	encodedHeartbeats := make(chan int64, 2)
	heartbeatAndHandshakeMocksWithResponse(mockEncoder, heartbeatPacket, func(data []byte) {
		encodedHeartbeats <- int64(binary.BigEndian.Uint64(data))
	})
	mockConn := mocks.NewMockPlayerConn(ctrl)
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

	mockConn.EXPECT().RemoteAddr().MaxTimes(1)
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
		assert.Equal(t, pendingWrite{data: heartbeatPacket}, pWrite)
		assert.Equal(t, tick.UnixMilli(), helpers.ShouldEventuallyReceive(t, encodedHeartbeats).(int64))
	}

	ticks <- lastAt.Add(3 * time.Second)
	waitForChannelClose(t, done, time.Second)
	waitForChannelClose(t, ag.chDie, time.Second)
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
	mockConn := mocks.NewMockPlayerConn(ctrl)
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

	mockConn.EXPECT().RemoteAddr().MaxTimes(1)
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
	mockConn := mocks.NewMockPlayerConn(ctrl)
	messageEncoder := message.NewMessagesEncoder(false)

	mockConn.EXPECT().RemoteAddr().MaxTimes(1)
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
	mockConn := mocks.NewMockPlayerConn(ctrl)
	messageEncoder := message.NewMessagesEncoder(false)
	mockSerializer.EXPECT().GetName()
	sessionPool := session.NewSessionPool()
	sessionPool.SetClusterCache(session.NewRedisCache(nil, 10*time.Second))
	ag := newAgent(mockConn, nil, mockEncoder, mockSerializer, 1*time.Second, 1, nil, messageEncoder, nil, sessionPool, "").(*agentImpl)
	assert.NotNil(t, ag)

	expectedBytes := []byte("bla")

	// Sends two heartbeats and then times out
	mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil).Times(3)
	mockConn.EXPECT().Write(hbd).Return(0, nil).Times(2)
	var wg sync.WaitGroup
	wg.Add(1)

	mockConn.EXPECT().Write(expectedBytes).Return(0, nil).Do(func(d []byte) {
		wg.Done()
	})

	// Session.Close on method exit
	mockConn.EXPECT().RemoteAddr().MaxTimes(1)
	mockConn.EXPECT().Close().MaxTimes(1)

	ag.chSend <- pendingWrite{ctx: nil, data: expectedBytes, err: nil}

	go ag.Handle()

	wg.Wait()
	waitForChannelClose(t, ag.chDie, 5*time.Second)
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
	mockConn := mocks.NewMockPlayerConn(ctrl)
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
