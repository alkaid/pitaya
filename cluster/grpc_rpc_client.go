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
	"sync"
	"sync/atomic"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/interfaces"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/tracing"
	"github.com/alkaid/pitaya/v2/util"
	"google.golang.org/grpc"
)

// GRPCClient rpc client struct
type GRPCClient struct {
	bindingStorage   interfaces.BindingStorage
	clientMap        sync.Map
	dialTimeout      time.Duration
	infoRetriever    InfoRetriever
	lazy             bool
	metricsReporters []metrics.Reporter
	reqTimeout       time.Duration
	server           *Server
	running          atomic.Bool
	operationMu      sync.Mutex
	operationWG      sync.WaitGroup
	operationsClosed bool
	lifecycleCtx     context.Context
	cancelLifecycle  context.CancelFunc
	shutdownOnce     sync.Once
}

// NewGRPCClient returns a new instance of GRPCClient
func NewGRPCClient(
	config config.GRPCClientConfig,
	server *Server,
	metricsReporters []metrics.Reporter,
	bindingStorage interfaces.BindingStorage,
	infoRetriever InfoRetriever,
) (*GRPCClient, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	gs := &GRPCClient{
		bindingStorage:   bindingStorage,
		infoRetriever:    infoRetriever,
		metricsReporters: metricsReporters,
		server:           server,
		lifecycleCtx:     lifecycleCtx,
		cancelLifecycle:  cancelLifecycle,
	}

	gs.dialTimeout = config.DialTimeout
	gs.lazy = config.LazyConnection
	gs.reqTimeout = config.RequestTimeout

	return gs, nil
}

type grpcClient struct {
	address   string
	cli       protos.PitayaClient
	conn      *grpc.ClientConn
	connected bool
	lock      sync.Mutex
}

// Init inits grpc rpc client
func (gs *GRPCClient) Init() error {
	gs.operationMu.Lock()
	defer gs.operationMu.Unlock()
	if gs.operationsClosed {
		return constants.ErrRPCClientNotInitialized
	}
	gs.running.Store(true)
	return nil
}

func (gs *GRPCClient) Publish(
	ctx context.Context,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
	observersCount int,
	timeouts ...time.Duration,
) ([]*protos.Response, error) {
	_, done, err := gs.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	return nil, constants.ErrNotImplemented
}

// Call makes a RPC Call
func (gs *GRPCClient) Call(
	ctx context.Context,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
	server *Server,
) (*protos.Response, error) {
	var err error
	ctx, done, err := gs.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	ctx = withTargetRPCAdmission(ctx, session, server)
	ctx, err = withTargetOwnerFencingMetadata(ctx, server)
	if err != nil {
		return nil, err
	}
	c, ok := gs.clientMap.Load(server.ID)
	if !ok {
		return nil, constants.ErrNoConnectionToServer
	}
	spanInfo := &tracing.SpanInfo{
		RpcSystem: "grpc",
		IsClient:  true,
		Route:     route,
		PeerID:    server.ID,
		PeerType:  server.Type,
		LocalID:   gs.server.ID,
		LocalType: gs.server.Type,
		RequestID: "",
	}
	ctx = tracing.RPCStartSpan(ctx, spanInfo)
	defer tracing.FinishSpan(ctx, err)

	if session != nil {
		requestID := util.NanoID(16)
		requestInfo := ""
		if route != nil {
			requestInfo = route.Method
		}
		session.SetRequestInFlight(requestID, requestInfo, true)
		defer session.SetRequestInFlight(requestID, "", false)
	}

	ctx = pcontext.AddToPropagateCtx(ctx, constants.RequestTimeout, gs.reqTimeout.String())
	req, err1 := buildRequest(ctx, rpcType, route.String(), session, msg, gs.server)
	err = err1
	if err != nil {
		return nil, err
	}

	ctxT, done := context.WithTimeout(ctx, gs.reqTimeout)
	defer done()

	if gs.metricsReporters != nil {
		startTime := time.Now()
		ctxT = pcontext.AddListToPropagateCtx(ctxT, constants.StartTimeKey, startTime.UnixNano(), constants.RouteKey, route.String())
		defer metrics.ReportTimingFromCtx(ctxT, gs.metricsReporters, "rpc", err)
	}

	res, err1 := c.(*grpcClient).call(ctxT, req)
	err = err1
	if err != nil {
		return nil, err
	}
	if res.Status != nil {
		err = apierrors.FromStatus(res.Status)
		return nil, err
	}
	return res, nil
}

// Fork not implemented RPCClient.Fork
func (gs *GRPCClient) Fork(ctx context.Context, route *route.Route, session session.Session, msg *message.Message) error {
	_, done, err := gs.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer done()
	return constants.ErrNotImplemented
}

// Send not implemented in grpc client
func (gs *GRPCClient) Send(uid string, d []byte) error {
	_, done, err := gs.beginOperation(context.Background())
	if err != nil {
		return err
	}
	defer done()
	return constants.ErrNotImplemented
}

// Deprecated:Use Fork instead
//
// sends the binding information to other servers that may be interested in this info
func (gs *GRPCClient) BroadcastSessionBind(uid string) error {
	ctx, done, err := gs.beginOperation(context.Background())
	if err != nil {
		return err
	}
	defer done()
	if gs.bindingStorage == nil {
		return constants.ErrNoBindingStorageModule
	}
	fid, _ := gs.bindingStorage.GetUserFrontendID(uid, gs.server.Type)
	if fid != "" {
		if c, ok := gs.clientMap.Load(fid); ok {
			msg := &protos.BindMsg{
				Uid: uid,
				Fid: gs.server.ID,
			}
			ctxT, cancel := context.WithTimeout(ctx, gs.reqTimeout)
			defer cancel()
			err := c.(*grpcClient).sessionBindRemote(ctxT, msg)
			return err
		}
	}
	return nil
}

// SendKick sends a kick to an user
func (gs *GRPCClient) SendKick(userID string, serverType string, kick *protos.KickMsg) error {
	ctx, done, admissionErr := gs.beginOperation(context.Background())
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	var svID string
	var err error

	if gs.bindingStorage == nil {
		return constants.ErrNoBindingStorageModule
	}

	svID, err = gs.bindingStorage.GetUserFrontendID(userID, serverType)
	if err != nil {
		return err
	}

	if c, ok := gs.clientMap.Load(svID); ok {
		ctxT, cancel := context.WithTimeout(ctx, gs.reqTimeout)
		defer cancel()
		err := c.(*grpcClient).sendKick(ctxT, kick)
		return err
	}
	return constants.ErrNoConnectionToServer
}

// SendPush sends a message to an user, if you dont know the serverID that the user is connected to, you need to set a BindingStorage when creating the client
// TODO: Jaeger?
func (gs *GRPCClient) SendPush(userID string, frontendSv *Server, push *protos.Push) error {
	ctx, done, admissionErr := gs.beginOperation(context.Background())
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	var svID string
	var err error
	if frontendSv.ID != "" {
		svID = frontendSv.ID
	} else {
		if gs.bindingStorage == nil {
			return constants.ErrNoBindingStorageModule
		}
		svID, err = gs.bindingStorage.GetUserFrontendID(userID, frontendSv.Type)
		if err != nil {
			return err
		}
	}
	if c, ok := gs.clientMap.Load(svID); ok {
		ctxT, cancel := context.WithTimeout(ctx, gs.reqTimeout)
		defer cancel()
		err := c.(*grpcClient).pushToUser(ctxT, push)
		return err
	}
	return constants.ErrNoConnectionToServer
}

// AddServer is called when a new server is discovered
func (gs *GRPCClient) AddServer(sv *Server) {
	_, done, err := gs.beginOperation(context.Background())
	if err != nil {
		logger.Log.Debugf("[grpc client] ignored server %s while shutting down", sv.ID)
		return
	}
	defer done()
	var host, port, portKey string
	var ok bool

	host, portKey = gs.getServerHost(sv)
	if host == "" {
		logger.Log.Errorf("[grpc client] server %s has no grpcHost specified in metadata", sv.ID)
		return
	}

	if port, ok = sv.Metadata[portKey]; !ok {
		logger.Log.Errorf("[grpc client] server %s has no %s specified in metadata", sv.ID, portKey)
		return
	}

	address := fmt.Sprintf("%s:%s", host, port)
	client := &grpcClient{address: address}
	if !gs.lazy {
		if err := client.connect(); err != nil {
			logger.Log.Errorf("[grpc client] unable to connect to server %s at %s: %v", sv.ID, address, err)
		}
	}
	gs.clientMap.Store(sv.ID, client)
	logger.Log.Debugf("[grpc client] added server %s at %s", sv.ID, address)
}

// RemoveServer is called when a server is removed
func (gs *GRPCClient) RemoveServer(sv *Server) {
	_, done, err := gs.beginOperation(context.Background())
	if err != nil {
		return
	}
	defer done()
	if c, ok := gs.clientMap.Load(sv.ID); ok {
		c.(*grpcClient).disconnect()
		gs.clientMap.Delete(sv.ID)
		logger.Log.Debugf("[grpc client] removed server %s", sv.ID)
	}
}

func (gs *GRPCClient) ModifyServer(sv *Server, old *Server) {
	// do nothing
}

// AfterInit runs after initialization
func (gs *GRPCClient) AfterInit() {}

// BeforeShutdown runs before shutdown
func (gs *GRPCClient) BeforeShutdown() {}

// Shutdown stops admission, cancels admitted calls, and closes every owned
// connection before returning.
func (gs *GRPCClient) Shutdown() error {
	gs.shutdownOnce.Do(func() {
		gs.operationMu.Lock()
		gs.operationsClosed = true
		gs.running.Store(false)
		if gs.cancelLifecycle != nil {
			gs.cancelLifecycle()
		}
		gs.operationMu.Unlock()

		gs.operationWG.Wait()
		gs.clientMap.Range(func(key, value any) bool {
			if client, ok := value.(*grpcClient); ok {
				client.disconnect()
			}
			gs.clientMap.Delete(key)
			return true
		})
	})
	return nil
}

// IsRunning reports whether the client has entered its initialized lifecycle
// and has not started shutdown.
func (gs *GRPCClient) IsRunning() bool {
	return gs.running.Load()
}

func (gs *GRPCClient) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	gs.operationMu.Lock()
	if gs.operationsClosed {
		gs.operationMu.Unlock()
		return nil, nil, constants.ErrRPCClientNotInitialized
	}
	if err := ctx.Err(); err != nil {
		gs.operationMu.Unlock()
		return nil, nil, err
	}
	gs.operationWG.Add(1)
	gs.operationMu.Unlock()

	operationCtx, cancel := context.WithCancel(ctx)
	stopLifecycle := func() bool { return false }
	if gs.lifecycleCtx != nil {
		stopLifecycle = context.AfterFunc(gs.lifecycleCtx, cancel)
	}
	return operationCtx, func() {
		stopLifecycle()
		cancel()
		gs.operationWG.Done()
	}, nil
}

func (gs *GRPCClient) getServerHost(sv *Server) (host, portKey string) {
	var (
		serverRegion, hasRegion   = sv.Metadata[constants.RegionKey]
		externalHost, hasExternal = sv.Metadata[constants.GRPCExternalHostKey]
		internalHost, _           = sv.Metadata[constants.GRPCHostKey]
	)

	hasRegion = hasRegion && serverRegion != ""
	hasExternal = hasExternal && externalHost != ""

	if !hasRegion {
		if hasExternal {
			logger.Log.Warnf("[grpc client] server %s has no region specified in metadata, using external host", sv.ID)
			return externalHost, constants.GRPCExternalPortKey
		}

		logger.Log.Warnf("[grpc client] server %s has no region nor external host specified in metadata, using internal host", sv.ID)
		return internalHost, constants.GRPCPortKey
	}

	if gs.infoRetriever.Region() == serverRegion || !hasExternal {
		logger.Log.Infof("[grpc client] server %s is in same region or external host not provided, using internal host", sv.ID)
		return internalHost, constants.GRPCPortKey
	}

	logger.Log.Infof("[grpc client] server %s is in other region, using external host", sv.ID)
	return externalHost, constants.GRPCExternalPortKey
}

func (gc *grpcClient) connect() error {
	gc.lock.Lock()
	defer gc.lock.Unlock()
	return gc.connectLocked()
}

func (gc *grpcClient) connectLocked() error {
	if gc.connected {
		return nil
	}

	conn, err := grpc.Dial(
		gc.address,
		grpc.WithInsecure(),
	)
	if err != nil {
		return err
	}
	c := protos.NewPitayaClient(conn)
	gc.cli = c
	gc.conn = conn
	gc.connected = true
	return nil
}

func (gc *grpcClient) disconnect() {
	gc.lock.Lock()
	if gc.conn != nil {
		_ = gc.conn.Close()
	}
	gc.cli = nil
	gc.conn = nil
	gc.connected = false
	gc.lock.Unlock()
}

func (gc *grpcClient) client() (protos.PitayaClient, error) {
	gc.lock.Lock()
	defer gc.lock.Unlock()
	if !gc.connected {
		if err := gc.connectLocked(); err != nil {
			return nil, err
		}
	}
	if gc.cli == nil {
		return nil, constants.ErrNoConnectionToServer
	}
	return gc.cli, nil
}

func (gc *grpcClient) pushToUser(ctx context.Context, push *protos.Push) error {
	client, err := gc.client()
	if err != nil {
		return err
	}
	_, err = client.PushToUser(ctx, push)
	return err
}

func (gc *grpcClient) call(ctx context.Context, req *protos.Request) (*protos.Response, error) {
	client, err := gc.client()
	if err != nil {
		return nil, err
	}
	return client.Call(ctx, req)
}

func (gc *grpcClient) sessionBindRemote(ctx context.Context, req *protos.BindMsg) error {
	client, err := gc.client()
	if err != nil {
		return err
	}
	_, err = client.SessionBindRemote(ctx, req)
	return err
}

func (gc *grpcClient) sendKick(ctx context.Context, req *protos.KickMsg) error {
	client, err := gc.client()
	if err != nil {
		return err
	}
	_, err = client.KickUser(ctx, req)
	return err
}
