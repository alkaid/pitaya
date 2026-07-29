ifeq ($(OS), Windows_NT)
	BIN := pitaya-cli.exe
	XK6_BIN := k6.exe
	MKFOLDER := if not exist "build" mkdir build
	GREP_CMD := findstr /V
	GOWORK_OFF := set "GOWORK=off" &&
else
	BIN := pitaya-cli
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

TESTABLE_PACKAGES = `go list ./... | $(GREP_CMD) examples | $(GREP_CMD) constants | $(GREP_CMD) mocks | $(GREP_CMD) helpers | $(GREP_CMD) interfaces | $(GREP_CMD) protos | $(GREP_CMD) e2e | $(GREP_CMD) benchmark`

compile-core:
	@tmpdir="$$(mktemp -d)"; trap 'rm -rf "$$tmpdir"' EXIT; go test -c -o "$$tmpdir/" $(CORE_PACKAGES)

init-toolchain setup-ci build-k6-extension protos-compile protos-compile-demo: export GOWORK = off

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
	@cd examples/testing && docker compose up -d etcd nats && cd ../demo/chat/ && go run main.go

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

ensure-testing-bin:
	@[ -f ./examples/testing/server ] || go build -o ./examples/testing/server ./examples/testing/main.go

ensure-testing-deps:
	@cd ./examples/testing && docker compose up -d

ensure-e2e-deps-grpc:
	@cd ./examples/testing && docker compose up -d etcd

kill-testing-deps:
	@cd ./examples/testing && docker compose down; true

kill-jaeger:
	@docker compose -f ./examples/testing/docker-compose-jaeger.yml down; true

e2e-test: e2e-test-nats e2e-test-grpc

e2e-test-nats: ensure-testing-deps ensure-testing-bin
	@echo "===============RUNNING E2E NATS TESTS==============="
	@go test ./e2e/e2e_test.go -update

e2e-test-grpc: ensure-testing-deps ensure-testing-bin
	@echo "===============RUNNING E2E GRPC TESTS==============="
	@go test ./e2e/e2e_test.go -update -grpc

bench-nats-sv:
	@PITAYA_METRICS_PROMETHEUS_PORT=9098 ./examples/testing/server -type game -frontend=false > /dev/null 2>&1 & echo $$! > back.PID
	@PITAYA_METRICS_PROMETHEUS_PORT=9099 ./examples/testing/server -type connector -frontend=true > /dev/null 2>&1 & echo $$! > front.PID

bench-grpc-sv:
	@PITAYA_METRICS_PROMETHEUS_PORT=9098 ./examples/testing/server -grpc -grpcport=3435 -type game -frontend=false > /dev/null 2>&1 & echo $$! > back.PID
	@PITAYA_METRICS_PROMETHEUS_PORT=9099 ./examples/testing/server -grpc -grpcport=3436 -type connector -frontend=true > /dev/null 2>&1 & echo $$! > front.PID

benchmark-test-nats: ensure-testing-deps ensure-testing-bin
	@echo "===============RUNNING BENCHMARK TESTS WITH NATS==============="
	@echo "--- starting testing servers"
	@echo "--- sleeping for 5 seconds"
	@make bench-nats-sv
	@sleep 5
	@go test ./benchmark/benchmark_test.go -bench=.
	@echo "--- killing testing servers"
	@kill `cat back.PID` && rm back.PID
	@kill `cat front.PID` && rm front.PID

benchmark-test-grpc: ensure-e2e-deps-grpc ensure-testing-bin
	@echo "===============RUNNING BENCHMARK TESTS WITH GRPC==============="
	@echo "--- starting testing servers"
	@echo "--- sleeping for 5 seconds"
	@make bench-grpc-sv
	@sleep 5
	@go test ./benchmark/benchmark_test.go -bench=.
	@echo "--- killing testing servers"
	@kill `cat back.PID` && rm back.PID
	@kill `cat front.PID` && rm front.PID

unit-test-coverage: kill-testing-deps
	@echo "===============RUNNING UNIT TESTS==============="
	@go test $(TESTABLE_PACKAGES) -coverprofile coverprofile.out

test: kill-testing-deps test-coverage
	@make rm-test-temp-files
	@make ensure-testing-deps
	@sleep 10
	@make e2e-test

test-coverage: unit-test-coverage
	@make rm-test-temp-files

test-coverage-html: test-coverage
	@go tool cover -html=coverprofile.out

test-coverage-func coverage-func: test-coverage
	@echo
	@echo "\033[1;34m=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-\033[0m"
	@echo "\033[1;34mFunctions NOT COVERED by Tests\033[0m"
	@echo "\033[1;34m=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-\033[0m"
	@go tool cover -func=coverprofile.out | grep -Ev "100[.]0%"

.PHONY: compile-core init-toolchain check-protoc setup init-submodules setup-ci protos-compile-demo protos-compile mocks agent-mock session-mock networkentity-mock pitaya-mock metrics-mock serializer-mock acceptor-mock message-mock protos-client-mock cluster-mock discovery-mock worker-mock interfaces-mock codec-mock

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
