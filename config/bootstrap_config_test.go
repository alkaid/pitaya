package config

import (
	"testing"
	"time"

	"github.com/alkaid/viper"
	"github.com/stretchr/testify/require"
)

// Startup flags are defaults in poker. A partial remote pitaya subtree must
// retain the values decoded before loading the remote document.
func TestPitayaConfigPreservesBootstrapAndDefaults(t *testing.T) {
	// TestMain's heartbeat override would mask the entire remote pitaya subtree.
	t.Setenv("PITAYA_HEARTBEAT_INTERVAL", "")
	for _, document := range []string{
		`{"server":{"type":"admin"}}`,
		`{"server":{"type":"admin"},"pitaya":{"log":{"level":"debug"}}}`,
		`{"pitaya":{"confsource":{"interval":"9m"}}}`,
	} {
		t.Run(document, func(t *testing.T) {
			previous := viper.RemoteConfig
			fixture := &configRemoteFixture{values: map[string][]byte{"/app": []byte(document)}}
			viper.RemoteConfig = fixture
			t.Cleanup(func() { viper.RemoteConfig = previous })
			vp := viper.New()
			vp.SetDefault("pitaya.confsource.etcd.endpoints", []string{"http://config-etcd.invalid:2379"})
			vp.SetDefault("pitaya.confsource.etcd.keys", []string{"/app"})
			vp.SetDefault("pitaya.confsource.etcd.dialtimeout", 5*time.Second)
			vp.SetDefault("pitaya.confsource.formatter", "json")
			c := NewConfig(vp)
			var before PitayaConfig
			require.NoError(t, c.UnmarshalKey("pitaya", &before))
			require.NoError(t, c.InitLoad())
			got := c.PitayaConfig()
			require.Equal(t, before.ConfSource.Etcd, got.ConfSource.Etcd)
			require.Equal(t, "json", got.ConfSource.Formatter)
			require.Equal(t, before.Heartbeat, got.Heartbeat)
			require.Equal(t, before.Cluster.SD, got.Cluster.SD)
			if document == `{"pitaya":{"confsource":{"interval":"9m"}}}` {
				require.Equal(t, 9*time.Minute, got.ConfSource.Interval)
			}
		})
	}
}

// Reload carries forward omitted fields, while explicit values still update
// them. Neither a successful nor a failed reload may mutate an older snapshot.
func TestPitayaConfigReloadPreservesValuesAndSnapshots(t *testing.T) {
	t.Setenv("PITAYA_HEARTBEAT_INTERVAL", "")
	c := NewConfig()
	c.Set("pitaya", map[string]interface{}{
		"heartbeat": map[string]interface{}{"interval": "17s"},
		"metrics":   map[string]interface{}{"constlabels": map[string]string{"host.example.com": "enabled"}},
		"gopools":   map[string]interface{}{"room.pool": map[string]string{"name": "room.pool"}},
	})
	require.NoError(t, c.InitLoad())
	initial := c.PitayaConfig()
	require.Equal(t, 17*time.Second, initial.Heartbeat.Interval)
	require.Equal(t, "room.pool", initial.GoPools["room.pool"].Name)
	c.Set("pitaya", map[string]interface{}{"log": map[string]string{"level": "warn"}})
	c.reloadAll()
	got := c.PitayaConfig()
	require.Equal(t, initial.Heartbeat, got.Heartbeat)
	require.Equal(t, initial.Metrics.ConstLabels, got.Metrics.ConstLabels)
	require.Equal(t, initial.GoPools, got.GoPools)
	c.Set("pitaya", map[string]interface{}{
		"heartbeat": map[string]string{"interval": "0s"},
		"metrics":   map[string]interface{}{"constlabels": map[string]string{"host.example.com": "updated"}},
	})
	c.reloadAll()
	valid := c.PitayaConfig()
	require.Zero(t, valid.Heartbeat.Interval)
	require.Equal(t, "updated", valid.Metrics.ConstLabels["host.example.com"])
	require.Equal(t, "enabled", initial.Metrics.ConstLabels["host.example.com"])
	c.Set("pitaya", map[string]interface{}{
		"heartbeat": map[string]string{"interval": "invalid"},
		"metrics":   map[string]interface{}{"constlabels": map[string]string{"host.example.com": "invalid"}},
	})
	c.reloadAll()
	require.Equal(t, valid, c.PitayaConfig())
	require.Equal(t, "updated", valid.Metrics.ConstLabels["host.example.com"])
}
