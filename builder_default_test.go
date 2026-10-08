package pitaya

import (
	"testing"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/stretchr/testify/require"
)

func TestDefaultBuilderBuildWithoutReloadSource(t *testing.T) {
	for _, mode := range []ServerMode{Standalone, Cluster} {
		builder := NewDefaultBuilder(true, "connector", mode, map[string]string{"region": "test"}, *config.NewDefaultPitayaConfig())
		app := builder.Build()
		require.Equal(t, "connector", app.GetServer().Type)
		require.Equal(t, "test", app.GetServer().Metadata["region"])
		require.NotNil(t, app.GetDieChan())
		require.False(t, app.IsRunning())
	}
}
