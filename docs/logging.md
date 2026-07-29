Logging
===

Pitaya uses [Zap](https://pkg.go.dev/go.uber.org/zap) for framework and application logging.

## Configure the global logger

Initialize the framework logger before starting the application, then write structured logs through `logger.Zap`:

```go
import (
	"github.com/alkaid/pitaya/v2/logger"
	"go.uber.org/zap"
)

func configureLogging() {
	logger.InitLogMgr(true, "DEBUG")
	logger.Zap.Info("logging initialized", zap.String("component", "example"))
}
```

`InitLogMgr` selects development or production output and applies the requested Zap level. Code that needs an independent logger can construct one from a Zap configuration:

```go
log := logger.NewLogger(zap.NewProductionConfig())
if log == nil {
	panic("failed to initialize logger")
}
log.Log.Info("worker started")
```

## Migrate from the first-party Logrus packages

The deprecated `github.com/alkaid/pitaya/v2/logger/logrus` adapter and `github.com/alkaid/pitaya/v2/logger/test` helper have been removed. There is no compatibility shim and no general `pitaya.SetLogger` replacement.

For application and framework logs, replace the adapter with `logger.InitLogMgr`, `logger.NewLogger`, and structured calls through `logger.Zap` or the returned `*logger.Logger`. Where application code accepts `*zap.Logger`, tests can pass `zap.NewNop()` or use Zap's test utilities.

The client no longer accepts a Logrus level. Construct it with the current API and provide an explicit disconnect reason:

```go
import "github.com/alkaid/pitaya/v2/client"

c := client.New()
defer c.Disconnect(client.CloseReasonManual)
```

`worker.SetLogger` remains a separate legacy integration that accepts `logger/interfaces.Logger`. Applications that still call it must own that adapter while migrating; Pitaya no longer provides a Logrus implementation for it.
