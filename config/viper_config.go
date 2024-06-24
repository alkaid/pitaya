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
	viperx.Viperx
	loaders      []ConfLoader
	pitayaConfig *PitayaConfig
	inited       bool
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
		Viperx:       *cfg,
		pitayaConfig: &PitayaConfig{},
	}
	c.fillDefaultValues()
	c.AddLoader(c)
	return c
}

func (c *Config) fillDefaultValues() {
	pitayaConfig := NewDefaultPitayaConfig()

	defaultsMap := map[string]interface{}{
		"pitaya.serializertype":        pitayaConfig.SerializerType,
		"pitaya.buffer.agent.messages": pitayaConfig.Buffer.Agent.Messages,
		// the max buffer size that nats will accept, if this buffer overflows, messages will begin to be dropped
		"pitaya.buffer.handler.localprocess":                    pitayaConfig.Buffer.Handler.LocalProcess,
		"pitaya.buffer.handler.remoteprocess":                   pitayaConfig.Buffer.Handler.RemoteProcess,
		"pitaya.cluster.info.region":                            pitayaConfig.Cluster.Info.Region,
		"pitaya.cluster.rpc.client.grpc.dialtimeout":            pitayaConfig.Cluster.RPC.Client.Grpc.DialTimeout,
		"pitaya.cluster.rpc.client.grpc.requesttimeout":         pitayaConfig.Cluster.RPC.Client.Grpc.RequestTimeout,
		"pitaya.cluster.rpc.client.grpc.lazyconnection":         pitayaConfig.Cluster.RPC.Client.Grpc.LazyConnection,
		"pitaya.cluster.rpc.client.nats.connect":                pitayaConfig.Cluster.RPC.Client.Nats.Connect,
		"pitaya.cluster.rpc.client.nats.connectiontimeout":      pitayaConfig.Cluster.RPC.Client.Nats.ConnectionTimeout,
		"pitaya.cluster.rpc.client.nats.maxreconnectionretries": pitayaConfig.Cluster.RPC.Client.Nats.MaxReconnectionRetries,
		"pitaya.cluster.rpc.client.nats.requesttimeout":         pitayaConfig.Cluster.RPC.Client.Nats.RequestTimeout,
		"pitaya.cluster.rpc.server.grpc.port":                   pitayaConfig.Cluster.RPC.Server.Grpc.Port,
		"pitaya.cluster.rpc.server.nats.connect":                pitayaConfig.Cluster.RPC.Server.Nats.Connect,
		"pitaya.cluster.rpc.server.nats.connectiontimeout":      pitayaConfig.Cluster.RPC.Server.Nats.ConnectionTimeout,
		"pitaya.cluster.rpc.server.nats.maxreconnectionretries": pitayaConfig.Cluster.RPC.Server.Nats.MaxReconnectionRetries,
		"pitaya.cluster.rpc.server.nats.services":               pitayaConfig.Cluster.RPC.Server.Nats.Services,
		"pitaya.cluster.rpc.server.nats.buffer.messages":        pitayaConfig.Cluster.RPC.Server.Nats.Buffer.Messages,
		"pitaya.cluster.rpc.server.nats.buffer.push":            pitayaConfig.Cluster.RPC.Server.Nats.Buffer.Push,
		"pitaya.cluster.sd.etcd.dialtimeout":                    pitayaConfig.Cluster.SD.Etcd.DialTimeout,
		"pitaya.cluster.sd.etcd.endpoints":                      pitayaConfig.Cluster.SD.Etcd.Endpoints,
		"pitaya.cluster.sd.etcd.prefix":                         pitayaConfig.Cluster.SD.Etcd.Prefix,
		"pitaya.cluster.sd.etcd.grantlease.maxretries":          pitayaConfig.Cluster.SD.Etcd.GrantLease.MaxRetries,
		"pitaya.cluster.sd.etcd.grantlease.retryinterval":       pitayaConfig.Cluster.SD.Etcd.GrantLease.RetryInterval,
		"pitaya.cluster.sd.etcd.grantlease.timeout":             pitayaConfig.Cluster.SD.Etcd.GrantLease.Timeout,
		"pitaya.cluster.sd.etcd.heartbeat.log":                  pitayaConfig.Cluster.SD.Etcd.Heartbeat.Log,
		"pitaya.cluster.sd.etcd.heartbeat.ttl":                  pitayaConfig.Cluster.SD.Etcd.Heartbeat.TTL,
		"pitaya.cluster.sd.etcd.revoke.timeout":                 pitayaConfig.Cluster.SD.Etcd.Revoke.Timeout,
		"pitaya.cluster.sd.etcd.syncservers.interval":           pitayaConfig.Cluster.SD.Etcd.SyncServers.Interval,
		"pitaya.cluster.sd.etcd.syncservers.parallelism":        pitayaConfig.Cluster.SD.Etcd.SyncServers.Parallelism,
		"pitaya.cluster.sd.etcd.shutdown.delay":                 pitayaConfig.Cluster.SD.Etcd.Shutdown.Delay,
		"pitaya.cluster.sd.etcd.servertypeblacklist":            pitayaConfig.Cluster.SD.Etcd.ServerTypesBlacklist,
		// the sum of this config among all the frontend servers should always be less than
		// the sum of pitaya.buffer.cluster.rpc.server.nats.messages, for covering the worst case scenario
		// a single backend server should have the config pitaya.buffer.cluster.rpc.server.nats.messages bigger
		// than the sum of the config pitaya.concurrency.handler.dispatch among all frontend servers
		"pitaya.acceptor.proxyprotocol":                    pitayaConfig.Acceptor.ProxyProtocol,
		"pitaya.concurrency.handler.dispatch":              pitayaConfig.Concurrency.Handler.Dispatch,
		"pitaya.defaultpipelines.structvalidation.enabled": pitayaConfig.DefaultPipelines.StructValidation.Enabled,
		"pitaya.groups.etcd.dialtimeout":                   pitayaConfig.Groups.Etcd.DialTimeout,
		"pitaya.groups.etcd.endpoints":                     pitayaConfig.Groups.Etcd.Endpoints,
		"pitaya.groups.etcd.prefix":                        pitayaConfig.Groups.Etcd.Prefix,
		"pitaya.groups.etcd.transactiontimeout":            pitayaConfig.Groups.Etcd.TransactionTimeout,
		"pitaya.groups.memory.tickduration":                pitayaConfig.Groups.Memory.TickDuration,
		"pitaya.handler.messages.compression":              pitayaConfig.Handler.Messages.Compression,
		"pitaya.heartbeat.interval":                        pitayaConfig.Heartbeat.Interval,
		"pitaya.metrics.additionalLabels":                  pitayaConfig.Metrics.AdditionalLabels,
		"pitaya.metrics.constLabels":                       pitayaConfig.Metrics.ConstLabels,
		"pitaya.metrics.custom":                            pitayaConfig.Metrics.Custom,
		"pitaya.metrics.period":                            pitayaConfig.Metrics.Period,
		"pitaya.metrics.prometheus.enabled":                pitayaConfig.Metrics.Prometheus.Enabled,
		"pitaya.metrics.prometheus.port":                   pitayaConfig.Metrics.Prometheus.Port,
		"pitaya.metrics.statsd.enabled":                    pitayaConfig.Metrics.Statsd.Enabled,
		"pitaya.metrics.statsd.host":                       pitayaConfig.Metrics.Statsd.Host,
		"pitaya.metrics.statsd.prefix":                     pitayaConfig.Metrics.Statsd.Prefix,
		"pitaya.metrics.statsd.rate":                       pitayaConfig.Metrics.Statsd.Rate,
		"pitaya.modules.bindingstorage.etcd.dialtimeout":   pitayaConfig.Modules.BindingStorage.Etcd.DialTimeout,
		"pitaya.modules.bindingstorage.etcd.endpoints":     pitayaConfig.Modules.BindingStorage.Etcd.Endpoints,
		"pitaya.modules.bindingstorage.etcd.leasettl":      pitayaConfig.Modules.BindingStorage.Etcd.LeaseTTL,
		"pitaya.modules.bindingstorage.etcd.prefix":        pitayaConfig.Modules.BindingStorage.Etcd.Prefix,
		"pitaya.conn.ratelimiting.limit":                   pitayaConfig.Conn.RateLimiting.Limit,
		"pitaya.conn.ratelimiting.interval":                pitayaConfig.Conn.RateLimiting.Interval,
		"pitaya.conn.ratelimiting.forcedisable":            pitayaConfig.Conn.RateLimiting.ForceDisable,
		"pitaya.session.unique":                            pitayaConfig.Session.Unique,
		"pitaya.session.drain.enabled":                     pitayaConfig.Session.Drain.Enabled,
		"pitaya.session.drain.timeout":                     pitayaConfig.Session.Drain.Timeout,
		"pitaya.session.drain.period":                      pitayaConfig.Session.Drain.Period,
		"pitaya.worker.concurrency":                        pitayaConfig.Worker.Concurrency,
		"pitaya.worker.redis.pool":                         pitayaConfig.Worker.Redis.Pool,
		"pitaya.worker.redis.url":                          pitayaConfig.Worker.Redis.ServerURL,
		"pitaya.worker.retry.enabled":                      pitayaConfig.Worker.Retry.Enabled,
		"pitaya.worker.retry.exponential":                  pitayaConfig.Worker.Retry.Exponential,
		"pitaya.worker.retry.max":                          pitayaConfig.Worker.Retry.Max,
		"pitaya.worker.retry.maxDelay":                     pitayaConfig.Worker.Retry.MaxDelay,
		"pitaya.worker.retry.maxRandom":                    pitayaConfig.Worker.Retry.MaxRandom,
		"pitaya.worker.retry.minDelay":                     pitayaConfig.Worker.Retry.MinDelay,

		"pitaya.cluster.rpc.server.nats.requesttimeout": pitayaConfig.Cluster.RPC.Server.Nats.RequestTimeout,
		"pitaya.cluster.sd.etcd.election.enable":        pitayaConfig.Cluster.SD.Etcd.Election.Enable,
		"pitaya.cluster.sd.etcd.election.name":          pitayaConfig.Cluster.SD.Etcd.Election.Name,
		"pitaya.session.cachettl":                       pitayaConfig.Session.CacheTTL,
		"pitaya.storage.redis.addrs":                    pitayaConfig.Storage.Redis.Addrs,
		"pitaya.storage.redis.username":                 pitayaConfig.Storage.Redis.Username,
		"pitaya.storage.redis.password":                 pitayaConfig.Storage.Redis.Password,
		"pitaya.log.development":                        pitayaConfig.Log.Development,
		"pitaya.log.level":                              pitayaConfig.Log.Level,
		// 配置的来源,比较特殊 由上层提供默认值
		// "pitaya.confsource.filepath":         pitayaConfig.ConfSource.FilePath,
		// "pitaya.confsource.etcd.endpoints":   pitayaConfig.ConfSource.Etcd.Endpoints,
		// "pitaya.confsource.etcd.keys":        pitayaConfig.ConfSource.Etcd.Keys,
		// "pitaya.confsource.etcd.dialtimeout": pitayaConfig.ConfSource.Etcd.DialTimeout,
		// "pitaya.confsource.formatter":        pitayaConfig.ConfSource.Formatter,
	}

	for param := range defaultsMap {
		val := c.Get(param)
		if val == nil {
			c.SetDefault(param, defaultsMap[param])
		} else {
			c.SetDefault(param, val)
			c.Set(param, val)
		}

	}
}

// PitayaConfig 获取框架配置
//
//	@receiver c
//	@return PitayaConfig
func (c *Config) PitayaConfig() PitayaConfig {
	return *c.pitayaConfig
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
	return "pitaya", c.pitayaConfig
}

// InitLoad 初始化加载本地或远程配置.业务层创建App前调用,仅允许一次
//
//	@receiver c
func (c *Config) InitLoad() error {
	if c.inited {
		return errors2.NewWithStack("config already inited")
	}
	c.inited = true
	err := c.UnmarshalKey("pitaya", c.pitayaConfig)
	if err != nil {
		return err
	}
	cnf := c.pitayaConfig.ConfSource
	if len(cnf.FilePath) > 0 {
		for _, fp := range cnf.FilePath {
			c.AddConfigPath(fp)
		}
		err := c.ReadInConfig()
		if err != nil {
			return errors2.WithStack(err)
		}
	}
	if len(cnf.Etcd.Keys) > 0 && len(cnf.Etcd.Endpoints) > 0 {
		if cnf.Formatter != "" {
			c.SetConfigType(cnf.Formatter)
		}
		for _, key := range cnf.Etcd.Keys {
			err := c.AddRemoteProviderCluster("etcd3", cnf.Etcd.Endpoints, key)
			if err != nil {
				return errors2.WithStack(err)
			}
		}
		err = c.ReadRemoteConfigWithMerged(true)
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
		err = c.WatchRemoteConfigWithChannel(context.Background(), watchChan, true)
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
