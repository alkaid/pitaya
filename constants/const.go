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

package constants

const (
	_ int32 = iota
	// StatusStart status
	StatusStart
	// StatusHandshake status
	StatusHandshake
	// StatusWorking status
	StatusWorking
	// StatusClosed status
	StatusClosed
)

const (
	// SessionPushRoute is the route used for updating session
	SessionPushRoute = "sys.pushsession"

	// SessionBindRoute is the route used for binding session
	SessionBindRoute = "sys.bindsession"

	// SessionBoundForkRoute session成功绑定的fork路由
	SessionBoundForkRoute = "sys.sessionboundfork"
	// SessionBoundRoute session成功绑定广播的路由
	SessionBoundRoute = "sys.sessionbound"

	// KickRoute is the route used for kicking an user
	KickRoute = "sys.kick"

	// SessionClosedRoute session关闭后的路由
	SessionClosedRoute = "sys.sessionclosed"

	// SessionBindBackendRoute session不在本服时转发请求绑定的路由
	SessionBindBackendRoute = "sys.bindbackendsession"

	// SessionBoundBackendForkRoute session成功绑定backend的fork路由
	SessionBoundBackendForkRoute = "sys.sessionboundbackendfork"
	// SessionBoundBackendRoute session成功绑定到 backend后的广播路由
	SessionBoundBackendRoute = "sys.sessionboundbackend"

	// KickBackendRoute session不在本服时的转发请求绑定sessionsticky backend的路由
	KickBackendRoute = "sys.kickbackend"

	// SessionKickedBackendRoute session与sessionsticky backend解绑后的路由
	SessionKickedBackendRoute = "sys.sessionkickedbackend"

	// ServerInternalErrorToClientRoute 服务器内部错误时若不是request类型消息引起的,以该路由回应客户端
	ServerInternalErrorToClientRoute = "internal.error"
)

// SessionCtxKey is the context key where the session will be set
var SessionCtxKey = "session"

var UserIdCtxKey = "userId"

// CoroutineCtxKey is the context key where the session will be set
var CoroutineCtxKey = "alkaid-coroutine"

// LoggerCtxKey is the context key where the default logger will be set
var LoggerCtxKey = "default-logger"

type propagateKey struct{}

// PropagateCtxKey is the context key where the content that will be
// propagated through rpc calls is set
var PropagateCtxKey = propagateKey{}

// SpanPropagateCtxKey is the key holding the opentracing spans inside
// the propagate key
var SpanPropagateCtxKey = "opentracing-span"

// PeerIDKey is the key holding the peer id to be sent over the context
var PeerIDKey = "peer.id"

// PeerServiceKey is the key holding the peer service to be sent over the context
var PeerServiceKey = "peer.service"

// StartTimeKey is the key holding the request start time (in ns) to be sent over the context
var StartTimeKey = "req-start-time"

// RequestIDKey is the key holding the request id to be sent over the context
var RequestIDKey = "request.id"

// RouteKey is the key holding the request route to be sent over the context
var RouteKey = "req-route"

// MetricTagsKey is the key holding request tags to be sent over the context
// to be reported
var MetricTagsKey = "metric-tags"

// GRPCHostKey is the key for grpc host on server metadata
var GRPCHostKey = "grpcHost"

// GRPCExternalHostKey is the key for grpc external host on server metadata
var GRPCExternalHostKey = "grpc-external-host"

// GRPCPortKey is the key for grpc port on server metadata
var GRPCPortKey = "grpcPort"

// GRPCExternalPortKey is the key for grpc external port on server metadata
var GRPCExternalPortKey = "grpc-external-port"

// RegionKey is the key to save the region server is on
var RegionKey = "region"

// IP constants
const (
	IPVersionKey = "ipversion"
	IPv4         = "ipv4"
	IPv6         = "ipv6"
)

// IOBufferBytesSize will be used when reading messages from clients
var IOBufferBytesSize = 4096

// RequestTimeout is the time it will take for a caller to timeout a request
var RequestTimeout = "reqTimeout"
