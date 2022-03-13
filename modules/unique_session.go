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

package modules

import (
	"context"

	"github.com/topfreegames/pitaya/v2/cluster"
	"github.com/topfreegames/pitaya/v2/session"
)

// UniqueSession module watches for sessions using the same UID and kicks them
type UniqueSession struct {
	Base
	server      *cluster.Server
	rpcClient   cluster.RPCClient
	sessionPool session.SessionPool
}

// NewUniqueSession creates a new unique session module
func NewUniqueSession(server *cluster.Server, rpcServer cluster.RPCServer, rpcClient cluster.RPCClient, sessionPool session.SessionPool) *UniqueSession {
	return &UniqueSession{
		server:      server,
		rpcClient:   rpcClient,
		sessionPool: sessionPool,
	}
}

// OnUserBind
// @implement cluster.RemoteBindingListener
// 收到非自己服务器的session绑定通知时(远程)
func (u *UniqueSession) OnUserBind(uid, fid string) {
	if u.server.ID == fid || !u.server.Frontend {
		return
	}
	oldSession := u.sessionPool.GetSessionByUID(uid)
	if oldSession != nil {
		// TODO: it would be nice to set this correctly
		oldSession.Kick(context.Background(), nil, session.CloseReasonRebind)
	}
}

// OnUserBindBackend 用户成功绑定backend服务器时
// @implement cluster.RemoteBindingListener
func (u *UniqueSession) OnUserBindBackend(uid, serverType, serverId string) {
	if serverType != u.server.Type || serverId == u.server.ID {
		return
	}
	oldSession := u.sessionPool.GetSessionByUID(uid)
	if oldSession != nil {
		oldSession.KickBackend(context.Background(), serverType, nil, session.CloseReasonRebind)
	}
}

// Init initializes the module
func (u *UniqueSession) Init() error {
	// 同类frontend广播逻辑移动到 service.Sys处理
	// u.sessionPool.OnSessionBind(func(ctx context.Context, s session.Session) error {
	// 	oldSession := u.sessionPool.GetSessionByUID(s.UID())
	// 	if oldSession != nil {
	// 		oldSession.Kick(ctx, session.CloseReasonRebind)
	// 	}
	// 	err := u.rpcClient.BroadcastSessionBind(s.UID())
	// 	return err
	// })
	return nil
}
