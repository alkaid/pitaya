//
// Copyright (c) TFG Co. All Rights Reserved.
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
	"fmt"
	"net/http"
	"reflect"
	"sync"

	"github.com/alkaid/goerrors/errors"
	"github.com/samber/lo"

	"github.com/topfreegames/pitaya/v2/co"
	"google.golang.org/protobuf/proto"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/topfreegames/pitaya/v2/agent"
	"github.com/topfreegames/pitaya/v2/cluster"
	"github.com/topfreegames/pitaya/v2/component"
	"github.com/topfreegames/pitaya/v2/conn/codec"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/docgenerator"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/pipeline"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/route"
	"github.com/topfreegames/pitaya/v2/router"
	"github.com/topfreegames/pitaya/v2/serialize"
	"github.com/topfreegames/pitaya/v2/session"
	"github.com/topfreegames/pitaya/v2/tracing"
	"github.com/topfreegames/pitaya/v2/util"
	"go.uber.org/zap"
)

// RemoteService struct
type RemoteService struct {
	baseService
	protos.UnimplementedPitayaServer
	rpcServer              cluster.RPCServer
	serviceDiscovery       cluster.ServiceDiscovery
	serializer             serialize.Serializer
	encoder                codec.PacketEncoder
	rpcClient              cluster.RPCClient
	services               map[string]*component.Service // all registered service
	router                 *router.Router
	messageEncoder         message.Encoder
	server                 *cluster.Server // server obj
	sessionPool            session.SessionPool
	handlerPool            *HandlerPool
	remotes                map[string]*component.Remote      // all remote method
	remoteBindingListeners []cluster.RemoteBindingListener   // 绑定发生时的回调，内部使用，仅用于相同serverType间的广播
	remoteSessionListeners []cluster.RemoteSessionListener   // session生命周期监听
	interceptors           map[string]*component.Interceptor // 所有拦截分发器,优先级别高于 remotes
	sysHandlerHooks        *pipeline.HandlerHooks            // 客户端api hook
}

// NewRemoteService creates and return a new RemoteService
func NewRemoteService(
	rpcClient cluster.RPCClient,
	rpcServer cluster.RPCServer,
	sd cluster.ServiceDiscovery,
	encoder codec.PacketEncoder,
	serializer serialize.Serializer,
	router *router.Router,
	messageEncoder message.Encoder,
	server *cluster.Server,
	sessionPool session.SessionPool,
	handlerHooks *pipeline.HandlerHooks,
	sysHandlerHooks *pipeline.HandlerHooks,
	handlerPool *HandlerPool,
) *RemoteService {
	remote := &RemoteService{
		services:               make(map[string]*component.Service),
		rpcClient:              rpcClient,
		rpcServer:              rpcServer,
		encoder:                encoder,
		serviceDiscovery:       sd,
		serializer:             serializer,
		router:                 router,
		messageEncoder:         messageEncoder,
		server:                 server,
		sessionPool:            sessionPool,
		handlerPool:            handlerPool,
		remotes:                make(map[string]*component.Remote),
		remoteBindingListeners: make([]cluster.RemoteBindingListener, 0),
		remoteSessionListeners: make([]cluster.RemoteSessionListener, 0),
		interceptors:           make(map[string]*component.Interceptor),
		sysHandlerHooks:        sysHandlerHooks,
	}

	remote.handlerHooks = handlerHooks

	return remote
}

func (r *RemoteService) remoteProcess(
	ctx context.Context,
	server *cluster.Server,
	a agent.Agent,
	route *route.Route,
	msg *message.Message,
) {
	logW := logger.Zap.With(zap.String("route", route.String()), zap.Any("rpcdata", msg))
	res, err := r.remoteCall(ctx, server, protos.RPCType_Sys, route, a.GetSession(), msg)
	switch msg.Type {
	case message.Request:
		if err != nil {
			a.AnswerWithError(ctx, msg.ID, err)
			return
		}
		err = a.GetSession().ResponseMID(ctx, msg.ID, res.Data)
		if err != nil {
			logW.Error("Failed to respond to remote server", zap.Error(err))
			a.AnswerWithError(ctx, msg.ID, err)
		}
	case message.Notify:
		defer tracing.FinishSpan(ctx, err)
		if err != nil {
			a.AnswerWithError(ctx, msg.ID, err)
			return
		}
	default:
		logW.Error("not support message type", zap.Uint8("msgType", uint8(msg.Type)))
	}
}

// AddRemoteBindingListener 添加绑定发生时的回调，内部使用，仅用于相同serverType间的广播
func (r *RemoteService) AddRemoteBindingListener(bindingListener cluster.RemoteBindingListener) {
	r.remoteBindingListeners = append(r.remoteBindingListeners, bindingListener)
}

// AddRemoteSessionListener 添加session各个生命周期完成时的回调
func (r *RemoteService) AddRemoteSessionListener(sessionListener cluster.RemoteSessionListener) {
	r.remoteSessionListeners = append(r.remoteSessionListeners, sessionListener)
}

func (r *RemoteService) GetRemoteSessionListener() []cluster.RemoteSessionListener {
	return r.remoteSessionListeners
}
func (r *RemoteService) GetRemoteBindingListener() []cluster.RemoteBindingListener {
	return r.remoteBindingListeners
}

// Call processes a remote call
//
//	@implement protos.PitayaServer
func (r *RemoteService) Call(ctx context.Context, req *protos.Request) (*protos.Response, error) {
	rt, err := route.Decode(req.GetMsg().GetRoute())
	if err != nil {
		return nil, err
	}
	spanInfo := tracing.SpanInfoFromRequest(ctx)
	spanInfo.IsClient = false
	spanInfo.Route = rt
	spanInfo.LocalID = r.server.ID
	spanInfo.LocalType = r.server.Type
	c, err := util.GetContextFromRequest(req, r.server.ID)
	c = tracing.RPCStartSpan(c, spanInfo)
	defer tracing.FinishSpan(c, err)
	var res *protos.Response

	if err == nil {
		// 这里不能和官方一样单独线程,调用方已经用co.GoByID包装.
		res = processRemoteMessage(c, req, r)
	}

	if err != nil {
		res = &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
	}

	if res.Status != nil {
		err = apierrors.FromStatus(res.Status)
	}

	defer tracing.FinishSpan(c, err)
	return res, err
}

// Deprecated:Use remote.Sys .SessionBoundFork() instead 由于上层frontend之间的广播方式改走Fork()实现,这里不会再收到响应
//
//	@implement protos.PitayaServer
//	is called when a remote server binds a user session and want us to acknowledge it
//	frontend收到其他frontend实例已经成功绑定session的消息广播时(该广播仅发给所有frontend)
//	具体来说是收到 modules.UniqueSession .Init() 中调用u.rpcClient.BroadcastSessionBind()发送的广播
//	与 remote.Sys .BindSession()不同,具体参见其注释
func (r *RemoteService) SessionBindRemote(ctx context.Context, msg *protos.BindMsg) (*protos.Response, error) {
	for _, r := range r.remoteBindingListeners {
		r.OnUserBind(msg.Uid, msg.Fid)
	}
	return &protos.Response{
		Data: []byte("ack"),
	}, nil
}

// PushToUser sends a push to user
//
//	@implement protos.PitayaServer
func (r *RemoteService) PushToUser(ctx context.Context, push *protos.Push) (*protos.Response, error) {
	logger.Zap.Debug("remote sending push to user", zap.String("uid", push.GetUid()), zap.String("data", string(push.Data)))
	s := r.sessionPool.GetSessionByUID(push.GetUid())
	if s != nil {
		err := s.Push(push.Route, push.Data)
		if err != nil {
			return nil, err
		}
		return &protos.Response{
			Data: []byte("ack"),
		}, nil
	}
	return nil, constants.ErrSessionNotFound
}

// KickUser sends a kick to user
//
//	@implement protos.PitayaServer
//	收到其他服务调用 cluster.RPCClient .SendKick() 时,一般来说是在拿不到 session.Session 只有uid的情况下.
//	与 remote.Sys .Kick()不同，后者用于session的调用
func (r *RemoteService) KickUser(ctx context.Context, kick *protos.KickMsg) (*protos.KickAnswer, error) {
	logger.Log.Debugf("sending kick to user %s", kick.GetUserId())
	s := r.sessionPool.GetSessionByUID(kick.GetUserId())
	if s != nil {
		err := s.Kick(ctx, kick.Metadata)
		if err != nil {
			return nil, err
		}
		return &protos.KickAnswer{
			Kicked: true,
		}, nil
	}
	return nil, constants.ErrSessionNotFound
}

// DoRPC do rpc and get answer
func (r *RemoteService) DoRPC(ctx context.Context, serverID string, route *route.Route, protoData []byte, session session.Session) (*protos.Response, error) {
	msg := &message.Message{
		Type:  message.Request,
		Route: route.Short(),
		Data:  protoData,
	}

	if serverID == "" {
		return r.remoteCall(ctx, nil, protos.RPCType_User, route, session, msg)
	}

	target, _ := r.serviceDiscovery.GetServer(serverID)
	if target == nil {
		return nil, errors.WithStack(constants.ErrServerNotFound)
	}

	return r.remoteCall(ctx, target, protos.RPCType_User, route, session, msg)
}

// DoNotify only support nats,don't use grpc.(copy then modify from DoRPC)
func (r *RemoteService) DoNotify(ctx context.Context, serverID string, route *route.Route, protoData []byte, session session.Session) error {
	co.Go(func() {
		msg := &message.Message{
			Type:  message.Notify,
			Route: route.Short(),
			Data:  protoData,
		}

		if serverID == "" {
			r.remoteCall(ctx, nil, protos.RPCType_User, route, session, msg)
			return
		}

		target, _ := r.serviceDiscovery.GetServer(serverID)
		if target == nil {
			err := constants.ErrServerNotFound
			logger.Zap.Error("notify error",
				zap.String("uid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
				zap.String("route", route.String()),
				zap.Error(err))
			return
		}

		_, err := r.remoteCall(ctx, target, protos.RPCType_User, route, session, msg)
		if err != nil {
			return
		}
	})
	return nil
}

// DoFork only support nats,don't use grpc.(copy then modify from DoRPC)
func (r *RemoteService) DoFork(ctx context.Context, route *route.Route, protoData []byte, session session.Session) error {
	co.Go(func() {
		msg := &message.Message{
			Type:  message.Notify,
			Route: route.Short(),
			Data:  protoData,
		}
		err := r.rpcClient.Fork(ctx, route, session, msg)
		if err != nil {
			logger.Zap.Error("error making fork",
				zap.String("uid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
				zap.Stringer("route", route),
				zap.Error(err))
			return
		}

	})
	return nil
}

func (r *RemoteService) DoPublish(ctx context.Context, topic string, request bool, protoData []byte, session session.Session) ([]*protos.Response, error) {
	topic = cluster.GetPublishTopic(topic)
	route, err := route.Decode(topic)
	if err != nil {
		return nil, err
	}
	msg := &message.Message{
		Type:  lo.If(request, message.Request).Else(message.Notify),
		Route: route.Short(),
		Data:  protoData,
	}
	responses, err := r.rpcClient.Publish(ctx, protos.RPCType_User, route, session, msg)
	if err != nil {
		logger.Zap.Error("error making publish",
			zap.String("uid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
			zap.String("route", route.String()),
			zap.Error(err))
	}
	return responses, err
}

// RPC makes rpcs
func (r *RemoteService) RPC(ctx context.Context, serverID string, route *route.Route, reply proto.Message, arg proto.Message, session session.Session) error {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return errors.WithStack(err)
		}
	}
	res, err := r.DoRPC(ctx, serverID, route, data, session)
	if err != nil {
		return err
	}

	if res.Status != nil {
		return apierrors.FromStatus(res.Status)
	}

	if reply != nil {
		err = proto.Unmarshal(res.GetData(), reply)
		if err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

// Notify only support nats,don't use grpc.(copy then modify from RPC)
func (r *RemoteService) Notify(ctx context.Context, serverID string, ro *route.Route, arg proto.Message, session session.Session) error {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return err
		}
	}
	err = r.DoNotify(ctx, serverID, ro, data, session)
	if err != nil {
		return err
	}
	return nil
}

// NotifyAll 通知集群内所有服务,不包括自己
//
//	@receiver r
//	@param ctx
//	@param ro
//	@param self
//	@param arg
//	@param session
//	@return error
func (r *RemoteService) NotifyAll(ctx context.Context, ro *route.Route, self *cluster.Server, arg proto.Message, session session.Session) error {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return err
		}
	}
	if ro.SvType != "" {
		return errors.WithStack(constants.ErrNotifyAllSvTypeNotEmpty)
	}
	// 服务器为空 全局广播(每种服务器只有一个实例消费)
	for _, server := range r.serviceDiscovery.GetServerTypes() {
		// 排除自己
		if server.Type == self.Type {
			continue
		}
		// 排除session未绑定的sessionStickness服务
		if server.SessionStickiness && session != nil {
			svId := session.GetBackendID(server.Type)
			if svId == "" {
				logger.Zap.Debug("NotifyAll ignore unbound sessionStickness server", zap.String("server", server.Type))
				continue
			}
		}
		newRoute, err := route.Decode(server.Type + "." + ro.Short())
		if err != nil {
			return err
		}
		r.DoNotify(ctx, "", newRoute, data, session)
	}
	return nil
}

func (r *RemoteService) Fork(ctx context.Context, ro *route.Route, arg proto.Message, session session.Session) error {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return err
		}
	}
	err = r.DoFork(ctx, ro, data, session)
	if err != nil {
		return err
	}
	return nil
}

func (r *RemoteService) Publish(ctx context.Context, topic string, request bool, arg proto.Message, session session.Session) ([]*protos.Response, error) {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return nil, err
		}
	}
	return r.DoPublish(ctx, topic, request, data, session)
}

// Register registers components
func (r *RemoteService) Register(comp component.Component, opts []component.Option) error {
	s := component.NewService(comp, opts)

	if _, ok := r.services[s.Name]; ok {
		return fmt.Errorf("remote: service already defined: %s", s.Name)
	}

	if err := s.ExtractRemote(); err != nil {
		return err
	}

	r.services[s.Name] = s
	// register all remotes
	for name, remote := range s.Remotes {
		r.remotes[fmt.Sprintf("%s.%s", s.Name, name)] = remote
	}

	return nil
}

// RegisterInterceptor 注册拦截分发器,优先级别高于 component.Remote
//
//	@receiver r
//	@param serviceName
//	@param interceptor
func (r *RemoteService) RegisterInterceptor(serviceName string, interceptor *component.Interceptor) {
	r.interceptors[serviceName] = interceptor
}

func processRemoteMessage(ctx context.Context, req *protos.Request, r *RemoteService) *protos.Response {
	rt, err := route.Decode(req.GetMsg().GetRoute())
	if err != nil {
		response := &protos.Response{
			Status: &apierrors.Status{
				Code:    http.StatusBadRequest,
				Message: "cannot decode route",
				Metadata: map[string]string{
					"route": req.GetMsg().GetRoute(),
				},
			},
		}
		return response
	}

	switch {
	case req.Type == protos.RPCType_Sys:
		return r.handleRPCSys(ctx, req, rt)
	case req.Type == protos.RPCType_User:
		return r.handleRPCUser(ctx, req, rt)
	default:
		return &protos.Response{
			Status: &apierrors.Status{
				Code:    http.StatusBadRequest,
				Message: "invalid rpc type",
				Metadata: map[string]string{
					"route": req.GetMsg().GetRoute(),
				},
			},
		}
	}
}

func (r *RemoteService) handleRPCUser(ctx context.Context, req *protos.Request, rt *route.Route) *protos.Response {
	response := &protos.Response{}

	// 拦截器优先工作
	interceptor, ok := r.interceptors[rt.Service]
	if ok {
		var err error
		var sess session.Session
		if req.Session != nil {
			a, err := agent.NewRemote(
				req.GetSession(), // 内部会优先从sessionPool中取session
				"",               // 服务器内部rpc 不需要 agent.Remote.ResponseMID()功能
				r.rpcClient,
				r.encoder,
				r.serializer,
				r.serviceDiscovery,
				req.FrontendID,
				r.messageEncoder,
				r.sessionPool,
			)
			if err != nil {
				logger.Log.Warn("pitaya/handler: cannot instantiate remote agent")
				response := &protos.Response{
					Status: &apierrors.FromError(err).Status,
				}
				return response
			}
			sess = a.Session
		}
		// 和 handleRPCSys 调用的 handlerPool.ProcessHandlerMessage 一样处理，把session存入context
		if sess != nil {
			ctx = context.WithValue(ctx, constants.SessionCtxKey, sess)
			ctx = util.CtxWithDefaultLogger(ctx, rt.String(), sess.UID())
		}
		var ret any
		// 若启用了单线程反应堆模型,则派发到全局Looper单例
		if interceptor.EnableReactor {
			ret, err = co.LooperInstance.Async(ctx, func(ctx context.Context, coroutine co.Coroutine) (any, error) {
				return interceptor.InterceptorFun(ctx, *rt, req.GetMsg().GetData())
			}).Wait(ctx)
		} else {
			ret, err = interceptor.InterceptorFun(ctx, *rt, req.GetMsg().GetData())
		}
		if err != nil {
			response := &protos.Response{
				Status: &apierrors.FromError(err).Status,
			}
			return response
		}

		var b []byte
		if ret != nil {
			pb, ok := ret.(proto.Message)
			if !ok {
				response := &protos.Response{
					Status: &apierrors.FromError(constants.ErrWrongValueType).Status,
				}
				return response
			}
			if b, err = proto.Marshal(pb); err != nil {
				response := &protos.Response{
					Status: &apierrors.FromError(err).Status,
				}
				return response
			}
		}

		response.Data = b
		return response
	}

	// 无拦截器情况下走常规remote
	remote, ok := r.remotes[rt.Short()]
	if !ok {
		// notify 情况下很多 route 都会找不到，警告太多
		logger.Zap.Info("pitaya/remote: router not found", zap.String("route", rt.Short()))
		response := &protos.Response{
			Status: &apierrors.Status{
				Code:    http.StatusNotFound,
				Message: "route not found",
				Metadata: map[string]string{
					"route": rt.Short(),
				},
			},
		}
		return response
	}
	var arg interface{}
	var err error
	receiver := remote.Receiver
	if remote.Options.ReceiverProvider != nil {
		rec := remote.Options.ReceiverProvider(ctx)
		if rec == nil {
			logger.Log.Warnf("pitaya/remote: %s not found,the ReceiverProvider return nil", rt.Short())
			response := &protos.Response{
				Status: &apierrors.Status{
					Code:    http.StatusNotFound,
					Message: "route not found,ReceiverProvider return nil",
					Metadata: map[string]string{
						"route": rt.Short(),
					},
				},
			}
			return response
		}
		receiver = reflect.ValueOf(rec)
	}
	var sess session.Session
	if req.Session != nil {
		a, err := agent.NewRemote(
			req.GetSession(), // 内部会优先从sessionPool中取session
			"",               // 服务器内部rpc 不需要 agent.Remote.ResponseMID()功能
			r.rpcClient,
			r.encoder,
			r.serializer,
			r.serviceDiscovery,
			req.FrontendID,
			r.messageEncoder,
			r.sessionPool,
		)
		if err != nil {
			logger.Log.Warn("pitaya/handler: cannot instantiate remote agent")
			response := &protos.Response{
				Status: &apierrors.FromError(err).Status,
			}
			return response
		}
		sess = a.Session
	}
	// 和 handleRPCSys 调用的 handlerPool.ProcessHandlerMessage 一样处理，把session存入context
	if sess != nil {
		ctx = context.WithValue(ctx, constants.SessionCtxKey, sess)
		ctx = util.CtxWithDefaultLogger(ctx, rt.String(), sess.UID())
	}
	params := []reflect.Value{receiver, reflect.ValueOf(ctx)}
	if remote.HasArgs {
		arg, err = unmarshalRemoteArg(remote, req.GetMsg().GetData())
		if err != nil {
			response := &protos.Response{
				Status: &apierrors.Status{
					Code:    http.StatusNotFound,
					Message: err.Error(),
					Metadata: map[string]string{
						"route": rt.Short(),
					},
				},
			}
			return response
		}
		params = append(params, reflect.ValueOf(arg))
	}
	// 和 handlerPool.ProcessHandlerMessage 一样加入hook处理
	var arg2 any
	if remote.Options.EnableReactor {
		arg2, err = co.LooperInstance.Async(ctx, func(ctx context.Context, coroutine co.Coroutine) (any, error) {
			ctx, arg2, err = r.handlerHooks.BeforeHandler.ExecuteBeforePipeline(ctx, rt, arg)
			return arg2, err
		}).Wait(ctx)
	} else {
		ctx, arg2, err = r.handlerHooks.BeforeHandler.ExecuteBeforePipeline(ctx, rt, arg)
	}
	if err != nil {
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
	}
	arg = arg2.(proto.Message)
	var ret any
	// 若启用了单线程反应堆模型,则派发到全局Looper单例
	if remote.Options.EnableReactor {
		ret, err = co.LooperInstance.Async(ctx, func(ctx context.Context, coroutine co.Coroutine) (any, error) {
			return util.Pcall(remote.Method, params)
		}).Wait(ctx)
	} else if remote.Options.TaskGoProvider != nil {
		// 若提供了自定义派发线程
		var wg sync.WaitGroup
		wg.Add(1)
		remote.Options.TaskGoProvider(ctx, func() {
			ret, err = util.Pcall(remote.Method, params)
			wg.Done()
		})
		wg.Wait()
	} else {
		ret, err = util.Pcall(remote.Method, params)
	}
	if err != nil {
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
	}
	if remote.Options.EnableReactor {
		ret, err = co.LooperInstance.Async(ctx, func(ctx context.Context, coroutine co.Coroutine) (any, error) {
			return r.handlerHooks.AfterHandler.ExecuteAfterPipeline(ctx, rt, arg, ret, err)
		}).Wait(ctx)
	} else {
		ret, err = r.handlerHooks.AfterHandler.ExecuteAfterPipeline(ctx, rt, arg, ret, err)
	}
	if err != nil {
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
	}

	var b []byte
	if ret != nil {
		pb, ok := ret.(proto.Message)
		if !ok {
			response := &protos.Response{
				Status: &apierrors.FromError(constants.ErrWrongValueType).Status,
			}
			return response
		}
		if b, err = proto.Marshal(pb); err != nil {
			response := &protos.Response{
				Status: &apierrors.FromError(err).Status,
			}
			return response
		}
	}

	response.Data = b
	return response
}

func (r *RemoteService) handleRPCSys(ctx context.Context, req *protos.Request, rt *route.Route) *protos.Response {
	reply := req.GetMsg().GetReply()
	response := &protos.Response{}
	// (warning) a new agent is created for every new request
	a, err := agent.NewRemote(
		req.GetSession(),
		reply,
		r.rpcClient,
		r.encoder,
		r.serializer,
		r.serviceDiscovery,
		req.FrontendID,
		r.messageEncoder,
		r.sessionPool,
	)
	if err != nil {
		logger.Log.Warn("pitaya/handler: cannot instantiate remote agent")
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
	}

	ret, err := r.handlerPool.ProcessHandlerMessage(ctx, rt, r.serializer, r.sysHandlerHooks, a.Session, req.GetMsg().GetData(), req.GetMsg().GetType(), true)
	if err != nil {
		logger.Zap.Warn("", zap.Error(err))
		response = &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
	} else {
		response = &protos.Response{Data: ret}
	}
	return response
}

func (r *RemoteService) remoteCall(
	ctx context.Context,
	server *cluster.Server,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
) (*protos.Response, error) {
	svType := route.SvType

	var err error
	target := server

	if target == nil {
		target, err = r.router.Route(ctx, rpcType, svType, route, msg, session)
		if err != nil {
			return nil, apierrors.FromError(err)
		}
	}

	res, err := r.rpcClient.Call(ctx, rpcType, route, session, msg, target)
	if err != nil {
		code := apierrors.Code(err)
		logFun := lo.If(code >= http.StatusInternalServerError, logger.Zap.Error).Else(logger.Zap.Warn)
		logFun("error making call to target",
			zap.String("uid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
			zap.Stringer("route", route), zap.String("host", target.Hostname), zap.String("svID", target.ID),
			zap.Error(err))
		return nil, err
	}
	return res, err
}

// DumpServices outputs all registered services
func (r *RemoteService) DumpServices() {
	for name := range r.remotes {
		logger.Log.Infof("registered remote %s", name)
	}
}

// Docs returns documentation for remotes
func (r *RemoteService) Docs(getPtrNames bool) (map[string]interface{}, error) {
	if r == nil {
		return map[string]interface{}{}, nil
	}
	return docgenerator.RemotesDocs(r.server.Type, r.services, getPtrNames)
}
