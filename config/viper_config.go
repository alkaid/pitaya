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
	"context"
	"github.com/topfreegames/pitaya/v2/util/viperx"
	"strings"
	"time"

	"github.com/topfreegames/pitaya/v2/util"

	errors2 "github.com/alkaid/goerrors/errors"
	"go.uber.org/zap"

	"github.com/spf13/viper"
	_ "github.com/spf13/viper/remote"
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
//
//	@implement ConfLoader
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
	config    *viperx.Viperx
	loaders   []ConfLoader
	pitayaAll *PitayaAll
	inited    bool
}

// NewConfig creates a new config with a given viper config if given
func NewConfig(cfgs ...*viper.Viper) *Config {
	var cfg *viperx.Viperx
	if len(cfgs) > 0 {
		cfg = &viperx.Viperx{Viper: cfgs[0]}
	} else {
		cfg = viperx.NewViperx()
	}

	cfg.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	cfg.AutomaticEnv()
	c := &Config{
		config:    cfg,
		pitayaAll: &PitayaAll{},
	}
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
		"pitaya.cluster.rpc.server.nats.requesttimeout":         natsRPCServerConfig.RequestTimeout,
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
		"pitaya.cluster.sd.etcd.election.enable":                etcdSDConfig.Election.Enable,
		"pitaya.cluster.sd.etcd.election.name":                  etcdSDConfig.Election.Name,
		// the sum of this config among all the frontend servers should always be less than
		// the sum of pitaya.buffer.cluster.rpc.server.nats.messages, for covering the worst case scenario
		// a single backend server should have the config pitaya.buffer.cluster.rpc.server.nats.messages bigger
		// than the sum of the config pitaya.concurrency.handler.dispatch among all frontend servers
		"pitaya.acceptor.proxyprotocol":                    pitayaConfig.Acceptor.ProxyProtocol,
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
		"pitaya.log.development":        pitayaConfig.Log.Development,
		"pitaya.log.level":              pitayaConfig.Log.Level,
		// 配置的来源,比较特殊 由上层提供默认值
		// "pitaya.confsource.filepath":         pitayaConfig.ConfSource.FilePath,
		// "pitaya.confsource.etcd.endpoints":   pitayaConfig.ConfSource.Etcd.Endpoints,
		// "pitaya.confsource.etcd.keys":        pitayaConfig.ConfSource.Etcd.Keys,
		// "pitaya.confsource.etcd.dialtimeout": pitayaConfig.ConfSource.Etcd.DialTimeout,
		// "pitaya.confsource.formatter":        pitayaConfig.ConfSource.Formatter,
	}

	for param := range defaultsMap {
		if c.config.Get(param) == nil {
			c.config.SetDefault(param, defaultsMap[param])
		}
	}
}

// PitayaAll 获取框架配置
//
//	@receiver c
//	@return PitayaAll
func (c *Config) PitayaAll() PitayaAll {
	return *c.pitayaAll
}
func (c *Config) Viper() *viperx.Viperx {
	return c.config
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
		// TODO 这里可以优化 已经反序列化过的类型可以缓存起来
		if key != "" {
			err = c.UnmarshalKey(key, confStruct)
		} else {
			err = c.Unmarshal(confStruct)
		}
		if err != nil {
			logger.Zap.Error("pitaya:reload config unmarshal error", zap.Error(err))
			continue
		}
		loader.Reload(key, confStruct)
	}
}

func (c *Config) Reload(key string, confStruct interface{}) {
	// cm:=confStruct.(*confMap)
}
func (c *Config) Provide() (key string, confStruct interface{}) {
	return "pitaya", c.pitayaAll
}

// InitLoad 初始化加载本地或远程配置.业务层创建App前调用,仅允许一次
//
//	@receiver c
func (c *Config) InitLoad() error {
	if c.inited {
		return errors2.NewWithStack("config already inited")
	}
	c.inited = true
	err := c.UnmarshalKey("pitaya", c.pitayaAll)
	if err != nil {
		return err
	}
	cnf := c.pitayaAll.ConfSource
	if len(cnf.FilePath) > 0 {
		for _, fp := range cnf.FilePath {
			c.config.AddConfigPath(fp)
		}
		err := c.config.ReadInConfig()
		if err != nil {
			return errors2.WithStack(err)
		}
	}
	if len(cnf.Etcd.Keys) > 0 && len(cnf.Etcd.Endpoints) > 0 {
		if cnf.Formatter != "" {
			c.config.SetConfigType(cnf.Formatter)
		}
		for _, key := range cnf.Etcd.Keys {
			err := c.config.AddRemoteProviderCluster("etcd3", cnf.Etcd.Endpoints, key)
			if err != nil {
				return errors2.WithStack(err)
			}
		}
		err = c.config.ReadRemoteConfigWithMerged(true)
		if err != nil {
			return errors2.WithStack(err)
		}
	}
	c.reloadAll()
	return nil
}

// Watch 开启监控
//
//	@receiver c
//	@return error
func (c *Config) watch() error {
	cnf := &ConfSource{}
	err := c.UnmarshalKey("pitaya.confsource", cnf)
	if err != nil {
		return err
	}
	// if len(cnf.FilePath) > 0 {
	// 	c.config.WatchConfig()
	// }
	watchChan := make(chan *viper.RemoteResponse, 20)
	if len(cnf.Etcd.Keys) > 0 && len(cnf.Etcd.Endpoints) > 0 {
		err = c.config.WatchRemoteConfigWithChannel(context.Background(), watchChan, true)
		if err != nil {
			return err
		}
	}
	go func() {
		for {
			w, ok := <-watchChan
			if !ok || w.Error != nil {
				time.Sleep(10 * time.Second)
				c.watch()
				return
			}
			util.SafeCall(nil, func() {
				c.reloadAll()
			})
		}
	}()
	return nil
}

// GetDuration returns a duration from the inner config
//
//	@receiver c
//	@param s finally call time.ParseDuration
//	@return time.Duration
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
	return nil
}
func (c *_ConfigModule) AfterInit() {
	c.core.reloadAll()
	c.core.watch()
}
func (c *_ConfigModule) BeforeShutdown() {

}
func (c *_ConfigModule) Shutdown() error {
	return nil
}
