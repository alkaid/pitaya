// Copyright (c) nano Author and TFG Co. All Rights Reserved.
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

package pitaya

import (
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/alkaid/pitaya/v2/acceptor"
	clustermocks "github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestDefaultBuilderBuildsWithoutReloadConfig(t *testing.T) {
	for _, table := range tables {
		t.Run(table.serverType, func(t *testing.T) {
			pitayaConfig := config.NewDefaultPitayaConfig()
			builder := NewDefaultBuilder(
				table.isFrontend,
				table.serverType,
				table.serverMode,
				table.serverMetadata,
				*pitayaConfig,
			)
			require.Nil(t, builder.conf)

			var built Pitaya
			require.NotPanics(t, func() {
				built = builder.Build()
			})
			require.NotNil(t, built)
			assert.Nil(t, built.(*App).conf)
		})
	}
}

func TestBuilderWithConfigsRegistersLoggerReload(t *testing.T) {
	for _, table := range tables {
		t.Run(table.serverType, func(t *testing.T) {
			conf := config.NewConfig(table.cfg)
			loaderCount := configLoaderCount(conf)
			builder := NewBuilderWithConfigs(
				table.isFrontend,
				table.serverType,
				table.serverMode,
				table.serverMetadata,
				conf,
			)

			built := builder.Build().(*App)
			assert.Same(t, conf, built.conf)
			assert.Equal(t, loaderCount+1, configLoaderCount(conf))
		})
	}
}

func configLoaderCount(conf *config.Config) int {
	return reflect.ValueOf(conf).Elem().FieldByName("loaders").Len()
}

func TestBuilderModeValidation(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()

	t.Run("standalone rejects RPC dependencies", func(t *testing.T) {
		builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *pitayaConfig)
		builder.RPCClient = clustermocks.NewMockRPCClient(gomock.NewController(t))

		assert.PanicsWithValue(t, "Standalone mode can't have RPC or service discovery instances", func() {
			builder.Build()
		})
	})

	t.Run("cluster requires every RPC dependency", func(t *testing.T) {
		builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *pitayaConfig)
		builder.RPCClient = nil

		assert.PanicsWithValue(t, "Cluster mode must have RPC and service discovery instances", func() {
			builder.Build()
		})
	})
}

func TestBuilderPassesRPCFanoutConcurrency(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	pitayaConfig.Cluster.RPC.FanoutConcurrency = 7

	builder := NewDefaultBuilder(false, "cluster", Cluster, nil, *pitayaConfig)
	app := builder.Build().(*App)

	configuredConcurrency := reflect.ValueOf(app.remoteService).Elem().FieldByName("fanoutConcurrency").Int()
	assert.Equal(t, int64(7), configuredConcurrency)
}

func TestBuilderReadsTrustedProxyConfigAtBuild(t *testing.T) {
	pitayaConfig := config.NewDefaultPitayaConfig()
	pitayaConfig.Acceptor.TrustedProxyCIDRs = []string{"10.0.0.0/8"}
	builder := NewDefaultBuilder(false, "standalone", Standalone, nil, *pitayaConfig)
	builder.Config.Acceptor.TrustedProxyCIDRs = []string{"not-a-cidr"}

	assert.Panics(t, func() {
		builder.Build()
	})
}

func TestBuilderStartConfigModulePolicy(t *testing.T) {
	for _, mode := range []string{"snapshot", "reloadable"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestBuilderStartConfigModulePolicyHelper$")
			cmd.Env = append(os.Environ(), "PITAYA_BUILDER_START_HELPER="+mode)
			output, err := cmd.CombinedOutput()
			require.NoErrorf(t, err, "helper output:\n%s", output)
		})
	}
}

func TestBuilderStartConfigModulePolicyHelper(t *testing.T) {
	mode := os.Getenv("PITAYA_BUILDER_START_HELPER")
	if mode == "" {
		return
	}

	var app *App
	switch mode {
	case "snapshot":
		pitayaConfig := config.NewDefaultPitayaConfig()
		app = NewDefaultBuilder(false, mode, Standalone, nil, *pitayaConfig).Build().(*App)
	case "reloadable":
		app = NewBuilderWithConfigs(false, mode, Standalone, nil, config.NewConfig()).Build().(*App)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}

	app.onStarted = func() { app.dieChan <- true }
	app.Start()

	_, err := app.GetModule("configLoader")
	if mode == "reloadable" {
		assert.NoError(t, err)
	} else {
		assert.EqualError(t, err, "module with name configLoader not found")
	}
}

func TestPostBuildHooks(t *testing.T) {
	acc := acceptor.NewTCPAcceptor("0.0.0.0:0")
	for _, table := range tables {
		builderConfig := config.NewDefaultPitayaConfig()

		t.Run("with_post_build_hooks", func(t *testing.T) {
			called := false
			builder := newAppTestBuilder(table.isFrontend, table.serverType, table.serverMode, table.serverMetadata, *builderConfig)
			builder.AddAcceptor(acc)
			builder.AddPostBuildHook(func(app Pitaya) {
				called = true
			})
			app := builder.Build()

			assert.True(t, called)
			assert.NotNil(t, app)
		})

		t.Run("without_post_build_hooks", func(t *testing.T) {
			called := false
			builder := newAppTestBuilder(table.isFrontend, table.serverType, table.serverMode, table.serverMetadata, *builderConfig)
			builder.AddAcceptor(acc)
			app := builder.Build()

			assert.False(t, called)
			assert.NotNil(t, app)
		})
	}
}
