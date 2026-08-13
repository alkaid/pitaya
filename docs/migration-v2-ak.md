# Migrating to the Alkaid v2 fork

This guide covers the unreleased API contract on the current `main` branch after
the verified `v2.11.1-ak-35` release. Pin the exact commit during migration and
do not publish an application upgrade until the framework has a verified tag.

## Module and toolchain

Use the fork module everywhere:

```go
import pitaya "github.com/alkaid/pitaya/v2"
```

The root module, `pitaya-cli`, and `xk6-pitaya` require Go 1.26.5. Remove any
application replace that maps the fork back to an upstream Pitaya checkout.
`xk6-pitaya` also moves from k6 v0.46.0 to v1.8.0 and exposes Sobek types in
place of Goja types. Rebuild custom extensions against the k6 v1 runtime and
update `goja.Promise`, `goja.Object`, and `goja.ConstructorCall` users to the
corresponding `github.com/grafana/sobek` types.

Generated gRPC stubs now require grpc-go 1.64 or newer and support package v9.
Servers embedding `protos.UnimplementedPitayaServer` must embed its value, not a
nil pointer. This is a compile-time migration even when gRPC stays disabled.

## Benchmark tooling

`benchmark-test-nats` now owns an isolated NATS/etcd/Redis Compose project,
managed testing servers, and their verified cleanup; it no longer depends on a
separate PID-file or Compose cleanup target. Use `BENCH_PATTERN`, `BENCH_TIME`,
`BENCH_COUNT`, `BENCH_TIMEOUT`, and `BENCH_OUT` instead of starting
`bench-nats-sv` or `ensure-testing-deps` manually. `benchmark-regression`
produces ten benchstat-compatible samples by default. A failed or zero-match run
no longer publishes `BENCH_OUT`; partial raw output is retained as
`<BENCH_OUT>.failed-<project>` for diagnosis. Cleanup failure also fails the run
and prints bounded diagnostics.

The old `ensure-testing-bin`, `ensure-testing-deps`, `bench-nats-sv`,
`bench-grpc-sv`, `ensure-e2e-deps-grpc`, and `kill-testing-deps` targets were
removed. The compatibility `benchmark-test-grpc` target exits with a NATS-only
migration message. Keep gRPC compile coverage, but migrate performance jobs to
`benchmark-test-nats`.

Managed benchmark servers deliberately discard inherited `PITAYA_*`
configuration and receive only the harness-owned dependency endpoints. Pass
workload controls through the documented `BENCH_*` variables rather than a
production Pitaya environment. The multi-client benchmarks now rotate clients
per operation and validate each response; the old implementation indexed every
operation with the constant `b.N` and normally exercised one client, so old and
new multi-client results are not equivalent baselines. `BENCH_COUNT` is limited
to 1..100, iteration-form `BENCH_TIME` to 1x..1000000x, `BENCH_TIMEOUT` must be a
positive duration, and `BENCH_OUT` must name a regular file. Concurrent runs must
use distinct output paths; an adjacent lock directory rejects reuse. Timeout
values use integer Go duration components and must resolve to at least `1ns`.

`benchmark-regression` keeps that local, configurable raw-output contract. A new
`benchmark-gate-nats` target performs the stricter fixed-policy comparison and
does not reuse `BENCH_PATTERN`, `BENCH_TIME`, `BENCH_COUNT`, or `BENCH_OUT`.
Formal use now goes through `scripts/run-benchmark-gate-controller.sh` on a
fixed bare host. The maintainer supplies only complete candidate and base SHAs
(plus an optional repository identity); the controller obtains
`BENCH_GATE_SOURCE_SHA` and `BENCH_POLICY_SHA256` exclusively from the protected
`benchmark-gate` Environment. The candidate must equal clean local `HEAD` and
current origin `refs/heads/main`; the base must be a distinct ancestor. Neither
measured revision needs to contain the verifier or policy. The same trusted policy is
applied to both source trees, so the base may predate the policy file. Gate or
policy changes require separate review and pin rotation; the first bootstrap run
of a new gate commit is informational until that pin is independently trusted.
The candidate must be maintainer-reviewed and trusted: the same-UID harness is not
an isolation boundary for hostile pull-request code. The gate exports tools and
policy from the pinned commit, verifies the tools module cache, and fixes all
trusted/measured builds to module mode with automatic PGO disabled. It also
clears inherited Git repository/object/config routing, disables replacement
objects, and rejects legacy graft or private-attribute metadata before resolving
any pinned commit. Every detached worktree's raw tracked blobs and executable
modes must then match its pinned tree; clean/smudge, sparse, LFS, or other
transformed checkouts fail closed. Existing
automation that relies on vendor mode or `default.pgo` must not use this formal
target without defining and reviewing a different apparatus policy.
Each harness run is bracketed by runner-state probes, and publication verifies
that the structured report agrees with the comparator exit status and pinned
identities. The apparatus fingerprint includes files selected by the actual
Linux/amd64 build graph inside policy-protected harness packages; selected
ignored or untracked files fail closed. `gate.json` preserves all ten metric
samples so the verifier can recompute the median, inclusive threshold (with one
normalized-float ULP of representation margin), and zero-allocation decision.
`compare` validates the sealed result and metadata contents and records their
digests. Final verification independently checks the reviewed policy's
configuration and benchmark inventory, then rehashes all four artifacts to bind
their exact bytes plus the externally supplied policy and apparatus identities
to the report. Exit status `3` means a complete regression decision whose artifact
bundle was still published; it must not be treated as an infrastructure failure
or discarded. The deciding process must be a bare-host process in Linux's initial cgroup
namespace. Relevant hierarchies must be mounted from `/` with complete ancestry,
no nested mount or overmount, and at least one readable CPU/cpuset control. The
kernel must expose nsfs namespace identity/type and `statx` mount IDs. Docker or
Kubernetes runners and older kernels without those proofs fail closed and are
supported only for informational runs. Cgroup v1 `noprefix` hierarchies are also
unsupported.

Create the `benchmark-gate` Environment with custom deployment policy branch
`main` and these two Environment variables:

```text
BENCH_GATE_SOURCE_SHA=746624aa8874b7305fa913901c7b434fbc360a36
BENCH_POLICY_SHA256=b566bf3367e53302ef20342057b2a5c2de78436dbc0eaf85d5fd968844090f4b
```

Remove repository or organization variables with either name; do not pass the
pins through the host environment or controller arguments. The repository must
have zero self-hosted GitHub runners. Prepare the fixed local bare Linux/X64 host
with Go 1.26.5, Docker/Compose, the required cgroup visibility, and writable
`/var/lock/pitaya`, then run:

```sh
scripts/audit-benchmark-gate-config.sh --repo alkaid/pitaya
```

The audit uses exact-name API reads for the Environment pins and lower-scope
shadows, verifies the exact `main` deployment policy, zero repository runners,
and policy bytes at the pinned gate commit. Any unavailable or concealed API
result, missing value, shadow variable, runner registration, or digest mismatch
fails closed. Because the two GitHub variable updates are not atomic, disable
the controller, update both, audit, then run one informational bootstrap before
restoring formal use:

```sh
scripts/run-benchmark-gate-controller.sh --bootstrap <candidate-sha> <base-sha>
```

The controller reads the complete pin document before and after execution. The
low-level gate publishes completed `0`/`3` evidence under a non-formal pending
name. Bootstrap evidence remains under `.bootstrap-controller-<run-id>` and
installs a host-local marker bound to the complete audited tuple; it never uses a
formal output name. A normal run requires the marker, an unchanged post-run
tuple and remote `main`, and successful temporary-worktree cleanup before it can
promote without clobbering to `nats-gate-<run-id>`. Signals are forwarded to the
owned gate process. All controller failures return `1` and retain only explicitly
non-formal evidence. Source installation, a bootstrap run, and formal blocking
evidence remain three distinct states.

Formal output parents cannot contain symlinks, and relative paths remain below the
candidate checkout. The runner-lock descriptor is closed before starting measured
code. Base still runs before candidate, so operators must stabilize the runner and
investigate borderline changes with counterbalanced runs or production profiles;
the no-overlap rule removes concurrent contention, not thermal/cache order effects.

## Coverage workflow

Manual runs of the Tests workflow now require a full `coverage_base_sha` input.
Update API or CLI automation that previously dispatched the workflow without
inputs, for example:

```sh
gh workflow run tests.yaml -f coverage_base_sha="$(git rev-parse HEAD^)"
```

The coverage job compares that exact commit with the selected head and rejects
a non-ancestor manual base. Pull requests use the tested merge commit's first
parent, while pushes use the event's exact before commit. Pull-request runs no
longer upload to Coveralls; they still enforce the local baseline and 90% diff
coverage gate. Generated code, mocks, examples, and test-only helpers are
excluded only through the literal paths in `scripts/coverage-baseline.json`.

Coverage scripts that invoke the regression gate must generate the profile with
`make coverage-stamped`, not a standalone `go test -coverprofile` command. The
new sidecar binds the profile hash to a full commit object ID and the
Linux/amd64 policy; `check` and `coverage-baseline` reject missing, modified, or
mixed-commit sidecars. Stamped generation requires committed root sources and
committed `tools/cmd/coveragegate` module inputs. `make coverage`,
`test-coverage-html`, and `coverage-func` retain their previous
WIP reporting behavior and do not require provenance.

Repositories derived from this fork must also register a verification lane
before adding another nested Go module. The root coverage gate recognizes only
`pitaya-cli`, `tools`, and `xk6-pitaya`; an unregistered nested `go.mod` now
fails closed instead of being silently skipped by `go test ./...`.

## Message and error payload decoding

Compressed Pitaya message payloads and compressed client handshake responses
are now rejected when decompression would produce more than 16 MiB, matching
the packet envelope. Both compressed paths were previously unbounded. Raw,
uncompressed payload handling is unchanged.
Applications that intentionally send highly compressible payloads above this
limit must split or reduce them; callers of `message.Decode` should treat the
new limit error like any other invalid inbound frame. Clients also abort the
handshake before becoming connected when a compressed response exceeds the
limit. A truncated routable message that ends before its route-length byte now
returns `ErrInvalidMessage` instead of panicking.

Malformed JSON or protobuf error payloads now decode to the canonical HTTP 500
status with reason and message `unknown`. Previously, decode errors were
ignored, which could expose a partially decoded JSON status or a zero-value
protobuf status. Valid payloads keep accepting unknown fields, so forward
compatible producers do not need to strip newer fields. Fix any producer that
sends malformed error bytes; do not depend on partial status recovery.

## Stateful pool admission errors

`StatefulPool.Go` and the keyed `co.Go*` wrappers now return a completion channel
and an admission error. `StatefulPool.Wait` and the keyed `co.Wait*` wrappers now
return an error.

Before:

```go
done := pool.Go(ctx, key, task, false)
<-done

pool.Wait(ctx, key, task, false)
```

After:

```go
done, err := pool.Go(ctx, key, task, false)
if err != nil {
    return fmt.Errorf("submit keyed task: %w", err)
}
<-done

if err := pool.Wait(ctx, key, task, false); err != nil {
    return fmt.Errorf("run keyed task: %w", err)
}
```

Nil tasks, a pre-cancelled context, a closed pool, and overload can reject
admission. In those cases `done` is nil and must not be awaited.

Cancellation after successful admission does not revoke the task. `Wait` may
return `ctx.Err()` before an accepted task exits. If the task borrows request
state, a lock, a transaction, or a `WaitGroup` slot, join the successful `done`
channel before releasing that resource.

## TaskGoProvider to TaskExecutor

`TaskGoProvider`, its detached `final` callback, and `WithTaskGoProvider` were
removed. Register a `component.TaskExecutor` instead:

```go
type keyedExecutor struct {
    pool *co.StatefulPool
    key  func(context.Context) int
}

func (e *keyedExecutor) Execute(ctx context.Context, task component.Task) error {
    var taskErr error
    done, err := e.pool.Go(ctx, e.key(ctx), func(taskCtx context.Context) {
        taskErr = task(taskCtx)
    }, false)
    if err != nil {
        return err
    }

    // An accepted task must be joined even if ctx is cancelled meanwhile.
    <-done
    return taskErr
}

app.Register(
    &Handler{},
    component.WithTaskExecutor(&keyedExecutor{pool: pool, key: requestKey}),
)
```

`Execute` can be called concurrently; synchronize executor-owned mutable state.
It must finish one task invocation before returning, or return a submission error
without invoking the task. It must not retain the task. Use
`component.ErrTaskSkipped` only for an intentional skip before invocation. A
joined skip plus another error is treated as a contract failure, not a normal
skip.

Handler errors remain owned by the dispatcher. The executor should propagate
the error returned by `task`; it must not translate application status codes.

For a request whose task was never invoked, an explicit skip maps to 503,
pre-start cancellation maps to 499, and a pre-start deadline maps to 504. A
single explicit API error keeps its application status; other rejections,
panics, joined skip errors, and executor contract failures map to a stable 500.
An explicitly skipped notification is acknowledged because it has no response
body. These statuses describe admission, not handler business results.

## Backend callback ownership

Applications no longer own the fallible primary `OnBindBackend` and
`OnKickBackend` callbacks. Backend binding mutation, remote RPC, frontend sync,
cache flush, and rollback ordering are coordinated by the framework.

Application observation belongs in after callbacks:

```go
app.OnAfterBindBackend(onBound)
app.OnAfterKickBackend(onKicked)
```

Bind after-callback errors preserve the historical best-effort contract. Kick
after-callback errors are returned to the caller. Neither callback rolls back an
already committed mutation.

A custom `SessionPool` used with the built-in remote service must implement the
narrow `session.BackendCoordinatorInstaller` capability. A custom session used
by remote unbind handling must support identity-aware atomic backend removal;
there is no read-then-remove fallback. Do not add application primary callbacks
around those capabilities.

## Stateful pool module lifecycle

`NewStatefulPoolsModule` now accepts an optional variadic metrics period. Normal
two-argument calls still compile, but exact function values, dependency
injection bindings, and signature assertions must use the variadic type or wrap
the constructor in a two-argument closure.

Module shutdown now releases every module-owned pool after stopping its monitor.
New submissions then fail with `ErrPoolClosed`. Shutdown does not cancel or wait
for an already running blocked task, so stop admission and own task drain or
cancellation before shutting down the module.

## Stateless admission and startup

Legacy `co.Go` remains best-effort. Use `co.Submit` when the caller must know
whether a stateless task was accepted. `App.Start`, `App.StartWorker`, and
`Worker.Start` panic if their startup task cannot be admitted; code that needs
an error should call `Worker.StartWithError`.

## Session, subscriber, and client IP behavior

A closed session now rejects `Bind` and `StoreSessionLocal` with
`ErrCloseClosedSession`. A bind callback that re-enters `Bind` is rejected with
`ErrSessionAlreadyBound`. Do not reuse a closing session or bind recursively.

`WithForkMethods` is now evaluated independently for every subscriber method.
List every method that should fan out; a method omitted from the list no longer
inherits `fork=true` accidentally from map iteration order.

`util/net.SourceIPs` and `GetClientIP` now return only the socket peer. Direct
helper users that need forwarded headers must compile explicit CIDRs with
`ParseTrustedProxies` and call `GetClientIPWithTrustedProxies`. Never use
`0.0.0.0/0` or `::/0` as a trusted proxy range.

## Builder, proxy, and fanout configuration

`NewDefaultBuilder` is a configuration snapshot. It does not install dynamic
reload machinery; use `NewBuilderWithConfigs` when the process owns a reloadable
source. Configuration changes after `Build` are unsupported.

`PitayaConfig.Acceptor.TrustedProxyCIDRs` and
`ClusterRPCConfig.FanoutConcurrency` are new public configuration fields. The
trusted-proxy list defaults to empty, is parsed during `Build`, and requires a
restart to change. An invalid CIDR makes `Build` panic. Fanout concurrency uses
the Viper key `pitaya.cluster.rpc.fanout_concurrency`, defaults to 32 per call,
and falls back to 32 for non-positive values. It is not a process-wide limit.

Successful fanout responses are ordered by server ID. Partial failures return
the successful responses together with an `errors.Join` error, and cancellation
stops unclaimed work. Do not depend on completion order, nil placeholders, or
last-error-wins behavior.

The new fields break unkeyed `ClusterRPCConfig` literals and exact anonymous
struct literals for the acceptor configuration. Start from
`config.NewDefaultPitayaConfig` and set named fields.

## GoPool configuration literals

`config.GoPool` added exported escape budget and policy fields. External
unkeyed struct literals therefore fail to compile. Convert them to keyed
literals:

```go
roomPool := config.GoPool{
    Name:                   "room",
    Expire:                 10 * time.Minute,
    TaskBuffer:             100,
    RunningTaskTimeout:     10 * time.Minute,
    MaxEscapedWorkers:      128,
    MaxEscapedWorkersPerID: 1,
}
```

The new fields are `RunningTaskTimeout`, `MaxEscapedWorkers`, and
`MaxEscapedWorkersPerID`. A zero `RunningTaskTimeout` inherits the resolved
`Expire`. Ordinary pools resolve both unset values to 10 minutes; the
module-owned `persist` pool resolves them to 1 hour. The budget zero values
resolve to 128 workers per pool and 1 worker per ID. These bounds limit
replacement workers; they do not cancel a task that is already running.

The zero-value escape policy remains compatible with earlier releases: custom,
`session`, `user`, and `default` pools allow running-task escape. The module-owned
`persist` pool remains strict. Use the existing `DisablePurgeRunning: true` field
when a pool requires strict single-writer execution. Such a pool can stop making
progress behind a stuck task, while an escaping pool requires fencing, version
checks, or idempotency for overlapping old and replacement workers.

`TaskBuffer` also changed behavior with the keyed-pool dependency upgrade. `N`
is now the non-blocking admission watermark and the physical per-key channel
capacity is `2N`; blocking submissions can use the reserved half. Zero resolves
to 100, and the valid range is 0 through 65536. Review memory use and recursive
same-key blocking submissions before changing this value.

## Removed Logrus packages

The first-party `logger/logrus` adapter and `logger/test` helper were removed
without a compatibility shim. Migrate server logging to the fork's Zap-backed
logger surface. The client package still has its own logger hook; it is not a
replacement for a server-wide `SetLogger`.

## Generated test mocks

First-party mocks now use `go.uber.org/mock/gomock` and were regenerated from
the current interfaces. Public mock methods and their `EXPECT()` recorders may
therefore have new arguments or return values, including entity/agent close and
kick, session bind, and Pitaya RPC methods. Update downstream expectations and
regenerate application-owned mocks with the pinned Uber Mock tool instead of
editing generated files.

## Other observable behavior changes

- The `sys` component and route namespace is reserved for internal remotes,
  case-insensitively after trimming. Rename application routes that use it.
- Heartbeat acknowledgements with timestamps now use `HeartbeatAck`, not
  `HandshakeAck`. Update clients or protocol fixtures that expected the old
  packet type.
- Session close and kick cleanup is idempotent. Subscription slices are copied,
  and callers must not depend on repeated close side effects or live slice
  mutation.
- CLI file-mode command failures now return a non-zero exit code and preserve
  request correlation. Update scripts that treated the old zero status as
  success.

## Rollout checklist

1. Update every root, CLI, xk6, and application import to the fork identity.
2. Compile all `StatefulPool.Go/Wait` and `co.Go*/Wait*` call sites and handle
   every admission error.
3. Replace every `TaskGoProvider` and prove accepted work is joined.
4. Remove application primary backend callbacks; retain only after callbacks.
5. Update exact `NewStatefulPoolsModule` function values and stop admission
   before module shutdown.
6. Convert external `config.GoPool` literals to keyed form, review the new
   escape budgets, and set `DisablePurgeRunning` only where strict execution is
   required.
7. Update generated mock expectations, explicit subscriber fork lists, direct
   trusted-proxy helper callers, and fanout response/error consumers.
8. Run `make test-consumer`, application compile/race/vet, and the real NATS E2E
   lane against the exact framework commit.
9. Address fire-and-forget errors at application boundaries by propagation or
   structured logging; do not discard new admission failures.
10. Regress `sys` routes, heartbeat packets, session close idempotency, CLI file
   mode, and the configured `TaskBuffer` behavior.
11. Upgrade only after a fork tag is published and a no-replace consumer can
   download that exact tag from the public proxy.
