package tracing

import (
	"context"
	"sort"
	"testing"

	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func TestAgentLifecycleWithoutExporter(t *testing.T) {
	originalProvider := otel.GetTracerProvider()
	originalPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		require.NoError(t, StopAgent())
		otel.SetTracerProvider(originalProvider)
		otel.SetTextMapPropagator(originalPropagator)
	})

	require.NoError(t, StopAgent())
	require.NoError(t, StartAgent(context.Background(), Config{Name: "coverage-one", Sampler: 1}))
	first := agentInstance
	require.NotNil(t, first)

	ctx, span := otel.Tracer(TraceName).Start(context.Background(), "agent-span")
	assert.True(t, trace.SpanContextFromContext(ctx).IsValid())
	span.End()

	require.NoError(t, StartAgent(context.Background(), Config{Name: "coverage-two", Sampler: 0}))
	require.NotNil(t, agentInstance)
	assert.NotSame(t, first, agentInstance)
	require.NoError(t, StopAgent())
	assert.Nil(t, agentInstance)
	require.NoError(t, StopAgent())
}

func TestCarrierKeysAndContextConversions(t *testing.T) {
	carrier := Carrier{"traceparent": "parent", "tracestate": "state"}
	carrier.Set("baggage", "tenant=east")
	assert.Equal(t, "parent", carrier.Get("traceparent"))
	keys := carrier.Keys()
	sort.Strings(keys)
	assert.Equal(t, []string{"baggage", "traceparent", "tracestate"}, keys)

	fromCarrier := CarrierFromPropagateCtx(pcontext.AddToPropagateCtx(
		context.Background(), constants.SpanPropagateCtxKey, carrier,
	))
	require.Equal(t, carrier, fromCarrier)
	fromCarrier["traceparent"] = "changed"
	assert.Equal(t, "parent", carrier["traceparent"], "context extraction must copy the carrier")

	fromMap := CarrierFromPropagateCtx(pcontext.AddToPropagateCtx(
		context.Background(),
		constants.SpanPropagateCtxKey,
		map[string]any{"traceparent": "mapped"},
	))
	assert.Equal(t, Carrier{"traceparent": "mapped"}, fromMap)
	assert.Nil(t, CarrierFromPropagateCtx(context.Background()))
	assert.Nil(t, CarrierFromPropagateCtx(pcontext.AddToPropagateCtx(
		context.Background(), constants.SpanPropagateCtxKey, "unsupported",
	)))
}

func TestSpanInfoFromRequestReadsPropagatedPeerIdentity(t *testing.T) {
	ctx := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.PeerServiceKey, "worker",
		constants.PeerIDKey, "worker-7",
		constants.RequestIDKey, "request-9",
	)

	info := SpanInfoFromRequest(ctx)
	assert.Equal(t, "worker", info.PeerType)
	assert.Equal(t, "worker-7", info.PeerID)
	assert.Equal(t, "request-9", info.RequestID)
	assert.Equal(t, &SpanInfo{}, SpanInfoFromRequest(context.Background()))
}

func TestRPCStartSpanClientAndServerAttributes(t *testing.T) {
	recorder := installTestTracer(t)
	rt := route.NewRoute("game", "player", "move")

	clientCtx := RPCStartSpan(context.Background(), &SpanInfo{
		RpcSystem: "nats",
		IsClient:  true,
		Route:     rt,
		PeerID:    "game-2",
		PeerType:  "game",
		LocalID:   "worker-1",
		LocalType: "worker",
		RequestID: "request-1",
		MsgType:   "request",
		UserID:    "user-1",
	}, attribute.String("custom", "value"))
	clientSpan := trace.SpanFromContext(clientCtx)
	require.True(t, clientSpan.SpanContext().IsValid())
	assert.NotEmpty(t, CarrierFromPropagateCtx(clientCtx).Get("traceparent"))

	serverCtx := RPCStartSpan(clientCtx, &SpanInfo{
		RpcSystem: "nats",
		Route:     rt,
		PeerID:    "worker-1",
		PeerType:  "worker",
		LocalID:   "game-2",
		LocalType: "game",
	})
	serverSpan := trace.SpanFromContext(serverCtx)
	assert.Equal(t, clientSpan.SpanContext().TraceID(), serverSpan.SpanContext().TraceID())

	FinishSpan(serverCtx, nil)
	FinishSpan(clientCtx, nil)
	ended := recorder.Ended()
	require.Len(t, ended, 2)
	assert.Equal(t, trace.SpanKindServer, ended[0].SpanKind())
	assert.Equal(t, clientSpan.SpanContext().SpanID(), ended[0].Parent().SpanID())
	assert.Equal(t, trace.SpanKindClient, ended[1].SpanKind())
	assert.Contains(t, ended[1].Attributes(), attribute.String("custom", "value"))
	assert.Contains(t, ended[1].Attributes(), RPCClientKey.String("worker"))
	assert.Contains(t, ended[1].Attributes(), RPCClientIDKey.String("worker-1"))
	assert.Contains(t, ended[1].Attributes(), RPCServiceIDKey.String("game-2"))
}
