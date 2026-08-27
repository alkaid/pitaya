# Pitaya API

The package API is published at
[pkg.go.dev](https://pkg.go.dev/github.com/alkaid/pitaya/v2). The examples below
describe the Alkaid fork on the current source branch; use
`github.com/alkaid/pitaya/v2` imports throughout.

## Build an application instance

Create a builder from a `config.PitayaConfig` value, configure it, and retain the
returned application instance:

```go
cfg := config.NewDefaultPitayaConfig()
builder := pitaya.NewDefaultBuilder(
    true,
    "connector",
    pitaya.Standalone,
    map[string]string{"region": "local"},
    *cfg,
)
builder.AddAcceptor(acceptor.NewTCPAcceptor(":3250"))

app := builder.Build()
app.Register(&Handler{}, component.WithName("user"))
app.Start()
```

Use `NewBuilderWithConfigs` only when the application owns a reloadable
`*config.Config`. Builder fields, nested map/slice configuration, serializers,
and route dictionary data must be finalized before `Build`. Mutation after
`Build` is unsupported: some maps and pointers remain shared while other values
have already been materialized, so callers must not depend on a later change
either taking effect or staying isolated. Construct, build, and run only one
`App` lifecycle per process because scheduler, ticker, logger, and metrics state
remain process-global.

`Builder` is intentionally single-use and is not concurrency-safe for public
field writes. Complete all configuration in one goroutine, then call `Build`
once. A concurrent or second `Build` panics. At Build entry Pitaya snapshots the
Builder-owned acceptor, post-build hook, and metrics reporter slices; later
field assignment cannot rewire the returned App. Mutation through shared
pointers or nested maps/slices remains unsupported.

Default Cluster dependencies are constructed as a graph and must be replaced
as consistent groups:

| Root replaced | Required companion replacement |
| --- | --- |
| `SessionPool` in Standalone mode | None |
| `SessionPool` in Cluster mode | Caller-owned `RPCServer` built from that pool |
| `MetricsReporters` in Cluster mode | Caller-owned `RPCClient` and `RPCServer` built from that reporter set |
| `Server` in Cluster mode | Caller-owned `ServiceDiscovery`, `RPCClient`, and `RPCServer` built from that server |
| `Redis` or session-cache construction config while retaining the default pool | Caller-owned `SessionPool` built from the new Redis/cache configuration |

Pitaya cannot inspect roots captured inside caller-owned transports. Supplying a
custom dependency group transfers its internal identity consistency to the
caller. Retaining only part of a default group fails before logger setup, Router
mutation, RPC server wiring, or post-build hooks; Pitaya never silently rebuilds
and discards a public transport instance that the caller may have configured.

## Handlers

Handlers are public component methods that receive a `context.Context` and an
optional pointer or `[]byte` payload. A request returns a pointer or `[]byte`
plus an error; a notify returns no values.

```go
package connector

import (
    "context"

    "github.com/alkaid/pitaya/v2/component"
)

type Handler struct {
    component.Base
}

type UserRequest struct {
    Name string `json:"name"`
}

type UserResponse struct {
    Accepted bool `json:"accepted"`
}

func (h *Handler) Login(ctx context.Context, req *UserRequest) (*UserResponse, error) {
    return &UserResponse{Accepted: true}, nil
}
```

With `component.WithName("user")`, a frontend route is
`connector.user.login` after the default name conversion. Register components
before `App.Start` so their lifecycle methods are owned by the application.

## Startup transaction and extension lifecycle

`App.Start` validates the complete handler/remote registration batch and all
framework module names before invoking any component lifecycle hook. Duplicate
services, reserved names, invalid method signatures, a remote component on an
App without `RemoteService`, or a framework module name conflict make startup
panic before modules or acceptors start. `IsRunning()` remains false.

Component and module initialization has a commit boundary. An `Init` hook that
panics or returns an error has not committed, so that hook must clean up every
resource it acquired before failing. Once `Init` returns successfully, the App
owns teardown and calls `BeforeShutdown` and `Shutdown` exactly once. A later
`Init`/`AfterInit` failure rolls back every successfully initialized owner in
reverse order. Cleanup errors and panics are joined with the primary startup
error while cleanup continues for the remaining owners.

`App.Start` keeps its existing no-return API and reports startup failure by
panicking with an error. Supervisors should treat that panic as a failed process
start; tests may use `errors.Is`/`errors.As` to inspect the original lifecycle or
registration error. Recovering and trying to reuse the same App is unsupported.

## Runtime registration

Use `LazyRegister` and `LazyRegisterRemote` only for components whose lifecycle
is owned outside the App. Runtime registration and interceptor replacement are
concurrency-safe and return errors directly to the caller:

```go
if err := app.LazyRegister(&RuntimeHandler{}, component.WithName("runtime")); err != nil {
    return fmt.Errorf("register runtime handler: %w", err)
}

if err := app.RegisterInterceptor("runtime", interceptor); err != nil {
    return fmt.Errorf("register runtime interceptor: %w", err)
}
```

`LazyRegister`, `LazyRegisterRemote`, `RegisterInterceptor`, and
`RegisterRemoteInterceptor` can fail for duplicate or invalid services, invalid
method signatures, nil interceptors, or a missing handler/remote capability.
Check every result. A successful service registration publishes all of its
methods in one immutable lookup snapshot; requests already executing may finish
with their previous handler pointer, while lookups started after registration
returns see the new snapshot. `GetHandlers` returns a map copy and must not be
used as a mutation API.

## Custom routing

`App.AddRoute` accepts a server type and `router.RoutingFunc`:

```go
func routeRoom(
    ctx context.Context,
    target *route.Route,
    payload []byte,
    servers map[string]*cluster.Server,
    sess session.Session,
) (*cluster.Server, error) {
    var selected *cluster.Server
    for _, server := range servers {
        if server != nil && (selected == nil || server.ID < selected.ID) {
            selected = server
        }
    }
    if selected == nil {
        return nil, errors.New("no room server available")
    }
    return selected, nil
}

if err := app.AddRoute("room", routeRoom); err != nil {
    return err
}
```

The default etcd service discovery supplies a shallow snapshot of the outer map
for the call. Its `*cluster.Server` values remain shared; treat both the map and
the values as read-only and do not retain them. The `ServiceDiscovery` interface
does not impose this copy behavior on custom implementations. Routing can use
the request context, decoded route, payload, and session.

## Remotes

Remotes receive internal cluster RPC. Register them with `App.RegisterRemote`.
RPC calls can select by server type or use `RPCTo`/`NotifyTo` for an exact server
ID. NATS is the supported production transport.

Remote methods receive `context.Context` plus an optional protobuf message and
return a protobuf message plus an error. Keep internal `sys.*` routes reserved
for the framework; they must never be exposed through a public client service.

## Publish subscribers

Use `App.RegisterSubscribe` for components reached by `Publish` or
`PublishRequest`. `component.WithForkMethods` and `component.WithForkAll` control
which methods are delivered to every eligible server instance. Options are
computed per method, so one method's fork setting does not leak to another.

Request fanout can return both partial responses and a joined error. Consumers
that can use partial data should inspect both values.

## Task executors

Install application scheduling with `component.WithTaskExecutor`:

```go
executor := component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
    return task(ctx)
})

app.Register(
    &Handler{},
    component.WithName("user"),
    component.WithTaskExecutor(executor),
)
```

`Execute` may be called concurrently. It must not retain `task` after returning,
and must either complete one invocation or return a rejection without invoking
it. Return `component.ErrTaskSkipped` only for an intentional, unstarted skip.
See {doc}`migration-v2-ak` for queued examples and error semantics.

## Stateful pools

`StatefulPool.Go` returns `(<-chan struct{}, error)` and `StatefulPool.Wait`
returns `error`. A successful completion channel closes after the task exits,
including a recovered panic. Context cancellation after admission does not
revoke the queued task, so callers that lend state to a task must join its
completion before releasing that state.

## Examples

Working source examples live in
[examples/demo](https://github.com/alkaid/pitaya/tree/main/examples/demo). Build
them from the same checkout so their imports and workspace resolve to the fork.
