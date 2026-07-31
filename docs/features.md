# Features and support boundaries

Pitaya provides the pieces needed to host connection-oriented game services and
route work across a cluster. This page describes the Alkaid fork rather than the
upstream defaults.

## Application and lifecycle

- `Builder` returns an explicit `App` with its server identity, modules, session
  pool, handlers, remotes, and shutdown state. Process-level scheduler, ticker,
  logger, and metrics state limit the process to one constructed, built, and
  running `App` lifecycle.
- Components have `Init`, `AfterInit`, `BeforeShutdown`, and `Shutdown`
  lifecycle phases. Shared handler/remote components are initialized and shut
  down once.
- Modules can be ordered before or after other modules. Session modules can
  participate in graceful session draining.
- Value-based builders do not install reload machinery. Use
  `NewBuilderWithConfigs` when the process genuinely owns a reloadable source.

## Frontend servers and sessions

Frontend servers expose TCP or WebSocket acceptors. Each accepted connection
owns a session, which can be bound to a UID, store session data, receive pushes,
and bind to stateful backend instances.

The socket peer is the client address by default. Forwarded headers are used
only when the immediate peer matches a configured trusted-proxy CIDR. See
{doc}`security` before enabling any proxy feature.

Session identity reads and pool lookups are designed for request hot paths.
Bind, close, online state, subscriptions, and backend binding remain explicit
write-side transitions. Applications can install after callbacks, but the
fallible primary backend transaction belongs to the framework.

## Handlers and remotes

Handlers process public client routes. Remotes process trusted cluster routes.
Both are registered on an `App` instance and can use:

- component names and name conversion;
- receiver providers for request-scoped component selection;
- interceptors;
- a `TaskExecutor` for application-owned scheduling.

A `TaskExecutor` must either run its task exactly once to completion before
returning, or reject/skip it without running it. The framework detects early,
duplicate, late, and panic outcomes. See {doc}`migration-v2-ak` for the migration
from `TaskGoProvider`.

## Routing, publish, and fork

Routes use `serverType.service.method`. A custom `router.RoutingFunc` can select
an instance from the map returned by the configured service discovery. The
default etcd implementation shallow-copies the outer map but shares its
`*cluster.Server` values, which must remain read-only. Without a custom route,
stateless work selects an available instance while a session backend binding can
provide stateful affinity.

`RegisterSubscribe` registers publish subscribers. `WithForkMethods` and
`WithForkAll` opt individual methods into per-instance delivery. Publish and
fork request variants return stable partial responses together with an error
when only part of a fanout succeeds. Fanout concurrency is bounded per call; it
is not a process-wide admission limit.

## Keyed execution

Stateful pools serialize accepted work for the same integer key while allowing
different keys to run independently. `StatefulPool.Go` returns a completion
channel and an admission error; `StatefulPool.Wait` returns an admission or
context error. Callers must not use a nil completion channel after rejection.

Custom pools and the built-in `session`, `user`, and `default` pools keep the
compatible zero-value policy: they allow bounded running-task escape. The
module-owned `persist` pool remains strict. Set `DisablePurgeRunning` when a
pool requires strict execution. Escape can restore throughput after a stuck
task, but the escaped task is still alive and may still mutate state.

## Cluster services

NATS is the supported internal RPC path. Deploy it with authenticated server
identities and least-privilege subject ACLs. etcd is the default service
discovery implementation, and Redis backs the default session cache.

NATS RPC calls use the configured request timeout by default. A caller can
override one request by adding a Go duration string to the propagated context
under `constants.RequestTimeout`; this per-request override is not supported by
the retained gRPC path.

The retained gRPC implementation has no peer authentication or mTLS and does
not share all NATS keyed-dispatch semantics. It is not a supported business
transport. Keep it disabled; temporary compatibility use requires loopback,
sidecar isolation, or explicit bidirectional network ACLs.

## Other framework services

- Memory and etcd group services manage membership and group broadcasts.
- Acceptor wrappers can transform connections before dispatch. The built-in
  rate limiter remains available through the wrapper package and connection
  configuration.
- Session and application APIs support pushes and kicks. Worker-backed reliable
  RPC returns a job ID and requires idempotent remote handlers.
- Before/after pipelines can inspect or replace handler and remote inputs.
- Optional modules cover child processes, unique sessions, and the retained
  gRPC binding-storage compatibility path.

## Observability

The fork supports Prometheus and StatsD reporters, OpenTelemetry trace-context
propagation, optional OTLP export, and actor-pool lifecycle metrics. Pool event
consumers run even without a reporter so escape-budget exhaustion and dropped
transitions remain visible in structured logs.

Custom Prometheus histogram registration is not part of the current stable
contract: a naming/ownership decision is still pending. Do not infer collector
availability merely because `ReportHistogram` accepts a value.

## Client protocol and tools

The client protocol supports JSON and protobuf serializers. Set the serializer
on the `Builder` before `Build`. The repository contains:

- `pitaya-cli`, a REPL client built from the same Go workspace;
- `xk6-pitaya`, a k6 extension for JSON-serialized load scenarios;
- local examples under `examples/demo`.

These submodules do not yet advertise an unverified public `@latest` install.
Build them from the checkout or use a separately verified module tag.
