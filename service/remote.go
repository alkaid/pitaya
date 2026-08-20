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
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"

	goerrors "github.com/alkaid/goerrors/errors"
	"github.com/samber/lo"
	"k8s.io/utils/strings/slices"

	"google.golang.org/protobuf/proto"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/agent"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/docgenerator"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/router"
	"github.com/alkaid/pitaya/v2/serialize"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/tracing"
	"github.com/alkaid/pitaya/v2/util"
	"go.uber.org/zap"
)

// remoteRegistryOwner owns the component registry and its synchronization.
// Handler execution only keeps a pointer snapshot, so registration never holds
// the lock while user code runs.
type remoteRegistryOwner struct {
	mu           sync.RWMutex
	services     map[string]*component.Service
	subscribers  map[string]*component.Service
	remotes      map[string]*component.Remote
	interceptors map[string]*component.Interceptor
}

func newRemoteRegistryOwner() remoteRegistryOwner {
	return remoteRegistryOwner{
		services:     make(map[string]*component.Service),
		subscribers:  make(map[string]*component.Service),
		remotes:      make(map[string]*component.Remote),
		interceptors: make(map[string]*component.Interceptor),
	}
}

func (o *remoteRegistryOwner) lookupInterceptor(service string) (*component.Interceptor, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	interceptor, ok := o.interceptors[service]
	return interceptor, ok
}

func (o *remoteRegistryOwner) lookupRemote(route string) (*component.Remote, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	remote, ok := o.remotes[route]
	return remote, ok
}

func (o *remoteRegistryOwner) remoteNames() []string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	names := make([]string, 0, len(o.remotes))
	for name := range o.remotes {
		names = append(names, name)
	}
	return names
}

func (o *remoteRegistryOwner) servicesSnapshot() map[string]*component.Service {
	o.mu.RLock()
	defer o.mu.RUnlock()
	services := make(map[string]*component.Service, len(o.services))
	for name, service := range o.services {
		services[name] = service
	}
	return services
}

// remoteListenerOwner owns listener publication. Callers receive snapshots so
// callbacks can register more listeners without deadlocking an active dispatch.
type remoteListenerOwner struct {
	mu                     sync.RWMutex
	remoteBindingListeners []cluster.RemoteBindingListener
	remoteSessionListeners []cluster.RemoteSessionListener
}

func (o *remoteListenerOwner) bindingSnapshot() []cluster.RemoteBindingListener {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return append([]cluster.RemoteBindingListener(nil), o.remoteBindingListeners...)
}

func (o *remoteListenerOwner) sessionSnapshot() []cluster.RemoteSessionListener {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return append([]cluster.RemoteSessionListener(nil), o.remoteSessionListeners...)
}

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

func (r *RemoteService) remoteProcess(
	ctx context.Context,
	server *cluster.Server,
	a agent.Agent,
	route *route.Route,
	msg *message.Message,
) {
	res, err := r.remoteCall(ctx, server, protos.RPCType_Sys, route, a.GetSession(), msg)
	switch msg.Type {
	case message.Request:
		if err != nil {
			a.AnswerWithError(ctx, msg.ID, err)
			return
		}
		err = a.GetSession().ResponseMID(ctx, msg.ID, res.Data)
		if err != nil {
			util.GetLoggerFromCtx(ctx).Error("Failed to respond to remote server", zap.Error(err), zap.String("route", route.String()), zap.Any("rpcdata", msg))
			a.AnswerWithError(ctx, msg.ID, err)
		}
	case message.Notify:
		defer tracing.FinishSpan(ctx, err)
		if err != nil {
			a.AnswerWithError(ctx, msg.ID, err)
			return
		}
	default:
		util.GetLoggerFromCtx(ctx).Error("not support message type", zap.Uint8("msgType", uint8(msg.Type)), zap.String("route", route.String()), zap.Any("rpcdata", msg))
	}
}

// AddRemoteBindingListener 添加绑定发生时的回调，内部使用，仅用于相同serverType间的广播
func (r *RemoteService) AddRemoteBindingListener(bindingListener cluster.RemoteBindingListener) {
	r.remoteListenerOwner.mu.Lock()
	defer r.remoteListenerOwner.mu.Unlock()
	r.remoteBindingListeners = append(r.remoteBindingListeners, bindingListener)
}

// AddRemoteSessionListener 添加session各个生命周期完成时的回调
func (r *RemoteService) AddRemoteSessionListener(sessionListener cluster.RemoteSessionListener) {
	r.remoteListenerOwner.mu.Lock()
	defer r.remoteListenerOwner.mu.Unlock()
	r.remoteSessionListeners = append(r.remoteSessionListeners, sessionListener)
}

func (r *RemoteService) GetRemoteSessionListener() []cluster.RemoteSessionListener {
	return r.remoteListenerOwner.sessionSnapshot()
}
func (r *RemoteService) GetRemoteBindingListener() []cluster.RemoteBindingListener {
	return r.remoteListenerOwner.bindingSnapshot()
}

// Call processes a remote call
//
//	@implement protos.PitayaServer
func (r *RemoteService) Call(ctx context.Context, req *protos.Request) (*protos.Response, error) {
	if err := validateOwnerFence(ctx, r.server); err != nil {
		return rpcErrorResponse(err), err
	}
	key, err := idempotencyKeyFromContext(ctx)
	if err != nil {
		return rpcErrorResponse(err), err
	}
	reliableDigest, reliableJob, err := reliableRPCJobMetadata(pcontext.ToMap(ctx), key)
	if err != nil {
		return rpcErrorResponse(err), err
	}
	fingerprint := ""
	if key != "" {
		fingerprint, err = rpcRequestFingerprint(req, key, reliableJob, reliableDigest)
		if err != nil {
			return rpcErrorResponse(err), err
		}
	} else if err := validateUnkeyedReliableRPCMetadata(req); err != nil {
		return rpcErrorResponse(err), err
	}
	handlerCtx, err := consumeRPCAdmissionMetadata(ctx)
	if err != nil {
		return rpcErrorResponse(err), err
	}
	if key == "" || r.idempotencyStore == nil {
		return r.callOnce(handlerCtx, req)
	}
	scope, err := idempotencyStoreKey(ctx, r.server, key)
	if err != nil {
		return rpcErrorResponse(err), err
	}
	handlerPropagation := pcontext.ToMap(handlerCtx)
	response, callErr := r.idempotencyStore.Execute(handlerCtx, scope, fingerprint, func(callCtx context.Context) (*protos.Response, error) {
		callCtx = rpccontext.WithoutReliableRPCJob(callCtx)
		callCtx = context.WithValue(callCtx, constants.PropagateCtxKey, handlerPropagation)
		callCtx = withRPCIdempotencyInfo(callCtx, scope, fingerprint)
		return r.callOnce(callCtx, req)
	})
	if callErr != nil && (response == nil || response.Status == nil) {
		response = rpcErrorResponse(callErr)
	}
	return response, callErr
}

func (r *RemoteService) callOnce(ctx context.Context, req *protos.Request) (*protos.Response, error) {
	var res *protos.Response
	var err error
	logg := util.GetLoggerFromCtx(ctx)
	defer func() {
		if err != nil {
			// code := http.StatusInternalServerError
			// if res != nil {
			// 	code = int(res.Status.Code)
			// }
			// logFun := lo.If(code >= http.StatusInternalServerError, logg.Error).Else(logg.Warn)
			// 这里由于error转换了两次stack trace会丢失,详细log堆栈必须业务层打印
			logg.Warn("error calling rpc service,upstream must print stacktrace", zap.String("cause", err.Error()))
		}
	}()
	rt, err := route.Decode(req.GetMsg().GetRoute())
	if err != nil {
		return nil, err
	}
	spanInfo := tracing.SpanInfoFromRequest(ctx)
	spanInfo.IsClient = false
	spanInfo.Route = rt
	spanInfo.LocalID = r.server.ID
	spanInfo.LocalType = r.server.Type
	// 上层nats_rpc_server已经调用过,无需重复
	// c, err := util.GetContextFromRequest(req, r.server.ID)
	c := tracing.RPCStartSpan(ctx, spanInfo)
	defer tracing.FinishSpan(c, err)

	if err == nil {
		// 这里不能和官方一样单独线程,调用方已经用co.GoByID包装.
		res = processRemoteMessage(c, req, r)
	}

	if err != nil {
		res = &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
	}

	if res.Status != nil {
		err = apierrors.FromStatus(res.Status)
	}

	return res, err
}

func rpcErrorResponse(err error) *protos.Response {
	return &protos.Response{Status: &apierrors.FromError(err).Status}
}

// Deprecated:Use remote.Sys .SessionBoundFork() instead 由于上层frontend之间的广播方式改走Fork()实现,这里不会再收到响应
//
//	@implement protos.PitayaServer
//	is called when a remote server binds a user session and want us to acknowledge it
//	frontend收到其他frontend实例已经成功绑定session的消息广播时(该广播仅发给所有frontend)
//	具体来说是收到 modules.UniqueSession .Init() 中调用u.rpcClient.BroadcastSessionBind()发送的广播
//	与 remote.Sys .BindSession()不同,具体参见其注释
func (r *RemoteService) SessionBindRemote(ctx context.Context, msg *protos.BindMsg) (*protos.Response, error) {
	for _, r := range r.remoteListenerOwner.bindingSnapshot() {
		r.OnUserBind(msg.Uid, msg.Fid)
	}
	return &protos.Response{
		Data: []byte("ack"),
	}, nil
}

// PushToUser sends a push to user
//
//	@implement protos.PitayaServer
func (r *RemoteService) PushToUser(ctx context.Context, push *protos.Push) (*protos.Response, error) {
	logger.Zap.Debug("remote sending push to user", zap.String("uid", push.GetUid()), zap.String("data", string(push.Data)))
	s := r.sessionPool.GetSessionByUID(push.GetUid())
	if s != nil {
		err := s.Push(push.Route, push.Data)
		if err != nil {
			return nil, err
		}
		return &protos.Response{
			Data: []byte("ack"),
		}, nil
	}
	return nil, constants.ErrSessionNotFound
}

// KickUser sends a kick to user
//
//	@implement protos.PitayaServer
//	收到其他服务调用 cluster.RPCClient .SendKick() 时,一般来说是在拿不到 session.Session 只有uid的情况下.
//	与 remote.Sys .Kick()不同，后者用于session的调用
func (r *RemoteService) KickUser(ctx context.Context, kick *protos.KickMsg) (*protos.KickAnswer, error) {
	logger.Log.Debugf("sending kick to user %s", kick.GetUserId())
	s := r.sessionPool.GetSessionByUID(kick.GetUserId())
	if s != nil {
		err := s.Kick(ctx, kick.Metadata, session.CloseReason(kick.Reason))
		if err != nil {
			return nil, err
		}
		return &protos.KickAnswer{
			Kicked: true,
		}, nil
	}
	return nil, constants.ErrSessionNotFound
}

// DoRPC do rpc and get answer
func (r *RemoteService) DoRPC(ctx context.Context, serverID string, route *route.Route, protoData []byte, session session.Session) (*protos.Response, error) {
	msg := &message.Message{
		Type:  message.Request,
		Route: route.Short(),
		Data:  protoData,
	}

	if serverID == "" {
		return r.remoteCall(ctx, nil, protos.RPCType_User, route, session, msg)
	}

	target, _ := r.serviceDiscovery.GetServer(serverID)
	if target == nil {
		return nil, goerrors.WithStack(fmt.Errorf("%w svID=%s", constants.ErrServerNotFound, serverID))
	}

	return r.remoteCall(ctx, target, protos.RPCType_User, route, session, msg)
}

// DoNotify only support nats,don't use grpc.(copy then modify from DoRPC)
func (r *RemoteService) DoNotify(ctx context.Context, serverID string, route *route.Route, protoData []byte, session session.Session) error {
	msg := &message.Message{
		Type:  message.Notify,
		Route: route.Short(),
		Data:  protoData,
	}

	if serverID == "" {
		_, err := r.remoteCall(ctx, nil, protos.RPCType_User, route, session, msg)
		return err
	}

	target, _ := r.serviceDiscovery.GetServer(serverID)
	if target == nil {
		err := fmt.Errorf("%w svID=%s", constants.ErrServerNotFound, serverID)
		logger.Zap.Error("notify error",
			zap.String("uid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
			zap.String("route", route.String()),
			zap.Error(err))
		return err
	}

	_, err := r.remoteCall(ctx, target, protos.RPCType_User, route, session, msg)
	return err
}

type remoteFanoutTask struct {
	server  *cluster.Server
	route   *route.Route
	session session.Session
	msg     *message.Message
}

type remoteFanoutResult struct {
	started  bool
	server   *cluster.Server
	response *protos.Response
	err      error
}

type remoteFanoutExecutor struct {
	remote      *RemoteService
	concurrency int
}

var errRemoteFanoutPanic = stderrors.New("fanout RPC handler failed")

func newRemoteFanoutExecutor(remote *RemoteService, concurrency int) *remoteFanoutExecutor {
	if concurrency <= 0 {
		concurrency = constants.DefaultRPCFanoutConcurrency
	}
	return &remoteFanoutExecutor{
		remote:      remote,
		concurrency: concurrency,
	}
}

func (r *RemoteService) fanoutOwner() *remoteFanoutExecutor {
	if r.fanout != nil {
		return r.fanout
	}
	return newRemoteFanoutExecutor(r, constants.DefaultRPCFanoutConcurrency)
}

func (e *remoteFanoutExecutor) run(ctx context.Context, tasks []remoteFanoutTask) ([]*protos.Response, error) {
	responses := make([]*protos.Response, 0, len(tasks))
	if len(tasks) == 0 {
		return responses, nil
	}

	concurrency := e.concurrency
	if concurrency <= 0 {
		concurrency = constants.DefaultRPCFanoutConcurrency
	}
	concurrency = min(concurrency, len(tasks))

	results := make([]remoteFanoutResult, len(tasks))
	var next atomic.Int64
	next.Store(-1)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				index := int(next.Add(1))
				if index >= len(tasks) {
					return
				}
				// Cancellation can race with claiming the next task. Recheck before
				// marking it started so canceled fanouts do not open another RPC.
				if ctx.Err() != nil {
					return
				}
				results[index].started = true
				e.execute(ctx, tasks[index], &results[index])
			}
		}()
	}

	wg.Wait()

	errs := make([]error, 0)
	firstUnstarted := -1
	for i, result := range results {
		if !result.started {
			if firstUnstarted < 0 {
				firstUnstarted = i
			}
			continue
		}
		if result.err != nil {
			errs = append(errs, tasks[i].wrapError(result.server, result.err))
			continue
		}
	}

	if firstUnstarted >= 0 && ctx.Err() != nil {
		errs = append(errs, tasks[firstUnstarted].wrapError(nil, fmt.Errorf("fanout stopped: %w", ctx.Err())))
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		contextReported := false
		for _, err := range errs {
			if stderrors.Is(err, ctxErr) {
				contextReported = true
				break
			}
		}
		if !contextReported {
			errs = append(errs, fmt.Errorf("fanout: %w", ctxErr))
		}
	}

	successes := make([]int, 0, len(results))
	for i, result := range results {
		if result.started && result.err == nil && result.response != nil {
			successes = append(successes, i)
		}
	}
	sort.Slice(successes, func(i, j int) bool {
		left := results[successes[i]].server
		right := results[successes[j]].server
		if left.ID == right.ID {
			return left.Type < right.Type
		}
		return left.ID < right.ID
	})
	for _, index := range successes {
		responses = append(responses, results[index].response)
	}

	return responses, stderrors.Join(errs...)
}

func (e *remoteFanoutExecutor) execute(ctx context.Context, task remoteFanoutTask, result *remoteFanoutResult) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			result.err = errRemoteFanoutPanic
			target := result.server
			if target == nil {
				target = task.server
			}
			util.LogRecoveredPanic(
				logger.Zap,
				"panic in fanout RPC",
				panicValue,
				zap.String("svType", serverType(target)),
				zap.String("svID", serverID(target)),
				zap.Stack("stack"),
			)
		}
	}()

	result.server = task.server
	if result.server == nil {
		result.server, result.err = e.remote.resolveRemoteTarget(
			ctx,
			protos.RPCType_User,
			task.route,
			task.session,
			task.msg,
		)
		if result.err != nil {
			return
		}
	}
	ctx = withFanoutIdempotencyScope(ctx)
	result.response, result.err = e.remote.remoteCall(
		ctx,
		result.server,
		protos.RPCType_User,
		task.route,
		task.session,
		task.msg,
	)
}

func (task remoteFanoutTask) wrapError(target *cluster.Server, err error) error {
	if target == nil {
		target = task.server
	}
	if target == nil {
		return fmt.Errorf("server type %s: %w", task.route.SvType, err)
	}
	return fmt.Errorf("server %s: %w", target.ID, err)
}

func (r *RemoteService) snapshotFanoutServers(servers map[string]*cluster.Server) []*cluster.Server {
	targets := make([]*cluster.Server, 0, len(servers))
	for _, server := range servers {
		if server == nil {
			continue
		}
		if r.server != nil && r.server.Type == server.Type && r.server.ID == server.ID {
			continue
		}
		targets = append(targets, server)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].ID == targets[j].ID {
			return targets[i].Type < targets[j].Type
		}
		return targets[i].ID < targets[j].ID
	})
	return targets
}

func snapshotServerTypes(servers map[string]*cluster.Server) []*cluster.Server {
	targets := make([]*cluster.Server, 0, len(servers))
	for _, server := range servers {
		if server != nil {
			targets = append(targets, server)
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Type == targets[j].Type {
			return targets[i].ID < targets[j].ID
		}
		return targets[i].Type < targets[j].Type
	})
	return targets
}

func sortFanoutTasks(tasks []remoteFanoutTask) {
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].route.SvType != tasks[j].route.SvType {
			return tasks[i].route.SvType < tasks[j].route.SvType
		}
		if tasks[i].server == nil {
			return tasks[j].server != nil
		}
		if tasks[j].server == nil {
			return false
		}
		return tasks[i].server.ID < tasks[j].server.ID
	})

	type taskGroup struct {
		next int
		end  int
	}
	groups := make([]taskGroup, 0)
	for start := 0; start < len(tasks); {
		end := start + 1
		for end < len(tasks) && tasks[end].route.SvType == tasks[start].route.SvType {
			end++
		}
		groups = append(groups, taskGroup{next: start, end: end})
		start = end
	}

	ordered := make([]remoteFanoutTask, 0, len(tasks))
	for len(groups) > 0 {
		remaining := groups[:0]
		for _, group := range groups {
			ordered = append(ordered, tasks[group.next])
			group.next++
			if group.next < group.end {
				remaining = append(remaining, group)
			}
		}
		groups = remaining
	}
	copy(tasks, ordered)
}

func (r *RemoteService) DoFork(ctx context.Context, route *route.Route, protoData []byte, session session.Session) error {
	ctx = withFanoutIdempotencyScope(ctx)
	msg := &message.Message{
		Type:  message.Notify,
		Route: route.Short(),
		Data:  protoData,
	}
	// Resolve each recipient so every hop carries that target's owner epoch.
	// A type-wide broker fanout cannot represent multiple fencing tokens.
	svs, err := r.serviceDiscovery.GetServersByType(route.SvType)
	if err != nil {
		return err
	}
	targets := r.snapshotFanoutServers(svs)
	errs := make([]error, 0)
	for _, server := range targets {
		if ctxErr := ctx.Err(); ctxErr != nil {
			errs = append(errs, fmt.Errorf("server %s: fanout stopped: %w", server.ID, ctxErr))
			break
		}
		_, callErr := r.remoteCall(ctx, server, protos.RPCType_User, route, session, msg)
		if callErr != nil {
			errs = append(errs, fmt.Errorf("server %s: %w", server.ID, callErr))
			logger.Zap.Error("error making fork",
				zap.String("uid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
				zap.Stringer("route", route),
				zap.Error(callErr))
		}
	}
	return stderrors.Join(errs...)
}

func (r *RemoteService) DoForkRequest(ctx context.Context, route *route.Route, protoData []byte, session session.Session) ([]*protos.Response, error) {
	msg := &message.Message{
		Type:  message.Request,
		Route: route.Short(),
		Data:  protoData,
	}
	svs, err := r.serviceDiscovery.GetServersByType(route.SvType)
	if err != nil {
		return nil, err
	}
	targets := r.snapshotFanoutServers(svs)
	tasks := make([]remoteFanoutTask, 0, len(targets))
	for _, target := range targets {
		tasks = append(tasks, remoteFanoutTask{
			server:  target,
			route:   route,
			session: session,
			msg:     msg,
		})
	}
	return r.fanoutOwner().run(ctx, tasks)
}

func (r *RemoteService) DoPublish(ctx context.Context, rt *route.Route, protoData []byte, session session.Session) error {
	errs := make([]error, 0)
	for _, server := range snapshotServerTypes(r.serviceDiscovery.GetServerTypes()) {
		sub := server.GetSubscribe(rt.Service, rt.Method)
		// 排除非订阅者
		if sub == nil {
			continue
		}
		// 非fork时 排除当前服务
		if r.server != nil && server.Type == r.server.Type && !sub.Fork {
			continue
		}
		// 排除session未绑定的sessionStickness服务
		if !sub.Fork && server.SessionStickiness && session != nil {
			svID := session.GetBackendID(server.Type)
			if svID == "" {
				logger.Zap.Debug("DoPublish ignore unbound sessionStickness server", zap.String("server", server.Type))
				continue
			}
		}

		targetRoute := route.NewRoute(server.Type, rt.Service, rt.Method)
		if sub.Fork {
			if err := r.DoFork(ctx, targetRoute, protoData, session); err != nil {
				errs = append(errs, fmt.Errorf("server type %s: %w", server.Type, err))
			}
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			errs = append(errs, fmt.Errorf("server type %s: publish stopped: %w", server.Type, ctxErr))
			break
		}
		if err := r.DoNotify(ctx, "", targetRoute, protoData, session); err != nil {
			errs = append(errs, fmt.Errorf("server type %s: %w", server.Type, err))
		}
	}
	return stderrors.Join(errs...)
}

func (r *RemoteService) DoPublishRequest(ctx context.Context, ro *route.Route, protoData []byte, session session.Session) ([]*protos.Response, error) {
	msg := &message.Message{
		Type:  message.Request,
		Route: ro.Short(),
		Data:  protoData,
	}
	tasks := make([]remoteFanoutTask, 0)
	snapshotErrs := make([]error, 0)

	for _, server := range snapshotServerTypes(r.serviceDiscovery.GetServerTypes()) {
		sub := server.GetSubscribe(ro.Service, ro.Method)
		// 排除非订阅者
		if sub == nil {
			continue
		}
		// 非fork时 排除当前服务
		if r.server != nil && server.Type == r.server.Type && !sub.Fork {
			continue
		}

		targetRoute := route.NewRoute(server.Type, ro.Service, ro.Method)
		if sub.Fork {
			servers, err := r.serviceDiscovery.GetServersByType(server.Type)
			if err != nil {
				snapshotErrs = append(snapshotErrs, fmt.Errorf("server type %s: %w", server.Type, err))
				continue
			}
			for _, target := range r.snapshotFanoutServers(servers) {
				tasks = append(tasks, remoteFanoutTask{
					server:  target,
					route:   targetRoute,
					session: session,
					msg:     msg,
				})
			}
			continue
		}

		// 排除session未绑定的sessionStickness服务
		if server.SessionStickiness && session != nil {
			svID := session.GetBackendID(server.Type)
			if svID == "" {
				logger.Zap.Debug("DoPublish ignore unbound sessionStickness server", zap.String("server", server.Type))
				continue
			}
		}

		tasks = append(tasks, remoteFanoutTask{
			route:   targetRoute,
			session: session,
			msg:     msg,
		})
	}

	sortFanoutTasks(tasks)
	responses, fanoutErr := r.fanoutOwner().run(ctx, tasks)
	if fanoutErr != nil {
		snapshotErrs = append(snapshotErrs, fanoutErr)
	}
	return responses, stderrors.Join(snapshotErrs...)
}

// RPC makes rpcs
func (r *RemoteService) RPC(ctx context.Context, serverID string, route *route.Route, reply proto.Message, arg proto.Message, session session.Session) error {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return goerrors.WithStack(err)
		}
	}
	res, err := r.DoRPC(ctx, serverID, route, data, session)
	if err != nil {
		return err
	}

	if res.Status != nil {
		return apierrors.FromStatus(res.Status)
	}

	if reply != nil {
		err = proto.Unmarshal(res.GetData(), reply)
		if err != nil {
			return goerrors.WithStack(err)
		}
	}
	return nil
}

// Notify only support nats,don't use grpc.(copy then modify from RPC)
func (r *RemoteService) Notify(ctx context.Context, serverID string, ro *route.Route, arg proto.Message, session session.Session) error {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return err
		}
	}
	err = r.DoNotify(ctx, serverID, ro, data, session)
	if err != nil {
		return err
	}
	return nil
}

// NotifyAll 通知集群内所有服务,不包括自己
//
//	@receiver r
//	@param ctx
//	@param ro
//	@param arg
//	@param session
//	@return error
func (r *RemoteService) NotifyAll(ctx context.Context, ro *route.Route, arg proto.Message, session session.Session) error {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return err
		}
	}
	if ro.SvType != "" {
		return goerrors.WithStack(constants.ErrNotifyAllSvTypeNotEmpty)
	}
	msg := &message.Message{
		Type:  message.Notify,
		Route: ro.Short(),
		Data:  data,
	}
	tasks := make([]remoteFanoutTask, 0)
	snapshotErrs := make([]error, 0)
	// 服务器为空 全局广播(每种服务器只有一个实例消费)
	for _, server := range snapshotServerTypes(r.serviceDiscovery.GetServerTypes()) {
		// 排除自己
		if server.Type == r.server.Type {
			continue
		}
		// 排除session未绑定的sessionStickness服务
		if server.SessionStickiness && session != nil {
			svId := session.GetBackendID(server.Type)
			if svId == "" {
				logger.Zap.Debug("NotifyAll ignore unbound sessionStickness server", zap.String("server", server.Type))
				continue
			}
		}
		newRoute, err := route.Decode(server.Type + "." + ro.Short())
		if err != nil {
			snapshotErrs = append(snapshotErrs, fmt.Errorf("server type %s: %w", server.Type, err))
			continue
		}
		tasks = append(tasks, remoteFanoutTask{
			route:   newRoute,
			session: session,
			msg:     msg,
		})
	}
	sortFanoutTasks(tasks)
	_, fanoutErr := r.fanoutOwner().run(ctx, tasks)
	if fanoutErr != nil {
		snapshotErrs = append(snapshotErrs, fanoutErr)
	}
	return stderrors.Join(snapshotErrs...)
}

func (r *RemoteService) Fork(ctx context.Context, notify bool, ro *route.Route, arg proto.Message, session session.Session) ([]*protos.Response, error) {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return nil, err
		}
	}
	if notify {
		return nil, r.DoFork(ctx, ro, data, session)
	}
	return r.DoForkRequest(ctx, ro, data, session)
}

func (r *RemoteService) Publish(ctx context.Context, ro *route.Route, notify bool, arg proto.Message, session session.Session) ([]*protos.Response, error) {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return nil, err
		}
	}
	if notify {
		return nil, r.DoPublish(ctx, ro, data, session)
	}
	return r.DoPublishRequest(ctx, ro, data, session)
}

// Register registers components
func (r *RemoteService) Register(comp component.Component, opts []component.Option) error {
	s := component.NewService(comp, opts)
	r.remoteRegistryOwner.mu.Lock()
	defer r.remoteRegistryOwner.mu.Unlock()
	services := lo.If(s.Options.Subscriber, r.subscribers).Else(r.services)

	if _, ok := services[s.Name]; ok {
		return goerrors.WithStack(fmt.Errorf("remote: service already defined: %s", s.Name))
	}

	if err := s.ExtractRemote(); err != nil {
		return err
	}

	services[s.Name] = s
	// register all remotes
	for name, remote := range s.Remotes {
		remoteName := fmt.Sprintf("%s.%s", s.Name, name)
		if _, ok := r.remotes[remoteName]; ok {
			logger.Zap.Warn("remote already registered, overwriting", zap.String("name", remoteName))
		}
		r.remotes[remoteName] = remote
	}
	if s.Options.Subscriber {
		// 注册订阅信息到服务发现，供后续同步订阅时使用
		for name := range s.Remotes {
			fork := s.Options.ForkAll
			if len(s.Options.ForkMethods) > 0 {
				fork = slices.Contains(s.Options.ForkMethods, name)
			}
			r.server.AddSubscribe(s.Name, name, fork)
		}
	}
	return nil
}

// RegisterInterceptor 注册拦截分发器,优先级别高于 component.Remote
//
//	@receiver r
//	@param serviceName
//	@param interceptor
func (r *RemoteService) RegisterInterceptor(serviceName string, interceptor *component.Interceptor) {
	r.remoteRegistryOwner.mu.Lock()
	defer r.remoteRegistryOwner.mu.Unlock()
	r.interceptors[serviceName] = interceptor
}

func processRemoteMessage(ctx context.Context, req *protos.Request, r *RemoteService) *protos.Response {
	rt, err := route.Decode(req.GetMsg().GetRoute())
	if err != nil {
		response := &protos.Response{
			Status: &apierrors.Status{
				Code:    http.StatusBadRequest,
				Message: "cannot decode route",
				Metadata: map[string]string{
					"route": req.GetMsg().GetRoute(),
				},
			},
		}
		return response
	}

	switch {
	case req.Type == protos.RPCType_Sys:
		return r.handleRPCSys(ctx, req, rt)
	case req.Type == protos.RPCType_User:
		return r.handleRPCUser(ctx, req, rt)
	default:
		return &protos.Response{
			Status: &apierrors.Status{
				Code:    http.StatusBadRequest,
				Message: "invalid rpc type",
				Metadata: map[string]string{
					"route": req.GetMsg().GetRoute(),
				},
			},
		}
	}
}

func (r *RemoteService) handleRPCUser(ctx context.Context, req *protos.Request, rt *route.Route) *protos.Response {
	response := &protos.Response{}

	// 拦截器优先工作
	interceptor, ok := r.remoteRegistryOwner.lookupInterceptor(rt.Service)
	if ok {
		var err error
		var sess session.Session
		if req.Session != nil {
			a, err := agent.NewRemote(
				req.GetSession(), // 内部会优先从sessionPool中取session
				"",               // 服务器内部rpc 不需要 agent.Remote.ResponseMID()功能
				r.rpcClient,
				r.encoder,
				r.serializer,
				r.serviceDiscovery,
				req.FrontendID,
				r.messageEncoder,
				r.sessionPool,
				req.Type,
			)
			if err != nil {
				logger.Log.Warn("pitaya/handler: cannot instantiate remote agent")
				response := &protos.Response{
					Status: &apierrors.FromError(err).Status,
				}
				return response
			}
			sess = a.Session
		}
		// 和 handleRPCSys 调用的 handlerPool.ProcessHandlerMessage 一样处理，把session存入context
		if sess != nil {
			ctx = context.WithValue(ctx, constants.SessionCtxKey, sess)
			ctx = util.CtxWithDefaultLogger(ctx, rt.String(), sess.UID())
		}
		var ret any
		ret, err = interceptor.InterceptorFun(ctx, *rt, req.GetMsg().GetData())
		if err != nil {
			response := &protos.Response{
				Status: &apierrors.FromError(err).Status,
			}
			return response
		}

		var b []byte
		if ret != nil {
			pb, ok := ret.(proto.Message)
			if !ok {
				response := &protos.Response{
					Status: &apierrors.FromError(constants.ErrWrongValueType).Status,
				}
				return response
			}
			if b, err = proto.Marshal(pb); err != nil {
				response := &protos.Response{
					Status: &apierrors.FromError(err).Status,
				}
				return response
			}
		}

		response.Data = b
		return response
	}

	// 无拦截器情况下走常规remote
	remote, ok := r.remoteRegistryOwner.lookupRemote(rt.Short())
	if !ok {
		// notify 情况下很多 route 都会找不到，警告太多
		logger.Zap.Info("pitaya/remote: router not found", zap.String("route", rt.Short()))
		response := &protos.Response{
			Status: &apierrors.Status{
				Code:    http.StatusNotFound,
				Message: "route not found",
				Metadata: map[string]string{
					"route": rt.Short(),
				},
			},
		}
		return response
	}
	receiver := remote.Receiver
	if remote.Options.ReceiverProvider != nil {
		rec := remote.Options.ReceiverProvider(ctx)
		if rec == nil {
			logger.Log.Warnf("pitaya/remote: %s not found,the ReceiverProvider return nil", rt.Short())
			response := &protos.Response{
				Status: &apierrors.Status{
					Code:    http.StatusNotFound,
					Message: "route not found,ReceiverProvider return nil",
					Metadata: map[string]string{
						"route": rt.Short(),
					},
				},
			}
			return response
		}
		receiver = reflect.ValueOf(rec)
	}

	var sess session.Session
	if req.Session != nil {
		a, err := agent.NewRemote(
			req.GetSession(), // 内部会优先从sessionPool中取session
			"",               // 服务器内部rpc 不需要 agent.Remote.ResponseMID()功能
			r.rpcClient,
			r.encoder,
			r.serializer,
			r.serviceDiscovery,
			req.FrontendID,
			r.messageEncoder,
			r.sessionPool,
			req.Type,
		)
		if err != nil {
			logger.Log.Warn("pitaya/handler: cannot instantiate remote agent")
			response := &protos.Response{
				Status: &apierrors.FromError(err).Status,
			}
			return response
		}
		sess = a.Session
	}
	// 和 handleRPCSys 调用的 handlerPool.ProcessHandlerMessage 一样处理，把session存入context
	if sess != nil {
		ctx = context.WithValue(ctx, constants.SessionCtxKey, sess)
		ctx = util.CtxWithDefaultLogger(ctx, rt.String(), sess.UID())
	}

	var ret interface{}
	var arg interface{}
	var err error

	if remote.HasArgs {
		arg, err = unmarshalRemoteArg(remote, req.GetMsg().GetData())
		if err != nil {
			response := &protos.Response{
				Status: &apierrors.Status{
					Code:    http.StatusNotFound,
					Message: err.Error(),
					Metadata: map[string]string{
						"route": rt.Short(),
					},
				},
			}
			return response
		}
	}

	ctx, arg, err = r.remoteHooks.BeforeHandler.ExecuteBeforePipeline(ctx, rt, arg)
	if err != nil {
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
	}

	params := []reflect.Value{receiver, reflect.ValueOf(ctx)}
	if remote.HasArgs {
		params = append(params, reflect.ValueOf(arg))
	}
	// Remote Handler 的 ret/err 仍由原有 RPC 流程处理；executionErr 只表示执行器调度或完成协议失败。
	// executeTask 会等待已开始的闭包收敛，返回后读取 ret/err 不会和后台 task 发生数据竞争。
	executionErr := executeTask(ctx, remote.Options.TaskExecutor, func(taskCtx context.Context) error {
		// 执行器可能为队列任务派生新的 context，反射参数需要同步替换。
		params[1] = reflect.ValueOf(taskCtx)
		ret, err = util.Pcall(remote.Method, params)
		// 业务错误通过外层 err 进入 AfterHandler 和 RPC status；这里不把它混入执行器错误。
		return nil
	})
	if executionErr != nil {
		if acknowledgeSkippedTask(req.GetMsg().GetType(), executionErr, rt, "remote") {
			response.Data = []byte("ack")
			return response
		}
		response.Status = &apierrors.FromError(normalizeTaskExecutionError(executionErr)).Status
		return response
	}

	ret, err = r.remoteHooks.AfterHandler.ExecuteAfterPipeline(ctx, rt, arg, ret, err)
	if err != nil {
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
	}
	if err != nil {
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
	}

	var b []byte
	if ret != nil {
		pb, ok := ret.(proto.Message)
		if !ok {
			response := &protos.Response{
				Status: &apierrors.FromError(constants.ErrWrongValueType).Status,
			}
			return response
		}
		if b, err = proto.Marshal(pb); err != nil {
			response := &protos.Response{
				Status: &apierrors.FromError(err).Status,
			}
			return response
		}
	}

	response.Data = b
	return response
}

func (r *RemoteService) handleRPCSys(ctx context.Context, req *protos.Request, rt *route.Route) *protos.Response {
	reply := req.GetMsg().GetReply()
	response := &protos.Response{}
	// (warning) a new agent is created for every new request
	a, err := agent.NewRemote(
		req.GetSession(),
		reply,
		r.rpcClient,
		r.encoder,
		r.serializer,
		r.serviceDiscovery,
		req.FrontendID,
		r.messageEncoder,
		r.sessionPool,
		req.Type,
	)
	if err != nil {
		logger.Log.Warn("pitaya/handler: cannot instantiate remote agent")
		response := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		return response
	}

	ret, err := r.handlerPool.ProcessHandlerMessage(ctx, rt, r.serializer, r.handlerHooks, a.Session, req.GetMsg().GetData(), req.GetMsg().GetType(), true)
	if err != nil {
		logger.Zap.Warn("", zap.Error(err))
		response = &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
	} else {
		response = &protos.Response{Data: ret}
	}
	return response
}

func (r *RemoteService) remoteCall(
	ctx context.Context,
	server *cluster.Server,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
) (*protos.Response, error) {
	var err error
	target := server

	if target == nil {
		target, err = r.resolveRemoteTarget(ctx, rpcType, route, session, msg)
		if err != nil {
			return nil, err
		}
	}
	ctx = target.WithOwnerFencingMetadata(ctx)
	ctx = cluster.PrepareOutboundRPCContext(ctx)

	res, err := r.rpcClient.Call(ctx, rpcType, route, session, msg, target)
	if err != nil {
		logg := logger.Zap.With(util.LogFieldsFromCtx(ctx)...)
		code := apierrors.Code(err)
		logFun := lo.If(code >= http.StatusInternalServerError, logg.Error).Else(logg.Warn)
		logFun("error making call to target",
			zap.String("targetUid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
			zap.Stringer("targetRoute", route), zap.String("host", target.Hostname), zap.String("svID", target.ID),
			zap.Error(err))
		return nil, err
	}
	return res, err
}

func (r *RemoteService) resolveRemoteTarget(
	ctx context.Context,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
) (*cluster.Server, error) {
	target, err := r.router.Route(ctx, rpcType, route.SvType, route, msg, session)
	if err == nil && target == nil {
		err = fmt.Errorf("%w server=%s", constants.ErrServerNotFound, route.SvType)
	}
	if err == nil {
		return target, nil
	}

	apiErr := apierrors.FromError(err)
	if rpcType == protos.RPCType_Sys {
		logW := util.GetLoggerFromCtx(ctx)
		logFun := lo.If(int(apiErr.Status.Code) >= http.StatusInternalServerError, logW.Error).Else(logW.Warn)
		logFun("Failed to respond to remote server", zap.String("route", route.String()), zap.Error(err))
	}
	return nil, apiErr
}

// DumpServices outputs all registered services
func (r *RemoteService) DumpServices() {
	for _, name := range r.remoteRegistryOwner.remoteNames() {
		logger.Log.Infof("registered remote %s", name)
	}
}

// Docs returns documentation for remotes
func (r *RemoteService) Docs(getPtrNames bool) (map[string]interface{}, error) {
	if r == nil {
		return map[string]interface{}{}, nil
	}
	return docgenerator.RemotesDocs(r.server.Type, r.remoteRegistryOwner.servicesSnapshot(), getPtrNames)
}
