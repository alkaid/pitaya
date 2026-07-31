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
go tool cover -func=coverprofile.out
```

Coverage is currently reporting evidence, not proof that the planned Phase 5
package thresholds have been met. Generated code, mocks, examples, CLI glue, and
other exclusions must eventually be represented by a fixed baseline file rather
than a dynamic package-name filter.

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

## Legacy benchmark targets

`benchmark-test-nats` and `benchmark-test-grpc` are manual legacy tools. They
still use fixed ports, PID files, a fixed five-second sleep, and broad default
Compose cleanup. They are not CI, release, readiness, cleanup, or performance
regression evidence. Phase 5 must replace them with a unique project, dynamic
ports, real readiness, one cleanup owner, bounded TERM/KILL plus `Wait`, and
failure diagnostics.

Microbenchmarks should compare a base SHA and candidate SHA on the same fixed
runner with identical arguments. Report time, bytes, and allocations; do not
substitute a microbenchmark for a production profile.

## Failure artifacts

- dependency JSON: `build/govulncheck/*.json`;
- default documentation output: `build/docs`;
- unit coverage: `coverprofile.out`;
- NATS E2E diagnostics: emitted directly by `scripts/test-e2e-nats.sh` before
  project cleanup;
- distribution scratch data: temporary and removed by the owning script.

Do not rerun a failed lane until it turns green without first preserving its
diagnostic output and determining whether the failure is product behavior,
infrastructure failure, or a reproducible pre-existing baseline.
