Tracing
=======

Pitaya propagates traces and baggage using [OpenTelemetry](https://opentelemetry.io/).

## Configure tracing

Start the tracing agent before the application and stop it during shutdown:

```go
import (
	"context"

	"github.com/alkaid/pitaya/v2/config"
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
		Headers:  map[string]string{"authorization": "Bearer token"},
		TLS: config.TLSConfig{
			Enabled:    true,
			CAFile:     "/etc/pitaya/collector-ca.pem",
			ServerName: "otel-collector.internal",
		},
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

TLS is optional and disabled by default, preserving the existing plaintext
connection. When enabled, `CAFile` selects the collector trust roots,
`ServerName` overrides certificate name verification, and `CertFile` plus
`KeyFile` enable mTLS. The certificate and key must be configured together.
`Headers` are sent as OTLP/gRPC metadata and are never logged by Pitaya.

Configure every server type with a distinct service name and point it at an
OTLP/gRPC collector such as Jaeger or an OpenTelemetry Collector.

Additional resource attributes can be supplied through the standard `OTEL_RESOURCE_ATTRIBUTES` environment variable.
