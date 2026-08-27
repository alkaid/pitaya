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
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/util"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultGRPCGracefulStopTimeout = 5 * time.Second

// GRPCServer rpc server struct
type GRPCServer struct {
	server              *Server
	port                int
	metricsReporters    []metrics.Reporter
	grpcSv              *grpc.Server
	listener            net.Listener
	pitayaServer        protos.PitayaServer
	stopOnce            sync.Once
	gracefulStopTimeout time.Duration
	rpcMu               sync.Mutex
	rpcWG               sync.WaitGroup
	running             atomic.Bool
	stopping            bool
}

// NewGRPCServer constructor
func NewGRPCServer(config config.GRPCServerConfig, server *Server, metricsReporters []metrics.Reporter) (*GRPCServer, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	gs := &GRPCServer{
		port:                config.Port,
		server:              server,
		metricsReporters:    metricsReporters,
		gracefulStopTimeout: defaultGRPCGracefulStopTimeout,
	}
	return gs, nil
}

// Init inits grpc rpc server
func (gs *GRPCServer) Init() error {
	port := gs.port
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	gs.listener = lis
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(gs.requestContextInterceptor))
	gs.grpcSv = grpcServer
	protos.RegisterPitayaServer(grpcServer, gs.pitayaServer)
	gs.running.Store(true)
	go func() {
		defer gs.running.Store(false)
		_ = grpcServer.Serve(lis)
	}()
	return nil
}

// IsRunning reports whether the gRPC server is currently accepting traffic.
func (gs *GRPCServer) IsRunning() bool {
	return gs.running.Load()
}

func (gs *GRPCServer) requestContextInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (response any, err error) {
	gs.rpcMu.Lock()
	if gs.stopping {
		gs.rpcMu.Unlock()
		return nil, status.Error(codes.Unavailable, "pitaya RPC server is shutting down")
	}
	gs.rpcWG.Add(1)
	gs.rpcMu.Unlock()
	defer gs.rpcWG.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			method := ""
			if info != nil {
				method = info.FullMethod
			}
			util.LogRecoveredPanic(
				logger.Zap,
				"gRPC unary handler panicked",
				recovered,
				zap.String("method", method),
				zap.Stack("stack"),
			)
			response = nil
			err = status.Error(codes.Internal, "pitaya RPC handler failed")
		}
	}()

	if info.FullMethod != protos.Pitaya_Call_FullMethodName {
		return handler(ctx, req)
	}
	request, ok := req.(*protos.Request)
	if !ok {
		return nil, fmt.Errorf("grpc pitaya Call received %T", req)
	}
	serverID := ""
	if gs.server != nil {
		serverID = gs.server.ID
	}
	requestCtx, err := util.GetContextFromRequestContext(
		ctx,
		request,
		serverID,
		request.GetSession().GetUid(),
	)
	if err != nil {
		return nil, err
	}
	return handler(requestCtx, request)
}

// SetPitayaServer sets the pitaya server
func (gs *GRPCServer) SetPitayaServer(ps protos.PitayaServer) {
	gs.pitayaServer = ps
}
func (gs *GRPCServer) Subscribe(topic string, groups ...string) error {
	return constants.ErrNotImplemented
}

// AfterInit runs after initialization
func (gs *GRPCServer) AfterInit() {}

// BeforeShutdown stops inbound RPC and waits for admitted calls before App
// closes handler task admission and performs its final session sweep.
func (gs *GRPCServer) BeforeShutdown() {
	gs.stop()
}

// Shutdown stops grpc rpc server
func (gs *GRPCServer) Shutdown() error {
	gs.stop()
	return nil
}

func (gs *GRPCServer) stop() {
	gs.running.Store(false)
	gs.stopOnce.Do(func() {
		if gs.grpcSv == nil {
			return
		}
		gs.rpcMu.Lock()
		gs.stopping = true
		gs.rpcMu.Unlock()
		timeout := gs.gracefulStopTimeout
		if timeout <= 0 {
			timeout = defaultGRPCGracefulStopTimeout
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		if gs.listener != nil {
			_ = gs.listener.Close()
		}

		rpcDrained := make(chan struct{})
		go func() {
			gs.rpcWG.Wait()
			close(rpcDrained)
		}()
		select {
		case <-rpcDrained:
		case <-timer.C:
			logger.Zap.Warn("gRPC graceful shutdown timed out; forcing stop", zap.Duration("timeout", timeout))
			gs.grpcSv.Stop()
			return
		}

		gracefullyStopped := make(chan struct{})
		go func() {
			gs.grpcSv.GracefulStop()
			close(gracefullyStopped)
		}()
		select {
		case <-gracefullyStopped:
		case <-timer.C:
			logger.Zap.Warn("gRPC graceful shutdown timed out; forcing stop", zap.Duration("timeout", timeout))
			gs.grpcSv.Stop()
		}
	})
}
