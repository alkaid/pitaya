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

	"github.com/pkg/errors"

	"github.com/topfreegames/pitaya/v2/co"

	"github.com/topfreegames/pitaya/v2/cluster"
	"github.com/topfreegames/pitaya/v2/component"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/route"
	"github.com/topfreegames/pitaya/v2/service"
	"github.com/topfreegames/pitaya/v2/session"
	"go.uber.org/zap"
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
	// 自己服务器的session绑定回调(本地)
	sys.sessionPool.OnSessionBind(func(ctx context.Context, s session.Session, callback map[string]string) error {
		if !sys.server.Frontend {
			// 非frontend的转发逻辑在 session.Session.Bind() 内部
			logger.Zap.Debug("session bind not called by frontend", zap.String("server", sys.server.Type))
			return nil
		}
		// 绑定当前frontendID
		var err error
		oldSession := sys.sessionPool.GetSessionByUID(s.UID())
		if oldSession != nil {
			err = oldSession.Kick(ctx, nil, session.CloseReasonKickRebind)
			if err != nil {
				return err
			}
		}
		olddata := s.GetDataEncoded()
		for i := 0; i < 1; i++ {
			// 从redis同步backend bind数据到本地
			err = s.ObtainFromCluster()
			if err != nil {
				break
			}
			s.SetFrontendData(sys.server.ID, s.ID())
			// 同步到redis
			err = s.Flush2Cluster()
			if err != nil {
				break
			}
			// 通知所有server已经成功绑定
			var r *route.Route
			msg := &protos.BindMsg{
				Uid:      s.UID(),
				Fid:      sys.server.ID,
				Sid:      s.ID(),
				Metadata: callback,
			}
			// 广播逻辑从 modules.UniqueSession 移到此处,原广播方法改用新的Fork方法
			// err = sys.rpcClient.BroadcastSessionBind(s.UID())
			r, err = route.Decode(sys.server.Type + "." + constants.SessionBoundForkRoute)
			if err != nil {
				break
			}
			// 通知所有frontend实例
			err = sys.remote.Fork(ctx, r, msg, s)
			if err != nil {
				break
			}
			// 通知所有其他服务
			r, err = route.Decode(constants.SessionBoundRoute)
			if err != nil {
				break
			}
			err = sys.remote.NotifyAll(ctx, r, sys.server, msg, s)
		}
		if err != nil {
			// 回滚
			// TODO 这里回滚的处理过于粗暴,后期考虑标志出上面的逻辑进行到哪一步了,根据不同的进度做不同的回滚策略,比如如果已经同步到redis，那就要回滚redis
			s.SetDataEncoded(olddata)
			logW := logger.Zap.With(zap.Int64("sid", s.ID()), zap.String("uid", s.UID()))
			logW.Error("session binding error", zap.Error(err))
			return err
		}
		return err
	})
	// 网关本地session关闭回调,通知其他服务器
	sys.sessionPool.OnSessionClose(func(s session.Session, callback map[string]string, reason session.CloseReason) {
		if !sys.server.Frontend {
			// 这个分支逻辑永远都不应该走进来
			logger.Zap.Error("", zap.Error(constants.ErrDeveloperLogicFatal))
			return
		}
		// 重绑定发起的kick不做处理
		if reason == session.CloseReasonKickRebind {
			return
		}
		// 与stateful backend不同,frontend的绑定数据无须清除
		// 通知所有 server
		logW := logger.Zap.With(zap.Int64("sid", s.ID()), zap.String("uid", s.UID()))
		r, err := route.Decode(constants.SessionClosedRoute)
		if err != nil {
			logW.Error("session on close error", zap.Error(err))
			return
		}
		msg := &protos.KickMsg{
			UserId:   s.UID(),
			Metadata: callback,
		}
		err = sys.remote.NotifyAll(context.Background(), r, sys.server, msg, s)
		if err != nil {
			logW.Error("session on close error", zap.Error(err))
			return
		}
		// 这里只可能是frontend 不再考虑stateful backend的处理
	})
	sys.sessionPool.OnBindBackend(func(ctx context.Context, s session.Session, serverType, serverId string, callback map[string]string) error {
		msg := &protos.BindBackendMsg{
			Uid:      s.UID(),
			Btype:    serverType,
			Bid:      serverId,
			Metadata: callback,
		}
		var err error
		var r *route.Route
		// 严禁网关调用
		if sys.server.Frontend {
			return constants.ErrDeveloperLogicFatal
		}
		// 目标服不是本服 转发给目标服
		if sys.server.ID != serverId {
			r, err = route.Decode(serverType + "." + constants.SessionBindBackendRoute)
			if err != nil {
				return err
			}
			err = sys.remote.RPC(ctx, serverId, r, &protos.Response{}, msg, s)
			if err != nil {
				return err
			}
		}
		for i := 0; i < 1; i++ {
			// session要绑定的就是本服,开始处理
			// 已经绑定过 报错
			if sys.sessionPool.GetSessionByUID(s.UID()) != nil {
				err = errors.WithStack(constants.ErrSessionAlreadyBound)
				break
			}
			// 本地存储
			err = sys.sessionPool.StoreSessionLocal(s)
			if err != nil {
				break
			}
			// 通知网关同步数据
			err = s.PushToFront(ctx)
			if err != nil {
				break
			}
			// 通知所有服务器
			// fork本类型服所有实例 然后通知所有其他类型服
			r, err = route.Decode(serverType + "." + constants.SessionBoundBackendForkRoute)
			if err != nil {
				break
			}
			err = sys.remote.Fork(ctx, r, msg, s)
			if err != nil {
				break
			}
			r, err = route.Decode(constants.SessionBoundBackendRoute)
			if err != nil {
				break
			}
			err = sys.remote.NotifyAll(ctx, r, sys.server, msg, s)
		}
		if err != nil {
			// 回滚
			// TODO 后期考虑标志出上面的逻辑进行到哪一步了,根据不同的进度做不同的回滚策略,比如如果已经同步到redis，那就要回滚redis
			logW := logger.Zap.With(zap.Int64("sid", s.ID()), zap.String("uid", s.UID()))
			logW.Error("session binding backend error", zap.Error(err))
			return err
		}
		return nil
	})
	sys.sessionPool.OnKickBackend(func(ctx context.Context, s session.Session, serverType, serverId string, callback map[string]string, reason session.CloseReason) error {
		msg := &protos.BindBackendMsg{
			Uid:      s.UID(),
			Btype:    serverType,
			Bid:      serverId,
			Metadata: callback,
		}
		var err error
		var r *route.Route
		// 严禁网关调用
		if sys.server.Frontend {
			return constants.ErrDeveloperLogicFatal
		}
		logW := logger.Zap.With(zap.Int64("sid", s.ID()), zap.String("uid", s.UID()), zap.Int("reason", reason), zap.String("sv", serverType), zap.String("svID", serverId))
		// 目标服不是本服 转发给目标服成功后再转发网关
		if sys.server.ID != serverId {
			// 转发目标服
			r, err = route.Decode(serverType + "." + constants.KickBackendRoute)
			if err != nil {
				return err
			}
			err = sys.remote.RPC(ctx, serverId, r, &protos.Response{}, msg, s)
			if err == nil {
				return nil
			}
			// 若目标服已经挂掉,不报错,继续后续的数据清理以及通知网关
			if errors.Is(err, constants.ErrServerNotFound) {
				logW.Info("backend target down,ignore error", zap.Error(err))
			} else {
				return err
			}
		}
		// 本服就是目标服或者目标服已经狗带，处理绑定状态并通知网关
		for i := 0; i < 1; i++ {
			// session要绑定的就是本服,开始处理
			// 本地存储
			if sys.server.ID == serverId {
				sys.sessionPool.RemoveSessionLocal(s)
			}
			// 重绑定发起的kick不继续处理
			if reason == session.CloseReasonKickRebind {
				return nil
			}
			// 通知网关同步数据
			err = s.PushToFront(ctx)
			if err != nil {
				// 网关报错session找不到,说明网关的session已经下线,忽略该错误并补偿redis清除步骤
				if errors.Is(err, protos.ErrSessionNotFound()) {
					logW.Debug("frontend return ErrSessionNotFound,the session must be closed. ignore the error then clean cluster's cache")
					err = s.Flush2Cluster()
				}
				if err != nil {
					break
				}
			}
			// 通知所有服务器 notify也是rpc调用，会导致frontend的session被SetDataEncoded,数据被同步
			r, err = route.Decode(constants.SessionKickedBackendRoute)
			if err != nil {
				break
			}
			err = sys.remote.NotifyAll(ctx, r, sys.server, msg, s)
		}
		if err != nil {
			// 回滚
			// TODO 后期考虑标志出上面的逻辑进行到哪一步了,根据不同的进度做不同的回滚策略,比如如果已经同步到redis，那就要回滚redis

			logW.Error("session kick backend error", zap.Error(err))
			return err
		}
		return nil
	})
}

// BindSession binds the local session
//
//	@see constants.SessionBindRoute
//	frontend收到非frotend服务转发的 session.Session .Bind()请求时
//	与 service.RemoteService .SessionBindRemote()不同,具体参见其注释
func (s *Sys) BindSession(ctx context.Context, bindMsg *protos.BindMsg) (*protos.Response, error) {
	// func (s *Sys) BindSession(ctx context.Context, sessionData *protos.Session) (*protos.Response, error) {
	sess := s.sessionPool.GetSessionByID(bindMsg.Sid)
	if sess == nil {
		return nil, constants.ErrSessionNotFound
	}
	if err := sess.Bind(ctx, bindMsg.Uid, bindMsg.Metadata); err != nil {
		return nil, err
	}
	return &protos.Response{Data: []byte("ack")}, nil
}

// PushSession updates the local session
//
//	@see constants.SessionPushRoute
//	@receiver s
//	@param ctx
//	@param sessionData
//	@return *protos.Response
//	@return error
func (s *Sys) PushSession(ctx context.Context, sessionData *protos.Session) (*protos.Response, error) {
	sess := s.sessionPool.GetSessionByID(sessionData.Id)
	if sess == nil {
		// return nil, constants.ErrSessionNotFound
		return nil, protos.ErrSessionNotFound()
	}
	if err := sess.SetDataEncoded(sessionData.Data); err != nil {
		return nil, err
	}
	err := sess.Flush2Cluster()
	if err != nil {
		return nil, err
	}
	return &protos.Response{Data: []byte("ack")}, nil
}

// Kick kicks a local user
//
//	@see constants.KickRoute
//	frontend收到非frotend服务转发的 session.Session .Kick()请求时
//	与 service.RemoteService .KickUser()不同, 后者用于拿不到 session.Session 只有uid的条件下用rpcClient发送的情况
func (s *Sys) Kick(ctx context.Context, msg *protos.KickMsg) (*protos.KickAnswer, error) {
	res := &protos.KickAnswer{
		Kicked: false,
	}
	sess := s.sessionPool.GetSessionByUID(msg.GetUserId())
	if sess == nil {
		return res, constants.ErrSessionNotFound
	}
	err := sess.Kick(ctx, msg.Metadata, session.CloseReason(msg.Reason))
	if err != nil {
		return res, err
	}
	res.Kicked = true
	return res, nil
}

// BindBackendSession 收到转发来的绑定backend请求
//
//	@see constants.SessionBindBackendRoute
//	@receiver s
//	@param ctx
//	@param sessionData
//	@return *protos.Response
//	@return error
func (s *Sys) BindBackendSession(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	// 不能是网关收到
	if s.server.Frontend {
		return nil, constants.ErrDeveloperLogicFatal
	}
	// 当前服不是目标服 报错
	if msg.Btype != s.server.Type || msg.Bid != s.server.ID {
		logger.Zap.Error(constants.ErrIllegalBindBackendID.Error())
		return nil, constants.ErrIllegalBindBackendID
	}
	sess := s.sessionPool.GetSessionByUID(msg.Uid)
	if sess == nil {
		sess = s.getSessionFromCtx(ctx)
		if sess == nil || sess.UID() != msg.Uid {
			logger.Zap.Error(constants.ErrSessionNotFound.Error())
			return nil, constants.ErrSessionNotFound
		}
	}
	if err := sess.BindBackend(ctx, msg.Btype, msg.Bid, msg.Metadata); err != nil {
		return nil, err
	}
	return &protos.Response{Data: []byte("ack")}, nil
}

// KickBackend 收到转发来的 session.Session .KickBackend()请求
//
//	@see constants.KickBackendRoute
//	@receiver s
//	@param ctx
//	@param msg
//	@return *protos.Response
//	@return error
func (s *Sys) KickBackend(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	// 不能是网关收到
	if s.server.Frontend {
		return nil, constants.ErrDeveloperLogicFatal
	}
	// 当前服不是目标服 报错
	if msg.Btype != s.server.Type || msg.Bid != s.server.ID {
		logger.Zap.Error(constants.ErrIllegalBindBackendID.Error())
		return nil, constants.ErrIllegalBindBackendID
	}
	sess := s.sessionPool.GetSessionByUID(msg.Uid)
	if sess == nil {
		// 找不到 没绑定过 无法解绑
		return nil, constants.ErrSessionNotFound
	}
	err := sess.KickBackend(ctx, msg.Btype, msg.Metadata)
	return &protos.Response{Data: []byte("ack")}, err
}

// SessionBoundFork 收到frontend的session绑定的fork通知
//
//	@see constants.SessionBoundForkRoute
//	@receiver s
//	@param ctx
//	@param msg
//	@return *protos.Response
//	@return error
func (s *Sys) SessionBoundFork(ctx context.Context, msg *protos.BindMsg) (*protos.Response, error) {
	for _, r := range s.remote.GetRemoteBindingListener() {
		// 框架已注册的回调有 modules.UniqueSession
		r.OnUserBind(msg.Uid, msg.Fid)
	}
	return &protos.Response{
		Data: []byte("ack"),
	}, nil
}

// SessionBound 收到frontend的session已绑定通知
//
//	@see constants.SessionBoundRoute
//	@receiver s
//	@param ctx
//	@param msg
//	@return *protos.Response
//	@return error
func (s *Sys) SessionBound(ctx context.Context, msg *protos.BindMsg) (*protos.Response, error) {
	// 修改session数据
	sess := s.sessionPool.GetSessionByUID(msg.Uid)
	if sess != nil {
		sess.SetFrontendData(msg.Fid, msg.Sid)
	}
	for _, r := range s.remote.GetRemoteSessionListener() {
		co.GoByUID(msg.Uid, func() {
			r.OnUserBound(ctx, msg.Uid, msg.Fid, msg.Metadata)
		})
	}
	return &protos.Response{Data: []byte("ack")}, nil
}
func (s *Sys) SessionClosed(ctx context.Context, msg *protos.KickMsg) (*protos.Response, error) {
	for _, r := range s.remote.GetRemoteSessionListener() {
		co.GoByUID(msg.UserId, func() {
			r.OnUserDisconnected(ctx, msg.UserId, msg.Metadata)
		})
	}
	return &protos.Response{Data: []byte("ack")}, nil
}
func (s *Sys) SessionBoundBackendFork(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	for _, r := range s.remote.GetRemoteBindingListener() {
		r.OnUserBindBackend(msg.Uid, msg.Btype, msg.Bid)
	}
	return &protos.Response{Data: []byte("ack")}, nil
}
func (s *Sys) SessionBoundBackend(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	for _, r := range s.remote.GetRemoteSessionListener() {
		co.GoByUID(msg.Uid, func() {
			r.OnUserBoundBackend(ctx, msg.Uid, msg.Btype, msg.Bid, msg.Metadata)
		})
	}
	return &protos.Response{Data: []byte("ack")}, nil

}
func (s *Sys) SessionKickedBackend(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	for _, r := range s.remote.GetRemoteSessionListener() {
		co.GoByUID(msg.Uid, func() {
			r.OnUserUnboundBackend(ctx, msg.Uid, msg.Btype, msg.Bid, msg.Metadata)
		})
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
