package config

import (
	"testing"

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
			require.Equal(t, &LoaderShapeOptions{Ignored: "runtime", hidden: "private"}, options(), "removed configuration must not survive")

			c.Set(key, map[string]interface{}{"enabled": "invalid-bool", "removed": "partial", "fields": map[string]string{"new": "value"}})
			c.reloadAll()
			require.Equal(t, 2, calls, "failed decoding must not call Reload")
			require.Equal(t, &LoaderShapeOptions{Ignored: "runtime", hidden: "private"}, options(), "failed decoding must not mutate the supplied object")
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
	require.Equal(t, &LoaderShapeOptions{Enabled: true, Ignored: "runtime"}, target.Nested.Options)
	require.Nil(t, target.Optional, "ordinary absent pointers do not need a decoder allocation")
}

func TestReloadDecoderShapeDoesNotRetainCollectionsOrCycles(t *testing.T) {
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
	require.Nil(t, target.Next)
	require.IsType(t, (*feature)(nil), target.Dynamic)
	require.Nil(t, target.Dynamic)
	require.Nil(t, target.Fields)
	require.Nil(t, target.Names)
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
