package pitaya

import (
	"errors"
	"fmt"

	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/agent"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/config"
	confPkg "github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/defaultpipelines"
	"github.com/alkaid/pitaya/v2/groups"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/router"
	"github.com/alkaid/pitaya/v2/serialize"
	"github.com/alkaid/pitaya/v2/service"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/util"
	netutil "github.com/alkaid/pitaya/v2/util/net"
	"github.com/alkaid/pitaya/v2/worker"
	"github.com/go-redis/redis/v8"
	"go.uber.org/zap"
)

// Builder holds dependency instances for a pitaya App
type Builder struct {
	acceptors          []acceptor.Acceptor
	postBuildHooks     []func(app Pitaya)
	dependencyDieChan  chan bool
	Config             config.PitayaConfig
	DieChan            chan bool
	PacketDecoder      codec.PacketDecoder
	PacketEncoder      codec.PacketEncoder
	MessageEncoder     *message.MessagesEncoder
	Serializer         serialize.Serializer
	Router             *router.Router
	RPCClient          cluster.RPCClient
	RPCServer          cluster.RPCServer
	IdempotencyStore   service.IdempotencyStore
	MetricsReporters   []metrics.Reporter
	Server             *cluster.Server
	ServerMode         ServerMode
	ServiceDiscovery   cluster.ServiceDiscovery
	Groups             groups.GroupService
	SessionPool        session.SessionPool
	Worker             *worker.Worker
	RemoteHooks        *pipeline.RemoteHooks
	HandlerHooks       *pipeline.HandlerHooks
	Redis              redis.Cmdable
	conf               *config.Config
	prometheusReporter *metrics.PrometheusReporter
	prometheusExporter *metrics.PrometheusExporter
	buildState         builderBuildState
	provenance         *builderProvenance
}

// PitayaBuilder is the legacy builder interface.
//
// Deprecated: use *Builder and BuildApp to retain the concrete App type.
type PitayaBuilder interface {
	// AddPostBuildHook adds a post-build hook to the builder, a function receiving a Pitaya instance as parameter.
	AddPostBuildHook(hook func(app Pitaya))
	Build() Pitaya
}

// NewBuilderWithConfigs return a builder instance with default dependency instances for a pitaya App
// with configs defined by a config file (config.Config) and default paths (see documentation).
func NewBuilderWithConfigs(
	isFrontend bool,
	serverType string,
	serverMode ServerMode,
	serverMetadata map[string]string,
	conf *config.Config,
) *Builder {
	pitayaConfig := config.NewPitayaConfig(conf)
	b := NewBuilder(
		isFrontend,
		serverType,
		serverMode,
		serverMetadata,
		*pitayaConfig,
	)
	b.conf = conf
	return b
}

// NewDefaultBuilder return a builder instance with default dependency instances for a pitaya App,
// with default configs
func NewDefaultBuilder(isFrontend bool, serverType string, serverMode ServerMode, serverMetadata map[string]string, pitayaConfig config.PitayaConfig) *Builder {
	return NewBuilder(
		isFrontend,
		serverType,
		serverMode,
		serverMetadata,
		pitayaConfig,
	)
}

// NewBuilder return a builder instance with default dependency instances for a pitaya App,
// with configs explicitly defined
func NewBuilder(isFrontend bool,
	serverType string,
	serverMode ServerMode,
	serverMetadata map[string]string,
	config config.PitayaConfig,
) *Builder {
	validateBuilderConfig(config, serverMode)
	server := cluster.NewServer(util.NanoID(12), serverType, isFrontend, serverMetadata)
	// The lifecycle signal is an event queue, not a broadcast channel. Keeping
	// one pending event makes dependency failures observable even when they race
	// with App.Start reaching its shutdown select.
	dieChan := make(chan bool, 1)
	dependencyDieChan := make(chan bool, 1)

	metricsReporters := []metrics.Reporter{}
	var prometheusReporter *metrics.PrometheusReporter
	var prometheusExporter *metrics.PrometheusExporter
	if config.Metrics.Prometheus.Enabled {
		var err error
		prometheusReporter, prometheusExporter, err = newManagedPrometheus(
			serverType,
			config.Metrics,
			config.Metrics.Custom,
			dependencyDieChan,
		)
		if err != nil {
			panic(fmt.Errorf("configure required Prometheus exporter: %w", err))
		}
		metricsReporters = append(metricsReporters, prometheusReporter)
	}

	if config.Metrics.Statsd.Enabled {
		metricsReporters = addDefaultStatsd(config.Metrics, metricsReporters, serverType)
	}

	handlerHooks := pipeline.NewHandlerHooks()
	if config.DefaultPipelines.StructValidation.Enabled {
		configureDefaultPipelines(handlerHooks)
	}

	// session 后端redis落地实例
	var redisClient redis.Cmdable
	if config.Storage.Redis.Type == "cluster" {
		redisOptions, err := confPkg.NewRedisClusterOptions(&config.Storage.Redis)
		if err != nil {
			panic(fmt.Errorf("create session Redis options: %w", err))
		}
		redisClient = redis.NewClusterClient(redisOptions)
	} else {
		redisOptions, err := confPkg.NewRedisNodeOptions(&config.Storage.Redis)
		if err != nil {
			panic(fmt.Errorf("create session Redis options: %w", err))
		}
		redisClient = redis.NewClient(redisOptions)
	}
	sessionCache := session.NewRedisCache(redisClient, config.Session.CacheTTL)
	sessionPool := session.NewSessionPool()
	sessionPool.SetClusterCache(sessionCache)

	var serviceDiscovery cluster.ServiceDiscovery
	var rpcServer cluster.RPCServer
	var rpcClient cluster.RPCClient
	if serverMode == Cluster {
		var err error
		serviceDiscovery, err = cluster.NewEtcdServiceDiscovery(config.Cluster.SD.Etcd, server, dependencyDieChan)
		if err != nil {
			logger.Zap.Fatal("error creating default cluster service discovery component", zap.Error(err))
		}

		rpcServer, err = cluster.NewNatsRPCServer(config.Cluster.RPC.Server.Nats, server, metricsReporters, dependencyDieChan, sessionPool)
		if err != nil {
			logger.Zap.Fatal("error setting default cluster rpc server component", zap.Error(err))
		}

		rpcClient, err = cluster.NewNatsRPCClient(config.Cluster.RPC.Client.Nats, server, metricsReporters, dependencyDieChan)
		if err != nil {
			logger.Zap.Fatal("error setting default cluster rpc client component", zap.Error(err))
		}
	}

	worker, err := worker.NewWorker(config.Worker, config.Worker.Retry)
	if err != nil {
		logger.Zap.Fatal("error creating default worker", zap.Error(err))
	}

	gsi := groups.NewMemoryGroupService(config.Groups.Memory)
	if err != nil {
		panic(err)
	}

	serializer, err := serialize.NewSerializer(serialize.Type(config.SerializerType))
	if err != nil {
		logger.Log.Fatalf("error creating serializer: %s", err.Error())
	}

	builder := &Builder{
		acceptors:          []acceptor.Acceptor{},
		postBuildHooks:     make([]func(app Pitaya), 0),
		dependencyDieChan:  dependencyDieChan,
		Config:             config,
		DieChan:            dieChan,
		PacketDecoder:      codec.NewPomeloPacketDecoder(),
		PacketEncoder:      codec.NewPomeloPacketEncoder(),
		MessageEncoder:     message.NewMessagesEncoder(config.Handler.Messages.Compression),
		Serializer:         serializer,
		Router:             router.New(),
		RPCClient:          rpcClient,
		RPCServer:          rpcServer,
		IdempotencyStore:   nil,
		MetricsReporters:   metricsReporters,
		Server:             server,
		ServerMode:         serverMode,
		Groups:             gsi,
		RemoteHooks:        pipeline.NewRemoteHooks(),
		HandlerHooks:       handlerHooks,
		ServiceDiscovery:   serviceDiscovery,
		SessionPool:        sessionPool,
		Worker:             worker,
		Redis:              redisClient,
		prometheusReporter: prometheusReporter,
		prometheusExporter: prometheusExporter,
	}
	builder.provenance = captureBuilderProvenance(builder)
	return builder
}

// AddAcceptor adds a new acceptor to app
func (builder *Builder) AddAcceptor(ac acceptor.Acceptor) {
	if !builder.Server.Frontend {
		logger.Zap.Error("tried to add an acceptor to a backend server, skipping")
		return
	}
	builder.acceptors = append(builder.acceptors, ac)
}

// AddPostBuildHook adds a post-build hook to the builder, a function receiving a Pitaya instance as parameter.
func (builder *Builder) AddPostBuildHook(hook func(app Pitaya)) {
	builder.postBuildHooks = append(builder.postBuildHooks, hook)
}

// Build returns a valid App through the legacy aggregate interface.
func (builder *Builder) Build() Pitaya {
	return builder.BuildApp()
}

// BuildApp returns a concrete App instance.
func (builder *Builder) BuildApp() *App {
	builder.beginBuild()
	resolved := builder.resolveDependencies()
	if err := resolved.validateGraph(); err != nil {
		panic(fmt.Errorf("invalid builder dependency graph: %w", err))
	}
	validateBuilderConfig(resolved.config, resolved.serverMode)
	for index, reporter := range resolved.metricsReporters {
		if reporter == nil {
			panic(fmt.Errorf("nil metrics reporter at index %d", index))
		}
	}
	if resolved.serverMode == Standalone {
		if resolved.serviceDiscovery != nil || resolved.rpcClient != nil || resolved.rpcServer != nil {
			panic("Standalone mode can't have RPC or service discovery instances")
		}
	} else if resolved.serverMode == Cluster {
		if resolved.serviceDiscovery == nil || resolved.rpcClient == nil || resolved.rpcServer == nil {
			panic("Cluster mode must have RPC and service discovery instances")
		}
		if err := resolved.validateTransportCapabilities(); err != nil {
			panic(fmt.Errorf("invalid RPC transport capabilities: %w", err))
		}
	}
	trustedProxies, err := netutil.ParseTrustedProxies(resolved.config.Acceptor.TrustedProxyCIDRs)
	if err != nil {
		panic(fmt.Errorf("invalid pitaya.acceptor.trustedproxycidrs: %w", err))
	}
	// 根据配置初始化zap
	logger.InitLogMgr(resolved.config.Log.Development, resolved.config.Log.Level)
	warnIgnoredHandlerConfiguration(resolved.config)
	if resolved.conf != nil {
		resolved.conf.AddLoader(logger.Manager.ReloadFactory("pitaya.log", func() {
			logger.ResetGlobalVar()
		}))
	}
	handlerPool := service.NewHandlerPool()
	var remoteService *service.RemoteService
	if resolved.serverMode == Cluster {
		resolved.router.SetServiceDiscovery(resolved.serviceDiscovery)
		idempotencyStore := resolved.resolveRPCIdempotencyStore()

		remoteService = service.NewRemoteServiceWithOptions(
			resolved.rpcClient,
			resolved.rpcServer,
			resolved.serviceDiscovery,
			resolved.packetEncoder,
			resolved.serializer,
			resolved.router,
			resolved.messageEncoder,
			resolved.server,
			resolved.sessionPool,
			resolved.remoteHooks,
			resolved.handlerHooks,
			handlerPool,
			service.WithFanoutConcurrency(resolved.config.Cluster.RPC.FanoutConcurrency),
			service.WithIdempotencyStore(idempotencyStore),
		)

		resolved.rpcServer.SetPitayaServer(remoteService)
	}

	agentFactory := agent.NewAgentFactoryWithTrustedProxies(resolved.dependencyDieChan,
		resolved.packetDecoder,
		resolved.packetEncoder,
		resolved.serializer,
		resolved.config.Heartbeat.Interval,
		resolved.messageEncoder,
		resolved.config.Buffer.Agent.Messages,
		resolved.sessionPool,
		resolved.metricsReporters,
		resolved.server.ID,
		trustedProxies,
	)

	handlerService := service.NewHandlerService(
		resolved.packetDecoder,
		resolved.serializer,
		0,
		0,
		resolved.server,
		remoteService,
		agentFactory,
		resolved.metricsReporters,
		resolved.handlerHooks,
		handlerPool,
	)

	app := newApp(
		resolved.serverMode,
		resolved.serializer,
		resolved.acceptors,
		resolved.dieChan,
		resolved.dependencyDieChan,
		resolved.router,
		resolved.server,
		resolved.rpcClient,
		resolved.rpcServer,
		resolved.worker,
		resolved.serviceDiscovery,
		remoteService,
		handlerService,
		resolved.groups,
		resolved.sessionPool,
		resolved.metricsReporters,
		resolved.prometheusExporter,
		resolved.config,
	)

	for _, postBuildHook := range resolved.postBuildHooks {
		postBuildHook(app)
	}

	app.conf = resolved.conf
	return app
}

func warnIgnoredHandlerConfiguration(pitayaConfig config.PitayaConfig) {
	defaults := config.NewDefaultPitayaConfig()
	if pitayaConfig.Buffer.Handler.LocalProcess == defaults.Buffer.Handler.LocalProcess &&
		pitayaConfig.Buffer.Handler.RemoteProcess == defaults.Buffer.Handler.RemoteProcess &&
		pitayaConfig.Concurrency.Handler.Dispatch == defaults.Concurrency.Handler.Dispatch {
		return
	}
	logger.Zap.Warn(
		"deprecated handler buffer and dispatch configuration is ignored",
		zap.Int("pitaya.buffer.handler.localprocess", pitayaConfig.Buffer.Handler.LocalProcess),
		zap.Int("pitaya.buffer.handler.remoteprocess", pitayaConfig.Buffer.Handler.RemoteProcess),
		zap.Int("pitaya.concurrency.handler.dispatch", pitayaConfig.Concurrency.Handler.Dispatch),
		zap.Strings("actual_capacity_owners", []string{
			"pitaya.buffer.agent.messages",
			"pitaya.gopools.<pool>.taskbuffer",
			"pitaya.cluster.rpc.server.nats.buffer.messages",
			"pitaya.cluster.rpc.server.nats.buffer.push",
		}),
	)
}

func validateBuilderConfig(pitayaConfig config.PitayaConfig, serverMode ServerMode) {
	var err error
	switch serverMode {
	case Standalone:
		err = pitayaConfig.Validate()
	case Cluster:
		err = errors.Join(pitayaConfig.Validate(), pitayaConfig.ValidateCluster())
	default:
		err = fmt.Errorf("unknown server mode %d", serverMode)
	}
	if err != nil {
		panic(fmt.Errorf("invalid pitaya config: %w", err))
	}
}

func (resolved resolvedDependencies) resolveRPCIdempotencyStore() service.IdempotencyStore {
	if resolved.idempotencyStore != nil {
		return resolved.idempotencyStore
	}
	idempotencyConfig := resolved.config.Cluster.RPC.Idempotency
	if !idempotencyConfig.Enabled {
		return nil
	}
	store, err := service.NewRedisIdempotencyStore(
		resolved.redis,
		service.RedisIdempotencyStoreOptions{
			KeyPrefix:         idempotencyConfig.KeyPrefix,
			ResultTTL:         idempotencyConfig.ResultTTL,
			LeaseTTL:          idempotencyConfig.LeaseTTL,
			HeartbeatInterval: idempotencyConfig.HeartbeatInterval,
			PollInterval:      idempotencyConfig.PollInterval,
			OperationTimeout:  idempotencyConfig.OperationTimeout,
		},
	)
	if err != nil {
		panic(fmt.Errorf("create Redis RPC idempotency store: %w", err))
	}
	return store
}

// NewDefaultApp returns a default pitaya app instance
func NewDefaultApp(isFrontend bool, serverType string, serverMode ServerMode, serverMetadata map[string]string, config config.PitayaConfig) Pitaya {
	builder := NewDefaultBuilder(isFrontend, serverType, serverMode, serverMetadata, config)
	return builder.Build()
}

func configureDefaultPipelines(handlerHooks *pipeline.HandlerHooks) {
	handlerHooks.BeforeHandler.PushBack(defaultpipelines.StructValidatorInstance.Validate)
}

func addDefaultStatsd(config config.MetricsConfig, reporters []metrics.Reporter, serverType string) []metrics.Reporter {
	statsd, err := CreateStatsdReporter(serverType, config)
	if err != nil {
		logger.Zap.Error("failed to start statsd metrics reporter, skipping", zap.Error(err))
	} else {
		reporters = append(reporters, statsd)
	}
	return reporters
}
