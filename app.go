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
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"go.uber.org/zap"

	"go.opentelemetry.io/otel/trace"

	"google.golang.org/protobuf/types/descriptorpb"

	"time"

	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/go-redis/redis/v8"

	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/docgenerator"
	"github.com/alkaid/pitaya/v2/groups"
	"github.com/alkaid/pitaya/v2/interfaces"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/remote"
	"github.com/alkaid/pitaya/v2/router"
	"github.com/alkaid/pitaya/v2/serialize"
	"github.com/alkaid/pitaya/v2/service"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/timer"
	"github.com/alkaid/pitaya/v2/tracing"
	"github.com/alkaid/pitaya/v2/util"
	"github.com/alkaid/pitaya/v2/worker"
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
	// SetSDLeaderChangeListener 设置leader变化监听
	SetSDLeaderChangeListener(listener cluster.LeaderChangeListener)
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
	//  数据不存在时返回 [constants.ErrSessionNotFound]
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
	// NotifyAll 通知集群内所有服务，每种服务只有一个实例消费，会排除当前服务
	//  @param ctx
	//  @param routeStr 不能带有serverType,否则报错
	//  @param arg
	//  @param uid
	//  @return error
	NotifyAll(ctx context.Context, routeStr string, arg proto.Message, uid string) error
	// Fork rpc调用同一类服务的所有实例,非阻塞.一般来说仅适用于目标服务为stateful类型时
	//  会排除当前副本
	//  @param ctx
	//  @param routeStr
	//  @param arg
	//  @param uid 若不为空会携带session数据
	Fork(ctx context.Context, routeStr string, arg proto.Message, uid string) error
	// ForkRequest rpc调用同一类服务的所有实例,阻塞.一般来说仅适用于目标服务为stateful类型时
	//  会排除当前副本
	//  @param ctx
	//  @param routeStr
	//  @param arg
	//  @param uid
	//  @return []*protos.Response
	//  @return error
	ForkRequest(ctx context.Context, routeStr string, arg proto.Message, uid string) ([]*protos.Response, error)
	// PublishRequest 发布一个topic,会阻塞等待请求返回,订阅参数fork为false时排除本服务,为true时排除本副本
	//  @param ctx
	//  @param routeStr 只需两位 如 publish.event
	//  @param arg
	//  @param uid
	//  @return []*protos.Response 自行判断status以及反序列化data,可以使用protoAny,switch typeUrl来解析
	//  @return error
	PublishRequest(ctx context.Context, routeStr string, arg proto.Message, uid string) ([]*protos.Response, error)
	// Publish 发布一个topic,不会阻塞,订阅参数fork为false时排除本服务,为true时排除本副本
	//  @param ctx
	//  @param routeStr 只需两位 如 publish.event
	//  @param arg
	//  @param uid
	//  @return error
	Publish(ctx context.Context, routeStr string, arg proto.Message, uid string) error
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
	//  @reason 原因
	//  @return []string
	//  @return error
	SendKickToUsers(uids []string, frontendType string, callback map[string]string, reason session.CloseReason) ([]string, error)

	GroupCreate(ctx context.Context, groupName string) error
	GroupCreateWithTTL(ctx context.Context, groupName string, ttlTime time.Duration) error
	GroupMembers(ctx context.Context, groupName string) ([]string, error)
	GroupMembersPage(ctx context.Context, groupName string, options groups.MemberPageOptions) (groups.MemberPage, error)
	GroupBroadcast(ctx context.Context, frontendType, groupName, route string, v interface{}) error
	GroupBroadcastWithOptions(ctx context.Context, frontendType, groupName, route string, v interface{}, options groups.BroadcastOptions) (groups.BroadcastResult, error)
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
	// LazyRegister 延迟注册handle(终端api响应)
	//  @param c 注意系统不会回调该component的生命周期函数
	//  @param options
	LazyRegister(c component.Component, options ...component.Option) error
	// LazyRegisterRemote  延迟注册remote(远端rpc响应)
	//  @param c 注意系统不会回调该component的生命周期函数
	//  @param options
	LazyRegisterRemote(c component.Component, options ...component.Option) error

	// RegisterInterceptor 注册handle的拦截器,注册后原hande的消息响应将不再分发给handle
	//  @param serviceName
	//  @param interceptor
	RegisterInterceptor(serviceName string, interceptor *component.Interceptor) error
	// RegisterRemoteInterceptor 注册remote的拦截器,注册后原remote的消息响应将不再分发给remote
	//  @param serviceName
	//  @param interceptor
	RegisterRemoteInterceptor(serviceName string, interceptor *component.Interceptor) error
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
	//  @return error session不存在时返回 [constants.ErrSessionNotFound]
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
	acceptors           []acceptor.Acceptor
	config              config.PitayaConfig
	dieChan             chan bool
	dependencyDieChan   <-chan bool
	heartbeat           time.Duration
	router              *router.Router
	rpcClient           cluster.RPCClient
	rpcServer           cluster.RPCServer
	metricsReporters    []metrics.Reporter
	metricsReporterMgr  metrics.Reporter
	prometheusExporter  *metrics.PrometheusExporter
	running             atomic.Bool
	configurationLocked atomic.Bool
	serializer          serialize.Serializer
	server              *cluster.Server
	serverMode          ServerMode
	serviceDiscovery    cluster.ServiceDiscovery
	startAt             time.Time
	worker              *worker.Worker
	remoteService       *service.RemoteService
	handlerService      *service.HandlerService
	handlerComp         []regComp
	remoteComp          []regComp
	modulesMap          map[string]interfaces.Module
	modulesArr          []moduleWrapper
	sessionModulesArr   []sessionModuleWrapper
	groups              groups.GroupService
	sessionPool         session.SessionPool
	redis               redis.Cmdable
	conf                *config.Config
	onStarted           func()
	stopCh              chan struct{}
	stopOnce            sync.Once
	acceptCtx           context.Context
	cancelAccept        context.CancelFunc
	connectionCtx       context.Context
	cancelConnections   context.CancelFunc
	backgroundCtx       context.Context
	cancelBackground    context.CancelFunc
	acceptorWG          sync.WaitGroup
	dispatcherWG        sync.WaitGroup
	backgroundWG        sync.WaitGroup
	acceptorsStopped    bool
	dispatchersStopped  bool
	handlerWG           sync.WaitGroup
	stopAcceptorsOnce   sync.Once
	stopConnectionsOnce sync.Once
	stopBackgroundOnce  sync.Once
	beforeModulesOnce   sync.Once
	beforeModulesErr    error
	drainTasksOnce      sync.Once
	forceSessionsOnce   sync.Once
	connectionsMu       sync.Mutex
	connections         map[*trackedPlayerConn]struct{}
	connectionsStopped  bool
	startupJournal      *startupJournal
}

type trackedPlayerConn struct {
	acceptor.PlayerConn
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
	return newApp(
		serverMode,
		serializer,
		acceptors,
		dieChan,
		dieChan,
		router,
		server,
		rpcClient,
		rpcServer,
		worker,
		serviceDiscovery,
		remoteService,
		handlerService,
		groups,
		sessionPool,
		metricsReporters,
		nil,
		config,
	)
}

func newApp(
	serverMode ServerMode,
	serializer serialize.Serializer,
	acceptors []acceptor.Acceptor,
	dieChan chan bool,
	dependencyDieChan <-chan bool,
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
	prometheusExporter *metrics.PrometheusExporter,
	config config.PitayaConfig,
) *App {
	app := &App{
		server:             server,
		config:             config,
		rpcClient:          rpcClient,
		rpcServer:          rpcServer,
		worker:             worker,
		serviceDiscovery:   serviceDiscovery,
		remoteService:      remoteService,
		handlerService:     handlerService,
		groups:             groups,
		startAt:            time.Now(),
		dieChan:            dieChan,
		dependencyDieChan:  dependencyDieChan,
		acceptors:          acceptors,
		metricsReporters:   metricsReporters,
		prometheusExporter: prometheusExporter,
		serverMode:         serverMode,
		serializer:         serializer,
		router:             router,
		handlerComp:        make([]regComp, 0),
		remoteComp:         make([]regComp, 0),
		modulesMap:         make(map[string]interfaces.Module),
		modulesArr:         []moduleWrapper{},
		sessionModulesArr:  []sessionModuleWrapper{},
		sessionPool:        sessionPool,
		stopCh:             make(chan struct{}),
		connections:        make(map[*trackedPlayerConn]struct{}),
		acceptorsStopped:   true,
		dispatchersStopped: true,
	}
	if app.heartbeat == time.Duration(0) {
		app.heartbeat = config.Heartbeat.Interval
	}
	app.metricsReporterMgr = metrics.NewReporterMgr(metricsReporters)

	if app.remoteService != nil {
		app.initSysRemotes()
	}
	return app
}

// GetDieChan returns the legacy dependency-failure event channel. Callers may
// signal it but must not close it; use Shutdown to stop the App.
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

// SetSDLeaderChangeListener 设置leader变化监听
func (app *App) SetSDLeaderChangeListener(listener cluster.LeaderChangeListener) {
	app.serviceDiscovery.OnLeaderChange(listener)
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
	app.remoteService.GetRemoteHooks().BeforeHandler.PushBack(beforeFunc)
}
func (app *App) PushAfterRemoteHook(afterFunc pipeline.AfterHandlerTempl) {
	app.remoteService.GetRemoteHooks().AfterHandler.PushBack(afterFunc)
}

// IsRunning is true only after modules are initialized and all acceptors are
// ready. It becomes false before listeners are stopped during shutdown.
func (app *App) IsRunning() bool {
	return app.running.Load()
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
}

func (app *App) startBackgroundTasks() {
	app.backgroundCtx, app.cancelBackground = context.WithCancel(context.Background())
	period := app.config.Metrics.Period
	if len(app.metricsReporters) > 0 {
		app.backgroundWG.Add(1)
		go func() {
			defer app.backgroundWG.Done()
			metrics.ReportSysMetricsContext(app.backgroundCtx, app.metricsReporters, period)
		}()
	}

	if app.worker != nil && app.worker.Started() && len(app.metricsReporters) > 0 {
		app.backgroundWG.Add(1)
		go func() {
			defer app.backgroundWG.Done()
			worker.ReportContext(app.backgroundCtx, app.metricsReporters, period)
		}()
	}

	if app.onStarted != nil {
		app.backgroundWG.Add(1)
		go func() {
			defer app.backgroundWG.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					util.LogRecoveredPanic(logger.Zap, "onStarted callback panicked", recovered)
				}
			}()
			app.onStarted()
		}()
	}
}

func (app *App) OnStarted(fun func()) {
	app.onStarted = fun
}

// Start starts the app. It panics if a required runtime task cannot be admitted.
func (app *App) Start() {
	// Runtime configuration is immutable once startup begins. Readiness may turn
	// false during drain while handlers still read routes and the dictionary.
	app.configurationLocked.Store(true)
	app.startupJournal = newStartupJournal()

	if err := app.validateStartConfiguration(); err != nil {
		panic(fmt.Errorf("start app: %w", err))
	}

	if err := app.listen(); err != nil {
		panic(app.rollbackStartup(err))
	}
	app.startBackgroundTasks()

	defer func() {
		app.startupJournal.stopTicker()
		app.running.Store(false)
	}()

	sg := make(chan os.Signal, 1)
	signal.Notify(sg, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGTERM)
	defer signal.Stop(sg)

	maxSessionCount := func() int64 {
		count := app.sessionPool.GetSessionCount()
		mc := app.maxModuleSessionCount()
		if mc > count {
			count = mc
		}
		return count
	}

	// Stop accepting traffic before draining sessions or tearing down modules.
	var shutdownSignal os.Signal
	select {
	case <-app.dependencyDieChan:
		logger.Zap.Warn("the app will shutdown in a few seconds")
		app.signalStop()
		if app.dependencyDieChan != app.dieChan {
			signalLegacyDie(app.dieChan)
		}
	case <-app.dieChan:
		logger.Zap.Warn("the app will shutdown in a few seconds")
		app.signalStop()
	case <-app.stopCh:
		logger.Zap.Warn("the app will shutdown in a few seconds")
	case shutdownSignal = <-sg:
		logger.Sugar.Warn("got signal: ", shutdownSignal, ", shutting down...")
		app.signalStop()
	}

	app.running.Store(false)
	acceptorErr := app.stopAcceptors()

	if app.config.Session.Drain.Enabled && shutdownSignal == syscall.SIGTERM {
		logger.Sugar.Info("Session drain is enabled, draining all sessions before shutting down")
		timeoutTimer := time.NewTimer(app.config.Session.Drain.Timeout)
		defer timeoutTimer.Stop()
		app.startModuleSessionDraining()
	loop:
		for {
			if maxSessionCount() == 0 {
				logger.Sugar.Info("All sessions drained")
				break loop
			}
			select {
			case nextSignal := <-sg:
				logger.Sugar.Warn("got signal: ", nextSignal)
				if nextSignal == syscall.SIGINT {
					logger.Log.Warnf("Bypassing session draing due to SIGINT. %d sessions will be immediately terminated", maxSessionCount())
				}
				break loop
			case <-timeoutTimer.C:
				logger.Sugar.Warnf("Session drain has reached maximum timeout. %d sessions will be immediately terminated", maxSessionCount())
				break loop
			case <-time.After(app.config.Session.Drain.Period):
				logger.Sugar.Infof("Waiting for all sessions to finish: %d sessions remaining...", maxSessionCount())
			}
		}
	}

	logger.Zap.Warn("server is stopping...")

	deadline := time.Now().Add(runtimeShutdownTimeout)
	quiesceErr := app.quiesceRuntimeBefore(deadline)
	if shutdownErr := errors.Join(acceptorErr, quiesceErr); shutdownErr != nil {
		logger.Zap.Error("skipping session, module, and component teardown because runtime shutdown did not quiesce")
		panic(shutdownErr)
	}
	app.forceCloseSessions()
	if teardownErr := errors.Join(app.shutdownModules(), app.shutdownComponents()); teardownErr != nil {
		panic(fmt.Errorf("shutdown app: %w", teardownErr))
	}
}

func (app *App) validateStartConfiguration() error {
	if app.server == nil {
		return fmt.Errorf("server is not configured")
	}
	if !app.server.Frontend && len(app.acceptors) > 0 {
		return fmt.Errorf("acceptors are not allowed on backend servers")
	}
	if app.server.Frontend && len(app.acceptors) == 0 {
		return fmt.Errorf("frontend servers should have at least one configured acceptor")
	}
	return nil
}

func (app *App) rollbackStartup(startErr error) error {
	app.running.Store(false)
	journal := app.ensureStartupJournal()
	journal.stopTicker()

	var runtimeErr error
	if journal.hasRuntimeStarted() {
		deadline := time.Now().Add(runtimeShutdownTimeout)
		acceptorErr := app.stopAcceptorsWithin(minDuration(dispatcherShutdownTimeout, time.Until(deadline)))
		quiesceErr := app.quiesceRuntimeBefore(deadline)
		runtimeErr = errors.Join(acceptorErr, quiesceErr)
		if runtimeErr != nil {
			logger.Zap.Error("skipping session, module, and component teardown because runtime shutdown did not quiesce")
			return fmt.Errorf("start app: %w", errors.Join(startErr, runtimeErr))
		}
	}

	if app.sessionPool != nil {
		app.forceCloseSessions()
	}
	moduleErr := app.shutdownModules()
	componentErr := journal.rollbackComponents()
	return fmt.Errorf("start app: %w", errors.Join(startErr, moduleErr, componentErr))
}

func (app *App) listen() error {
	preparedModules, err := app.prepareRuntimeModules()
	if err != nil {
		return fmt.Errorf("prepare runtime modules: %w", err)
	}
	if err := app.startupComponents(); err != nil {
		return err
	}
	// create global ticker instance, timer precision could be customized
	// by SetTimerPrecision
	timer.GlobalTicker = time.NewTicker(timer.Precision)
	app.startupJournal.setTicker(timer.GlobalTicker)

	if err := app.commitRuntimeModules(preparedModules); err != nil {
		return fmt.Errorf("commit runtime modules: %w", err)
	}
	if err := app.startModules(); err != nil {
		return err
	}

	logger.Sugar.Infof("starting server %s:%s", app.server.Type, app.server.ID)
	app.startupJournal.markRuntimeStarted()
	if err := app.startRuntimeTasks(); err != nil {
		return err
	}

	app.running.Store(true)
	logger.Zap.Info("all modules started!")
	return nil
}

const (
	acceptorStartupTimeout    = 10 * time.Second
	dispatcherShutdownTimeout = 5 * time.Second
	runtimeShutdownTimeout    = 30 * time.Second
)

// ErrDispatcherShutdownTimeout means a synchronous timer callback prevented a
// safe module/component teardown. Start panics with this error so supervisors
// cannot mistake the fail-stop path for a completed graceful shutdown.
var ErrDispatcherShutdownTimeout = errors.New("pitaya: handler dispatcher shutdown timed out")

// ErrAcceptorShutdownTimeout means an Acceptor ignored both context cancellation
// and Stop. App shutdown enters fail-stop mode and does not tear down dependencies.
var ErrAcceptorShutdownTimeout = errors.New("pitaya: acceptor shutdown timed out")

// ErrRuntimeShutdownTimeout means App-owned work did not quiesce before the
// process-level shutdown budget expired. Callers must terminate the process.
var ErrRuntimeShutdownTimeout = errors.New("pitaya: runtime shutdown timed out")

func (app *App) startRuntimeTasks() error {
	app.acceptCtx, app.cancelAccept = context.WithCancel(context.Background())
	app.connectionCtx, app.cancelConnections = context.WithCancel(context.Background())
	app.dispatcherWG.Add(1)
	if err := co.Submit(func() {
		defer app.dispatcherWG.Done()
		app.handlerService.DispatchContext(app.acceptCtx, 0)
	}); err != nil {
		app.dispatcherWG.Done()
		return fmt.Errorf("start handler dispatcher: %w", err)
	}

	type startedAcceptor struct {
		acceptor acceptor.Acceptor
		ready    <-chan error
	}
	started := make([]startedAcceptor, 0, len(app.acceptors))
	for _, acc := range app.acceptors {
		a := acc
		if app.config.Acceptor.ProxyProtocol {
			logger.Zap.Info("Enabling PROXY protocol for inbound connections")
			a.EnableProxyProtocol()
		} else {
			logger.Zap.Debug("PROXY protocol is disabled for inbound connections")
		}

		ready := a.Ready()
		app.acceptorWG.Add(1)
		if err := co.Submit(func() {
			defer app.acceptorWG.Done()
			err := a.Serve(app.acceptCtx)
			if app.acceptCtx.Err() != nil {
				return
			}
			if err != nil {
				logger.Zap.Error("acceptor stopped unexpectedly", zap.Error(err))
			} else {
				logger.Zap.Error("acceptor stopped unexpectedly without an error")
			}
			app.signalStop()
		}); err != nil {
			app.acceptorWG.Done()
			return fmt.Errorf("start acceptor: %w", err)
		}
		started = append(started, startedAcceptor{acceptor: a, ready: ready})
	}

	for _, item := range started {
		a := item.acceptor
		logger.Sugar.Infof("Waiting for Acceptor %s to start on addr %s", reflect.TypeOf(a), a.GetConfiguredAddress())

		startupTimer := time.NewTimer(acceptorStartupTimeout)
		var readyErr error
		var readyOK bool
		select {
		case readyErr, readyOK = <-item.ready:
			startupTimer.Stop()
		case <-startupTimer.C:
			return fmt.Errorf("start acceptor %T: readiness timed out after %s", a, acceptorStartupTimeout)
		case <-app.stopCh:
			startupTimer.Stop()
			// An acceptor publishes readiness before its Serve call can report an
			// unexpected exit. Prefer that already-published result so Start can
			// enter its normal shutdown path instead of turning the exit into a
			// nondeterministic startup panic.
			select {
			case readyErr, readyOK = <-item.ready:
			default:
				return fmt.Errorf("start acceptor %T: app stopped before readiness", a)
			}
		}
		if !readyOK {
			return fmt.Errorf("start acceptor %T: readiness channel closed without a result", a)
		}
		if readyErr != nil {
			return fmt.Errorf("start acceptor %T: %w", a, readyErr)
		}

		logger.Sugar.Infof("Acceptor %s on addr %s is now accepting connections", reflect.TypeOf(a), a.GetAddr())
	}

	for _, item := range started {
		a := item.acceptor
		app.acceptorWG.Add(1)
		if err := co.Submit(func() {
			defer app.acceptorWG.Done()
			app.readAcceptorConnections(a)
		}); err != nil {
			app.acceptorWG.Done()
			return fmt.Errorf("start acceptor connection reader: %w", err)
		}
	}
	return nil
}

func (app *App) readAcceptorConnections(a acceptor.Acceptor) {
	for {
		select {
		case conn, ok := <-a.GetConnChan():
			if !ok {
				return
			}
			tracked := &trackedPlayerConn{PlayerConn: conn}
			if !app.trackConnection(tracked) {
				_ = tracked.Close()
				continue
			}
			if err := co.Submit(func() {
				defer app.handlerWG.Done()
				defer app.untrackConnection(tracked)
				app.handlerService.HandleContext(app.connectionCtx, tracked)
			}); err != nil {
				app.handlerWG.Done()
				app.untrackConnection(tracked)
				_ = tracked.Close()
				logger.Zap.Error("start connection handler", zap.Error(err))
			}
		case <-app.acceptCtx.Done():
			return
		}
	}
}

func (app *App) trackConnection(conn *trackedPlayerConn) bool {
	app.connectionsMu.Lock()
	defer app.connectionsMu.Unlock()
	if app.connectionsStopped {
		return false
	}
	// Publish the connection and reserve its handler under the same admission
	// lock used by stopConnectionsAndWait. Once shutdown closes admission, no
	// WaitGroup.Add can race with its Wait.
	app.handlerWG.Add(1)
	app.connections[conn] = struct{}{}
	return true
}

func (app *App) untrackConnection(conn *trackedPlayerConn) {
	app.connectionsMu.Lock()
	delete(app.connections, conn)
	app.connectionsMu.Unlock()
}

func (app *App) signalStop() {
	app.stopOnce.Do(func() {
		close(app.stopCh)
	})
}

func (app *App) stopAcceptors() error {
	return app.stopAcceptorsWithin(dispatcherShutdownTimeout)
}

func (app *App) stopAcceptorsWithin(timeout time.Duration) error {
	app.stopAcceptorsOnce.Do(func() {
		deadline := time.Now().Add(timeout)
		if app.cancelAccept != nil {
			app.cancelAccept()
		}
		for _, a := range app.acceptors {
			// A third-party Stop implementation may block. Track it with Serve and
			// connection readers so the shared shutdown deadline still applies.
			app.acceptorWG.Add(1)
			go func() {
				defer app.acceptorWG.Done()
				a.Stop()
			}()
		}
		// Start both waits before either can consume the shared deadline. This
		// preserves accurate attribution when only one group is blocked.
		acceptorsDone := waitGroupDone(&app.acceptorWG)
		dispatchersDone := waitGroupDone(&app.dispatcherWG)
		app.acceptorsStopped = waitDoneBefore(acceptorsDone, deadline)
		app.dispatchersStopped = waitDoneBefore(dispatchersDone, deadline)
		if !app.acceptorsStopped {
			logger.Sugar.Errorf("acceptor goroutines did not stop within %s; entering fail-stop shutdown", timeout)
		}
		if !app.dispatchersStopped {
			logger.Sugar.Errorf("handler dispatcher did not stop within %s; entering fail-stop shutdown with a blocked timer callback", timeout)
		}
	})
	var shutdownErrors []error
	if !app.acceptorsStopped {
		shutdownErrors = append(shutdownErrors, fmt.Errorf("%w after %s", ErrAcceptorShutdownTimeout, timeout))
	}
	if !app.dispatchersStopped {
		shutdownErrors = append(shutdownErrors, fmt.Errorf("%w after %s", ErrDispatcherShutdownTimeout, timeout))
	}
	return errors.Join(shutdownErrors...)
}

func (app *App) stopConnectionsAndWait() {
	app.stopConnectionsOnce.Do(func() {
		app.connectionsMu.Lock()
		app.connectionsStopped = true
		connections := make([]*trackedPlayerConn, 0, len(app.connections))
		for conn := range app.connections {
			connections = append(connections, conn)
		}
		app.connectionsMu.Unlock()
		if app.cancelConnections != nil {
			app.cancelConnections()
		}
		for _, conn := range connections {
			_ = conn.Close()
		}
		app.handlerWG.Wait()
	})
}

func (app *App) cancelBackgroundTasks() {
	app.stopBackgroundOnce.Do(func() {
		if app.cancelBackground != nil {
			app.cancelBackground()
		}
	})
}

func (app *App) drainHandlerTasks() {
	app.drainTasksOnce.Do(func() {
		// Module BeforeShutdown hooks have already stopped RPC, service-discovery,
		// and other framework ingress. Closing admission now makes the final session
		// sweep safe from framework-owned task creation.
		app.handlerService.DrainTasks()
	})
}

func (app *App) forceCloseSessions() {
	app.forceSessionsOnce.Do(func() {
		app.sessionPool.ForceCloseAll()
	})
}

func (app *App) quiesceRuntimeBefore(deadline time.Time) error {
	app.cancelBackgroundTasks()
	return runShutdownPhaseBefore(deadline, "runtime quiescence", func() {
		app.stopConnectionsAndWait()
		app.backgroundWG.Wait()
		if app.worker != nil {
			app.worker.Stop()
		}
		_ = app.beforeShutdownModules()
		app.drainHandlerTasks()
	})
}

type shutdownPhaseResult struct {
	panicValue any
	panicked   bool
}

func runShutdownPhaseBefore(deadline time.Time, phase string, run func()) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("%w before %s", ErrRuntimeShutdownTimeout, phase)
	}
	budget := remaining
	result := make(chan shutdownPhaseResult, 1)
	go func() {
		phaseResult := shutdownPhaseResult{}
		defer func() {
			if recovered := recover(); recovered != nil {
				phaseResult.panicValue = recovered
				phaseResult.panicked = true
			}
			result <- phaseResult
		}()
		run()
	}()
	timeout := time.NewTimer(remaining)
	defer timeout.Stop()
	select {
	case phaseResult := <-result:
		if phaseResult.panicked {
			panic(phaseResult.panicValue)
		}
		return nil
	case <-timeout.C:
		return fmt.Errorf("%w while waiting for %s after %s", ErrRuntimeShutdownTimeout, phase, budget)
	}
}

func waitGroupWithin(wg *sync.WaitGroup, timeout time.Duration) bool {
	return waitGroupBefore(wg, time.Now().Add(timeout))
}

func waitGroupBefore(wg *sync.WaitGroup, deadline time.Time) bool {
	return waitDoneBefore(waitGroupDone(wg), deadline)
}

func waitGroupDone(wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

func waitDoneBefore(done <-chan struct{}, deadline time.Time) bool {
	select {
	case <-done:
		return true
	default:
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// SetDictionary sets routes map
func (app *App) SetDictionary(dict map[string]uint16) error {
	if app.configurationLocked.Load() {
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
		if app.configurationLocked.Load() {
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
	app.signalStop()
	// Preserve the legacy event for callers observing GetDieChan without using
	// it as the App's cancellation primitive.
	signalLegacyDie(app.dieChan)
}

func signalLegacyDie(dieChan chan bool) {
	if dieChan == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	select {
	case dieChan <- true:
	default:
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
// 'pitaya.metrics.additionalLabels' config
func AddMetricTagsToPropagateCtx(
	ctx context.Context,
	tags map[string]string,
) context.Context {
	snapshot := make(map[string]string, len(tags))
	for key, value := range tags {
		snapshot[key] = value
	}
	return pcontext.AddToPropagateCtx(ctx, constants.MetricTagsKey, snapshot)
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

// StartWorker starts the pitaya worker and panics if it cannot be admitted.
func (app *App) StartWorker() {
	if err := app.worker.StartWithError(); err != nil {
		panic(err)
	}
}

// RegisterRPCJob registers rpc job to execute jobs with retries
func (app *App) RegisterRPCJob(rpcJob worker.RPCJob) error {
	err := app.worker.RegisterRPCJob(rpcJob)
	return err
}
