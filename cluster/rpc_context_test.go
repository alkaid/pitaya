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

func TestPrepareOutboundRPCContextOwnsStatefulAdmissionWireMetadata(t *testing.T) {
	spoofed := pcontext.AddListToPropagateCtx(
		context.Background(),
		"tenant", "east",
		constants.IdempotencyFanoutKey, "1",
		rpccontext.StatefulRouteModeKey, string(rpccontext.StatefulRouteModeAssigned),
	)
	prepared := PrepareOutboundRPCContext(spoofed)
	assert.Equal(t, "east", pcontext.GetFromPropagateCtx(prepared, "tenant"))
	assert.Nil(t, pcontext.GetFromPropagateCtx(prepared, constants.IdempotencyFanoutKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(prepared, rpccontext.StatefulRouteModeKey))

	nullSpoof := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.IdempotencyFanoutKey, nil,
		rpccontext.StatefulRouteModeKey, nil,
	)
	prepared = PrepareOutboundRPCContext(nullSpoof)
	_, fanoutFound := pcontext.ToMap(prepared)[constants.IdempotencyFanoutKey]
	_, routeModeFound := pcontext.ToMap(prepared)[rpccontext.StatefulRouteModeKey]
	assert.False(t, fanoutFound)
	assert.False(t, routeModeFound)

	for _, mode := range []rpccontext.StatefulRouteMode{
		rpccontext.StatefulRouteModeAssigned,
		rpccontext.StatefulRouteModeDirect,
	} {
		t.Run(string(mode), func(t *testing.T) {
			trusted := rpccontext.WithStatefulRouteMode(spoofed, mode)
			prepared := PrepareOutboundRPCContext(trusted)
			assert.Equal(t, string(mode), pcontext.GetFromPropagateCtx(prepared, rpccontext.StatefulRouteModeKey))
			assert.Nil(t, pcontext.GetFromPropagateCtx(prepared, constants.IdempotencyFanoutKey))
		})
	}

	invalidTyped := rpccontext.WithStatefulRouteMode(spoofed, rpccontext.StatefulRouteMode("invalid"))
	prepared = PrepareOutboundRPCContext(invalidTyped)
	assert.Nil(t, pcontext.GetFromPropagateCtx(prepared, constants.IdempotencyFanoutKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(prepared, rpccontext.StatefulRouteModeKey))

	trustedFanout := rpccontext.WithRPCFanout(
		rpccontext.WithStatefulRouteMode(spoofed, rpccontext.StatefulRouteModeAssigned),
	)
	prepared = PrepareOutboundRPCContext(trustedFanout)
	assert.Equal(t, "1", pcontext.GetFromPropagateCtx(prepared, constants.IdempotencyFanoutKey))
	assert.Equal(
		t,
		string(rpccontext.StatefulRouteModeFanout),
		pcontext.GetFromPropagateCtx(prepared, rpccontext.StatefulRouteModeKey),
	)

	fromNil := PrepareOutboundRPCContext(
		rpccontext.WithStatefulRouteMode(nil, rpccontext.StatefulRouteModeDirect),
	)
	assert.Equal(
		t,
		string(rpccontext.StatefulRouteModeDirect),
		pcontext.GetFromPropagateCtx(fromNil, rpccontext.StatefulRouteModeKey),
	)
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

	request, err = buildRequest(nil, protos.RPCType_User, "game.player.move", nil, msg, server)
	require.NoError(t, err)
	requestCtx, err = pcontext.Decode(request.Metadata)
	require.NoError(t, err)
	assert.Equal(t, server.ID, pcontext.GetFromPropagateCtx(requestCtx, constants.PeerIDKey))
	assert.Equal(t, server.Type, pcontext.GetFromPropagateCtx(requestCtx, constants.PeerServiceKey))
}
