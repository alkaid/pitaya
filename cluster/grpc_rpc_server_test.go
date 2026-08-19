package cluster

import (
	"fmt"
	"net"
	"testing"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewGRPCServer(t *testing.T) {
	t.Parallel()
	sv := getServer()
	gs, err := NewGRPCServer(config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc, sv, []metrics.Reporter{})
	assert.NoError(t, err)
	assert.NotNil(t, gs)
}

func TestGRPCServerInit(t *testing.T) {
	t.Parallel()
	c := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
	c.Port = helpers.GetFreePort(t)
	sv := getServer()
	gs, err := NewGRPCServer(c, sv, []metrics.Reporter{})
	gs.SetPitayaServer(&protos.UnimplementedPitayaServer{})
	err = gs.Init()
	assert.NoError(t, err)
	assert.NotNil(t, gs)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", c.Port))
	assert.NoError(t, err)
	assert.NotNil(t, conn)
	assert.NotNil(t, gs.grpcSv)
	require.NoError(t, conn.Close())

	gs.BeforeShutdown()
	assert.NoError(t, gs.Shutdown(), "Shutdown must be idempotent after the admission barrier")
}
