# ak-35 稳定化验收记录

验收日期：2026-10-08。基线：`v2.11.1-ak-35`。候选：本分支；生产实现冻结在 `1884da4`，其后的提交补齐测试、CI 与交付文档。以下为本次实际执行结果。

## 环境与依赖

Linux amd64、Go 1.27.1。crypt 使用已发布 `v0.31.0-ak-4`；Viper core/remote 使用已发布 `v1.21.0-ak-8`，根模块不含本地 replace。生成器、镜像和重现步骤见[开发说明](development.md)。

真实服务使用任务专用 etcd 3.6.14、Redis 7-alpine 和固定 digest 的 NATS。断连/压缩仅操作一次性 etcd。根包测试中的 etcd/NATS 为真实内嵌服务，配置集成为真实 etcd gRPC 网络连接，Redis 集成为实际 Redis 命令。

## 已执行验证

| 范围 | 命令/方式 | 结果 |
| --- | --- | --- |
| 根模块 | `GOWORK=off make test-unit vet compile-root` | 全部通过；逐包编译不运行 TestMain |
| 并发 | `GOWORK=off go test -mod=readonly -race -count=1 ./cluster ./config ./session ./agent ./conn/message ./conn/codec ./client` | 全部通过 |
| client 重复交错 | `go test -race -count=20 ./client` | 全部通过，含断开回调同步重连窗口 |
| 配置真实恢复 | 设置 `PITAYA_TEST_ETCD_ENDPOINT`，`go test -mod=readonly -race -tags=integration ./config` | 通过，包含断流期间 PUT 与物理压缩后恢复、后续 PUT、取消退出 |
| Redis | 设置 `PITAYA_TEST_REDIS_ADDR`，`go test -mod=readonly -race -count=1 -tags=integration ./session` | 通过，字段值、TTL、context 和不存在字段均断言 |
| NATS E2E | 重新构建 testing server，`go test -mod=readonly -count=1 -tags=e2e ./e2e` | 全套通过 |
| gRPC E2E | 同上，增加 `-grpc` | 全套通过 |
| 标记测试编译 | 分别以 `e2e`、`benchmark` tag 执行 `go test -c` | 通过；未将 benchmark 编译记为性能验证 |
| 生成一致性 | `make mocks protos-compile protos-compile-demo` 后检查生成文件 diff | 无漂移；descriptor 字段与服务定义已对照 |
| 嵌套模块 | `make verify-modules`、workspace 下 `go test ./pitaya-cli/... ./xk6-pitaya/...` | 通过；独立验证只在临时 modfile 替换未发布的候选根模块 |
| CLI | `make build-cli`，实际执行 `build/pitaya-cli --help` | 正常输出参数帮助 |
| k6 | `make build-k6-extension`，1 VU/1 iteration 导入 `k6/x/pitaya` 并构造 Client | 成功执行；不是负载测试 |
| 最小应用迁移 | 独立 Go module，按指南使用 fresh Provide + atomic.Pointer；Standalone 启动后关闭 | 成功加载配置、启动和退出 |
| Viper | core/remote 全量 race、vet、特性 tag、golangci-lint v2.14.0 | 本地通过；[ak-8 CI](https://github.com/alkaid/viper/actions/runs/37767117536)通过（含 3 OS、特性 tag、wasm/AIX） |
| crypt | 全量 race/vet、真实 etcd 集成和重复恢复用例 | 本地通过；发布后未发现远端 CI run，未将其记为远端通过 |

根仓库 CI 已加入真实后端恢复、受影响包 race、生成漂移及嵌套模块检查。远端结果应以该候选提交的 Actions 记录为准，不能以依赖库 CI 或本地结果代替。

## 根因与回归证据

- **配置永久停止更新**：旧 crypt 在 watch 终止/压缩后停止；上层首次 setup 或重建失败也可能退出。旧代码用例先失败，补丁后同场景恢复。当前真实测试通过 TCP proxy 切断应用 streams，管理 client 在故障期间写入并压缩，恢复后断言最新值与下一次 PUT，覆盖“直到重启才恢复”的故障模型。缺少线上日志，不能断言这就是现场唯一根因。
- **快照窗口**：旧 Get/Watch 间保存会遗漏。当前 crypt 先取得 revision R 的快照，再监听 R+1；服务发现也使用同一策略。测试固定了初始加载到监听、快照到 watch 的保存窗口。
- **A→B→A**：旧比较/解码读取不同版本，loader 会停在 B；确定性复现失败，使用同一只读快照后通过。另覆盖 delimiter、环境绑定、字面 map key 和运行态字段保留。
- **服务发现已有节点更新**：基线 keys-only 补偿跳过已有 ID，旧值保持不变；完整快照补丁后收敛到最新值。另覆盖事务多 key、删除、陈旧事件、连续恢复失败、通知中查询缓存、Shutdown 等待外部同步。
- **client 竞态**：基线并发发送/关闭复现空指针和 race。候选按连接隔离资源，测试强制旧连接与新连接交错、响应与超时竞争、bootstrap/断开及回调重连；完整 client race 重复 20 次通过。
- **HMSet**：旧实现未传字段，真实 Redis 返回参数错误；修复后 round-trip 与 TTL 通过。
- **断言有效性**：移除 NATS bindings/push/kick 的生产分发进行 mutation 对照，旧 sleep 用例仍可能假绿，改为等待真实 callback 后三项均失败；恢复生产分发后三项通过。未用 Skip、空断言或替换后端来通过这些场景。

## 新旧版本互通

使用独立 Go module。旧服务/客户端直接依赖已发布 `v2.11.1-ak-35`，没有替换旧实现；候选服务/客户端指向本分支，Viper/crypt 使用正式 tag。真实 etcd/Redis/NATS/gRPC，各组使用独立服务发现 prefix 和端口。

NATS 和 gRPC 各运行以下三组，每组分别使用旧、新客户端，共 12 个客户端组合；6 组服务均验证 SIGTERM 正常退出。

| 前端 | 后端 | 验证结果 |
| --- | --- | --- |
| 候选 | ak-35 | 绑定、双向 RPC、直接 push、kick、重连后再绑定通过 |
| ak-35 | 候选 | 双向 RPC、直接 push、kick、重连通过；绑定精确复现 ak-35 的 `PIT-444` / HMSet 空参数错误 |
| 候选 | 候选 | 绑定、双向 RPC、直接 push、kick、重连后再绑定通过 |

候选客户端始终使用同一个实例重连。旧客户端复用实例曾在 kick 后重连超时；最终旧客户端矩阵使用新实例重连。上表不代表旧前端绑定或旧客户端同实例重连已通过。

gRPC 错误消息带有逐跳 status 包装，NATS 不带；5 项差异已用真实 ak-35 服务复现。测试精确断言两个 transport 的完整 code/reason/message/metadata，候选保留该旧行为。

## 变更规模与工程边界

与 ak-35 比较，框架非生成的运行时代码改动集中在 15 个文件，约新增 1,384 行、删除 650 行（净增 734 行）；主要为 client 连接所有权与配置/服务发现恢复。测试与测试 helper 为 60 个文件，生成代码为 39 个文件，其余为示例、工具、依赖及文档。数字按 diff 文件分类，不等同于新增功能数量。

必要新增状态均对应回归场景：连接资源防止旧连接关闭新资源，reconcile 锁保证快照/事件/通知顺序，配置监督循环负责恢复和取消，remote snapshot 绑定比较与解码。未引入 owner fencing、通用幂等框架或新的任务执行体系。

## 限制与交付边界

本次覆盖上述功能、故障注入和互通场景。未执行长时间线上流量压测、超大注册表性能测试、所有平台的 Pitaya 集成、生产 TLS/鉴权组合或任意业务回调行为验证；k6 仅做构建及模块加载 smoke。Go1.27.1 的最低构建要求、公开 client 字段并发限制、loader 默认值/字段删除语义及回调重入约束必须按迁移指南处理。

交付是可审查的候选分支与已发布依赖 tag。应用侧仍需小流量灰度并观察指标；本次未部署生产，测试通过不等于任意生产负载下零缺陷。回退方法见[迁移指南](migration-from-v2.11.1-ak-35.md)。
