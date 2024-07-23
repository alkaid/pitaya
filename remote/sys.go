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
	"fmt"
	"strconv"

	"github.com/pkg/errors"
	"github.com/topfreegames/pitaya/v2/util"

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
		// 剔除旧的session数据,若有
		var err error
		oldSession := sys.sessionPool.GetSessionByUID(s.UID())
		if oldSession != nil {
			err = oldSession.Kick(ctx, nil, session.CloseReasonKickRebind)
			if err != nil {
				return err
			}
		}
		logW := logger.Zap.With(zap.Int64("sid", s.ID()), zap.String("uid", s.UID()))
		// 从redis同步backend bind数据到本地
		err = s.InitialFromCluster()
		if err != nil {
			logW.Error("session binding error", zap.Error(err))
			return err
		}
		// 同步到redis
		err = s.FlushFrontendData()
		if err != nil {
			logW.Error("session binding error", zap.Error(err))
			return err
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
			// 不可能走到该分支
			logW.Error("session binding error", zap.Error(err))
			return err
		}
		// 通知所有frontend实例
		_, err = sys.remote.Fork(ctx, true, r, msg, s)
		if err != nil {
			logW.Warn("session bind fork error", zap.Error(err))
			// 非致命异常 继续
		}
		// 通知所有其他服务
		r, err = route.Decode(constants.SessionBoundRoute)
		if err != nil {
			logW.Warn("session bind notify error", zap.Error(err))
			// 非致命异常 继续
		}
		err = sys.remote.NotifyAll(ctx, r, msg, s)
		if err != nil {
			logW.Warn("session bind notify error", zap.Error(err))
			// 非致命异常 继续
		}
		return nil
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
		logW := logger.Zap.With(zap.Int64("sid", s.ID()), zap.String("uid", s.UID()))
		// 与stateful backend不同,frontend的绑定数据无须清除
		if s.UID() != "" {
			err := s.FlushOnline()
			if err != nil {
				logW.Error("session on close error", zap.Error(err))
				return
			}
		}
		// 通知所有 server
		r, err := route.Decode(constants.SessionClosedRoute)
		if err != nil {
			logW.Error("session on close error", zap.Error(err))
			return
		}
		msg := &protos.KickMsg{
			UserId:   s.UID(),
			Metadata: callback,
		}
		err = sys.remote.NotifyAll(context.Background(), r, msg, s)
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
			return errors.WithStack(constants.ErrDeveloperLogicFatal)
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
		// session要绑定的就是本服,开始处理
		logErr := func(err error) {
			logger.Zap.Error("session binding backend error", zap.Error(err), zap.Int64("sid", s.ID()), zap.String("uid", s.UID()), zap.String("svType", serverType), zap.String("svID", serverId))
		}
		rollback := func(err error) {
			// 回滚 清理本地缓存 清理redis
			s.RemoveBackendID(serverType)
			sys.sessionPool.RemoveSessionLocal(s)
			err2 := s.FlushBackendData()
			if err2 != nil {
				logErr(err2)
			}
			logErr(err)
		}
		// 已经绑定过 警告 或略错误
		existsSession := sys.sessionPool.GetSessionByUID(s.UID())
		if existsSession != nil {
			err = errors.WithStack(fmt.Errorf("%w:found in pool,uid=%s,bound=%s,target=%s", constants.ErrSessionAlreadyBound, s.UID(), existsSession.GetBackendID(serverType), serverId))
			logger.Zap.Error("", zap.Error(err))
			return nil
		}
		// 本地存储
		err = sys.sessionPool.StoreSessionLocal(s)
		if err != nil {
			rollback(err)
			return err
		}
		// 通知网关同步数据
		_, err = s.SendRequestToFrontend(ctx, constants.SessionBindBackendRoute, msg)
		if err != nil {
			// 同步失败(网关狗带或session已经下线) 回滚
			rollback(err)
			return err
		}
		// 后续流程都可忽略错误
		// 通知所有服务器
		// fork本类型服所有实例 然后通知所有其他类型服
		r, err = route.Decode(serverType + "." + constants.SessionBoundBackendForkRoute)
		if err != nil {
			logErr(err)
			return nil
		}
		_, err = sys.remote.Fork(ctx, true, r, msg, s)
		if err != nil {
			logErr(err)
			return nil
		}
		r, err = route.Decode(constants.SessionBoundBackendRoute)
		if err != nil {
			logErr(err)
			return nil
		}
		err = sys.remote.NotifyAll(ctx, r, msg, s)
		if err != nil {
			logErr(err)
			return nil
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
			// 若目标服已经挂掉,不报错,代替目标服继续后续的数据清理以及通知网关
			if errors.Is(err, constants.ErrServerNotFound) {
				logW.Info("backend target down,ignore error", zap.Error(err))
			} else {
				return err
			}
		}
		// 本服就是目标服或者目标服已经狗带(代替目标服)，处理绑定状态并通知网关
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
		_, err = s.SendRequestToFrontend(ctx, constants.KickBackendRoute, msg)
		if err != nil {
			// 报错session找不到,说明网关的session已经下线,不管是session找不到还是网关狗带等其他错误都应该忽略该错误并代替网关执行redis清除步骤
			// if  errors.Is(err, protos.ErrSessionNotFound()) {
			logW.Warn("the session may be closed or frontend is down. ignore the error then clean cluster's cache", zap.Error(err))
			err = s.FlushBackendData()
			if err != nil {
				return err
			}
			// }
		}
		// 后续流程都可以忽略错误
		// 通知所有服务器
		r, err = route.Decode(constants.SessionKickedBackendRoute)
		if err != nil {
			logW.Error("session kick backend error,ignore", zap.Error(err))
			return nil
		}
		err = sys.remote.NotifyAll(ctx, r, msg, s)
		if err != nil {
			logW.Error("session kick backend error,ignore", zap.Error(err))
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
		return nil, protos.ErrSessionNotFound().WithMetadata(map[string]string{"uid": bindMsg.Uid, "sid": strconv.Itoa(int(bindMsg.Sid)), "fid": bindMsg.Fid}).WithStack()
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
//	@param sessionData 废弃,使用context session
//	@return *protos.Response
//	@return error
func (s *Sys) PushSession(ctx context.Context, sessionData *protos.Session) (*protos.Response, error) {
	sess := s.sessionPool.GetSessionByID(sessionData.Id)
	if sess == nil {
		return nil, protos.ErrSessionNotFound().WithMetadata(map[string]string{"uid": sessionData.Uid, "sid": strconv.Itoa(int(sessionData.Id))}).WithStack()
	}
	if sess.UID() != sessionData.Uid {
		// 不应该到该分支,到这说明有bug
		return nil, fmt.Errorf("uid not equal.src=%d,dest=%d", sess.UID(), sessionData.Uid)
	}
	if err := sess.SetDataEncoded(sessionData.Data); err != nil {
		return nil, err
	}
	err := sess.FlushUserData()
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
		return res, protos.ErrSessionNotFound().WithMetadata(map[string]string{"uid": msg.UserId, "reason": strconv.Itoa(int(msg.Reason))}).WithStack()
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
	// 若是网关收到 说明是同步数据的请求
	if s.server.Frontend {
		sess := s.sessionPool.GetSessionByUID(msg.Uid)
		if sess == nil {
			return nil, protos.ErrSessionNotFound().WithMessage("session is nil on bind backend push to frontend").WithMetadata(map[string]string{"uid": msg.Uid, "bid": msg.Bid, "btype": msg.Btype}).WithStack()
		}
		sess.SetBackendID(msg.Btype, msg.Bid)
		err := sess.FlushBackendData()
		if err != nil {
			return nil, err
		}
		return &protos.Response{}, nil
	}
	// 当前服不是目标服 报错
	if msg.Btype != s.server.Type || msg.Bid != s.server.ID {
		logger.Zap.Error(constants.ErrIllegalBindBackendID.Error())
		return nil, constants.ErrIllegalBindBackendID
	}
	sess := s.sessionPool.GetSessionByUID(msg.Uid)
	if sess == nil {
		sess = s.getSessionFromCtx(ctx)
		var err error
		if sess == nil {
			err = protos.ErrSessionNotFound().WithMessage("session is nil on bind backend").WithMetadata(map[string]string{"uid": msg.Uid, "bid": msg.Bid, "btype": msg.Btype}).WithStack()
		} else if sess.UID() != msg.Uid {
			err = protos.ErrSessionNotFound().WithMessage("session uid not equl target uid on ").WithMetadata(map[string]string{"uid": msg.Uid, "sessUid": sess.UID(), "bid": msg.Bid, "btype": msg.Btype}).WithStack()
		}
		if err != nil {
			logger.Zap.Error("", zap.Error(err))
			return nil, err

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
	// 若是网关收到 说明是同步数据的请求
	if s.server.Frontend {
		sess := s.sessionPool.GetSessionByUID(msg.Uid)
		if sess == nil {
			return nil, protos.ErrSessionNotFound().WithMessage("session is nil on bind backend push to frontend").WithMetadata(map[string]string{"uid": msg.Uid, "bid": msg.Bid, "btype": msg.Btype}).WithStack()
		}
		sess.RemoveBackendID(msg.Btype)
		err := sess.FlushBackendData()
		if err != nil {
			return nil, err
		}
		return &protos.Response{}, nil
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
	co.GoWithUser(ctx, util.ForceIdStrToInt(msg.Uid), func(ctx context.Context) {
		for _, r := range s.remote.GetRemoteSessionListener() {
			r.OnUserBound(ctx, msg.Uid, msg.Fid, msg.Metadata)
		}
	})
	return &protos.Response{Data: []byte("ack")}, nil
}

func (s *Sys) SessionClosed(ctx context.Context, msg *protos.KickMsg) (*protos.Response, error) {
	if msg.UserId == "" {
		co.Go(func() {
			for _, r := range s.remote.GetRemoteSessionListener() {
				r.OnUserDisconnected(ctx, msg.UserId, msg.Metadata)
			}
		})
		return &protos.Response{Data: []byte("ack")}, nil
	}
	co.GoWithUser(ctx, util.ForceIdStrToInt(msg.UserId), func(ctx context.Context) {
		for _, r := range s.remote.GetRemoteSessionListener() {
			r.OnUserDisconnected(ctx, msg.UserId, msg.Metadata)
		}
	})
	return &protos.Response{Data: []byte("ack")}, nil
}

func (s *Sys) SessionBoundBackendFork(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	if msg.Uid == "" {
		co.Go(func() {
			for _, r := range s.remote.GetRemoteBindingListener() {
				r.OnUserBindBackend(msg.Uid, msg.Btype, msg.Bid)
			}
		})
		return &protos.Response{Data: []byte("ack")}, nil
	}
	co.GoWithUser(ctx, util.ForceIdStrToInt(msg.Uid), func(ctx context.Context) {
		for _, r := range s.remote.GetRemoteBindingListener() {
			r.OnUserBindBackend(msg.Uid, msg.Btype, msg.Bid)
		}
	})
	return &protos.Response{Data: []byte("ack")}, nil
}

// SessionBoundBackend 所有服务收到backend已绑定通知
//
//	@receiver s
//	@param ctx
//	@param msg
//	@return *protos.Response
//	@return error
func (s *Sys) SessionBoundBackend(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	if !s.server.Frontend {
		sess := s.sessionPool.GetSessionByUID(msg.Uid)
		if sess != nil {
			sess.SetBackendID(msg.Btype, msg.Bid)
		}
	}
	co.GoWithUser(ctx, util.ForceIdStrToInt(msg.Uid), func(ctx context.Context) {
		for _, r := range s.remote.GetRemoteSessionListener() {
			r.OnUserBoundBackend(ctx, msg.Uid, msg.Btype, msg.Bid, msg.Metadata)
		}
	})
	return &protos.Response{Data: []byte("ack")}, nil

}

// SessionKickedBackend 所有服务收到backend已解绑通知
//
//	@receiver s
//	@param ctx
//	@param msg
//	@return *protos.Response
//	@return error
func (s *Sys) SessionKickedBackend(ctx context.Context, msg *protos.BindBackendMsg) (*protos.Response, error) {
	if !s.server.Frontend {
		sess := s.sessionPool.GetSessionByUID(msg.Uid)
		if sess != nil {
			sess.RemoveBackendID(msg.Btype)
		}
	}
	co.GoWithUser(ctx, util.ForceIdStrToInt(msg.Uid), func(ctx context.Context) {
		for _, r := range s.remote.GetRemoteSessionListener() {
			r.OnUserUnboundBackend(ctx, msg.Uid, msg.Btype, msg.Bid, msg.Metadata)
		}
	})
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
