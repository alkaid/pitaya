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

package pitaya

import (
	"context"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"

	"go.uber.org/zap"

	"go.opentelemetry.io/otel/trace"

	"google.golang.org/protobuf/types/descriptorpb"

	"time"

	"github.com/go-redis/redis/v8"
	"github.com/topfreegames/pitaya/v2/co"
	"github.com/topfreegames/pitaya/v2/pipeline"
	"github.com/topfreegames/pitaya/v2/protos"

	"github.com/topfreegames/pitaya/v2/acceptor"
	"github.com/topfreegames/pitaya/v2/cluster"
	"github.com/topfreegames/pitaya/v2/component"
	"github.com/topfreegames/pitaya/v2/config"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/constants"
	pcontext "github.com/topfreegames/pitaya/v2/context"
	"github.com/topfreegames/pitaya/v2/docgenerator"
	"github.com/topfreegames/pitaya/v2/groups"
	"github.com/topfreegames/pitaya/v2/interfaces"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/metrics"
	mods "github.com/topfreegames/pitaya/v2/modules"
	"github.com/topfreegames/pitaya/v2/remote"
	"github.com/topfreegames/pitaya/v2/router"
	"github.com/topfreegames/pitaya/v2/serialize"
	"github.com/topfreegames/pitaya/v2/service"
	"github.com/topfreegames/pitaya/v2/session"
	"github.com/topfreegames/pitaya/v2/timer"
	"github.com/topfreegames/pitaya/v2/tracing"
	"github.com/topfreegames/pitaya/v2/worker"
	"google.golang.org/protobuf/proto"
)

// ServerMode represents a server mode
type ServerMode byte

const (
	_ ServerMode = iota
	// Cluster represents a server running with connection to other servers
	Cluster
	// Standalone represents a server running without connection to other servers
	Standalone
)

// Pitaya App interface
type Pitaya interface {
	GetDieChan() chan bool
	SetHeartbeatTime(interval time.Duration)
	GetServerID() string
	GetMetricsReporters() []metrics.Reporter
	// GetMetricsReporterMgr GetMetricsReporters
	//  集合的包装,各接口方法内部自动迭代集合 注意永远不会返回error
	//  @return metrics.Reporter
	GetMetricsReporterMgr() metrics.Reporter
	GetServer() *cluster.Server
	GetServerByID(id string) (*cluster.Server, error)
	GetServersByType(t string) (map[string]*cluster.Server, error)
	// GetServerTypes 获得serverType列表,取每种serverType的第一个Server
	//  @receiver sd
	//  @return map[string]*Server 索引为serverType,元素为Server
	GetServerTypes() map[string]*cluster.Server
	GetServers() []*cluster.Server
	// LeaderID 获取主节点ID 只有在配置 sd.etcd.election.enable 开启时有效
	//
	// @return string
	LeaderID() string
	// IsLeader 是否主节点 只有在配置 sd.etcd.election.enable 开启时有效
	//
	// @return bool
	IsLeader() bool
	// FlushServer2Cluster 将修改后的server数据保存到云端(etcd)
	//  @param server
	//  @return error
	FlushServer2Cluster(server *cluster.Server) error
	// AddServerDiscoveryListener 添加服务发现中服务的生命周期监听
	//  @receiver app
	//  @param listener
	AddServerDiscoveryListener(listener cluster.SDListener)
	// AddConfLoader 添加配置重载回调
	//  @param loader
	AddConfLoader(loader config.ConfLoader)
	// PushBeforeHandleHook 添加handle执行前的hook
	//  @param beforeFunc
	PushBeforeHandleHook(beforeFunc pipeline.HandlerTempl)
	// PushAfterHandleHook 添加handle执行后的hook
	//  @param afterFunc
	PushAfterHandleHook(afterFunc pipeline.AfterHandlerTempl)
	// PushBeforeRemoteHook 添加remote执行前的hook
	//  @param beforeFunc
	PushBeforeRemoteHook(beforeFunc pipeline.HandlerTempl)
	// PushAfterRemoteHook 添加remote执行后的hook
	//  @param afterFunc
	PushAfterRemoteHook(afterFunc pipeline.AfterHandlerTempl)
	GetSessionFromCtx(ctx context.Context) session.SessPublic
	// GetSessionByUID 根据uid获取session,尝试顺序 sessionPool>context>cache cluster
	//
	// @param ctx
	// @param uid
	// @return session.SessPublic
	// @return error
	GetSessionByUID(ctx context.Context, uid string) (session.SessPublic, error)
	// OnStarted 设置 Start 后的回调,内部会启一个goroutine执行
	//  @param fun
	OnStarted(fun func())
	Start()
	SetDictionary(dict map[string]uint16) error
	AddRoute(serverType string, routingFunction router.RoutingFunc) error
	Shutdown()
	StartWorker()
	RegisterRPCJob(rpcJob worker.RPCJob) error
	Documentation(getPtrNames bool) (map[string]interface{}, error)
	IsRunning() bool
	// SetSessionCache 自定义session缓存设施
	//  @param cache
	SetSessionCache(cache session.CacheInterface)
	// AddSessionListener 添加session状态监听
	//  @param listener
	AddSessionListener(listener cluster.RemoteSessionListener)
	// RangeUsers 遍历所有已绑定用户
	//
	// @param f
	RangeUsers(f func(uid string, sess session.SessPublic) bool)
	// RangeSessions 遍历所有session,包括未绑定
	//
	// @param f
	RangeSessions(f func(sid int64, sess session.SessPublic) bool)
	// SessionCount
	//  仅 frontend 和 sessionStickness backend 有效
	//
	// @return int64
	SessionCount() int64
	// UserCount Bound user's count
	//  仅 frontend 和 sessionStickness backend 有效
	//
	// @return int64
	UserCount() int64
	// Notify 通知其他服务,无阻塞,无返回值。如果session绑定了backend,则会路由到持有session引用的backend
	//  @param ctx
	//  @param routeStr
	//  @param arg
	//  @param uid 若不为空会携带session数据
	//  @return error
	Notify(ctx context.Context, routeStr string, arg proto.Message, uid string) error
	// NotifyTo 通知指定服务,无阻塞,无返回值
	//  @param ctx
	//  @param serverID
	//  @param routeStr
	//  @param arg
	//  @return error
	NotifyTo(ctx context.Context, serverID, routeStr string, arg proto.Message) error
	// NotifyAll 通知集群内所有服务，每种服务只有一个实例消费
	//  @param ctx
	//  @param routeStr 不能带有serverType,否则报错
	//  @param arg
	//  @param uid
	//  @return error
	NotifyAll(ctx context.Context, routeStr string, arg proto.Message, uid string) error
	// Fork rpc调用同一类服务的所有实例,非阻塞.一般来说仅适用于目标服务为stateful类型时
	//  若fork的服务类型和自己相同,则自己也会收到消息
	//  @param ctx
	//  @param routeStr
	//  @param arg
	//  @param uid 若不为空会携带session数据
	Fork(ctx context.Context, routeStr string, arg proto.Message, uid string) error
	// PublishRequest 发布一个topic,会阻塞等待请求返回
	//  @param ctx
	//  @param topic 主题,框架会自动加上 pitaya.publish. 的前缀,若topic中含有"."会自动转化为"_"
	//  @param arg
	//  @param uid
	//  @return []*protos.Response 自行判断status以及反序列化data,可以使用protoAny,switch typeUrl来解析
	//  @return error
	PublishRequest(ctx context.Context, topic string, arg proto.Message, uid string) ([]*protos.Response, error)
	// Publish 发布一个topic,不会阻塞
	//  @param ctx
	//  @param topic
	//  @param arg
	//  @param uid
	//  @return error
	Publish(ctx context.Context, topic string, arg proto.Message, uid string) error
	// RPC 根据route调用remote,会阻塞等待 reply 。如果uid不为空且目标服务器是stateful,则会路由到持有session引用的服
	//  @param ctx
	//  @param routeStr
	//  @param reply
	//  @param arg
	//  @param uid 若不为空会携带session数据
	//  @return error
	RPC(ctx context.Context, routeStr string, reply proto.Message, arg proto.Message, uid string) error
	// RPCTo 根据route调用remote,会阻塞等待 reply
	//
	// @param ctx
	// @param serverID
	// @param routeStr
	// @param reply
	// @param arg
	// @return error
	RPCTo(ctx context.Context, serverID, routeStr string, reply proto.Message, arg proto.Message) error
	// RpcRaw RPC 的[]byte形式调用
	//
	// @param ctx
	// @param serverID
	// @param routeStr
	// @param arg
	// @param uid
	// @return []byte
	// @return error
	RpcRaw(ctx context.Context, serverID, routeStr string, arg []byte, uid string) ([]byte, error)
	ReliableRPC(
		routeStr string,
		metadata map[string]interface{},
		reply, arg proto.Message,
	) (jid string, err error)
	ReliableRPCWithOptions(
		routeStr string,
		metadata map[string]interface{},
		reply, arg proto.Message,
		opts *config.EnqueueOpts,
	) (jid string, err error)

	SendPushToUsers(route string, v interface{}, uids []string, frontendType string) ([]string, error)
	// SendKickToUsers
	//  @param uids
	//  @param frontendType
	//  @param callback 透传数据,会在 cluster.RemoteSessionListener .OnUserDisconnected()里回传，使用 AddSessionListener 设置该回调
	//  @return []string
	//  @return error
	SendKickToUsers(uids []string, frontendType string, callback map[string]string) ([]string, error)

	GroupCreate(ctx context.Context, groupName string) error
	GroupCreateWithTTL(ctx context.Context, groupName string, ttlTime time.Duration) error
	GroupMembers(ctx context.Context, groupName string) ([]string, error)
	GroupBroadcast(ctx context.Context, frontendType, groupName, route string, v interface{}) error
	GroupContainsMember(ctx context.Context, groupName, uid string) (bool, error)
	GroupAddMember(ctx context.Context, groupName, uid string) error
	GroupRemoveMember(ctx context.Context, groupName, uid string) error
	GroupRemoveAll(ctx context.Context, groupName string) error
	GroupCountMembers(ctx context.Context, groupName string) (int, error)
	GroupRenewTTL(ctx context.Context, groupName string) error
	GroupDelete(ctx context.Context, groupName string) error

	// Register 注册handle(终端api响应)
	//  @param c
	//  @param options
	Register(c component.Component, options ...component.Option)
	// RegisterRemote 注册remote(远端rpc响应)
	//  @param c
	//  @param options
	RegisterRemote(c component.Component, options ...component.Option)
	// RegisterSubscribe 注册subscribe(publish的远端响应)
	//
	// 若要使用消费组,请使用 component.WithSubscriberGroup
	//
	// 不同subscriber之间的方法名不能重名
	//  @param c
	//  @param options
	//
	RegisterSubscribe(c component.Component, options ...component.Option)
	// LazyRegisterSubscribe 注册subscribe(publish的远端响应) 意系统不会回调该component的生命周期函数
	//
	// 若要使用消费组,请使用 component.WithSubscriberGroup
	//
	// 不同subscriber之间的方法名不能相同
	//  @param c
	//  @param options
	LazyRegisterSubscribe(c component.Component, options ...component.Option)
	// LazyRegister 延迟注册handle(终端api响应)
	//  @param c 注意系统不会回调该component的生命周期函数
	//  @param options
	LazyRegister(c component.Component, options ...component.Option)
	// LazyRegisterRemote  延迟注册remote(远端rpc响应)
	//  @param c 注意系统不会回调该component的生命周期函数
	//  @param options
	LazyRegisterRemote(c component.Component, options ...component.Option)

	// RegisterInterceptor 注册handle的拦截器,注册后原hande的消息响应将不再分发给handle
	//  @param serviceName
	//  @param interceptor
	RegisterInterceptor(serviceName string, interceptor *component.Interceptor)
	// RegisterRemoteInterceptor 注册remote的拦截器,注册后原remote的消息响应将不再分发给remote
	//  @param serviceName
	//  @param interceptor
	RegisterRemoteInterceptor(serviceName string, interceptor *component.Interceptor)
	RegisterModule(module interfaces.Module, name string) error
	RegisterModuleAfter(module interfaces.Module, name string) error
	RegisterModuleBefore(module interfaces.Module, name string) error
	GetModule(name string) (interfaces.Module, error)

	// BindBackend bind session in stateful backend 注意业务层若当前服务是frontend时请勿调用。frontend时仅框架内自己调用
	//  @param ctx
	//  @param uid
	//  @param targetServerType 要绑定的stateful backend服务
	//  @param targetServerID 要绑定的stateful backend id
	//  @param callback 回调数据,通知其他服务时透传
	//  @return error
	BindBackend(ctx context.Context, uid string, targetServerType string, targetServerID string, callback map[string]string) error

	// KickBackend 解绑backend
	//  @param ctx
	//  @param uid
	//  @param targetServerType 目标服
	//  @param callback 回调数据,通知其他服务时透传
	//  @param reason
	//  @return error
	KickBackend(ctx context.Context, uid string, targetServerType string, callback map[string]string) error

	// GetLocalSessionByUid 获取本地已绑定的session,若没有返回空. 可用于判断uid是否绑定到了该服务器
	//
	// @param ctx
	// @param uid
	// @return session.SessPublic
	GetLocalSessionByUid(ctx context.Context, uid string) session.SessPublic
	// OnSessionClose 设置本地session关闭后的回调
	//
	// @receiver app
	// @param f
	OnSessionClose(f session.OnSessionCloseFunc)
	// OnAfterSessionBind 设置本地session bind后的回调
	//  @param f
	OnAfterSessionBind(f session.OnSessionBindFunc)
	// OnAfterBindBackend 设置本地session bind backend后的回调
	//  @param f
	OnAfterBindBackend(f session.OnSessionBindBackendFunc)
	// OnAfterKickBackend 设置本地session 解绑 backend后的回调
	//  @param f
	OnAfterKickBackend(f session.OnSessionKickBackendFunc)
}

// App is the base app struct
type App struct {
	acceptors          []acceptor.Acceptor
	config             config.PitayaConfig
	dieChan            chan bool
	heartbeat          time.Duration
	router             *router.Router
	rpcClient          cluster.RPCClient
	rpcServer          cluster.RPCServer
	metricsReporters   []metrics.Reporter
	metricsReporterMgr metrics.Reporter
	running            bool
	serializer         serialize.Serializer
	server             *cluster.Server
	serverMode         ServerMode
	serviceDiscovery   cluster.ServiceDiscovery
	startAt            time.Time
	worker             *worker.Worker
	remoteService      *service.RemoteService
	handlerService     *service.HandlerService
	handlerComp        []regComp
	remoteComp         []regComp
	modulesMap         map[string]interfaces.Module
	modulesArr         []moduleWrapper
	groups             groups.GroupService
	sessionPool        session.SessionPool
	redis              redis.Cmdable
	conf               *config.Config
	onStarted          func()
}

// NewApp is the base constructor for a pitaya app instance
func NewApp(
	serverMode ServerMode,
	serializer serialize.Serializer,
	acceptors []acceptor.Acceptor,
	dieChan chan bool,
	router *router.Router,
	server *cluster.Server,
	rpcClient cluster.RPCClient,
	rpcServer cluster.RPCServer,
	worker *worker.Worker,
	serviceDiscovery cluster.ServiceDiscovery,
	remoteService *service.RemoteService,
	handlerService *service.HandlerService,
	groups groups.GroupService,
	sessionPool session.SessionPool,
	metricsReporters []metrics.Reporter,
	config config.PitayaConfig,
) *App {
	app := &App{
		server:           server,
		config:           config,
		rpcClient:        rpcClient,
		rpcServer:        rpcServer,
		worker:           worker,
		serviceDiscovery: serviceDiscovery,
		remoteService:    remoteService,
		handlerService:   handlerService,
		groups:           groups,
		startAt:          time.Now(),
		dieChan:          dieChan,
		acceptors:        acceptors,
		metricsReporters: metricsReporters,
		serverMode:       serverMode,
		running:          false,
		serializer:       serializer,
		router:           router,
		handlerComp:      make([]regComp, 0),
		remoteComp:       make([]regComp, 0),
		modulesMap:       make(map[string]interfaces.Module),
		modulesArr:       []moduleWrapper{},
		sessionPool:      sessionPool,
	}
	if app.heartbeat == time.Duration(0) {
		app.heartbeat = config.Heartbeat.Interval
	}
	app.metricsReporterMgr = metrics.NewReporterMgr(metricsReporters)

	app.initSysRemotes()
	return app
}

// GetDieChan gets the channel that the app sinalizes when its going to die
func (app *App) GetDieChan() chan bool {
	return app.dieChan
}

// SetHeartbeatTime sets the heartbeat time
func (app *App) SetHeartbeatTime(interval time.Duration) {
	app.heartbeat = interval
}

// GetServerID returns the generated server id
func (app *App) GetServerID() string {
	return app.server.ID
}

// GetMetricsReporters gets registered metrics reporters
func (app *App) GetMetricsReporters() []metrics.Reporter {
	return app.metricsReporters
}

func (app *App) GetMetricsReporterMgr() metrics.Reporter {
	return app.metricsReporterMgr
}

// GetServer gets the local server instance
func (app *App) GetServer() *cluster.Server {
	return app.server
}

// GetServerByID returns the server with the specified id
func (app *App) GetServerByID(id string) (*cluster.Server, error) {
	return app.serviceDiscovery.GetServer(id)
}

// GetServersByType get all servers of type
func (app *App) GetServersByType(t string) (map[string]*cluster.Server, error) {
	return app.serviceDiscovery.GetServersByType(t)
}

// GetServerTypes
//
//	@implement App.GetServerTypes
func (app *App) GetServerTypes() map[string]*cluster.Server {
	return app.serviceDiscovery.GetServerTypes()
}

// GetServers get all servers
func (app *App) GetServers() []*cluster.Server {
	return app.serviceDiscovery.GetServers()
}

func (app *App) LeaderID() string {
	return app.serviceDiscovery.LeaderID()
}
func (app *App) IsLeader() bool {
	return app.serviceDiscovery.IsLeader()
}

// FlushServer2Cluster
//
//	@implement Pitaya.FlushServer2Cluster
//	@receiver app
//	@param server
//	@return error
func (app *App) FlushServer2Cluster(server *cluster.Server) error {
	return app.serviceDiscovery.FlushServer2Cluster(server)
}

// AddServerDiscoveryListener
//
//	@implement Pitaya.AddServerDiscoveryListener
//	@receiver app
//	@param listener
func (app *App) AddServerDiscoveryListener(listener cluster.SDListener) {
	app.serviceDiscovery.AddListener(listener)
}

// AddConfLoader
//
//	@implement Pitaya.AddConfLoader
//	@receiver app
//	@param loader
func (app *App) AddConfLoader(loader config.ConfLoader) {
	if app.conf == nil {
		logger.Zap.Error("app conf is nil!")
		return
	}
	app.conf.AddLoader(loader)
}

func (app *App) PushBeforeHandleHook(beforeFunc pipeline.HandlerTempl) {
	app.handlerService.GetHandleHooks().BeforeHandler.PushBack(beforeFunc)
}
func (app *App) PushAfterHandleHook(afterFunc pipeline.AfterHandlerTempl) {
	app.handlerService.GetHandleHooks().AfterHandler.PushBack(afterFunc)
}
func (app *App) PushBeforeRemoteHook(beforeFunc pipeline.HandlerTempl) {
	app.remoteService.GetHandleHooks().BeforeHandler.PushBack(beforeFunc)
}
func (app *App) PushAfterRemoteHook(afterFunc pipeline.AfterHandlerTempl) {
	app.remoteService.GetHandleHooks().AfterHandler.PushBack(afterFunc)
}

// IsRunning indicates if the Pitaya app has been initialized. Note: This
// doesn't cover acceptors, only the pitaya internal registration and modules
// initialization.
func (app *App) IsRunning() bool {
	return app.running
}

func (app *App) SetSessionCache(cache session.CacheInterface) {
	app.sessionPool.SetClusterCache(cache)
}

func (app *App) AddSessionListener(listener cluster.RemoteSessionListener) {
	app.remoteService.AddRemoteSessionListener(listener)
}

func (app *App) RangeUsers(f func(uid string, sess session.SessPublic) bool) {
	app.sessionPool.RangeUsers(f)
}
func (app *App) RangeSessions(f func(sid int64, sess session.SessPublic) bool) {
	app.sessionPool.RangeSessions(f)
}
func (app *App) SessionCount() int64 {
	return app.sessionPool.GetSessionCount()
}
func (app *App) UserCount() int64 {
	return app.sessionPool.GetUserCount()
}

// BindBackend bind session in stateful backend 注意业务层若当前服务是frontend时请勿调用。frontend时仅框架内自己调用
//
//	@param ctx
//	@param uid
//	@param targetServerType 要绑定的stateful backend服务
//	@param targetServerID 要绑定的stateful backend id
//	@param callback 回调数据,通知其他服务时透传
//	@return error
func (app *App) BindBackend(ctx context.Context, uid string, targetServerType string, targetServerID string, callback map[string]string) error {
	sess, err := app.imperfectSessionForRPC(ctx, uid)
	if err != nil {
		return err
	}
	return sess.BindBackend(ctx, targetServerType, targetServerID, callback)
}

// KickBackend 解绑backend
//
//	@param ctx
//	@param uid
//	@param targetServerType 目标服
//	@param callback 回调数据,通知其他服务时透传
//	@param reason
//	@return error
func (app *App) KickBackend(ctx context.Context, uid string, targetServerType string, callback map[string]string) error {
	sess, err := app.imperfectSessionForRPC(ctx, uid)
	if err != nil {
		return err
	}
	return sess.KickBackend(ctx, targetServerType, callback)
}

// GetLocalSessionByUid 获取本地已绑定的session,若没有返回空. 可用于判断uid是否绑定到了该服务器
//
// @receiver app
// @param ctx
// @param uid
// @return session.SessPublic
func (app *App) GetLocalSessionByUid(ctx context.Context, uid string) session.SessPublic {
	return app.sessionPool.GetSessionByUID(uid)
}

func (app *App) OnSessionClose(f session.OnSessionCloseFunc) {
	app.sessionPool.OnSessionClose(f)
}
func (app *App) OnAfterSessionBind(f session.OnSessionBindFunc) {
	app.sessionPool.OnAfterSessionBind(f)
}
func (app *App) OnAfterBindBackend(f session.OnSessionBindBackendFunc) {
	app.sessionPool.OnAfterBindBackend(f)
}
func (app *App) OnAfterKickBackend(f session.OnSessionKickBackendFunc) {
	app.sessionPool.OnAfterKickBackend(f)
}

func (app *App) initSysRemotes() {
	sys := remote.NewSys(app.sessionPool, app.server, app.serviceDiscovery, app.rpcClient, app.remoteService)
	app.RegisterRemote(sys,
		component.WithName("sys"),
		component.WithNameFunc(strings.ToLower),
	)
	app.Register(sys,
		component.WithName("sys"),
		component.WithNameFunc(strings.ToLower),
	)
}

func (app *App) periodicMetrics() {
	period := app.config.Metrics.Period
	co.Go(func() { metrics.ReportSysMetrics(app.metricsReporters, period) })

	if app.worker.Started() {
		co.Go(func() { worker.Report(app.metricsReporters, period) })
	}
}

func (app *App) OnStarted(fun func()) {
	app.onStarted = fun
}

// Start starts the app
func (app *App) Start() {
	if !app.server.Frontend && len(app.acceptors) > 0 {
		logger.Zap.Fatal("acceptors are not allowed on backend servers")
	}

	if app.server.Frontend && len(app.acceptors) == 0 {
		logger.Zap.Fatal("frontend servers should have at least one configured acceptor")
	}

	if app.serverMode == Cluster {
		if reflect.TypeOf(app.rpcClient) == reflect.TypeOf(&cluster.GRPCClient{}) {
			app.serviceDiscovery.AddListener(app.rpcClient.(*cluster.GRPCClient))
		}

		if err := app.RegisterModuleBefore(app.rpcServer, "rpcServer"); err != nil {
			logger.Zap.Fatal("failed to register rpc server module", zap.Error(err))
		}
		if err := app.RegisterModuleBefore(app.rpcClient, "rpcClient"); err != nil {
			logger.Zap.Fatal("failed to register rpc client module: %s", zap.Error(err))
		}
		// set the service discovery as the last module to be started to ensure
		// all modules have been properly initialized before the server starts
		// receiving requests from other pitaya servers
		if err := app.RegisterModuleAfter(app.serviceDiscovery, "serviceDiscovery"); err != nil {
			logger.Zap.Fatal("failed to register service discovery module: %s", zap.Error(err))
		}
	}

	app.periodicMetrics()

	app.listen()

	defer func() {
		timer.GlobalTicker.Stop()
		app.running = false
	}()

	// 用户回调
	if app.onStarted != nil {
		co.Go(func() {
			app.onStarted()
		})
	}

	sg := make(chan os.Signal)
	signal.Notify(sg, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGTERM)

	// stop server
	select {
	case <-app.dieChan:
		logger.Zap.Warn("the app will shutdown in a few seconds")
	case s := <-sg:
		logger.Sugar.Warn("got signal: ", s, ", shutting down...")
		close(app.dieChan)
	}

	logger.Zap.Warn("server is stopping...")

	app.sessionPool.CloseAll()
	app.shutdownModules()
	app.shutdownComponents()
}

func (app *App) listen() {
	app.startupComponents()
	// create global ticker instance, timer precision could be customized
	// by SetTimerPrecision
	timer.GlobalTicker = time.NewTicker(timer.Precision)

	logger.Sugar.Infof("starting server %s:%s", app.server.Type, app.server.ID)
	for i := 0; i < app.config.Concurrency.Handler.Dispatch; i++ {
		threadID := i // 避免闭包值拷贝问题
		co.Go(func() { app.handlerService.Dispatch(threadID) })
	}
	for _, acc := range app.acceptors {
		a := acc
		// TODO 池化效果待验证
		co.Go(func() {
			for conn := range a.GetConnChan() {
				connV := conn // 避免闭包值拷贝问题
				co.Go(func() { app.handlerService.Handle(connV) })
			}
		})
		if app.config.Acceptor.ProxyProtocol {
			logger.Log.Info("Enabling PROXY protocol for inbond connections")
			a.EnableProxyProtocol()
		} else {
			logger.Log.Debug("PROXY protocol is disabled for inbound connections")
		}
		co.Go(func() {
			a.ListenAndServe()
		})

		logger.Sugar.Infof("listening with acceptor %s on addr %s", reflect.TypeOf(a), a.GetAddr())
	}

	// 改为强制唯一session
	// if app.serverMode == Cluster && app.config.Session.Unique {
	if app.serverMode == Cluster {
		unique := mods.NewUniqueSession(app.server, app.rpcServer, app.rpcClient, app.sessionPool)
		app.remoteService.AddRemoteBindingListener(unique)
		app.RegisterModule(unique, "uniqueSession")
	}
	// 注册配置重载回调
	app.RegisterModuleBefore(config.NewConfigModule(app.conf), "configLoader")
	statefulGoPool := co.NewStatefulPoolsModule(app.config.GoPools, app.metricsReporters)

	app.RegisterModuleBefore(statefulGoPool, "statefulGoPool")

	app.startModules()

	logger.Zap.Info("all modules started!")

	app.running = true
}

// SetDictionary sets routes map
func (app *App) SetDictionary(dict map[string]uint16) error {
	if app.running {
		return constants.ErrChangeDictionaryWhileRunning
	}
	return message.SetDictionary(dict)
}

// AddRoute adds a routing function to a server type
func (app *App) AddRoute(
	serverType string,
	routingFunction router.RoutingFunc,
) error {
	if app.router != nil {
		if app.running {
			return constants.ErrChangeRouteWhileRunning
		}
		app.router.AddRoute(serverType, routingFunction)
	} else {
		return constants.ErrRouterNotInitialized
	}
	return nil
}

// Shutdown send a signal to let 'pitaya' shutdown itself.
func (app *App) Shutdown() {
	select {
	case <-app.dieChan: // prevent closing closed channel
	default:
		close(app.dieChan)
	}
}

//
// // Error creates a new error with a code, message and metadata
// func Error(err error, code string, metadata ...map[string]string) *errors.Error {
// 	return errors.NewError(err, code, metadata...)
// }

// GetSessionFromCtx retrieves a session from a given context
func (app *App) GetSessionFromCtx(ctx context.Context) session.SessPublic {
	sessionVal := ctx.Value(constants.SessionCtxKey)
	if sessionVal == nil {
		logger.Zap.Debug("ctx doesn't contain a session, are you calling GetSessionFromCtx from inside a remote?")
		return nil
	}
	return sessionVal.(session.SessPublic)
}

func (app *App) GetSessionByUID(ctx context.Context, uid string) (session.SessPublic, error) {
	return app.imperfectSessionForRPC(ctx, uid)
}

// GetDefaultLoggerFromCtx returns the default logger from the given context
func GetDefaultLoggerFromCtx(ctx context.Context) *zap.Logger {
	l := ctx.Value(constants.LoggerCtxKey)
	if l == nil {
		return logger.Zap
	}

	return l.(*zap.Logger)
}

// AddMetricTagsToPropagateCtx adds a key and metric tags that will
// be propagated through RPC calls. Use the same tags that are at
// 'pitaya.metrics.additionalTags' config
func AddMetricTagsToPropagateCtx(
	ctx context.Context,
	tags map[string]string,
) context.Context {
	return pcontext.AddToPropagateCtx(ctx, constants.MetricTagsKey, tags)
}

// AddToPropagateCtx adds a key and value that will be propagated through RPC calls
func AddToPropagateCtx(ctx context.Context, key string, val interface{}) context.Context {
	return pcontext.AddToPropagateCtx(ctx, key, val)
}

// GetFromPropagateCtx adds a key and value that came through RPC calls
func GetFromPropagateCtx(ctx context.Context, key string) interface{} {
	return pcontext.GetFromPropagateCtx(ctx, key)
}

// ExtractSpan retrieves an opentracing span context from the given context
// The span context can be received directly or via an RPC call
func ExtractSpan(ctx context.Context) (context.Context, trace.SpanContext) {
	return tracing.ExtractSpan(ctx)
}

// Documentation returns handler and remotes documentacion
func (app *App) Documentation(getPtrNames bool) (map[string]interface{}, error) {
	handlerDocs, err := app.handlerService.Docs(getPtrNames)
	if err != nil {
		return nil, err
	}
	remoteDocs, err := app.remoteService.Docs(getPtrNames)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"handlers": handlerDocs,
		"remotes":  remoteDocs,
	}, nil
}

// AddGRPCInfoToMetadata adds host, external host and
// port into metadata
func AddGRPCInfoToMetadata(
	metadata map[string]string,
	region string,
	host, port string,
	externalHost, externalPort string,
) map[string]string {
	metadata[constants.GRPCHostKey] = host
	metadata[constants.GRPCPortKey] = port
	metadata[constants.GRPCExternalHostKey] = externalHost
	metadata[constants.GRPCExternalPortKey] = externalPort
	metadata[constants.RegionKey] = region
	return metadata
}

// Descriptor 根据 protoName 获取对应 protobuf 文件及其引用文件的 descriptor
//
//	@param protoName
//	@param protoDescs key为proto文件注册路径
//	@return error
func Descriptor(protoName string, protoDescs map[string]*descriptorpb.FileDescriptorProto) error {
	return docgenerator.ProtoDescriptors(protoName, protoDescs)
}

// FileDescriptor 根据文件名获取对应 protobuf 文件及其引用文件的 descriptor
//
//	@param protoName
//	@param protoDescs key为proto文件注册路径
//	@return error
func FileDescriptor(protoName string, protoDescs map[string]*descriptorpb.FileDescriptorProto) error {
	return docgenerator.ProtoFileDescriptors(protoName, protoDescs)
}

// StartWorker configures, starts and returns pitaya worker
func (app *App) StartWorker() {
	app.worker.Start()
}

// RegisterRPCJob registers rpc job to execute jobs with retries
func (app *App) RegisterRPCJob(rpcJob worker.RPCJob) error {
	err := app.worker.RegisterRPCJob(rpcJob)
	return err
}
