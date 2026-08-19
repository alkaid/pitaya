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
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/logger"
	nats "github.com/nats-io/nats.go"
)

func getChannel(serverType, serverID string) string {
	return fmt.Sprintf("pitaya/servers/%s/%s", serverType, serverID)
}

func signalAppDie(appDieChan chan bool) {
	if appDieChan == nil {
		return
	}
	// Legacy callers can still supply or close this channel. Internal lifecycle
	// code never closes it, and signaling must never block a transport callback.
	defer func() {
		_ = recover()
	}()
	select {
	case appDieChan <- true:
	default:
	}
}

func setupNatsConn(connectString string, appDieChan chan bool, options ...nats.Option) (*nats.Conn, error) {
	nc, _, err := setupTrackedNatsConn(connectString, appDieChan, nil, options...)
	return nc, err
}

func setupTrackedNatsConn(
	connectString string,
	appDieChan chan bool,
	terminalCallback func(),
	options ...nats.Option,
) (*nats.Conn, <-chan struct{}, error) {
	terminalCallbackDone := make(chan struct{})
	var terminalCallbackOnce sync.Once
	natsOptions := append(
		options,
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Zap.Warn("disconnected from nats!", zap.Error(err))
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Log.Warnf("reconnected to nats server %s with address %s in cluster %s!", nc.ConnectedServerName(), nc.ConnectedAddr(), nc.ConnectedClusterName())
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			defer terminalCallbackOnce.Do(func() {
				close(terminalCallbackDone)
			})
			if terminalCallback != nil {
				terminalCallback()
			}
			err := nc.LastError()
			if err == nil {
				logger.Log.Warn("nats connection closed with no error.")
				return
			}

			logger.Zap.Error("nats connection closed", zap.Error(err))
			signalAppDie(appDieChan)
		}),
		nats.ErrorHandler(func(nc *nats.Conn, sub *nats.Subscription, err error) {
			if err == nats.ErrSlowConsumer {
				dropped, _ := sub.Dropped()
				logger.Log.Warn("nats slow consumer on subject %q: dropped %d messages\n",
					sub.Subject, dropped)
			} else {
				logger.Log.Errorf(err.Error())
			}
		}),
	)

	nc, err := nats.Connect(connectString, natsOptions...)
	if err != nil {
		return nil, nil, err
	}
	return nc, terminalCallbackDone, nil
}
