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

package config

import (
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/topfreegames/pitaya/v2/co"
	"github.com/topfreegames/pitaya/v2/logger"
)

// ConfLoader 重载监听
type ConfLoader interface {
	// Reload 重载
	//  @return key Provide 提供的 key
	//  @param confStruct 反序列化后的 Provide 提供的 confStruct
	Reload(key string, confStruct interface{})
	// Provide 提供给重载前反序列化用的key和struct
	//  @return key
	//  @return confStruct
	Provide() (key string, confStruct interface{})
}

// LoaderFactory ConfLoader 工厂,用于生成需要动态包装的 ConfLoader 实现
//  @implement ConfLoader
type LoaderFactory struct {
	// ConfLoader.Reload 的包装函数
	ReloadApply func(key string, confStruct interface{})
	// ConfLoader.Provide 的包装函数
	ProvideApply func() (key string, confStruct interface{})
}

func (l LoaderFactory) Reload(key string, confStruct interface{}) {
	l.ReloadApply(key, confStruct)
}
func (l LoaderFactory) Provide() (key string, confStruct interface{}) {
	return l.ProvideApply()
}

// Config is a wrapper around a viper config
type Config struct {
	config   *viper.Viper
	loaders  []ConfLoader
	confconf *confMap // 配置的配置
}

// NewConfig creates a new config with a given viper config if given
func NewConfig(cfgs ...*viper.Viper) *Config {
	var cfg *viper.Viper
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	} else {
		cfg = viper.New()
	}

	cfg.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	cfg.AutomaticEnv()
	c := &Config{config: cfg, confconf: &confMap{}}
	c.fillDefaultValues()
	c.AddLoader(c)
	return c
}

func (c *Config) fillDefaultValues() {
	customMetricsSpec := NewDefaultCustomMetricsSpec()
	builderConfig := NewDefaultBuilderConfig()
	pitayaConfig := NewDefaultPitayaConfig()
	prometheusConfig := NewDefaultPrometheusConfig()
	statsdConfig := NewDefaultStatsdConfig()
	etcdSDConfig := NewDefaultEtcdServiceDiscoveryConfig()
	natsRPCServerConfig := NewDefaultNatsRPCServerConfig()
	natsRPCClientConfig := NewDefaultNatsRPCClientConfig()
	grpcRPCClientConfig := NewDefaultGRPCClientConfig()
	grpcRPCServerConfig := NewDefaultGRPCServerConfig()
	workerConfig := NewDefaultWorkerConfig()
	enqueueOpts := NewDefaultEnqueueOpts()
	groupServiceConfig := NewDefaultMemoryGroupConfig()
	etcdGroupServiceConfig := NewDefaultEtcdGroupServiceConfig()
	rateLimitingConfig := NewDefaultRateLimitingConfig()
	infoRetrieverConfig := NewDefaultInfoRetrieverConfig()
	etcdBindingConfig := NewDefaultETCDBindingConfig()
	redisConfig := NewDefaultRedisConfig()

	defaultsMap := map[string]interface{}{
		"pitaya.buffer.agent.messages": pitayaConfig.Buffer.Agent.Messages,
		// the max buffer size that nats will accept, if this buffer overflows, messages will begin to be dropped
		"pitaya.buffer.handler.localprocess":                    pitayaConfig.Buffer.Handler.LocalProcess,
		"pitaya.buffer.handler.remoteprocess":                   pitayaConfig.Buffer.Handler.RemoteProcess,
		"pitaya.cluster.info.region":                            infoRetrieverConfig.Region,
		"pitaya.cluster.rpc.client.grpc.dialtimeout":            grpcRPCClientConfig.DialTimeout,
		"pitaya.cluster.rpc.client.grpc.requesttimeout":         grpcRPCClientConfig.RequestTimeout,
		"pitaya.cluster.rpc.client.grpc.lazyconnection":         grpcRPCClientConfig.LazyConnection,
		"pitaya.cluster.rpc.client.nats.connect":                natsRPCClientConfig.Connect,
		"pitaya.cluster.rpc.client.nats.connectiontimeout":      natsRPCClientConfig.ConnectionTimeout,
		"pitaya.cluster.rpc.client.nats.maxreconnectionretries": natsRPCClientConfig.MaxReconnectionRetries,
		"pitaya.cluster.rpc.client.nats.requesttimeout":         natsRPCClientConfig.RequestTimeout,
		"pitaya.cluster.rpc.server.grpc.port":                   grpcRPCServerConfig.Port,
		"pitaya.cluster.rpc.server.nats.connect":                natsRPCServerConfig.Connect,
		"pitaya.cluster.rpc.server.nats.connectiontimeout":      natsRPCServerConfig.ConnectionTimeout,
		"pitaya.cluster.rpc.server.nats.maxreconnectionretries": natsRPCServerConfig.MaxReconnectionRetries,
		"pitaya.cluster.rpc.server.nats.services":               natsRPCServerConfig.Services,
		"pitaya.cluster.rpc.server.nats.buffer.messages":        natsRPCServerConfig.Buffer.Messages,
		"pitaya.cluster.rpc.server.nats.buffer.push":            natsRPCServerConfig.Buffer.Push,
		"pitaya.cluster.sd.etcd.dialtimeout":                    etcdSDConfig.DialTimeout,
		"pitaya.cluster.sd.etcd.endpoints":                      etcdSDConfig.Endpoints,
		"pitaya.cluster.sd.etcd.prefix":                         etcdSDConfig.Prefix,
		"pitaya.cluster.sd.etcd.grantlease.maxretries":          etcdSDConfig.GrantLease.MaxRetries,
		"pitaya.cluster.sd.etcd.grantlease.retryinterval":       etcdSDConfig.GrantLease.RetryInterval,
		"pitaya.cluster.sd.etcd.grantlease.timeout":             etcdSDConfig.GrantLease.Timeout,
		"pitaya.cluster.sd.etcd.heartbeat.log":                  etcdSDConfig.Heartbeat.Log,
		"pitaya.cluster.sd.etcd.heartbeat.ttl":                  etcdSDConfig.Heartbeat.TTL,
		"pitaya.cluster.sd.etcd.revoke.timeout":                 etcdSDConfig.Revoke.Timeout,
		"pitaya.cluster.sd.etcd.syncservers.interval":           etcdSDConfig.SyncServers.Interval,
		"pitaya.cluster.sd.etcd.syncserversparallelism":         etcdSDConfig.SyncServers.Parallelism,
		"pitaya.cluster.sd.etcd.shutdown.delay":                 etcdSDConfig.Shutdown.Delay,
		"pitaya.cluster.sd.etcd.servertypeblacklist":            etcdSDConfig.ServerTypesBlacklist,
		// the sum of this config among all the frontend servers should always be less than
		// the sum of pitaya.buffer.cluster.rpc.server.nats.messages, for covering the worst case scenario
		// a single backend server should have the config pitaya.buffer.cluster.rpc.server.nats.messages bigger
		// than the sum of the config pitaya.concurrency.handler.dispatch among all frontend servers
		"pitaya.concurrency.handler.dispatch":              pitayaConfig.Concurrency.Handler.Dispatch,
		"pitaya.defaultpipelines.structvalidation.enabled": builderConfig.DefaultPipelines.StructValidation.Enabled,
		"pitaya.groups.etcd.dialtimeout":                   etcdGroupServiceConfig.DialTimeout,
		"pitaya.groups.etcd.endpoints":                     etcdGroupServiceConfig.Endpoints,
		"pitaya.groups.etcd.prefix":                        etcdGroupServiceConfig.Prefix,
		"pitaya.groups.etcd.transactiontimeout":            etcdGroupServiceConfig.TransactionTimeout,
		"pitaya.groups.memory.tickduration":                groupServiceConfig.TickDuration,
		"pitaya.handler.messages.compression":              pitayaConfig.Handler.Messages.Compression,
		"pitaya.heartbeat.interval":                        pitayaConfig.Heartbeat.Interval,
		"pitaya.metrics.prometheus.additionalTags":         prometheusConfig.Prometheus.AdditionalLabels,
		"pitaya.metrics.constTags":                         prometheusConfig.ConstLabels,
		"pitaya.metrics.custom":                            customMetricsSpec,
		"pitaya.metrics.periodicMetrics.period":            pitayaConfig.Metrics.Period,
		"pitaya.metrics.prometheus.enabled":                builderConfig.Metrics.Prometheus.Enabled,
		"pitaya.metrics.prometheus.port":                   prometheusConfig.Prometheus.Port,
		"pitaya.metrics.statsd.enabled":                    builderConfig.Metrics.Statsd.Enabled,
		"pitaya.metrics.statsd.host":                       statsdConfig.Statsd.Host,
		"pitaya.metrics.statsd.prefix":                     statsdConfig.Statsd.Prefix,
		"pitaya.metrics.statsd.rate":                       statsdConfig.Statsd.Rate,
		"pitaya.modules.bindingstorage.etcd.dialtimeout":   etcdBindingConfig.DialTimeout,
		"pitaya.modules.bindingstorage.etcd.endpoints":     etcdBindingConfig.Endpoints,
		"pitaya.modules.bindingstorage.etcd.leasettl":      etcdBindingConfig.LeaseTTL,
		"pitaya.modules.bindingstorage.etcd.prefix":        etcdBindingConfig.Prefix,
		"pitaya.conn.ratelimiting.limit":                   rateLimitingConfig.Limit,
		"pitaya.conn.ratelimiting.interval":                rateLimitingConfig.Interval,
		"pitaya.conn.ratelimiting.forcedisable":            rateLimitingConfig.ForceDisable,
		"pitaya.session.unique":                            pitayaConfig.Session.Unique,
		"pitaya.session.cachettl":                          pitayaConfig.Session.CacheTTL,
		"pitaya.worker.concurrency":                        workerConfig.Concurrency,
		"pitaya.worker.redis.pool":                         workerConfig.Redis.Pool,
		"pitaya.worker.redis.url":                          workerConfig.Redis.ServerURL,
		"pitaya.worker.retry.enabled":                      enqueueOpts.Enabled,
		"pitaya.worker.retry.exponential":                  enqueueOpts.Exponential,
		"pitaya.worker.retry.max":                          enqueueOpts.Max,
		"pitaya.worker.retry.maxDelay":                     enqueueOpts.MaxDelay,
		"pitaya.worker.retry.maxRandom":                    enqueueOpts.MaxRandom,
		"pitaya.worker.retry.minDelay":                     enqueueOpts.MinDelay,

		"pitaya.storage.redis.addrs":    redisConfig.Addrs,
		"pitaya.storage.redis.username": redisConfig.Username,
		"pitaya.storage.redis.password": redisConfig.Password,
		"pitaya.conf.filepath":          pitayaConfig.Conf.FilePath,
		"pitaya.conf.etcdaddr":          pitayaConfig.Conf.EtcdAddr,
		"pitaya.conf.etcdkeys":          pitayaConfig.Conf.EtcdKeys,
		"pitaya.conf.interval":          pitayaConfig.Conf.Interval,
		"pitaya.conf.formatter":         pitayaConfig.Conf.Formatter,
		"pitaya.log.development":        pitayaConfig.Log.Development,
		"pitaya.log.level":              pitayaConfig.Log.Level,
	}

	for param := range defaultsMap {
		if c.config.Get(param) == nil {
			c.config.SetDefault(param, defaultsMap[param])
		}
	}
}

func (c *Config) AddLoader(loader ConfLoader) {
	c.loaders = append(c.loaders, loader)
}

func (c *Config) reloadAll() {
	for _, loader := range c.loaders {
		key, confStruct := loader.Provide()
		if confStruct == nil {
			continue
		}
		var err error
		if key != "" {
			err = c.UnmarshalKey(key, confStruct)
		} else {
			err = c.Unmarshal(confStruct)
		}
		if err != nil {
			logger.Log.Errorf("pitaya:reload config unmarshal error:%v", err)
			continue
		}
		loader.Reload(key, confStruct)
	}
}

func (c *Config) Reload(key string, confStruct interface{}) {
	// cm:=confStruct.(*confMap)
}
func (c *Config) Provide() (key string, confStruct interface{}) {
	return "pitaya.config", c.confconf
}

type confMap struct {
	FilePath  []string      // 配置文件路径,不为空表明使用本地文件配置
	EtcdAddr  string        // Etcd地址,不为空表明使用远程etcd配置
	EtcdKeys  []string      // 要读取监听的etcd key列表
	Interval  time.Duration // 重载间隔
	Formatter string        // 配置格式 必须为 viper.SupportedRemoteProviders
}

// InitLoad 初始化加载本地或远程配置.业务层创建App前调用,仅允许一次
//  @receiver c
func (c *Config) InitLoad() error {
	cm := c.confconf
	err := c.UnmarshalKey("pitaya.conf", cm)
	if err != nil {
		return err
	}
	if len(cm.FilePath) > 0 {
		for _, fp := range cm.FilePath {
			c.config.AddConfigPath(fp)
		}
		c.config.ReadInConfig()
	}
	if len(cm.EtcdKeys) > 0 && cm.EtcdAddr != "" {
		if cm.Formatter != "" {
			c.config.SetConfigType(cm.Formatter)
		}
		for _, key := range cm.EtcdKeys {
			c.config.AddRemoteProvider("etcd", cm.EtcdAddr, key)
		}
		c.config.ReadRemoteConfig()
	}
	c.reloadAll()
	return nil
}

// Watch 开启监控
//  @receiver c
//  @return error
func (c *Config) watch() error {
	cm := &confMap{}
	err := c.UnmarshalKey("pitaya.conf", cm)
	if err != nil {
		return err
	}
	if len(cm.FilePath) > 0 {
		c.config.WatchConfig()
	}
	if len(cm.EtcdKeys) > 0 && cm.EtcdAddr != "" {
		err = c.config.WatchRemoteConfigOnChannel()
		if err != nil {
			return err
		}
	}
	// TODO 暂时用loop实现配置重载,后期fork viper修改watchKeyValueConfigOnChannel()添加回调来实现
	co.Go(func() {
		for {
			time.Sleep(c.confconf.Interval)
			c.reloadAll()
		}
	})
	return nil
}

// GetDuration returns a duration from the inner config
//  @receiver c
//  @param s finally call time.ParseDuration
//  @return time.Duration
func (c *Config) GetDuration(s string) time.Duration {
	return c.config.GetDuration(s)
}

// GetString returns a string from the inner config
func (c *Config) GetString(s string) string {
	return c.config.GetString(s)
}

// GetInt returns an int from the inner config
func (c *Config) GetInt(s string) int {
	return c.config.GetInt(s)
}

// GetBool returns an boolean from the inner config
func (c *Config) GetBool(s string) bool {
	return c.config.GetBool(s)
}

// GetStringSlice returns a string slice from the inner config
func (c *Config) GetStringSlice(s string) []string {
	return c.config.GetStringSlice(s)
}

// Get returns an interface from the inner config
func (c *Config) Get(s string) interface{} {
	return c.config.Get(s)
}

// GetStringMapString returns a string map string from the inner config
func (c *Config) GetStringMapString(s string) map[string]string {
	return c.config.GetStringMapString(s)
}

// UnmarshalKey unmarshals key into v
func (c *Config) UnmarshalKey(s string, v interface{}) error {
	return c.config.UnmarshalKey(s, v)
}

// Unmarshal unmarshals config into v
func (c *Config) Unmarshal(v interface{}) error {
	return c.config.Unmarshal(v)
}

type _ConfigModule struct {
	core *Config
}

func NewConfigModule(core *Config) *_ConfigModule {
	return &_ConfigModule{core: core}
}

func (c *_ConfigModule) Init() error {
	c.core.reloadAll()
	c.core.watch()
	return nil
}
func (c *_ConfigModule) AfterInit() {
}
func (c *_ConfigModule) BeforeShutdown() {

}
func (c *_ConfigModule) Shutdown() error {
	return nil
}
