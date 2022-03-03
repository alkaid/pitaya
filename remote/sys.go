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

package remote

import (
	"context"
	"github.com/topfreegames/pitaya/v2/cluster"
	"github.com/topfreegames/pitaya/v2/component"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/route"
	"github.com/topfreegames/pitaya/v2/service"
	"github.com/topfreegames/pitaya/v2/session"
)

// Sys contains logic for handling sys remotes
type Sys struct {
	component.Base
	sessionPool     session.SessionPool
	server          *cluster.Server
	serverDiscovery cluster.ServiceDiscovery
	rpcClient       cluster.RPCClient
	remote          *service.RemoteService
}

// NewSys returns a new Sys instance
func NewSys(sessionPool session.SessionPool, server *cluster.Server, serverDiscovery cluster.ServiceDiscovery, client cluster.RPCClient, remoteService *service.RemoteService) *Sys {
	return &Sys{sessionPool: sessionPool, server: server, serverDiscovery: serverDiscovery, rpcClient: client, remote: remoteService}
}

// Init initializes the module
func (sys *Sys) Init() {
	//自己服务器的session绑定回调(本地)
	sys.sessionPool.OnSessionBind(func(ctx context.Context, s session.Session) error {
		//绑定当前frontendID 其实这里只可能是frontend 不用判断考虑stateful backend的处理
		if !sys.server.Frontend {
			return nil
		}
		//从redis同步backend bind数据到本地
		s.ObtainFromCluster()
		s.SetFrontendData(sys.server.ID, s.ID())
		//同步到redis, //TODO 错误处理
		err := s.Flush2Cluster()
		if err != nil {
			return err
		}
		//通知所有server已经成功绑定
		r, err := route.Decode(constants.SessionBoundRoute)
		if err != nil {
			return err
		}
		msg := &protos.BindMsg{
			Uid: s.UID(),
			Fid: sys.server.ID,
			Sid: s.ID(),
		}
		err = sys.remote.Notify(ctx, "", r, msg, s)
		return err
	})
	//移除frontendID 同步redis
	sys.sessionPool.OnSessionClose(func(s session.Session, reason session.CloseReason) {
		//解绑当前frontendID 其实这里只可能是frontend 不用判断考虑stateful backend的处理
		if !sys.server.Frontend {
			return
		}
		//重绑定发起的kick不做处理
		if reason == session.CloseReasonRebind {
			return
		}
		//与stateful backend不同,frontend的绑定数据无须清除
		//通知所有 server
		r, err := route.Decode(constants.SessionClosedRoute)
		if err != nil {
			logger.Log.Error(err)
			return
		}
		msg := &protos.KickMsg{
			UserId: s.UID(),
		}
		err = sys.remote.Notify(context.Background(), "", r, msg, s)
		if err != nil {
			logger.Log.Error(err)
			return
		}
		//这里只可能是frontend 不再考虑stateful backend的处理
	})
	sys.sessionPool.OnBindBackend(func(ctx context.Context, s session.Session, serverType, serverId string) error {
		if sys.server.ID == serverId {
			//session要绑定的就是本服,开始处理
			//已经绑定过 报错
			if sys.sessionPool.GetSessionByUID(s.UID()) != nil {
				return constants.ErrSessionAlreadyBound
			}
			//本地存储
			sys.sessionPool.StoreSessionLocal(s)
			//同步到redis
			s.Flush2Cluster()
			//通知所有服务器
			msg := &protos.BindBackendMsg{
				Uid:   s.UID(),
				Btype: serverType,
				Bid:   sys.server.ID,
			}
			//fork本类型服所有实例 然后通知所有其他类型服
			r, _ := route.Decode(sys.server.Type + "." + constants.SessionBoundBackendRoute)
			err := sys.remote.Fork(ctx, r, msg, s)
			if err != nil {
				return err
			}
			for _, sv := range sys.serverDiscovery.GetServerTypes() {
				r, _ := route.Decode(sv.Type + "." + constants.SessionBoundBackendRoute)
				err = sys.remote.Notify(ctx, "", r, msg, s)
				if err != nil {
					return err
				}
			}
		} else {
			//目标服不是本服 转发给目标服
			r, err := route.Decode(constants.SessionBindBackendRoute)
			if err != nil {
				return err
			}
			msg := &protos.BindBackendMsg{
				Uid:   s.UID(),
				Btype: serverType,
				Bid:   sys.server.ID,
			}
			sys.remote.Notify(ctx, serverId, r, msg, s)
		}
		return nil
	})
	sys.sessionPool.OnKickBackend(func(ctx context.Context, s session.Session, serverType, serverId string, reason session.CloseReason) error {
		if sys.server.ID == serverId {
			//session要绑定的就是本服,开始处理
			//本地存储
			sys.sessionPool.RemoveSessionLocal(s)
			//重绑定发起的kick不继续处理
			if reason == session.CloseReasonRebind {
				return nil
			}
			//同步到redis
			s.Flush2Cluster()
			//通知所有服务器
			r, err := route.Decode(constants.SessionKickedBackendRoute)
			if err != nil {
				return err
			}
			msg := &protos.BindBackendMsg{
				Uid:   s.UID(),
				Btype: serverType,
				Bid:   sys.server.ID,
			}
			err = sys.remote.Notify(ctx, "", r, msg, s)
		} else {
			//目标服不是本服 转发给目标服
			r, err := route.Decode(constants.KickBackendRoute)
			if err != nil {
				return err
			}
			msg := &protos.BindBackendMsg{
				Uid:   s.UID(),
				Btype: serverType,
				Bid:   sys.server.ID,
			}
			sys.remote.Notify(ctx, serverId, r, msg, s)
		}
		return nil
	})
	return
}

// BindSession binds the local session
func (s *Sys) BindSession(ctx context.Context, sessionData *protos.Session) (*protos.Response, error) {
	sess := s.sessionPool.GetSessionByID(sessionData.Id)
	if sess == nil {
		return nil, constants.ErrSessionNotFound
	}
	if err := sess.Bind(ctx, sessionData.Uid); err != nil {
		return nil, err
	}
	return &protos.Response{Data: []byte("ack")}, nil
}

// PushSession updates the local session
func (s *Sys) PushSession(ctx context.Context, sessionData *protos.Session) (*protos.Response, error) {
	sess := s.sessionPool.GetSessionByID(sessionData.Id)
	if sess == nil {
		return nil, constants.ErrSessionNotFound
	}
	if err := sess.SetDataEncoded(sessionData.Data); err != nil {
		return nil, err
	}
	return &protos.Response{Data: []byte("ack")}, nil
}

// Kick kicks a local user
func (s *Sys) Kick(ctx context.Context, msg *protos.KickMsg) (*protos.KickAnswer, error) {
	res := &protos.KickAnswer{
		Kicked: false,
	}
	sess := s.sessionPool.GetSessionByUID(msg.GetUserId())
	if sess == nil {
		return res, constants.ErrSessionNotFound
	}
	err := sess.Kick(ctx)
	if err != nil {
		return res, err
	}
	res.Kicked = true
	return res, nil
}

//BindBackendSession 收到转发来的绑定backend请求
//  @Description: bind local session in stateful backend OR update local session bind data in frontend 响应绑定session到backend的请求
//  @receiver s
//  @param ctx
//  @param sessionData
//  @return *protos.Response
//  @return error
func (s *Sys) BindBackendSession(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	sess := s.sessionPool.GetSessionByUID(msg.Uid)
	if sess == nil {
		sess = s.getSessionFromCtx(ctx)
		if sess == nil && sess.UID() != msg.Uid {
			logger.Log.Error(constants.ErrSessionNotFound.Error())
			return nil, constants.ErrSessionNotFound
		}
	}
	if msg.Btype != s.server.Type || msg.Bid != s.server.ID {
		logger.Log.Error(constants.ErrIllegalBindBackendID.Error())
		return nil, constants.ErrIllegalBindBackendID
	}
	if err := sess.BindBackend(ctx, s.server.Type, s.server.ID); err != nil {
		return nil, err
	}
	return &protos.Response{Data: []byte("ack")}, nil
}

func (s *Sys) KickBackend(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	sess := s.sessionPool.GetSessionByUID(msg.Uid)
	if sess == nil {
		return nil, constants.ErrSessionNotFound
	}
	err := sess.KickBackend(ctx, s.server.Type)
	return &protos.Response{Data: []byte("ack")}, err
}

func (s *Sys) SessionBound(ctx context.Context, msg *protos.BindMsg) (*protos.Response, error) {
	//修改session数据
	sess := s.sessionPool.GetSessionByUID(msg.Uid)
	if sess != nil {
		sess.SetFrontendData(msg.Fid, msg.Sid)
	}
	for _, r := range s.remote.GetRemoteSessionListener() {
		r.OnUserBind(msg.Uid, msg.Fid)
	}
	return &protos.Response{Data: []byte("ack")}, nil
}
func (s *Sys) SessionClosed(ctx context.Context, msg *protos.KickMsg) (*protos.Response, error) {
	for _, r := range s.remote.GetRemoteSessionListener() {
		r.OnUserDisconnect(msg.UserId)
	}
	return &protos.Response{Data: []byte("ack")}, nil
}
func (s *Sys) SessionBoundBackend(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	for _, r := range s.remote.GetRemoteBindingListener() {
		r.OnUserBindBackend(msg.Uid, msg.Btype, msg.Bid)
	}
	//排除除了自己的同类backend
	if msg.Btype != s.server.Type || msg.Bid == s.server.ID {
		for _, r := range s.remote.GetRemoteSessionListener() {
			r.OnUserBindBackend(msg.Uid, msg.Btype, msg.Bid)
		}
	}
	return &protos.Response{Data: []byte("ack")}, nil

}
func (s *Sys) SessionKickedBackend(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	for _, r := range s.remote.GetRemoteSessionListener() {
		r.OnUserUnBindBackend(msg.Uid, msg.Btype, msg.Bid)
	}
	return &protos.Response{Data: []byte("ack")}, nil
}
func (s *Sys) getSessionFromCtx(ctx context.Context) session.Session {
	sessionVal := ctx.Value(constants.SessionCtxKey)
	if sessionVal == nil {
		logger.Log.Debug("ctx doesn't contain a session, are you calling GetSessionFromCtx from inside a remote?")
		return nil
	}
	return sessionVal.(session.Session)
}
