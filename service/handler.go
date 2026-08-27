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
	"net"
	"sync"
	"time"

	"github.com/nats-io/nuid"
	"go.uber.org/zap"

	"github.com/alkaid/goerrors/apierrors"

	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/pipeline"

	"github.com/alkaid/pitaya/v2/agent"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/docgenerator"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/serialize"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/timer"
	"github.com/alkaid/pitaya/v2/tracing"
	"github.com/alkaid/pitaya/v2/util"
)

var (
	handlerType                  = "handler"
	ErrHandlerServiceStopped     = errors.New("handler service is stopping")
	ErrHandlerServiceUnavailable = errors.New("handler service is not configured")
)

type (
	// HandlerService service
	HandlerService struct {
		baseService
		decoder          codec.PacketDecoder // binary decoder
		remoteService    *RemoteService
		serializer       serialize.Serializer          // message serializer
		server           *cluster.Server               // server obj
		services         map[string]*component.Service // all registered service
		metricsReporters []metrics.Reporter
		agentFactory     agent.AgentFactory
		handlerPool      *HandlerPool
		handlers         map[string]*component.Handler // all handler method
		registryMu       sync.RWMutex
		taskMu           sync.Mutex
		taskWG           sync.WaitGroup
		tasksClosed      bool
		taskCtx          context.Context
		cancelTasks      context.CancelFunc
	}

	unhandledMessage struct {
		ctx   context.Context
		agent agent.Agent
		route *route.Route
		msg   *message.Message
	}
)

// NewHandlerService creates and returns a new handler service.
// localProcessBufferSize and remoteProcessBufferSize are retained for source
// compatibility only. They are deprecated and ignored.
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
	_ = localProcessBufferSize
	_ = remoteProcessBufferSize
	taskCtx, cancelTasks := context.WithCancel(context.Background())
	h := &HandlerService{
		services:         make(map[string]*component.Service),
		decoder:          packetDecoder,
		serializer:       serializer,
		server:           server,
		remoteService:    remoteService,
		agentFactory:     agentFactory,
		metricsReporters: metricsReporters,
		handlerPool:      handlerPool,
		handlers:         make(map[string]*component.Handler),
		taskCtx:          taskCtx,
		cancelTasks:      cancelTasks,
	}

	h.handlerHooks = handlerHooks

	return h
}

// Dispatch message to corresponding logic handler
func (h *HandlerService) Dispatch(thread int) {
	h.DispatchContext(context.Background(), thread)
}

// DispatchContext dispatches timer work until the owning App lifecycle ends.
func (h *HandlerService) DispatchContext(ctx context.Context, _ int) {
	timer.Manager.Run(ctx, timer.GlobalTicker.C)
}

// Register registers components
func (h *HandlerService) Register(comp component.Component, opts []component.Option) error {
	prepared, err := h.PrepareRegistrations([]ComponentRegistration{{Component: comp, Options: opts}})
	if err != nil {
		return err
	}
	return h.CommitPreparedRegistrations(prepared)
}

// PrepareRegistrations validates a handler batch against the current registry
// without publishing it.
func (h *HandlerService) PrepareRegistrations(registrations []ComponentRegistration) (*PreparedHandlerRegistrations, error) {
	prepared, err := PrepareHandlerRegistrations(registrations)
	if err != nil {
		return nil, err
	}
	h.registryMu.RLock()
	defer h.registryMu.RUnlock()
	for _, preparedService := range prepared.services {
		if _, ok := h.services[preparedService.Name]; ok {
			return nil, fmt.Errorf("handler: service already defined: %s", preparedService.Name)
		}
	}
	return prepared, nil
}

// CommitPreparedRegistrations publishes a complete prepared batch. The method
// rechecks live registry names so a concurrent runtime registration cannot cause
// a partial startup commit.
func (h *HandlerService) CommitPreparedRegistrations(prepared *PreparedHandlerRegistrations) error {
	if prepared == nil || len(prepared.services) == 0 {
		return nil
	}
	h.registryMu.Lock()
	defer h.registryMu.Unlock()
	if err := h.validatePreparedRegistrationsLocked(prepared); err != nil {
		return err
	}
	h.commitPreparedRegistrationsLocked(prepared)
	return nil
}

func (h *HandlerService) validatePreparedRegistrationsLocked(prepared *PreparedHandlerRegistrations) error {
	for _, preparedService := range prepared.services {
		if _, ok := h.services[preparedService.Name]; ok {
			return fmt.Errorf("handler: service already defined: %s", preparedService.Name)
		}
	}
	return nil
}

func (h *HandlerService) commitPreparedRegistrationsLocked(prepared *PreparedHandlerRegistrations) {
	batch := make(map[string]map[string]*component.Handler, len(prepared.services))
	for _, preparedService := range prepared.services {
		batch[preparedService.Name] = preparedService.Handlers
	}
	h.handlerPool.registerServices(batch)
	for _, preparedService := range prepared.services {
		h.services[preparedService.Name] = preparedService
	}
}

// RegisterInterceptor 注册拦截分发器,优先级别高于 component.Remote
//
//	@receiver h
//	@param serviceName
//	@param interceptor
func (h *HandlerService) RegisterInterceptor(serviceName string, interceptor *component.Interceptor) error {
	return h.handlerPool.RegisterInterceptor(serviceName, interceptor)
}

// Handle handles messages from a conn
func (h *HandlerService) Handle(conn acceptor.PlayerConn) {
	// create a client agent and startup write goroutine
	a := h.agentFactory.CreateAgent(conn)

	// Keep the agent lifecycle inside this connection handler so App shutdown can
	// wait until its final cleanup and logging have completed.
	agentDone := make(chan struct{})
	if err := co.Submit(func() {
		defer close(agentDone)
		a.Handle()
	}); err != nil {
		a.GetSession().Close(nil)
		logger.Zap.Error("Failed to start agent", zap.Error(err))
		return
	}

	logger.Log.Debugf("New session established: %s", a.String())

	// guarantee agent related resource is destroyed
	defer func() {
		a.GetSession().Close(nil)
		<-agentDone
		logger.Log.Debugf("Session read goroutine exit, SessionID=%d, UID=%s", a.GetSession().ID(), a.GetSession().UID())
	}()

	for {
		msg, err := conn.GetNextMessage()

		if err != nil {
			// Check if this is an expected error due to connection being closed
			if errors.Is(err, net.ErrClosed) {
				logger.Zap.Debug("Connection no longer available while reading next available message", zap.Error(err))
			} else if errors.Is(err, constants.ErrConnectionClosed) {
				logger.Zap.Debug("Connection no longer available while reading next available message", zap.Error(err))
			} else {
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

// HandleContext closes the accepted connection when the App ends the
// connection lifecycle. This also covers handlers that were admitted to the
// worker pool but had not created their Agent when shutdown began.
func (h *HandlerService) HandleContext(ctx context.Context, conn acceptor.PlayerConn) {
	if ctx == nil {
		h.Handle(conn)
		return
	}
	stopClose := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	defer stopClose()
	h.Handle(conn)
}

func (h *HandlerService) processPacket(a agent.Agent, p *packet.Packet) error {
	switch p.Type {
	case packet.Handshake:
		if status := a.GetStatus(); status != constants.StatusStart {
			return rejectPacketForStatus(a, p.Type, status, constants.StatusStart)
		}
		logger.Log.Debug("Received handshake packet")

		// Parse the json sent with the handshake by the client
		handshakeData := &session.HandshakeData{}
		if err := json.Unmarshal(p.Data, handshakeData); err != nil {
			defer a.GetSession().Close(nil)
			logger.Zap.Error("Failed to unmarshal handshake data", zap.Error(err))
			if serr := a.SendHandshakeErrorResponse(); serr != nil {
				logger.Zap.Error("Error sending handshake error response", zap.Error(err))
				return err
			}

			return fmt.Errorf("invalid handshake data. Id=%d", a.GetSession().ID())
		}

		if err := a.GetSession().ValidateHandshake(handshakeData); err != nil {
			defer a.GetSession().Close(nil)
			logger.Log.Errorf("Handshake validation failed: %s", err.Error())
			if serr := a.SendHandshakeErrorResponse(); serr != nil {
				logger.Zap.Error("Error sending handshake error response", zap.Error(err))
				return err
			}

			return fmt.Errorf("handshake validation failed: %w. SessionId=%d", err, a.GetSession().ID())
		}

		if !agent.TransitionStatus(a, constants.StatusStart, constants.StatusHandshakeValidated) {
			return rejectPacketForStatus(a, p.Type, a.GetStatus(), constants.StatusStart)
		}

		a.GetSession().SetHandshakeData(handshakeData)
		if err := a.SendHandshakeResponse(); err != nil {
			logger.Zap.Error("Error sending handshake response", zap.Error(err))
			return err
		}
		logger.Log.Debugf("Session handshake Id=%d, Remote=%s", a.GetSession().ID(), a.RemoteAddr())

		// ipversion 暂时用不到
		//err := a.GetSession().Set(constants.IPVersionKey, a.IPVersion())
		//if err != nil {
		//	logger.Log.Warnf("failed to save ip version on session: %q\n", err)
		//}

		logger.Zap.Debug("Successfully saved handshake data")

	case packet.HandshakeAck:
		if !agent.TransitionStatus(a, constants.StatusHandshakeValidated, constants.StatusWorking) {
			return rejectPacketForStatus(a, p.Type, a.GetStatus(), constants.StatusHandshakeValidated)
		}
		logger.Log.Debugf("Receive handshake ACK Id=%d, Remote=%s", a.GetSession().ID(), a.RemoteAddr())

	case packet.Data:
		if status := a.GetStatus(); status != constants.StatusWorking {
			return rejectPacketForStatus(a, p.Type, status, constants.StatusWorking)
		}

		msg, err := message.Decode(p.Data)
		if err != nil {
			return err
		}
		h.processMessage(a, msg)

	case packet.Heartbeat:
		if status := a.GetStatus(); status != constants.StatusWorking {
			return rejectPacketForStatus(a, p.Type, status, constants.StatusWorking)
		}
		rawUnixMillTime, err := decodeHeartbeatTimestamp(p.Data)
		if err != nil {
			return err
		}
		if err := a.SendHeartbeatResponse(rawUnixMillTime); err != nil {
			logger.Zap.Error("Error sending handshake response", zap.Error(err))
			return err
		}

	case packet.HeartbeatAck:
		if status := a.GetStatus(); status != constants.StatusWorking {
			return rejectPacketForStatus(a, p.Type, status, constants.StatusWorking)
		}
		rawUnixMillTime, err := decodeHeartbeatTimestamp(p.Data)
		if err != nil {
			return err
		}
		if rawUnixMillTime > 0 {
			lastTime := time.UnixMilli(rawUnixMillTime)
			latency := time.Since(lastTime)
			if latency > time.Millisecond*100 {
				logger.Zap.Debug("client latency too much", zap.Int64("id", a.GetSession().ID()), zap.String("uid", a.GetSession().UID()), zap.Duration("latency", latency))
			}
			// TODO 上报监控
		}

	default:
		return rejectPacketForStatus(a, p.Type, a.GetStatus(), constants.StatusWorking)
	}

	a.SetLastAt()
	return nil
}

func decodeHeartbeatTimestamp(data []byte) (int64, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("invalid heartbeat payload length %d: expected 0 or 8 bytes", len(data))
	}
	return int64(binary.BigEndian.Uint64(data)), nil
}

func rejectPacketForStatus(a agent.Agent, packetType packet.Type, actual, expected int32) error {
	a.GetSession().Close(nil)
	return fmt.Errorf("unexpected packet type %d for agent status %d, expected status %d", packetType, actual, expected)
}

func (h *HandlerService) processMessage(a agent.Agent, msg *message.Message) {
	// request id 不能用msg.id,msg.id不同客户端会重复
	requestID := nuid.Next()
	ctx := pcontext.AddListToPropagateCtx(context.Background(), constants.StartTimeKey, time.Now().UnixNano(), constants.RouteKey, msg.Route, constants.RequestIDKey, requestID)
	ctx = context.WithValue(ctx, constants.SessionCtxKey, a.GetSession())

	r, err := route.Decode(msg.Route)
	if err != nil {
		logger.Zap.Error("Failed to decode route", zap.Error(err))
		a.AnswerWithError(ctx, msg.ID, apierrors.BadRequest("", "Failed to decode route", "").WithCause(err))
		return
	}
	if isReservedHandlerServiceName(r.Service) {
		a.AnswerWithError(ctx, msg.ID, handlerNotFoundError(r))
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

	if r.SvType == h.server.Type {
		// 派发给session独立线程避免互相影响
		err = h.submitSessionTask(a.GetSession(), ctx, func(ctx context.Context) {
			metrics.ReportMessageProcessDelayFromCtx(ctx, h.metricsReporters, "local")
			h.localProcess(ctx, a, r, msg)
		})
	} else {
		if h.remoteService != nil {
			err = h.submitSessionTask(a.GetSession(), ctx, func(ctx context.Context) {
				metrics.ReportMessageProcessDelayFromCtx(ctx, h.metricsReporters, "remote")
				h.remoteService.remoteProcess(ctx, nil, a, r, msg)
			})
		} else {
			publicErr := apierrors.ServiceUnavailable(
				"remote_service_unavailable",
				"remote service unavailable",
				"",
			).WithMetadata(map[string]string{
				"route":       r.String(),
				"server_type": r.SvType,
			}).WithCause(ErrRemoteServiceUnavailable)
			util.GetLoggerFromCtx(ctx).Warn(
				"request routed to another server type without a remote service",
				zap.String("route", r.String()),
				zap.String("server_type", r.SvType),
			)
			h.completeMessageWithError(ctx, a, msg, publicErr)
			return
		}
	}
	if err != nil {
		logg := util.GetLoggerFromCtx(ctx)
		logg.Error("handler task submission failed", zap.Error(err))
		publicErr := cluster.NormalizeTaskSubmissionError(err)
		h.completeMessageWithError(ctx, a, msg, publicErr)
	}
}

func (h *HandlerService) completeMessageWithError(ctx context.Context, a agent.Agent, msg *message.Message, err error) {
	if msg.Type == message.Notify {
		tracing.FinishSpan(ctx, err)
		metrics.ReportTimingFromCtx(ctx, h.metricsReporters, handlerType, err)
		return
	}
	a.AnswerWithError(ctx, msg.ID, err)
}

func (h *HandlerService) submitSessionTask(sess session.Session, ctx context.Context, task func(context.Context)) error {
	h.taskMu.Lock()
	if h.tasksClosed {
		h.taskMu.Unlock()
		return ErrHandlerServiceStopped
	}
	h.taskWG.Add(1)
	h.taskMu.Unlock()

	taskCtx, cancelTask := contextWithLifecycle(ctx, h.taskCtx)
	err := sess.Go(taskCtx, func(ctx context.Context) {
		defer cancelTask()
		defer h.taskWG.Done()
		task(ctx)
	})
	if err != nil {
		cancelTask()
		h.taskWG.Done()
	}
	return err
}

// DrainTasks closes task admission and waits for every accepted local or
// remote handler task. Call it only after all connection handlers have exited.
func (h *HandlerService) DrainTasks() {
	h.taskMu.Lock()
	h.tasksClosed = true
	cancelTasks := h.cancelTasks
	h.taskMu.Unlock()
	if cancelTasks != nil {
		cancelTasks()
	}
	h.taskWG.Wait()
}

func contextWithLifecycle(parent, lifecycle context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	if lifecycle == nil {
		return ctx, cancel
	}
	stopLifecycle := context.AfterFunc(lifecycle, cancel)
	return ctx, func() {
		stopLifecycle()
		cancel()
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
}

// DumpServices outputs all registered services
func (h *HandlerService) DumpServices() {
	handlers := h.handlerPool.GetHandlers()
	for name := range handlers {
		logger.Log.Infof("registered handler %s, isRawArg: %v", name, handlers[name].IsRawArg)
	}
}

func (h *HandlerService) servicesSnapshot() map[string]*component.Service {
	h.registryMu.RLock()
	defer h.registryMu.RUnlock()
	services := make(map[string]*component.Service, len(h.services))
	for name, registered := range h.services {
		services[name] = registered
	}
	return services
}

// Docs returns documentation for handlers
func (h *HandlerService) Docs(getPtrNames bool) (map[string]interface{}, error) {
	if h == nil {
		return map[string]interface{}{}, nil
	}
	return docgenerator.HandlersDocs(h.server.Type, h.servicesSnapshot(), getPtrNames)
}
