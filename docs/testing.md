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

Docker Engine and Docker Compose v2 are required only for distribution and NATS
E2E checks. Unit, vet, consumer, module, and docs-lint lanes do not start Docker.

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
| `test-consumer` | Independent temporary consumer compiles current public contracts |
| `docs-lint` | Fork identity, upstream-link policy, README invariants, and ledger shape |

The fast gate does not prove NATS/etcd/Redis behavior, race safety, coverage,
published tags, or dependency safety.

There is intentionally no root `make verify` aggregate yet. Phase 5 must define
that command only after its integration, race, fuzz, coverage, and benchmark
contracts are executable. Do not introduce a green alias from `verify` to
`verify-fast`.

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
make test-e2e-nats
make test-distribution
```

The NATS E2E script creates a unique Compose project, binds dynamically selected
loopback ports, waits for health and application readiness, and owns cleanup for
that project. On failure it prints Compose state and bounded recent logs. A
second run must not depend on containers or files left by the first.

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

## Dependency scan

```sh
make scan-deps
```

The scanner writes per-module JSON under `build/govulncheck`. Exit status `0`
means no reachable unaccepted finding, `3` means at least one reachable finding,
and `1` means the scanner, database, or report parser failed. Both `1` and `3`
block release. The current OpenPGP advisory has no fixed version and remains a
known release blocker; the command is expected to report it rather than hide it.

## Race and focused verification

Concurrency changes must run the affected packages with `-race` and repeated
counts chosen for the risk. The full targeted set is:

```sh
go test -race ./co ./session ./agent ./cluster ./service ./router -count=10
```

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

Run a base SHA and candidate SHA in separate worktrees on the same fixed runner
with identical `BENCH_PATTERN`, `BENCH_TIME`, and `BENCH_COUNT`, then compare the
two raw files. Report time, bytes, and allocations; shared-runner results remain
informational, and a network benchmark does not replace a production profile.

Runtime gRPC benchmarking is unsupported because this fork supports NATS as its
business transport. `benchmark-test-grpc` now exits with a migration message;
gRPC remains compile-only and must be disabled or isolated in deployments.

## Failure artifacts

- dependency JSON: `build/govulncheck/*.json`;
- default documentation output: `build/docs`;
- unit coverage: `coverprofile.out`;
- stamped coverage provenance: `coverprofile.out.meta.json`;
- NATS E2E diagnostics: emitted directly by `scripts/test-e2e-nats.sh` before
  project cleanup;
- failed NATS benchmark output: `<BENCH_OUT>.failed-<project>`;
- distribution scratch data: temporary and removed by the owning script.

Do not rerun a failed lane until it turns green without first preserving its
diagnostic output and determining whether the failure is product behavior,
infrastructure failure, or a reproducible pre-existing baseline.
