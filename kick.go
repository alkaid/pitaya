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
	"context"

	"go.uber.org/zap"

	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/protos"
)

// SendKickToUsers sends kick to an user array
func (app *App) SendKickToUsers(uids []string, frontendType string, callback map[string]string) ([]string, error) {
	if !app.server.Frontend && frontendType == "" {
		return uids, constants.ErrFrontendTypeNotSpecified
	}

	var notKickedUids []string

	for _, uid := range uids {
		if s := app.sessionPool.GetSessionByUID(uid); s != nil {
			if err := s.Kick(context.Background(), callback); err != nil {
				notKickedUids = append(notKickedUids, uid)
				logger.Zap.Error("Session kick error", zap.Int64("ID", s.ID()), zap.String("UID", s.UID()), zap.Error(err))
			}
		} else if app.rpcClient != nil {
			kick := &protos.KickMsg{UserId: uid, Metadata: callback}
			if err := app.rpcClient.SendKick(uid, frontendType, kick); err != nil {
				notKickedUids = append(notKickedUids, uid)
				logger.Zap.Error("RPCClient send kick error", zap.String("UID", uid), zap.String("svType", frontendType), zap.Error(err))
			}
		} else {
			notKickedUids = append(notKickedUids, uid)
		}

	}

	if len(notKickedUids) != 0 {
		return notKickedUids, constants.ErrKickingUsers
	}

	return nil, nil
}
