package config

import (
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/topfreegames/pitaya/v2/metrics/models"
)

// PitayaConfig provides all the configuration for a pitaya app
type PitayaConfig struct {
	SerializerType   uint16 `mapstructure:"serializertype"`
	DefaultPipelines struct {
		StructValidation struct {
			Enabled bool `mapstructure:"enabled"`
		} `mapstructure:"structvalidation"`
	} `mapstructure:"defaultpipelines"`
	Modules   ModulesConfig
	Heartbeat struct {
		Interval time.Duration `mapstructure:"interval"`
	} `mapstructure:"heartbeat"`
	Handler struct {
		Messages struct {
			Compression bool `mapstructure:"compression"`
		} `mapstructure:"messages"`
	} `mapstructure:"handler"`
	Buffer struct {
		Agent struct {
			Messages int `mapstructure:"messages"`
		} `mapstructure:"agent"`
		Handler struct {
			LocalProcess  int `mapstructure:"localprocess"`
			RemoteProcess int `mapstructure:"remoteprocess"`
		} `mapstructure:"handler"`
	} `mapstructure:"buffer"`
	Concurrency struct {
		Handler struct {
			Dispatch int `mapstructure:"dispatch"`
		} `mapstructure:"handler"`
	} `mapstructure:"concurrency"`
	Session struct {
		Unique bool `mapstructure:"unique"`
		// CacheTTL 缓存过期时间
		CacheTTL time.Duration
		Drain    struct {
			Enabled bool          `mapstructure:"enabled"`
			Timeout time.Duration `mapstructure:"timeout"`
			Period  time.Duration `mapstructure:"period"`
		} `mapstructure:"drain"`
	} `mapstructure:"session"`

	Acceptor struct {
		ProxyProtocol bool `mapstructure:"proxyprotocol"`
	} `mapstructure:"acceptor"`
	Conn struct {
		RateLimiting RateLimitingConfig `mapstructure:"rateLimiting"`
	} `mapstructure:"conn"`
	Metrics    MetricsConfig `mapstructure:"metrics"`
	Cluster    ClusterConfig `mapstructure:"cluster"`
	Groups     GroupsConfig  `mapstructure:"groups"`
	Worker     WorkerConfig  `mapstructure:"worker"`
	ConfSource ConfSource    // 配置源
	Log        struct {
		Development bool   // 是否开发模式
		Level       string // 日志等级
	}
	GoPools map[string]GoPool // 有状态线程池配置
	Storage struct {
		Redis RedisConfig
	}
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

type GoPool struct {
	Name                string          // 线程池名
	Expire              time.Duration   // 线程池回收不使用的goroutine的间隔时长
	DisablePurge        bool            // 是否禁止超时回收
	DisablePurgeRunning bool            // 是否禁止回收有任务运行中的线程(即使超时)
	TaskBuffer          int             // 每个worker的队列长度
	DisableTimeoutWatch bool            // 是否禁用监控超时
	TimeoutBuckets      []time.Duration // 监控项
}

// NewDefaultPitayaConfig provides default configuration for Pitaya App
func NewDefaultPitayaConfig() *PitayaConfig {
	return &PitayaConfig{
		SerializerType: 1,
		DefaultPipelines: struct {
			StructValidation struct {
				Enabled bool `mapstructure:"enabled"`
			} `mapstructure:"structvalidation"`
		}{
			StructValidation: struct {
				Enabled bool `mapstructure:"enabled"`
			}{
				Enabled: false,
			},
		},
		Heartbeat: struct {
			Interval time.Duration `mapstructure:"interval"`
		}{
			Interval: time.Duration(30 * time.Second),
		},
		Handler: struct {
			Messages struct {
				Compression bool `mapstructure:"compression"`
			} `mapstructure:"messages"`
		}{
			Messages: struct {
				Compression bool `mapstructure:"compression"`
			}{
				Compression: false,
			},
		},
		Buffer: struct {
			Agent struct {
				Messages int `mapstructure:"messages"`
			} `mapstructure:"agent"`
			Handler struct {
				LocalProcess  int `mapstructure:"localprocess"`
				RemoteProcess int `mapstructure:"remoteprocess"`
			} `mapstructure:"handler"`
		}{
			Agent: struct {
				Messages int `mapstructure:"messages"`
			}{
				Messages: 100,
			},
			Handler: struct {
				LocalProcess  int `mapstructure:"localprocess"`
				RemoteProcess int `mapstructure:"remoteprocess"`
			}{
				LocalProcess:  20,
				RemoteProcess: 20,
			},
		},
		Concurrency: struct {
			Handler struct {
				Dispatch int `mapstructure:"dispatch"`
			} `mapstructure:"handler"`
		}{
			Handler: struct {
				Dispatch int `mapstructure:"dispatch"`
			}{
				Dispatch: 25,
			},
		},
		Session: struct {
			Unique   bool `mapstructure:"unique"`
			CacheTTL time.Duration
			Drain    struct {
				Enabled bool          `mapstructure:"enabled"`
				Timeout time.Duration `mapstructure:"timeout"`
				Period  time.Duration `mapstructure:"period"`
			} `mapstructure:"drain"`
		}{
			Unique:   true,
			CacheTTL: time.Hour * 24 * 3,
			Drain: struct {
				Enabled bool          `mapstructure:"enabled"`
				Timeout time.Duration `mapstructure:"timeout"`
				Period  time.Duration `mapstructure:"period"`
			}{
				Enabled: false,
				Timeout: time.Duration(6 * time.Hour),
				Period:  time.Duration(5 * time.Second),
			},
		},
		Metrics: *newDefaultMetricsConfig(),
		Cluster: *newDefaultClusterConfig(),
		Groups:  *newDefaultGroupsConfig(),
		Worker:  *newDefaultWorkerConfig(),
		Modules: *newDefaultModulesConfig(),
		Acceptor: struct {
			ProxyProtocol bool `mapstructure:"proxyprotocol"`
		}{
			ProxyProtocol: false,
		},
		Conn: struct {
			RateLimiting RateLimitingConfig `mapstructure:"rateLimiting"`
		}{
			RateLimiting: *newDefaultRateLimitingConfig(),
		},
		ConfSource: ConfSource{
			Interval: 5 * time.Minute,
		},
		Log: struct {
			Development bool
			Level       string
		}{Development: false, Level: "ERROR"},
		GoPools: map[string]GoPool{},
		Storage: struct{ Redis RedisConfig }{Redis: *newDefaultRedisConfig()},
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

// GRPCClientConfig rpc client config struct
type GRPCClientConfig struct {
	DialTimeout    time.Duration `mapstructure:"dialtimeout"`
	LazyConnection bool          `mapstructure:"lazyconnection"`
	RequestTimeout time.Duration `mapstructure:"requesttimeout"`
}

// newDefaultGRPCClientConfig rpc client default config struct
func newDefaultGRPCClientConfig() *GRPCClientConfig {
	return &GRPCClientConfig{
		DialTimeout:    time.Duration(5 * time.Second),
		LazyConnection: false,
		RequestTimeout: time.Duration(5 * time.Second),
	}
}

// GRPCServerConfig provides configuration for GRPCServer
type GRPCServerConfig struct {
	Port int `mapstructure:"port"`
}

// newDefaultGRPCServerConfig returns a default GRPCServerConfig
func newDefaultGRPCServerConfig() *GRPCServerConfig {
	return &GRPCServerConfig{
		Port: 3434,
	}
}

// NatsRPCClientConfig provides nats client configuration
type NatsRPCClientConfig struct {
	Connect                string        `mapstructure:"connect"`
	MaxReconnectionRetries int           `mapstructure:"maxreconnectionretries"`
	RequestTimeout         time.Duration `mapstructure:"requesttimeout"`
	ConnectionTimeout      time.Duration `mapstructure:"connectiontimeout"`
}

// newDefaultNatsRPCClientConfig provides default nats client configuration
func newDefaultNatsRPCClientConfig() *NatsRPCClientConfig {
	return &NatsRPCClientConfig{
		Connect:                "nats://localhost:4222",
		MaxReconnectionRetries: 15,
		RequestTimeout:         time.Duration(5 * time.Second),
		ConnectionTimeout:      time.Duration(2 * time.Second),
	}
}

// NatsRPCServerConfig provides nats server configuration
type NatsRPCServerConfig struct {
	Connect                string `mapstructure:"connect"`
	MaxReconnectionRetries int    `mapstructure:"maxreconnectionretries"`
	Buffer                 struct {
		Messages int `mapstructure:"messages"`
		Push     int `mapstructure:"push"`
	} `mapstructure:"buffer"`
	Services          int           `mapstructure:"services"`
	ConnectionTimeout time.Duration `mapstructure:"connectiontimeout"`
	RequestTimeout    time.Duration
}

// newDefaultNatsRPCServerConfig provides default nats server configuration
func newDefaultNatsRPCServerConfig() *NatsRPCServerConfig {
	return &NatsRPCServerConfig{
		Connect:                "nats://localhost:4222",
		MaxReconnectionRetries: 15,
		Buffer: struct {
			Messages int `mapstructure:"messages"`
			Push     int `mapstructure:"push"`
		}{
			Messages: 75,
			Push:     100,
		},
		Services:          30,
		ConnectionTimeout: time.Duration(2 * time.Second),
		RequestTimeout:    time.Duration(5 * time.Second),
	}
}

// InfoRetrieverConfig provides InfoRetriever configuration
type InfoRetrieverConfig struct {
	Region string `mapstructure:"region"`
}

// newDefaultInfoRetrieverConfig provides default configuration for InfoRetriever
func newDefaultInfoRetrieverConfig() *InfoRetrieverConfig {
	return &InfoRetrieverConfig{
		Region: "",
	}
}

// EtcdServiceDiscoveryConfig Etcd service discovery config
type EtcdServiceDiscoveryConfig struct {
	Endpoints   []string      `mapstructure:"endpoints"`
	User        string        `mapstructure:"user"`
	Pass        string        `mapstructure:"pass"`
	DialTimeout time.Duration `mapstructure:"dialtimeout"`
	Prefix      string        `mapstructure:"prefix"`
	Heartbeat   struct {
		TTL time.Duration `mapstructure:"ttl"`
		Log bool          `mapstructure:"log"`
	} `mapstructure:"heartbeat"`
	SyncServers struct {
		Interval    time.Duration `mapstructure:"interval"`
		Parallelism int           `mapstructure:"parallelism"`
	} `mapstructure:"syncservers"`
	Revoke struct {
		Timeout time.Duration `mapstructure:"timeout"`
	} `mapstructure:"revoke"`
	GrantLease struct {
		Timeout       time.Duration `mapstructure:"timeout"`
		MaxRetries    int           `mapstructure:"maxretries"`
		RetryInterval time.Duration `mapstructure:"retryinterval"`
	} `mapstructure:"grantlease"`
	Shutdown struct {
		Delay time.Duration `mapstructure:"delay"`
	} `mapstructure:"shutdown"`
	ServerTypesBlacklist []string `mapstructure:"servertypesblacklist"`
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

func newDefaultRedisConfig() *RedisConfig {
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
	conf := newDefaultRedisConfig()
	if err := config.UnmarshalKey("pitaya.storage.redis", &conf); err != nil {
		panic(err)
	}
	return conf
}

// newDefaultEtcdServiceDiscoveryConfig Etcd service discovery default config
func newDefaultEtcdServiceDiscoveryConfig() *EtcdServiceDiscoveryConfig {
	return &EtcdServiceDiscoveryConfig{
		Endpoints:   []string{"localhost:2379"},
		User:        "",
		Pass:        "",
		DialTimeout: time.Duration(5 * time.Second),
		Prefix:      "pitaya/",
		Heartbeat: struct {
			TTL time.Duration `mapstructure:"ttl"`
			Log bool          `mapstructure:"log"`
		}{
			TTL: time.Duration(60 * time.Second),
			Log: false,
		},
		SyncServers: struct {
			Interval    time.Duration `mapstructure:"interval"`
			Parallelism int           `mapstructure:"parallelism"`
		}{
			Interval:    time.Duration(120 * time.Second),
			Parallelism: 10,
		},
		Revoke: struct {
			Timeout time.Duration `mapstructure:"timeout"`
		}{
			Timeout: time.Duration(5 * time.Second),
		},
		GrantLease: struct {
			Timeout       time.Duration `mapstructure:"timeout"`
			MaxRetries    int           `mapstructure:"maxretries"`
			RetryInterval time.Duration `mapstructure:"retryinterval"`
		}{
			Timeout:       time.Duration(60 * time.Second),
			MaxRetries:    15,
			RetryInterval: time.Duration(5 * time.Second),
		},
		Shutdown: struct {
			Delay time.Duration `mapstructure:"delay"`
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

// Metrics provides configuration for all metrics related configurations
type MetricsConfig struct {
	Period           time.Duration            `mapstructure:"period"`
	Game             string                   `mapstructure:"game"`
	AdditionalLabels map[string]string        `mapstructure:"additionallabels"`
	ConstLabels      map[string]string        `mapstructure:"constlabels"`
	Custom           models.CustomMetricsSpec `mapstructure:"custom"`
	Prometheus       *PrometheusConfig        `mapstructure:"prometheus"`
	Statsd           *StatsdConfig            `mapstructure:"statsd"`
}

// newDefaultPrometheusConfig provides default configuration for PrometheusReporter
func newDefaultMetricsConfig() *MetricsConfig {
	return &MetricsConfig{
		Period:           time.Duration(15 * time.Second),
		ConstLabels:      map[string]string{},
		AdditionalLabels: map[string]string{},
		Custom:           *NewDefaultCustomMetricsSpec(),
		Prometheus:       newDefaultPrometheusConfig(),
		Statsd:           newDefaultStatsdConfig(),
	}
}

// PrometheusConfig provides configuration for PrometheusReporter
type PrometheusConfig struct {
	Port    int  `mapstructure:"port"`
	Enabled bool `mapstructure:"enabled"`
}

// newDefaultPrometheusConfig provides default configuration for PrometheusReporter
func newDefaultPrometheusConfig() *PrometheusConfig {
	return &PrometheusConfig{
		Port:    9090,
		Enabled: false,
	}
}

// StatsdConfig provides configuration for statsd
type StatsdConfig struct {
	Enabled bool    `mapstructure:"enabled"`
	Host    string  `mapstructure:"host"`
	Prefix  string  `mapstructure:"prefix"`
	Rate    float64 `mapstructure:"rate"`
}

// newDefaultStatsdConfig provides default configuration for statsd
func newDefaultStatsdConfig() *StatsdConfig {
	return &StatsdConfig{
		Enabled: false,
		Host:    "localhost:9125",
		Prefix:  "pitaya.",
		Rate:    1,
	}
}

// newDefaultStatsdConfig provides default configuration for statsd
func newDefaultClusterConfig() *ClusterConfig {
	return &ClusterConfig{
		Info: *newDefaultInfoRetrieverConfig(),
		RPC:  *newDefaultClusterRPCConfig(),
		SD:   *newDefaultClusterSDConfig(),
	}
}

type ClusterConfig struct {
	Info InfoRetrieverConfig `mapstructure:"info"`
	RPC  ClusterRPCConfig    `mapstructure:"rpc"`
	SD   ClusterSDConfig     `mapstructure:"sd"`
}

type ClusterRPCConfig struct {
	Client struct {
		Grpc GRPCClientConfig    `mapstructure:"grpc"`
		Nats NatsRPCClientConfig `mapstructure:"nats"`
	} `mapstructure:"client"`
	Server struct {
		Grpc GRPCServerConfig    `mapstructure:"grpc"`
		Nats NatsRPCServerConfig `mapstructure:"nats"`
	} `mapstructure:"server"`
}

func newDefaultClusterRPCConfig() *ClusterRPCConfig {
	return &ClusterRPCConfig{
		Client: struct {
			Grpc GRPCClientConfig    `mapstructure:"grpc"`
			Nats NatsRPCClientConfig `mapstructure:"nats"`
		}{
			Grpc: *newDefaultGRPCClientConfig(),
			Nats: *newDefaultNatsRPCClientConfig(),
		},
		Server: struct {
			Grpc GRPCServerConfig    `mapstructure:"grpc"`
			Nats NatsRPCServerConfig `mapstructure:"nats"`
		}{
			Grpc: *newDefaultGRPCServerConfig(),
			Nats: *newDefaultNatsRPCServerConfig(),
		},
	}

}

type ClusterSDConfig struct {
	Etcd EtcdServiceDiscoveryConfig `mapstructure:"etcd"`
}

func newDefaultClusterSDConfig() *ClusterSDConfig {
	return &ClusterSDConfig{Etcd: *newDefaultEtcdServiceDiscoveryConfig()}
}

// WorkerConfig provides worker configuration
type WorkerConfig struct {
	Redis struct {
		ServerURL string `mapstructure:"serverurl"`
		Pool      string `mapstructure:"pool"`
		Password  string `mapstructure:"password"`
	} `mapstructure:"redis"`
	Namespace   string      `mapstructure:"namespace"`
	Concurrency int         `mapstructure:"concurrency"`
	Retry       EnqueueOpts `mapstructure:"retry"`
}

// newDefaultWorkerConfig provides worker default configuration
func newDefaultWorkerConfig() *WorkerConfig {
	return &WorkerConfig{
		Redis: struct {
			ServerURL string `mapstructure:"serverurl"`
			Pool      string `mapstructure:"pool"`
			Password  string `mapstructure:"password"`
		}{
			ServerURL: "localhost:6379",
			Pool:      "10",
		},
		Concurrency: 1,
		Retry:       *newDefaultEnqueueOpts(),
	}
}

// EnqueueOpts has retry options for worker
type EnqueueOpts struct {
	Enabled     bool `mapstructure:"enabled"`
	Max         int  `mapstructure:"max"`
	Exponential int  `mapstructure:"exponential"`
	MinDelay    int  `mapstructure:"mindelay"`
	MaxDelay    int  `mapstructure:"maxdelay"`
	MaxRandom   int  `mapstructure:"maxrandom"`
}

// newDefaultEnqueueOpts provides default EnqueueOpts
func newDefaultEnqueueOpts() *EnqueueOpts {
	return &EnqueueOpts{
		Enabled:     true,
		Max:         2,
		Exponential: 5,
		MinDelay:    10,
		MaxDelay:    10,
		MaxRandom:   0,
	}
}

// MemoryGroupConfig provides configuration for MemoryGroup
type MemoryGroupConfig struct {
	TickDuration time.Duration `mapstructure:"tickduration"`
}

// newDefaultMemoryGroupConfig returns a new, default group instance
func newDefaultMemoryGroupConfig() *MemoryGroupConfig {
	return &MemoryGroupConfig{TickDuration: time.Duration(30 * time.Second)}
}

// EtcdGroupServiceConfig provides ETCD configuration
type EtcdGroupServiceConfig struct {
	DialTimeout        time.Duration `mapstructure:"dialtimeout"`
	Endpoints          []string      `mapstructure:"endpoints"`
	Prefix             string        `mapstructure:"prefix"`
	TransactionTimeout time.Duration `mapstructure:"transactiontimeout"`
}

// newDefaultEtcdGroupServiceConfig provides default ETCD configuration
func newDefaultEtcdGroupServiceConfig() *EtcdGroupServiceConfig {
	return &EtcdGroupServiceConfig{
		DialTimeout:        time.Duration(5 * time.Second),
		Endpoints:          []string{"localhost:2379"},
		Prefix:             "pitaya/",
		TransactionTimeout: time.Duration(5 * time.Second),
	}
}

// NewEtcdGroupServiceConfig reads from config to build ETCD configuration
func newEtcdGroupServiceConfig(config *Config) *EtcdGroupServiceConfig {
	conf := newDefaultEtcdGroupServiceConfig()
	if err := config.UnmarshalKey("pitaya.groups.etcd", &conf); err != nil {
		panic(err)
	}
	return conf
}

type GroupsConfig struct {
	Etcd   EtcdGroupServiceConfig `mapstructure:"etcd"`
	Memory MemoryGroupConfig      `mapstructure:"memory"`
}

// NewDefaultGroupConfig provides default ETCD configuration
func newDefaultGroupsConfig() *GroupsConfig {
	return &GroupsConfig{
		Etcd:   *newDefaultEtcdGroupServiceConfig(),
		Memory: *newDefaultMemoryGroupConfig(),
	}
}

// ETCDBindingConfig provides configuration for ETCDBindingStorage
type ETCDBindingConfig struct {
	DialTimeout time.Duration `mapstructure:"dialtimeout"`
	Endpoints   []string      `mapstructure:"endpoints"`
	Prefix      string        `mapstructure:"prefix"`
	LeaseTTL    time.Duration `mapstructure:"leasettl"`
}

// NewDefaultETCDBindingConfig provides default configuration for ETCDBindingStorage
func newDefaultETCDBindingConfig() *ETCDBindingConfig {
	return &ETCDBindingConfig{
		DialTimeout: time.Duration(5 * time.Second),
		Endpoints:   []string{"localhost:2379"},
		Prefix:      "pitaya/",
		LeaseTTL:    time.Duration(5 * time.Hour),
	}
}

// ModulesConfig provides configuration for Pitaya Modules
type ModulesConfig struct {
	BindingStorage struct {
		Etcd ETCDBindingConfig `mapstructure:"etcd"`
	} `mapstructure:"bindingstorage"`
}

// NewDefaultModulesConfig provides default configuration for Pitaya Modules
func newDefaultModulesConfig() *ModulesConfig {
	return &ModulesConfig{
		BindingStorage: struct {
			Etcd ETCDBindingConfig `mapstructure:"etcd"`
		}{
			Etcd: *newDefaultETCDBindingConfig(),
		},
	}
}

// RateLimitingConfig rate limits config
type RateLimitingConfig struct {
	Limit        int           `mapstructure:"limit"`
	Interval     time.Duration `mapstructure:"interval"`
	ForceDisable bool          `mapstructure:"forcedisable"`
}

// newDefaultRateLimitingConfig rate limits default config
func newDefaultRateLimitingConfig() *RateLimitingConfig {
	return &RateLimitingConfig{
		Limit:        20,
		Interval:     time.Duration(time.Second),
		ForceDisable: false,
	}
}
