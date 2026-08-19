# 从 v2.11.1-ak-35 升级到 Alkaid v2 分支当前 `main`

本文以已验证发布 `v2.11.1-ak-35` 为唯一升级基线，记录当前 `main` 分支中尚未发布、且下游应用可以观察到的 API、配置、协议、生命周期和验证工具变化。

迁移期间应固定框架的完整提交 SHA。框架发布并验证新 tag 之前，不要发布应用升级。内部实现修复、审计批次和未发布的中间方案不属于应用迁移契约，因此不在本文保留。

## 升级范围和顺序

所有模块统一使用 fork module：

```go
import pitaya "github.com/alkaid/pitaya/v2"
```

如果应用仍使用 upstream module identity，再通过 `replace` 指向 fork，应先在单独的 clean branch 中把 import 和 require 全部切到 `github.com/alkaid/pitaya/v2`。删除把 fork 映回 upstream checkout 的 `replace`。临时 workspace 可以用于新 tag 发布前的编译验证，但不能作为发布证明。

建议按以下顺序迁移：

1. 统一 module identity、Go 工具链和依赖版本。
2. 处理公共 Go API 的编译错误。
3. 核对配置默认值、协议和网络边界。
4. 回归启动、关闭、服务发现和集群传输。
5. 在精确框架提交上运行应用门禁和真实依赖环境测试。

## 模块、工具链和测试依赖

根模块、`pitaya-cli` 和 `xk6-pitaya` 都要求 Go 1.26.5。

`xk6-pitaya` 从 k6 v0.46.0 升到 v1.8.0，对外类型从 Goja 切到 `github.com/grafana/sobek`。自定义扩展需要按 k6 v1 运行时重新构建，并把 `goja.Promise`、`goja.Object` 和 `goja.ConstructorCall` 改为对应的 Sobek 类型。

生成的 gRPC stub 使用 support package v9，要求 grpc-go 1.64 或更高版本。嵌入 `protos.UnimplementedPitayaServer` 时必须按值嵌入，不能嵌入可能为 nil 的指针。即使部署没有启用 gRPC，这仍然是编译期迁移。

`helpers.GetTestEtcd` 现在返回 `*integration.Cluster`，不再返回 `*integration.ClusterV3`。这只影响直接使用该测试 helper 的外部测试代码。

### 生成测试 Mock

第一方 mock 已切到 `go.uber.org/mock/gomock`，并根据当前接口重新生成。公开 mock 方法和 `EXPECT()` recorder 的参数或返回值可能发生变化，包括 entity/agent 的 close 和 kick、session bind 以及 Pitaya RPC。

下游测试应统一 gomock import、controller 和依赖版本，并使用固定版本的 Uber Mock 重新生成自有 mock，不要手改生成文件。使用 unkeyed 测试配置 literal 的代码也应改成 keyed literal，避免新增 exported 字段再次造成编译失败。

### 已删除的 Logrus 包

第一方 `logger/logrus` 适配器和 `logger/test` 测试辅助包已删除，不提供兼容层。

服务端日志改用当前基于 Zap 的 `logger` API。client 包仍保留独立的 logger hook，但它不能替代服务端全局日志配置；需要继续使用 Logrus 的应用应自行维护 adapter。

## 公共 Go API

### 有状态任务接纳

`StatefulPool.Go`、keyed `co.Go*` wrapper、`cluster.GoWithRequest` 和 `session.SessPublic.Go` 不再静默丢弃拒绝结果。主要签名变化如下：

| API | v2.11.1-ak-35 | 当前结果 |
| --- | --- | --- |
| `co.GoPersist` / `GoWithID` / `GoWithUser` / `GoMain` | `chan struct{}` | `(<-chan struct{}, error)` |
| `co.WaitWithID` / `WaitWithUser` / `WaitMain` | 无返回值 | `error` |
| `(*StatefulPool).Go` | `chan struct{}` | `(<-chan struct{}, error)` |
| `(*StatefulPool).Wait` | 无返回值 | `error` |
| `cluster.GoWithRequest` | 无返回值 | `error` |
| `session.SessPublic.Go` | 无返回值 | `error` |

旧写法：

```go
done := pool.Go(ctx, key, task, false)
<-done

pool.Wait(ctx, key, task, false)
```

新写法：

```go
done, err := pool.Go(ctx, key, task, false)
if err != nil {
	return fmt.Errorf("submit keyed task: %w", err)
}
<-done

if err := pool.Wait(ctx, key, task, false); err != nil {
	return fmt.Errorf("run keyed task: %w", err)
}
```

nil task、非法 UID、预先取消的 context、已关闭的 pool 和过载都可能拒绝接纳。拒绝时 `done == nil`，不能等待它。

任务成功接纳后，context 取消不会撤销任务。`Wait` 可能在已接纳任务退出前返回 `ctx.Err()`。任务如果借用了请求状态、锁、事务或 `WaitGroup` slot，必须等待成功返回的 `done` 后才能释放这些资源。自定义 `SessPublic` 实现也要同步更新 method set。

### 任务执行器 TaskExecutor

`TaskGoProvider`、异步 `final` callback 和 `WithTaskGoProvider` 已删除。应用改为注册 `component.TaskExecutor`：

```go
type keyedExecutor struct {
	pool *co.StatefulPool
	key  func(context.Context) int
}

func (e *keyedExecutor) Execute(ctx context.Context, task component.Task) error {
	var taskErr error
	done, err := e.pool.Go(ctx, e.key(ctx), func(taskCtx context.Context) {
		taskErr = task(taskCtx)
	}, false)
	if err != nil {
		return err
	}

	// 已接纳的任务即使随后取消 ctx，也必须等待完成。
	<-done
	return taskErr
}

app.Register(
	&Handler{},
	component.WithTaskExecutor(&keyedExecutor{pool: pool, key: requestKey}),
)
```

`Execute` 可能被并发调用，executor 自有的可变状态必须同步。每次调用最多执行 task 一次；一旦调用，就要在 `Execute` 返回前完成。拒绝接纳时返回明确错误，不得保留 task 并在返回后执行。

executor 应原样传播受控 task 的返回值，不要重新解释 handler 的业务状态。`component.ErrTaskSkipped` 只用于 task 调用前的明确跳过。任务未运行时，对外结果如下：

- Request 明确跳过返回 503，Notify 明确跳过后直接确认。
- 启动前 `context.Canceled` 和 `DeadlineExceeded` 分别返回 499 和 504。
- 单一、明确的 `apierrors.Error` 保留应用状态。
- 其他拒绝、panic、joined skip 和 executor 契约违约统一返回稳定的 500。

这些状态描述接纳结果，不覆盖 task 已运行后的 handler 业务错误。

### 后端回调所有权

应用不再持有可失败的主 `OnBindBackend` 和 `OnKickBackend` callback。backend mutation、远端 RPC、frontend 同步、缓存 flush 和 rollback 顺序由框架协调。

观察型逻辑改到 after callback：

```go
app.OnAfterBindBackend(onBound)
app.OnAfterKickBackend(onKicked)
```

Bind after callback 的错误保持 best-effort 语义；Kick after callback 的错误返回调用方。两者都不会回滚已经提交的 mutation。

自定义 `SessionPool` 与内置 remote service 配合时，必须实现 `session.BackendCoordinatorInstaller`。remote unbind 使用的自定义 session 必须实现 identity-aware 的原子 backend 删除能力，不再提供先读后删的 fallback。`SetBackends` 会复制传入的 map，调用方不能靠后续修改原 map 改变 session。

### StatefulPoolsModule 构造与关闭

`NewStatefulPoolsModule` 新增可选的 `reportPeriod ...time.Duration`。普通两参数调用仍能编译，但精确函数值、依赖注入绑定和签名断言必须改成 variadic 类型，或者用两参数闭包包装。

`StatefulPoolsModule.Shutdown` 会先停止 monitor，再释放 module-owned 的所有 pool。此后的新提交返回 `ErrPoolClosed`。`Release` 不会取消或等待已经开始的阻塞任务，应用必须先停止接纳，并自行 drain 或取消运行中的任务。

App 关闭模块改为明确的两阶段协议。第一阶段按逆序调用所有 module 的 `BeforeShutdown`，用于停止 RPC、服务发现 callback 等模块自有入站并等待其 worker；框架随后关闭 handler task admission、等待已接纳任务并执行最终 session 扫描。第二阶段才按逆序调用 `Shutdown` 释放 module 资源。自定义 module 的 `BeforeShutdown` 必须形成入站屏障，但不能释放仍可能被已接纳任务使用的依赖；资源释放应留到 `Shutdown`。

### 无状态任务接纳与启动

旧 `co.Go` 继续保持 best-effort。调用方需要知道任务是否被接纳时，应使用 `co.Submit`。

`App.Start`、`App.StartWorker` 和 `Worker.Start` 在启动任务无法接纳时会 panic。需要错误返回的调用方改用 `Worker.StartWithError`。`co.Group.Wait` 可能返回 pool submission error，`TryGo` 也可能因此返回 false。

Worker 队列 runtime 已内置基于 `go-workers v1.2.1` 的兼容实现，不再直接依赖上游 module。Redis key、Sidekiq queue payload 和 retry wire format 保持不变，不需要迁移已有队列。生命周期语义发生变化：`Worker.Stop` 会先取消传给 RPC job 的 context，再等待 scheduler、fetcher、manager、worker 以及已经接纳的 job 全部退出，并原子清除绑定旧 App callback 的 manager；后续 App 不会再启动旧 manager。

该 runtime 使用进程级全局状态，同一进程只能由一个 Worker 或 App worker runtime 持有。不要在一个进程内启动多个彼此独立的 Worker 或 App；需要隔离多套 worker 时，应拆成独立进程。

默认 Redis 连接的 connect、read 和 write timeout 固定为 5 秒，runtime context 取消也会中断 fetch 和 scheduler I/O。job 成功返回后，manager 才为该 job 启动独立的 5 秒确认窗口并执行 `LREM`；确认按 worker 并发，不会按任务数串行累加关闭时间。`Worker.Stop` 会等这些确认全部结束。Redis 在确认窗口内不可用时，job 保留在 in-progress list，下一次启动会按 at-least-once 语义重放。job panic 并启用 retry 时，只有 retry `ZADD` 成功或策略明确不再重试才会确认原 job；retry 入队失败时，原 job 同样留在 in-progress list。业务处理必须幂等。

到期的 retry 和 scheduled job 通过单条 Lua `EVAL` 原子执行 `LPUSH` 和 `ZREM`，取消或响应不明确时不会把 job 留在两个命令之间。受限 Redis ACL 除旧命令集外，还要允许 `EVAL`、`ZSCORE`、`LPUSH` 和 `ZREM`；标准 Redis Cluster 还要求脚本涉及的 zset 与目标 queue 位于同一 hash slot。当前 key 格式没有 hash tag，无法满足该条件的集群部署不能启用 retry 或 scheduled job，应使用支持多 key 脚本的单实例或代理方案。

`RegisterRPCJob` 与 `Stop` 现在串行。`Stop` 已开始或完成、且 Worker 尚未通过 `Start` 或 `StartWithError` 再次启动时，注册返回新增的 `constants.ErrWorkerStopped`。需要在同一进程重新使用 Worker 的代码必须先重新启动，再重新注册 RPC job；不能依赖上一个生命周期保留的 manager。`Worker.SetLogger` 仍保留，并通过稳定 facade 并发更新，不要求恢复已删除的第一方 Logrus adapter。

`App.Start` 一旦进入，route 和 message dictionary 配置便永久冻结；即使 shutdown drain 期间 `IsRunning()` 已返回 false，或者 `App.Start` 已返回，也不能再调用 `AddRoute` 或 `SetDictionary`。这避免仍在退出的 handler 与路由 map 并发读写。动态路由或字典必须在 `Start` 前完成，进程重启时重新构造 App。

### Acceptor 生命周期接口

公开 `acceptor.Acceptor` 接口删除 `ListenAndServe()` 要求，新增：

```go
Serve(context.Context) error
Ready() <-chan error
```

自定义 Acceptor 和 wrapper 必须满足以下契约：

- 每个实例只能启动一次。启动结果只确定一次，但每次调用 `Ready()` 都返回独立的缓冲 channel；该 channel 收到结果后关闭，晚到调用会立即回放同一结果。bind 成功发布 nil，bind、TLS 或 wrapper 启动失败发布对应启动错误，ready 后的运行错误由 `Serve` 返回。
- `Stop` 在启动前、运行中和并发重复调用时都必须安全。context 取消或主动停止后，`Serve` 返回 nil；启动前停止后再调用 `Serve` 返回 `acceptor.ErrAcceptorStopped`，并发重复启动返回 `acceptor.ErrAcceptorAlreadyStarted`。
- listener、地址和 running 状态必须在 ready 前原子发布。`GetConnChan` 由实现保持开放，App context 负责停止 reader。
- wrapper 必须在同一个 context 中启动连接 pipe，再调用底层 `Serve`。重复 `Serve` 在任何 pipe 或底层调用前返回，不能产生第二份副作用。

`TCPAcceptor` 和 `WSAcceptor` 的兼容方法 `ListenAndServe()` 以及 `ListenAndServeTLS(cert, key)` 现在返回 `error`。保存为精确 `func()` 函数值或实现旧接口的代码需要改签名；直接作为语句或 goroutine 调用仍可忽略返回值。`acceptorwrapper.BaseWrapper.ListenAndServe()` 保留无返回值的旧具体方法，wrapper 的新生命周期入口是 `Serve`。

### SessionPool 关闭契约

`session.SessionPool` 新增 `ForceCloseAll()`，这是自定义 SessionPool 和相应 mock 的编译期破坏性变更。`CloseAll()` 继续等待 session 的 in-flight request，同时会继续扫描并关闭其他空闲 session；`ForceCloseAll()` 忽略 in-flight 标记并关闭当前全部 session。

App 在 listener、connection reader 和 connection handler 全部停止后，先关闭 task admission 并等待所有已接纳任务，再调用 `ForceCloseAll()`。因此框架已接纳的任务不能在最终扫描后创建新 session，drain 超时或被第二个信号中断后也不会再次无限等待 in-flight request。框架外直接调用 `CloseAll()` 的代码如果也要求有界关闭，应改用 `ForceCloseAll()`；强制关闭前必须先停止 session 接纳，并等待所有仍可能创建 session 的工作退出。

## 配置、资源池和指标

### Builder、受信代理与扇出

`NewDefaultBuilder` 使用传入的 `PitayaConfig` snapshot，不安装动态 reload。进程拥有可 reload 配置源时继续使用 `NewBuilderWithConfigs`。两种 Builder 在 `Build` 后都不支持直接修改配置。

新增配置：

| Go 字段 / Viper 键 | 默认结果 | 迁移动作 |
| --- | --- | --- |
| `PitayaConfig.Acceptor.TrustedProxyCIDRs` / `pitaya.acceptor.trustedproxycidrs` | 空列表，只信任 socket peer | 配置真实反向代理 CIDR；非法 CIDR 会使 `Builder.Build` panic；修改后重启 |
| `ClusterRPCConfig.FanoutConcurrency` / `pitaya.cluster.rpc.fanout_concurrency` | 每次 fanout 最多启动 32 个 RPC | 根据单次请求目标数和 transport 容量调整；非正值回退 32 |

这些 exported 字段会使外部 unkeyed `ClusterRPCConfig` literal 和 acceptor 配置的精确匿名 struct literal 编译失败。优先从 `config.NewDefaultPitayaConfig` 构造，再按字段修改。

fanout 成功响应按 server ID 稳定排序。部分失败会同时返回成功响应和 `errors.Join` 错误，取消会停止尚未领取的工作。不要依赖完成顺序、nil placeholder 或 last-error-wins。`FanoutConcurrency` 是每次调用的预算，不是进程级总上限。

### GoPool 配置

`config.GoPool` 新增以下 exported 字段：

| Go 字段 / Viper 键 | 默认值 | 说明 |
| --- | ---: | --- |
| `RunningTaskTimeout` / `runningtasktimeout` | 零值继承已解析的 `Expire` | 从 task 实际开始执行时计时；显式负值会使 pool 构造失败 |
| `MaxEscapedWorkers` / `maxescapedworkers` | 128 | 全 pool 仍存活的 replacement worker 上限，不随 `Tune` 改变 |
| `MaxEscapedWorkersPerID` / `maxescapedworkersperid` | 1 | 单 ID 上限，同时受全 pool 预算约束 |

普通 custom、`session`、`user` 和 `default` pool 的零值策略继续允许 running-task escape；module-owned `persist` pool 继续使用严格模式。普通 pool 的 `Expire` 和继承的 `RunningTaskTimeout` 默认都是 10 分钟；`persist` 未显式配置时为 1 小时。

需要严格单写时，继续使用 `DisablePurgeRunning: true`。严格 pool 可能被卡住的任务永久阻塞；允许 escape 的 pool 则需要 fencing、版本检查或幂等，避免旧 worker 与 replacement 重叠产生重复副作用。budget 耗尽只停止创建 replacement，不会取消已经运行的任务。

`NewStatefulPoolsModule` 会复制传入的 `poolsCfg` map，不再回写调用方。`persist` 下过去存在但未生效的 expire、buffer、timeout 或 budget 配置现在会生效，升级前应逐项检查。`DisablePurge=true` 同时禁用 idle purge 和 running escape，优先级更高。

外部 `config.GoPool` 和直接使用 `ants.Options` 的代码都应改为 keyed literal：

```go
roomPool := config.GoPool{
	Name:                   "room",
	Expire:                 10 * time.Minute,
	TaskBuffer:             100,
	RunningTaskTimeout:     10 * time.Minute,
	MaxEscapedWorkers:      128,
	MaxEscapedWorkersPerID: 1,
}
```

需要固定生产配置时，可以显式写为：

```yaml
pitaya:
  gopools:
    room:
      expire: 10m
      runningtasktimeout: 10m
      maxescapedworkers: 128
      maxescapedworkersperid: 1
      disablepurgerunning: false
```

### TaskBuffer 容量

keyed-pool 依赖升级后，`TaskBuffer=N` 表示非阻塞接纳水位，实际每 key channel 容量为 `2N`，blocking submit 可以使用预留的一半。零值归一为 100，合法范围是 0 到 65536。

与基线相比，每个活跃 ID 的 channel 内存可能增加。同 key task 在自身队列已满时递归 blocking submit 不能保证继续前进，可能自锁。迁移时需要重新做容量和递归调用检查。

### 指标周期与自定义直方图

`pitaya.metrics.period` 默认仍为 15 秒，现在也控制 StatefulPool 指标采样周期。显式设置很短周期的部署需要重新评估 pool snapshot 和 reporter 开销。

`pitaya.metrics.custom.histograms` 中显式声明的 collector 现在会真正注册并出现在 Prometheus scrape 中。升级后会增加对应的 `_bucket`、`_sum` 和 `_count` series，应按 label cardinality 和 retention 评估存储成本。

histogram raw `name` 必须唯一，即使 subsystem 不同也不能复用。最终 Prometheus FQName 以及 histogram/summary 派生的 `_bucket`、`_sum`、`_count` 不能与内建或其他 custom collector 冲突。冲突会在首次 Register 前使 reporter 构造失败。

既有 `pitaya_handler_response_time_ns` 继续是 SUMMARY，名称、labels、objectives、quantile、`_sum` 和 `_count` 不变，不要把它迁移为同名 histogram。未配置同名 custom histogram 时，直接调用 `PrometheusReporter.ReportHistogram(metrics.ResponseTime, ...)` 会返回 `constants.ErrMetricNotKnown`；框架正常 response-time 路径仍调用 `ReportSummary`。

外部 registry 若预先注册了独立的 `H_bucket`，通用 `prometheus.Registerer` 无法枚举它，随后注册 histogram `H` 仍可能到 Gather 时才报告后缀冲突。建议使用 reporter 专用 registry，并在 staging 执行真实 Gather。

## 协议、会话和网络边界

### 严格握手状态机

客户端 packet 顺序收紧为：

```text
Start -> HandshakeValidated -> Working -> Closed
```

客户端必须先发送 `Handshake`，收到服务端成功响应后发送 `HandshakeAck`，然后才能发送 `Data`、`Heartbeat` 或 `HeartbeatAck`。ACK-first、Data-first、握手前 heartbeat、重复 Handshake/ACK 和其他乱序 packet 都会关闭 session，且不会刷新 `lastAt`。

握手前的 `Heartbeat` 和 `HeartbeatAck` 不会刷新 `lastAt`。未进入 `Working` 的连接由既有 heartbeat liveness 检查渐进清理，不承诺在单个 `pitaya.heartbeat.interval` 内关闭；按当前两倍 interval 的过期比较和 ticker 对齐，通常会在最后一次合法握手活动后的第二到第三个 interval 关闭。旧 `StatusHandshake` 常量保留为 `StatusHandshakeValidated` 的同值别名。

`Heartbeat` 和 `HeartbeatAck` 的 payload 只能为空，或为恰好 8 bytes 的大端 Unix 毫秒时间戳。其他长度会按非法 packet 关闭连接。旧客户端若在时间戳后附加扩展数据，必须先移除这些数据。

`agent.Agent` 新增 `TransitionStatus(from, to int32) bool`，这是第三方 Agent、AgentFactory 和 mock 的编译期破坏性变更。实现必须用单次原子 compare-and-swap 完成迁移，`StatusClosed` 是终态；不能用 `GetStatus()` 后接 `SetStatus()` 模拟，否则 close 与握手并发时可能把已关闭连接恢复为工作态。自维护 gomock 应从新接口重新生成。

部署新服务端前，应先升级或验证全部客户端。协议测试至少覆盖 ACK-first、Data-first、heartbeat-before-handshake、重复 Handshake/ACK、延迟 ACK 和未完成握手的渐进清理。

### 消息和错误载荷解码

压缩的 Pitaya message payload 与压缩的客户端握手响应，解压后都不得超过 16 MiB。超过上限时，message decoder 返回错误；client 会在进入 connected 状态前中止握手。未压缩 payload 的处理不变。需要发送高压缩率大载荷的应用必须拆分或缩小数据。

缺少 route-length byte 的截断 routable message 现在返回 `ErrInvalidMessage`，不再 panic。`message.Decode` 调用方应把新的大小限制错误与其他非法入站 frame 一样处理。

JSON 或 protobuf error payload 反序列化失败时，统一返回 HTTP 500，reason 和 message 都为 `unknown`。有效 payload 仍忽略未知字段，保持 wire 向前兼容。应用不能再依赖 malformed payload 的 partial decode。

### WebSocket、来源校验与 PROXY 协议

WebSocket Upgrade 默认从接受任意 Origin 改为同源策略。没有 Origin 的非浏览器客户端仍然接受。合法跨域部署必须在 `Serve` 前配置明确的受信 Origin：

```go
ws.SetCheckOrigin(func(r *http.Request) bool {
	return r.Header.Get("Origin") == "https://game.example.com"
})
```

默认策略同时比较 Host 和 scheme。明文 WS 只接受同 Host 的 `http` Origin，WSS 只接受同 Host 的 `https` Origin；`http` Origin 不能连接 WSS。传入 nil 会恢复该策略，不要使用恒真 callback 恢复旧默认。

TLS 在反向代理终止时，后端请求的 `r.TLS` 为空，默认策略会把 transport 视为明文 WS，并拒绝浏览器发送的 `https` Origin。此类部署应通过 `SetCheckOrigin` 配置精确 allowlist。框架不会依据未经验证的 `X-Forwarded-Proto` 放宽来源校验。

WS Upgrade 的 header timeout、idle timeout 和 header 上限分别为 5 秒、60 秒和 64 KiB。Upgrade 完成后立即设置 `4 byte 协议头 + 16 MiB payload` 的 read limit，超限 frame 会在业务解码前被拒绝。

`TCPAcceptor` 启用 PROXY protocol 时，客户端必须在 5 秒内完成 header。header 使用最多 64 路有界并发解析，单个慢连接不会占住主 accept loop；容量耗尽时新连接会被立即关闭，不进入业务 handler。超时、半包或非法 header 会关闭该连接，成功解析后清除 deadline。`TCPAcceptor.Stop` 或 Serve context 取消会立即关闭所有解析中的 raw connection，不等待 header deadline。未启用 PROXY protocol 的 TCP 连接直接使用底层 listener，不占用这 64 个解析槽位。

`WSAcceptor` 当前不支持 PROXY protocol；它的 `EnableProxyProtocol()` 保留为空操作。WebSocket 部署不能依赖上述 5 秒 header 超时或 64 路解析保护，反向代理场景应由代理层终止 PROXY protocol，并通过受信代理配置处理客户端地址。

### 其他可观察行为

- `sys` component 和 route namespace 经过 trim 后按大小写不敏感方式保留给内部 remote。应用占用该名称时必须重命名。
- 带 timestamp 的 heartbeat acknowledgement 使用 `HeartbeatAck`，不再错误发送 `HandshakeAck`。
- session close 和 kick cleanup 幂等。订阅 slice 采用复制语义，不要依赖重复 close side effect 或 live slice mutation。
- 已关闭 session 的 `Bind` 和 `StoreSessionLocal` 返回 `ErrCloseClosedSession`；bind callback 重入 `Bind` 返回 `ErrSessionAlreadyBound`。
- `WithForkMethods` 对每个 subscriber method 独立计算。需要 fanout 的方法必须全部显式列出。
- `util/net.SourceIPs` 和 `GetClientIP` 只返回 socket peer。需要 forwarded header 时，先用 `ParseTrustedProxies` 编译 CIDR，再调用 `GetClientIPWithTrustedProxies`。禁止把 `0.0.0.0/0` 或 `::/0` 设为 trusted proxy。
- CLI file mode 的命令错误现在返回非零退出码，并保留 request 关联。依赖旧零退出码的脚本需要修正。

## 集群、服务发现和传输

### Etcd 服务发现

构造器现在拒绝 nil server、空 `ID` 或 `Type`，以及包含 `/` 的 `ID` 或 `Type`。应用必须在构造前提供完整身份。调用方注入的 `*clientv3.Client` 仍归调用方所有，discovery shutdown 只关闭自己创建的 client。

server type blacklist 继续过滤该类型的其他实例，但当前节点精确的 type 和 ID 保留例外，避免删除自己的注册与续租状态。

`FlushServer2Cluster` 只更新当前 key 已绑定当前 published lease 的注册。key 缺失、绑定旧 lease，或者续租后重试仍未绑定当前 lease 时会返回错误，不再把缺失 key 当作无条件创建入口。lease generation 改变时，Flush 会按新 lease 重试一次；延迟到达的旧写入不能重新绑定旧 lease。

Flush 不从 `DialTimeout` 或 `GrantLease.Timeout` 派生 deadline，普通调用仍可能等待 etcd transport。`BeforeShutdown` 和 `Shutdown` 会取消 discovery-owned Flush context。最近一次成功 Flush 的完整 payload 按 revision 保存，续租发现 key 缺失时用它重建，因此动态 metadata 不会被静态构造快照覆盖。

topology listener 与 leader listener 使用各自的 FIFO lane。同一 lane 保序，两条 lane 可以并发。每条 callback queue 上限为 64，满时对 discovery worker 施加 backpressure，不丢弃已接纳事件。listener 应快速返回，长期阻塞仍可能让 etcd client 的上游缓存增长。

`Init` 等待初始 topology callback 和首个已确认的非空 leader 状态，但 leader listener 自身在独立 lane 异步执行。`SyncServers` 会等待本次同步接纳的 topology callback，`SDListener` 不能同步调用它，否则会等待自己所在的 lane。

正常关闭顺序必须是 `BeforeShutdown -> Shutdown`。`BeforeShutdown` 停止新 callback 接纳、取消 Flush、等待已接纳 callback drain，再 revoke lease。callback 不能同步调用 `BeforeShutdown`；callback 内可以调用 `Shutdown`，但这不等价于立即 revoke，注册可能继续存在到 lease TTL，默认最长约 60 秒。

leader 候选会增加一次有界的线性 etcd read，用于核对当前 key、revision、value 和 lease。这增加启动及选主切换的 quorum RTT，不进入请求、Flush、keepalive 或普通 topology 热路径。部署仍要求每个活跃进程的 server ID 唯一；同 ID 多进程隔离不属于兼容承诺。

### Standalone 模式绑定

Standalone frontend 的 Bind 和 Close 继续执行本地 UID ownership、Redis restore/flush 以及重绑清理，但不再尝试 cluster Fork 或 Notify。应用不应依赖 Standalone 模式产生集群广播副作用。

### NATS RPC 入站校验

NATS RPC server 会在 protobuf 解引用前拒绝以下输入，并继续处理后续消息：

- 空 payload、超过 16 MiB 的 Request payload，或者缺少 `Msg`。
- 非 `Sys` 或 `User` RPC type，以及非 Request/Notify message type。
- 空 route 或者无法解码的 route。NATS protobuf route 不受客户端一字节 route-length 字段的 255 bytes 限制，仍受整个 Request 16 MiB 上限约束。
- 单个 message data 超过 16 MiB、负 session ID，或者编码后超过 16 MiB 的 session。
- Request 缺少 transport reply subject，或者 reply 超过 4 KiB、含空 token、空白或 NATS wildcard。Notify 携带 reply 时也使用同一 subject 规则。

内部 publisher 也必须生成满足这些约束的 Request，不能依赖 proto3 空对象被接纳。拒绝会计入 `dropped_messages`。消息 pump panic、subscription channel 异常关闭等非正常退出会触发 App 停止信号；正常 Shutdown 不报告故障。

`NatsRPCServer.BeforeShutdown` 和 `Shutdown` 共享幂等的停止屏障。正常关闭会先冻结 subscription admission，对全部 server-owned 入站订阅执行 Drain，并等待每个订阅进入 closed 状态及异步 push/kick callback 返回。确认 NATS 不再写本地 channel 后，message、binding、push 和 kick consumer 会完整消费已经交付到本地缓冲的消息；message pump 最后关闭私有的 request dispatcher channel，使 dispatcher 追平全部任务接纳。

本地 consumer 全部退出后，server 才关闭 RPC task admission、取消关停前已接纳 task 的 context 并等待 task 完成，最后 Drain NATS 连接并等待 terminal callback。业务 callback 必须监听 context 并及时返回，否则会耗尽下文的 30 秒 runtime quiescence 预算。subscription Drain 或 Unsubscribe 异常，或者 NATS connection 在 Shutdown 前或过程中已经关闭时，会退回快速取消路径。该路径不再等待底层不会发送的 subscription closed 状态，会冻结新的 push/kick callback、取消本地 consumer，并等待已经进入的 callback 退出；它不能承诺本地缓冲零丢失。依赖旧版 Shutdown 很快返回的应用需要重新分配关闭预算。

`GetBindingsChannel()` 返回的绑定投递通道在 Shutdown 后同样保持开放。旧代码若通过 `range` 等待该通道关闭，升级后会持续阻塞；应改为监听应用自己的退出信号，并以 `NatsRPCServer` 或 `App` 的 Shutdown 完成作为生命周期边界。

`NatsRPCClient.BeforeShutdown` 保持 outbound RPC 可用，让已经接纳的 handler 在 drain 期间继续调用 `Send`、`Publish`、`Call` 和 `Fork`。最终 `Shutdown` 才停止接纳这些调用、关闭连接，并等待 NATS terminal callback 及排在它之前的异步 transport callback 全部完成。直接管理 client 生命周期的应用必须把这段等待纳入现有 30 秒关闭预算，且不能在 `Shutdown` 开始后发起新 RPC。NATS server 的 Drain 同样等待 terminal callback，module 返回后不会再有 transport callback 访问已经拆除的 logger 或 App 依赖。

## 应用启动和关闭

App 先初始化 components、stateful pools、RPC 和 service discovery modules，再启动所有 Acceptor。全部 Acceptor 都报告 ready 后才启动 connection reader、进入 running 并执行 `onStarted`。首个 listener ready 不再表示业务流量已经开始处理；bind、TLS 或 ready 失败由 `App.Start` panic 传播。

关闭顺序固定为：

```text
not-ready -> stop listeners/readers/dispatchers -> drain -> close accepted connections -> wait handlers -> module BeforeShutdown -> wait accepted tasks -> ForceCloseAll -> module Shutdown -> components
```

Shutdown 开始后不再接受新连接。SIGTERM drain 期间已经接纳的连接仍可完成请求；drain 结束后 App 主动关闭所有已接纳连接并等待 connection handler 和 Agent，随后通过所有 module 的 `BeforeShutdown` 停止 RPC、服务发现 callback 等框架入站。入站屏障完成后，App 才等待已接纳的本地/远端 session task、执行最终 session 强制关闭，再调用 module `Shutdown` 和 component teardown。依赖 shutdown 期间继续建连、module shutdown 后继续使用连接，或者 `App.Start` 返回后继续占用监听端口的代码必须删除。

关停有两个独立的进程级门禁。Acceptor 和 timer dispatcher 共用 5 秒停止预算；Acceptor 忽略 context cancellation 和 `Stop` 时返回 `ErrAcceptorShutdownTimeout`，同步 timer callback 阻塞时返回 `ErrDispatcherShutdownTimeout`。可选的 SIGTERM session drain 结束后，runtime quiescence 另有 30 秒总预算，依次覆盖 connection handler 和 Agent、metrics/worker reporter、`onStarted`、Worker runtime、所有 module 的 `BeforeShutdown`，以及本地/远端 handler task。`Agent.Handle` 会等待已经接纳的写循环、心跳和集群缓存保活全部退出；自定义连接写入或存储续期若阻塞，会占用这段 30 秒预算。框架会取消 reporter、Worker RPC job、Handler 和 NATS task 的 lifecycle context；`Worker.Stop` 随后进入内置兼容 runtime 的 `QuitAndReset` 停止屏障，等待全部内部 goroutine、已接纳 job 及其 Redis 确认退出，再删除旧 manager。业务 callback 和 `onStarted` 必须响应自己的取消/退出条件，不能依赖框架强杀 goroutine。

任一 5 秒门禁失败，或者 runtime quiescence 超过 30 秒，`App.Start` 都会分别以 `ErrAcceptorShutdownTimeout`、`ErrDispatcherShutdownTimeout` 或 `ErrRuntimeShutdownTimeout` panic，并跳过最终 `ForceCloseAll()`、module `Shutdown` 和 component teardown。超时后迟到完成的 goroutine 不会再触发这些资源释放步骤，避免它访问已经拆除的依赖。该 panic 是进程级失败信号，不再表现为正常返回；supervisor 必须按非零退出处理并重启进程。应用不得 recover 后在同一进程重启或继续服务；业务 callback 和 timer callback 应自行设置 deadline，且在 shutdown 后不得继续访问框架或业务依赖。

默认 Builder 会分别创建容量为 1 的内部依赖故障通道和公开兼容通道。内建 NATS、etcd 和 Agent 只持有内部通道，该通道只由 App 消费；故障到达后，App 先发布幂等停止信号，再把事件尽力转发到 `Builder.DieChan` 和 `GetDieChan()` 返回的兼容通道。外部代码即使正在消费兼容通道，也不会截走内建依赖触发的停机信号。

仓库内部不会关闭这两个通道，生产者也只做非阻塞发送。`Builder.DieChan` 仍可作为自定义组件的兼容停机入口，但它是事件队列，不是广播或完成通知；多个消费者会竞争同一事件。直接调用 `NewApp` 时，传入通道仍同时承担兼容入口和依赖故障输入，以保留原有构造契约。需要隔离内建故障信号时应使用 Builder 构造 App。

下游不能关闭 `Builder.DieChan` 或 `GetDieChan()`，也不能通过接收该事件判断关停已经完成。等待完整关闭应等待 `App.Start` 返回。`App.Shutdown` 使用独立的 `sync.Once` 停止信号，并发调用安全。

Acceptor 在 ready 后意外返回错误或 nil 都会触发 App 停止。自定义 Acceptor 必须遵守同样的生命周期，否则 App 会一直等待。

## 验证工具迁移

### 基准测试工具

`benchmark-test-nats` 现在独占一个隔离的 NATS、etcd、Redis Compose project，并负责测试 server 和清理。它不再依赖独立 PID 文件或 Compose cleanup target。

使用 `BENCH_PATTERN`、`BENCH_TIME`、`BENCH_COUNT`、`BENCH_TIMEOUT` 和 `BENCH_OUT` 控制 workload。`benchmark-regression` 默认生成十个 benchstat-compatible 样本。旧 `ensure-testing-bin`、`ensure-testing-deps`、`bench-nats-sv`、`bench-grpc-sv`、`ensure-e2e-deps-grpc` 和 `kill-testing-deps` target 已删除；`benchmark-test-grpc` 只返回 NATS-only 迁移提示。

managed server 会清除继承的 `PITAYA_*` 配置，只接收 harness 管理的依赖 endpoint。`BENCH_COUNT` 范围是 1 到 100；iteration-form `BENCH_TIME` 范围是 1x 到 1000000x；`BENCH_TIMEOUT` 必须是正的整数 Go duration 组合，最小 1ns；`BENCH_OUT` 必须指向普通文件。

失败或零匹配不会发布正式 `BENCH_OUT`，partial raw output 保留为 `<BENCH_OUT>.failed-<project>`。cleanup 失败也会使命令失败并输出有界诊断。并发运行必须使用不同输出路径，相邻 lock directory 会拒绝复用。SIGKILL 留下的 stale lock 只能在确认对应进程已退出后手工删除。

multi-client benchmark 现在按 operation 轮转 client，并校验每个响应。旧实现通常只命中一个 client，因此旧结果不能作为等价基线。base 和 candidate 必须使用同一份当前 harness 和相同参数。

### 正式基准测试门禁

`benchmark-gate-nats` 使用固定策略，不复用 `BENCH_PATTERN`、`BENCH_TIME`、`BENCH_COUNT` 或 `BENCH_OUT`。正式运行必须通过：

```sh
scripts/run-benchmark-gate-controller.sh [--repo OWNER/REPO] [--bootstrap] <candidate-sha> <base-sha>
```

candidate 和 base 必须是完整的小写 SHA，candidate 等于 clean local `HEAD` 和当前 origin `main`，base 是不同的祖先。same-UID harness 不是恶意 PR 代码的隔离边界，candidate 必须已经过维护者审核并可信。

两个被测 revision 都不要求自身包含当前 verifier 或 policy，base 可以早于它们；candidate 和 base 仍必须由同一个 pinned gate/harness 及 policy 测量，不能分别采用各自 revision 内的工具或策略。依赖 vendor mode 或 `default.pgo` 的现有自动化不能直接使用这个正式 target；如需保留这些条件，必须另行定义并审核 apparatus policy。

controller 从受保护的 `benchmark-gate` GitHub Actions Environment 读取唯一的 gate 和 policy pin。受信 gate 从 pinned commit 导出工具和 policy，验证 tools module cache，并强制两个测量 worktree 使用 module mode、关闭 automatic PGO。它会清除继承的 Git repository、object 和 config 路由，禁用 replacement object，拒绝 graft 或 private attribute 元数据。

detached worktree 的 tracked blob 和 executable mode 必须与 pinned tree 完全一致。clean/smudge、sparse、LFS 或其他转换 checkout 都会 fail closed。policy 保护的 harness package 在实际 Linux/amd64 build graph 中选中的源码和嵌入文件会计入 apparatus digest；选中的 ignored 或 untracked 文件会使 gate 失败。

`gate.json` 保存每项十个样本。verifier 重算 median、包含一个 normalized-float ULP 表示余量的阈值，以及 zero-allocation 决策。`compare` 校验 sealed result 和 metadata 并记录 digest；最终验证重新检查 policy 配置、benchmark inventory 和四个 artifact 的 exact bytes。

exit status 3 表示已经完成回退判定并发布 artifact，不能当作基础设施失败丢弃。exit status 1 才表示环境、来源或可比性失败。

正式判定必须在固定的 bare Linux/X64 主机上运行。进程要位于 Linux initial cgroup namespace，相关 hierarchy 从 `/` 完整挂载，没有 nested mount 或 overmount，并至少有一个可读的 CPU/cpuset control。内核还要提供 nsfs namespace identity/type 和 `statx` mount ID。Docker、Kubernetes runner、缺少这些证明的旧内核以及 cgroup v1 `noprefix` hierarchy 只能用于信息性运行。

主机需要 Go 1.26.5、Docker/Compose、上述 cgroup 可见性，以及可写的 `/var/lock/pitaya`。仓库不能注册 self-hosted GitHub runner。

`benchmark-gate` Environment 使用只允许 `main` 的 custom deployment policy，并配置：

```text
BENCH_GATE_SOURCE_SHA=746624aa8874b7305fa913901c7b434fbc360a36
BENCH_POLICY_SHA256=b566bf3367e53302ef20342057b2a5c2de78436dbc0eaf85d5fd968844090f4b
```

删除 repository 或 organization 层的同名变量，也不要从宿主环境或 controller 参数传入 pin。启用或轮换前运行：

```sh
scripts/audit-benchmark-gate-config.sh --repo alkaid/pitaya
```

审计会检查 Environment pin、唯一 `main` deployment policy、零 repository runner，以及 pinned policy bytes。API 权限不足、分页或 schema 错误、缺失值、shadow variable、runner registration 或 digest 不匹配都会失败。

GitHub 两个变量的更新不是原子事务。轮换时先停用 controller，更新两项并完成审计，再运行一次信息性 bootstrap。bootstrap evidence 保留在 `.bootstrap-controller-<run-id>`，只安装绑定完整 tuple 的 host-local marker，不创建正式输出。

普通运行要求 marker 和前后两次审计结果一致，并再次核对远端 `main`。临时 worktree 清理成功后，pending evidence 才会以 no-clobber 方式提升为 `nats-gate-<run-id>`。signal 会转发给 owned gate process。source installation、bootstrap 和 formal blocking evidence 是三个不同状态。

正式输出目录的 parent 不能含 symlink，相对路径必须留在 candidate checkout 下。开始运行被测代码前会关闭 runner-lock FD。base 固定先于 candidate 运行；no-overlap 只消除并发争用，不能消除温度和缓存顺序影响。边界结果应使用 counterbalanced run 或 production profile 复核。

### NATS 重连与压力门禁

`make test-stress-nats` 和独立 nightly/manual workflow 默认保持 10,000 个 fresh TCP client，再用 1,000 个 fresh client 做同 UID replacement。它用于容量和 Session ownership 验证，不进入 `verify-fast` 或普通 release lane。缩小 workload 只能作为 smoke。

stress harness 为隔离测试 server 注入 NATS message buffer 16,384，用于吸收 cleanup 阶段最多 10,000 个 `SessionClosed` 通知。生产默认仍为 75，不要把测试值复制到线上配置。

`helpers.TestServerConfig` 新增 `NATSMessagesBuffer` 和 `OutputMarkers`。零值保持旧 helper 行为；`NATSMessagesBuffer` 只接受非负值，正值才覆盖测试 server。`helpers.StartServer` 的签名和自定义 `PITAYA_TEST_SERVER_BINARY` 参数集合保持不变。

`client.ConnectToStress` 只在 `benchmark && stress` build tag 下存在，用于 fresh、未连接的 stress client，并允许 context 取消 TCP handshake。它不是生产 `ConnectTo` 的替代 API。

成功时会原子发布 `<STRESS_OUT>` JSON 和相邻 `.log`，同一路径由 lock directory 串行化。run ID、配置、主机限制、MID/body/error、ownership、连接和关闭计数，以及 cleanup 后零存活都必须匹配。失败证据使用 `<STRESS_OUT>.failed-<project>*`，旧 JSON 或零匹配测试不能作为本次通过。

### 覆盖率工作流

Tests workflow 的手工运行必须提供完整 `coverage_base_sha`：

```sh
gh workflow run tests.yaml -f coverage_base_sha="$(git rev-parse HEAD^)"
```

coverage job 会校验该 commit 是被测 head 的祖先。PR 使用 tested merge commit 的 first parent，push 使用 event 的 exact before commit。PR 不再上传 Coveralls，但仍执行本地 baseline 和 90% diff coverage gate。generated code、mock、example 和 test-only helper 只按 `scripts/coverage-baseline.json` 中的 literal path 排除。

需要 provenance 的 gate profile 必须通过 `make coverage-stamped` 生成，不能直接使用 `go test -coverprofile`。运行前，root production sources 和 `tools/cmd/coveragegate` module 的全部输入都必须已提交；这些输入处于 dirty 状态或来自混合提交时，不能生成正式 provenance。sidecar 绑定 profile SHA-256、完整 commit ID 和 Linux/amd64 policy。缺失、被修改、混合 commit 或其他平台的 sidecar 都会被拒绝。

`make coverage`、`test-coverage-html` 和 `coverage-func` 继续用于 dirty WIP 报告，不要求 provenance。`COVERAGE_PROFILE` 统一控制 HTML 和 function report 输入。

root coverage gate 只识别有独立验证 lane 的 `pitaya-cli`、`tools` 和 `xk6-pitaya`。新增、删除或重命名 nested `go.mod` 时必须先在 verifier 注册并建立独立 lane，否则 fail closed。

当前保守 baseline 是 `4184/7308 = 57.25%`，最近验证为 `4214/7308 = 57.66%`。overall、逐包、package set 和文件清单不能回退；新增或修改的 executable production 行要求 90% diff coverage。刷新 baseline 只接受 clean checkout 上同一 commit 的独立 green stamped profile。

### 模糊测试门禁

`scripts/fuzz-targets.json` 是 root fuzz target 的唯一清单。PR、push 和 manual lane 运行 `make test-fuzz-seeds`，nightly workflow 默认每个 target mutation 2 分钟。

新增、删除或重命名 fuzz target 时必须同步 manifest。source/manifest 不一致、corpus 未执行、任一 corpus skip/fail 或 mutation execution 为零都会失败。单 target 调试使用 `make test-fuzz-one`，package 和 target 必须已注册。

## 升级验收清单

1. 把 root、CLI、xk6 和应用的 import/require 全部切到 `github.com/alkaid/pitaya/v2`，删除 upstream replace。
2. 使用 Go 1.26.5 编译全部模块，并按 k6 v1/Sobek、grpc-go 1.64+ 和 Uber Mock 要求更新依赖。
3. 处理所有 `StatefulPool.Go/Wait`、keyed `co.Go*/Wait*`、`cluster.GoWithRequest` 和 `SessPublic.Go` 的新错误返回。
4. 替换全部 `TaskGoProvider`，证明每个已接纳任务都会等待完成。
5. 删除应用主 backend callback，只保留 after callback；更新自定义 backend coordinator capability。
6. 更新精确 `NewStatefulPoolsModule` 函数类型，并在 module shutdown 前停止接纳、drain 任务。
7. 迁移自定义 Acceptor、wrapper 和保存的 `ListenAndServe*` 函数值。
8. 把外部 `GoPool`、`ClusterRPCConfig`、测试配置和 `ants.Options` 改为 keyed literal，核对 128/1 escape budget、`persist` 配置和 `TaskBuffer` 容量。
9. 更新生成 mock、subscriber fork method 列表、fanout 消费方、trusted proxy helper 和 CLI file-mode 脚本。
10. 在 staging 验证全部客户端握手顺序、浏览器 Origin、WSS、PROXY header 超时、4 byte + 16 MiB WS 边界和压缩载荷边界。
11. 验证全部 Acceptor ready 前不启动 connection reader，Shutdown 后端口不可连接；确认 module `BeforeShutdown` 先停止 RPC/服务发现入站，NATS server 已排空本地缓冲并完成 terminal callback，任务 drain 和最终 session 扫描完成后才执行 module `Shutdown`，NATS client terminal callback 在最终 `Shutdown` 中完成。正常关停时 `App.Start` 必须等待 handler、reporter、`onStarted`、Worker、module 和 component 退出，Worker RPC context 被取消，scheduler/fetcher/manager/worker 及 Redis 确认全部退出且旧 manager 已清除；阻塞 Acceptor 或 timer dispatcher 超过共用的 5 秒预算时，必须观察到对应的 `ErrAcceptorShutdownTimeout` 或 `ErrDispatcherShutdownTimeout`，其余 runtime 无法在 30 秒内 quiesce 时必须观察到 `ErrRuntimeShutdownTimeout`。三种情况都必须形成进程级失败，并跳过最终 session/module/component teardown。
12. 发送空 payload、缺 `Msg`、非法 route/reply 的 NATS Request，再发送合法 Request，确认消息泵继续服务。
13. 在独立 Prometheus registry 中 Gather custom histogram，确认 `pitaya_handler_response_time_ns` 仍是 SUMMARY，且没有同名 `_bucket`。
14. 用生产等价的 pool 配置验证普通 pool 允许有界 escape、`persist` 保持严格，并观察 `pool_escape_budget_exhausted`、escaped worker、RSS、GC pause 和热点 UID 延迟。
15. 回归 `sys` route、heartbeat packet、session close 幂等、closed session Bind/Store、`WithForkMethods`、fanout 排序和部分错误。
16. 在精确框架提交上运行 `make test-consumer`、应用 compile/race/vet 和真实 NATS E2E。应用边界不得丢弃新增 admission error，应传播或记录结构化日志。
17. 使用生产 Redis ACL 回归 Worker enqueue、fetch、确认、retry 和 scheduled job；确认允许 `EVAL` 及脚本内的 `ZSCORE`、`LPUSH`、`ZREM`，并验证关停时成功 job 不会在重启后重放。
18. 只有在 fork tag 发布，并且无 `replace` 的 consumer 能从公共 Go proxy 下载该 exact tag 后，才发布应用升级。
