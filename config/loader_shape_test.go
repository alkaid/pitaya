package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type LoaderShapeOptions struct {
	Enabled bool
	Removed string
	Fields  map[string]string
	Names   []string
	Ignored string `mapstructure:"-"`
	hidden  string
}

func (*LoaderShapeOptions) configOptions() {}

type loaderShapeBackend interface{ configOptions() }

type privateLoaderShapeOptions LoaderShapeOptions

func TestReloadPreservesDecoderShape(t *testing.T) {
	for _, kind := range []string{"squash pointer", "private squash pointer", "typed interface", "empty interface", "interface struct"} {
		t.Run(kind, func(t *testing.T) {
			initial := &LoaderShapeOptions{Ignored: "runtime", hidden: "private"}
			var target interface{}
			var options func() *LoaderShapeOptions
			key := "feature.options"
			switch kind {
			case "squash pointer":
				type feature struct {
					*LoaderShapeOptions `mapstructure:",squash"`
				}
				value := &feature{LoaderShapeOptions: initial}
				target = value
				options = func() *LoaderShapeOptions { return value.LoaderShapeOptions }
				key = "feature"
			case "private squash pointer":
				type feature struct {
					*privateLoaderShapeOptions `mapstructure:",squash"`
				}
				value := &feature{privateLoaderShapeOptions: (*privateLoaderShapeOptions)(initial)}
				target = value
				options = func() *LoaderShapeOptions { return (*LoaderShapeOptions)(value.privateLoaderShapeOptions) }
				key = "feature"
			case "typed interface":
				type feature struct{ Options loaderShapeBackend }
				value := &feature{Options: initial}
				target = value
				options = func() *LoaderShapeOptions { return value.Options.(*LoaderShapeOptions) }
			case "empty interface":
				type feature struct{ Options interface{} }
				value := &feature{Options: initial}
				target = value
				options = func() *LoaderShapeOptions {
					decoded, ok := value.Options.(*LoaderShapeOptions)
					require.True(t, ok, "concrete interface type must survive decoding")
					return decoded
				}
			case "interface struct":
				type feature struct{ Options interface{} }
				value := &feature{Options: *initial}
				target = value
				options = func() *LoaderShapeOptions {
					decoded, ok := value.Options.(LoaderShapeOptions)
					require.True(t, ok, "concrete interface type must survive decoding")
					return &decoded
				}
			}
			c := NewConfig()
			calls := 0
			c.AddLoader(LoaderFactory{
				ProvideApply: func() (string, interface{}) { return "feature", target },
				ReloadApply:  func(string, interface{}) { calls++ },
			})
			c.Set(key, map[string]interface{}{
				"enabled": true, "removed": "old",
				"fields": map[string]string{"old": "value"}, "names": []string{"old"},
			})
			c.reloadAll()
			require.Equal(t, 1, calls, "valid document must reach Reload")
			require.True(t, options().Enabled)
			require.Equal(t, "runtime", options().Ignored)
			require.Equal(t, "private", options().hidden)

			c.Set(key, map[string]interface{}{"enabled": false})
			c.reloadAll()
			require.Equal(t, 2, calls)
			retained := &LoaderShapeOptions{Removed: "old", Fields: map[string]string{"old": "value"}, Names: []string{"old"}, Ignored: "runtime", hidden: "private"}
			require.Equal(t, retained, options(), "omitted configuration must retain its previous value")

			c.Set(key, map[string]interface{}{"enabled": "invalid-bool", "removed": "partial", "fields": map[string]string{"new": "value"}, "names": []string{"partial"}})
			c.reloadAll()
			require.Equal(t, 2, calls, "failed decoding must not call Reload")
			require.Equal(t, retained, options(), "failed decoding must not mutate the supplied object")
		})
	}
}

func TestReloadPreservesNestedDecoderShape(t *testing.T) {
	type nested struct{ Options loaderShapeBackend }
	type feature struct {
		Nested   *nested
		Optional *LoaderShapeOptions
	}
	target := &feature{
		Nested:   &nested{Options: &LoaderShapeOptions{Removed: "stale", Ignored: "runtime"}},
		Optional: &LoaderShapeOptions{Removed: "stale"},
	}
	c := NewConfig()
	c.Set("feature.nested.options.enabled", true)
	calls := 0
	c.AddLoader(LoaderFactory{
		ProvideApply: func() (string, interface{}) { return "feature", target },
		ReloadApply:  func(string, interface{}) { calls++ },
	})
	c.reloadAll()
	require.Equal(t, 1, calls)
	require.Equal(t, &LoaderShapeOptions{Enabled: true, Removed: "stale", Ignored: "runtime"}, target.Nested.Options)
	require.Equal(t, &LoaderShapeOptions{Removed: "stale"}, target.Optional)
}

func TestReloadPreservesCollectionsAndCycles(t *testing.T) {
	type feature struct {
		Enabled bool
		Next    *feature
		Dynamic interface{}
		Fields  map[string]string
		Names   []string
	}
	target := &feature{Fields: map[string]string{"old": "value"}, Names: []string{"old"}}
	target.Next = target
	target.Dynamic = target
	c := NewConfig()
	c.Set("feature.enabled", true)
	calls := 0
	c.AddLoader(LoaderFactory{
		ProvideApply: func() (string, interface{}) { return "feature", target },
		ReloadApply:  func(string, interface{}) { calls++ },
	})
	c.reloadAll()
	require.Equal(t, 1, calls)
	require.True(t, target.Enabled)
	require.Same(t, target, target.Next)
	require.Same(t, target, target.Dynamic)
	require.Equal(t, map[string]string{"old": "value"}, target.Fields)
	require.Equal(t, []string{"old"}, target.Names)
}

func TestReloadPreservesNestedRuntimeState(t *testing.T) {
	for _, kind := range []string{"array", "pointer chain"} {
		t.Run(kind, func(t *testing.T) {
			initial := LoaderShapeOptions{Ignored: "runtime", hidden: "private"}
			var target interface{}
			var options func() *LoaderShapeOptions
			var document interface{}
			if kind == "array" {
				type feature struct{ Options [1]LoaderShapeOptions }
				value := &feature{Options: [1]LoaderShapeOptions{initial}}
				target = value
				options = func() *LoaderShapeOptions { return &value.Options[0] }
				document = []map[string]interface{}{{"enabled": true}}
			} else {
				type feature struct{ Options **LoaderShapeOptions }
				pointer := &initial
				value := &feature{Options: &pointer}
				target = value
				options = func() *LoaderShapeOptions { return *value.Options }
				document = map[string]interface{}{"enabled": true}
			}
			c := NewConfig()
			c.Set("feature.options", document)
			calls := 0
			c.AddLoader(LoaderFactory{
				ProvideApply: func() (string, interface{}) { return "feature", target },
				ReloadApply:  func(string, interface{}) { calls++ },
			})
			c.reloadAll()
			require.Equal(t, 1, calls)
			require.Equal(t, &LoaderShapeOptions{Enabled: true, Ignored: "runtime", hidden: "private"}, options())
		})
	}
}

// In-place Viper decoding is the compatibility oracle for omitted fields,
// explicit zeroes and collections; snapshot isolation must not change it.
func TestReloadMatchesInPlaceDecode(t *testing.T) {
	type feature struct {
		Enabled bool
		Count   int
		Label   string
		Names   []string
		Fields  map[string]string
		Options *LoaderShapeOptions
		Items   []LoaderShapeOptions
		At      time.Time
	}
	initial := func() *feature {
		return &feature{
			Enabled: true, Count: 7, Label: "old", Names: []string{"old"},
			Fields:  map[string]string{"old": "value"},
			Options: &LoaderShapeOptions{Enabled: true, Removed: "old"},
			Items:   []LoaderShapeOptions{{Enabled: true, Removed: "old"}},
			At:      time.Date(2026, time.November, 5, 0, 0, 0, 0, time.Local),
		}
	}
	for _, tc := range []struct {
		name     string
		document map[string]interface{}
	}{
		{"omitted", map[string]interface{}{"enabled": false}},
		{"empty document", map[string]interface{}{}},
		{"explicit zeroes", map[string]interface{}{
			"enabled": false, "count": 0, "label": "", "at": "",
			"names": []string{}, "fields": map[string]string{},
		}},
		{"partial collections", map[string]interface{}{
			"names": []string{"new"}, "fields": map[string]string{"new": "value"},
			"options": map[string]interface{}{"enabled": false},
			"items":   []map[string]interface{}{{"enabled": false}},
		}},
		{"null", map[string]interface{}{"names": nil, "fields": nil, "options": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConfig()
			c.Set("feature", tc.document)
			expected, target := initial(), initial()
			require.NoError(t, c.UnmarshalKey("feature", expected))
			calls := 0
			c.AddLoader(LoaderFactory{
				ProvideApply: func() (string, interface{}) { return "feature", target },
				ReloadApply:  func(string, interface{}) { calls++ },
			})
			c.reloadAll()
			require.Equal(t, 1, calls)
			require.Equal(t, expected, target)
			if tc.name == "explicit zeroes" {
				require.False(t, target.Enabled)
				require.Zero(t, target.Count)
				require.Empty(t, target.Label)
				require.Empty(t, target.Names)
				require.Equal(t, map[string]string{"old": "value"}, target.Fields, "an empty map does not delete old entries")
			}
		})
	}
}

// An initialized private embedded pointer is valid even without squash. The
// clone must allocate its slot before copying fields that the decoder retains.
func TestReloadPreservesPrivateEmbeddedPointer(t *testing.T) {
	type feature struct{ *privateLoaderShapeOptions }
	target := &feature{&privateLoaderShapeOptions{Enabled: true, Removed: "old"}}
	expected := &feature{&privateLoaderShapeOptions{Enabled: true, Removed: "old"}}
	c := NewConfig()
	c.Set("feature", map[string]interface{}{"other": "value"})
	require.NoError(t, c.UnmarshalKey("feature", expected))
	c.AddLoader(LoaderFactory{
		ProvideApply: func() (string, interface{}) { return "feature", target },
		ReloadApply:  func(string, interface{}) {},
	})
	c.reloadAll()
	require.Equal(t, expected, target)
}
