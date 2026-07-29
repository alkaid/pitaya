Tracing
=======

Pitaya propagates traces and baggage using [OpenTelemetry](https://opentelemetry.io/).

## Configure tracing

Start the tracing agent before the application and stop it during shutdown:

```go
import (
	"context"

	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/tracing"
	"go.uber.org/zap"
)

func main() {
	ctx := context.Background()
	err := tracing.StartAgent(ctx, tracing.Config{
		Name:     "my-pitaya-server",
		Endpoint: "localhost:4317",
		Sampler:  1,
	})
	if err != nil {
		logger.Zap.Fatal("failed to start tracing", zap.Error(err))
	}
	defer func() {
		if err := tracing.StopAgent(); err != nil {
			logger.Zap.Error("failed to stop tracing", zap.Error(err))
		}
	}()

	// Build and start the Pitaya application.
}
```

`Name` becomes the OpenTelemetry service name. `Sampler` is the trace sampling ratio from `0` to `1`. When `Endpoint` is set, Pitaya exports spans over OTLP/gRPC; leave it empty to install propagation and sampling without an exporter.

Configure every server type with a distinct service name and point it at an OTLP/gRPC collector such as Jaeger or an OpenTelemetry Collector. The built-in exporter uses an insecure gRPC connection, so keep the collector endpoint on a trusted network.

Additional resource attributes can be supplied through the standard `OTEL_RESOURCE_ATTRIBUTES` environment variable.
