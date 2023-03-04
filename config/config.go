package config

import (
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/topfreegames/pitaya/v2/metrics/models"
)

type PitayaAll struct {
	PitayaConfig `mapstructure:",squash"`
	Cluster      struct {
		Info InfoRetrieverConfig
		Rpc  struct {
			Client struct {
				Grpc GRPCClientConfig
				Nats NatsRPCClientConfig
			}
			Server struct {
				Grpc GRPCServerConfig
				Nats NatsRPCServerConfig
			}
		}
		Sd struct {
			Etcd EtcdServiceDiscoveryConfig
		}
	}
	DefaultPipelines struct {
		StructValidation struct {
			Enabled bool
		}
	}
	Groups struct {
		Etcd   EtcdGroupServiceConfig
		Memory MemoryGroupConfig
	}
	Metrics struct {
		Prometheus struct {
			PrometheusConfig `mapstructure:",squash"`
			Enabled          bool
		}
		Statsd struct {
			StatsdConfig `mapstructure:",squash"`
			Enabled      bool
		}
	}
	Modules struct {
		BindingStorage struct {
			Etcd ETCDBindingConfig
		}
	}
	Conn struct {
		RateLimiting RateLimitingConfig
	}
	Worker struct {
		WorkerConfig `mapstructure:",squash"`
		Retry        EnqueueOpts
	}
	Storage struct {
		Redis RedisConfig
	}
}

// PitayaConfig provides configuration for a pitaya app
type PitayaConfig struct {
	Heartbeat struct {
		Interval time.Duration
	}
	Handler struct {
		Messages struct {
			Compression bool
		}
	}
	Buffer struct {
		Agent struct {
			Messages int
		}
		Handler struct {
			LocalProcess  int
			RemoteProcess int
		}
	}
	Concurrency struct {
		Handler struct {
			Dispatch int
		}
	}
	Session struct {
		Unique bool
		// CacheTTL 缓存过期时间
		CacheTTL time.Duration
	}
	Metrics struct {
		Period time.Duration
	}
	Acceptor struct {
		ProxyProtocol bool
	}
	ConfSource ConfSource // 配置源
	Log        struct {
		Development bool   // 是否开发模式
		Level       string // 日志等级
	}
	Coroutine CoroutineConfig
}

type CoroutineConfig struct {
	Nums    int
	Buffers int
}
type ConfSource struct {
	FilePath []string // 配置文件路径,不为空表明使用本地文件配置
	Etcd     struct {
		DialTimeout time.Duration
		Endpoints   []string // Etcd地址,不为空表明使用远程etcd配置
		Keys        []string // 要读取监听的etcd key列表
	}
	Interval  time.Duration // 重载间隔
	Formatter string        // 配置格式 必须为 viper.SupportedRemoteProviders
}

// NewDefaultPitayaConfig provides default configuration for Pitaya App
func NewDefaultPitayaConfig() *PitayaConfig {
	return &PitayaConfig{
		Heartbeat: struct{ Interval time.Duration }{
			Interval: time.Duration(30 * time.Second),
		},
		Handler: struct {
			Messages struct {
				Compression bool
			}
		}{
			Messages: struct {
				Compression bool
			}{
				Compression: false,
			},
		},
		Buffer: struct {
			Agent struct {
				Messages int
			}
			Handler struct {
				LocalProcess  int
				RemoteProcess int
			}
		}{
			Agent: struct {
				Messages int
			}{
				Messages: 100,
			},
			Handler: struct {
				LocalProcess  int
				RemoteProcess int
			}{
				LocalProcess:  20,
				RemoteProcess: 20,
			},
		},
		Concurrency: struct {
			Handler struct {
				Dispatch int
			}
		}{
			Handler: struct {
				Dispatch int
			}{
				Dispatch: 25,
			},
		},
		Session: struct {
			Unique   bool
			CacheTTL time.Duration
		}{
			Unique:   true,
			CacheTTL: time.Duration(time.Hour * 24 * 3),
		},
		Metrics: struct {
			Period time.Duration
		}{
			Period: time.Duration(15 * time.Second),
		},
		Acceptor: struct {
			ProxyProtocol bool
		}{
			ProxyProtocol: false,
		},
		ConfSource: ConfSource{
			Interval: time.Duration(5 * time.Minute),
		},
		Log: struct {
			Development bool
			Level       string
		}{Development: false, Level: "ERROR"},
	}
}

// NewPitayaConfig returns a config instance with values extracted from default config paths
func NewPitayaConfig(config *Config) *PitayaConfig {
	conf := NewDefaultPitayaConfig()
	if err := config.UnmarshalKey("pitaya", &conf); err != nil {
		panic(err)
	}
	return conf
}

// BuilderConfig provides configuration for Builder
type BuilderConfig struct {
	Pitaya  PitayaConfig
	Metrics struct {
		Prometheus struct {
			Enabled bool
		}
		Statsd struct {
			Enabled bool
		}
	}
	DefaultPipelines struct {
		StructValidation struct {
			Enabled bool
		}
	}
}

// NewDefaultBuilderConfig provides default builder configuration
func NewDefaultBuilderConfig() *BuilderConfig {
	return &BuilderConfig{
		Pitaya: *NewDefaultPitayaConfig(),
		Metrics: struct {
			Prometheus struct {
				Enabled bool
			}
			Statsd struct {
				Enabled bool
			}
		}{
			Prometheus: struct {
				Enabled bool
			}{
				Enabled: false,
			},
			Statsd: struct {
				Enabled bool
			}{
				Enabled: false,
			},
		},
		DefaultPipelines: struct {
			StructValidation struct {
				Enabled bool
			}
		}{
			StructValidation: struct {
				Enabled bool
			}{
				Enabled: false,
			},
		},
	}
}

// NewBuilderConfig reads from config to build builder configuration
func NewBuilderConfig(config *Config) *BuilderConfig {
	conf := NewDefaultBuilderConfig()
	if err := config.Unmarshal(&conf); err != nil {
		panic(err)
	}
	fmt.Println(conf)
	return conf
}

// GRPCClientConfig rpc client config struct
type GRPCClientConfig struct {
	DialTimeout    time.Duration
	LazyConnection bool
	RequestTimeout time.Duration
}

// NewDefaultGRPCClientConfig rpc client default config struct
func NewDefaultGRPCClientConfig() *GRPCClientConfig {
	return &GRPCClientConfig{
		DialTimeout:    time.Duration(5 * time.Second),
		LazyConnection: false,
		RequestTimeout: time.Duration(5 * time.Second),
	}
}

// NewGRPCClientConfig reads from config to build GRPCCLientConfig
func NewGRPCClientConfig(config *Config) *GRPCClientConfig {
	conf := NewDefaultGRPCClientConfig()
	if err := config.UnmarshalKey("pitaya.cluster.rpc.client.grpc", &conf); err != nil {
		panic(err)
	}
	return conf
}

// GRPCServerConfig provides configuration for GRPCServer
type GRPCServerConfig struct {
	Port int
}

// NewDefaultGRPCServerConfig returns a default GRPCServerConfig
func NewDefaultGRPCServerConfig() *GRPCServerConfig {
	return &GRPCServerConfig{
		Port: 3434,
	}
}

// NewGRPCServerConfig reads from config to build GRPCServerConfig
func NewGRPCServerConfig(config *Config) *GRPCServerConfig {
	return &GRPCServerConfig{
		Port: config.GetInt("pitaya.cluster.rpc.server.grpc.port"),
	}
}

// NatsRPCClientConfig provides nats client configuration
type NatsRPCClientConfig struct {
	Connect                string
	MaxReconnectionRetries int
	RequestTimeout         time.Duration
	ConnectionTimeout      time.Duration
}

// NewDefaultNatsRPCClientConfig provides default nats client configuration
func NewDefaultNatsRPCClientConfig() *NatsRPCClientConfig {
	return &NatsRPCClientConfig{
		Connect:                "nats://localhost:4222",
		MaxReconnectionRetries: 15,
		RequestTimeout:         time.Duration(5 * time.Second),
		ConnectionTimeout:      time.Duration(2 * time.Second),
	}
}

// NewNatsRPCClientConfig reads from config to build nats client configuration
func NewNatsRPCClientConfig(config *Config) *NatsRPCClientConfig {
	conf := NewDefaultNatsRPCClientConfig()
	if err := config.UnmarshalKey("pitaya.cluster.rpc.client.nats", &conf); err != nil {
		panic(err)
	}
	return conf
}

// NatsRPCServerConfig provides nats server configuration
type NatsRPCServerConfig struct {
	Connect                string
	MaxReconnectionRetries int
	Buffer                 struct {
		Messages int
		Push     int
	}
	Services          int
	ConnectionTimeout time.Duration
}

// NewDefaultNatsRPCServerConfig provides default nats server configuration
func NewDefaultNatsRPCServerConfig() *NatsRPCServerConfig {
	return &NatsRPCServerConfig{
		Connect:                "nats://localhost:4222",
		MaxReconnectionRetries: 15,
		Buffer: struct {
			Messages int
			Push     int
		}{
			Messages: 75,
			Push:     100,
		},
		Services:          30,
		ConnectionTimeout: time.Duration(2 * time.Second),
	}
}

// NewNatsRPCServerConfig reads from config to build nats server configuration
func NewNatsRPCServerConfig(config *Config) *NatsRPCServerConfig {
	conf := NewDefaultNatsRPCServerConfig()
	if err := config.UnmarshalKey("pitaya.cluster.rpc.server.nats", &conf); err != nil {
		panic(err)
	}
	return conf
}

// InfoRetrieverConfig provides InfoRetriever configuration
type InfoRetrieverConfig struct {
	Region string
}

// NewDefaultInfoRetrieverConfig provides default configuration for InfoRetriever
func NewDefaultInfoRetrieverConfig() *InfoRetrieverConfig {
	return &InfoRetrieverConfig{
		Region: "",
	}
}

// NewInfoRetrieverConfig reads from config to build configuration for InfoRetriever
func NewInfoRetrieverConfig(c *Config) *InfoRetrieverConfig {
	conf := NewDefaultInfoRetrieverConfig()
	if err := c.UnmarshalKey("pitaya.cluster.info", &conf); err != nil {
		panic(err)
	}
	return conf
}

// EtcdServiceDiscoveryConfig Etcd service discovery config
type EtcdServiceDiscoveryConfig struct {
	Endpoints   []string
	User        string
	Pass        string
	DialTimeout time.Duration
	Prefix      string
	Heartbeat   struct {
		TTL time.Duration
		Log bool
	}
	SyncServers struct {
		Interval    time.Duration
		Parallelism int
	}
	Revoke struct {
		Timeout time.Duration
	}
	GrantLease struct {
		Timeout       time.Duration
		MaxRetries    int
		RetryInterval time.Duration
	}
	Shutdown struct {
		Delay time.Duration
	}
	ServerTypesBlacklist []string
	// 选举
	Election struct {
		Enable bool   // 是否开启
		Name   string // 岗位名称
	}
}

type RedisConfig struct {
	// A seed list of host:port addresses of cluster nodes.
	Addrs        []string
	Username     string
	Password     string
	PoolSize     int // 连接池最大socket连接数，默认为4倍CPU数， 4 * runtime.NumCPU
	MinIdleConns int // 在启动阶段创建指定数量的Idle连接，并长期维持idle状态的连接数不少于指定数量；。

	// 超时
	DialTimeout  time.Duration // 连接建立超时时间，默认5秒。
	ReadTimeout  time.Duration // 读超时，默认3秒， -1表示取消读超时
	WriteTimeout time.Duration // 写超时，默认等于读超时
	PoolTimeout  time.Duration // 当所有连接都处在繁忙状态时，客户端等待可用连接的最大等待时长，默认为读超时+1秒。

	// 闲置连接检查包括IdleTimeout，MaxConnAge
	IdleCheckFrequency time.Duration // 闲置连接检查的周期，默认为1分钟，-1表示不做周期性检查，只在客户端获取连接时对闲置连接进行处理。
	IdleTimeout        time.Duration // 闲置超时，默认5分钟，-1表示取消闲置超时检查
	MaxConnAge         time.Duration // 连接存活时长，从创建开始计时，超过指定时长则关闭连接，默认为0，即不关闭存活时长较长的连接

	// 命令执行失败时的重试策略
	MaxRetries      int           // 命令执行失败时，最多重试多少次，默认为0即不重试
	MinRetryBackoff time.Duration // 每次计算重试间隔时间的下限，默认8毫秒，-1表示取消间隔
	MaxRetryBackoff time.Duration // 每次计算重试间隔时间的上限，默认8毫秒，-1表示取消间隔
	Type            string        // redis模式: node/cluster
	CacheTTL        time.Duration // 过期时间
}

func NewDefaultRedisConfig() *RedisConfig {
	return &RedisConfig{
		Addrs: []string{"localhost:6379"},
	}
}
func ToRedisNodeConfig(conf *RedisConfig) *redis.Options {
	opts := &redis.Options{
		Addr:               conf.Addrs[0],
		Dialer:             nil,
		OnConnect:          nil,
		Username:           conf.Username,
		Password:           conf.Password,
		MaxRetries:         conf.MaxRetries,
		MinRetryBackoff:    conf.MinRetryBackoff,
		MaxRetryBackoff:    conf.MaxRetryBackoff,
		DialTimeout:        conf.DialTimeout,
		ReadTimeout:        conf.ReadTimeout,
		WriteTimeout:       conf.WriteTimeout,
		PoolFIFO:           false,
		PoolSize:           conf.PoolSize,
		MinIdleConns:       conf.MinIdleConns,
		MaxConnAge:         conf.MaxConnAge,
		PoolTimeout:        conf.PoolTimeout,
		IdleTimeout:        conf.IdleTimeout,
		IdleCheckFrequency: conf.IdleCheckFrequency,
		TLSConfig:          nil,
	}
	return opts
}
func ToRedisClusterOption(conf *RedisConfig) *redis.ClusterOptions {
	opts := &redis.ClusterOptions{
		Addrs:              conf.Addrs,
		NewClient:          nil,
		MaxRedirects:       0,
		ReadOnly:           false,
		RouteByLatency:     false,
		RouteRandomly:      false,
		ClusterSlots:       nil,
		Dialer:             nil,
		OnConnect:          nil,
		Username:           conf.Username,
		Password:           conf.Password,
		MaxRetries:         conf.MaxRetries,
		MinRetryBackoff:    conf.MinRetryBackoff,
		MaxRetryBackoff:    conf.MaxRetryBackoff,
		DialTimeout:        conf.DialTimeout,
		ReadTimeout:        conf.ReadTimeout,
		WriteTimeout:       conf.WriteTimeout,
		PoolFIFO:           false,
		PoolSize:           conf.PoolSize,
		MinIdleConns:       conf.MinIdleConns,
		MaxConnAge:         conf.MaxConnAge,
		PoolTimeout:        conf.PoolTimeout,
		IdleTimeout:        conf.IdleTimeout,
		IdleCheckFrequency: conf.IdleCheckFrequency,
		TLSConfig:          nil,
	}
	return opts
}
func ToRedisUniversalOptions(conf *RedisConfig) *redis.UniversalOptions {
	opts := &redis.UniversalOptions{
		Addrs:              conf.Addrs,
		MaxRedirects:       0,
		ReadOnly:           false,
		RouteByLatency:     false,
		RouteRandomly:      false,
		Dialer:             nil,
		OnConnect:          nil,
		Username:           conf.Username,
		Password:           conf.Password,
		MaxRetries:         conf.MaxRetries,
		MinRetryBackoff:    conf.MinRetryBackoff,
		MaxRetryBackoff:    conf.MaxRetryBackoff,
		DialTimeout:        conf.DialTimeout,
		ReadTimeout:        conf.ReadTimeout,
		WriteTimeout:       conf.WriteTimeout,
		PoolFIFO:           false,
		PoolSize:           conf.PoolSize,
		MinIdleConns:       conf.MinIdleConns,
		MaxConnAge:         conf.MaxConnAge,
		PoolTimeout:        conf.PoolTimeout,
		IdleTimeout:        conf.IdleTimeout,
		IdleCheckFrequency: conf.IdleCheckFrequency,
		TLSConfig:          nil,
	}
	return opts
}
func NewRedisConfig(config *Config) *RedisConfig {
	conf := NewDefaultRedisConfig()
	if err := config.UnmarshalKey("pitaya.storage.redis", &conf); err != nil {
		panic(err)
	}
	return conf
}

// NewDefaultEtcdServiceDiscoveryConfig Etcd service discovery default config
func NewDefaultEtcdServiceDiscoveryConfig() *EtcdServiceDiscoveryConfig {
	return &EtcdServiceDiscoveryConfig{
		Endpoints:   []string{"localhost:2379"},
		User:        "",
		Pass:        "",
		DialTimeout: time.Duration(5 * time.Second),
		Prefix:      "pitaya/",
		Heartbeat: struct {
			TTL time.Duration
			Log bool
		}{
			TTL: time.Duration(60 * time.Second),
			Log: false,
		},
		SyncServers: struct {
			Interval    time.Duration
			Parallelism int
		}{
			Interval:    time.Duration(120 * time.Second),
			Parallelism: 10,
		},
		Revoke: struct {
			Timeout time.Duration
		}{
			Timeout: time.Duration(5 * time.Second),
		},
		GrantLease: struct {
			Timeout       time.Duration
			MaxRetries    int
			RetryInterval time.Duration
		}{
			Timeout:       time.Duration(60 * time.Second),
			MaxRetries:    15,
			RetryInterval: time.Duration(5 * time.Second),
		},
		Shutdown: struct {
			Delay time.Duration
		}{
			Delay: time.Duration(300 * time.Millisecond),
		},
		ServerTypesBlacklist: nil,
		Election: struct {
			Enable bool
			Name   string
		}{},
	}
}

// NewEtcdServiceDiscoveryConfig Etcd service discovery config with default config paths
func NewEtcdServiceDiscoveryConfig(config *Config) *EtcdServiceDiscoveryConfig {
	conf := NewDefaultEtcdServiceDiscoveryConfig()
	if err := config.UnmarshalKey("pitaya.cluster.sd.etcd", &conf); err != nil {
		panic(err)
	}
	return conf
}

// NewDefaultCustomMetricsSpec returns an empty *CustomMetricsSpec
func NewDefaultCustomMetricsSpec() *models.CustomMetricsSpec {
	return &models.CustomMetricsSpec{
		Summaries: []*models.Summary{},
		Gauges:    []*models.Gauge{},
		Counters:  []*models.Counter{},
	}
}

// NewCustomMetricsSpec returns a *CustomMetricsSpec by reading config key (DEPRECATED)
func NewCustomMetricsSpec(config *Config) *models.CustomMetricsSpec {
	spec := &models.CustomMetricsSpec{}

	if err := config.UnmarshalKey("pitaya.metrics.custom", &spec); err != nil {
		return NewDefaultCustomMetricsSpec()
	}

	return spec
}

// PrometheusConfig provides configuration for PrometheusReporter
type PrometheusConfig struct {
	Prometheus struct {
		Port             int
		AdditionalLabels map[string]string
	}
	Game        string
	ConstLabels map[string]string
}

// NewDefaultPrometheusConfig provides default configuration for PrometheusReporter
func NewDefaultPrometheusConfig() *PrometheusConfig {
	return &PrometheusConfig{
		Prometheus: struct {
			Port             int
			AdditionalLabels map[string]string
		}{
			Port:             9090,
			AdditionalLabels: map[string]string{},
		},
		ConstLabels: map[string]string{},
	}
}

// NewPrometheusConfig reads from config to build configuration for PrometheusReporter
func NewPrometheusConfig(config *Config) *PrometheusConfig {
	conf := NewDefaultPrometheusConfig()
	if err := config.UnmarshalKey("pitaya.metrics", &conf); err != nil {
		panic(err)
	}
	return conf
}

// StatsdConfig provides configuration for statsd
type StatsdConfig struct {
	Statsd struct {
		Host   string
		Prefix string
		Rate   float64
	}
	ConstLabels map[string]string
}

// NewDefaultStatsdConfig provides default configuration for statsd
func NewDefaultStatsdConfig() *StatsdConfig {
	return &StatsdConfig{
		Statsd: struct {
			Host   string
			Prefix string
			Rate   float64
		}{
			Host:   "localhost:9125",
			Prefix: "pitaya.",
			Rate:   1,
		},
		ConstLabels: map[string]string{},
	}
}

// NewStatsdConfig reads from config to build configuration for statsd
func NewStatsdConfig(config *Config) *StatsdConfig {
	conf := NewDefaultStatsdConfig()
	if err := config.UnmarshalKey("pitaya.metrics", &conf); err != nil {
		panic(err)
	}
	return conf
}

// WorkerConfig provides worker configuration
type WorkerConfig struct {
	Redis struct {
		ServerURL string
		Pool      string
		Password  string
	}
	Namespace   string
	Concurrency int
}

// NewDefaultWorkerConfig provides worker default configuration
func NewDefaultWorkerConfig() *WorkerConfig {
	return &WorkerConfig{
		Redis: struct {
			ServerURL string
			Pool      string
			Password  string
		}{
			ServerURL: "localhost:6379",
			Pool:      "10",
		},
		Concurrency: 1,
	}
}

// NewWorkerConfig provides worker configuration based on default string paths
func NewWorkerConfig(config *Config) *WorkerConfig {
	conf := NewDefaultWorkerConfig()
	if err := config.UnmarshalKey("pitaya.worker", &conf); err != nil {
		panic(err)
	}
	return conf
}

// EnqueueOpts has retry options for worker
type EnqueueOpts struct {
	Enabled     bool
	Max         int
	Exponential int
	MinDelay    int
	MaxDelay    int
	MaxRandom   int
}

// NewDefaultEnqueueOpts provides default EnqueueOpts
func NewDefaultEnqueueOpts() *EnqueueOpts {
	return &EnqueueOpts{
		Enabled:     true,
		Max:         2,
		Exponential: 5,
		MinDelay:    10,
		MaxDelay:    10,
		MaxRandom:   0,
	}
}

// NewEnqueueOpts reads from config to build *EnqueueOpts
func NewEnqueueOpts(config *Config) *EnqueueOpts {
	conf := NewDefaultEnqueueOpts()
	if err := config.UnmarshalKey("pitaya.worker.retry", &conf); err != nil {
		panic(err)
	}
	return conf
}

// MemoryGroupConfig provides configuration for MemoryGroup
type MemoryGroupConfig struct {
	TickDuration time.Duration
}

// NewDefaultMemoryGroupConfig returns a new, default group instance
func NewDefaultMemoryGroupConfig() *MemoryGroupConfig {
	return &MemoryGroupConfig{TickDuration: time.Duration(30 * time.Second)}
}

// NewMemoryGroupConfig returns a new, default group instance
func NewMemoryGroupConfig(conf *Config) *MemoryGroupConfig {
	c := NewDefaultMemoryGroupConfig()
	if err := conf.UnmarshalKey("pitaya.groups.memory", &c); err != nil {
		panic(err)
	}
	return c
}

// EtcdGroupServiceConfig provides ETCD configuration
type EtcdGroupServiceConfig struct {
	DialTimeout        time.Duration
	Endpoints          []string
	Prefix             string
	TransactionTimeout time.Duration
}

// NewDefaultEtcdGroupServiceConfig provides default ETCD configuration
func NewDefaultEtcdGroupServiceConfig() *EtcdGroupServiceConfig {
	return &EtcdGroupServiceConfig{
		DialTimeout:        time.Duration(5 * time.Second),
		Endpoints:          []string{"localhost:2379"},
		Prefix:             "pitaya/",
		TransactionTimeout: time.Duration(5 * time.Second),
	}
}

// NewEtcdGroupServiceConfig reads from config to build ETCD configuration
func NewEtcdGroupServiceConfig(config *Config) *EtcdGroupServiceConfig {
	conf := NewDefaultEtcdGroupServiceConfig()
	if err := config.UnmarshalKey("pitaya.groups.etcd", &conf); err != nil {
		panic(err)
	}
	return conf
}

// ETCDBindingConfig provides configuration for ETCDBindingStorage
type ETCDBindingConfig struct {
	DialTimeout time.Duration
	Endpoints   []string
	Prefix      string
	LeaseTTL    time.Duration
}

// NewDefaultETCDBindingConfig provides default configuration for ETCDBindingStorage
func NewDefaultETCDBindingConfig() *ETCDBindingConfig {
	return &ETCDBindingConfig{
		DialTimeout: time.Duration(5 * time.Second),
		Endpoints:   []string{"localhost:2379"},
		Prefix:      "pitaya/",
		LeaseTTL:    time.Duration(5 * time.Hour),
	}
}

// NewETCDBindingConfig reads from config to build ETCDBindingStorage configuration
func NewETCDBindingConfig(config *Config) *ETCDBindingConfig {
	conf := NewDefaultETCDBindingConfig()
	if err := config.UnmarshalKey("pitaya.modules.bindingstorage.etcd", &conf); err != nil {
		panic(err)
	}
	return conf
}

// RateLimitingConfig rate limits config
type RateLimitingConfig struct {
	Limit        int
	Interval     time.Duration
	ForceDisable bool
}

// NewDefaultRateLimitingConfig rate limits default config
func NewDefaultRateLimitingConfig() *RateLimitingConfig {
	return &RateLimitingConfig{
		Limit:        20,
		Interval:     time.Duration(time.Second),
		ForceDisable: false,
	}
}

// NewRateLimitingConfig reads from config to build rate limiting configuration
func NewRateLimitingConfig(config *Config) *RateLimitingConfig {
	conf := NewDefaultRateLimitingConfig()
	if err := config.UnmarshalKey("pitaya.conn.ratelimiting", &conf); err != nil {
		panic(err)
	}
	return conf
}
