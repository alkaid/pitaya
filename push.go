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

package pitaya

import (
	"github.com/topfreegames/pitaya/v2/cluster"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/util"
	"go.uber.org/zap"
)

// SendPushToUsers sends a message to the given list of users
func (app *App) SendPushToUsers(route string, v interface{}, uids []string, frontendType string) ([]string, error) {
	data, err := util.SerializeOrRaw(app.serializer, v)
	if err != nil {
		return uids, err
	}

	if !app.server.Frontend && frontendType == "" {
		return uids, constants.ErrFrontendTypeNotSpecified
	}

	var notPushedUids []string

	logger.Zap.Debug("Type=PushToUsers", zap.String("route", route), zap.String("svType", frontendType), zap.Strings("users", uids), zap.Any("data", v))

	for _, uid := range uids {
		if s := app.sessionPool.GetSessionByUID(uid); s != nil && app.server.Type == frontendType {
			if err := s.Push(route, data); err != nil {
				notPushedUids = append(notPushedUids, uid)
				logger.Zap.Error("Session push message error",
					zap.Int64("ID", s.ID()), zap.String("UID", s.UID()), zap.Error(err))
			}
		} else if app.rpcClient != nil {
			push := &protos.Push{
				Route: route,
				Uid:   uid,
				Data:  data,
			}
			if err = app.rpcClient.SendPush(uid, &cluster.Server{Type: frontendType}, push); err != nil {
				notPushedUids = append(notPushedUids, uid)
				logger.Zap.Error("RPCClient send message error", zap.String("UID", uid), zap.String("svType", frontendType), zap.Error(err))
			}
		} else {
			notPushedUids = append(notPushedUids, uid)
		}
	}

	if len(notPushedUids) != 0 {
		return notPushedUids, constants.ErrPushingToUsers
	}

	return nil, nil
}
