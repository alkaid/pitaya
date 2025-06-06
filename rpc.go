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

package pitaya

import (
	"context"
	"github.com/alkaid/goerrors/apierrors"
	"github.com/topfreegames/pitaya/v2/protos"
	"reflect"

	"github.com/topfreegames/pitaya/v2/logger"
	"go.uber.org/zap"

	agent2 "github.com/topfreegames/pitaya/v2/agent"
	"github.com/topfreegames/pitaya/v2/session"

	"github.com/topfreegames/pitaya/v2/config"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/route"
	"google.golang.org/protobuf/proto"
)

// RPC calls a method in a different server
func (app *App) RPC(ctx context.Context, routeStr string, reply proto.Message, arg proto.Message, uid string) error {
	return app.doSendRPC(ctx, "", routeStr, reply, arg, uid)
}

func (app *App) RpcRaw(ctx context.Context, serverID, routeStr string, arg []byte, uid string) ([]byte, error) {
	return app.doSendRPCRaw(ctx, serverID, routeStr, arg, uid)
}

// RPCTo send a rpc to a specific server
func (app *App) RPCTo(ctx context.Context, serverID, routeStr string, reply proto.Message, arg proto.Message) error {
	return app.doSendRPC(ctx, serverID, routeStr, reply, arg, "")
}

// Notify
//
//	@implement Pitaya.Notify
func (app *App) Notify(ctx context.Context, routeStr string, arg proto.Message, uid string) error {
	return app.doSendNotify(ctx, "", routeStr, arg, uid)
}

// NotifyAll
//
//	@implement Pitaya.NotifyAll
func (app *App) NotifyAll(ctx context.Context, routeStr string, arg proto.Message, uid string) error {
	return app.doSendNotifyAll(ctx, routeStr, arg, uid)
}

// Fork implement Pitaya.Fork
func (app *App) Fork(ctx context.Context, routeStr string, arg proto.Message, uid string) error {
	_, err := app.doFork(ctx, true, routeStr, arg, uid)
	return err
}

// ForkRequest implement Pitaya.ForkRequest
func (app *App) ForkRequest(ctx context.Context, routeStr string, arg proto.Message, uid string) ([]*protos.Response, error) {
	return app.doFork(ctx, false, routeStr, arg, uid)
}

func (app *App) PublishRequest(ctx context.Context, routeStr string, arg proto.Message, uid string) ([]*protos.Response, error) {
	return app.doPublish(ctx, routeStr, false, arg, uid)
}

func (app *App) Publish(ctx context.Context, routeStr string, arg proto.Message, uid string) error {
	_, err := app.doPublish(ctx, routeStr, true, arg, uid)
	return err
}

// NotifyTo
//
//	@implement Pitaya.NotifyTo
func (app *App) NotifyTo(ctx context.Context, serverID, routeStr string, arg proto.Message) error {
	return app.doSendNotify(ctx, serverID, routeStr, arg, "")
}

// ReliableRPC enqueues RPC to worker so it's executed asynchronously
// Default enqueue options are used
func (app *App) ReliableRPC(
	routeStr string,
	metadata map[string]interface{},
	reply, arg proto.Message,
) (jid string, err error) {
	return app.worker.EnqueueRPC(routeStr, metadata, reply, arg)
}

// ReliableRPCWithOptions enqueues RPC to worker
// Receive worker options for this specific RPC
func (app *App) ReliableRPCWithOptions(
	routeStr string,
	metadata map[string]interface{},
	reply, arg proto.Message,
	opts *config.EnqueueOpts,
) (jid string, err error) {
	return app.worker.EnqueueRPCWithOptions(routeStr, metadata, reply, arg, opts)
}

func (app *App) doSendRPC(ctx context.Context, serverID, routeStr string, reply proto.Message, arg proto.Message, uid string) error {
	if app.rpcServer == nil {
		return constants.ErrRPCServerNotInitialized
	}

	if reflect.TypeOf(reply).Kind() != reflect.Ptr {
		return constants.ErrReplyShouldBePtr
	}

	r, err := route.Decode(routeStr)
	if err != nil {
		return err
	}

	if r.SvType == "" {
		return constants.ErrNoServerTypeChosenForRPC
	}

	if (r.SvType == app.server.Type && serverID == "") || serverID == app.server.ID {
		return constants.ErrNonsenseRPC
	}
	var sess session.Session = nil
	if uid != "" {
		sess, err = app.imperfectSessionForRPC(ctx, uid)
		if err != nil {
			return err
		}
	}
	return app.remoteService.RPC(ctx, serverID, r, reply, arg, sess)
}

func (app *App) doSendRPCRaw(ctx context.Context, serverID, routeStr string, arg []byte, uid string) ([]byte, error) {
	if app.rpcServer == nil {
		return nil, constants.ErrRPCServerNotInitialized
	}

	r, err := route.Decode(routeStr)
	if err != nil {
		return nil, err
	}

	if r.SvType == "" {
		return nil, constants.ErrNoServerTypeChosenForRPC
	}

	if (r.SvType == app.server.Type && serverID == "") || serverID == app.server.ID {
		return nil, constants.ErrNonsenseRPC
	}
	var sess session.Session = nil
	if uid != "" {
		sess, err = app.imperfectSessionForRPC(ctx, uid)
		if err != nil {
			return nil, err
		}
	}
	res, err := app.remoteService.DoRPC(ctx, serverID, r, arg, sess)
	if err != nil {
		return nil, err
	}
	if res.Status != nil {
		return nil, apierrors.FromStatus(res.Status)
	}
	return res.Data, nil
}

func (app *App) doSendNotifyAll(ctx context.Context, routeStr string, arg proto.Message, uid string) error {
	r, err := route.Decode(routeStr)
	if err != nil {
		return err
	}
	if r.SvType != "" {
		return constants.ErrNotifyAllSvTypeNotEmpty
	}
	if app.rpcServer == nil {
		return constants.ErrRPCServerNotInitialized
	}

	var sess session.Session = nil
	if uid != "" {
		sess, err = app.imperfectSessionForRPC(ctx, uid)
		if err != nil {
			return err
		}
	}
	return app.remoteService.NotifyAll(ctx, r, arg, sess)
}

// doSendNotify only support nats,don't use grpc.(copy then modify from doSendRPC)
func (app *App) doSendNotify(ctx context.Context, serverID, routeStr string, arg proto.Message, uid string) error {
	if app.rpcServer == nil {
		return constants.ErrRPCServerNotInitialized
	}

	r, err := route.Decode(routeStr)
	if err != nil {
		return err
	}

	if (r.SvType == app.server.Type && serverID == "") || serverID == app.server.ID {
		return constants.ErrNonsenseRPC
	}

	var sess session.Session = nil
	if uid != "" {
		sess, err = app.imperfectSessionForRPC(ctx, uid)
		if err != nil {
			return err
		}
	}
	return app.remoteService.Notify(ctx, serverID, r, arg, sess)
}

func (app *App) doFork(ctx context.Context, notify bool, routeStr string, arg proto.Message, uid string) ([]*protos.Response, error) {
	if app.rpcServer == nil {
		return nil, constants.ErrRPCServerNotInitialized
	}

	r, err := route.Decode(routeStr)
	if err != nil {
		return nil, err
	}

	if r.SvType == "" {
		return nil, constants.ErrNoServerTypeChosenForRPC
	}
	var sess session.Session = nil
	if uid != "" {
		sess, err = app.imperfectSessionForRPC(ctx, uid)
		if err != nil {
			return nil, err
		}
	}
	return app.remoteService.Fork(ctx, notify, r, arg, sess)
}

func (app *App) doPublish(ctx context.Context, routeStr string, notify bool, arg proto.Message, uid string) ([]*protos.Response, error) {
	if app.rpcClient == nil {
		return nil, constants.ErrRPCClientNotInitialized
	}
	r, err := route.Decode(routeStr)
	if err != nil {
		return nil, err
	}
	var sess session.Session = nil
	if uid != "" {
		sess, err = app.imperfectSessionForRPC(ctx, uid)
		if err != nil {
			return nil, err
		}
	}
	return app.remoteService.Publish(ctx, r, notify, arg, sess)
}

// imperfectSessionForRPC 为rpc调用获取一个可能不健全的session
//
//	"不健全"指session的agent仅有少数 networkentity.NetworkEntity 方法
//	- 本地pool有session返回session(数据和cluster是同步的)
//	- context里有session返回session(bind后的session数据是和cache cluster同步的,因为从网关转发时打包了数据)
//	- 上述都不存在时从cache cluster获取缓存数据构建一个不健全session
//	- 数据不存在时返回 [constants.ErrSessionNotFound]
//	@receiver app
//	@param ctx
//	@param uid
//	@return session.Session 若cache数据不存在，可能返回一个各自段都是零值的session
//	@return error
func (app *App) imperfectSessionForRPC(ctx context.Context, uid string) (session.Session, error) {
	if uid == "" {
		return nil, constants.ErrIllegalUID
	}
	sess := app.sessionPool.GetSessionByUID(uid)
	if sess != nil {
		return sess, nil
	}
	sess2 := app.GetSessionFromCtx(ctx)
	if sess2 != nil && sess2.UID() == uid {
		return sess2.(session.Session), nil
	}
	agent, err := agent2.NewCluster(uid, app.sessionPool, app.rpcClient, app.serializer, app.serviceDiscovery)
	if err != nil {
		logger.Zap.Error("use session from cluster error", zap.String("uid", uid), zap.Error(err))
		return nil, err
	}
	return agent.Session, nil
}
