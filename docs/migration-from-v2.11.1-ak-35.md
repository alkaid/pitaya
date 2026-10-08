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
