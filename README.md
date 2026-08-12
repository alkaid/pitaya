# Alkaid Pitaya

[![Build Status](https://github.com/alkaid/pitaya/actions/workflows/tests.yaml/badge.svg)](https://github.com/alkaid/pitaya/actions/workflows/tests.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/alkaid/pitaya/v2.svg)](https://pkg.go.dev/github.com/alkaid/pitaya/v2)
[![MIT licensed](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

This repository is the Alkaid-maintained Pitaya v2 fork. Its Go module is
`github.com/alkaid/pitaya/v2`, and the source tree requires Go 1.26.5. It is a
distributed game-server framework with an explicit application lifecycle,
sessions, routing, NATS RPC, and keyed task execution.

The current `main` branch contains unreleased, compile-time API changes. The
last independently verified public fork release is `v2.11.1-ak-35`:

```sh
go get github.com/alkaid/pitaya/v2@v2.11.1-ak-35
```

Pin an explicit `-ak-` version. Do not use `@latest` until a newer stable fork
tag is published and verified: inherited upstream stable tags can sort above
fork prereleases while declaring a different module path.

## Fork contract

The fork deliberately differs from the upstream product in several areas:

- the builder returns an explicit `App` instead of exposing a mutable default
  application API; construct, build, and run only one `App` lifecycle per process;
- UID identity snapshots use atomic request-path reads and session-pool lookup
  uses `sync.Map`; lifecycle and backend-binding mutations have explicit
  framework ownership;
- keyed pools report admission errors, bound running-task escape, and expose
  lifecycle metrics instead of silently accepting all work;
- component scheduling uses `TaskExecutor`, whose completion and rejection
  contract is owned and checked by the framework;
- routing, publish subscribers, and per-instance fanout are supported on the
  NATS cluster path;
- client address forwarding is disabled unless the socket peer matches a
  trusted-proxy CIDR compiled when the `App` is built; changes require restart;
- the retained gRPC cluster transport is compatibility code, not a supported
  business-traffic path. Keep it disabled or strictly network-isolated.

See the [fork migration guide](docs/migration-v2-ak.md),
[security boundaries](docs/security.md), and
[actor/keyed executor model](docs/actor-keyed-executor.md) before upgrading an
existing service.

## Build from source

Prerequisites:

- Go 1.26.5;
- Git submodule support;
- `protoc` 35.1 only when regenerating protobuf files;
- Python 3.12 or newer only when building documentation;
- Docker Engine and Docker Compose v2 only for Redis integration, distribution,
  NATS E2E, network benchmark, and NATS stress checks;
- GNU coreutils `timeout` (or Homebrew `gtimeout`) for Redis integration;
- Linux with GNU coreutils `timeout` for NATS E2E and the full `make verify`
  process-tree ownership contract.

```sh
git clone https://github.com/alkaid/pitaya.git
cd pitaya
make setup
make verify-fast
```

`make verify-fast` verifies pinned tools, module identity, and fork documentation
policy; compiles the root, CLI, and xk6 modules; then runs unit tests, vet, and an
independent consumer smoke test. It does not start external services.

The current checkout can also build the REPL client against the same workspace:

```sh
make build-cli
```

## Verification

Use the narrowest gate that matches a change:

```sh
make verify-fast          # source, docs policy, unit, vet, and consumer checks
make verify               # sequential engineering-health aggregate
make docs-lint            # fork identity, link policy, and ledger checks
make docs                 # warning-free Sphinx build
make test-integration     # isolated Redis session-storage round trip
make test-e2e-nats        # isolated Docker-backed NATS/etcd/Redis path
make test-distribution    # versioned CLI/xk6/module/Docker smoke
make benchmark-regression # isolated NATS network benchmark report
make benchmark-gate-nats  # fixed-runner decision for trusted maintainer candidates
make scan-deps            # reachable dependency report
```

`make verify` runs `verify-fast`, the full targeted race lane, Redis integration,
and NATS E2E in sequence. It is an explicit engineering-health check, not the
ordinary pull-request or lightweight release gate.

The dependency scan intentionally returns a non-zero status for an unaccepted
reachable vulnerability. See [Testing the fork](docs/testing.md) for lane
dependencies, timeouts, artifacts, and the network benchmark workflow.

## Documentation

- [Overview](docs/overview.md)
- [Features and support boundaries](docs/features.md)
- [API guide](docs/API.md)
- [Configuration](docs/configuration.rst)
- [Testing](docs/testing.md)
- [Security](docs/security.md)
- [Migration to the Alkaid v2 fork](docs/migration-v2-ak.md)
- [Upstream review ledger](docs/upstream-ledger.md)

The docs can be built locally after installing the pinned requirements:

```sh
python3 -m venv build/docs-venv
build/docs-venv/bin/python -m pip install -r docs/requirements.txt
make docs SPHINXBUILD="$PWD/build/docs-venv/bin/sphinx-build"
```

## Security

Do not report fork vulnerabilities to the former upstream security mailbox and
do not post sensitive details in a public issue. Use
[GitHub private vulnerability reporting](https://github.com/alkaid/pitaya/security/advisories/new)
for this repository. Deployment boundaries and the reporting procedure are
defined in [docs/security.md](docs/security.md).

## Lineage

Pitaya was originally developed by TFG Co and is based on nano and ideas from
Pomelo. This fork preserves that lineage while maintaining a separate module,
runtime contract, and release process. Historical upstream material is tracked
explicitly in the [upstream ledger](docs/upstream-ledger.md); availability in an
upstream branch is not evidence that a change is safe for this fork.

- [Upstream Pitaya project](https://github.com/topfreegames/pitaya)
- [Upstream libpitaya client SDK](https://github.com/topfreegames/libpitaya)
- [Upstream libpitaya-cluster](https://github.com/topfreegames/libpitaya-cluster)
- [nano](https://github.com/lonnng/nano)
- [Pomelo](https://github.com/NetEase/pomelo)

## License

[MIT License](LICENSE)
