package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuiltInRPCTransportCapabilities(t *testing.T) {
	natsCapabilities := RPCTransportCapabilities(
		(*NatsRPCClient)(nil),
		(*NatsRPCServer)(nil),
	)
	assert.Equal(t, FullTransportCapabilities, natsCapabilities)
	for _, capability := range []TransportCapability{
		TransportPointToPoint,
		TransportFanout,
		TransportUserDelivery,
		TransportBrokerSubscription,
	} {
		assert.True(t, natsCapabilities.Has(capability))
	}

	grpcCapabilities := RPCTransportCapabilities(
		(*GRPCClient)(nil),
		(*GRPCServer)(nil),
	)
	assert.Equal(t, GRPCTransportCapabilities, grpcCapabilities)
	assert.True(t, grpcCapabilities.Has(TransportPointToPoint))
	assert.Equal(
		t,
		"fanout,user-delivery,broker-subscription",
		FullTransportCapabilities.MissingFrom(grpcCapabilities).String(),
	)
}
