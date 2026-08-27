package pitaya

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/groups"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/router"
	"github.com/alkaid/pitaya/v2/serialize"
	"github.com/alkaid/pitaya/v2/service"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/worker"
	"github.com/go-redis/redis/v8"
)

type builderBuildState struct {
	mu      sync.Mutex
	started bool
}

type builderProvenance struct {
	server           *cluster.Server
	sessionPool      session.SessionPool
	redis            redis.Cmdable
	metricsReporters []metrics.Reporter
	serviceDiscovery cluster.ServiceDiscovery
	rpcClient        cluster.RPCClient
	rpcServer        cluster.RPCServer
	redisConfig      config.RedisConfig
	sessionCacheTTL  time.Duration
}

type resolvedDependencies struct {
	acceptors          []acceptor.Acceptor
	postBuildHooks     []func(app Pitaya)
	dependencyDieChan  chan bool
	config             config.PitayaConfig
	dieChan            chan bool
	packetDecoder      codec.PacketDecoder
	packetEncoder      codec.PacketEncoder
	messageEncoder     *message.MessagesEncoder
	serializer         serialize.Serializer
	router             *router.Router
	rpcClient          cluster.RPCClient
	rpcServer          cluster.RPCServer
	idempotencyStore   service.IdempotencyStore
	metricsReporters   []metrics.Reporter
	prometheusReporter *metrics.PrometheusReporter
	prometheusExporter *metrics.PrometheusExporter
	server             *cluster.Server
	serverMode         ServerMode
	serviceDiscovery   cluster.ServiceDiscovery
	groups             groups.GroupService
	sessionPool        session.SessionPool
	worker             *worker.Worker
	remoteHooks        *pipeline.RemoteHooks
	handlerHooks       *pipeline.HandlerHooks
	redis              redis.Cmdable
	conf               *config.Config
	provenance         *builderProvenance
}

func (builder *Builder) beginBuild() {
	builder.buildState.mu.Lock()
	defer builder.buildState.mu.Unlock()
	if builder.buildState.started {
		panic(errors.New("builder build already started"))
	}
	builder.buildState.started = true
}

func (builder *Builder) resolveDependencies() resolvedDependencies {
	return resolvedDependencies{
		acceptors:          cloneSlice(builder.acceptors),
		postBuildHooks:     cloneSlice(builder.postBuildHooks),
		dependencyDieChan:  builder.dependencyDieChan,
		config:             builder.Config,
		dieChan:            builder.DieChan,
		packetDecoder:      builder.PacketDecoder,
		packetEncoder:      builder.PacketEncoder,
		messageEncoder:     builder.MessageEncoder,
		serializer:         builder.Serializer,
		router:             builder.Router,
		rpcClient:          builder.RPCClient,
		rpcServer:          builder.RPCServer,
		idempotencyStore:   builder.IdempotencyStore,
		metricsReporters:   cloneSlice(builder.MetricsReporters),
		prometheusReporter: builder.prometheusReporter,
		prometheusExporter: builder.prometheusExporter,
		server:             builder.Server,
		serverMode:         builder.ServerMode,
		serviceDiscovery:   builder.ServiceDiscovery,
		groups:             builder.Groups,
		sessionPool:        builder.SessionPool,
		worker:             builder.Worker,
		remoteHooks:        builder.RemoteHooks,
		handlerHooks:       builder.HandlerHooks,
		redis:              builder.Redis,
		conf:               builder.conf,
		provenance:         builder.provenance,
	}
}

func captureBuilderProvenance(builder *Builder) *builderProvenance {
	redisConfig := builder.Config.Storage.Redis
	redisConfig.Addrs = cloneSlice(redisConfig.Addrs)
	return &builderProvenance{
		server:           builder.Server,
		sessionPool:      builder.SessionPool,
		redis:            builder.Redis,
		metricsReporters: cloneSlice(builder.MetricsReporters),
		serviceDiscovery: builder.ServiceDiscovery,
		rpcClient:        builder.RPCClient,
		rpcServer:        builder.RPCServer,
		redisConfig:      redisConfig,
		sessionCacheTTL:  builder.Config.Session.CacheTTL,
	}
}

func (resolved resolvedDependencies) validateGraph() error {
	provenance := resolved.provenance
	if provenance == nil {
		return nil
	}

	var errs []error
	if resolved.prometheusExporter != nil && !containsReporter(resolved.metricsReporters, resolved.prometheusReporter) {
		errs = append(errs, errors.New("Builder.MetricsReporters removed the reporter owned by the required Prometheus exporter; keep the default Prometheus reporter in Builder.MetricsReporters"))
	}
	if !isNilDependency(provenance.rpcServer) && sameDependencyIdentity(resolved.rpcServer, provenance.rpcServer) {
		if resolved.server != provenance.server {
			errs = append(errs, fmt.Errorf("Builder.RPCServer retains the default NATS server captured with the original Builder.Server; replace Builder.RPCServer with a caller-owned RPCServer built from the current Builder.Server"))
		}
		if !sameDependencyIdentity(resolved.sessionPool, provenance.sessionPool) {
			errs = append(errs, fmt.Errorf("Builder.RPCServer retains the default NATS server captured with the original Builder.SessionPool; replace Builder.RPCServer with a caller-owned RPCServer built from the current Builder.SessionPool"))
		}
		if !sameReporterGraph(resolved.metricsReporters, provenance.metricsReporters) {
			errs = append(errs, fmt.Errorf("Builder.RPCServer retains the default NATS server captured with the original Builder.MetricsReporters; replace Builder.RPCServer with a caller-owned RPCServer built from the current Builder.MetricsReporters"))
		}
	}
	if !isNilDependency(provenance.rpcClient) && sameDependencyIdentity(resolved.rpcClient, provenance.rpcClient) {
		if resolved.server != provenance.server {
			errs = append(errs, fmt.Errorf("Builder.RPCClient retains the default NATS client captured with the original Builder.Server; replace Builder.RPCClient with a caller-owned RPCClient built from the current Builder.Server"))
		}
		if !sameReporterGraph(resolved.metricsReporters, provenance.metricsReporters) {
			errs = append(errs, fmt.Errorf("Builder.RPCClient retains the default NATS client captured with the original Builder.MetricsReporters; replace Builder.RPCClient with a caller-owned RPCClient built from the current Builder.MetricsReporters"))
		}
	}
	if !isNilDependency(provenance.serviceDiscovery) && sameDependencyIdentity(resolved.serviceDiscovery, provenance.serviceDiscovery) && resolved.server != provenance.server {
		errs = append(errs, fmt.Errorf("Builder.ServiceDiscovery retains the default etcd discovery captured with the original Builder.Server; replace Builder.ServiceDiscovery with a caller-owned ServiceDiscovery built from the current Builder.Server"))
	}
	if sameDependencyIdentity(resolved.sessionPool, provenance.sessionPool) {
		if !sameDependencyIdentity(resolved.redis, provenance.redis) {
			errs = append(errs, fmt.Errorf("Builder.SessionPool retains the default pool captured with the original Builder.Redis; replace Builder.SessionPool with a caller-owned pool configured from the current Builder.Redis"))
		}
		if !reflect.DeepEqual(resolved.config.Storage.Redis, provenance.redisConfig) {
			errs = append(errs, fmt.Errorf("Builder.SessionPool retains the default pool built from the original Builder.Config.Storage.Redis; replace Builder.SessionPool after changing Redis construction config"))
		}
		if resolved.config.Session.CacheTTL != provenance.sessionCacheTTL {
			errs = append(errs, fmt.Errorf("Builder.SessionPool retains the default pool built from the original Builder.Config.Session.CacheTTL; replace Builder.SessionPool after changing session cache TTL"))
		}
	}
	return errors.Join(errs...)
}

func containsReporter(reporters []metrics.Reporter, target metrics.Reporter) bool {
	for _, reporter := range reporters {
		if sameDependencyIdentity(reporter, target) {
			return true
		}
	}
	return false
}

func sameReporterGraph(current, captured []metrics.Reporter) bool {
	if len(current) != len(captured) {
		return false
	}
	for index := range current {
		if !sameDependencyIdentity(current[index], captured[index]) {
			return false
		}
	}
	return true
}

func sameDependencyIdentity(left, right any) bool {
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	if !leftValue.IsValid() || !rightValue.IsValid() {
		return !leftValue.IsValid() && !rightValue.IsValid()
	}
	if leftValue.Type() != rightValue.Type() {
		return false
	}
	switch leftValue.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Ptr, reflect.Slice, reflect.UnsafePointer:
		return leftValue.Pointer() == rightValue.Pointer()
	default:
		if leftValue.Type().Comparable() {
			return leftValue.Interface() == rightValue.Interface()
		}
		return false
	}
}

func isNilDependency(value any) bool {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return true
	}
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func cloneSlice[T any](source []T) []T {
	if source == nil {
		return nil
	}
	cloned := make([]T, len(source))
	copy(cloned, source)
	return cloned
}
