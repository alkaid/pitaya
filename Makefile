ifeq ($(OS), Windows_NT)
	BIN := pitaya-cli.exe
	OWNERCTL_BIN := pitaya-ownerctl.exe
	XK6_BIN := k6.exe
	MKFOLDER := if not exist "build" mkdir build
	GREP_CMD := findstr /V
	GOWORK_OFF := set "GOWORK=off" &&
else
	BIN := pitaya-cli
	OWNERCTL_BIN := pitaya-ownerctl
	XK6_BIN := k6
	MKFOLDER := mkdir -p build
	GREP_CMD := grep -v
	GOWORK_OFF := GOWORK=off
endif

MOCKGEN := go tool mockgen
CORE_PACKAGES := . ./agent ./remote ./service
TOOLS_MOD := tools/go.mod
GO_TOOL = $(GOWORK_OFF) go tool -modfile=$(TOOLS_MOD)
PROTOC := protoc
PROTOC_VERSION := 35.1
PROTOC_GEN_GO = $(shell $(GO_TOOL) -n protoc-gen-go)
PROTOC_GEN_GO_GRPC = $(shell $(GO_TOOL) -n protoc-gen-go-grpc)
PROTOC_GEN_GO_ERRORS = $(shell $(GO_TOOL) -n protoc-gen-go-errors)
XK6_VERSION := v1.4.7
K6_VERSION := v1.8.0
DEV_IMAGE := pitaya-dev:go1.26.5
WORKSPACE := $(abspath go.work)
DOCS_BUILDDIR ?= $(abspath build/docs)
SPHINXBUILD ?= sphinx-build
SPHINXOPTS ?= -W --keep-going
COVERAGE_PROFILE ?= $(CURDIR)/coverprofile.out
COVERAGE_POLICY ?= $(CURDIR)/scripts/coverage-baseline.json
COVERAGE_PROFILES ?= $(COVERAGE_PROFILE)
FUZZ_MANIFEST ?= $(CURDIR)/scripts/fuzz-targets.json
FUZZ_SEED_TIMEOUT ?= 30s
FUZZ_TIME ?= 2m
FUZZ_MINIMIZE_TIME ?= 30s
FUZZ_TIMEOUT ?= 4m
FUZZ_PARALLEL ?= 2
FUZZ_ARTIFACT_DIR ?= $(CURDIR)/build/fuzz
TARGET_GOOS ?=
TARGET_GOARCH ?= amd64

TESTABLE_PACKAGES = `go list ./... | $(GREP_CMD) examples | $(GREP_CMD) constants | $(GREP_CMD) mocks | $(GREP_CMD) helpers | $(GREP_CMD) interfaces | $(GREP_CMD) protos | $(GREP_CMD) e2e | $(GREP_CMD) benchmark`

compile-core:
	@tmpdir="$$(mktemp -d)"; trap 'rm -rf "$$tmpdir"' EXIT; go test -c -o "$$tmpdir/" $(CORE_PACKAGES)

compile-root:
	@./scripts/compile-module.sh .

compile-cli:
	@./scripts/compile-module.sh pitaya-cli

compile-xk6:
	@./scripts/compile-module.sh xk6-pitaya

compile: compile-root compile-cli compile-xk6

compile-platform:
	@./scripts/compile-platform.sh "$(TARGET_GOOS)" "$(TARGET_GOARCH)"

test-unit:
	@go test ./... -count=1

test-race-root:
	@go test -race ./co ./session ./agent ./cluster ./service ./router ./acceptor ./acceptorwrapper ./client ./metrics ./timer ./logger ./config -count=10 -timeout=10m

test-race-xk6:
	@cd xk6-pitaya && go test -race ./... -count=10 -timeout=10m

test-race: test-race-root test-race-xk6

test-integration:
	@./scripts/test-integration.sh

vet:
	@go vet ./...

verify-root:
	@$(MAKE) compile-root
	@$(MAKE) test-unit
	@$(MAKE) vet

module-verify:
	@./scripts/verify-modules.sh

tools-verify:
	@./scripts/verify-tools.sh

logrus-verify:
	@./scripts/verify-logrus.sh

test-consumer:
	@./scripts/test-consumer.sh

docs-lint:
	@./scripts/verify-docs.sh

docs:
	@$(MAKE) -C docs SPHINXBUILD="$(SPHINXBUILD)" SPHINXOPTS="$(SPHINXOPTS)" BUILDDIR="$(DOCS_BUILDDIR)" html

test-distribution:
	@./scripts/test-distribution.sh

verify-fast: tools-verify module-verify logrus-verify docs-lint compile test-unit vet test-fuzz-seeds test-consumer

# This is the sequential engineering-health aggregate. It is intentionally
# broader than the ordinary pull-request and lightweight release gate.
verify:
	@$(MAKE) verify-fast
	@$(MAKE) test-race
	@$(MAKE) test-integration
	@$(MAKE) test-e2e-nats

scan-deps: init-toolchain
	@./scripts/scan-deps.sh

compile-core compile-root compile-platform test-unit test-race-root test-integration test-e2e-nats e2e-test-nats vet verify-root verify-fast verify init-toolchain setup-ci build-k6-extension protos-compile protos-compile-demo scan-deps: override export GOWORK := off
compile-cli compile-xk6 test-race-xk6: override export GOWORK := $(WORKSPACE)

compile-core compile-root compile-cli compile-xk6 compile compile-platform test-unit test-race-root test-race-xk6 test-race test-integration vet verify-root verify-fast test-e2e-nats e2e-test-nats verify: override export GOFLAGS :=

init-toolchain:
	@go mod download -modfile=$(TOOLS_MOD)

check-protoc:
	@version="$$($(PROTOC) --version 2>/dev/null)" || { echo "protoc $(PROTOC_VERSION) is required"; exit 1; }; \
		test "$$version" = "libprotoc $(PROTOC_VERSION)" || { echo "protoc $(PROTOC_VERSION) is required (found $$version)"; exit 1; }

setup: init-submodules init-toolchain
	@go mod tidy

build-cli:
	@$(MKFOLDER)
	@go build -o build/$(BIN) github.com/alkaid/pitaya/pitaya-cli/v2
	@echo "build pitaya-cli at ./build/$(BIN)"

build-ownerctl:
	@$(MKFOLDER)
	@go build -o build/$(OWNERCTL_BIN) ./cmd/pitaya-ownerctl
	@echo "build pitaya-ownerctl at ./build/$(OWNERCTL_BIN)"

build-k6-extension:
	@$(MKFOLDER)
	@go run go.k6.io/xk6/cmd/xk6@$(XK6_VERSION) build $(K6_VERSION) --build-flags=-trimpath --build-flags="-ldflags=-s -w" --build-flags=-buildvcs=false --with github.com/alkaid/pitaya/xk6-pitaya=./xk6-pitaya/ --with github.com/alkaid/pitaya/v2=./ --output ./build/$(XK6_BIN)
	@echo "build pitaya k6 extension at ./build/$(XK6_BIN)"

init-submodules:
	@git submodule update --init --recursive

setup-ci: init-toolchain
	@go mod download

setup-protobuf-macos:
	@brew install protobuf
	@$(MAKE) check-protoc

run-jaeger-aio:
	@docker compose -f ./examples/testing/docker-compose-jaeger.yml up -d
	@echo "Access jaeger UI @ http://localhost:16686"

run-chat-example:
	@cd examples/testing && PITAYA_NATS_HOST_PORT=4222 PITAYA_ETCD_HOST_PORT=2379 docker compose up --detach --wait etcd nats && cd ../demo/chat/ && go run main.go

run-cluster-example-frontend-tracing:
	@PITAYA_METRICS_PROMETHEUS_PORT=9090 JAEGER_SAMPLER_PARAM=1 JAEGER_DISABLED=false JAEGER_SERVICE_NAME=example-frontend JAEGER_AGENT_PORT=6832 go run examples/demo/cluster/main.go

run-cluster-example-backend-tracing:
	@PITAYA_METRICS_PROMETHEUS_PORT=9091 JAEGER_SAMPLER_PARAM=1 JAEGER_DISABLED=false JAEGER_SERVICE_NAME=example-backend JAEGER_AGENT_PORT=6832 go run examples/demo/cluster/main.go --port 3251 --type room --frontend=false

run-cluster-example-frontend:
	@PITAYA_METRICS_PROMETHEUS_PORT=9090 go run examples/demo/cluster/main.go

run-cluster-protobuf-frontend-example:
	@cd examples/demo/cluster_protobuf && go run main.go

run-cluster-protobuf-backend-example:
	@cd examples/demo/cluster_protobuf && go run main.go --port 3251 --type room --frontend=false

run-cluster-example-backend:
	@PITAYA_METRICS_PROMETHEUS_PORT=9091 go run examples/demo/cluster/main.go --port 3251 --type room --frontend=false

run-cluster-grpc-example-connector:
	@cd examples/demo/cluster_grpc && go run main.go

run-cluster-grpc-example-room:
	@cd examples/demo/cluster_grpc && go run main.go --port 3251 --rpcsvport 3435 --type room --frontend=false

run-cluster-worker-example-room:
	@cd examples/demo/worker && go run main.go --type room --frontend=true

run-cluster-worker-example-metagame:
	@cd examples/demo/worker && go run main.go --type metagame --frontend=false

run-cluster-worker-example-worker:
	@cd examples/demo/worker && go run main.go --type worker --frontend=false

run-custom-metrics-example:
	@cd examples/demo/custom_metrics && go run main.go --port 3250

run-rate-limiting-example:
	@go run examples/demo/rate_limiting/main.go

protos-compile-demo: check-protoc init-toolchain
	@$(PROTOC) --plugin=protoc-gen-go=$(PROTOC_GEN_GO) -I examples/demo/protos examples/demo/protos/*.proto --go_out=.
	@$(PROTOC) --plugin=protoc-gen-go=$(PROTOC_GEN_GO) -I examples/testing/protos examples/testing/protos/*.proto --go_out=.
	@$(PROTOC) --plugin=protoc-gen-go=$(PROTOC_GEN_GO) -I examples/demo/cluster_protobuf/protos examples/demo/cluster_protobuf/protos/*.proto --go_out=.
	@$(PROTOC) --plugin=protoc-gen-go=$(PROTOC_GEN_GO) -I examples/demo/worker/protos examples/demo/worker/protos/*.proto --go_out=.
	@$(PROTOC) --plugin=protoc-gen-go=$(PROTOC_GEN_GO) -I examples/demo/cluster_ak/protos examples/demo/cluster_ak/protos/*.proto --go_out=.

#https://developers.google.com/protocol-buffers/docs/reference/go-generated
protos-compile: check-protoc init-toolchain
	@cd benchmark/testdata && PROTOC="$(PROTOC)" ./gen_proto.sh
	@$(PROTOC) --plugin=protoc-gen-go=$(PROTOC_GEN_GO) --plugin=protoc-gen-go-errors=$(PROTOC_GEN_GO_ERRORS) --proto_path=. --proto_path=./pitaya-protos/lib/alkaid/goerrors pitaya-protos/*.proto --go_out=. --go_opt=paths=import --go-errors_out=.
	@$(PROTOC) --plugin=protoc-gen-go-grpc=$(PROTOC_GEN_GO_GRPC) --proto_path=. --proto_path=./pitaya-protos/lib/alkaid/goerrors pitaya-protos/*.proto --go-grpc_out=.
	@$(PROTOC) --plugin=protoc-gen-go=$(PROTOC_GEN_GO) -I pitaya-protos/test pitaya-protos/test/*.proto --go_out=protos/test --go_opt=paths=source_relative

rm-test-temp-files:
	@rm -f cluster/127.0.0.1* 127.0.0.1*
	@rm -f cluster/localhost* localhost*

kill-jaeger:
	@docker compose -f ./examples/testing/docker-compose-jaeger.yml down; true

test-e2e-nats e2e-test-nats:
	@./scripts/test-e2e-nats.sh

e2e-test: test-e2e-nats

benchmark-test-nats:
	@BENCH_COUNT=$${BENCH_COUNT:-1} ./scripts/test-benchmark-nats.sh

benchmark-regression:
	@BENCH_COUNT=$${BENCH_COUNT:-10} ./scripts/test-benchmark-nats.sh

# Low-level gate primitive. Formal runs use
# scripts/run-benchmark-gate-controller.sh so protected pins are audited before
# and after execution and pending evidence is promoted only after a stable read.
benchmark-gate-nats:
	@./scripts/benchmark-gate-nats.sh

# Long-running capacity evidence belongs only in the nightly/manual lane.
test-stress-nats:
	@NATS_WORKLOAD=stress ./scripts/test-benchmark-nats.sh

benchmark-test-grpc:
	@echo "gRPC runtime benchmarks are unsupported; use benchmark-test-nats" >&2
	@exit 2

test-fuzz-seeds:
	@$(GOWORK_OFF) go -C tools run ./cmd/fuzzgate seeds \
		-repo "$(CURDIR)" \
		-manifest "$(abspath $(FUZZ_MANIFEST))" \
		-timeout "$(FUZZ_SEED_TIMEOUT)"

test-fuzz-nightly:
	@$(GOWORK_OFF) go -C tools run ./cmd/fuzzgate fuzz \
		-repo "$(CURDIR)" \
		-manifest "$(abspath $(FUZZ_MANIFEST))" \
		-artifacts "$(abspath $(FUZZ_ARTIFACT_DIR))" \
		-clean-cache \
		-seedtimeout "$(FUZZ_SEED_TIMEOUT)" \
		-fuzztime "$(FUZZ_TIME)" \
		-minimizetime "$(FUZZ_MINIMIZE_TIME)" \
		-timeout "$(FUZZ_TIMEOUT)" \
		-parallel "$(FUZZ_PARALLEL)"

test-fuzz-one:
	@test -n "$(FUZZ_PACKAGE)" || { echo "FUZZ_PACKAGE is required" >&2; exit 2; }
	@test -n "$(FUZZ_TARGET)" || { echo "FUZZ_TARGET is required" >&2; exit 2; }
	@$(GOWORK_OFF) go -C tools run ./cmd/fuzzgate fuzz \
		-repo "$(CURDIR)" \
		-manifest "$(abspath $(FUZZ_MANIFEST))" \
		-artifacts "$(abspath $(FUZZ_ARTIFACT_DIR))" \
		-package "$(FUZZ_PACKAGE)" \
		-target "$(FUZZ_TARGET)" \
		-seedtimeout "$(FUZZ_SEED_TIMEOUT)" \
		-fuzztime "$(FUZZ_TIME)" \
		-minimizetime "$(FUZZ_MINIMIZE_TIME)" \
		-timeout "$(FUZZ_TIMEOUT)" \
		-parallel "$(FUZZ_PARALLEL)"

unit-test-coverage:
	@echo "===============RUNNING UNIT TESTS==============="
	@GOWORK=off go test ./... -count=1 -coverpkg=./... -coverprofile="$(COVERAGE_PROFILE)"

coverage-stamped:
	@echo "===============RUNNING STAMPED UNIT TESTS==============="
	@$(GOWORK_OFF) go -C tools run ./cmd/coveragegate generate \
		-repo "$(CURDIR)" \
		-policy "$(abspath $(COVERAGE_POLICY))" \
		-profile "$(abspath $(COVERAGE_PROFILE))"

test: verify-fast

test-coverage: unit-test-coverage
	@make rm-test-temp-files

coverage: unit-test-coverage

coverage-gate:
	@test -n "$(COVERAGE_DIFF_BASE)" || { echo "COVERAGE_DIFF_BASE is required" >&2; exit 2; }
	@test -n "$(COVERAGE_DIFF_HEAD)" || { echo "COVERAGE_DIFF_HEAD is required" >&2; exit 2; }
	@$(GOWORK_OFF) go -C tools run ./cmd/coveragegate check \
		-repo "$(CURDIR)" \
		-policy "$(abspath $(COVERAGE_POLICY))" \
		-profile "$(abspath $(COVERAGE_PROFILE))" \
		-base "$(COVERAGE_DIFF_BASE)" \
		-head "$(COVERAGE_DIFF_HEAD)"

coverage-baseline:
	@test -n "$(COVERAGE_BASE_SHA)" || { echo "COVERAGE_BASE_SHA is required" >&2; exit 2; }
	@$(GOWORK_OFF) go -C tools run ./cmd/coveragegate update \
		-repo "$(CURDIR)" \
		-policy "$(abspath $(COVERAGE_POLICY))" \
		$(foreach profile,$(COVERAGE_PROFILES),-profile "$(abspath $(profile))") \
		-base-sha "$(COVERAGE_BASE_SHA)"

test-coverage-html: test-coverage
	@go tool cover -html="$(COVERAGE_PROFILE)"

test-coverage-func coverage-func: test-coverage
	@echo
	@echo "\033[1;34m=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-\033[0m"
	@echo "\033[1;34mFunctions NOT COVERED by Tests\033[0m"
	@echo "\033[1;34m=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-\033[0m"
	@go tool cover -func="$(COVERAGE_PROFILE)" | grep -Ev "100[.]0%"

.PHONY: compile-core compile-root compile-cli compile-xk6 compile compile-platform test-unit test-race-root test-race-xk6 test-race test-integration vet verify-root module-verify tools-verify logrus-verify test-consumer test-distribution docs docs-lint verify-fast verify scan-deps test-e2e-nats e2e-test-nats e2e-test benchmark-test-nats benchmark-regression benchmark-gate-nats test-stress-nats benchmark-test-grpc test-fuzz-seeds test-fuzz-nightly test-fuzz-one test coverage coverage-stamped coverage-gate coverage-baseline init-toolchain check-protoc setup init-submodules setup-ci build-cli build-ownerctl protos-compile-demo protos-compile mocks agent-mock session-mock networkentity-mock pitaya-mock metrics-mock serializer-mock acceptor-mock message-mock protos-client-mock cluster-mock discovery-mock worker-mock interfaces-mock codec-mock

mocks: agent-mock session-mock networkentity-mock pitaya-mock serializer-mock metrics-mock acceptor-mock message-mock protos-client-mock cluster-mock discovery-mock worker-mock interfaces-mock codec-mock

agent-mock:
	@$(MOCKGEN) -destination=agent/mocks/agent.go -package=mocks github.com/alkaid/pitaya/v2/agent Agent,AgentFactory

session-mock:
	@$(MOCKGEN) -destination=session/mocks/session.go -package=mocks github.com/alkaid/pitaya/v2/session Session,SessionPool

networkentity-mock:
	@$(MOCKGEN) -destination=networkentity/mocks/networkentity.go -package=mocks github.com/alkaid/pitaya/v2/networkentity NetworkEntity

pitaya-mock:
	@$(MOCKGEN) -destination=mocks/app.go -package=mocks github.com/alkaid/pitaya/v2 Pitaya

metrics-mock:
	@$(MOCKGEN) -destination=metrics/mocks/reporter.go -package=mocks github.com/alkaid/pitaya/v2/metrics Reporter
	@$(MOCKGEN) -destination=metrics/mocks/statsd_reporter.go -package=mocks github.com/alkaid/pitaya/v2/metrics Client

serializer-mock:
	@$(MOCKGEN) -destination=serialize/mocks/serializer.go -package=mocks github.com/alkaid/pitaya/v2/serialize Serializer

acceptor-mock:
	@$(MOCKGEN) -destination=mocks/acceptor.go -package=mocks github.com/alkaid/pitaya/v2/acceptor PlayerConn,Acceptor

message-mock:
	@$(MOCKGEN) -destination=conn/message/mocks/message_encoder.go -package=mocks github.com/alkaid/pitaya/v2/conn/message Encoder

protos-client-mock:
	@$(MOCKGEN) -destination=protos/mocks/pitaya.go -package=mocks github.com/alkaid/pitaya/v2/protos PitayaClient,PitayaServer

cluster-mock:
	@$(MOCKGEN) -destination=cluster/mocks/cluster.go -package=mocks github.com/alkaid/pitaya/v2/cluster RPCServer,RPCClient,SDListener,RemoteBindingListener,RemoteSessionListener,InfoRetriever

discovery-mock:
	@$(MOCKGEN) -destination=cluster/mocks/service_discovery.go -package=mocks github.com/alkaid/pitaya/v2/cluster ServiceDiscovery

worker-mock:
	@$(MOCKGEN) -destination=worker/mocks/rpc_job.go -package=mocks github.com/alkaid/pitaya/v2/worker RPCJob

interfaces-mock:
	@$(MOCKGEN) -destination=interfaces/mocks/interfaces.go -package=mocks github.com/alkaid/pitaya/v2/interfaces Module,BindingStorage

codec-mock:
	@$(MOCKGEN) -destination=conn/codec/mocks/packet_encoder.go -package=mocks github.com/alkaid/pitaya/v2/conn/codec PacketEncoder
	@$(MOCKGEN) -destination=conn/codec/mocks/packet_decoder.go -package=mocks github.com/alkaid/pitaya/v2/conn/codec PacketDecoder

build-dev-image:
	@docker build . -f dev.Dockerfile -t $(DEV_IMAGE) --build-arg GOPROXY=https://goproxy.io,direct --build-arg GOPRIVATE=git.mycompany.com,github.com/my/private

ensure-dev-image-exists:
	@docker image inspect $(DEV_IMAGE) >/dev/null 2>&1 || $(MAKE) build-dev-image

test-many-clients-in-container:ensure-dev-image-exists
	@docker run --rm $(DEV_IMAGE) go test -v benchmark/benchmark_test.go -bench=CreateManyClients -benchmem -benchtime=50x
