package tracing

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.32.0"
	"google.golang.org/grpc/credentials"
)

// copy from go-zero trace

// A Config is a opentelemetry config.
type Config struct {
	Name     string            `json:",optional"`
	Endpoint string            `json:",optional"`
	Sampler  float64           `json:",default=0.1"`
	TLS      config.TLSConfig  `json:",optional"`
	Headers  map[string]string `json:",optional"`
}

var (
	lock          sync.Mutex
	agentInstance *sdktrace.TracerProvider
)

// StartAgent restart an opentelemetry agent.
func StartAgent(ctx context.Context, c Config, opts ...sdktrace.TracerProviderOption) error {
	lock.Lock()
	defer lock.Unlock()

	if agentInstance != nil {
		err := stopAgent()
		if err != nil {
			return err
		}
	}

	// if error happens, let later calls run.
	if err := startAgent(ctx, c, opts...); err != nil {
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
	instance := agentInstance
	agentInstance = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := instance.Shutdown(ctx)
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func startAgent(ctx context.Context, c Config, opts ...sdktrace.TracerProviderOption) error {
	if err := c.TLS.Validate("pitaya.tracing.tls"); err != nil {
		return errors.WithStack(err)
	}
	resourceAttrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(c.Name),
	}
	if localIP := getLocalIP(); localIP != "" {
		resourceAttrs = append(resourceAttrs,
			attribute.String("ip", localIP), // 用于 discovery processor 匹配 https://grafana.com/docs/alloy/latest/reference/components/otelcol/otelcol.processor.discovery/#arguments
		)
	}
	if hostname, err := os.Hostname(); err == nil {
		resourceAttrs = append(resourceAttrs,
			attribute.String("hostname", hostname), // 同上 用于 discovery processor
		)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(resourceAttrs...),
		resource.WithFromEnv(), // 自动从环境变量获取资源属性
		//resource.WithHost(),    // 自动添加主机信息
	)
	if err != nil {
		return errors.WithStack(fmt.Errorf("failed to create resource: %w", err))
	}
	opts = append([]sdktrace.TracerProviderOption{
		// Set the sampling rate based on the parent span to 100%
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(c.Sampler))),
		// Record information about this application in an Resource.
		sdktrace.WithResource(res),
	}, opts...)

	if len(c.Endpoint) > 0 {
		exporterOptions := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(c.Endpoint),
			otlptracegrpc.WithHeaders(cloneHeaders(c.Headers)),
		}
		clientTLS, err := config.LoadClientTLS("pitaya.tracing.tls", c.TLS)
		if err != nil {
			return errors.WithStack(fmt.Errorf("failed to configure trace exporter TLS: %w", err))
		}
		if clientTLS == nil {
			exporterOptions = append(exporterOptions, otlptracegrpc.WithInsecure())
		} else {
			exporterOptions = append(exporterOptions, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(clientTLS)))
		}

		exp, err := otlptracegrpc.New(ctx, exporterOptions...)
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

func cloneHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	cloned := make(map[string]string, len(headers))
	for key, value := range headers {
		cloned[key] = value
	}
	return cloned
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}
