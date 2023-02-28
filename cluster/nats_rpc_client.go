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
	"time"

	"github.com/alkaid/goerrors/errors"
	"github.com/topfreegames/pitaya/v2/util"
	"go.uber.org/zap"

	"github.com/alkaid/goerrors/apierrors"

	nats "github.com/nats-io/nats.go"
	"github.com/samber/lo"
	"github.com/topfreegames/pitaya/v2/config"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/constants"
	pcontext "github.com/topfreegames/pitaya/v2/context"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/metrics"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/route"
	"github.com/topfreegames/pitaya/v2/session"
	"github.com/topfreegames/pitaya/v2/tracing"
	"google.golang.org/protobuf/proto"
)

// NatsRPCClient struct
type NatsRPCClient struct {
	conn                   *nats.Conn
	connString             string
	connectionTimeout      time.Duration
	maxReconnectionRetries int
	reqTimeout             time.Duration
	running                bool
	server                 *Server
	metricsReporters       []metrics.Reporter
	appDieChan             chan bool
}

// NewNatsRPCClient ctor
func NewNatsRPCClient(
	config config.NatsRPCClientConfig,
	server *Server,
	metricsReporters []metrics.Reporter,
	appDieChan chan bool,
) (*NatsRPCClient, error) {
	ns := &NatsRPCClient{
		server:            server,
		running:           false,
		metricsReporters:  metricsReporters,
		appDieChan:        appDieChan,
		connectionTimeout: nats.DefaultTimeout,
	}
	if err := ns.configure(config); err != nil {
		return nil, err
	}
	return ns, nil
}

func (ns *NatsRPCClient) configure(config config.NatsRPCClientConfig) error {
	ns.connString = config.Connect
	if ns.connString == "" {
		return constants.ErrNoNatsConnectionString
	}
	ns.connectionTimeout = config.ConnectionTimeout
	ns.maxReconnectionRetries = config.MaxReconnectionRetries
	ns.reqTimeout = config.RequestTimeout
	if ns.reqTimeout == 0 {
		return constants.ErrNatsNoRequestTimeout
	}
	return nil
}

// Deprecated:Use Fork instead
//
// sends the binding information to other servers that may be interested in this info
func (ns *NatsRPCClient) BroadcastSessionBind(uid string) error {
	msg := &protos.BindMsg{
		Uid: uid,
		Fid: ns.server.ID,
	}
	msgData, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	return ns.Send(GetBindBroadcastTopic(ns.server.Type), msgData)
}

// Send publishes a message in a given topic
func (ns *NatsRPCClient) Send(topic string, data []byte) error {
	if !ns.running {
		return constants.ErrRPCClientNotInitialized
	}
	return ns.conn.Publish(topic, data)
}

// SendPush sends a message to a user
func (ns *NatsRPCClient) SendPush(userID string, frontendSv *Server, push *protos.Push) error {
	topic := GetUserMessagesTopic(userID, frontendSv.Type)
	msg, err := proto.Marshal(push)
	if err != nil {
		return err
	}
	return ns.Send(topic, msg)
}

// SendKick kicks an user
func (ns *NatsRPCClient) SendKick(userID string, serverType string, kick *protos.KickMsg) error {
	topic := GetUserKickTopic(userID, serverType)
	msg, err := proto.Marshal(kick)
	if err != nil {
		return err
	}
	return ns.Send(topic, msg)
}

func (ns *NatsRPCClient) Publish(
	ctx context.Context,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
	timeouts ...time.Duration,
) ([]*protos.Response, error) {
	var err error
	spanInfo := &tracing.SpanInfo{
		RpcSystem: "nats",
		IsClient:  true,
		Route:     route,
		LocalID:   ns.server.ID,
		LocalType: ns.server.Type,
		RequestID: "",
	}
	ctx = tracing.RPCStartSpan(ctx, spanInfo)
	defer tracing.FinishSpan(ctx, err)

	if !ns.running {
		err = constants.ErrRPCClientNotInitialized
		return nil, errors.WithStack(err)
	}
	req, err := buildRequest(ctx, rpcType, route.String(), session, msg, ns.server)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	marshalledData, err := proto.Marshal(&req)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if ns.metricsReporters != nil {
		startTime := time.Now()
		ctx = pcontext.AddListToPropagateCtx(ctx, constants.StartTimeKey, startTime.UnixNano(), constants.RouteKey, route.String())
		defer func() {
			typ := "rpc"
			metrics.ReportTimingFromCtx(ctx, ns.metricsReporters, typ, err)
		}()
	}

	// support notify type.  notify msg don't need wait response
	if msg.Type == message.Notify {
		err = ns.Send(GetPublishTopic(route.Method), marshalledData)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return nil, nil
	}
	if !ns.running {
		return nil, constants.ErrRPCClientNotInitialized
	}
	reply := route.Method + session.UID() + util.NanoID(8)
	sub, err := ns.conn.SubscribeSync(reply)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer sub.Unsubscribe()
	err = ns.conn.Flush()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	err = ns.conn.PublishRequest(GetPublishTopic(route.Method), reply, marshalledData)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	// Wait for a single response
	var responses []*protos.Response
	timeoutPerReply := lo.If(len(timeouts) == 0, ns.reqTimeout).ElseF(func() time.Duration { return timeouts[0] })
	for {
		m, err := sub.NextMsg(timeoutPerReply)
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				err = fmt.Errorf("%w:%s", constants.ErrRPCTimeout, nats.ErrTimeout.Error())
				return responses, errors.WithStack(err)
			} else if errors.Is(err, nats.ErrNoResponders) {
				// 读完最后一个
				logger.Zap.Debug("",
					zap.String("uid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
					zap.String("topic", route.String()),
					zap.Error(err))
				break
			}
			return responses, errors.WithStack(err)
		}
		res := &protos.Response{}
		err = proto.Unmarshal(m.Data, res)
		if err != nil {
			return responses, errors.WithStack(err)
		}
		responses = append(responses, res)
	}
	return responses, nil
}

// Call calls a method remotelly
func (ns *NatsRPCClient) Call(
	ctx context.Context,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
	server *Server,
) (*protos.Response, error) {
	var err error
	spanInfo := &tracing.SpanInfo{
		RpcSystem: "nats",
		IsClient:  true,
		Route:     route,
		PeerID:    server.ID,
		PeerType:  server.Type,
		LocalID:   ns.server.ID,
		LocalType: ns.server.Type,
		RequestID: "",
	}
	ctx = tracing.RPCStartSpan(ctx, spanInfo)
	defer tracing.FinishSpan(ctx, err)

	if !ns.running {
		err = constants.ErrRPCClientNotInitialized
		return nil, errors.WithStack(err)
	}
	req, err := buildRequest(ctx, rpcType, route.String(), session, msg, ns.server)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	marshalledData, err := proto.Marshal(&req)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var m *nats.Msg

	if ns.metricsReporters != nil {
		startTime := time.Now()
		ctx = pcontext.AddListToPropagateCtx(ctx, constants.StartTimeKey, startTime.UnixNano(), constants.RouteKey, route.String())
		defer func() {
			typ := "rpc"
			metrics.ReportTimingFromCtx(ctx, ns.metricsReporters, typ, err)
		}()
	}

	// support notify type.  notify msg don't need wait response
	if msg.Type == message.Notify {
		err = ns.Send(getChannel(server.Type, server.ID), marshalledData)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return &protos.Response{}, nil
	}

	m, err = ns.conn.Request(getChannel(server.Type, server.ID), marshalledData, ns.reqTimeout)
	if err != nil {
		// 针对超时封装一层error便于上层判断
		if errors.Is(err, nats.ErrTimeout) {
			err = constants.ErrRPCTimeout
		}
		return nil, errors.WithStack(err)
	}

	res := &protos.Response{}
	err = proto.Unmarshal(m.Data, res)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if res.Status != nil {
		return nil, apierrors.FromStatus(res.Status).WithStack()
	}
	return res, nil
}

// Fork implement RPCClient.Fork
func (ns *NatsRPCClient) Fork(
	ctx context.Context,
	route *route.Route,
	session session.Session,
	msg *message.Message,
) error {
	msg.Type = message.Notify
	var err error
	spanInfo := &tracing.SpanInfo{
		RpcSystem: "nats",
		IsClient:  true,
		Route:     route,
		LocalID:   ns.server.ID,
		LocalType: ns.server.Type,
		RequestID: "",
	}
	ctx = tracing.RPCStartSpan(ctx, spanInfo)
	defer tracing.FinishSpan(ctx, err)

	if !ns.running {
		err = constants.ErrRPCClientNotInitialized
		return errors.WithStack(err)
	}
	req, err := buildRequest(ctx, protos.RPCType_User, route.String(), session, msg, ns.server)
	if err != nil {
		return errors.WithStack(err)
	}
	marshalledData, err := proto.Marshal(&req)
	if err != nil {
		return errors.WithStack(err)
	}
	if ns.metricsReporters != nil {
		startTime := time.Now()
		ctx = pcontext.AddListToPropagateCtx(ctx, constants.StartTimeKey, startTime.UnixNano(), constants.RouteKey, route.String())
		defer func() {
			typ := "rpc"
			metrics.ReportTimingFromCtx(ctx, ns.metricsReporters, typ, err)
		}()
	}
	return ns.Send(GetForkTopic(route.SvType), marshalledData)
}

// Init inits nats rpc client
func (ns *NatsRPCClient) Init() error {
	ns.running = true
	logger.Log.Debugf("connecting to nats (client) with timeout of %s", ns.connectionTimeout)
	conn, err := setupNatsConn(
		ns.connString,
		ns.appDieChan,
		nats.MaxReconnects(ns.maxReconnectionRetries),
		nats.Timeout(ns.connectionTimeout),
	)
	if err != nil {
		return err
	}
	ns.conn = conn
	return nil
}

// AfterInit runs after initialization
func (ns *NatsRPCClient) AfterInit() {}

// BeforeShutdown runs before shutdown
func (ns *NatsRPCClient) BeforeShutdown() {}

// Shutdown stops nats rpc server
func (ns *NatsRPCClient) Shutdown() error {
	return nil
}

func (ns *NatsRPCClient) stop() {
	ns.running = false
}

func (ns *NatsRPCClient) getSubscribeChannel() string {
	return fmt.Sprintf("pitaya/servers/%s/%s", ns.server.Type, ns.server.ID)
}
