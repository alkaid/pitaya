package config

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/viper"
	"github.com/stretchr/testify/require"
)

type configRemoteFixture struct {
	mu       sync.Mutex
	values   map[string][]byte
	channels map[string]chan *viper.RemoteResponse
	quits    map[string]chan bool
	failures int
	attempts int
}

func (f *configRemoteFixture) Get(rp viper.RemoteProvider) (io.Reader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.values[rp.Path()]
	if !ok {
		return nil, errors.New("missing key")
	}
	return bytes.NewReader(data), nil
}
func (f *configRemoteFixture) Watch(rp viper.RemoteProvider) (io.Reader, error) { return f.Get(rp) }
func (f *configRemoteFixture) WatchChannel(rp viper.RemoteProvider) (<-chan *viper.RemoteResponse, chan bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.failures > 0 {
		f.failures--
		return nil, nil
	}
	ch := make(chan *viper.RemoteResponse, 20)
	quit := make(chan bool)
	f.channels[rp.Path()] = ch
	f.quits[rp.Path()] = quit
	if value, ok := f.values[rp.Path()]; ok {
		ch <- &viper.RemoteResponse{Value: value}
	}
	return ch, quit
}
func (f *configRemoteFixture) put(key, document string) {
	f.mu.Lock()
	f.values[key] = []byte(document)
	ch := f.channels[key]
	f.mu.Unlock()
	if ch != nil {
		ch <- &viper.RemoteResponse{Value: []byte(document)}
	}
}
func (f *configRemoteFixture) waitReady(t *testing.T, key string, minAttempts int) {
	t.Helper()
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.channels[key] != nil && f.attempts >= minAttempts
	}, 6*time.Second, time.Millisecond)
}

func setupRemoteFixture(t *testing.T, values map[string][]byte) (*Config, *configRemoteFixture) {
	t.Helper()
	old := viper.RemoteConfig
	f := &configRemoteFixture{values: values, channels: map[string]chan *viper.RemoteResponse{}, quits: map[string]chan bool{}}
	viper.RemoteConfig = f
	t.Cleanup(func() { viper.RemoteConfig = old })
	c := NewConfig()
	c.SetConfigType("json")
	keys := []string{"/low"}
	if _, ok := values["/high"]; ok {
		keys = append(keys, "/high")
	}
	c.Set("pitaya.confsource.etcd.keys", keys)
	c.Set("pitaya.confsource.etcd.endpoints", []string{"test"})
	c.Set("pitaya.confsource.formatter", "json")
	t.Cleanup(c.stopWatch)
	return c, f
}

type watchedFeature struct {
	Value, Removed string
	Fields         map[string]string
}

func recordFeature(c *Config) (<-chan watchedFeature, *atomic.Int32) {
	updates := make(chan watchedFeature, 100)
	count := &atomic.Int32{}
	// Reuse the object deliberately: removed fields must not remain in it.
	target := &watchedFeature{}
	c.AddLoader(LoaderFactory{ProvideApply: func() (string, interface{}) { return "feature", target }, ReloadApply: func(_ string, value interface{}) { count.Add(1); updates <- *value.(*watchedFeature) }})
	return updates, count
}
func awaitFeature(t *testing.T, updates <-chan watchedFeature, value string) watchedFeature {
	t.Helper()
	timeout := time.NewTimer(7 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case got := <-updates:
			if got.Value == value {
				return got
			}
		case <-timeout.C:
			t.Fatalf("did not receive feature value %q", value)
			return watchedFeature{}
		}
	}
}

func TestRemoteWatchRetriesInitialAndRepeatedFailures(t *testing.T) {
	c, f := setupRemoteFixture(t, map[string][]byte{"/low": []byte(`{"feature":{"value":"initial"}}`)})
	updates, count := recordFeature(c)
	require.NoError(t, c.InitLoad())
	awaitFeature(t, updates, "initial")
	f.mu.Lock()
	f.failures = 2
	f.mu.Unlock()
	module := NewConfigModule(c)
	module.AfterInit()
	awaitFeature(t, updates, "initial")
	f.waitReady(t, "/low", 3)
	f.put("/low", `{"feature":{"value":"first"}}`)
	awaitFeature(t, updates, "first")
	require.Equal(t, int32(3), count.Load(), "initial snapshot must not duplicate reload")
	f.mu.Lock()
	firstQuit := f.quits["/low"]
	f.failures = 2
	close(f.channels["/low"])
	delete(f.channels, "/low")
	f.values["/low"] = []byte(`{"feature":{"value":"during-recovery"}}`)
	f.mu.Unlock()
	f.waitReady(t, "/low", 6)
	awaitFeature(t, updates, "during-recovery")
	select {
	case <-firstQuit:
	case <-time.After(time.Second):
		t.Fatal("old watcher survived recovery")
	}
	f.put("/low", `{"feature":{"value":"later"}}`)
	awaitFeature(t, updates, "later")
	f.mu.Lock()
	lastQuit := f.quits["/low"]
	f.mu.Unlock()
	require.NoError(t, module.Shutdown())
	require.NoError(t, module.Shutdown())
	select {
	case <-lastQuit:
	case <-time.After(time.Second):
		t.Fatal("watch survived shutdown")
	}
	before := count.Load()
	f.put("/low", `{"feature":{"value":"after-stop"}}`)
	require.Equal(t, before, count.Load())
}

func TestRemoteWatchPriorityInvalidDocumentAndFieldRemoval(t *testing.T) {
	c, f := setupRemoteFixture(t, map[string][]byte{
		"/low":  []byte(`{"feature":{"value":"low","fields":{"low":"value"}}}`),
		"/high": []byte(`{"feature":{"value":"high","removed":"stale","fields":{"high":"value"}}}`),
	})
	updates, _ := recordFeature(c)
	require.NoError(t, c.InitLoad())
	awaitFeature(t, updates, "high")
	NewConfigModule(c).AfterInit()
	awaitFeature(t, updates, "high")
	f.waitReady(t, "/high", 2)
	f.put("/low", `{"feature":{"value":"new-low","fields":{"low":"new"}}}`)
	// Invalid JSON must not replace the high-priority provider's last good value.
	f.put("/high", `{"feature":`)
	f.put("/high", `{"feature":{"value":"valid-again","removed":"stale"}}`)
	awaitFeature(t, updates, "valid-again")
	f.put("/high", `{}`)
	got := awaitFeature(t, updates, "new-low")
	require.Empty(t, got.Removed)
	require.Equal(t, map[string]string{"low": "new"}, got.Fields)
}

func TestRemoteWatchShutdownCancelsRetry(t *testing.T) {
	c, f := setupRemoteFixture(t, map[string][]byte{"/low": []byte(`{}`)})
	require.NoError(t, c.InitLoad())
	f.failures = 100
	NewConfigModule(c).AfterInit()
	require.Eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.attempts > 0 }, time.Second, time.Millisecond)
	done := make(chan struct{})
	go func() { c.stopWatch(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for retry timer")
	}
	require.ErrorIs(t, c.watch(), context.Canceled)
}

func TestReloadPreservesNonConfigState(t *testing.T) {
	type loaderState struct {
		Value   string
		Ignored string `mapstructure:"-"`
		hidden  string
	}
	c := NewConfig()
	c.Set("feature.value", "updated")
	legacy := loaderState{Ignored: "runtime", hidden: "private"}
	if err := c.UnmarshalKey("feature", &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Ignored != "runtime" || legacy.hidden != "private" {
		t.Fatalf("baseline decoder did not preserve state: %+v", legacy)
	}
	target := loaderState{Ignored: "runtime", hidden: "private"}
	c.AddLoader(LoaderFactory{ProvideApply: func() (string, interface{}) { return "feature", &target }, ReloadApply: func(string, interface{}) {}})
	c.reloadAll()
	if target.Ignored != "runtime" || target.hidden != "private" {
		t.Fatalf("reload cleared non-config state: ignored=%q hidden=%q (direct decode preserves both)", target.Ignored, target.hidden)
	}
}

func TestWatchDedupUsesDecodedSnapshot(t *testing.T) {
	c, f := setupRemoteFixture(t, map[string][]byte{"/low": []byte(`{"feature":{"value":"initial"}}`)})
	target := &watchedFeature{}
	var latest atomic.Value
	latest.Store("")
	var gate atomic.Bool
	entered, decode, sawB, finishB := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var heldB atomic.Bool
	c.AddLoader(LoaderFactory{
		ProvideApply: func() (string, interface{}) {
			if gate.CompareAndSwap(true, false) {
				close(entered)
				<-decode
			}
			return "feature", target
		},
		ReloadApply: func(_ string, value interface{}) {
			current := value.(*watchedFeature).Value
			latest.Store(current)
			if current == "B" && heldB.CompareAndSwap(false, true) {
				close(sawB)
				<-finishB
			}
		},
	})
	require.NoError(t, c.InitLoad())
	NewConfigModule(c).AfterInit()
	f.waitReady(t, "/low", 1)
	gate.Store(true)
	f.put("/low", `{"feature":{"value":"A"}}`)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("loader was not reached")
	}
	f.put("/low", `{"feature":{"value":"B"}}`)
	require.Eventually(t, func() bool { return c.GetString("feature.value") == "B" }, time.Second, time.Millisecond)
	close(decode)
	select {
	case <-sawB:
	case <-time.After(time.Second):
		t.Fatal("B was not decoded")
	}
	f.put("/low", `{"feature":{"value":"A"}}`)
	require.Eventually(t, func() bool { return c.GetString("feature.value") == "A" }, time.Second, time.Millisecond)
	close(finishB)
	// Force a later error behind the B/A updates. Reaching another attempt
	// proves the supervisor has consumed those queued updates and the error.
	f.mu.Lock()
	close(f.channels["/low"])
	delete(f.channels, "/low")
	f.mu.Unlock()
	f.waitReady(t, "/low", 2)
	if got := latest.Load().(string); got != "A" {
		t.Fatalf("Viper is A but last delivered loader value is %q after draining updates and restarting", got)
	}
}

func TestReloadPreservesExperimentalEnvBinding(t *testing.T) {
	t.Setenv("CUSTOM_LABEL", "from-env")
	c := NewConfig(viper.NewWithOptions(viper.ExperimentalBindStruct()))
	type appSettings struct {
		CustomLabel string `mapstructure:"custom_label"`
	}
	var direct appSettings
	require.NoError(t, c.Unmarshal(&direct))
	require.Equal(t, "from-env", direct.CustomLabel)
	target := &appSettings{}
	c.AddLoader(LoaderFactory{ProvideApply: func() (string, interface{}) { return "", target }, ReloadApply: func(string, interface{}) {}})
	require.NoError(t, c.InitLoad())
	require.Equal(t, direct, *target)
}
func TestReloadPreservesCustomKeyDelimiter(t *testing.T) {
	c := NewConfig(viper.NewWithOptions(viper.KeyDelimiter("::")))
	c.Set("feature::nested::value", "from-settings")
	type appSettings struct{ Value string }
	var direct appSettings
	require.NoError(t, c.UnmarshalKey("feature::nested", &direct))
	require.Equal(t, "from-settings", direct.Value)
	target := &appSettings{}
	c.AddLoader(LoaderFactory{ProvideApply: func() (string, interface{}) { return "feature::nested", target }, ReloadApply: func(string, interface{}) {}})
	require.NoError(t, c.InitLoad())
	require.Equal(t, direct, *target)
}

func TestReloadPreservesLiteralMapKeys(t *testing.T) {
	type feature struct{ Labels map[string]string }
	c := NewConfig()
	c.Set("feature", map[string]interface{}{"labels": map[string]interface{}{"host.example.com": "enabled"}})
	legacy := &feature{}
	require.NoError(t, c.UnmarshalKey("feature", legacy))
	require.Equal(t, map[string]string{"host.example.com": "enabled"}, legacy.Labels)
	var called bool
	target := &feature{}
	c.AddLoader(LoaderFactory{ProvideApply: func() (string, interface{}) { return "feature", target }, ReloadApply: func(string, interface{}) { called = true }})
	c.reloadAll()
	require.True(t, called, "literal dotted map key must not cause an otherwise valid document to fail decoding")
	require.Equal(t, legacy.Labels, target.Labels)
}

func TestArchitectureReviewWatchesLiteralMapKeyOnlyChanges(t *testing.T) {
	type feature struct{ Labels map[string]string }
	c, f := setupRemoteFixture(t, map[string][]byte{"/low": []byte(`{"feature":{"labels":{"host.example.com":"initial"}}}`)})
	updates := make(chan feature, 20)
	c.AddLoader(LoaderFactory{
		ProvideApply: func() (string, interface{}) { return "feature", &feature{} },
		ReloadApply:  func(_ string, value interface{}) { updates <- *value.(*feature) },
	})
	require.NoError(t, c.InitLoad())
	require.Equal(t, "initial", (<-updates).Labels["host.example.com"])
	NewConfigModule(c).AfterInit()
	require.Equal(t, "initial", (<-updates).Labels["host.example.com"])
	f.waitReady(t, "/low", 1)
	f.put("/low", `{"feature":{"labels":{"host.example.com":"updated"}}}`)
	select {
	case got := <-updates:
		require.Equal(t, "updated", got.Labels["host.example.com"])
	case <-time.After(time.Second):
		t.Fatal("literal map key update was skipped by deduplication")
	}
}

func TestArchitectureReviewPreservesUnmarshalSubtreePrecedence(t *testing.T) {
	type feature struct{ Value, Fallback string }
	c := NewConfig()
	c.SetDefault("feature.fallback", "default")
	c.Set("feature", map[string]interface{}{"value": "override"})
	legacy := &feature{}
	require.NoError(t, c.UnmarshalKey("feature", legacy))
	target := &feature{}
	c.AddLoader(LoaderFactory{ProvideApply: func() (string, interface{}) { return "feature", target }, ReloadApply: func(string, interface{}) {}})
	c.reloadAll()
	require.Equal(t, legacy, target)
}

type architectureReviewOptions struct{ Enabled bool }

func TestArchitectureReviewUnexportedEmbeddedConfig(t *testing.T) {
	type feature struct {
		architectureReviewOptions `mapstructure:",squash"`
	}
	c := NewConfig()
	c.Set("feature.enabled", true)
	legacy := &feature{}
	require.NoError(t, c.UnmarshalKey("feature", legacy))
	require.True(t, legacy.Enabled)
	target := &feature{}
	c.AddLoader(LoaderFactory{ProvideApply: func() (string, interface{}) { return "feature", target }, ReloadApply: func(string, interface{}) {}})
	c.reloadAll()
	require.Equal(t, legacy, target)
}

func TestReloadUnexportedEmbeddedConfig(t *testing.T) {
	type feature struct {
		architectureReviewOptions `mapstructure:",squash"`
	}
	c := NewConfig()
	c.Set("feature.enabled", true)
	legacy := &feature{}
	require.NoError(t, c.UnmarshalKey("feature", legacy))
	require.True(t, legacy.Enabled)
	target := &feature{}
	c.AddLoader(LoaderFactory{ProvideApply: func() (string, interface{}) { return "feature", target }, ReloadApply: func(string, interface{}) {}})
	c.reloadAll()
	require.Equal(t, legacy, target)
}
