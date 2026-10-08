# 构建与测试

最低 Go 版本为 1.27.1；CI 从根 go.mod 读取版本。运行时依赖保持锁定，生成器位于独立的 tools 模块。

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
```

Windows 使用 Git Bash 执行相同脚本；benchmark 的 .bat 入口也调用该脚本，避免生成另一套 protobuf API。

`compile-root` 逐包执行 `go test -c`，不会触发 TestMain。`test-unit` 包括使用内嵌 etcd/NATS 的包测试，需要允许本机监听端口；它不启动或关闭 Docker Compose。`vet` 独立执行静态检查。E2E 和 benchmark 使用 `e2e`、`benchmark` build tag，通过 Makefile 对应入口执行。

生成结果应提交。重复运行 `make mocks protos-compile` 后应无 diff。protobuf 更新需检查 descriptor 中的消息字段、枚举和服务定义，不能仅凭生成成功判断 wire 兼容。

## CLI 和 k6 扩展

两个子模块使用本 fork，go.work 将它们与当前根模块一起构建：

```sh
go test ./pitaya-cli/... ./xk6-pitaya/...
make build-cli
make build-k6-extension
```

xk6 固定 v1.4.14。扩展依然使用 goja，k6 宿主固定 v0.46.0；升级到使用 sobek 的新宿主需要单独迁移，未混入本次稳定化。Docker 构建从仓库根目录传入 context，不再用同一个 revision 覆盖不同仓库。

子模块 go.mod 的根依赖保留已发布的 v2.11.1-ak-35；候选分支尚未发版时，独立验证必须用临时 modfile 的 local replace 指向根目录。发布候选根版本后再将依赖改成该确切 tag，并运行无 replace 的 `GOWORK=off go test ./...`。不得将本机绝对路径写入提交的模块文件。
