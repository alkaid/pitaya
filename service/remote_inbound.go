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
	"net/http"
	"reflect"

	"google.golang.org/protobuf/proto"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/agent"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/tracing"
	"github.com/alkaid/pitaya/v2/util"
	"go.uber.org/zap"
)

// Call processes a remote call
//
//	@implement protos.PitayaServer
func (r *RemoteService) Call(ctx context.Context, req *protos.Request) (*protos.Response, error) {
	if err := validateOwnerFence(ctx, r.server); err != nil {
		return rpcErrorResponse(err), err
	}
	statefulAdmission, err := r.admitStatefulOwner(ctx, req)
	if err != nil {
		return rpcErrorResponse(err), err
	}
	key, err := idempotencyKeyFromContext(ctx)
	if err != nil {
		return rpcErrorResponse(err), err
	}
	reliableDigest, reliableJob, err := reliableRPCJobMetadata(pcontext.ToMap(ctx), key)
	if err != nil {
		return rpcErrorResponse(err), err
	}
	fingerprint := ""
	if key != "" {
		fingerprint, err = rpcRequestFingerprint(req, key, reliableJob, reliableDigest)
		if err != nil {
			return rpcErrorResponse(err), err
		}
	} else if err := validateUnkeyedReliableRPCMetadata(req); err != nil {
		return rpcErrorResponse(err), err
	}
	handlerCtx, err := consumeRPCAdmissionMetadata(ctx)
	if err != nil {
		return rpcErrorResponse(err), err
	}
	if statefulAdmission.admitted() {
		handlerCtx = withRPCStatefulOwnerInfo(handlerCtx, statefulAdmission.info)
	}
	admissionCtx := ctx
	guard := newRPCAdmissionGuard(func(invocationCtx context.Context) error {
		if err := validateOwnerFence(admissionCtx, r.server); err != nil {
			return err
		}
		return statefulAdmission.revalidate(invocationCtx, serverType(r.server))
	})
	handlerCtx = withRPCAdmissionGuard(handlerCtx, guard)
	if key == "" || r.idempotencyStore == nil {
		return r.callOnce(handlerCtx, req)
	}
	scope, err := idempotencyStoreKey(ctx, r.server, key)
	if err != nil {
		return rpcErrorResponse(err), err
	}
	handlerPropagation := pcontext.ToMap(handlerCtx)
	response, callErr := r.idempotencyStore.Execute(handlerCtx, scope, fingerprint, func(callCtx context.Context) (*protos.Response, error) {
		if admissionErr := guard.check(callCtx); admissionErr != nil {
			return rpcErrorResponse(admissionErr), admissionErr
		}
		if callCtx == nil {
			callCtx = context.Background()
		}
		callCtx = rpccontext.WithoutReliableRPCJob(callCtx)
		callCtx = context.WithValue(callCtx, constants.PropagateCtxKey, handlerPropagation)
		callCtx = withRPCAdmissionGuard(callCtx, guard)
		if statefulAdmission.admitted() {
			callCtx = withRPCStatefulOwnerInfo(callCtx, statefulAdmission.info)
		}
		callCtx = withRPCIdempotencyInfo(callCtx, scope, fingerprint)
		return r.callOnce(callCtx, req)
	})
	if callErr != nil && (response == nil || response.Status == nil) {
		response = rpcErrorResponse(callErr)
	}
	return response, callErr
}

func (r *RemoteService) callOnce(ctx context.Context, req *protos.Request) (*protos.Response, error) {
	var res *protos.Response
	var err error
	logg := util.GetLoggerFromCtx(ctx)
	defer func() {
		if err != nil {
			// code := http.StatusInternalServerError
			// if res != nil {
			// 	code = int(res.Status.Code)
			// }
			// logFun := lo.If(code >= http.StatusInternalServerError, logg.Error).Else(logg.Warn)
			// 这里由于error转换了两次stack trace会丢失,详细log堆栈必须业务层打印
			logg.Warn("error calling rpc service,upstream must print stacktrace", zap.String("cause", err.Error()))
		}
	}()
	rt, err := route.Decode(req.GetMsg().GetRoute())
	if err != nil {
		return nil, err
	}
	spanInfo := tracing.SpanInfoFromRequest(ctx)
	spanInfo.IsClient = false
	spanInfo.Route = rt
	spanInfo.LocalID = r.server.ID
	spanInfo.LocalType = r.server.Type
	// 上层nats_rpc_server已经调用过,无需重复
	// c, err := util.GetContextFromRequest(req, r.server.ID)
	c := tracing.RPCStartSpan(ctx, spanInfo)
	defer tracing.FinishSpan(c, err)

	if err == nil {
		// 这里不能和官方一样单独线程,调用方已经用co.GoByID包装.
		res = processRemoteMessage(c, req, r)
	}

	if admissionErr := rpcAdmissionRejectionFromContext(c); admissionErr != nil {
		err = admissionErr
		res = rpcErrorResponse(admissionErr)
	} else if err != nil {
		res = &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
	} else if res.Status != nil {
		err = apierrors.FromStatus(res.Status)
	}

	return res, err
}

func rpcErrorResponse(err error) *protos.Response {
	return &protos.Response{Status: &apierrors.FromError(err).Status}
}

// Deprecated:Use remote.Sys .SessionBoundFork() instead 由于上层frontend之间的广播方式改走Fork()实现,这里不会再收到响应
//
//	@implement protos.PitayaServer
//	is called when a remote server binds a user session and want us to acknowledge it
//	frontend收到其他frontend实例已经成功绑定session的消息广播时(该广播仅发给所有frontend)
//	具体来说是收到 modules.UniqueSession .Init() 中调用u.rpcClient.BroadcastSessionBind()发送的广播
//	与 remote.Sys .BindSession()不同,具体参见其注释
func (r *RemoteService) SessionBindRemote(ctx context.Context, msg *protos.BindMsg) (*protos.Response, error) {
	for _, r := range r.remoteListenerOwner.bindingSnapshot() {
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
		err := s.Kick(ctx, kick.Metadata, session.CloseReason(kick.Reason))
		if err != nil {
			return nil, err
		}
		return &protos.KickAnswer{
			Kicked: true,
		}, nil
	}
	return nil, constants.ErrSessionNotFound
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
	admissionCtx := ctx

	// 拦截器优先工作
	interceptor, ok := r.remoteRegistryOwner.lookupInterceptor(rt.Service)
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
				req.Type,
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
		if err := validateRPCAdmissionAtInvocation(admissionCtx, ctx); err != nil {
			response.Status = &apierrors.FromError(err).Status
			return response
		}
		ret, err = interceptor.InterceptorFun(ctx, *rt, req.GetMsg().GetData())
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
	remote, ok := r.remoteRegistryOwner.lookupRemote(rt.Short())
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
	receiver := remote.Receiver
	if remote.Options.ReceiverProvider != nil {
		if admissionErr := validateRPCAdmissionAtInvocation(admissionCtx, ctx); admissionErr != nil {
			response.Status = &apierrors.FromError(admissionErr).Status
			return response
		}
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
			req.Type,
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

	var ret interface{}
	var arg interface{}
	var err error

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
	}

	if admissionErr := validateRPCAdmissionAtInvocation(admissionCtx, ctx); admissionErr != nil {
		response.Status = &apierrors.FromError(admissionErr).Status
		return response
	}
	ctx, arg, err = r.remoteHooks.BeforeHandler.ExecuteBeforePipeline(ctx, rt, arg)
	if err != nil {
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
	}

	params := []reflect.Value{receiver, reflect.ValueOf(ctx)}
	if remote.HasArgs {
		params = append(params, reflect.ValueOf(arg))
	}
	// Remote Handler 的 ret/err 仍由原有 RPC 流程处理；executionErr 只表示执行器调度或完成协议失败。
	// executeTask 会等待已开始的闭包收敛，返回后读取 ret/err 不会和后台 task 发生数据竞争。
	executionErr := executeTask(ctx, remote.Options.TaskExecutor, func(taskCtx context.Context) error {
		if admissionErr := validateRPCAdmissionAtInvocation(admissionCtx, taskCtx); admissionErr != nil {
			return admissionErr
		}
		// 执行器可能为队列任务派生新的 context，反射参数需要同步替换。
		params[1] = reflect.ValueOf(taskCtx)
		ret, err = util.Pcall(remote.Method, params)
		// 业务错误通过外层 err 进入 AfterHandler 和 RPC status；这里不把它混入执行器错误。
		return nil
	})
	if executionErr != nil {
		if IsRPCAdmissionRejected(executionErr) {
			response.Status = &apierrors.FromError(executionErr).Status
			return response
		}
		if acknowledgeSkippedTask(req.GetMsg().GetType(), executionErr, rt, "remote") {
			response.Data = []byte("ack")
			return response
		}
		response.Status = &apierrors.FromError(normalizeTaskExecutionError(executionErr)).Status
		return response
	}

	ret, err = r.remoteHooks.AfterHandler.ExecuteAfterPipeline(ctx, rt, arg, ret, err)
	if err != nil {
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
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
		req.Type,
	)
	if err != nil {
		logger.Log.Warn("pitaya/handler: cannot instantiate remote agent")
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
	}

	ret, err := r.handlerPool.ProcessHandlerMessage(ctx, rt, r.serializer, r.handlerHooks, a.Session, req.GetMsg().GetData(), req.GetMsg().GetType(), true)
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
