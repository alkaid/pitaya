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
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/util"
	nats "github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

var ErrAlreadySubscribed = errors.New("already subscribed")

var errNatsRPCServerStopping = errors.New("nats rpc server is stopping")

const (
	// NATS defaults to a 4 KiB maximum protocol control line.
	maxNATSReplySubjectBytes = 4 << 10
)

type natsRPCInboundSubscription struct {
	sub      *nats.Subscription
	done     chan struct{}
	doneOnce sync.Once
}

func (s *natsRPCInboundSubscription) complete() {
	s.doneOnce.Do(func() { close(s.done) })
}

// NatsRPCServer struct
type NatsRPCServer struct {
	service                int
	connString             string
	connectionTimeout      time.Duration
	maxReconnectionRetries int
	server                 *Server
	conn                   *nats.Conn
	terminalCallbackDone   <-chan struct{}
	connectionWatchOnce    sync.Once
	connectionClosedOnce   sync.Once
	connectionClosed       chan struct{}
	pushBufferSize         int
	messagesBufferSize     int
	stopChan               chan bool
	subChan                chan *nats.Msg // subChan is the channel used by the server to receive network messages addressed to itself
	bindingsChan           chan *nats.Msg // bindingsChan receives notify from other servers on every user bind to session
	unhandledReqCh         chan *protos.Request
	responses              []*protos.Response
	requests               []*protos.Request
	userPushCh             chan *protos.Push
	userKickCh             chan *protos.KickMsg
	sub                    *nats.Subscription
	broadcastSubs          []*nats.Subscription          // 广播订阅
	publishSubs            map[string]*nats.Subscription // publish订阅:topic->sub
	preparePubSubTopics    map[string]string             // publish预备订阅:topic->group
	dropped                int
	malformedRequests      atomic.Uint64
	pitayaServer           protos.PitayaServer
	metricsReporters       []metrics.Reporter
	sessionPool            session.SessionPool
	appDieChan             chan bool
	reqTimeout             time.Duration
	lifecycleCtx           context.Context
	cancelLifecycle        context.CancelFunc
	stopOnce               sync.Once
	shutdownOnce           sync.Once
	connectionShutdownOnce sync.Once
	handlerMu              sync.Mutex
	handlerWG              sync.WaitGroup
	handlersClosed         bool
	consumersStarted       bool
	taskMu                 sync.Mutex
	taskWG                 sync.WaitGroup
	tasksClosed            bool
	inboundMu              sync.Mutex
	inboundCond            *sync.Cond
	inboundRegistering     int
	inboundSubscriptions   map[*nats.Subscription]*natsRPCInboundSubscription
	inboundClosed          bool
	inboundCallbacks       int
	inboundCallbacksClosed bool
	transportDraining      chan struct{}
	transportDrainOnce     sync.Once
}

// NewNatsRPCServer ctor
func NewNatsRPCServer(
	config config.NatsRPCServerConfig,
	server *Server,
	metricsReporters []metrics.Reporter,
	appDieChan chan bool,
	sessionPool session.SessionPool,
) (*NatsRPCServer, error) {
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	ns := &NatsRPCServer{
		server:               server,
		stopChan:             make(chan bool),
		unhandledReqCh:       make(chan *protos.Request),
		dropped:              0,
		metricsReporters:     metricsReporters,
		appDieChan:           appDieChan,
		connectionTimeout:    nats.DefaultTimeout,
		sessionPool:          sessionPool,
		broadcastSubs:        make([]*nats.Subscription, 0),
		publishSubs:          map[string]*nats.Subscription{},
		preparePubSubTopics:  map[string]string{},
		inboundSubscriptions: map[*nats.Subscription]*natsRPCInboundSubscription{},
		connectionClosed:     make(chan struct{}),
		transportDraining:    make(chan struct{}),
		lifecycleCtx:         lifecycleCtx,
		cancelLifecycle:      cancelLifecycle,
	}
	if err := ns.configure(config); err != nil {
		return nil, err
	}

	return ns, nil
}

func (ns *NatsRPCServer) configure(config config.NatsRPCServerConfig) error {
	ns.service = config.Services
	ns.connString = config.Connect
	if ns.connString == "" {
		return constants.ErrNoNatsConnectionString
	}
	ns.connectionTimeout = config.ConnectionTimeout
	ns.maxReconnectionRetries = config.MaxReconnectionRetries
	ns.messagesBufferSize = config.Buffer.Messages
	if ns.messagesBufferSize == 0 {
		return constants.ErrNatsMessagesBufferSizeZero
	}
	ns.pushBufferSize = config.Buffer.Push
	if ns.pushBufferSize == 0 {
		return constants.ErrNatsPushBufferSizeZero
	}
	ns.subChan = make(chan *nats.Msg, ns.messagesBufferSize)
	ns.bindingsChan = make(chan *nats.Msg, ns.messagesBufferSize)
	// the reason this channel is buffered is that we can achieve more performance by not
	// blocking producers on a massive push
	ns.userPushCh = make(chan *protos.Push, ns.pushBufferSize)
	ns.userKickCh = make(chan *protos.KickMsg, ns.messagesBufferSize)
	ns.responses = make([]*protos.Response, ns.service)
	ns.requests = make([]*protos.Request, ns.service)
	ns.reqTimeout = config.RequestTimeout
	return nil
}

// GetBindingsChannel gets the channel that will receive all bindings
func (ns *NatsRPCServer) GetBindingsChannel() chan *nats.Msg {
	return ns.bindingsChan
}

// GetUserMessagesTopic get the topic for user
func GetUserMessagesTopic(uid string, svType string) string {
	return fmt.Sprintf("pitaya/%s/user/%s/push", svType, uid)
}

// GetUserKickTopic get the topic for kicking an user
func GetUserKickTopic(uid string, svType string) string {
	return fmt.Sprintf("pitaya/%s/user/%s/kick", svType, uid)
}

// GetBindBroadcastTopic gets the topic on which bind events will be broadcasted
func GetBindBroadcastTopic(svType string) string {
	return fmt.Sprintf("pitaya/%s/bindings", svType)
}

func GetForkTopic(svrType string) string {
	return fmt.Sprintf("pitaya.fork.%s", svrType)
}

// onSessionBind should be called on each session bind
func (ns *NatsRPCServer) onSessionBind(ctx context.Context, s session.Session, callback map[string]string) error {
	if ns.server.Frontend {
		subu, err := ns.subscribeToUserMessages(s.UID(), ns.server.Type)
		if err != nil {
			return err
		}
		subk, err := ns.subscribeToUserKickChannel(s.UID(), ns.server.Type)
		if err != nil {
			return err
		}
		s.SetSubscriptions([]*nats.Subscription{subu, subk})
	}
	return nil
}

// SetPitayaServer sets the pitaya server
func (ns *NatsRPCServer) SetPitayaServer(ps protos.PitayaServer) {
	ns.pitayaServer = ps
}

func (ns *NatsRPCServer) subscribeToBindingsChannel() error {
	_, err := ns.addInboundSubscription(func() (*nats.Subscription, error) {
		return ns.conn.ChanSubscribe(GetBindBroadcastTopic(ns.server.Type), ns.bindingsChan)
	})
	return err
}

func (ns *NatsRPCServer) subscribeToUserKickChannel(uid string, svType string) (*nats.Subscription, error) {
	return ns.addInboundSubscription(func() (*nats.Subscription, error) {
		return ns.conn.Subscribe(GetUserKickTopic(uid, svType), func(msg *nats.Msg) {
			if !ns.beginInboundCallback() {
				return
			}
			defer ns.endInboundCallback()
			kick := &protos.KickMsg{}
			err := proto.Unmarshal(msg.Data, kick)
			if err != nil {
				logger.Zap.Error("error unrmarshalling push", zap.Error(err))
			}
			select {
			case ns.userKickCh <- kick:
			case <-ns.stopChan:
			}
		})
	})
}

func (ns *NatsRPCServer) subscribeToUserMessages(uid string, svType string) (*nats.Subscription, error) {
	return ns.addInboundSubscription(func() (*nats.Subscription, error) {
		return ns.conn.Subscribe(GetUserMessagesTopic(uid, svType), func(msg *nats.Msg) {
			if !ns.beginInboundCallback() {
				return
			}
			defer ns.endInboundCallback()
			push := &protos.Push{}
			err := proto.Unmarshal(msg.Data, push)
			if err != nil {
				logger.Zap.Error("error unmarshalling push", zap.Error(err))
			}
			logger.Zap.Debug("receive user's push", zap.String("uid", uid), zap.Int("remain", len(ns.userPushCh)))
			select {
			case ns.userPushCh <- push:
			case <-ns.stopChan:
			}
		})
	})
}

func (ns *NatsRPCServer) handleMessages() {
	normalStop := false
	defer (func() {
		recovered := recover()
		// ChanSubscribe channels remain open because the NATS read loop owns their
		// write side and may still be selecting a delivery while closing.
		if !normalStop {
			ns.stopHandlers()
			if ns.conn != nil {
				ns.finishConnection(false)
			}
		}
		close(ns.unhandledReqCh)
		if recovered != nil {
			logger.Zap.Error("nats rpc message pump panicked", zap.Any("panic", recovered))
		}
		if !normalStop {
			logger.Zap.Error("nats rpc message pump exited unexpectedly")
			signalAppDie(ns.appDieChan)
		}
	})()
	maxPending := float64(0)
	draining := false
	for {
		var msg *nats.Msg
		var ok bool
		if draining {
			select {
			case msg, ok = <-ns.subChan:
			default:
				normalStop = true
				return
			}
		} else {
			select {
			case msg, ok = <-ns.subChan:
			case <-ns.transportDraining:
				draining = true
				continue
			case <-ns.stopChan:
				normalStop = true
				return
			}
		}
		if !ok {
			return
		}
		dropped := 0
		if ns.sub != nil {
			var err error
			dropped, err = ns.sub.Dropped()
			if err != nil {
				logger.Zap.Error("error getting number of dropped messages", zap.Error(err))
			}
		}
		// 添加广播订阅的消息丢失日志和统计
		for _, sub := range ns.broadcastSubs {
			tmpDropped, err := sub.Dropped()
			if err != nil {
				logger.Zap.Error("error getting number of dropped messages", zap.Error(err))
			}
			dropped += tmpDropped
		}
		for _, sub := range ns.publishSubs {
			tmpDropped, err := sub.Dropped()
			if err != nil {
				logger.Zap.Error("error getting number of dropped messages", zap.Error(err))
			}
			dropped += tmpDropped
		}
		if dropped > ns.dropped {
			logger.Log.Warnf("[rpc server] some messages were dropped! numDropped: %d", dropped)
			ns.dropped = dropped
		}
		req, err := validateNatsRPCRequest(msg)
		if err != nil {
			ns.rejectMalformedRequest(msg, err)
		}
		ns.reportMetrics()
		subsChanLen := float64(len(ns.subChan))
		maxPending = math.Max(float64(maxPending), subsChanLen)
		logger.Log.Debugf("subs channel size: %f, max: %f, dropped: %d", subsChanLen, maxPending, dropped)
		if err != nil {
			continue
		}
		// TODO: Add tracing here to report delay to start processing message in spans
		select {
		case ns.unhandledReqCh <- req:
		case <-ns.stopChan:
			normalStop = true
			return
		}
	}
}

func (ns *NatsRPCServer) drainConnection() {
	ns.finishConnection(true)
}

func (ns *NatsRPCServer) finishConnection(drain bool) {
	ns.connectionShutdownOnce.Do(func() {
		if ns.conn == nil {
			return
		}
		var closed chan nats.Status
		if !ns.conn.IsClosed() {
			closed = ns.conn.StatusChanged(nats.CLOSED)
			defer ns.conn.RemoveStatusListener(closed)
			if drain {
				if err := ns.conn.Drain(); err != nil {
					logger.Zap.Warn("error draining nats rpc server connection", zap.Error(err))
					// Drain normally closes asynchronously. An early error does not
					// provide that guarantee, so stop delivery synchronously.
					ns.conn.Close()
				}
			} else {
				ns.conn.Close()
			}
		}
		if closed != nil && !ns.conn.IsClosed() {
			<-closed
		}
		if ns.terminalCallbackDone != nil {
			<-ns.terminalCallbackDone
		}
	})
}

func (ns *NatsRPCServer) watchConnectionClose() <-chan struct{} {
	conn := ns.conn
	if conn == nil {
		return nil
	}
	ns.connectionWatchOnce.Do(func() {
		if ns.connectionClosed == nil {
			ns.connectionClosed = make(chan struct{})
		}
		closed := conn.StatusChanged(nats.CLOSED)
		if conn.IsClosed() {
			conn.RemoveStatusListener(closed)
			ns.markConnectionClosed()
			return
		}
		go func() {
			<-closed
			ns.markConnectionClosed()
			conn.RemoveStatusListener(closed)
		}()
	})
	return ns.connectionClosed
}

func (ns *NatsRPCServer) markConnectionClosed() {
	ns.connectionClosedOnce.Do(func() {
		close(ns.connectionClosed)
	})
}

func (ns *NatsRPCServer) connectionHasClosed(closed <-chan struct{}) bool {
	if closed != nil {
		select {
		case <-closed:
			return true
		default:
		}
	}
	return ns.conn != nil && ns.conn.IsClosed()
}

func validateNatsRPCRequest(msg *nats.Msg) (*protos.Request, error) {
	if msg == nil {
		return nil, errors.New("nil nats message")
	}
	if len(msg.Data) == 0 {
		return nil, errors.New("empty request payload")
	}
	if len(msg.Data) > codec.MaxPacketSize {
		return nil, errors.Errorf("request payload exceeds %d bytes", codec.MaxPacketSize)
	}

	req := &protos.Request{}
	if err := proto.Unmarshal(msg.Data, req); err != nil {
		return nil, errors.Wrap(err, "unmarshal request")
	}
	if req.Msg == nil {
		return nil, errors.New("request msg is required")
	}
	if req.Type != protos.RPCType_Sys && req.Type != protos.RPCType_User {
		return nil, errors.Errorf("invalid rpc type %d", req.Type)
	}
	if req.Msg.Type != protos.MsgType_MsgRequest && req.Msg.Type != protos.MsgType_MsgNotify {
		return nil, errors.Errorf("invalid rpc message type %d", req.Msg.Type)
	}
	if len(req.Msg.Route) == 0 {
		return nil, errors.Errorf("invalid route length %d", len(req.Msg.Route))
	}
	if _, err := route.Decode(req.Msg.Route); err != nil {
		return nil, errors.Wrap(err, "invalid route")
	}
	if len(req.Msg.Data) > codec.MaxPacketSize {
		return nil, errors.Errorf("message data exceeds %d bytes", codec.MaxPacketSize)
	}
	if req.Session != nil {
		if req.Session.Id < 0 {
			return nil, errors.Errorf("invalid session id %d", req.Session.Id)
		}
		if proto.Size(req.Session) > codec.MaxPacketSize {
			return nil, errors.Errorf("session exceeds %d bytes", codec.MaxPacketSize)
		}
	}
	if req.Msg.Type == protos.MsgType_MsgRequest {
		if err := validateNATSReplySubject(msg.Reply); err != nil {
			return nil, err
		}
	} else if msg.Reply != "" {
		if err := validateNATSReplySubject(msg.Reply); err != nil {
			return nil, err
		}
	}

	req.Msg.Reply = msg.Reply
	return req, nil
}

func validateNATSReplySubject(subject string) error {
	if subject == "" {
		return errors.New("request reply subject is required")
	}
	if len(subject) > maxNATSReplySubjectBytes {
		return errors.Errorf("reply subject exceeds %d bytes", maxNATSReplySubjectBytes)
	}
	if strings.ContainsAny(subject, " \t\r\n*>") {
		return errors.New("invalid reply subject")
	}
	for _, token := range strings.Split(subject, ".") {
		if token == "" {
			return errors.New("invalid reply subject")
		}
	}
	return nil
}

func (ns *NatsRPCServer) rejectMalformedRequest(msg *nats.Msg, err error) {
	count := ns.malformedRequests.Add(1)
	fields := []zap.Field{
		zap.Error(err),
		zap.Uint64("malformedCount", count),
	}
	if msg != nil {
		fields = append(fields,
			zap.String("subject", msg.Subject),
			zap.String("reply", msg.Reply),
			zap.Int("payloadBytes", len(msg.Data)),
		)
	}
	logger.Zap.Warn("rejecting malformed nats rpc request", fields...)
}

// GetUnhandledRequestsChannel gets the unhandled requests channel from nats rpc server
func (ns *NatsRPCServer) GetUnhandledRequestsChannel() chan *protos.Request {
	return ns.unhandledReqCh
}

func (ns *NatsRPCServer) getUserPushChannel() chan *protos.Push {
	return ns.userPushCh
}

func (ns *NatsRPCServer) getUserKickChannel() chan *protos.KickMsg {
	return ns.userKickCh
}

func (ns *NatsRPCServer) marshalResponse(res *protos.Response) ([]byte, error) {
	p, err := proto.Marshal(res)
	if err != nil {
		res := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		p, _ = proto.Marshal(res)
	}

	if err == nil && res.Status != nil {
		err = apierrors.FromStatus(res.Status)
	}
	return p, err
}

func (ns *NatsRPCServer) processMessages(threadID int) {
	for req := range ns.GetUnhandledRequestsChannel() {
		ns.requests[threadID] = req
		logger.Log.Debugf("(%d) processing message %v", threadID, req.GetMsg().GetId())
		uid := ""
		if req.Session != nil {
			uid = req.Session.Uid
		}
		ctx, err := util.GetContextFromRequest(ns.requests[threadID], ns.server.ID, uid)
		if err != nil {
			ns.responses[threadID] = &protos.Response{
				Status: &apierrors.FromError(err).Status,
			}
			if ns.requests[threadID].GetMsg().Type != protos.MsgType_MsgNotify {
				p, err := ns.marshalResponse(ns.responses[threadID])
				err = ns.conn.Publish(ns.requests[threadID].GetMsg().GetReply(), p)
				if err != nil {
					logger.Zap.Error("error sending message response", zap.Error(err))
				}
			}
			continue
		}
		logg := util.GetLoggerFromCtx(ctx)
		logg.Debug("rpcsv processing msg")
		submitErr := ns.submitRequestTask(ctx, req, func(ctx context.Context) {
			resp, err := ns.pitayaServer.Call(ctx, req)
			if err != nil {
				// pitayaServer.Call已有打印error,这里不再重复
				logg.Info("rpc error calling pitayaServer", zap.String("cause", err.Error()))
			}
			if req.GetMsg().Type != protos.MsgType_MsgNotify {
				p, err := ns.marshalResponse(resp)
				err = ns.conn.Publish(req.GetMsg().GetReply(), p)
				if err != nil {
					logg.Error("error sending message response", zap.Error(err))
				}
			}
		})
		if submitErr != nil {
			logg.Error("rpc task submission failed", zap.Error(submitErr))
			if req.GetMsg().Type != protos.MsgType_MsgNotify {
				publicErr := NormalizeTaskSubmissionError(submitErr)
				response := &protos.Response{Status: &apierrors.FromError(publicErr).Status}
				// Status responses intentionally make marshalResponse return the status error.
				payload, _ := ns.marshalResponse(response)
				if publishErr := ns.conn.Publish(req.GetMsg().GetReply(), payload); publishErr != nil {
					logg.Error("error sending task submission response", zap.Error(publishErr))
				}
			}
		}
	}
}

func (ns *NatsRPCServer) processSessionBindings() {
	draining := false
	for {
		var bind *nats.Msg
		var ok bool
		if draining {
			select {
			case bind, ok = <-ns.bindingsChan:
			default:
				return
			}
		} else {
			select {
			case bind, ok = <-ns.bindingsChan:
			case <-ns.transportDraining:
				draining = true
				continue
			case <-ns.stopChan:
				return
			}
		}
		if !ok {
			return
		}
		b := &protos.BindMsg{}
		err := proto.Unmarshal(bind.Data, b)
		if err != nil {
			logger.Zap.Error("error processing binding msg", zap.Error(err))
			continue
		}
		ns.pitayaServer.SessionBindRemote(ns.runtimeContext(), b)
	}
}

func (ns *NatsRPCServer) processPushes() {
	draining := false
	for {
		var push *protos.Push
		var ok bool
		if draining {
			select {
			case push, ok = <-ns.getUserPushChannel():
			default:
				return
			}
		} else {
			select {
			case push, ok = <-ns.getUserPushChannel():
			case <-ns.transportDraining:
				draining = true
				continue
			case <-ns.stopChan:
				return
			}
		}
		if !ok {
			return
		}
		_, err := ns.pitayaServer.PushToUser(ns.runtimeContext(), push)
		if err != nil {
			logger.Zap.Error("error sending push to user", zap.Error(err))
		}
	}
}

func (ns *NatsRPCServer) processKick() {
	draining := false
	for {
		var kick *protos.KickMsg
		var ok bool
		if draining {
			select {
			case kick, ok = <-ns.getUserKickChannel():
			default:
				return
			}
		} else {
			select {
			case kick, ok = <-ns.getUserKickChannel():
			case <-ns.transportDraining:
				draining = true
				continue
			case <-ns.stopChan:
				return
			}
		}
		if !ok {
			return
		}
		logger.Log.Debugf("Sending kick to user %s: %v", kick.GetUserId())
		_, err := ns.pitayaServer.KickUser(ns.runtimeContext(), kick)
		if err != nil {
			logger.Zap.Error("error sending kick to user", zap.Error(err))
		}
	}
}

// Init inits nats rpc server
func (ns *NatsRPCServer) Init() error {
	logger.Log.Debugf("connecting to nats (server) with timeout of %s", ns.connectionTimeout)
	conn, terminalCallbackDone, err := setupTrackedNatsConn(
		ns.connString,
		ns.appDieChan,
		nil,
		nats.MaxReconnects(ns.maxReconnectionRetries),
		nats.Timeout(ns.connectionTimeout),
	)
	if err != nil {
		return err
	}
	ns.conn = conn
	ns.terminalCallbackDone = terminalCallbackDone
	ns.watchConnectionClose()
	defer func() {
		if err != nil {
			ns.finishConnection(false)
		}
	}()
	if ns.sub, err = ns.subscribe(getChannel(ns.server.Type, ns.server.ID), false); err != nil {
		return err
	}
	// fork订阅
	var bcstSub *nats.Subscription
	topic := GetForkTopic(ns.server.Type)
	if bcstSub, err = ns.subscribe(topic, false); err != nil {
		return err
	}
	ns.broadcastSubs = append(ns.broadcastSubs, bcstSub)
	// publish订阅
	for t, group := range ns.preparePubSubTopics {
		var sub *nats.Subscription
		sub, err = ns.addInboundSubscription(func() (*nats.Subscription, error) {
			if group == "" {
				return ns.conn.ChanSubscribe(t, ns.subChan)
			}
			return ns.conn.ChanQueueSubscribe(t, group, ns.subChan)
		})
		if err != nil {
			return errors.WithStack(err)
		}
		ns.publishSubs[t] = sub
	}
	// topic = GetForkTopic("", false)
	// queue = NeedQueueSubscribe(ns.server, true, false)
	// if bcstSub, err = ns.subscribe(topic, queue); err != nil {
	//	return err
	// }
	// ns.broadcastSubs = append(ns.broadcastSubs, bcstSub)
	// topic = GetForkTopic(ns.server.Type)
	// if bcstSub, err = ns.subscribe(topic, false); err != nil {
	//	return err
	// }
	// ns.broadcastSubs = append(ns.broadcastSubs, bcstSub)
	// topic = GetForkTopic("", true)
	// queue = NeedQueueSubscribe(ns.server, true, true)
	// if bcstSub, err = ns.subscribe(topic, queue); err != nil {
	//	return err
	// }
	// ns.broadcastSubs = append(ns.broadcastSubs, bcstSub)

	if err = ns.subscribeToBindingsChannel(); err != nil {
		return err
	}

	// Keep message delivery ordered here; requests fan out through the keyed
	// session pools after this single dispatcher accepts them.
	handlers := []func(){
		func() { ns.processMessages(0) },
		ns.processSessionBindings,
		ns.handleMessages,
		ns.processPushes,
		ns.processKick,
	}
	accepted := 0
	for _, handler := range handlers {
		if err = ns.startHandler(handler); err != nil {
			// Once admitted, handleMessages owns the request-channel shutdown.
			if accepted >= 3 {
				ns.stopHandlers()
			} else {
				ns.stopHandlers()
				if accepted > 0 {
					close(ns.unhandledReqCh)
				}
			}
			ns.handlerWG.Wait()
			return errors.Wrap(err, "start nats rpc handler")
		}
		accepted++
	}
	ns.handlerMu.Lock()
	if ns.handlersClosed {
		err = errNatsRPCServerStopping
	} else {
		ns.consumersStarted = true
	}
	ns.handlerMu.Unlock()
	if err != nil {
		ns.handlerWG.Wait()
		return errors.Wrap(err, "start nats rpc consumers")
	}

	ns.sessionPool.OnSessionBind(ns.onSessionBind)

	return nil
}

// AfterInit runs after initialization
func (ns *NatsRPCServer) AfterInit() {}

// BeforeShutdown runs before shutdown
func (ns *NatsRPCServer) BeforeShutdown() {
	ns.stopAndWait()
}

// Shutdown stops nats rpc server
func (ns *NatsRPCServer) Shutdown() error {
	ns.stopAndWait()
	return nil
}

func (ns *NatsRPCServer) stopHandlers() {
	ns.handlerMu.Lock()
	ns.handlersClosed = true
	ns.stopOnce.Do(func() {
		if ns.cancelLifecycle != nil {
			ns.cancelLifecycle()
		}
		close(ns.stopChan)
	})
	ns.handlerMu.Unlock()

	ns.taskMu.Lock()
	ns.tasksClosed = true
	ns.taskMu.Unlock()
}

func (ns *NatsRPCServer) startHandler(handler func()) error {
	ns.handlerMu.Lock()
	defer ns.handlerMu.Unlock()
	if ns.handlersClosed {
		return errNatsRPCServerStopping
	}
	ns.handlerWG.Add(1)
	if err := co.Submit(func() {
		defer ns.handlerWG.Done()
		handler()
	}); err != nil {
		ns.handlerWG.Done()
		return err
	}
	return nil
}

func (ns *NatsRPCServer) submitRequestTask(ctx context.Context, req *protos.Request, task func(context.Context)) error {
	ns.taskMu.Lock()
	if ns.tasksClosed {
		ns.taskMu.Unlock()
		return errNatsRPCServerStopping
	}
	ns.taskWG.Add(1)
	ns.taskMu.Unlock()

	taskCtx, cancelTask := ns.contextWithLifecycle(ctx)
	err := GoWithRequest(taskCtx, req, func(ctx context.Context) {
		defer cancelTask()
		defer ns.taskWG.Done()
		task(ctx)
	})
	if err != nil {
		cancelTask()
		ns.taskWG.Done()
	}
	return err
}

func (ns *NatsRPCServer) runtimeContext() context.Context {
	if ns.lifecycleCtx == nil {
		return context.Background()
	}
	return ns.lifecycleCtx
}

func (ns *NatsRPCServer) contextWithLifecycle(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	if ns.lifecycleCtx == nil {
		return ctx, cancel
	}
	stopLifecycle := context.AfterFunc(ns.lifecycleCtx, cancel)
	return ctx, func() {
		stopLifecycle()
		cancel()
	}
}

func (ns *NatsRPCServer) addInboundSubscription(
	subscribe func() (*nats.Subscription, error),
) (*nats.Subscription, error) {
	ns.inboundMu.Lock()
	if ns.inboundClosed {
		ns.inboundMu.Unlock()
		return nil, errNatsRPCServerStopping
	}
	ns.ensureInboundConditionLocked()
	// Reserve admission before entering NATS, whose close path uses its own locks.
	ns.inboundRegistering++
	ns.inboundMu.Unlock()

	connectionClosed := ns.watchConnectionClose()
	sub, err := subscribe()
	var tracked *natsRPCInboundSubscription
	var closed <-chan nats.SubStatus
	if err == nil && sub != nil {
		tracked = &natsRPCInboundSubscription{
			sub:  sub,
			done: make(chan struct{}),
		}
		closed = sub.StatusChanged(nats.SubscriptionClosed)
	}

	ns.inboundMu.Lock()
	if tracked != nil {
		if ns.inboundSubscriptions == nil {
			ns.inboundSubscriptions = make(map[*nats.Subscription]*natsRPCInboundSubscription)
		}
		ns.inboundSubscriptions[sub] = tracked
	}
	ns.inboundRegistering--
	if ns.inboundRegistering == 0 {
		ns.inboundCond.Broadcast()
	}
	ns.inboundMu.Unlock()
	if err != nil || sub == nil {
		return sub, err
	}

	go func() {
		select {
		case <-closed:
		case <-connectionClosed:
		}
		tracked.complete()
		ns.inboundMu.Lock()
		delete(ns.inboundSubscriptions, sub)
		ns.inboundMu.Unlock()
	}()
	return sub, nil
}

func (ns *NatsRPCServer) ensureInboundConditionLocked() {
	if ns.inboundCond == nil {
		ns.inboundCond = sync.NewCond(&ns.inboundMu)
	}
}

func (ns *NatsRPCServer) beginInboundCallback() bool {
	ns.inboundMu.Lock()
	defer ns.inboundMu.Unlock()
	if ns.inboundCallbacksClosed {
		return false
	}
	ns.inboundCallbacks++
	return true
}

func (ns *NatsRPCServer) endInboundCallback() {
	ns.inboundMu.Lock()
	ns.inboundCallbacks--
	if ns.inboundCallbacks == 0 && ns.inboundCond != nil {
		ns.inboundCond.Broadcast()
	}
	ns.inboundMu.Unlock()
}

func (ns *NatsRPCServer) closeInboundCallbackAdmission() {
	ns.inboundMu.Lock()
	ns.inboundCallbacksClosed = true
	ns.inboundMu.Unlock()
}

func (ns *NatsRPCServer) waitInboundCallbacks() {
	ns.inboundMu.Lock()
	ns.ensureInboundConditionLocked()
	for ns.inboundCallbacks > 0 {
		ns.inboundCond.Wait()
	}
	ns.inboundMu.Unlock()
}

func (ns *NatsRPCServer) closeInboundSubscriptionAdmission() []*natsRPCInboundSubscription {
	ns.inboundMu.Lock()
	defer ns.inboundMu.Unlock()
	ns.ensureInboundConditionLocked()
	ns.inboundClosed = true
	// Every registration admitted before closure must publish its result before the snapshot.
	for ns.inboundRegistering > 0 {
		ns.inboundCond.Wait()
	}
	subscriptions := make([]*natsRPCInboundSubscription, 0, len(ns.inboundSubscriptions))
	for _, subscription := range ns.inboundSubscriptions {
		subscriptions = append(subscriptions, subscription)
	}
	ns.inboundSubscriptions = nil
	return subscriptions
}

func (ns *NatsRPCServer) drainInboundSubscriptions(subscriptions []*natsRPCInboundSubscription) bool {
	connectionClosed := ns.watchConnectionClose()
	if ns.connectionHasClosed(connectionClosed) {
		ns.closeInboundCallbackAdmission()
		return false
	}
	for _, subscription := range subscriptions {
		if subscription == nil || subscription.sub == nil {
			continue
		}
		sub := subscription.sub
		if err := sub.Drain(); err != nil {
			if ns.connectionHasClosed(connectionClosed) {
				ns.closeInboundCallbackAdmission()
				return false
			}
			if sub.IsValid() {
				logger.Zap.Warn("error draining nats rpc server subscription", zap.Error(err))
				if unsubscribeErr := sub.Unsubscribe(); unsubscribeErr != nil && sub.IsValid() {
					logger.Zap.Warn("error stopping nats rpc server subscription", zap.Error(unsubscribeErr))
					ns.closeInboundCallbackAdmission()
					return false
				}
			}
		}
	}

	drained := true
waitForSubscriptions:
	for _, subscription := range subscriptions {
		if subscription == nil {
			continue
		}
		select {
		case <-subscription.done:
		case <-connectionClosed:
			drained = false
			break waitForSubscriptions
		case <-ns.stopChan:
			drained = false
			break waitForSubscriptions
		}
	}
	if ns.connectionHasClosed(connectionClosed) {
		drained = false
	}
	ns.closeInboundCallbackAdmission()
	if !drained {
		return false
	}
	ns.waitInboundCallbacks()
	return true
}

func (ns *NatsRPCServer) beginGracefulTransportDrain() bool {
	ns.handlerMu.Lock()
	defer ns.handlerMu.Unlock()
	if !ns.consumersStarted || ns.handlersClosed || ns.transportDraining == nil {
		return false
	}
	// Freeze owned-handler admission before waiting on handlerWG. The transport
	// consumers remain live until the private drain signal is published.
	ns.handlersClosed = true
	return true
}

func (ns *NatsRPCServer) signalTransportDrain() {
	ns.transportDrainOnce.Do(func() {
		close(ns.transportDraining)
	})
}

func (ns *NatsRPCServer) stopAndWait() {
	ns.shutdownOnce.Do(func() {
		subscriptions := ns.closeInboundSubscriptionAdmission()
		graceful := ns.beginGracefulTransportDrain()
		if graceful && ns.drainInboundSubscriptions(subscriptions) {
			ns.signalTransportDrain()
			ns.handlerWG.Wait()
			ns.stopHandlers()
		} else {
			ns.closeInboundCallbackAdmission()
			ns.stopHandlers()
			ns.waitInboundCallbacks()
			ns.handlerWG.Wait()
		}
		ns.taskWG.Wait()
		ns.drainConnection()
	})
}

func (ns *NatsRPCServer) subscribe(topic string, queue bool) (*nats.Subscription, error) {
	return ns.addInboundSubscription(func() (*nats.Subscription, error) {
		if queue {
			return ns.conn.ChanQueueSubscribe(topic, ns.server.Type, ns.subChan)
		}
		return ns.conn.ChanSubscribe(topic, ns.subChan)
	})
}

func (ns *NatsRPCServer) stop() {
}

// Subscribe @implement [RPCServer.Subscribe]
//
// Deprecated: unused
//
//	@receiver ns
//	@param topic
//	@param groups
//	@return error
func (ns *NatsRPCServer) Subscribe(topic string, groups ...string) error {
	//topic = GetPublishTopic(topic)
	// TODO 线程不安全,若后续真有动态订阅需求再优化
	sub, ok := ns.publishSubs[topic]
	if ok {
		logger.Zap.Warn("", zap.String("topic", topic), zap.Error(ErrAlreadySubscribed))
		return nil
	}
	_, ok = ns.preparePubSubTopics[topic]
	if ok {
		logger.Zap.Warn("", zap.String("topic", topic), zap.Error(ErrAlreadySubscribed))
		return nil
	}
	var err error
	group := ""
	if len(groups) > 0 {
		group = groups[0]
	}
	// 若还未连接则加入预备
	if ns.conn == nil {
		ns.preparePubSubTopics[topic] = group
		return nil
	}
	// 已连接直接订阅
	sub, err = ns.addInboundSubscription(func() (*nats.Subscription, error) {
		if group == "" {
			return ns.conn.ChanSubscribe(topic, ns.subChan)
		}
		return ns.conn.ChanQueueSubscribe(topic, group, ns.subChan)
	})
	if err != nil {
		return errors.WithStack(err)
	}
	ns.publishSubs[topic] = sub
	return nil
}

func (ns *NatsRPCServer) reportMetrics() {
	if ns.metricsReporters != nil {
		for _, mr := range ns.metricsReporters {
			dropped := uint64(ns.dropped) + ns.malformedRequests.Load()
			if err := mr.ReportGauge(metrics.DroppedMessages, map[string]string{}, float64(dropped)); err != nil {
				logger.Zap.Warn("failed to report dropped message", zap.Error(err))
			}

			// subchan
			subChanCapacity := ns.messagesBufferSize - len(ns.subChan)
			if subChanCapacity == 0 {
				logger.Log.Warn("subChan is at maximum capacity")
			}
			if err := mr.ReportGauge(metrics.ChannelCapacity, map[string]string{"channel": "rpc_server_subchan"}, float64(subChanCapacity)); err != nil {
				logger.Zap.Warn("failed to report subChan queue capacity", zap.Error(err))
			}

			// bindingschan
			bindingsChanCapacity := ns.messagesBufferSize - len(ns.bindingsChan)
			if bindingsChanCapacity == 0 {
				logger.Log.Warn("bindingsChan is at maximum capacity")
			}
			if err := mr.ReportGauge(metrics.ChannelCapacity, map[string]string{"channel": "rpc_server_bindingschan"}, float64(bindingsChanCapacity)); err != nil {
				logger.Zap.Warn("failed to report bindingsChan capacity", zap.Error(err))
			}

			// userpushch
			userPushChanCapacity := ns.pushBufferSize - len(ns.userPushCh)
			if userPushChanCapacity == 0 {
				logger.Log.Warn("userPushChan is at maximum capacity")
			}
			if err := mr.ReportGauge(metrics.ChannelCapacity, map[string]string{"channel": "rpc_server_userpushchan"}, float64(userPushChanCapacity)); err != nil {
				logger.Log.Warnf("failed to report userPushCh capacity: %s", err.Error())
			}
		}
	}
}
