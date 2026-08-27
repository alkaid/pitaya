package config

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultPitayaConfigIsValid(t *testing.T) {
	cfg := NewDefaultPitayaConfig()
	require.NoError(t, cfg.Validate())
	require.NoError(t, cfg.ValidateCluster())
}

func TestDefaultRPCIdempotencyConfig(t *testing.T) {
	got := NewDefaultPitayaConfig().Cluster.RPC.Idempotency
	assert.Equal(t, RPCIdempotencyConfig{
		Enabled:           true,
		KeyPrefix:         "pitaya:rpc:idempotency:",
		ResultTTL:         15 * time.Minute,
		LeaseTTL:          30 * time.Second,
		HeartbeatInterval: 10 * time.Second,
		PollInterval:      25 * time.Millisecond,
		OperationTimeout:  3 * time.Second,
	}, got)
}

func TestPitayaConfigValidateClusterRejectsInvalidRPCIdempotencyConfig(t *testing.T) {
	t.Run("empty key prefix", func(t *testing.T) {
		cfg := NewDefaultPitayaConfig()
		cfg.Cluster.RPC.Idempotency.KeyPrefix = " \t"

		err := cfg.ValidateCluster()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pitaya.cluster.rpc.idempotency.keyprefix")
	})

	durationFields := []struct {
		name string
		path string
		set  func(*RPCIdempotencyConfig, time.Duration)
	}{
		{name: "result TTL", path: "resultttl", set: func(cfg *RPCIdempotencyConfig, value time.Duration) { cfg.ResultTTL = value }},
		{name: "lease TTL", path: "leasettl", set: func(cfg *RPCIdempotencyConfig, value time.Duration) { cfg.LeaseTTL = value }},
		{name: "heartbeat interval", path: "heartbeatinterval", set: func(cfg *RPCIdempotencyConfig, value time.Duration) { cfg.HeartbeatInterval = value }},
		{name: "poll interval", path: "pollinterval", set: func(cfg *RPCIdempotencyConfig, value time.Duration) { cfg.PollInterval = value }},
		{name: "operation timeout", path: "operationtimeout", set: func(cfg *RPCIdempotencyConfig, value time.Duration) { cfg.OperationTimeout = value }},
	}
	invalidValues := []struct {
		name  string
		value time.Duration
	}{
		{name: "zero", value: 0},
		{name: "negative", value: -time.Nanosecond},
	}
	for _, field := range durationFields {
		for _, invalid := range invalidValues {
			t.Run(field.name+"/"+invalid.name, func(t *testing.T) {
				cfg := NewDefaultPitayaConfig()
				field.set(&cfg.Cluster.RPC.Idempotency, invalid.value)

				err := cfg.ValidateCluster()
				require.Error(t, err)
				assert.Contains(t, err.Error(), "pitaya.cluster.rpc.idempotency."+field.path)
			})
		}
	}
}

func TestPitayaConfigValidateClusterEnforcesRPCIdempotencyLeaseMargin(t *testing.T) {
	t.Run("equal boundary is rejected", func(t *testing.T) {
		cfg := NewDefaultPitayaConfig()
		cfg.Cluster.RPC.Idempotency.HeartbeatInterval = 10 * time.Second
		cfg.Cluster.RPC.Idempotency.OperationTimeout = 5 * time.Second
		cfg.Cluster.RPC.Idempotency.LeaseTTL = 15 * time.Second

		err := cfg.ValidateCluster()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be less than")
	})

	t.Run("strictly below boundary is accepted", func(t *testing.T) {
		cfg := NewDefaultPitayaConfig()
		cfg.Cluster.RPC.Idempotency.HeartbeatInterval = 10 * time.Second
		cfg.Cluster.RPC.Idempotency.OperationTimeout = 5 * time.Second
		cfg.Cluster.RPC.Idempotency.LeaseTTL = 15*time.Second + time.Nanosecond

		require.NoError(t, cfg.ValidateCluster())
	})
}

func TestPitayaConfigValidateClusterIgnoresDisabledRPCIdempotencyConfig(t *testing.T) {
	cfg := NewDefaultPitayaConfig()
	cfg.Cluster.RPC.Idempotency = RPCIdempotencyConfig{Enabled: false}

	require.NoError(t, cfg.ValidateCluster())
}

func TestPitayaConfigValidateRejectsUnsafeRuntimeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PitayaConfig)
		want   string
	}{
		{
			name:   "metrics period",
			mutate: func(cfg *PitayaConfig) { cfg.Metrics.Period = 0 },
			want:   "pitaya.metrics.period",
		},
		{
			name:   "agent buffer",
			mutate: func(cfg *PitayaConfig) { cfg.Buffer.Agent.Messages = -1 },
			want:   "pitaya.buffer.agent.messages",
		},
		{
			name: "session drain period",
			mutate: func(cfg *PitayaConfig) {
				cfg.Session.Drain.Enabled = true
				cfg.Session.Drain.Period = 0
			},
			want: "pitaya.session.drain.period",
		},
		{
			name: "prometheus port",
			mutate: func(cfg *PitayaConfig) {
				cfg.Metrics.Prometheus.Enabled = true
				cfg.Metrics.Prometheus.Port = 0
			},
			want: "pitaya.metrics.prometheus.port",
		},
		{
			name: "statsd NaN rate",
			mutate: func(cfg *PitayaConfig) {
				cfg.Metrics.Statsd.Enabled = true
				cfg.Metrics.Statsd.Rate = math.NaN()
			},
			want: "pitaya.metrics.statsd.rate",
		},
		{
			name: "statsd infinite rate",
			mutate: func(cfg *PitayaConfig) {
				cfg.Metrics.Statsd.Enabled = true
				cfg.Metrics.Statsd.Rate = math.Inf(1)
			},
			want: "pitaya.metrics.statsd.rate",
		},
		{
			name:   "redis addresses",
			mutate: func(cfg *PitayaConfig) { cfg.Storage.Redis.Addrs = nil },
			want:   "pitaya.storage.redis.addrs",
		},
		{
			name:   "worker pool",
			mutate: func(cfg *PitayaConfig) { cfg.Worker.Redis.Pool = "zero" },
			want:   "pitaya.worker.redis.pool",
		},
		{
			name: "stateful pool duration",
			mutate: func(cfg *PitayaConfig) {
				cfg.GoPools["custom"] = GoPool{Expire: -time.Second}
			},
			want: "pitaya.gopools.custom.expire",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := NewDefaultPitayaConfig()
			test.mutate(cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestPitayaConfigValidateIgnoresDeprecatedHandlerQueueSettings(t *testing.T) {
	tables := []struct {
		name   string
		mutate func(*PitayaConfig)
	}{
		{name: "local process", mutate: func(cfg *PitayaConfig) { cfg.Buffer.Handler.LocalProcess = -1 }},
		{name: "remote process", mutate: func(cfg *PitayaConfig) { cfg.Buffer.Handler.RemoteProcess = -1 }},
		{name: "dispatch zero", mutate: func(cfg *PitayaConfig) { cfg.Concurrency.Handler.Dispatch = 0 }},
		{name: "dispatch negative", mutate: func(cfg *PitayaConfig) { cfg.Concurrency.Handler.Dispatch = -1000 }},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			cfg := NewDefaultPitayaConfig()
			table.mutate(cfg)

			require.NoError(t, cfg.Validate())
		})
	}
}

func TestPitayaConfigValidateClusterRejectsUnsafeRuntimeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PitayaConfig)
		want   string
	}{
		{
			name:   "nats client connect",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Client.Nats.Connect = " \t" },
			want:   "pitaya.cluster.rpc.client.nats.connect",
		},
		{
			name:   "nats client connection timeout",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Client.Nats.ConnectionTimeout = 0 },
			want:   "pitaya.cluster.rpc.client.nats.connectiontimeout",
		},
		{
			name:   "nats client reconnect retries",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Client.Nats.MaxReconnectionRetries = -2 },
			want:   "pitaya.cluster.rpc.client.nats.maxreconnectionretries",
		},
		{
			name:   "zero services",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Server.Nats.Services = 0 },
			want:   "pitaya.cluster.rpc.server.nats.services",
		},
		{
			name:   "parallel services",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Server.Nats.Services = 2 },
			want:   "must be 1 to preserve ordered dispatch",
		},
		{
			name:   "message buffer",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Server.Nats.Buffer.Messages = -1 },
			want:   "pitaya.cluster.rpc.server.nats.buffer.messages",
		},
		{
			name:   "push buffer",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Server.Nats.Buffer.Push = 0 },
			want:   "pitaya.cluster.rpc.server.nats.buffer.push",
		},
		{
			name:   "request timeout",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Client.Nats.RequestTimeout = 0 },
			want:   "pitaya.cluster.rpc.client.nats.requesttimeout",
		},
		{
			name:   "nats server connect",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Server.Nats.Connect = "" },
			want:   "pitaya.cluster.rpc.server.nats.connect",
		},
		{
			name:   "nats server connection timeout",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Server.Nats.ConnectionTimeout = -time.Second },
			want:   "pitaya.cluster.rpc.server.nats.connectiontimeout",
		},
		{
			name:   "nats server request timeout",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Server.Nats.RequestTimeout = 0 },
			want:   "pitaya.cluster.rpc.server.nats.requesttimeout",
		},
		{
			name:   "nats server reconnect retries",
			mutate: func(cfg *PitayaConfig) { cfg.Cluster.RPC.Server.Nats.MaxReconnectionRetries = -2 },
			want:   "pitaya.cluster.rpc.server.nats.maxreconnectionretries",
		},
		{
			name: "partial etcd credentials",
			mutate: func(cfg *PitayaConfig) {
				cfg.Cluster.SD.Etcd.User = "pitaya"
				cfg.Cluster.SD.Etcd.Pass = ""
			},
			want: "user and pass must be configured together",
		},
		{
			name: "election auth context",
			mutate: func(cfg *PitayaConfig) {
				cfg.Cluster.SD.Etcd.Election.Enable = true
				cfg.Cluster.SD.Etcd.Election.Name = ""
			},
			want: "pitaya.cluster.sd.etcd.election.name",
		},
		{
			name: "sub-second heartbeat TTL",
			mutate: func(cfg *PitayaConfig) {
				cfg.Cluster.SD.Etcd.Heartbeat.TTL = 500 * time.Millisecond
			},
			want: "pitaya.cluster.sd.etcd.heartbeat.ttl",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := NewDefaultPitayaConfig()
			test.mutate(cfg)
			err := cfg.ValidateCluster()
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestRPCSubconfigsAreValidByDefault(t *testing.T) {
	rpc := NewDefaultPitayaConfig().Cluster.RPC
	require.NoError(t, rpc.Client.Nats.Validate())
	require.NoError(t, rpc.Server.Nats.Validate())
	require.NoError(t, rpc.Client.Grpc.Validate())
	require.NoError(t, rpc.Server.Grpc.Validate())
}

func TestPitayaConfigValidateClusterIgnoresUnusedGRPCTransport(t *testing.T) {
	cfg := NewDefaultPitayaConfig()
	cfg.Cluster.RPC.Client.Grpc.DialTimeout = 0
	cfg.Cluster.RPC.Client.Grpc.RequestTimeout = 0
	cfg.Cluster.RPC.Server.Grpc.Port = 0

	require.Error(t, cfg.Cluster.RPC.Client.Grpc.Validate())
	require.Error(t, cfg.Cluster.RPC.Server.Grpc.Validate())
	require.NoError(t, cfg.ValidateCluster())
}

func TestPitayaConfigValidationAggregatesErrors(t *testing.T) {
	cfg := NewDefaultPitayaConfig()
	cfg.Metrics.Period = 0
	cfg.Buffer.Agent.Messages = -1

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pitaya.metrics.period")
	assert.Contains(t, err.Error(), "pitaya.buffer.agent.messages")
}

func TestPitayaConfigValidateClusterAllowsZeroLeaseRetries(t *testing.T) {
	cfg := NewDefaultPitayaConfig()
	cfg.Cluster.SD.Etcd.GrantLease.MaxRetries = 0

	require.NoError(t, cfg.ValidateCluster())
}

func TestPitayaConfigValidateClusterShutdownDelay(t *testing.T) {
	for _, table := range []struct {
		name    string
		delay   time.Duration
		wantErr bool
	}{
		{name: "negative", delay: -time.Nanosecond, wantErr: true},
		{name: "zero", delay: 0},
		{name: "default", delay: 300 * time.Millisecond},
		{name: "maximum", delay: 5 * time.Second},
		{name: "above maximum", delay: 5*time.Second + time.Nanosecond, wantErr: true},
	} {
		t.Run(table.name, func(t *testing.T) {
			cfg := NewDefaultPitayaConfig()
			cfg.Cluster.SD.Etcd.Shutdown.Delay = table.delay
			err := cfg.ValidateCluster()
			if table.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "pitaya.cluster.sd.etcd.shutdown.delay")
				return
			}
			require.NoError(t, err)
		})
	}
}
