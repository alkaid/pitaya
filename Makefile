ifeq ($(OS), Windows_NT)
	BIN := pitaya-cli.exe
	XK6_BIN := k6.exe
	MKFOLDER := if not exist "build" mkdir build
	GREP_CMD := findstr /V
else
	BIN := pitaya-cli
	XK6_BIN := k6
	MKFOLDER := mkdir -p build
	GREP_CMD := grep -v
endif

TESTABLE_PACKAGES = `go list ./... | $(GREP_CMD) examples | $(GREP_CMD) constants | $(GREP_CMD) mocks | $(GREP_CMD) helpers | $(GREP_CMD) interfaces | $(GREP_CMD) protos | $(GREP_CMD) e2e | $(GREP_CMD) benchmark`

# Generator dependencies are isolated from the framework runtime module.
GO_TOOL = GOWORK=off go tool -modfile=$(CURDIR)/tools/go.mod
MOCKGEN = $(GO_TOOL) mockgen
PROTOC_VERSION = 36.2

init-toolchain:
	@GOWORK=off go mod download -modfile=tools/go.mod
	@echo "Install protoc $(PROTOC_VERSION) from the official protobuf release; see docs/development.md"

compile-root:
	@GOWORK=off bash scripts/compile-module.sh

test-unit:
	@GOWORK=off go test -mod=readonly ./...

vet:
	@GOWORK=off go vet -mod=readonly ./...

verify-root: compile-root test-unit vet

verify-modules:
	@bash scripts/verify-modules.sh

setup: init-submodules
	@go mod tidy

build-cli:
	@$(MKFOLDER)
	@go build -o build/$(BIN) github.com/alkaid/pitaya/pitaya-cli/v2
	@echo "build pitaya-cli at ./build/$(BIN)"

# k6 v0.46.0 is the last tested host for this extension's goja API.
# Upgrade the host separately from the framework stabilization.
init-xk6:
	@$(MKFOLDER)
	@GOBIN=$(CURDIR)/build/tools GOWORK=off go install go.k6.io/xk6/cmd/xk6@v1.4.14

build-k6-extension: init-xk6
	@GOWORK=off $(CURDIR)/build/tools/xk6 build v0.46.0 --with github.com/alkaid/pitaya/xk6-pitaya=$(CURDIR)/xk6-pitaya --replace github.com/alkaid/pitaya/v2=$(CURDIR) --output ./build/$(XK6_BIN)
	@echo "build pitaya k6 extension at ./build/$(XK6_BIN)"

init-submodules:
	@git submodule init

setup-ci:
	@GOWORK=off go mod download -modfile=tools/go.mod

setup-protobuf-macos: init-toolchain

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

protos-compile-demo:
	@bash scripts/generate-protos.sh demo

protos-compile:
	@bash scripts/generate-protos.sh

rm-test-temp-files:
	@rm -f cluster/127.0.0.1* 127.0.0.1*
	@rm -f cluster/localhost* localhost*

ensure-testing-bin:
	@GOWORK=off go build -mod=readonly -o ./examples/testing/server ./examples/testing/main.go

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
	@GOWORK=off go test -mod=readonly -count=1 -tags=e2e ./e2e

e2e-test-grpc: ensure-testing-deps ensure-testing-bin
	@echo "===============RUNNING E2E GRPC TESTS==============="
	@GOWORK=off go test -mod=readonly -count=1 -tags=e2e ./e2e -grpc

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
	@go test -tags=benchmark ./benchmark -bench=.
	@echo "--- killing testing servers"
	@kill `cat back.PID` && rm back.PID
	@kill `cat front.PID` && rm front.PID

benchmark-test-grpc: ensure-e2e-deps-grpc ensure-testing-bin
	@echo "===============RUNNING BENCHMARK TESTS WITH GRPC==============="
	@echo "--- starting testing servers"
	@echo "--- sleeping for 5 seconds"
	@make bench-grpc-sv
	@sleep 5
	@go test -tags=benchmark ./benchmark -bench=.
	@echo "--- killing testing servers"
	@kill `cat back.PID` && rm back.PID
	@kill `cat front.PID` && rm front.PID

unit-test-coverage:
	@echo "===============RUNNING UNIT TESTS==============="
	@GOWORK=off go test -mod=readonly ./... -coverprofile coverprofile.out

test: test-coverage
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
	@go tool cover -func=coverprofile.out | egrep -v "100.0[%]"

.PHONY: mocks agent-mock session-mock networkentity-mock pitaya-mock metrics-mock serializer-mock acceptor-mock message-mock protos-client-mock cluster-mock discovery-mock worker-mock interfaces-mock codec-mock

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
	@docker build . -f dev.Dockerfile -t pitaya-dev:latest --build-arg GOPROXY=https://goproxy.io,direct --build-arg GOPRIVATE=git.mycompany.com,github.com/my/private

ensure-dev-image-exists:
	@docker images | grep pitaya-dev || make build-dev-image

test-many-clients-in-container:ensure-dev-image-exists
	@docker run pitaya-dev go test -tags=benchmark -v ./benchmark -bench=CreateManyClients -benchmem -benchtime=50x
