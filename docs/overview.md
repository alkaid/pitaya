# Overview

Alkaid Pitaya is a maintained v2 fork of the Pitaya distributed game-server
framework. The fork's module identity is `github.com/alkaid/pitaya/v2`, its
toolchain baseline is Go 1.26.5, and its runtime contracts differ materially
from the upstream project.

The framework separates public client traffic from internal cluster traffic.
Frontend servers own client connections and sessions. Backend servers receive
forwarded handler calls and application RPC. Standalone mode keeps those pieces
inside one process; cluster mode uses service discovery and NATS.

## Fork architecture

### Explicit application instance

An application is constructed through `Builder` and returned as an `App`
instance. Code should retain and use that instance rather than expecting a
mutable package-global default app. Process-level scheduler, ticker, and logger
state still exist, and builder setup can mutate process-global state. Construct,
build, and run only one `App` lifecycle per process; use separate processes for
multiple servers.

`NewDefaultBuilder` receives `config.PitayaConfig` by value but nested maps and
slices are not deep-copied. Finalize the builder and its nested configuration
before `Build`. `NewBuilderWithConfigs` is the separate entry point for reloadable
configuration. Security boundaries such as trusted proxy CIDRs are materialized
during `Build` and require a restart to change.

### Sessions and keyed work

Every client connection has a frontend session. A bound UID lets the framework
route pushes and stateful work consistently. Session identity and pool lookup
are optimized for request reads; lifecycle and backend binding mutations are
serialized by framework-owned state transitions.

Stateful pools execute work by integer key. Admission can fail and callers must
handle the returned error. Custom pools allow bounded running-task escape by
default for compatibility; the module-owned `persist` pool is strict. Escape is
a liveness mechanism, not a substitute for fencing or idempotency. See
{doc}`actor-keyed-executor` for the complete ordering model.

### Handlers, remotes, and subscribers

Handlers receive client requests and notifications. Remotes receive internal
RPC calls. Both can install a `TaskExecutor` through component options when work
must be dispatched to an application-owned queue.

Publish subscribers are registered separately from ordinary remotes. Fork
options are calculated per method, and request fanout uses a bounded worker set
with stable partial-response aggregation. The NATS transport is the supported
cluster path.

## Supported boundaries

| Surface | Current contract |
| --- | --- |
| Client listeners | TCP and WebSocket |
| Cluster RPC | NATS with credentials and subject ACLs |
| Service discovery | etcd through the default cluster builder |
| Session cache | Redis through the default builder |
| Metrics | Prometheus and StatsD reporters |
| Tracing | OpenTelemetry context propagation and optional OTLP export |
| gRPC cluster RPC | Retained compatibility code; disabled or network-isolated only |

Client libraries and cluster implementations from the upstream ecosystem may
remain protocol-compatible, but they have an independent release and security
posture. Treat them as upstream projects and verify the exact version before
deployment.

## Release status

The `main` branch is a source candidate and can contain unreleased breaking API
changes. Consumers should pin an explicit verified `-ak-` tag. The release
process does not treat a green unit suite as sufficient: module identity,
consumer, dependency, documentation, and NATS E2E evidence are separate gates.
See {doc}`testing` and {doc}`migration-v2-ak`.

## Project lineage

Pitaya was originally developed by TFG Co on top of nano and ideas from Pomelo.
The [upstream Pitaya project](https://github.com/topfreegames/pitaya) is retained
as historical upstream lineage, not as the installation or security-reporting
destination for this fork. Missing upstream commits are tracked individually in
{doc}`upstream-ledger`.
