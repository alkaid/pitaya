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
