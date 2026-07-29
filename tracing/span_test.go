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
	"errors"
	"testing"

	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func installTestTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	originalProvider := otel.GetTracerProvider()
	originalPropagator := otel.GetTextMapPropagator()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	t.Cleanup(func() {
		require.NoError(t, provider.Shutdown(context.Background()))
		otel.SetTracerProvider(originalProvider)
		otel.SetTextMapPropagator(originalPropagator)
	})
	return recorder
}

func TestExtractSpan(t *testing.T) {
	installTestTracer(t)
	ctx, span := StartSpan(context.Background(), "op")
	defer span.End()

	extractedCtx, spanCtx := ExtractSpan(ctx)

	assert.Equal(t, span.SpanContext(), spanCtx)
	assert.Equal(t, span.SpanContext(), trace.SpanContextFromContext(extractedCtx))
}

func TestExtractSpanInjectedSpan(t *testing.T) {
	installTestTracer(t)

	member1, err := baggage.NewMember("some_key", "12345")
	require.NoError(t, err)
	member2, err := baggage.NewMember("some-other-key", "42")
	require.NoError(t, err)
	bag, err := baggage.New(member1, member2)
	require.NoError(t, err)

	parentCtx := baggage.ContextWithBaggage(context.Background(), bag)
	parentCtx, span := StartSpan(parentCtx, "someOp")
	defer span.End()
	propagatedCtx := InjectSpan(parentCtx)
	remoteCtx := pcontext.AddToPropagateCtx(
		context.Background(),
		constants.SpanPropagateCtxKey,
		CarrierFromPropagateCtx(propagatedCtx),
	)

	extractedCtx, spanCtx := ExtractSpan(remoteCtx)

	assert.Equal(t, span.SpanContext().TraceID(), spanCtx.TraceID())
	assert.Equal(t, span.SpanContext().SpanID(), spanCtx.SpanID())
	assert.True(t, spanCtx.IsRemote())
	extractedBag := baggage.FromContext(extractedCtx)
	assert.Equal(t, "12345", extractedBag.Member("some_key").Value())
	assert.Equal(t, "42", extractedBag.Member("some-other-key").Value())
}

func TestExtractSpanNoSpan(t *testing.T) {
	installTestTracer(t)

	_, spanCtx := ExtractSpan(context.Background())

	assert.False(t, spanCtx.IsValid())
}

func TestExtractSpanBadInjected(t *testing.T) {
	installTestTracer(t)
	ctx := pcontext.AddToPropagateCtx(
		context.Background(),
		constants.SpanPropagateCtxKey,
		[]byte("unsupported"),
	)

	_, spanCtx := ExtractSpan(ctx)

	assert.False(t, spanCtx.IsValid())
}

func TestInjectSpanContextWithoutSpan(t *testing.T) {
	installTestTracer(t)

	ctx := InjectSpan(context.Background())

	assert.NotNil(t, CarrierFromPropagateCtx(ctx))
}

func TestInjectSpan(t *testing.T) {
	installTestTracer(t)
	originalCtx, span := StartSpan(context.Background(), "op")
	defer span.End()

	ctx := InjectSpan(originalCtx)

	carrier := CarrierFromPropagateCtx(ctx)
	assert.NotEmpty(t, carrier.Get("traceparent"))
}

func TestStartSpan(t *testing.T) {
	recorder := installTestTracer(t)

	ctx, span := StartSpan(
		context.Background(),
		"my-op",
		trace.WithAttributes(attribute.String("hi", "hello")),
	)
	assert.True(t, trace.SpanContextFromContext(ctx).IsValid())
	span.End()

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, "my-op", ended[0].Name())
	assert.Contains(t, ended[0].Attributes(), attribute.String("hi", "hello"))
}

func TestFinishSpanNilCtx(t *testing.T) {
	assert.NotPanics(t, func() { FinishSpan(nil, nil) })
}

func TestFinishSpanCtxWithoutSpan(t *testing.T) {
	assert.NotPanics(t, func() { FinishSpan(context.Background(), nil) })
}

func TestFinishSpanWithErr(t *testing.T) {
	recorder := installTestTracer(t)
	ctx, _ := StartSpan(context.Background(), "my-op")
	expectedErr := errors.New("hello")

	FinishSpan(ctx, expectedErr)

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, codes.Error, ended[0].Status().Code)
	assert.Equal(t, expectedErr.Error(), ended[0].Status().Description)
	assert.NotEmpty(t, ended[0].Events())
}

func TestFinishSpan(t *testing.T) {
	recorder := installTestTracer(t)
	ctx, _ := StartSpan(context.Background(), "my-op")

	FinishSpan(ctx, nil)

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, codes.Ok, ended[0].Status().Code)
}
