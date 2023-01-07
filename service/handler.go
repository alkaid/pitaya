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

package service

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/alkaid/goerrors/apierrors"

	"github.com/topfreegames/pitaya/v2/acceptor"
	"github.com/topfreegames/pitaya/v2/co"
	"github.com/topfreegames/pitaya/v2/pipeline"

	"github.com/topfreegames/pitaya/v2/agent"
	"github.com/topfreegames/pitaya/v2/cluster"
	"github.com/topfreegames/pitaya/v2/component"
	"github.com/topfreegames/pitaya/v2/conn/codec"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/conn/packet"
	"github.com/topfreegames/pitaya/v2/constants"
	pcontext "github.com/topfreegames/pitaya/v2/context"
	"github.com/topfreegames/pitaya/v2/docgenerator"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/metrics"
	"github.com/topfreegames/pitaya/v2/route"
	"github.com/topfreegames/pitaya/v2/serialize"
	"github.com/topfreegames/pitaya/v2/session"
	"github.com/topfreegames/pitaya/v2/timer"
	"github.com/topfreegames/pitaya/v2/tracing"
)

var (
	handlerType = "handler"
)

type (
	// HandlerService service
	HandlerService struct {
		baseService
		chLocalProcess   chan unhandledMessage // channel of messages that will be processed locally
		chRemoteProcess  chan unhandledMessage // channel of messages that will be processed remotely
		decoder          codec.PacketDecoder   // binary decoder
		remoteService    *RemoteService
		serializer       serialize.Serializer          // message serializer
		server           *cluster.Server               // server obj
		services         map[string]*component.Service // all registered service
		metricsReporters []metrics.Reporter
		agentFactory     agent.AgentFactory
		handlerPool      *HandlerPool
		handlers         map[string]*component.Handler // all handler method
	}

	unhandledMessage struct {
		ctx   context.Context
		agent agent.Agent
		route *route.Route
		msg   *message.Message
	}
)

// NewHandlerService creates and returns a new handler service
func NewHandlerService(
	packetDecoder codec.PacketDecoder,
	serializer serialize.Serializer,
	localProcessBufferSize int,
	remoteProcessBufferSize int,
	server *cluster.Server,
	remoteService *RemoteService,
	agentFactory agent.AgentFactory,
	metricsReporters []metrics.Reporter,
	handlerHooks *pipeline.HandlerHooks,
	handlerPool *HandlerPool,
) *HandlerService {
	h := &HandlerService{
		services:         make(map[string]*component.Service),
		chLocalProcess:   make(chan unhandledMessage, localProcessBufferSize),
		chRemoteProcess:  make(chan unhandledMessage, remoteProcessBufferSize),
		decoder:          packetDecoder,
		serializer:       serializer,
		server:           server,
		remoteService:    remoteService,
		agentFactory:     agentFactory,
		metricsReporters: metricsReporters,
		handlerPool:      handlerPool,
		handlers:         make(map[string]*component.Handler),
	}

	h.handlerHooks = handlerHooks

	return h
}

// Dispatch message to corresponding logic handler
func (h *HandlerService) Dispatch(thread int) {
	// TODO: This timer is being stopped multiple times, it probably doesn't need to be stopped here
	defer timer.GlobalTicker.Stop()

	for {
		// Calls to remote servers block calls to local server
		select {
		case lm := <-h.chLocalProcess:
			metrics.ReportMessageProcessDelayFromCtx(lm.ctx, h.metricsReporters, "local")
			h.localProcess(lm.ctx, lm.agent, lm.route, lm.msg)

		case rm := <-h.chRemoteProcess:
			metrics.ReportMessageProcessDelayFromCtx(rm.ctx, h.metricsReporters, "remote")
			h.remoteService.remoteProcess(rm.ctx, nil, rm.agent, rm.route, rm.msg)

		case <-timer.GlobalTicker.C: // execute cron task
			timer.Cron()

		case t := <-timer.Manager.ChCreatedTimer: // new Timers
			timer.AddTimer(t)

		case id := <-timer.Manager.ChClosingTimer: // closing Timers
			timer.RemoveTimer(id)
		}
	}
}

// Register registers components
func (h *HandlerService) Register(comp component.Component, opts []component.Option) error {
	s := component.NewService(comp, opts)

	if _, ok := h.services[s.Name]; ok {
		return fmt.Errorf("handler: service already defined: %s", s.Name)
	}

	if err := s.ExtractHandler(); err != nil {
		return err
	}

	// register all handlers
	h.services[s.Name] = s
	for name, handler := range s.Handlers {
		h.handlerPool.Register(s.Name, name, handler)
	}
	return nil
}

// RegisterInterceptor 注册拦截分发器,优先级别高于 component.Remote
//
//	@receiver h
//	@param serviceName
//	@param interceptor
func (h *HandlerService) RegisterInterceptor(serviceName string, interceptor *component.Interceptor) {
	h.handlerPool.RegisterInterceptor(serviceName, interceptor)
}

// Handle handles messages from a conn
func (h *HandlerService) Handle(conn acceptor.PlayerConn) {
	// create a client agent and startup write goroutine
	a := h.agentFactory.CreateAgent(conn)

	// startup agent goroutine
	co.Go(func() { a.Handle() })

	logger.Log.Debugf("New session established: %s", a.String())

	// guarantee agent related resource is destroyed
	defer func() {
		a.GetSession().Close(nil)
		logger.Log.Debugf("Session read goroutine exit, SessionID=%d, UID=%s", a.GetSession().ID(), a.GetSession().UID())
	}()

	for {
		msg, err := conn.GetNextMessage()

		if err != nil {
			if !errors.Is(err, constants.ErrConnectionClosed) {
				logger.Zap.Warn("Error reading next available message", zap.Error(err))
			}

			return
		}

		packets, err := h.decoder.Decode(msg)
		if err != nil {
			logger.Zap.Error("Failed to decode message", zap.Error(err))
			return
		}

		if len(packets) < 1 {
			logger.Zap.Warn("Read no packets, data", zap.Error(err))
			continue
		}

		// process all packet
		for i := range packets {
			if err := h.processPacket(a, packets[i]); err != nil {
				logger.Zap.Error("Failed to process packet", zap.Error(err))
				return
			}
		}
	}
}

func (h *HandlerService) processPacket(a agent.Agent, p *packet.Packet) error {
	switch p.Type {
	case packet.Handshake:
		logger.Log.Debug("Received handshake packet")
		if err := a.SendHandshakeResponse(); err != nil {
			logger.Zap.Error("Error sending handshake response", zap.Error(err))
			return err
		}
		logger.Log.Debugf("Session handshake Id=%d, Remote=%s", a.GetSession().ID(), a.RemoteAddr())

		// Parse the json sent with the handshake by the client
		handshakeData := &session.HandshakeData{}
		err := json.Unmarshal(p.Data, handshakeData)
		if err != nil {
			a.SetStatus(constants.StatusClosed)
			return fmt.Errorf("Invalid handshake data. Id=%d", a.GetSession().ID())
		}

		a.GetSession().SetHandshakeData(handshakeData)
		a.SetStatus(constants.StatusHandshake)
		// ipversion 暂时用不到
		// err = a.GetSession().Set(constants.IPVersionKey, a.IPVersion())
		// if err != nil {
		// 	logger.Log.Warnf("failed to save ip version on session: %q\n", err)
		// }

		logger.Log.Debug("Successfully saved handshake data")

	case packet.HandshakeAck:
		a.SetStatus(constants.StatusWorking)
		logger.Log.Debugf("Receive handshake ACK Id=%d, Remote=%s", a.GetSession().ID(), a.RemoteAddr())

	case packet.Data:
		if a.GetStatus() < constants.StatusWorking {
			return fmt.Errorf("receive data on socket which is not yet ACK, session will be closed immediately, remote=%s",
				a.RemoteAddr().String())
		}

		msg, err := message.Decode(p.Data)
		if err != nil {
			return err
		}
		h.processMessage(a, msg)

	case packet.Heartbeat:
		var rawUnixMillTime int64 = 0
		if len(p.Data) > 0 {
			rawUnixMillTime = int64(binary.BigEndian.Uint64(p.Data))
		}
		if err := a.SendHeartbeatResponse(rawUnixMillTime); err != nil {
			logger.Zap.Error("Error sending handshake response", zap.Error(err))
			return err
		}

	case packet.HeartbeatAck:
		var rawUnixMillTime int64 = 0
		if len(p.Data) > 0 {
			rawUnixMillTime = int64(binary.BigEndian.Uint64(p.Data))
			if rawUnixMillTime > 0 {
				lastTime := time.UnixMilli(rawUnixMillTime)
				latency := time.Now().Sub(lastTime)
				if latency > time.Millisecond*100 {
					logger.Zap.Debug("client latency too much", zap.Int64("id", a.GetSession().ID()), zap.String("uid", a.GetSession().UID()), zap.Duration("latency", latency))
				}
				// TODO 上报监控
			}
		}
	}

	a.SetLastAt()
	return nil
}

func (h *HandlerService) processMessage(a agent.Agent, msg *message.Message) {
	requestID := strconv.Itoa(int(msg.ID))
	ctx := pcontext.AddListToPropagateCtx(context.Background(), constants.StartTimeKey, time.Now().UnixNano(), constants.RouteKey, msg.Route, constants.RequestIDKey, requestID)
	ctx = context.WithValue(ctx, constants.SessionCtxKey, a.GetSession())

	r, err := route.Decode(msg.Route)
	if err != nil {
		logger.Zap.Error("Failed to decode route", zap.Error(err))
		a.AnswerWithError(ctx, msg.ID, apierrors.BadRequest("", "Failed to decode route", "").WithCause(err))
		return
	}

	spanInfo := &tracing.SpanInfo{
		RpcSystem: "pitaya",
		IsClient:  false,
		Route:     r,
		PeerType:  r.SvType,
		LocalID:   h.server.ID,
		LocalType: h.server.Type,
		RequestID: requestID,
	}
	ctx = tracing.RPCStartSpan(ctx, spanInfo)

	if r.SvType == "" {
		r.SvType = h.server.Type
	}

	message := unhandledMessage{
		ctx:   ctx,
		agent: a,
		route: r,
		msg:   msg,
	}
	if r.SvType == h.server.Type {
		h.chLocalProcess <- message
	} else {
		if h.remoteService != nil {
			h.chRemoteProcess <- message
		} else {
			logger.Log.Warnf("request made to another server type but no remoteService running")
		}
	}
}

func (h *HandlerService) localProcess(ctx context.Context, a agent.Agent, route *route.Route, msg *message.Message) {
	var mid uint
	switch msg.Type {
	case message.Request:
		mid = msg.ID
	case message.Notify:
		mid = 0
	}
	// 根据session数据决策派发线程id
	sess := a.GetSession()
	// 派发给session绑定的线程
	sess.GoBySession(func() {
		ret, err := h.handlerPool.ProcessHandlerMessage(ctx, route, h.serializer, h.handlerHooks, a.GetSession(), msg.Data, msg.Type, false)
		if msg.Type != message.Notify {
			if err != nil {
				logger.Zap.Error("Failed to process handler message", zap.Error(err))
				a.AnswerWithError(ctx, mid, err)
			} else {
				err := a.GetSession().ResponseMID(ctx, mid, ret)
				if err != nil {
					tracing.FinishSpan(ctx, err)
					metrics.ReportTimingFromCtx(ctx, h.metricsReporters, handlerType, err)
				}
			}
		} else {
			if err != nil {
				logger.Zap.Error("Failed to process handler message", zap.Error(err))
				a.AnswerWithError(ctx, mid, err)
			}
			metrics.ReportTimingFromCtx(ctx, h.metricsReporters, handlerType, nil)
			tracing.FinishSpan(ctx, err)
		}
	})
}

// DumpServices outputs all registered services
func (h *HandlerService) DumpServices() {
	handlers := h.handlerPool.GetHandlers()
	for name := range handlers {
		logger.Log.Infof("registered handler %s, isRawArg: %v", name, handlers[name].IsRawArg)
	}
}

// Docs returns documentation for handlers
func (h *HandlerService) Docs(getPtrNames bool) (map[string]interface{}, error) {
	if h == nil {
		return map[string]interface{}{}, nil
	}
	return docgenerator.HandlersDocs(h.server.Type, h.services, getPtrNames)
}
