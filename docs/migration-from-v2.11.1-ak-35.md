# 从 v2.11.1-ak-35 迁移

本分支从 v2.11.1-ak-35 提取必要修复，保留公开 RPC/client 方法签名、protobuf wire 字段与 Redis/etcd key schema。

## 构建和生成

最低 Go 版本提高到 1.27.1。应用的 go.mod、go.work、CI 和构建镜像需要同步；使用 `go version`、`go env GOTOOLCHAIN` 核对实际工具链。离线构建需要预装 Go 1.27.1 和已锁定模块，不能依赖自动下载。

```sh
go mod edit -go=1.27.1
# 使用 workspace 时执行：
go work edit -go=1.27.1
GOTOOLCHAIN=local go test ./...
```

应用自有 mock 改用 `go.uber.org/mock/gomock`；重新生成时使用 mockgen v0.6.0。仓库的生成工具由 tools/go.mod 独立锁定，执行 `make mocks protos-compile`。protobuf runtime 为 v1.36.12，grpc runtime 保持 v1.75.1；protobuf descriptor 未改变。生成器和 protoc 安装见 development.md。

CLI/k6 使用本 fork 的 module path，通过仓库 go.work 构建候选代码。xk6 工具固定 v1.4.14；k6 宿主暂保留 v0.46.0/goja API。此分支未发布根模块 tag，独立子模块的正式版本依赖应在根模块发布后更新，不能直接把 workspace 构建视为独立发布验证。

## Client 并发和重连

`ConnectTo`、`Disconnect`、`SendRequest`、`SendNotify`、`SafeWrite`、`ConnectedStatus`、`MsgChannel` 和回调注册可并发调用。已连接时再次 ConnectTo 返回错误；连接建立中再次调用也返回错误。先 Disconnect，再发起新的连接。握手/拨号受 5 秒连接建立超时限制，Disconnect 可取消建立中的连接；失败的建立不触发 OnDisconnected。

每次成功连接拥有独立 socket、请求表、消息队列、心跳和超时循环。排队等待发送不计入响应超时。响应和超时竞争时只交付一次；断开丢弃该连接尚未完成的请求，由 OnDisconnected 统一通知应用，不为每个请求补造响应。

公开字段 `Connected` 和 `IncomingMsgChan` 为兼容保留；外部直接读写这些字段不具备并发安全保证。改用访问方法。握手数据和 TLS 配置在传入后不要并发修改；框架会复制 TLS 配置以避免自身修改调用方对象，TLS 验证默认策略保持 ak-35 行为。

```go
// 旧：if c.Connected { ch = c.IncomingMsgChan }
if c.ConnectedStatus() {
    ch = c.MsgChannel()
}
```

Client 和 ProtoClient 每次重连后都应重新取得 MsgChannel；旧 channel 不会被新连接使用，也不应由应用关闭。用 OnDisconnected 停止旧连接的接收循环，避免无限等待旧 channel。收到断开通知时可以同步重连，回调执行时不持有连接锁。慢回调会延迟发起它的关闭路径，应尽快返回。

ProtoClient 的 bootstrap 使用独立临时连接，不触发业务断开回调。描述符和响应按连接、请求 route 归属；LoadInfo/ExportInformation 会复制命令表，应用修改导出的 map 不再改变 client 内部状态。LoadInfo 在连接中调用只影响下一次连接；AddPushResponse 应在首次连接/加载描述符前调用。

## etcd 服务发现

注册 key/schema、self 和类型黑名单保持不变。SyncServers 现在读取完整 prefix 快照，补偿已有节点的 value 更新；watch 从快照 revision + 1 开始，关闭或压缩后持续重建。旧事件不能覆盖更新后的快照。同一事务的多个节点会完整提交后再通知 listener。

一次补偿读取的数据量约为“注册节点数 × 单节点 payload 大小”；`pitaya.cluster.sd.etcd.syncservers.parallelism` 保留配置兼容，但不再影响执行，因为已删除逐节点并行补读。大注册表需观察完整快照耗时和流量，再调整既有 syncservers.interval。

读取失败保留旧缓存。非法注册文档保留该节点上一份有效值，记录错误，后续合法 PUT 可恢复。快照恢复保证最终状态收敛，不重放断线期间每个中间事件；内容未变化的补偿不重复发 Modify。

listener 可以同步调用 GetServer/GetServers/GetServersByType/GetServerTypes/GetConsistentHashNode。通知仍同步执行，SyncServers 返回表示该批通知完成；listener 中需要再次 SyncServers 或关闭模块时，应异步触发，避免等待自身通知完成。Init/Shutdown 是单次模块生命周期，不支持对同一个实例重新 Init。

## 配置热更新

依赖固定为 Viper/core、Viper/remote `v1.21.0-ak-8` 和 crypt `v0.31.0-ak-4`。应用若有显式依赖或 replace，应同步检查 `go list -m all`，删除指向旧 fork/开发机的替换；无需修改 etcd key 或 value 格式。

首次启动监听失败、监听关闭和重建失败会持续重试，不再因一次失败永久停止。crypt 在每次恢复时读取快照，再从下一 revision 监听；Pitaya 对同一个远程合并快照进行比较和解码，恢复后远程状态相同不会额外通知。去重不比较环境变量或 flags；被本地 override 遮盖的远程字段变化仍可能多触发一次 Reload。启动仍要求 InitLoad 成功读到全部配置来源，不能将启动失败视为已加载默认值。

多 key 按配置注册顺序从低到高合并。暂时缺失 key、网络失败或非法文档保留该来源最近的有效配置；要撤销某个字段，应写入不含该字段的合法文档，写入 `{}` 会撤销该来源的全部贡献。删除 etcd key 本身不等于清空配置。断连恢复保证最新有效状态，不保证回放期间的每次保存。

每个 ConfLoader 解码到新对象，成功后才写回 Provide 提供的对象并调用 Reload。**Provide 对象预填但未在配置中定义的配置字段会归零**；未导出字段和 `mapstructure:"-"` 字段保留，不作为配置覆盖。把默认值放入 Viper.SetDefault，而不是依赖复用 struct 中残留的值。解码失败保留上一次应用配置且不调用该 loader 的 Reload。推荐让 Provide 每次创建新对象，Reload 使用应用自己的锁或 atomic.Pointer 发布：

```go
type Feature struct { Enabled bool `mapstructure:"enabled"` }
var current atomic.Pointer[Feature]
conf.SetDefault("feature.enabled", true) // 启动监听前设置
conf.AddLoader(config.LoaderFactory{
    ProvideApply: func() (string, interface{}) { return "feature", &Feature{} },
    ReloadApply: func(_ string, value interface{}) { current.Store(value.(*Feature)) },
})
// 业务线程读取 current.Load()；发布后的对象按只读处理。
```

旧的 Provide 复用指针方式仍可用，但业务读取该对象时需要应用自行同步。Viper 的远程更新与读取已有同步保护；运行中并发 Set/SetDefault、更改 aliases/flags/文件来源仍需应用同步。PitayaConfig 返回的 map/slice 也应只读。loader 回调同步执行，应快速返回；不要在 Reload 内同步 Shutdown 自身模块，应异步发起。Shutdown 取消重试并等待当前回调结束；同步远程 Get 的取消由后端超时限制，不能保证任意自定义后端立即返回。

## 混部与错误响应

候选前端搭配 ak-35 或候选后端，均已验证绑定、双向 RPC、push、kick 和重连，覆盖 NATS/gRPC 及新旧客户端。ak-35 前端的 HMSet 空参数问题仍会导致绑定失败，更新后端不会修复旧前端；灰度顺序应优先覆盖出现该问题的前端。

候选客户端测试同一实例重连。旧客户端复用实例曾在 kick 后重连超时，互通验证使用新的旧版客户端实例重连，不能将其解读为修复了旧客户端二进制。客户端应用应更新依赖后重新构建。

gRPC 的既有错误消息会包含 `code=...,reason=...,message=...,metadata=...`，多跳可能重复包装；NATS 返回原 message。这一差异已用真实 ak-35 服务对照并保留。应用应根据 code/reason 分支，不要依赖跨传输方式相同的完整 message 文本。

## 其他修复与回退

Redis HMSet 现在实际保存传入的字段；key、TTL、schema 不变。timestamp 心跳返回正确 HeartbeatAck，旧客户端仍可互通。默认 Builder 不再因没有配置热更新源而 panic。Session 的 in-flight map 在副本间共享同一把锁，公开值类型用法不变。

升级前保留 ak-35 二进制和应用配置，并在隔离环境按本文构建和测试。先部署少量候选实例，观察错误率、超时、连接数、goroutine 数、配置更新时间和服务发现同步耗时，再扩大实例数。出现回归时停止候选并换回原二进制；不用清空 Redis/etcd。HMSet 修复后已经成功写入的业务字段不会因回退自动撤销，业务补偿需按应用规则处理。

本分支是可审查的修复候选；互通和验收范围记录在 [验收记录](stabilization-validation.md)。Pitaya 未发布新 tag，应用试用时固定该分支的确切 commit；正式发布后再将 CLI/xk6 的根模块依赖换成发布 tag。
