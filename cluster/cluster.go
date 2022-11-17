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

	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/constants"
	pcontext "github.com/topfreegames/pitaya/v2/context"
	"github.com/topfreegames/pitaya/v2/interfaces"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/route"
	"github.com/topfreegames/pitaya/v2/session"
)

// RPCServer interface
type RPCServer interface {
	SetPitayaServer(protos.PitayaServer)
	interfaces.Module
}

// RPCClient interface
type RPCClient interface {
	Send(route string, data []byte) error
	SendPush(userID string, frontendSv *Server, push *protos.Push) error
	SendKick(userID string, serverType string, kick *protos.KickMsg) error
	// Deprecated:Use Fork instead
	BroadcastSessionBind(uid string) error
	Call(ctx context.Context, rpcType protos.RPCType, route *route.Route, session session.Session, msg *message.Message, server *Server) (*protos.Response, error)
	// Fork 广播给同类型实例
	//  @param ctx
	//  @param route
	//  @param session
	//  @param msg
	//  @return error
	Fork(ctx context.Context, route *route.Route, session session.Session, msg *message.Message) error
	interfaces.Module
}

// SDListener interface
type SDListener interface {
	AddServer(*Server)
	RemoveServer(*Server)
	// ModifyServer server数据改变时
	//  @param sv 新server
	//  @param old 旧server
	ModifyServer(sv *Server, old *Server)
}

// RemoteBindingListener listens to session bindings in remote servers
//
// 框架内部使用
type RemoteBindingListener interface {
	// OnUserBind frontend 收到 session所在frontend实例绑定后的广播通知
	//  @param uid
	//  @param fid 网关id
	OnUserBind(uid, fid string)
	// OnUserBindBackend sessionsticky backend 收到相同type的backend实例绑定后的广播通知
	//  @param uid
	//  @param serverType
	//  @param serverId
	OnUserBindBackend(uid, serverType, serverId string)
}

// RemoteSessionListener session生命周期监听
type RemoteSessionListener interface {
	// OnUserBound 用户成功绑定网关时
	//  @param uid
	//  @param fid 网关id
	OnUserBound(ctx context.Context, uid, fid string, callback map[string]string)
	// OnUserDisconnected 用户断线时
	//  @param uid
	OnUserDisconnected(ctx context.Context, uid string, callback map[string]string)
	// OnUserBoundBackend 用户成功绑定backend服务器时
	//  @param uid
	//  @param serverType
	//  @param serverId
	OnUserBoundBackend(ctx context.Context, uid, serverType, serverId string, callback map[string]string)
	// OnUserUnboundBackend 用户成功解绑backend服务器时
	//  @param uid
	//  @param serverType
	//  @param serverId
	OnUserUnboundBackend(ctx context.Context, uid, serverType, serverId string, callback map[string]string)
}

// InfoRetriever gets cluster info
// It can be implemented, for exemple, by reading
// env var, config or by accessing the cluster API
type InfoRetriever interface {
	Region() string
}

// Action type for enum
type Action int

// Action values
const (
	ADD Action = iota
	DEL
	Modify
)

func buildRequest(
	ctx context.Context,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
	thisServer *Server,
) (protos.Request, error) {
	var err error
	req := protos.Request{
		Type: rpcType,
		Msg: &protos.Msg{
			Route: route.String(),
			Data:  msg.Data,
		},
	}
	ctx = pcontext.AddListToPropagateCtx(ctx, constants.PeerIDKey, thisServer.ID, constants.PeerServiceKey, thisServer.Type)
	req.Metadata, err = pcontext.Encode(ctx)
	if err != nil {
		return req, err
	}
	if thisServer.Frontend {
		req.FrontendID = thisServer.ID
		// 设置frontend信息到 session.data,以保证转发后decode的session.data中有frontend信息
		// TODO 本来应该在 agentImpl 实例化时设置,实在没有可以传入frontendID的地方
		if session != nil {
			session.SetFrontendData(thisServer.ID, session.ID())
		}
	}

	switch msg.Type {
	case message.Request:
		req.Msg.Type = protos.MsgType_MsgRequest
	case message.Notify:
		req.Msg.Type = protos.MsgType_MsgNotify
	}

	if rpcType == protos.RPCType_Sys {
		mid := uint(0)
		if msg.Type == message.Request {
			mid = msg.ID
		}
		req.Msg.Id = uint64(mid)
	}

	// 无论是 RPCType_Sys 还是 RPCType_User 只要传入了session就带数据过去
	if session != nil {
		req.Session = &protos.Session{
			Id:   session.ID(),
			Uid:  session.UID(),
			Data: session.GetDataEncoded(),
		}
	}
	return req, nil
}
