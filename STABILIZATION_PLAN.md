# Pitaya ak-35 稳定化计划与执行记录

基线：`v2.11.1-ak-35`（`e275630e4042aa3dd73819365dfd5cad06d97bb9`）。交付分支：`codex/ak35-stabilization`。更新日期：2026-10-08。

目标是从长期运行的基线提取有复现证据的修复，恢复可信的 mock/testing 和构建入口。原重构只作为问题与补丁参考。公开 RPC/client 方法签名、protobuf 字段编号及 Redis/etcd key schema 保持兼容；实际行为和构建要求变化见[迁移指南](docs/migration-from-v2.11.1-ak-35.md)。

## 执行结果

| 批次 | 实施内容 | 验收 |
| --- | --- | --- |
| A：工具与测试 | Go 1.27.1；独立 tools 模块；固定 mock/protobuf 生成器；修复基线测试与示例 API；CLI/xk6 使用本 fork | 根模块测试、vet、逐包编译；生成无漂移；子模块 workspace/独立构建；CLI 和 k6 实际运行 |
| B：client | 按连接持有 socket、pending 和后台循环；单次完成请求；隔离旧连接；ProtoClient 描述符/连接尝试归属；断开回调可同步重连 | 确定性交错回归、完整 client race 重复 20 次、真实服务新旧客户端互通 |
| C1：服务发现 | 完整 prefix 快照；全局 revision；从 R+1 监听；失败持续恢复；事务批次提交；退出等待通知完成 | 已有节点更新红绿对照、真实内嵌 etcd、关闭/压缩/读取连续失败/旧事件/事务/退出回归 |
| C2：配置 | crypt 快照与连续监听；Viper provider 缓存替换、错误与关闭传递、同步读取；Pitaya 可取消监督循环、快照去重和解码 | mock 故障回归；真实 etcd 断流、故障期间 PUT、全局压缩、恢复及后续 PUT；非法/删除/多来源优先级 |
| D：独立修复 | Redis HMSet 传字段；timestamp HeartbeatAck；默认 Builder 无 reload source；Session in-flight map 副本共享锁 | 对应回归及真实 Redis 字段/TTL/context 检查 |
| E：解码与分帧 | 读取完整帧；保留握手后的首帧；非法 route/body 受控报错 | 碎片、粘包、整帧、截断及异常输入回归 |
| F：交付 | 应用迁移指南、验收记录、持续执行真实集成/生成/子模块检查的 CI | 最小外部应用启动关闭、旧新版本混部、按职责分批提交 |

详细命令、互通边界和未覆盖项见[验收记录](docs/stabilization-validation.md)。

## 必要的范围调整

- Session in-flight map 使用值类型传递，副本原来复制锁却共享 map，直接影响并发 RPC。修复为共享同一把锁，保持公开值类型用法。
- 只修 crypt 不足以恢复整条热更新链：Viper 的来源缓存、watch 关闭/错误传播和并发读取也必须修复。因此分别发布 crypt 与 Viper，并固定正式版本，未在 Pitaya 旁路原有配置体系。
- 比较配置 A 后再从可变化的 Viper 解码 B，会在 A→B→A 时把 loader 永久停在 B。Viper 增加只读 `RemoteSnapshot`，使一次比较与一批解码使用同一份 remote 文档。这个 API 保留原 Viper 的 delimiter、环境绑定和 decoder 行为，不暴露额外 writer 或 watcher。
- 新工具揭示的 protobuf 锁复制、测试 API 漂移以及旧异步断言假绿，作为可信验证的必要修复单独提交。生成文件与生产修复分开提交。

## 简化边界

服务发现使用一把 reconcile 锁和一个 applied revision；没有 per-server revision、tombstone 或常驻通知 worker。配置使用一个可取消监督循环；没有递归重启或 goroutine 身份识别。client 按连接分配资源，以所有权隔离替代跨连接复用可变状态。

未移植 owner fencing、stateful assignment、框架幂等层、新 TaskExecutor API、App/Agent/PROXY 全面生命周期重构、群组分页 API 或自建 coverage/benchmark gate。k6 保留已验证的 v0.46.0/goja 宿主，宿主迁移另行处理。

## 依赖交付

| 依赖 | 固定版本 | Commit |
| --- | --- | --- |
| crypt | `v0.31.0-ak-4` | `c445e1761398febc68f181b144696454fd052c7f` |
| Viper core | `v1.21.0-ak-8` | `d432d78ad80012a671de4b8d531b0ac24e02924b` |
| Viper remote | `remote/v1.21.0-ak-8` | 同上 |

以上 tag 已发布，Pitaya 使用正式模块校验值，无开发机 replace。Pitaya 本轮交付分支，不发布根模块 tag 或部署生产。

## 升级与回退

应用先按迁移指南升级 Go、检查显式依赖和 client/config 用法，然后固定候选 commit 构建。新旧协议和存储 schema 兼容，但旧前端的 HMSet 缺陷、旧客户端的复用重连问题不会因更新其他进程而自动消失。

保留原二进制和配置，在小流量实例观察错误率、超时、连接数、goroutine、配置更新时间和服务发现同步耗时。回退替换二进制，不清空 Redis/etcd；HMSet 修复后成功写入的业务数据不会自动回滚。
