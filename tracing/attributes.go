package tracing

import (
	"go.opentelemetry.io/otel/attribute"
)

const (
	RPCClientKey    = attribute.Key("rpc.client")
	RPCClientIDKey  = attribute.Key("rpc.client.id")
	RPCServiceIDKey = attribute.Key("rpc.service.id")
)
