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

package client

import (
	"crypto/tls"

	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/session"
)

// PitayaClient iface
type PitayaClient interface {
	ConnectTo(addr string, tlsConfig ...*tls.Config) error
	ConnectedStatus() bool
	Disconnect(reason CloseReason)
	MsgChannel() chan *message.Message
	SendNotify(route string, data []byte) error
	SendRequest(route string, data []byte) (uint, error)
	SetClientHandshakeData(data *session.HandshakeData)
	OnDisconnected(callback func(reason CloseReason))
}

type CloseReason int // 关闭原因

const (
	_                 CloseReason = iota
	CloseReasonFatal              // 发生致命错误后关闭(上层不应该重连而应该检查代码)
	CloseReasonError              // 发生错误,如网络断开,心跳超时等(可以重连)
	CloseReasonKicked             // 被服务端踢出(根据需求看是否重连)
	CloseReasonManual             // 手动关闭(不应该重连)
)
