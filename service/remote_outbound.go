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
	"fmt"
	"net/http"

	goerrors "github.com/alkaid/goerrors/errors"
	"github.com/samber/lo"
	"google.golang.org/protobuf/proto"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/agent"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/tracing"
	"github.com/alkaid/pitaya/v2/util"
	"go.uber.org/zap"
)

func (r *RemoteService) remoteProcess(
	ctx context.Context,
	server *cluster.Server,
	a agent.Agent,
	route *route.Route,
	msg *message.Message,
) {
	res, err := r.remoteCall(ctx, server, protos.RPCType_Sys, route, a.GetSession(), msg)
	switch msg.Type {
	case message.Request:
		if err != nil {
			a.AnswerWithError(ctx, msg.ID, err)
			return
		}
		err = a.GetSession().ResponseMID(ctx, msg.ID, res.Data)
		if err != nil {
			util.GetLoggerFromCtx(ctx).Error("Failed to respond to remote server", zap.Error(err), zap.String("route", route.String()), zap.Any("rpcdata", msg))
			a.AnswerWithError(ctx, msg.ID, err)
		}
	case message.Notify:
		defer tracing.FinishSpan(ctx, err)
		if err != nil {
			a.AnswerWithError(ctx, msg.ID, err)
			return
		}
	default:
		util.GetLoggerFromCtx(ctx).Error("not support message type", zap.Uint8("msgType", uint8(msg.Type)), zap.String("route", route.String()), zap.Any("rpcdata", msg))
	}
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
		return nil, goerrors.WithStack(fmt.Errorf("%w svID=%s", constants.ErrServerNotFound, serverID))
	}

	return r.remoteCall(ctx, target, protos.RPCType_User, route, session, msg)
}

// DoNotify only support nats,don't use grpc.(copy then modify from DoRPC)
func (r *RemoteService) DoNotify(ctx context.Context, serverID string, route *route.Route, protoData []byte, session session.Session) error {
	msg := &message.Message{
		Type:  message.Notify,
		Route: route.Short(),
		Data:  protoData,
	}

	if serverID == "" {
		_, err := r.remoteCall(ctx, nil, protos.RPCType_User, route, session, msg)
		return err
	}

	target, _ := r.serviceDiscovery.GetServer(serverID)
	if target == nil {
		err := fmt.Errorf("%w svID=%s", constants.ErrServerNotFound, serverID)
		logger.Zap.Error("notify error",
			zap.String("uid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
			zap.String("route", route.String()),
			zap.Error(err))
		return err
	}

	_, err := r.remoteCall(ctx, target, protos.RPCType_User, route, session, msg)
	return err
}

// RPC makes rpcs
func (r *RemoteService) RPC(ctx context.Context, serverID string, route *route.Route, reply proto.Message, arg proto.Message, session session.Session) error {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return goerrors.WithStack(err)
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
			return goerrors.WithStack(err)
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

func (r *RemoteService) remoteCall(
	ctx context.Context,
	server *cluster.Server,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
) (*protos.Response, error) {
	var err error
	target := server

	if target == nil {
		target, err = r.resolveRemoteTarget(ctx, rpcType, route, session, msg)
		if err != nil {
			return nil, err
		}
	}
	if session != nil && !target.Frontend && !rpccontext.RPCFanoutFromContext(ctx) {
		mode := rpccontext.StatefulRouteModeDirect
		if _, assigned := target.StatefulOwnerFenceSnapshot(); assigned {
			mode = rpccontext.StatefulRouteModeAssigned
		}
		ctx = rpccontext.WithStatefulRouteMode(ctx, mode)
	}
	ctx = target.WithOwnerFencingMetadata(ctx)
	ctx = cluster.PrepareOutboundRPCContext(ctx)

	res, err := r.rpcClient.Call(ctx, rpcType, route, session, msg, target)
	if err != nil {
		logg := logger.Zap.With(util.LogFieldsFromCtx(ctx)...)
		code := apierrors.Code(err)
		logFun := lo.If(code >= http.StatusInternalServerError, logg.Error).Else(logg.Warn)
		logFun("error making call to target",
			zap.String("targetUid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
			zap.Stringer("targetRoute", route), zap.String("host", target.Hostname), zap.String("svID", target.ID),
			zap.Error(err))
		return nil, err
	}
	return res, err
}

func (r *RemoteService) resolveRemoteTarget(
	ctx context.Context,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
) (*cluster.Server, error) {
	target, err := r.router.Route(ctx, rpcType, route.SvType, route, msg, session)
	if err == nil && target == nil {
		err = fmt.Errorf("%w server=%s", constants.ErrServerNotFound, route.SvType)
	}
	if err == nil {
		return target, nil
	}

	apiErr := apierrors.FromError(err)
	if rpcType == protos.RPCType_Sys {
		logW := util.GetLoggerFromCtx(ctx)
		logFun := lo.If(int(apiErr.Status.Code) >= http.StatusInternalServerError, logW.Error).Else(logW.Warn)
		logFun("Failed to respond to remote server", zap.String("route", route.String()), zap.Error(err))
	}
	return nil, apiErr
}
