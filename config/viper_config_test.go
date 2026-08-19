package config

import (
	"testing"
	"time"

	"github.com/alkaid/viper"
	"github.com/stretchr/testify/assert"
)

func TestConfigRegistersRPCIdempotencyDefaults(t *testing.T) {
	conf := NewConfig()
	defaults := map[string]any{
		"pitaya.cluster.rpc.idempotency.enabled":           true,
		"pitaya.cluster.rpc.idempotency.keyprefix":         "pitaya:rpc:idempotency:",
		"pitaya.cluster.rpc.idempotency.resultttl":         15 * time.Minute,
		"pitaya.cluster.rpc.idempotency.leasettl":          30 * time.Second,
		"pitaya.cluster.rpc.idempotency.heartbeatinterval": 10 * time.Second,
		"pitaya.cluster.rpc.idempotency.pollinterval":      25 * time.Millisecond,
		"pitaya.cluster.rpc.idempotency.operationtimeout":  3 * time.Second,
	}

	for path, want := range defaults {
		t.Run(path, func(t *testing.T) {
			assert.True(t, conf.IsSet(path))
			assert.Equal(t, want, conf.Get(path))
		})
	}
}

func TestNewPitayaConfigReadsRPCIdempotencyOverrides(t *testing.T) {
	v := viper.New()
	v.Set("pitaya.cluster.rpc.idempotency.enabled", false)
	v.Set("pitaya.cluster.rpc.idempotency.keyprefix", "tenant:commands:")
	v.Set("pitaya.cluster.rpc.idempotency.resultttl", 21*time.Minute)
	v.Set("pitaya.cluster.rpc.idempotency.leasettl", 45*time.Second)
	v.Set("pitaya.cluster.rpc.idempotency.heartbeatinterval", 12*time.Second)
	v.Set("pitaya.cluster.rpc.idempotency.pollinterval", 40*time.Millisecond)
	v.Set("pitaya.cluster.rpc.idempotency.operationtimeout", 4*time.Second)

	got := NewPitayaConfig(NewConfig(v)).Cluster.RPC.Idempotency
	assert.Equal(t, RPCIdempotencyConfig{
		Enabled:           false,
		KeyPrefix:         "tenant:commands:",
		ResultTTL:         21 * time.Minute,
		LeaseTTL:          45 * time.Second,
		HeartbeatInterval: 12 * time.Second,
		PollInterval:      40 * time.Millisecond,
		OperationTimeout:  4 * time.Second,
	}, got)
}
