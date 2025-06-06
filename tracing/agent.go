package tracing

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	"github.com/topfreegames/pitaya/v2/logger"
	"go.uber.org/zap"
	"sync"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// copy from go-zero trace

// A Config is a opentelemetry config.
type Config struct {
	Name     string  `json:",optional"`
	Endpoint string  `json:",optional"`
	Sampler  float64 `json:",default=0.1"`
}

var (
	lock          sync.Mutex
	agentInstance *sdktrace.TracerProvider
)

// StartAgent restart an opentelemetry agent.
func StartAgent(c Config) error {
	lock.Lock()
	defer lock.Unlock()

	if agentInstance != nil {
		err := stopAgent()
		if err != nil {
			return err
		}
	}

	// if error happens, let later calls run.
	if err := startAgent(c); err != nil {
		return err
	}
	return nil
}

func StopAgent() error {
	lock.Lock()
	defer lock.Unlock()
	return stopAgent()
}

func stopAgent() error {
	if agentInstance == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := agentInstance.Shutdown(ctx)
	if err != nil {
		return errors.WithStack(err)
	}
	agentInstance = nil
	return nil
}

func startAgent(c Config) error {
	ctx := context.Background()
	opts := []sdktrace.TracerProviderOption{
		// Set the sampling rate based on the parent span to 100%
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(c.Sampler))),
		// Record information about this application in an Resource.
		sdktrace.WithResource(resource.NewSchemaless(semconv.ServiceNameKey.String(c.Name))),
	}

	if len(c.Endpoint) > 0 {
		// conn, err := grpc.DialContext(ctx, opt.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		conn, err := grpc.NewClient(c.Endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return errors.WithStack(fmt.Errorf("failed to create gRPC connection to collector: %w", err))
		}

		// Set up a trace exporter
		exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
		if err != nil {
			return errors.WithStack(fmt.Errorf("failed to create trace exporter: %w", err))
		}
		// Always be sure to batch in production.
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Zap.Error("[otel] error", zap.Error(err))
	}))
	agentInstance = tp
	return nil
}
