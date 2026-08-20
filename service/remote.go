//
// Copyright (c) TFG Co. All Rights Reserved.
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

package service

import (
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/router"
	"github.com/alkaid/pitaya/v2/serialize"
	"github.com/alkaid/pitaya/v2/session"
)

// RemoteService struct
type RemoteService struct {
	baseService
	protos.UnimplementedPitayaServer
	remoteRegistryOwner
	remoteListenerOwner
	rpcServer        cluster.RPCServer
	serviceDiscovery cluster.ServiceDiscovery
	serializer       serialize.Serializer
	encoder          codec.PacketEncoder
	rpcClient        cluster.RPCClient
	router           *router.Router
	messageEncoder   message.Encoder
	server           *cluster.Server // server obj
	remoteHooks      *pipeline.RemoteHooks
	sessionPool      session.SessionPool
	handlerPool      *HandlerPool
	fanout           *remoteFanoutExecutor
	idempotencyStore IdempotencyStore
}

// RemoteServiceOption customizes a RemoteService without changing existing
// constructor call sites.
type RemoteServiceOption func(*RemoteService)

// WithFanoutConcurrency sets the maximum number of RPCs a single fanout
// operation runs concurrently. Non-positive values keep the framework default.
func WithFanoutConcurrency(concurrency int) RemoteServiceOption {
	return func(remote *RemoteService) {
		if concurrency > 0 {
			fanout := remote.fanoutOwner()
			fanout.concurrency = concurrency
			remote.fanout = fanout
		}
	}
}

// WithIdempotencyStore replaces the bounded in-memory RPC command store. A
// durable implementation can preserve deduplication across owner failover.
func WithIdempotencyStore(store IdempotencyStore) RemoteServiceOption {
	return func(remote *RemoteService) {
		if store != nil {
			remote.idempotencyStore = store
		}
	}
}

// NewRemoteService creates and returns a RemoteService with framework defaults.
func NewRemoteService(
	rpcClient cluster.RPCClient,
	rpcServer cluster.RPCServer,
	sd cluster.ServiceDiscovery,
	encoder codec.PacketEncoder,
	serializer serialize.Serializer,
	router *router.Router,
	messageEncoder message.Encoder,
	server *cluster.Server,
	sessionPool session.SessionPool,
	remoteHooks *pipeline.RemoteHooks,
	handlerHooks *pipeline.HandlerHooks,
	handlerPool *HandlerPool,

) *RemoteService {
	return newRemoteService(
		rpcClient, rpcServer, sd, encoder, serializer, router, messageEncoder,
		server, sessionPool, remoteHooks, handlerHooks, handlerPool,
	)
}

// NewRemoteServiceWithOptions creates a RemoteService with optional settings.
func NewRemoteServiceWithOptions(
	rpcClient cluster.RPCClient,
	rpcServer cluster.RPCServer,
	sd cluster.ServiceDiscovery,
	encoder codec.PacketEncoder,
	serializer serialize.Serializer,
	router *router.Router,
	messageEncoder message.Encoder,
	server *cluster.Server,
	sessionPool session.SessionPool,
	remoteHooks *pipeline.RemoteHooks,
	handlerHooks *pipeline.HandlerHooks,
	handlerPool *HandlerPool,
	opts ...RemoteServiceOption,
) *RemoteService {
	return newRemoteService(
		rpcClient, rpcServer, sd, encoder, serializer, router, messageEncoder,
		server, sessionPool, remoteHooks, handlerHooks, handlerPool, opts...,
	)
}

func newRemoteService(
	rpcClient cluster.RPCClient,
	rpcServer cluster.RPCServer,
	sd cluster.ServiceDiscovery,
	encoder codec.PacketEncoder,
	serializer serialize.Serializer,
	router *router.Router,
	messageEncoder message.Encoder,
	server *cluster.Server,
	sessionPool session.SessionPool,
	remoteHooks *pipeline.RemoteHooks,
	handlerHooks *pipeline.HandlerHooks,
	handlerPool *HandlerPool,
	opts ...RemoteServiceOption,
) *RemoteService {
	remote := &RemoteService{
		remoteRegistryOwner: newRemoteRegistryOwner(),
		remoteListenerOwner: remoteListenerOwner{},
		rpcClient:           rpcClient,
		rpcServer:           rpcServer,
		encoder:             encoder,
		serviceDiscovery:    sd,
		serializer:          serializer,
		router:              router,
		messageEncoder:      messageEncoder,
		server:              server,
		sessionPool:         sessionPool,
		handlerPool:         handlerPool,
		idempotencyStore:    NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions()),
	}
	remote.fanout = newRemoteFanoutExecutor(remote, constants.DefaultRPCFanoutConcurrency)

	remote.remoteHooks = remoteHooks
	remote.handlerHooks = handlerHooks
	for _, opt := range opts {
		opt(remote)
	}

	return remote
}

func (r *RemoteService) SetRemoteHooks(remoteHooks *pipeline.RemoteHooks) {
	r.remoteHooks = remoteHooks
}

func (r *RemoteService) GetRemoteHooks() *pipeline.RemoteHooks {
	return r.remoteHooks
}
