# Testing the fork

The repository separates fast source checks from external-service,
distribution, security, and long-running evidence. A green command only proves
the lane it names.

## Toolchain

The source and all first-party Go modules require Go 1.26.5. Keep
`GOTOOLCHAIN=local` in automation so a runner does not silently download a
different compiler. Protobuf regeneration additionally requires `protoc` 35.1.
Documentation uses Python 3.12 or newer with the exact packages in
`docs/requirements.txt`.

Docker Engine and Docker Compose v2 are required only for Redis integration,
distribution, NATS E2E, network benchmark, and NATS stress checks. Unit, vet,
consumer, module, and docs-lint lanes do not start Docker. Redis integration
requires GNU coreutils `timeout` (Homebrew's `gtimeout` is detected on macOS).
NATS E2E and the full `make verify` require Linux and GNU `timeout`: the
testing-server process tree relies on Linux parent-death signaling. The standalone
NATS lane fails before starting Docker on other platforms; `make verify` reaches
that check after its Redis integration lane.

## Fast source gate

```sh
make verify-fast
```

This target contains:

| Target | Evidence |
| --- | --- |
| `tools-verify` | Pinned Go tools, GitHub Actions, and container image policy |
| `module-verify` | Workspace plus standalone root/CLI/xk6 module identity and graph |
| `logrus-verify` | Removed first-party Logrus packages do not reappear |
| `compile` | Root, CLI, and xk6 packages compile |
| `test-unit` | Root package unit tests run once |
| `vet` | Root production and test packages pass `go vet` |
| `test-fuzz-seeds` | Every registered fuzz target and committed corpus entry runs and passes |
| `test-consumer` | Independent temporary consumer compiles current public contracts |
| `docs-lint` | Fork identity, upstream-link policy, README invariants, and ledger shape |

The fast gate does not prove NATS/etcd/Redis behavior, race safety, coverage,
long-running fuzz mutation, published tags, or dependency safety.

The explicit engineering-health aggregate is:

```sh
make verify
```

It runs `verify-fast`, `test-race`, `test-integration`, and `test-e2e-nats`
sequentially. Required targets clear inherited `GOFLAGS` and fix their workspace
mode so caller settings cannot skip tests or alter the source graph. This broad
command is not the ordinary pull-request or lightweight release gate. Coverage,
distribution, dependency scanning, nightly fuzz mutation, benchmarks, stress,
and published-artifact verification remain independent evidence lanes.

## Documentation

Install the pinned Sphinx environment once, then pass its executable to the root
target:

```sh
python3 -m venv build/docs-venv
build/docs-venv/bin/python -m pip install -r docs/requirements.txt
make docs-lint
make docs SPHINXBUILD="$PWD/build/docs-venv/bin/sphinx-build"
```

`make docs` defaults to `-W --keep-going`, so warnings fail the build. Output is
written under ignored `build/docs` by default. Override `DOCS_BUILDDIR` for an
isolated CI artifact; do not weaken `SPHINXOPTS` in a required gate.

## External-service and distribution lanes

```sh
make test-integration
make test-e2e-nats
make test-distribution
```

The Redis integration harness starts only the digest-pinned Redis service in a
unique Compose project. It uses a dynamic literal IPv4 loopback port, health-based
readiness, bounded client timeouts, and a short-lived sentinel written from
inside the container. The tagged test refuses to write until that sentinel
matches and its UUID session key is absent, then exercises the public session
pool and Redis cache APIs and checks the exact five-field hash. Cleanup deletes
only that confirmed-owned key and removes only the harness's project and volumes.
Missing configuration, an invalid address, a zero-match test, a skipped test, or
failed teardown is an error. The Tests workflow runs this lane only on `main`
pushes and manual dispatches.

The NATS E2E script creates a unique Compose project, binds dynamically selected
loopback ports, waits for health and application readiness, and owns cleanup for
that project. On failure it prints Compose state and bounded recent logs; failed
teardown also fails the lane. A second run must not depend on containers or files
left by the first.

The distribution lane creates a local file proxy for synthetic root, CLI, and
xk6 versions. It verifies no-replace module graphs, versioned `go install`, a
host xk6 binary, a tracked-source Docker build, OCI revision metadata, and
constructor smoke tests. It does not publish a tag or contact the public Go
proxy.

## Coverage

```sh
make coverage
make coverage-stamped
make coverage-gate \
  COVERAGE_DIFF_BASE="$(git rev-parse HEAD^)" \
  COVERAGE_DIFF_HEAD="$(git rev-parse HEAD)"
```

`make coverage` keeps the reporting workflow available on an uncommitted
working tree. The regression gate requires `make coverage-stamped`, which runs
the same root tests with committed root and coverage-verifier sources, then
atomically writes `<profile>.meta.json`. The sidecar binds the exact profile bytes to the checked
out commit and the policy platform; failed generation removes the prior
sidecar. `check` rejects a changed profile, a different checkout, symbolic or
equal diff endpoints, and a policy baseline that is not an ancestor of the
tested head.

The root-module gate reads `scripts/coverage-baseline.json`. Its literal file
list excludes generated code, mocks, examples, and test-only process or
benchmark helpers. It fails when the aggregate or any included package drops
below its exact baseline ratio, when the included package set changes, or when
changed executable production lines are below 90% coverage. Base and head must
be full commit object IDs. The policy and CI lane cover the Linux/amd64 build
graph. A changed platform-specific production file that has no Linux profile
entry fails closed rather than being silently omitted.

The root gate skips only the separately verified `pitaya-cli`, `tools`, and
`xk6-pitaya` modules. Adding or removing a nested `go.mod` fails the gate until
the module has an explicit compile/test lane and is registered in the tool.

The current conservative baseline is 4184/7308 statements (57.25%), generated
from three independently green runs so timing-dependent cluster coverage does
not become a flaky floor. The Phase 5 targets remain 70% overall and 80% for
`cluster`, `co`, `service`, and `session`; they are recorded in the policy but
remain non-blocking until behavior tests raise every package to those targets.
The gate therefore prevents regressions without claiming that Phase 5 coverage
is complete.

Refresh the baseline only after an intentional coverage-policy review. Supply a
full commit SHA and one or more independently green profiles; the update command
uses the lowest complete-run package counts and rejects incompatible runs,
mixed commits, modified profiles, or a commit other than the current checkout.

```sh
make coverage-stamped COVERAGE_PROFILE=/tmp/coverage-1.out
make coverage-stamped COVERAGE_PROFILE=/tmp/coverage-2.out
make coverage-baseline \
  COVERAGE_BASE_SHA="$(git rev-parse HEAD)" \
  COVERAGE_PROFILES="/tmp/coverage-1.out /tmp/coverage-2.out"
```

The manually dispatched Tests workflow requires `coverage_base_sha`, a full
commit SHA that is an ancestor of the selected head. Pull requests compare the
tested merge commit with its first parent; pushes compare the event's exact
before and after commits.

## Fuzzing

`scripts/fuzz-targets.json` is the canonical inventory for root-module fuzz
targets. The gate parses the active Go test files from `go list` and requires an
exact, non-empty match between source and manifest, so adding or removing a
target without updating the manifest fails before any test starts.
Build-constrained fuzz targets are rejected when they are inactive in the
Linux CI build graph. Keep root targets portable, or add a separate manifest
and execution lane for the constrained platform before adding one.

Run the committed seed corpus with the same command used by the fast gate:

```sh
make test-fuzz-seeds
```

Run all registered targets with the nightly defaults, or select one registered
target while developing:

```sh
make test-fuzz-nightly
make test-fuzz-one \
  FUZZ_PACKAGE=./conn/message \
  FUZZ_TARGET=FuzzMessageDecode \
  FUZZ_TIME=30s
```

The all-target command runs targets sequentially for two minutes each by
default. Override `FUZZ_SEED_TIMEOUT`, `FUZZ_TIME`, `FUZZ_MINIMIZE_TIME`,
`FUZZ_TIMEOUT`, `FUZZ_PARALLEL`, or `FUZZ_ARTIFACT_DIR` without editing the
manifest. A single-target request must match a manifest entry; an unknown target
fails even on Go versions that return success for an empty `-run` or `-fuzz`
match.

The verifier reads `go test -json`, rather than trusting only the process exit
code. A seed pass requires the target and every `f.Add` or committed
`testdata/fuzz` child to run and pass. A mutation pass additionally requires a
target-scoped Go fuzz progress event whose total executions exceed the completed
baseline corpus count. Package logs, baseline-only runs, skipped corpus entries,
missing targets, and zero-mutation runs therefore cannot produce a green gate.
The all-target nightly command clears Go's fuzz cache first; only
source-controlled corpus files are accepted as durable nightly inputs. The
single-target developer command preserves the local cache.

Pull requests, pushes to `main`, and manual Tests runs execute the seed gate.
`.github/workflows/nightly-fuzz.yaml` runs the mutation gate daily and on manual
dispatch. Failures preserve per-target JSON event streams and stderr under
package SHA-256 and target SHA-256 directory levels in `build/fuzz`. Each target
directory includes `target.json`, which records the original package and target
behind those hashes. The upload also includes any generated `testdata/fuzz`
input. The workflow writes `build/fuzz/gate.log` before invoking the verifier,
so setup, compilation, and manifest failures retain their command output too.
The fuzz step has its own timeout so the job retains a separate upload window
after a hang. Reproduce a generated input with
`test-fuzz-one`, keep it only when it deterministically fails the old behavior
and passes the fix, and commit the minimized corpus file with the regression.

## Dependency scan

```sh
make scan-deps
```

The scanner writes per-module JSON under `build/govulncheck`. Exit status `0`
means no reachable unaccepted finding, `3` means at least one reachable
unaccepted finding, and `1` means the scanner, database, report parser, or
exception policy failed. Both `1` and `3` block release.
`scripts/vuln-exceptions.txt` contains reviewed, module-scoped exceptions; an
expired, malformed, out-of-scope, or unused entry returns `1`. Expiry dates are
inclusive UTC dates.

`GO-2026-5932` is accepted through 2027-08-05 for the root, CLI, and xk6
modules because no fixed version is available and production uses only ordinary
etcd remote configuration, not OpenPGP-encrypted remote configuration. The gate
continues to print the finding as `ACCEPTED RISK ... (not fixed)` on every scan.

## Race and focused verification

The complete targeted race lane is:

```sh
make test-race
```

It runs `co`, `session`, `agent`, `cluster`, `service`, and `router` ten times
under the race detector, with a fixed 10-minute test timeout. The Tests workflow
runs this lane on pushes to `main` and on manual dispatch, with a 15-minute job
timeout so checkout and setup do not consume the test's diagnostic window. It is
not part of `verify-fast` or the default pull-request matrix. The target forces
`GOWORK=off` and clears inherited `GOFLAGS`, so caller settings cannot skip or
narrow the required packages and repetitions.

Concurrency changes must still run the affected packages with `-race` and
repeated counts chosen for the risk before they reach `main`.
Known baseline races or timing failures must be reproduced on the exact base
commit before being classified as unrelated. A passing focused package does not
support a whole-repository race claim.

## Generated code

`make protos-compile protos-compile-demo` requires the pinned protoc and Go
plugins. Generated files are valid only when a subsequent `git diff --exit-code`
is empty. Mock targets use the pinned `mockgen`; regenerate the complete affected
interface surface and verify that a second generation is identical.

## Network benchmark harness

`make benchmark-test-nats` runs one report sample. `make benchmark-regression`
runs ten samples by default and writes raw, benchstat-compatible output to
`build/benchmarks/nats-pitaya-benchmark-<run>.txt`. Override the workload without
editing the harness:

```sh
BENCH_PATTERN='BackHandler' BENCH_TIME=2s BENCH_COUNT=10 \
  BENCH_OUT=build/benchmarks/candidate.txt make benchmark-regression
```

`BENCH_COUNT` accepts 1 through 100. Iteration-form `BENCH_TIME` is limited to
`1x` through `1000000x`; duration-form values remain bounded by the positive,
non-zero `BENCH_TIMEOUT` (20 minutes by default). Timeout values use one or more
integer Go duration components such as `90s` or `1m30s`; the minimum is `1ns`.
`BENCH_OUT` must name a regular file, not a directory or device. Concurrent runs
must use distinct outputs; an adjacent lock directory rejects accidental reuse,
and a lock left by `SIGKILL` must be removed after confirming that no matching
run is alive.

The harness creates a unique Compose project, leaves all dependency host ports
dynamic, waits for container health, and starts the backend and frontend as
managed process trees. Readiness requires successful connector and backend
requests, not a fixed sleep or a listening socket alone. The benchmark test
supervisor owns bounded TERM/KILL plus `Wait` for both servers and fails if
either exits unexpectedly; the shell script owns that supervisor, its benchmark
process, and only its Compose project. It publishes the requested output
atomically after successful cleanup and at least one benchmark sample. Failures,
including a pattern that matches no benchmark, leave the formal output unchanged
and preserve partial output as `<BENCH_OUT>.failed-<project>`. Diagnostics include
the last 200 dependency/server log lines. The harness never uses PID files or the
default Compose project. Managed servers also discard inherited `PITAYA_*`
configuration so a developer's production endpoints, listeners, or pool settings
cannot silently change the benchmark topology.

The multi-client benchmarks now select a client for every operation. The legacy
code used the constant final `b.N` as its index and usually sent every operation
through one client, so results produced by that implementation are not directly
comparable to results from this harness.

Concurrent benchmark responses are dispatched back to the caller waiting for
that message ID. A timeout or server error fails the sample; success no longer
means merely receiving an arbitrary known response from the shared client
channel. The production client still filters late or unknown response IDs before
the benchmark dispatcher sees them.

`make benchmark-regression` remains the configurable local raw-result producer;
it does not make a blocking performance decision. To produce a source-verified
base/candidate decision on a dedicated fixed runner, use:

```sh
BENCH_RUNNER_ID=linux-amd64-perf-01 \
  BENCH_RUNNER_LOCK=/var/lock/pitaya-benchmark.lock \
  BENCH_GATE_SOURCE_SHA=<reviewed-gate-commit-sha> \
  BENCH_POLICY_SHA256=<reviewed-policy-sha256> \
  BENCH_REPO_ROOT=/path/to/clean/candidate-checkout \
  BENCH_BASE_REF=<full-base-sha> \
  BENCH_CANDIDATE_REF=<full-candidate-sha> \
  make benchmark-gate-nats
```

Invoke the target from a clean, separately reviewed gate checkout whose current
commit exactly matches `BENCH_GATE_SOURCE_SHA`. `BENCH_REPO_ROOT` names the clean
trusted, maintainer-authored candidate checkout; its current `HEAD` must equal
`BENCH_CANDIDATE_REF`, and the
distinct base must be its ancestor. The candidate does not need to contain the
gate implementation or policy. Both source SHA and `BENCH_POLICY_SHA256` must
come from protected runner/workflow configuration; do not calculate either from
the candidate during the deciding run. The trusted gate exports `tools` and the
policy directly from the pinned commit, verifies the tools module cache, and
builds with `-mod=readonly -pgo=off`; ignored files or an automatic vendor/PGO
selection in the long-lived gate checkout therefore cannot alter the deciding
binary. Before any Git operation it clears inherited repository/object routing
and config/attribute injection, disables replacement objects, and rejects legacy
grafts or repository-private attributes. Every pinned SHA therefore resolves to
its original object graph and ancestry. After checkout, the trusted verifier
compares every tracked blob's raw object hash and executable mode with the pinned
tree before any build. Filtered, sparse, or otherwise transformed worktrees fail
closed. The gate applies the exported policy bytes to both detached measured worktrees,
which permits a base that predates the policy file. The measured builds use the
same module/PGO flags. The gate verifies the
policy-selected measurement apparatus is byte-identical before it runs base
followed by candidate under one host-wide lock. The apparatus digest includes
the explicit policy paths and every source or embedded file selected by the
actual Linux/amd64 Go build graph inside those protected harness packages;
selected ignored or untracked files fail closed. Ordinary production dependencies
remain the measured input rather than being frozen as apparatus. A gate or policy
change needs separate review and pin rotation; it cannot self-authorize a
performance decision. The first run used to establish a newly introduced gate commit is
bootstrap evidence and remains informational until that commit and policy digest
have been independently reviewed and installed in protected configuration. The
fixed `nats-network-v1` policy
selects the exact network benchmark set, `1s` benchtime, ten samples,
`benchmem`, and a 20-minute per-run timeout. `time/op` and `allocs/op` block when
their medians exceed the base by more than 10%; exactly 10% passes. One normalized
float ULP is allowed at that inclusive boundary so benchfmt unit conversion cannot
turn decimal `100 ns/op` -> `110 ns/op` into a false regression. An all-zero
allocation baseline blocks any positive candidate sample. `B/op` is required
evidence but is currently informational.

Exit `0` is a passing decision, `3` is a completed regression decision, `2` is
invalid input, and `1` is an infrastructure, provenance, or comparability
failure. Both exit `0` and exit `3` atomically publish a complete directory under
`build/benchmarks/nats-gate-<run-id>` by default. It contains raw and sealed base
and candidate benchfmt files, JSON provenance sidecars, deterministic runner
snapshots from before and after each harness invocation, `benchstat` output, and
`gate.json` plus human-readable comparator output. Before publication, the
trusted verifier reconciles the comparator exit status with the structured
report and the externally supplied commit, run, runner, gate, policy, and
apparatus identities. The report carries all ten samples for each metric, so
verification recomputes medians, the inclusive 10% boundary, and the all-zero
allocation invariant instead of trusting summary booleans. `compare` validates
the sealed result and metadata contents and records their SHA-256 digests in the
report. The final verification independently reads the reviewed policy, checks
its configuration and benchmark inventory, and rehashes all four artifacts to
bind the exact bytes to the report before publication.
Infrastructure failures retain
available evidence under the corresponding `.failed-<run-id>` path; stale formal
output is never replaced. A post-rename rollback collision retains the adjacent
output lock as a quarantine marker rather than exposing an unlocked formal
result.

`BENCH_RUNNER_ID` is a label, not proof of dedicated hardware. Blocking use also
requires an otherwise idle runner with stable CPU allocation, toolchain, Docker
daemon, and Compose version; `BENCH_RUNNER_LOCK` must be an absolute host-wide
regular-file path. The protected runner/workflow, reviewed policy digest, and
reviewed clean gate source form the trust boundary; a self-selected runner ID is
not sufficient. The result records machine, boot, kernel, CPU/cgroup, Go, and
container-tool provenance. Deterministic probes bracket each harness run, and
`stamp` resamples the runner before sealing the after-probe, so observed drift
fails closed. No hosted GitHub Actions job currently claims this fixed-runner
result. Shared-runner and same-checkout runs remain informational, and a network
benchmark does not replace a production profile. The `benchgate stamp` command
is an internal orchestration primitive, not a supported way to turn an old raw
file into trusted evidence.

This lane is deliberately not a hostile-code boundary. The measured harness runs
under the same Unix identity as the orchestrator and can access that identity's
files and Docker daemon. Run it only for maintainer-reviewed commits; never grant
blocking authority to an untrusted pull request. Hostile-candidate isolation
requires a separate identity and mount/cgroup boundary with gate artifacts kept
outside the workload. The trusted candidate must not daemonize outside the
harness or change its affinity/cgroup. Closing the inherited runner-lock file
descriptor prevents accidental lock ownership from entering the workload, but
does not turn the workload into a sandbox.

Base runs before candidate without overlap. This removes simultaneous resource
contention but not residual thermal, DVFS, cache, or Docker warm-state bias. A
deciding runner must be idle and thermally stabilized; borderline results need a
counterbalanced follow-up or production profile. Relative output paths must stay
under `BENCH_REPO_ROOT`, and every output-parent component must be a real directory,
not a symlink.

Formal blocking runs must execute directly in Linux's initial cgroup namespace,
with each relevant cgroup hierarchy mounted from `/` and its complete membership
ancestry visible without nested mounts or overmounts. At least one CPU or cpuset
control must be readable. The kernel must expose nsfs namespace type/identity and
`statx` mount IDs so every consumed control file can be bound to the mountinfo
record. Containerized Docker/Kubernetes runners, hidden cgroup ancestors, and
older kernels without those identity primitives fail as infrastructure rather
than producing partially comparable evidence. Cgroup v1 `noprefix` hierarchies
are also unsupported because their control-file identity is ambiguous. Use such
environments only for informational runs.

Runtime gRPC benchmarking is unsupported because this fork supports NATS as its
business transport. `benchmark-test-grpc` now exits with a migration message;
gRPC remains compile-only and must be disabled or isolated in deployments.

## NATS connection stress

Run the Linux-only capacity lane with:

```sh
make test-stress-nats
```

The default workload holds 10,000 clients at once and validates one strict-MID
echo from every connection. It then performs 1,000 same-UID replacements with
fresh client instances at bounded concurrency. Each replacement requires the
server to observe the old owner close before bind completes, then immediately
pipelines session-ownership and unique echo checks on the stable bound identity.
It observes the old client close exactly once and checks that the replacement
still owns the UID afterward. A kick packet and peer EOF can race, so the server
may record either normal or rebind closure; all surrounding identity and count
checks remain mandatory. The gate requires all configured counts and the run
identity to match a fresh atomic JSON report; a skipped/zero-match test or stale
report cannot pass.

This lane records the soft file limit and Linux ephemeral port range. It fails
as an environment error when they cannot cover the configured resident and
replacement sockets; it never changes host limits or sysctls. Override
`STRESS_CLIENTS`, `STRESS_CONNECT_CONCURRENCY`, `STRESS_REQUEST_CONCURRENCY`,
`STRESS_RECONNECTS`, `STRESS_RECONNECT_CONCURRENCY`, `STRESS_REQUEST_TIMEOUT`,
`STRESS_INTERNAL_TIMEOUT`, `STRESS_CLEANUP_TIMEOUT`, `STRESS_TIMEOUT`,
`STRESS_NATS_MESSAGES_BUFFER`, or `STRESS_OUT` for local diagnosis. Reduced
settings are smoke evidence only, not a substitute for the default gate.

The formal gate declares a stress-only NATS message buffer of 16,384 and records
the value in its JSON report. This covers the burst of 10,000 `SessionClosed`
notifications produced while the resident clients are cleaned up. The
supervisor passes it only to the isolated test servers; the framework's
production default of 75 and its configuration semantics remain unchanged.
Raising or lowering the buffer or workload defines a different capacity profile
and must not be reported as passing the default gate. A slow-consumer or
dropped-message diagnostic remains a hard failure even when all client-side
count and ownership assertions pass.

`.github/workflows/nightly-stress.yaml` runs the defaults nightly and by manual
dispatch. The lane is deliberately absent from `verify-fast` and ordinary
release checks. The existing benchmark script remains the single owner of its
unique Compose project, server supervisor, workload process, cleanup, and
diagnostics. A successful run publishes the JSON report and adjacent `.log`
only after the supervisor honors TERM, its retained frontend/backend logs contain
no NATS slow-consumer or dropped-message warning, and the Compose project stops
cleanly. Needing KILL is a gate failure. Failures preserve available JSON and log
data as `<STRESS_OUT>.failed-<project>` and
`<STRESS_OUT>.failed-<project>.log`; available supervisor and server tails use
the same prefix with `.supervisor.log`, `.frontend.log`, or `.backend.log`.

## Failure artifacts

- dependency JSON: `build/govulncheck/*.json`;
- default documentation output: `build/docs`;
- unit coverage: `coverprofile.out`;
- stamped coverage provenance: `coverprofile.out.meta.json`;
- fuzz JSON/stderr and the gate marker: `build/fuzz`;
- minimized fuzz failures: `<package>/testdata/fuzz/<target>/<hash>`;
- Redis integration diagnostics: bounded Compose state and Redis log tails from
  `scripts/test-integration.sh` before owned-project cleanup;
- NATS E2E diagnostics: emitted directly by `scripts/test-e2e-nats.sh` before
  project cleanup;
- failed NATS benchmark output: `<BENCH_OUT>.failed-<project>`;
- NATS base/candidate decision bundles: `build/benchmarks/nats-gate-<run-id>`,
  with infrastructure failures retained at the adjacent `.failed-<run-id>` path;
- NATS stress JSON/log evidence: `build/stress`, including failed outputs named
  from `<STRESS_OUT>.failed-<project>`;
- distribution scratch data: temporary and removed by the owning script.

Do not rerun a failed lane until it turns green without first preserving its
diagnostic output and determining whether the failure is product behavior,
infrastructure failure, or a reproducible pre-existing baseline.
