// Copyright (c) TFG Co. All Rights Reserved.
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

package tracing

import (
	"context"

	"github.com/topfreegames/pitaya/v2/route"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	semconv "go.opentelemetry.io/otel/semconv/v1.32.0"

	"go.opentelemetry.io/otel/codes"

	"go.opentelemetry.io/otel"

	"go.opentelemetry.io/otel/trace"

	"github.com/topfreegames/pitaya/v2/constants"
	pcontext "github.com/topfreegames/pitaya/v2/context"
)

const TraceName = "pitaya"

// ExtractSpan retrieves an opentracing span context from the given context.Context
// The span context can be received directly (inside the context) or via an RPC call
// (encoded in binary format)
func ExtractSpan(ctx context.Context) (context.Context, trace.SpanContext) {
	carrier := CarrierFromPropagateCtx(ctx)
	if carrier != nil {
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	}
	bags := baggage.FromContext(ctx)
	ctx = baggage.ContextWithBaggage(ctx, bags)
	return ctx, trace.SpanContextFromContext(ctx)
}

// InjectSpan retrieves an opentrancing span from the current context and creates a new context
// with it encoded in binary format inside the propagatable context content
func InjectSpan(ctx context.Context) context.Context {
	carrier := CarrierFromPropagateCtx(ctx)
	if carrier == nil {
		carrier = make(Carrier)
	}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return pcontext.AddToPropagateCtx(ctx, constants.SpanPropagateCtxKey, carrier)
}

// StartSpan starts a new span with a given parent context, operation name, tags and
// optional parent span. It returns a context with the created span.
func StartSpan(
	parentCtx context.Context,
	opName string,
	opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	tr := otel.Tracer(TraceName)
	return tr.Start(parentCtx, opName, opts...)
}

// FinishSpan finishes a span retrieved from the given context and logs the error if it exists
func FinishSpan(ctx context.Context, err error) {
	if ctx == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	defer span.End()
	if err == nil {
		span.SetStatus(codes.Ok, "")
		return
	}
	LogError(span, err)
}

type SpanInfo struct {
	RpcSystem string
	IsClient  bool
	Route     *route.Route
	PeerID    string
	PeerType  string
	LocalID   string
	LocalType string
	RequestID string
	MsgType   string
	UserID    string
}

func SpanInfoFromRequest(ctx context.Context) *SpanInfo {
	spInfo := &SpanInfo{}
	peerTypeAny := pcontext.GetFromPropagateCtx(ctx, constants.PeerServiceKey)
	if peerTypeAny != nil {
		spInfo.PeerType = peerTypeAny.(string)
	}
	peerIDAny := pcontext.GetFromPropagateCtx(ctx, constants.PeerIDKey)
	if peerIDAny != nil {
		spInfo.PeerID = peerIDAny.(string)
	}
	if reqIDAny := pcontext.GetFromPropagateCtx(ctx, constants.RequestIDKey); reqIDAny != nil {
		spInfo.RequestID = reqIDAny.(string)
	}
	return spInfo
}

func RPCStartSpan(ctx context.Context, spanInfo *SpanInfo, attrs ...attribute.KeyValue) context.Context {
	ctx, parent := ExtractSpan(ctx)
	attrs = append(attrs, semconv.RPCSystemKey.String(spanInfo.RpcSystem))
	attrs = append(attrs, semconv.RPCMethodKey.String(spanInfo.Route.Method))
	attrs = append(attrs, semconv.RPCServiceKey.String(spanInfo.Route.SvType))
	var clientType, clientID, svID string
	if spanInfo.IsClient {
		clientType = spanInfo.LocalType
		clientID = spanInfo.LocalID
		svID = spanInfo.PeerID
	} else {
		clientType = spanInfo.PeerType
		clientID = spanInfo.PeerID
		svID = spanInfo.LocalID
	}
	attrs = append(attrs, RPCClientKey.String(clientType))
	attrs = append(attrs, RPCClientIDKey.String(clientID))
	attrs = append(attrs, RPCServiceIDKey.String(svID))
	if spanInfo.RequestID != "" {
		attrs = append(attrs, attribute.Key(constants.RequestIDKey).String(spanInfo.RequestID))
	}
	if spanInfo.MsgType != "" {
		attrs = append(attrs, attribute.Key("msg.type").String(spanInfo.MsgType))
	}
	if spanInfo.UserID != "" {
		attrs = append(attrs, attribute.Key("user.id").String(spanInfo.UserID))
	}
	kind := trace.SpanKindServer
	if spanInfo.IsClient {
		kind = trace.SpanKindClient
	}
	if spanInfo.IsClient {
		ctx = trace.ContextWithSpanContext(ctx, parent)
	} else {
		ctx = trace.ContextWithRemoteSpanContext(ctx, parent)
	}
	ctx, _ = StartSpan(ctx,
		spanInfo.Route.String(),
		trace.WithSpanKind(kind),
		trace.WithAttributes(attrs...))
	if spanInfo.IsClient {
		ctx = InjectSpan(ctx)
	}
	return ctx
}
