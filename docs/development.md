# 构建与测试

最低 Go 版本为 [Go 1.27.1](https://go.dev/dl/?mode=json)；CI 从根 go.mod 读取版本。运行时依赖保持锁定，生成器位于独立的 tools 模块。

| 工具 | 固定版本 |
| --- | --- |
| Go | 1.27.1 |
| mockgen / gomock | v0.6.0 |
| protoc | 36.2 |
| protoc-gen-go | v1.36.12 |
| protoc-gen-go-grpc | v1.6.2 |
| protoc-gen-go-errors | v1.0.18 |
| goveralls | v0.0.12 |

从 [protobuf 官方 release](https://github.com/protocolbuffers/protobuf/releases/tag/v36.2) 安装 protoc，并将 bin 放入 PATH。Linux amd64 安装包 protoc-36.2-linux-x86_64.zip 的官方 SHA256 为 `121f6c7afe1d4d0e3ea6aab9432038599250134cbf4474cb1167d2c7decd4278`；其他平台使用对应 release asset 的 digest。生成前先 `git submodule update --init --recursive`。

```sh
make init-toolchain
make mocks
make protos-compile
make protos-compile-demo
make verify-root
make verify-modules
```

Windows 使用 Git Bash 执行相同脚本；benchmark 的 .bat 入口也调用该脚本，避免生成另一套 protobuf API。

`compile-root` 逐包执行 `go test -c`，不会触发 TestMain。`test-unit` 包括使用内嵌 etcd/NATS 的包测试，需要允许本机监听端口；它不启动或关闭 Docker Compose。`vet` 独立执行静态检查。E2E 和 benchmark 使用 `e2e`、`benchmark` build tag，通过 Makefile 对应入口执行。

生成结果应提交。重复运行 `make mocks protos-compile protos-compile-demo` 后应无 diff。protobuf 更新需检查 descriptor 中的消息字段、枚举和服务定义，不能仅凭生成成功判断 wire 兼容。

## CLI 和 k6 扩展

两个子模块使用本 fork，go.work 将它们与当前根模块一起构建：

```sh
go test ./pitaya-cli/... ./xk6-pitaya/...
make build-cli
make build-k6-extension
```

xk6 固定 v1.4.14。扩展依然使用 goja，k6 宿主固定 v0.46.0；升级到使用 sobek 的新宿主需要单独迁移，未混入本次稳定化。Docker 构建从仓库根目录传入 context，不再用同一个 revision 覆盖不同仓库。

子模块 go.mod 的根依赖为已发布的 v2.11.1-ak-35。独立验证先使用正式依赖和已提交的 go.sum，再用临时 modfile 的 local replace 验证当前根代码。发布子模块前，应将依赖更新到所需的已发布根版本，并重新执行无 replace 的 `GOWORK=off go test -mod=readonly ./...`。不得将本机绝对路径写入提交的模块文件。

## 真实后端回归

使用独立的 etcd、Redis 和 NATS。配置恢复测试会压缩 etcd 全局历史，必须使用一次性测试实例。Docker Compose 文件固定了已验证的 etcd/NATS 镜像 digest，Redis 使用 7-alpine。

```sh
docker compose -f examples/testing/docker-compose.yml up -d --wait
PITAYA_TEST_ETCD_ENDPOINT=http://127.0.0.1:2379 \
PITAYA_TEST_REDIS_ADDR=127.0.0.1:6379 \
GOWORK=off go test -mod=readonly -race -count=1 -tags=integration ./config ./session
make e2e-test-nats e2e-test-grpc
docker compose -f examples/testing/docker-compose.yml down
```

已有测试服务使用其他端口时，可通过 `PITAYA_TEST_ETCD_ENDPOINT`、`PITAYA_TEST_REDIS_ADDR`、`PITAYA_TEST_NATS_URL` 传给 E2E，先执行 `make ensure-testing-bin`，再直接 `GOWORK=off go test -mod=readonly -count=1 -tags=e2e ./e2e`（gRPC 添加 `-grpc`）。Makefile 的 E2E 入口会重新构建服务，避免复用旧二进制。

CI 除根模块测试/vet 外，还执行受影响包的 race 检查、上述真实后端恢复测试、NATS/gRPC E2E、生成一致性、CLI/xk6 workspace 及独立模块验证。`make verify-modules` 先对正式依赖执行 readonly test/vet，再在临时 modfile 中替换候选根模块验证，另外检查 tools 的 tidy 一致性；不会改写子模块的正式依赖。
