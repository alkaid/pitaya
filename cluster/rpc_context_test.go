package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareOutboundRPCContextOwnsReliableWireMetadata(t *testing.T) {
	ordinary := pcontext.AddListToPropagateCtx(
		context.Background(),
		"tenant", "east",
		constants.IdempotencyKey, "job-1",
		rpccontext.ReliableJobKey, "spoofed-job",
		rpccontext.ReliableJobMetadataDigestKey, strings.Repeat("f", 64),
	)
	prepared := PrepareOutboundRPCContext(ordinary)
	assert.Equal(t, "east", pcontext.GetFromPropagateCtx(prepared, "tenant"))
	assert.Equal(t, "job-1", pcontext.GetFromPropagateCtx(prepared, constants.IdempotencyKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(prepared, rpccontext.ReliableJobKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(prepared, rpccontext.ReliableJobMetadataDigestKey))

	nullSpoof := pcontext.AddListToPropagateCtx(
		context.Background(),
		rpccontext.ReliableJobKey, nil,
		rpccontext.ReliableJobMetadataDigestKey, nil,
	)
	prepared = PrepareOutboundRPCContext(nullSpoof)
	_, markerFound := pcontext.ToMap(prepared)[rpccontext.ReliableJobKey]
	_, digestFound := pcontext.ToMap(prepared)[rpccontext.ReliableJobMetadataDigestKey]
	assert.False(t, markerFound)
	assert.False(t, digestFound)

	digest := strings.Repeat("a", 64)
	trusted := rpccontext.WithReliableRPCJob(ordinary, "job-1", digest)
	prepared = PrepareOutboundRPCContext(trusted)
	assert.Equal(t, "job-1", pcontext.GetFromPropagateCtx(prepared, rpccontext.ReliableJobKey))
	assert.Equal(t, digest, pcontext.GetFromPropagateCtx(prepared, rpccontext.ReliableJobMetadataDigestKey))

	background := context.Background()
	assert.Equal(t, background, PrepareOutboundRPCContext(background))
	assert.Nil(t, PrepareOutboundRPCContext(nil))
}

func TestBuildRequestStripsSpoofedReliableMetadataAndInjectsTrustedMarker(t *testing.T) {
	server := NewServer("worker-1", "worker", false)
	msg := &message.Message{Type: message.Request, Data: []byte("payload")}
	spoofed := pcontext.AddListToPropagateCtx(
		context.Background(),
		rpccontext.ReliableJobKey, "spoofed-job",
		rpccontext.ReliableJobMetadataDigestKey, strings.Repeat("f", 64),
	)

	request, err := buildRequest(spoofed, protos.RPCType_User, "game.player.move", nil, msg, server)
	require.NoError(t, err)
	requestCtx, err := pcontext.Decode(request.Metadata)
	require.NoError(t, err)
	assert.Nil(t, pcontext.GetFromPropagateCtx(requestCtx, rpccontext.ReliableJobKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(requestCtx, rpccontext.ReliableJobMetadataDigestKey))

	digest := strings.Repeat("a", 64)
	trusted := rpccontext.WithReliableRPCJob(spoofed, "job-1", digest)
	request, err = buildRequest(trusted, protos.RPCType_User, "game.player.move", nil, msg, server)
	require.NoError(t, err)
	requestCtx, err = pcontext.Decode(request.Metadata)
	require.NoError(t, err)
	assert.Equal(t, "job-1", pcontext.GetFromPropagateCtx(requestCtx, rpccontext.ReliableJobKey))
	assert.Equal(t, digest, pcontext.GetFromPropagateCtx(requestCtx, rpccontext.ReliableJobMetadataDigestKey))
}
