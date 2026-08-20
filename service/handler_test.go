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

package service

import (
	"context"
	encjson "encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	agentmocks "github.com/alkaid/pitaya/v2/agent/mocks"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	metricsmocks "github.com/alkaid/pitaya/v2/metrics/mocks"
	connmock "github.com/alkaid/pitaya/v2/mocks"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/serialize/json"
	serializemocks "github.com/alkaid/pitaya/v2/serialize/mocks"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

var (
	once sync.Once
)

type mockAddr struct{}

func (m *mockAddr) Network() string { return "" }
func (m *mockAddr) String() string  { return "remote-string" }

type MyComp struct {
	component.Base
}

func (m *MyComp) Init()                        {}
func (m *MyComp) Shutdown()                    {}
func (m *MyComp) Handler1(ctx context.Context) {}
func (m *MyComp) Handler2(ctx context.Context, b []byte) ([]byte, error) {
	return nil, nil
}
func (m *MyComp) HandlerRawRaw(ctx context.Context, b []byte) ([]byte, error) {
	return b, nil
}

type NoHandlerRemoteComp struct {
	component.Base
}

func (m *NoHandlerRemoteComp) Init()     {}
func (m *NoHandlerRemoteComp) Shutdown() {}

func TestNewHandlerService(t *testing.T) {
	packetDecoder := codec.NewPomeloPacketDecoder()
	serializer := json.NewSerializer()
	sv := &cluster.Server{}
	remoteSvc := &RemoteService{}
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockMetricsReporter := metricsmocks.NewMockReporter(ctrl)
	mockMetricsReporters := []metrics.Reporter{mockMetricsReporter}
	mockAgentFactory := agentmocks.NewMockAgentFactory(ctrl)
	handlerHooks := pipeline.NewHandlerHooks()
	handlerPool := NewHandlerPool()
	svc := NewHandlerService(
		packetDecoder,
		serializer,
		9, 8,
		sv,
		remoteSvc,
		mockAgentFactory,
		mockMetricsReporters,
		handlerHooks,
		handlerPool,
	)

	assert.NotNil(t, svc)
	assert.Equal(t, packetDecoder, svc.decoder)
	assert.Equal(t, serializer, svc.serializer)
	assert.Equal(t, mockMetricsReporters, svc.metricsReporters)
	assert.Equal(t, sv, svc.server)
	assert.Equal(t, remoteSvc, svc.remoteService)
	assert.Equal(t, mockAgentFactory, svc.agentFactory)
	assert.Equal(t, handlerHooks, svc.handlerHooks)
	assert.Equal(t, handlerPool, svc.handlerPool)
}

func TestHandlerServiceRegister(t *testing.T) {
	handlerPool := NewHandlerPool()
	svc := NewHandlerService(nil, nil, 0, 0, nil, nil, nil, nil, nil, handlerPool)
	err := svc.Register(&MyComp{}, []component.Option{})
	assert.NoError(t, err)
	assert.Len(t, svc.services, 1)
	val, ok := svc.services["MyComp"]
	assert.True(t, ok)
	assert.NotNil(t, val)
	val2, ok := handlerPool.GetHandlers()["MyComp.Handler1"]
	assert.True(t, ok)
	assert.NotNil(t, val2)
	val2, ok = handlerPool.GetHandlers()["MyComp.Handler2"]
	assert.True(t, ok)
	assert.NotNil(t, val2)
	val2, ok = handlerPool.GetHandlers()["MyComp.HandlerRawRaw"]
	assert.True(t, ok)
	assert.NotNil(t, val2)
}

func TestHandlerServiceRegisterFailsIfRegisterTwice(t *testing.T) {
	handlerPool := NewHandlerPool()
	svc := NewHandlerService(nil, nil, 0, 0, nil, nil, nil, nil, nil, handlerPool)
	err := svc.Register(&MyComp{}, []component.Option{})
	assert.NoError(t, err)
	err = svc.Register(&MyComp{}, []component.Option{})
	assert.Contains(t, err.Error(), "handler: service already defined")
}

func TestHandlerServiceRegisterFailsIfNoHandlerMethods(t *testing.T) {
	handlerPool := NewHandlerPool()
	svc := NewHandlerService(nil, nil, 0, 0, nil, nil, nil, nil, nil, handlerPool)
	err := svc.Register(&NoHandlerRemoteComp{}, []component.Option{})
	assert.Equal(t, errors.New("type NoHandlerRemoteComp has no exported methods of handler type"), err)
}

func TestHandlerServiceRegisterRejectsReservedSysName(t *testing.T) {
	tests := []struct {
		name    string
		options []component.Option
	}{
		{name: "explicit lowercase", options: []component.Option{component.WithName("sys")}},
		{name: "explicit mixed case", options: []component.Option{component.WithName("sYs")}},
		{name: "explicit surrounding whitespace", options: []component.Option{component.WithName(" Sys ")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handlerPool := NewHandlerPool()
			svc := NewHandlerService(nil, nil, 0, 0, nil, nil, nil, nil, nil, handlerPool)

			err := svc.Register(&MyComp{}, test.options)

			assert.Error(t, err)
			assert.Empty(t, svc.services)
			assert.Empty(t, handlerPool.GetHandlers())
		})
	}
}

func TestHandlerServiceProcessMessage(t *testing.T) {
	tables := []struct {
		name    string
		msg     *message.Message
		wantErr bool
	}{
		{"failed_decode", &message.Message{ID: 1, Route: "k.k.k.k"}, true},
		{"local_process", &message.Message{ID: 1, Route: "k.k"}, false},
		{"remote_process", &message.Message{ID: 1, Route: "backend.k.k"}, false},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			sv := &cluster.Server{ID: "frontend-1", Type: "frontend"}
			handlerPool := NewHandlerPool()
			svc := NewHandlerService(nil, nil, 1, 1, sv, &RemoteService{}, nil, nil, nil, handlerPool)

			mockSession := mocks.NewMockSession(ctrl)
			mockAgent := agentmocks.NewMockAgent(ctrl)
			mockAgent.EXPECT().GetSession().Return(mockSession).AnyTimes()

			var scheduledCtx context.Context
			if table.wantErr {
				mockAgent.EXPECT().AnswerWithError(gomock.Any(), table.msg.ID, gomock.Any()).Do(
					func(_ context.Context, _ uint, err error) {
						assert.True(t, apierrors.IsBadRequest(err))
						assert.ErrorContains(t, err, "invalid route")
					},
				).Times(1)
			} else {
				mockSession.EXPECT().Go(gomock.Any(), gomock.Any()).Do(
					func(ctx context.Context, task func(context.Context)) {
						scheduledCtx = ctx
						assert.NotNil(t, task)
					},
				).Return(nil).Times(1)
			}

			svc.processMessage(mockAgent, table.msg)

			if !table.wantErr {
				assert.NotNil(t, pcontext.GetFromPropagateCtx(scheduledCtx, constants.StartTimeKey))
				assert.Equal(t, table.msg.Route, pcontext.GetFromPropagateCtx(scheduledCtx, constants.RouteKey))
			}
		})
	}
}

func TestHandlerServiceProcessMessageReportsSubmissionError(t *testing.T) {
	ctrl := gomock.NewController(t)
	submitErr := errors.New("stateful pool rejected task")
	msg := &message.Message{ID: 42, Type: message.Request, Route: "local.handle"}
	mockSession := mocks.NewMockSession(ctrl)
	mockSession.EXPECT().Go(gomock.Any(), gomock.Any()).Return(submitErr).Times(1)
	mockAgent := agentmocks.NewMockAgent(ctrl)
	mockAgent.EXPECT().GetSession().Return(mockSession).AnyTimes()
	mockAgent.EXPECT().AnswerWithError(gomock.Any(), msg.ID, gomock.Any()).Do(
		func(_ context.Context, _ uint, err error) {
			assert.True(t, apierrors.IsInternalServer(err))
			assert.ErrorIs(t, err, submitErr)
			assert.Equal(t, "task submission failed", apierrors.FromError(err).Status.Message)
		},
	).Times(1)
	svc := NewHandlerService(
		nil,
		nil,
		0,
		0,
		&cluster.Server{ID: "frontend-1", Type: "frontend"},
		&RemoteService{},
		nil,
		nil,
		nil,
		NewHandlerPool(),
	)

	svc.processMessage(mockAgent, msg)
}

func TestHandlerServiceDrainTasksWaitsForAcceptedTask(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockSession := mocks.NewMockSession(ctrl)
	started := make(chan struct{})
	release := make(chan struct{})
	mockSession.EXPECT().Go(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, task func(context.Context)) error {
			go task(ctx)
			return nil
		},
	)

	svc := NewHandlerService(nil, nil, 0, 0, nil, nil, nil, nil, nil, NewHandlerPool())
	require.NoError(t, svc.submitSessionTask(mockSession, context.Background(), func(context.Context) {
		close(started)
		<-release
	}))
	<-started

	drained := make(chan struct{})
	go func() {
		svc.DrainTasks()
		close(drained)
	}()
	require.Eventually(t, func() bool {
		svc.taskMu.Lock()
		defer svc.taskMu.Unlock()
		return svc.tasksClosed
	}, time.Second, time.Millisecond)
	select {
	case <-drained:
		t.Fatal("DrainTasks returned while an accepted task was running")
	default:
	}

	close(release)
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("DrainTasks did not return after the accepted task completed")
	}
	assert.ErrorIs(t, svc.submitSessionTask(mockSession, context.Background(), func(context.Context) {}), ErrHandlerServiceStopped)
}

func TestHandlerServiceDrainTasksCancelsAcceptedTaskContext(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockSession := mocks.NewMockSession(ctrl)
	started := make(chan struct{})
	canceled := make(chan struct{})
	mockSession.EXPECT().Go(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, task func(context.Context)) error {
			go task(ctx)
			return nil
		},
	)

	svc := NewHandlerService(nil, nil, 0, 0, nil, nil, nil, nil, nil, NewHandlerPool())
	require.NoError(t, svc.submitSessionTask(mockSession, context.Background(), func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(canceled)
	}))
	<-started

	drained := make(chan struct{})
	go func() {
		svc.DrainTasks()
		close(drained)
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("DrainTasks did not cancel the accepted task context")
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("DrainTasks did not return after the canceled task exited")
	}
}

func TestHandlerServiceProcessNotifyLogsSubmissionErrorWithoutResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	submitErr := errors.New("stateful pool rejected notify")
	msg := &message.Message{Type: message.Notify, Route: "local.handle"}
	mockSession := mocks.NewMockSession(ctrl)
	mockSession.EXPECT().Go(gomock.Any(), gomock.Any()).Return(submitErr).Times(1)
	mockAgent := agentmocks.NewMockAgent(ctrl)
	mockAgent.EXPECT().GetSession().Return(mockSession).AnyTimes()
	mockAgent.EXPECT().AnswerWithError(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	svc := NewHandlerService(
		nil,
		nil,
		0,
		0,
		&cluster.Server{ID: "frontend-1", Type: "frontend"},
		&RemoteService{},
		nil,
		nil,
		nil,
		NewHandlerPool(),
	)

	svc.processMessage(mockAgent, msg)
}

func TestHandlerServiceProcessMessageRejectsReservedSysRoutes(t *testing.T) {
	tables := []struct {
		name    string
		route   string
		msgType message.Type
	}{
		{"local_request", "sys.kick", message.Request},
		{"local_notify", "sys.pushsession", message.Notify},
		{"remote_request", "backend.sys.bindsession", message.Request},
		{"remote_notify", "backend.sys.flushsession", message.Notify},
		{"default_type_name", "Sys.kick", message.Request},
		{"mixed_case", "sYs.pushsession", message.Request},
		{"surrounding_whitespace", " Sys .bindsession", message.Request},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockSession := mocks.NewMockSession(ctrl)
			mockSession.EXPECT().Go(gomock.Any(), gomock.Any()).Times(0)

			msg := &message.Message{ID: 42, Type: table.msgType, Route: table.route}
			mockAgent := agentmocks.NewMockAgent(ctrl)
			mockAgent.EXPECT().GetSession().Return(mockSession).Times(1)
			mockAgent.EXPECT().AnswerWithError(gomock.Any(), msg.ID, gomock.Any()).Do(
				func(_ context.Context, _ uint, err error) {
					assert.True(t, apierrors.IsNotFound(err))
					assert.ErrorContains(t, err, table.route)
				},
			).Times(1)

			svc := NewHandlerService(
				nil,
				nil,
				0,
				0,
				&cluster.Server{ID: "frontend-1", Type: "frontend"},
				&RemoteService{},
				nil,
				nil,
				nil,
				NewHandlerPool(),
			)

			svc.processMessage(mockAgent, msg)
		})
	}
}

func TestHandlerServiceLocalProcess(t *testing.T) {
	tObj := &MyComp{}
	m, ok := reflect.TypeOf(tObj).MethodByName("HandlerRawRaw")
	assert.True(t, ok)
	assert.NotNil(t, m)
	rt := route.NewRoute("", uuid.New().String(), uuid.New().String())
	handlerPool := NewHandlerPool()
	handlerPool.handlers[rt.Short()] = &component.Handler{Receiver: reflect.ValueOf(tObj), Method: m, Type: m.Type.In(2), IsRawArg: true}

	tables := []struct {
		name    string
		msg     *message.Message
		rt      *route.Route
		wantErr bool
	}{
		{"process_handler_msg_err", &message.Message{}, route.NewRoute("bla", "bla", "bla"), true},
		{"success", &message.Message{ID: 1, Data: []byte(`["ok"]`)}, rt, false},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockSession := mocks.NewMockSession(ctrl)
			mockSession.EXPECT().UID().Return("uid").Times(1)

			mockAgent := agentmocks.NewMockAgent(ctrl)
			mockAgent.EXPECT().GetSession().Return(mockSession).AnyTimes()

			svc := NewHandlerService(nil, nil, 1, 1, nil, nil, nil, nil, pipeline.NewHandlerHooks(), handlerPool)

			ctx := context.Background()

			if table.wantErr {
				mockAgent.EXPECT().AnswerWithError(gomock.Any(), table.msg.ID, gomock.Any()).Do(
					func(_ context.Context, _ uint, err error) {
						assert.True(t, apierrors.IsNotFound(err))
					},
				)
			} else {
				mockSession.EXPECT().ResponseMID(ctx, table.msg.ID, table.msg.Data).Return(nil).Times(1)
				mockSession.EXPECT().ID().Return(int64(1)).Times(1)
			}

			svc.localProcess(ctx, mockAgent, table.rt, table.msg)
		})
	}
}

func TestHandlerServiceProcessPacketHandshake(t *testing.T) {
	tables := []struct {
		name      string
		packet    *packet.Packet
		validator func(data *session.HandshakeData) error
		errStr    string
	}{
		{"invalid_handshake_data", &packet.Packet{Type: packet.Handshake, Data: []byte("asiodjasd")}, nil, "invalid handshake data"},
		{"validator_error", &packet.Packet{Type: packet.Handshake, Data: []byte(`{"sys":{"platform":"mac"}}`)}, func(data *session.HandshakeData) error { return errors.New("validation failed") }, "handshake validation failed"},
		{"valid_handshake_data", &packet.Packet{Type: packet.Handshake, Data: []byte(`{"sys":{"platform":"mac"}}`)}, func(data *session.HandshakeData) error { return nil }, ""},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockSession := mocks.NewMockSession(ctrl)
			mockSession.EXPECT().ID().Return(int64(1)).AnyTimes()

			mockAgent := agentmocks.NewMockAgent(ctrl)
			mockAgent.EXPECT().GetSession().Return(mockSession).AnyTimes()

			if table.validator != nil {
				mockSession.EXPECT().ValidateHandshake(gomock.Any()).DoAndReturn(func(data *session.HandshakeData) error {
					return table.validator(data)
				}).Times(1)
			}

			if table.errStr == "" {
				handshakeData := &session.HandshakeData{}
				_ = encjson.Unmarshal(table.packet.Data, handshakeData)
				mockAgent.EXPECT().GetStatus().Return(constants.StatusStart)
				mockAgent.EXPECT().TransitionStatus(constants.StatusStart, constants.StatusHandshakeValidated).Return(true)
				mockAgent.EXPECT().RemoteAddr().Return(&mockAddr{})
				mockAgent.EXPECT().SendHandshakeResponse().Return(nil).Times(1)
				mockAgent.EXPECT().SetLastAt().Times(1)

				mockSession.EXPECT().SetHandshakeData(handshakeData).Times(1)
			} else {
				mockAgent.EXPECT().GetStatus().Return(constants.StatusStart)
				mockAgent.EXPECT().SendHandshakeErrorResponse().Return(nil).Times(1)
				mockSession.EXPECT().Close(nil).Times(1)
			}

			handlerPool := NewHandlerPool()
			svc := NewHandlerService(nil, nil, 1, 1, nil, nil, nil, nil, pipeline.NewHandlerHooks(), handlerPool)
			err := svc.processPacket(mockAgent, table.packet)
			if table.errStr == "" {
				assert.Nil(t, err)
			} else {
				assert.NotNil(t, err)
				assert.Contains(t, err.Error(), table.errStr)
			}
		})
	}
}

func TestHandlerServiceRejectsDuplicateOrOutOfOrderHandshake(t *testing.T) {
	for _, status := range []int32{constants.StatusHandshakeValidated, constants.StatusWorking, constants.StatusClosed} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockSession := mocks.NewMockSession(ctrl)
			mockSession.EXPECT().Close(nil)
			mockAgent := agentmocks.NewMockAgent(ctrl)
			mockAgent.EXPECT().GetStatus().Return(status)
			mockAgent.EXPECT().GetSession().Return(mockSession)

			svc := NewHandlerService(nil, nil, 1, 1, nil, nil, nil, nil, nil, NewHandlerPool())
			err := svc.processPacket(mockAgent, &packet.Packet{Type: packet.Handshake, Data: []byte(`{}`)})
			assert.ErrorContains(t, err, "unexpected packet type")
		})
	}
}

func TestHandlerServiceProcessPacketHandshakeAck(t *testing.T) {
	tables := []struct {
		name    string
		status  int32
		wantErr bool
	}{
		{"ack_first", constants.StatusStart, true},
		{"valid_ack", constants.StatusHandshakeValidated, false},
		{"duplicate_ack", constants.StatusWorking, true},
		{"ack_after_close", constants.StatusClosed, true},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockSession := mocks.NewMockSession(ctrl)
			mockAgent := agentmocks.NewMockAgent(ctrl)
			if table.wantErr {
				mockAgent.EXPECT().TransitionStatus(constants.StatusHandshakeValidated, constants.StatusWorking).Return(false)
				mockAgent.EXPECT().GetStatus().Return(table.status)
				mockAgent.EXPECT().GetSession().Return(mockSession)
				mockSession.EXPECT().Close(nil)
			} else {
				mockAgent.EXPECT().TransitionStatus(constants.StatusHandshakeValidated, constants.StatusWorking).Return(true)
				mockAgent.EXPECT().GetSession().Return(mockSession)
				mockSession.EXPECT().ID().Return(int64(1))
				mockAgent.EXPECT().RemoteAddr().Return(&mockAddr{})
				mockAgent.EXPECT().SetLastAt()
			}

			svc := NewHandlerService(nil, nil, 1, 1, nil, nil, nil, nil, nil, NewHandlerPool())
			err := svc.processPacket(mockAgent, &packet.Packet{Type: packet.HandshakeAck})
			if table.wantErr {
				assert.ErrorContains(t, err, "unexpected packet type")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestHandlerServiceProcessPacketHeartbeat(t *testing.T) {
	tables := []struct {
		name       string
		packetType packet.Type
		status     int32
		wantErr    bool
	}{
		{"heartbeat_first", packet.Heartbeat, constants.StatusStart, true},
		{"heartbeat_ack_first", packet.HeartbeatAck, constants.StatusStart, true},
		{"heartbeat_before_ack", packet.Heartbeat, constants.StatusHandshakeValidated, true},
		{"heartbeat_ack_before_ack", packet.HeartbeatAck, constants.StatusHandshakeValidated, true},
		{"working_heartbeat", packet.Heartbeat, constants.StatusWorking, false},
		{"working_heartbeat_ack", packet.HeartbeatAck, constants.StatusWorking, false},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockAgent := agentmocks.NewMockAgent(ctrl)
			mockAgent.EXPECT().GetStatus().Return(table.status)
			if table.wantErr {
				mockSession := mocks.NewMockSession(ctrl)
				mockAgent.EXPECT().GetSession().Return(mockSession)
				mockSession.EXPECT().Close(nil)
			} else {
				if table.packetType == packet.Heartbeat {
					mockAgent.EXPECT().SendHeartbeatResponse(int64(0)).Return(nil)
				}
				mockAgent.EXPECT().SetLastAt()
			}

			svc := NewHandlerService(nil, nil, 1, 1, nil, nil, nil, nil, nil, NewHandlerPool())
			err := svc.processPacket(mockAgent, &packet.Packet{Type: table.packetType})
			if table.wantErr {
				assert.ErrorContains(t, err, "unexpected packet type")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestHandlerServiceRejectsInvalidHeartbeatPayloadLength(t *testing.T) {
	for _, packetType := range []packet.Type{packet.Heartbeat, packet.HeartbeatAck} {
		for _, size := range []int{1, 7, 9} {
			t.Run(fmt.Sprintf("type_%d_size_%d", packetType, size), func(t *testing.T) {
				ctrl := gomock.NewController(t)
				mockAgent := agentmocks.NewMockAgent(ctrl)
				mockAgent.EXPECT().GetStatus().Return(constants.StatusWorking)

				svc := NewHandlerService(nil, nil, 1, 1, nil, nil, nil, nil, nil, NewHandlerPool())
				var err error
				require.NotPanics(t, func() {
					err = svc.processPacket(mockAgent, &packet.Packet{
						Type: packetType,
						Data: make([]byte, size),
					})
				})
				assert.ErrorContains(t, err, "invalid heartbeat payload length")
			})
		}
	}
}

func TestHandlerServiceProcessPacketData(t *testing.T) {
	msgID := uint(1)
	msg := &message.Message{Type: message.Request, ID: msgID, Data: []byte("ok")}
	messageEncoder := message.NewMessagesEncoder(false)
	encodedMsg, err := messageEncoder.Encode(msg)
	assert.NoError(t, err)
	tables := []struct {
		name         string
		packet       *packet.Packet
		socketStatus int32
		errStr       string
	}{
		{"data_first", &packet.Packet{Type: packet.Data, Data: []byte("ok")}, constants.StatusStart, "unexpected packet type"},
		{"data_before_ack", &packet.Packet{Type: packet.Data, Data: []byte("ok")}, constants.StatusHandshakeValidated, "unexpected packet type"},
		{"data_after_close", &packet.Packet{Type: packet.Data, Data: []byte("ok")}, constants.StatusClosed, "unexpected packet type"},
		{"failed_decode", &packet.Packet{Type: packet.Data, Data: []byte("ok")}, constants.StatusWorking, "wrong message type"},
		{"success", &packet.Packet{Type: packet.Data, Data: encodedMsg}, constants.StatusWorking, ""},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockSession := mocks.NewMockSession(ctrl)

			mockAgent := agentmocks.NewMockAgent(ctrl)

			mockAgent.EXPECT().GetStatus().Return(table.socketStatus)
			if table.socketStatus != constants.StatusWorking {
				mockAgent.EXPECT().GetSession().Return(mockSession)
				mockSession.EXPECT().Close(nil)
			} else {
				if table.errStr == "" {
					mockAgent.EXPECT().GetSession().Return(mockSession).Times(1)

					mockAgent.EXPECT().AnswerWithError(gomock.Any(), msgID, gomock.Any()).Times(1)
					mockAgent.EXPECT().SetLastAt().Times(1)
				}
			}

			handlerPool := NewHandlerPool()
			svc := NewHandlerService(nil, nil, 1, 1, &cluster.Server{}, nil, nil, nil, nil, handlerPool)
			err := svc.processPacket(mockAgent, table.packet)
			if table.errStr != "" {
				assert.Contains(t, err.Error(), table.errStr)
			}
		})
	}
}

func TestHandlerServiceHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	packetEncoder := codec.NewPomeloPacketEncoder()
	packetDecoder := codec.NewPomeloPacketDecoder()
	handshakeBuffer := `{"sys":{"platform":"mac","libVersion":"0.3.5-release","clientBuildNumber":"20","clientVersion":"2.1"},"user":{"age":30}}`
	bbb, err := packetEncoder.Encode(packet.Handshake, []byte(handshakeBuffer))
	assert.NoError(t, err)

	mockSerializer := serializemocks.NewMockSerializer(ctrl)

	mockConn := connmock.NewMockPlayerConn(ctrl)

	mockAgent := agentmocks.NewMockAgent(ctrl)
	mockAgentFactory := agentmocks.NewMockAgentFactory(ctrl)
	mockAgentFactory.EXPECT().CreateAgent(mockConn).Return(mockAgent).Times(1)

	var wg sync.WaitGroup
	wg.Add(4)
	defer wg.Wait()

	mockAgent.EXPECT().Handle().Do(func() {
		wg.Done()
	})

	mockAgent.EXPECT().SendHandshakeResponse().Return(nil)

	mockSession := mocks.NewMockSession(ctrl)
	mockSession.EXPECT().SetHandshakeData(gomock.Any()).Times(1)
	mockSession.EXPECT().ValidateHandshake(gomock.Any()).Times(1)
	mockSession.EXPECT().UID().Return("uid").Times(1)
	mockSession.EXPECT().ID().Return(int64(1)).Times(2)
	mockSession.EXPECT().Close(nil)

	mockAgent.EXPECT().String().Return("")
	mockAgent.EXPECT().GetStatus().Return(constants.StatusStart)
	mockAgent.EXPECT().TransitionStatus(constants.StatusStart, constants.StatusHandshakeValidated).Return(true)
	mockAgent.EXPECT().GetSession().Return(mockSession).Times(6)
	mockAgent.EXPECT().RemoteAddr().Return(&mockAddr{}).AnyTimes()
	mockAgent.EXPECT().SetLastAt().Do(func() {
		wg.Done()
	})

	firstCall := mockConn.EXPECT().GetNextMessage().Return(bbb, nil).Do(func() {
		wg.Done()
	})

	mockConn.EXPECT().GetNextMessage().Return(nil, errors.New("die")).Do(func() {
		wg.Done()
	}).After(firstCall)

	mockConn.EXPECT().Close().MaxTimes(1)

	handlerPool := NewHandlerPool()
	svc := NewHandlerService(packetDecoder, mockSerializer, 1, 1, nil, nil, mockAgentFactory, nil, pipeline.NewHandlerHooks(), handlerPool)
	svc.Handle(mockConn)
}

func TestHandlerServiceHandleTreatsNetClosedAsExpected(t *testing.T) {
	ctrl := gomock.NewController(t)
	previousZap := logger.Zap
	core, observed := observer.New(zapcore.DebugLevel)
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousZap })

	mockConn := connmock.NewMockPlayerConn(ctrl)
	mockAgent := agentmocks.NewMockAgent(ctrl)
	mockAgentFactory := agentmocks.NewMockAgentFactory(ctrl)
	mockSession := mocks.NewMockSession(ctrl)

	mockAgentFactory.EXPECT().CreateAgent(mockConn).Return(mockAgent)
	mockAgent.EXPECT().Handle()
	mockAgent.EXPECT().String().Return("")
	mockAgent.EXPECT().GetSession().Return(mockSession).Times(3)
	mockSession.EXPECT().Close(nil)
	mockSession.EXPECT().ID().Return(int64(1))
	mockSession.EXPECT().UID().Return("uid")
	mockConn.EXPECT().GetNextMessage().Return(nil, net.ErrClosed)

	svc := NewHandlerService(nil, nil, 1, 1, nil, nil, mockAgentFactory, nil, pipeline.NewHandlerHooks(), NewHandlerPool())
	svc.Handle(mockConn)

	entries := observed.FilterMessage("Connection no longer available while reading next available message").All()
	require.Len(t, entries, 1)
	assert.Equal(t, zap.DebugLevel, entries[0].Level)
}
