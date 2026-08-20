package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/alkaid/pitaya/v2/cluster"
	clustermocks "github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestClusterSendRequestPreparesReliableMetadata(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := clustermocks.NewMockRPCClient(ctrl)
	discovery := clustermocks.NewMockServiceDiscovery(ctrl)
	target := cluster.NewServer("game-1", "game", false)
	target.SetOwnerEpoch(73)
	discovery.EXPECT().GetServer(target.ID).Return(target, nil)
	digest := strings.Repeat("a", 64)
	client.EXPECT().Call(gomock.Any(), protos.RPCType_User, gomock.Any(), nil, gomock.Any(), target).
		DoAndReturn(func(ctx context.Context, _ protos.RPCType, _ *route.Route, _ session.Session, _ *message.Message, _ *cluster.Server) (*protos.Response, error) {
			assert.Equal(t, target.ID, pcontext.GetFromPropagateCtx(ctx, constants.OwnerServerIDKey))
			assert.Equal(t, "73", pcontext.GetFromPropagateCtx(ctx, constants.OwnerEpochKey))
			assert.Equal(t, "job-1", pcontext.GetFromPropagateCtx(ctx, rpccontext.ReliableJobKey))
			assert.Equal(t, digest, pcontext.GetFromPropagateCtx(ctx, rpccontext.ReliableJobMetadataDigestKey))
			return &protos.Response{}, nil
		})

	agent := &Cluster{rpcClient: client, serviceDiscovery: discovery}
	ctx := rpccontext.WithReliableRPCJob(
		pcontext.AddListToPropagateCtx(
			context.Background(),
			constants.IdempotencyKey, "job-1",
			rpccontext.ReliableJobKey, "spoofed-job",
			rpccontext.ReliableJobMetadataDigestKey, strings.Repeat("f", 64),
		),
		"job-1",
		digest,
	)
	response, err := agent.SendRequest(ctx, target.ID, "game.player.move", []byte("payload"))
	require.NoError(t, err)
	require.NotNil(t, response)
}
