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

根模块、`pitaya-cli` 和 `xk6-pitaya` 都要求 Go 1.26.5 或更高版本。

`xk6-pitaya` 从 k6 v0.46.0 升到 v1.8.0，对外类型从 Goja 切到 `github.com/grafana/sobek`。自定义扩展需要按 k6 v1 运行时重新构建，并把 `goja.Promise`、`goja.Object` 和 `goja.ConstructorCall` 改为对应的 Sobek 类型。

`xk6-pitaya` 的 request waiter 现在会在响应可投递前完成注册，并在响应、发送失败、超时、VU context 取消或断连后按 request identity 清理。断连会立即 reject 全部未完成 promise；迟到和重复响应会被丢弃，不再创建孤立 waiter。公开 JS API、`requestTimeoutMs`、指标名以及非 JSON 响应继续以原始字节数组 resolve 的行为保持不变。若底层 client 产生重复 request ID，连接会按协议错误关闭，所有未完成 promise 都会 reject，避免把响应错配给旧请求。

生成的 gRPC stub 使用 support package v9，要求 grpc-go 1.64 或更高版本。嵌入 `protos.UnimplementedPitayaServer` 时必须按值嵌入，不能嵌入可能为 nil 的指针。即使部署没有启用 gRPC，这仍然是编译期迁移。

`helpers.GetTestEtcd` 现在返回 `*integration.Cluster`，不再返回 `*integration.ClusterV3`。这只影响直接使用该测试 helper 的外部测试代码。

### 生成测试 Mock

第一方 mock 已切到 `go.uber.org/mock/gomock`，并根据当前接口重新生成。公开 mock 方法和 `EXPECT()` recorder 的参数或返回值可能发生变化，包括 entity/agent 的 close 和 kick、session bind 以及 Pitaya RPC。

下游测试应统一 gomock import、controller 和依赖版本，并使用固定版本的 Uber Mock 重新生成自有 mock，不要手改生成文件。使用 unkeyed 测试配置 literal 的代码也应改成 keyed literal，避免新增 exported 字段再次造成编译失败。

### 已删除的 Logrus 包

第一方 `logger/logrus` 适配器和 `logger/test` 测试辅助包已删除，不提供兼容层。

服务端日志改用当前基于 Zap 的 `logger` API。client 包仍保留独立的 logger hook，但它不能替代服务端全局日志配置；需要继续使用 Logrus 的应用应自行维护 adapter。

## 公共 Go API

### 运行期注册返回错误

四个运行期注册方法现在直接返回 `error`：

```go
LazyRegister(component.Component, ...component.Option) error
LazyRegisterRemote(component.Component, ...component.Option) error
RegisterInterceptor(string, *component.Interceptor) error
RegisterRemoteInterceptor(string, *component.Interceptor) error
```

普通的 `app.LazyRegister(...)` 调用语句仍可编译但会丢弃结果；升级时应全部改为检查错误。把这些方法保存为精确函数值、自定义实现 `pitaya.Pitaya`、或从该接口生成 fake/mock 的代码必须更新 method set 并重新生成测试产物。

重复 service、保留 handler service 名、非法 handler/remote 签名、nil interceptor、nil interceptor function、空白/带点号的 interceptor service 名都会同步返回错误，不再只写日志或在调用时 panic。Standalone 或不完整测试 App 缺少对应能力时分别返回可用 `errors.Is` 匹配的 `service.ErrHandlerServiceUnavailable` 或 `service.ErrRemoteServiceUnavailable`。

handler 与 interceptor 的热注册改为注册侧串行、读侧不可变快照。一次 handler service 注册只发布一个包含全部 method 的新快照；注册返回后开始的 lookup 一定可见，在途调用可以继续使用已取得的旧 handler。`HandlerPool.GetHandlers` 返回 map 复制品，修改该 map 不再影响运行期 registry。

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

TaskExecutor、task 或反射调用的 handler/remote panic 都只返回稳定 sentinel，
不再拼接 panic 值或 method 名。反射调用使用 `util.ErrRPCHandlerPanic`；
依赖匹配 panic 文本的代码必须改用 `errors.Is`。这些状态描述接纳结果，
不覆盖 task 已运行后的 handler 业务错误。

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

Worker 启动恢复只扫描 `<queue>:<current-hostname>:inprogress`。替换实例如果使用不同 hostname，不会自动接管旧 hostname 下的 in-progress job；旧 list 会保留在 Redis 中但不会被新实例消费。需要崩溃恢复的部署必须为 Worker 使用稳定 hostname（例如 StatefulSet 的稳定 pod identity），或在更换 hostname 前排空队列并确认旧 in-progress list 为空。随机 pod 名称的 Deployment 不能被视为已经具备跨实例 orphan recovery；本批未提供安全的多实例孤儿认领协议。

retry payload 的 `error_message` 现在只保存 recovered panic 的 Go 类型，不再调用或持久化 panic 值的 `Error`/`String` 文本。依赖该字段提取业务错误详情的消费者必须改用显式 job error/result 字段；panic 详情不属于稳定 wire contract。

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

### 客户端 TLS、连接与分帧

`client.Client.ConnectTo` 对 `wss://`、`tls://` 和 `tcps://` 连接不再隐式设置 `InsecureSkipVerify`。默认使用系统信任链并校验证书主机名；传入 `tls.Config` 时使用其克隆，调用方的 `ServerName`、信任池和其他字段不会被框架改写。WebSocket 连接也只修改 `websocket.DefaultDialer` 的值副本，不再把进程级默认 dialer 变成共享可变状态。

使用私有 CA 或自签证书的应用必须把 CA 安装到系统信任链，或在库调用处显式传入 `tls.Config.RootCAs`。依赖错误证书仍能连接的环境会在升级后失败。调用方仍可自行显式设置 `InsecureSkipVerify=true`，但该选择完全由应用承担，框架和 CLI 不会代为开启。

`pitaya-cli` 现在只尝试用户指定的 scheme，不再按 `wss -> ws -> tls -> tcp` 顺序自动探测，也不会在 TLS 失败后静默降级到明文。未写 scheme 的地址保持 legacy 含义，确定性解释为 `tcp://`。TLS 用户必须显式写 `wss://`、`tls://` 或 `tcps://`。

客户端 dial 和 Pitaya 握手都有 5 秒有界超时。握手首包不是 `Handshake`、响应 JSON/压缩体非法、ACK 写失败或服务端停滞时，`ConnectTo` 返回错误并关闭、清除本次连接；失败的连接不会启动 heartbeat、消息 reader 或 disconnect callback。服务端返回零或负 heartbeat 时客户端不启动 ticker。

TCP/TLS/WS 输入按 4 byte header 中声明的长度精确读取，不再以一次 `Read`、短读或 1024 byte 整数倍推断 packet 边界。合法的任意 header/payload 分段和粘连 frame 保持原 wire format；截断 header/body、非法 packet type 或畸形 Data message 会作为协议错误受控断连。message 解码失败不会向 `MsgChannel` 投递 nil，也不会把原始 payload 写入日志。

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

`TCPAcceptor` 启用 PROXY protocol 时，客户端必须在 5 秒内完成 header。header 使用最多 64 路有界并发解析，单个慢连接不会占住主 accept loop；容量耗尽时新连接会被立即关闭，不进入业务 handler。超时、半包或非法 header 会关闭该连接，成功解析后清除 deadline。`TCPAcceptor.Stop` 或 Serve context 取消会立即关闭所有解析中的 raw connection，不等待 header deadline；关闭 wrapper 后，正在等待的 `Accept` 统一返回 `net.ErrClosed`，不再非确定性透传底层 listener 的关闭错误。未启用 PROXY protocol 的 TCP 连接直接使用底层 listener，不占用这 64 个解析槽位。

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

两类 listener 都是非关键 observer。单个 topology listener panic 会记录结构化恢复日志，并继续同一事件的其余 listener；leader listener panic 也不会阻止后续 generation。权威 topology/leader 状态已经在 callback 前提交，不会因 observer panic 回滚。dispatcher 的 barrier 和 drain 在 panic 后仍会完成。旧代码若依赖 listener panic 终止进程，必须改为显式向应用 supervisor 报告不可恢复错误；框架不再把 observer panic 当作隐式 fail-fast 信号。

`Init` 等待初始 topology callback 和首个已确认的非空 leader 状态，但 leader listener 自身在独立 lane 异步执行。`SyncServers` 会等待本次同步接纳的 topology callback，`SDListener` 不能同步调用它，否则会等待自己所在的 lane。

正常关闭顺序必须是 `BeforeShutdown -> Shutdown`。两者一进入就会立即使本地 owner epoch 失效，带旧 epoch 的新 RPC 会在 callback drain 或 lease revoke 完成前 fail-closed。`BeforeShutdown` 随后停止新 callback 接纳、取消 Flush、等待已接纳 callback drain，再 revoke lease。callback 不能同步调用 `BeforeShutdown`；callback 内可以调用 `Shutdown`，但这不等价于立即 revoke，注册可能继续存在到 lease TTL，默认最长约 60 秒。

leader 候选会增加一次有界的线性 etcd read，用于核对当前 key、revision、value 和 lease。这增加启动及选主切换的 quorum RTT，不进入请求、Flush、keepalive 或普通 topology 热路径。部署仍要求每个活跃进程的 server ID 唯一；同 ID 多进程隔离不属于兼容承诺。

### Standalone 模式绑定

Standalone frontend 的 Bind 和 Close 继续执行本地 UID ownership、Redis restore/flush 以及重绑清理，但不再尝试 cluster Fork 或 Notify。应用不应依赖 Standalone 模式产生集群广播副作用。

Standalone 或自定义构造的 `HandlerService` 没有 `RemoteService` 时，显式路由到其他 server type 不再静默等待客户端超时。Request 会立即返回 HTTP 503 语义的 `remote_service_unavailable`，metadata 只包含 `route` 和 `server_type`；Notify 不发送 response，只记录不含 payload 的结构化 warning，并完成对应 trace 和 response-time metric。本机 route 以及配置完整的 Cluster route 行为不变。调用方若确实需要跨服路由，必须提供与当前 server/session graph 一致的 `RemoteService`，不能把客户端 timeout 当作能力探测。

### NATS RPC 入站校验

NATS RPC server 会在 protobuf 解引用前拒绝以下输入，并继续处理后续消息：

- 空 payload、超过 16 MiB 的 Request payload，或者缺少 `Msg`。
- 非 `Sys` 或 `User` RPC type，以及非 Request/Notify message type。
- 空 route 或者无法解码的 route。NATS protobuf route 不受客户端一字节 route-length 字段的 255 bytes 限制，仍受整个 Request 16 MiB 上限约束。
- 单个 message data 超过 16 MiB、负 session ID，或者编码后超过 16 MiB 的 session。
- Request 缺少 transport reply subject，或者 reply 超过 4 KiB、含空 token、空白或 NATS wildcard。Notify 携带 reply 时也使用同一 subject 规则。

内部 publisher 也必须生成满足这些约束的 Request，不能依赖 proto3 空对象被接纳。拒绝会计入 `dropped_messages`。消息 pump panic、subscription channel 异常关闭等非正常退出会触发 App 停止信号；正常 Shutdown 不报告故障。

传播 context 中的 `reqTimeout` 现在只接受可由 `time.ParseDuration` 解析且大于零的 string。缺失值使用 NATS client 默认 timeout，并以规范化 duration string 传播；wrong type（包括 JSON number 解码后的 `float64`）、空串、非法、零或负值会在 marshal 和 NATS publish 前返回 `invalid_request_timeout` BadRequest。合法 override 仍受父 context 的更早 deadline 限制。依赖数字 timeout 或把非法值当作立即超时的调用方必须改为正 duration string。

user push/kick subscription 解码失败后会立即丢弃消息并计入 `dropped_messages`，不再向下游投递零值 protobuf。诊断日志只包含 channel kind、payload 长度、累计 malformed 数和 decode error，不包含 payload。空 protobuf 在 wire 上仍是合法零值对象；只有 `proto.Unmarshal` 失败的 payload 属于本条拒绝规则。

`NatsRPCServer.BeforeShutdown` 和 `Shutdown` 共享幂等的停止屏障。正常关闭会先冻结 subscription admission，对全部 server-owned 入站订阅执行 Drain，并等待每个订阅进入 closed 状态及异步 push/kick callback 返回。确认 NATS 不再写本地 channel 后，message、binding、push 和 kick consumer 会完整消费已经交付到本地缓冲的消息；message pump 最后关闭私有的 request dispatcher channel，使 dispatcher 追平全部任务接纳。

本地 consumer 全部退出后，server 才关闭 RPC task admission、取消关停前已接纳 task 的 context 并等待 task 完成，最后 Drain NATS 连接并等待 terminal callback。业务 callback 必须监听 context 并及时返回，否则会耗尽下文的 30 秒 runtime quiescence 预算。subscription Drain 或 Unsubscribe 异常，或者 NATS connection 在 Shutdown 前或过程中已经关闭时，会退回快速取消路径。该路径不再等待底层不会发送的 subscription closed 状态，会冻结新的 push/kick callback、取消本地 consumer，并等待已经进入的 callback 退出；它不能承诺本地缓冲零丢失。依赖旧版 Shutdown 很快返回的应用需要重新分配关闭预算。

`GetBindingsChannel()` 返回的绑定投递通道在 Shutdown 后同样保持开放。旧代码若通过 `range` 等待该通道关闭，升级后会持续阻塞；应改为监听应用自己的退出信号，并以 `NatsRPCServer` 或 `App` 的 Shutdown 完成作为生命周期边界。

`NatsRPCClient.BeforeShutdown` 保持 outbound RPC 可用，让已经接纳的 handler 在 drain 期间继续调用 `Send`、`Publish`、`Call` 和 `Fork`。最终 `Shutdown` 才停止接纳这些调用、关闭连接，并等待 NATS terminal callback 及排在它之前的异步 transport callback 全部完成。直接管理 client 生命周期的应用必须把这段等待纳入现有 30 秒关闭预算，且不能在 `Shutdown` 开始后发起新 RPC。NATS server 的 Drain 同样等待 terminal callback，module 返回后不会再有 transport callback 访问已经拆除的 logger 或 App 依赖。

## 应用启动和关闭

App 先初始化 components、stateful pools、RPC 和 service discovery modules，再启动所有 Acceptor。全部 Acceptor 都报告 ready 后才启动 connection reader、进入 running 并执行 `onStarted`。首个 listener ready 不再表示业务流量已经开始处理；bind、TLS 或 ready 失败由 `App.Start` panic 传播。

启动现在使用显式事务账本。框架会在任何 `Component.Init` 前预检完整的 handler/remote 注册批次和 framework module 名称；重复 service、保留名、非法签名、Standalone 上的 remote 注册、缺失 remote capability，以及与 `rpcServer`、`rpcClient`、`serviceDiscovery`、`uniqueSession`、`configLoader`、`statefulGoPool` 冲突的 module 名都会使 `App.Start` panic。此时 component/module Init 和 Acceptor Serve 均未发生，`IsRunning()` 保持 false。旧版本依赖“记录注册错误后继续提供部分路由”的部署必须改为修正注册，不能再把该状态当作降级启动。

自定义生命周期增加明确的提交边界：`Component.Init` panic 或 `Module.Init` error/panic 的当前对象尚未提交，必须在失败前自行清理本次获得的资源，框架不会猜测半初始化对象能否安全 Shutdown。只要 Init 正常返回，对象就进入 App 账本；后续 Init/AfterInit 失败时，框架会逆序调用所有已提交 owner 的 `BeforeShutdown` 和 `Shutdown`。单个清理 hook panic 或 Module.Shutdown error 会与原始启动错误通过 `errors.Join` 合并，但不会阻止其他 owner 清理。`App.Start` 仍无返回值并以 panic 暴露该聚合错误，下游 supervisor 必须按启动失败处理；测试可以用 `errors.Is`/`errors.As` 匹配主错误和回滚错误。失败后的同一 App 不支持再次 Start。

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

## 审计第二、第三阶段兼容性变化

本节对应 `pitaya-code-audit-2026-08-19.md` 的 P1 和 P2 整改。这里列出的配置、返回错误、RPC 元数据和公共接口变化都可能影响现有应用；升级时不能只依赖编译通过。

### Builder 配置改为 fail-fast

`NewBuilder` 会在构造依赖前校验配置，`Builder.Build` 会再次校验可能被调用方修改过的 `Builder.Config`。任一校验失败都会以包含全部字段错误的 `invalid pitaya config` panic 终止构建，不再把非法值带到后台 goroutine 后才表现为忙等、索引越界或资源泄漏。

以下值现在必须为正数：heartbeat、metrics period、session cache TTL、memory-group tick、启用后的 session drain timeout/period、启用后的 rate-limit interval/limit、Worker concurrency/pool，以及 Cluster 模式下 RPC fanout、NATS connection/request timeout、NATS services/message/push buffer、etcd dial/heartbeat/sync/revoke/grant/retry interval 和 sync parallelism。etcd heartbeat TTL 还必须至少为 1 秒，因为 etcd lease API 以整秒接收 TTL；旧配置中的亚秒值会在 Build 阶段被拒绝。StatsD 启用时 `rate` 必须是有限数且位于 `(0, 1]`，NaN 和正负无穷值会在 Build 阶段被拒绝。agent buffer、Redis pool、GoPool buffer/escape budget 及部分可选 duration 只允许非负值。Prometheus、StatsD、Redis、Worker、NATS 和 etcd endpoint 的必需字段也会被验证。

etcd 用户名和密码必须同时为空或同时配置；启用 election 时必须提供名称。NATS `maxreconnectionretries` 继续接受 `-1`（无限重试）和 `0`（不重试），etcd `grantlease.maxretries=0` 也继续表示零次重试。默认 Builder 只构造 NATS transport，因此 `ValidateCluster` 不校验未启用的 gRPC 子配置。导出的 `NewNatsRPCClient`、`NewNatsRPCServer`、`NewGRPCClient` 和 `NewGRPCServer` 各自校验实际使用的子配置并返回 error；直接构造 gRPC 时 dial/request timeout 必须为正，server port 必须位于 1..65535。NATS 旧 sentinel 现在会被带字段路径的错误包装；`errors.Is` 仍可识别，但依赖 `err == constants.ErrNats...` 的代码必须改用 `errors.Is`。自定义配置生成器应直接调用 `PitayaConfig.Validate`，Cluster 部署还应调用 `ValidateCluster`，并在进入 Builder 前处理错误。

Builder 现在是 single-use，且公开字段配置只支持在一个 goroutine 中、首次 `Build` 前完成；并发或第二次 `Build` 会 panic。Build 入口会复制 Builder 自有的 acceptor、post-build hook 和 metrics reporter slices，并生成只读 resolved graph，后续 App、RemoteService、AgentFactory 和 HandlerService 不再交叉读取实时 Builder 字段。Build 返回后重新赋值 Builder 字段不会改接已构造 App；通过共享指针或嵌套 map/slice 修改对象仍是不受支持的 data race/共享 mutation。

Cluster 默认派生依赖不再允许局部 root 替换。单换 `SessionPool` 必须同时提供用该 pool 构造的 caller-owned `RPCServer`；单换 `MetricsReporters` 必须同时提供 caller-owned `RPCClient` 和 `RPCServer`；单换 `Server` 必须同时提供 caller-owned `ServiceDiscovery`、`RPCClient` 和 `RPCServer`。默认 `SessionPool` 仍在使用时，替换 `Redis`、`pitaya.storage.redis` 构造配置或 session cache TTL 也会失败，必须提供从新 cache graph 构造的 pool。Standalone 单换 `SessionPool` 继续支持。

不一致组合会在 logger 初始化、Router mutation、`RPCServer.SetPitayaServer` 和 post-build hook 前以 `invalid builder dependency graph` panic，并在错误中指出字段路径和成组替换方法。框架不会自动重建公开 transport，因为调用方可能已经在该实例上安装自定义配置。完整 caller-owned transport group 仍可使用，但其内部捕获的 roots 由调用方保证一致，Builder 不做反射式 introspection。

`pitaya.buffer.handler.localprocess`、`pitaya.buffer.handler.remoteprocess` 和 `pitaya.concurrency.handler.dispatch` 仍会被 Viper 解析，但已弃用且完全忽略，任意值都不再参与配置验证。偏离旧默认值 20/20/25 时，每次 Build 只记录一条合并 warning。容量应改由 `pitaya.buffer.agent.messages`、`pitaya.gopools.<pool>.taskbuffer`、`pitaya.cluster.rpc.server.nats.buffer.messages` 和 `.push` 调整；下一个 major 会删除这三个兼容字段及 `NewHandlerService` 的两个 legacy buffer 参数。

本批次为 NATS client/server 新增 TLS 和认证配置。TLS 默认关闭；生产集群必须显式启用，并选择 username/password、token 或 credentials file 中恰好一种认证方式。NATS URL 不再接受内嵌 userinfo，旧的 `nats://user:pass@host:4222` 必须改为普通 URL 加 `auth` 字段。TLS 最低版本为 1.2；`cafile` 为空时使用系统根证书，`certfile`/`keyfile` 可配置 client certificate。证书、credentials file 和相关配置只在 transport 构造时读取，轮换后必须重启进程。

主 service-discovery etcd、groups etcd、compatibility binding etcd、session Redis 和 Worker Redis 也支持相同的 client TLS 配置。etcd 的 `user`/`pass` 必须成对设置；Redis 允许只设置 password 以认证默认 user，但显式 username 时必须同时提供 password，Worker 会发送 Redis 6 ACL 的双参数 `AUTH username password`。读取或解析证书/credentials 失败会在构造阶段 fail closed，不会静默退回明文连接。

保留的 gRPC transport 仍不提供 TLS、peer authentication 或 mTLS，只能放在 loopback、受控 sidecar 网络或严格的双向网络 ACL 后。本批新增字段会改变多个 exported struct 的 unkeyed composite literal 形状，包括 NATS、etcd、Redis、Worker 和 groups/binding config；外部 `Type{value1, ...}` literal 可能不再编译，必须改为 keyed literal。Builder 的 `MetricsReporters` 也不再接受 nil 元素，`Build` 会指出对应下标并 panic。

### Redis session hash 的原子 TTL

导出的 `session.CacheInterface` 新增 `CacheTTL() time.Duration`。所有自定义 session cache、wrapper 和测试 mock 都必须实现该方法，否则升级后无法编译；返回值必须为正数，`SessionPool.SetClusterCache` 会对零或负 TTL 直接 panic。

`RedisCache.Hset`、`Hsetnx` 和 `Hmset` 现在用单条 Lua `EVAL` 原子执行 hash 写入与 `PEXPIRE`。新 key 在写入成功时立即带 TTL；已有 key 每次 hash 写入都会续期。`NewRedisCache` 对零或负 TTL 直接 panic，小于 1ms 的正 TTL 会提升到 Redis 可表达的 1ms。

受限 Redis ACL 除既有 hash 命令外必须允许 `EVAL`，以及脚本内的 `HSET`、`HSETNX` 和 `PEXPIRE`。这组脚本只访问一个 key，兼容标准 Redis Cluster 的单 slot 约束。

### Timer 执行与关闭语义

每个 App 现在固定启动一个全局 `TimerManager` 调度 owner，不再按 legacy handler dispatch 值提交 standby goroutine。`NewTimer` 和 `NewCountTimer` 保持源码兼容，但默认回调策略明确为 `timer.ExecutionSerial`：慢回调会阻塞后续 tick，且关闭会等待已经开始的回调完成。等待串行回调期间，owner 仍排空 timer 创建/关闭 mutation，因此回调内批量创建超过 channel capacity 的 timer 不会自锁，也不会推进新的 tick。

需要其他 overrun 行为时使用 `NewTimerWithPolicy` 或 `NewCountTimerWithPolicy`：

- `ExecutionSerial`：不重入；回调由独立 goroutine 执行，owner 同步等待并只处理 timer mutation；
- `ExecutionAsync`：每次到期都异步接纳，允许同一 timer 重叠；
- `ExecutionSkipIfRunning`：已有回调运行时跳过本次到期，不消耗有限计数。

nil callback、非正 interval 或未知 policy 现在 panic。`Condition.Check` 如果 panic，框架会记录错误并删除对应 timer，其他 timer 和 manager 会继续运行；依赖 condition panic 终止调度器或进程的代码必须改为显式故障信号。`TimerManager.Stats` 暴露 tick/callback lag 及各策略 overrun 计数；自定义 shutdown 必须取消 `Run` 的 context，并为业务回调设置自己的终止条件。

兼容入口 `timer.Cron()` 现在也参与同一个 owner 协议。`Run` 或另一轮 `Cron` 已持有 owner 时，它会拒绝本次 tick 并立即返回，不再并发推进 timer；需要观察接纳结果的调用方改用新增的 `timer.TryCron() bool`。应用启用 `Run` 后不能再依赖外部 `Cron` 同时执行回调。`Run` 取消后会在等待异步回调时继续排空已接纳的 create/close mutation，但不会推进新 tick；回调仍必须响应自己的终止条件。

`SetTimerBacklog` 现在会重建尚未使用的进程级 `timer.Manager`，并成为 `NewTimerManager(0)` 的默认容量。它必须在 `Run`、`Cron` 或任何进程级 timer 创建/增删 API 之前调用；Manager 一旦使用，再修改 backlog 会 panic。过去在运行中调用但实际不生效的代码必须移到应用初始化最前端。

### NATS、NotifyAll 和 fanout 错误

NATS client/server 会在断连时降为 not-ready，重连后恢复；最终连接关闭会取消 lifecycle context。`Shutdown` 冻结新操作并取消 client 侧在途 request wait。server 侧先关闭 subscription admission，再 drain 已接纳的 broker delivery；这些 delivery 的 lifecycle context 在处理完成前保持有效。排空成功后才停止 consumer、取消 lifecycle context、关闭连接并等待 transport callback 和已接纳工作结束；排空失败则进入强制取消路径。bind、request、push 或 kick consumer 若 panic 或意外返回，server 会立即降为 not-ready、停止其余 consumer，并触发 App die signal，不再以部分消息泵失效的状态继续宣告可用。Shutdown 开始后及连接不可用时，`Send`、`Call` 以及未启用 owner fencing 的低层 `Publish`/`Fork` 返回 `constants.ErrRPCClientNotInitialized`；fenced `Publish`/`Fork` 的 target-resolution 错误优先，见下文。调用方不能再假定 client shutdown 是 no-op。

NATS request task 会在 transport 边界恢复业务 panic，并把 panic、`nil,error` 或 `nil,nil` 的非法返回规范化为可序列化的 HTTP 500 status response；panic 细节只写服务端日志。带 idempotency key 的 panic 结果也会缓存非 nil error response，因此同 key 重试会及时收到相同错误，而不再持续 timeout 到去重 TTL 结束。依赖 panic 造成无响应超时的调用方必须改为处理显式 500 response。

gRPC unary transport 同样会在边界恢复 handler panic，记录服务端堆栈，并只向客户端返回不包含 panic 细节的 `Internal` status。gRPC server shutdown 会停止新 admission，并最多等待 5 秒让已接纳 RPC 完成；超时后调用强制 `Stop`。业务 handler 若可能长时间阻塞，必须响应请求 context 或实现更短的业务 deadline，不能再假定框架会无限等待。

`RemoteService.NotifyAll` 不再忽略逐目标错误或恒返回 nil。目标快照按稳定顺序执行，解析失败、RPC 失败、panic 和 context 取消会用 `errors.Join` 聚合返回。所有调用方都必须检查该错误；一次非 nil 返回可能表示其他目标已经成功，不能直接无条件重放整批副作用。

remote fanout 的并发所有权已收拢到单个 executor，默认上限为 `constants.DefaultRPCFanoutConcurrency`，Builder 使用 `pitaya.cluster.rpc.fanout_concurrency`。结果和目标错误采用稳定顺序，但仍是部分成功语义。

`RemoteService.DoFork` 不再在目标类型不同于本机类型时调用一次 type-wide `RPCClient.Fork`。它现在始终从 service discovery 取得目标快照，并对每个具体实例执行 `RPCClient.Call`，从而为每一 hop 写入该实例的 owner epoch。该调用会等待所有已开始的目标调用，并用 `errors.Join` 返回带 server ID 的目标错误；依赖 broker publish 立即返回、只实现 `Fork` 而未正确实现 `Call` 的自定义 transport，或依赖同 ID 不同类型实例被当成本机排除的代码必须调整。

直接调用导出的 `RPCClient.Call` 时，内置 NATS 和 gRPC client 现在会自行验证目标 ownership，并覆盖当前 hop 的 owner metadata；携带 session 的普通显式 backend target 会被标记为可信 `direct` mode，通过 `cluster.ResolveStatefulServer` 得到且携带 exact fence 的 request-scoped target 会使用 `assigned` mode。目标 ownership 已失效时会在发送前返回 `constants.ErrStaleOwnerEpoch`；有效的非零 epoch 会替换继承自上一 hop 的值，有效的 epoch `0` 会清除旧值以保留未启用 fencing 的兼容路径。自定义 transport 的 `Call` 实现也必须遵守同一契约；正常 `RemoteService` 路径会提供 typed route mode，自定义实现必须保留 context 并在编码前调用 `cluster.PrepareOutboundRPCContext`。绕过 `RemoteService` 直接构造 session RPC 的自定义 transport 不具备创建内部 authority marker 的能力，应改走框架路由入口。

自定义 `RPCClient.Call` 在编码 propagate metadata 前还必须调用 `cluster.PrepareOutboundRPCContext(ctx)`。该函数会删除业务 context 中伪造的 `rpc.reliable-job`、`rpc.reliable-metadata-sha256`、`rpc.idempotency-fanout` 和 `rpc.stateful-route-mode`，并且只从框架私有 typed marker 重建对应 wire 字段。框架持有的 RemoteService、cluster session 和 remote agent 调用点已经执行该步骤，内置 NATS/gRPC client 的 request builder 也会重复执行以覆盖直接调用；自定义实现若绕过该函数，framework marker 不会被正确序列化，业务注入值也不会被剥离。目标 transport 必须从同一份 `Request.Metadata` 构造传给 `PitayaServer.Call` 的 context；ReliableRPC marker/digest 或 stateful mode/fanout 不一致会 fail closed。wire marker 不是密码学认证，直接构造 NATS/gRPC 请求的集群 peer 仍属于受信任边界，生产环境必须依赖 broker ACL、网络隔离或传输认证限制该能力。

底层 `RPCClient.Publish` 和 `RPCClient.Fork` 均已弃用。只有本地 owner epoch 为有效的 `0` 时，NATS 实现才保留原有 type-wide publish 行为；owner fencing 活跃、epoch 非零或 ownership 已失效时，它们会在修改消息或发布前分别同步返回 `constants.ErrRPCPublishRequiresTargetResolution` 或 `constants.ErrRPCForkRequiresTargetResolution`。需要 fencing 的调用方必须改用 `App.Publish`/`RemoteService.Publish` 或 `App.Fork`/`RemoteService.Fork`，由框架解析每个具体目标并附带对应 token。

### 指标标签、Reporter 和 Logger reload

Prometheus reporter 对缺失、多余或未知 label、负 counter 和未注册 histogram 返回 error，不再通过 `Vec.With` panic。多个 reporter 的失败由 `errors.Join` 聚合；无返回值的框架 metrics helper 会记录 reporter error，遇到 nil reporter 也只记录错误而不 panic。直接调用 `Reporter` 的应用应传播其 error。

`AddMetricTagsToPropagateCtx` 会复制传入 map。跨 RPC metric tags 必须是 `map[string]string` 语义；解码到非字符串 value 会使 context decode 失败。框架保留的 `route`、`status`、`type` 和 `code` 最后写入，业务传播标签不能覆盖这些维度。框架不会替业务标签实施 cardinality allowlist；只能传播受控的低基数值，禁止把 UID、request ID、原始错误文本或其他无界值作为 label，否则 Prometheus/StatsD series 数仍可能失控。

Logger 热重载不再替换已发布的 `*zap.Logger`/`*zap.SugaredLogger` facade。保存过 `logger.Zap`、`logger.Sugar` 或 `logger.Log` 的代码会继续收到新 core、level、development、DPanic 和 stacktrace 行为，不需要重新读取全局指针。

### Stateful routing、owner fencing 与 RPC 幂等

需要 session affinity 的 stateful route 在一致性哈希失败时不再随机选择节点。缺少 UID/frontend session key 时返回 `constants.ErrStatefulRouteKeyNotFound`；服务发现无法给出 hash owner 时原样返回错误。依赖旧随机降级的调用方必须改为在请求前建立稳定 routing key 或显式处理不可用。

etcd 服务发现使用 server registration key 的 `CreateRevision` 作为 owner epoch。新进程不能在旧 registration lease 仍有效时用相同 type/ID 覆盖它；初始化会返回 identity-owned 错误。只有旧 lease 失效、key 删除后才能接管，并获得新的 epoch。选择 etcd discovery 后，共享 server 会在 discovery 构造完成时立即进入 invalid 状态；因此即使 RPC module 先初始化，registration 提交前的无 token 请求也会 fail-closed。keepalive 丢失同样会立即使本地 epoch 失效，恢复 lease 和 registration 前 RPC admission 保持 fail-closed。出站 RPC 在目标 epoch 非零时传播 `constants.OwnerServerIDKey`（`rpc.owner-server-id`）和 `constants.OwnerEpochKey`（`rpc.owner-epoch`）；接收端本地 epoch 非零时，缺失、非法、ID 不匹配或 epoch 不相等都会返回 `constants.ErrStaleOwnerEpoch`。`RemoteService` 路由调用、backend `Session.SendRequestToFrontend` 使用的 remote agent，以及 cluster session 的定向 `SendRequest` 都会按具体目标替换这两个字段；目标 epoch 为 `0` 时会清除上一 hop 的字段。自定义 discovery 传给 `Server.SetOwnerEpoch` 的 generation 必须小于 `2^63`；最高位由框架保留为 invalid 标记，越界值会 fail-closed 而不会截断为 unfenced epoch。自定义 RPC transport 必须完整保留 propagate context 到目标入口，否则启用 etcd fencing 的请求会被拒绝。owner metadata 只属于当前 hop，入口验证后会在业务 handler 前移除；handler 发起下游 RPC 时由新目标重新写入，epoch=0 的目标不会继承上一跳 fence。非 etcd/无法提供 epoch 的 discovery 保持 epoch `0`，不声明 fencing 保证。

etcd discovery 现在还为默认 consistent-hash 选出的 session-bearing、point-to-point backend RPC 建立持久的 stateful owner assignment。为保持既有节点选择，consistent-hash candidate 的输入仍是原始 UID，或无 UID 时的历史 `<frontendID>-<sessionID>` 文本；assignment/admission 使用另一套带域的 canonical identity：UID 为 `v1:uid:<byte-length>:<uid>`，无 UID 的 frontend session 为 `v1:session:<frontend-byte-length>:<frontendID>:<sessionID>`。因此 UID `frontend-1` 与 `(frontend, 1)` 即使历史 hash 文本相同也不能共享 token。最终 assignment key 是 `(server type, canonical identity)` 的结构化 SHA-256，不截断、不分 shard；不同 identity 不会因框架哈希截断而共享 owner token。assignment 的 etcd `ModRevision` 是单调 fencing epoch，record 同时绑定 owner registration 的 `CreateRevision`。owner registration 消失或被新 generation 替换后，下一次解析通过 CAS 激活新 owner；扩容和普通 lease renewal不会自动搬迁仍有效的 assignment。

`SessionStickiness` backend binding、自定义 route 和显式 server ID 保留原 target，使用 framework-owned `direct` mode，不会被 assignment 覆盖；fanout 使用独立的 `fanout` mode并保留每个 recipient。默认 consistent-hash 路由使用 `assigned` mode。三个值通过 hop-local `rpc.stateful-route-mode` 传输；`rpc.idempotency-fanout` 和 route mode 在出站边界会先删除，再仅从 `internal/rpccontext` 的进程内 typed marker 重建。应用通过公开 propagate-context API 写入同名 raw 字段不能改变出站模式。该边界仍信任已经通过 broker ACL、网络隔离或传输认证的 RPC peer；拥有底层 subject/transport 权限的 peer 仍可直接构造 wire metadata。

assignment 存在配置的 etcd namespace 下，relative prefix 为 `stateful-owners/v1/`。键按 canonical identity 首次访问创建，没有固定数量上界；record 不保存 UID 或原始 routing key，也不绑定短期 registration lease。框架不会自动刷新或 TTL-renew assignment，避免无业务交接时改变 `ModRevision`/epoch，但也不会自动删除退役 routing key。容量规划必须按 assignment 实际基数预留 key 和存储，并为稳定请求至少预留路由端一次 linearizable read、一次 validation transaction，以及目标初始 admission、`BeforeHandler` 边界和紧邻组件调用各一次 transaction；配置 `ReceiverProvider` 时每次调用还会增加一次，带 idempotency store 的请求在真正取得 claim 后也会额外增加一次。运行账号必须对 `<etcd-prefix>/stateful-owners/v1/` 具有 range/read 和 put/transaction 权限；按业务保留期清理退役 key 还需要 delete。删除 live assignment 会产生新 epoch并可能迁移 owner，只能在确认 routing key 已退役，或停止并 drain 对应业务后执行。高基数或高 QPS 部署必须先做真实 etcd capacity test，不能沿用旧的固定 65,536-key 估算。

这是滚动升级不兼容的 admission 变化。新版本 etcd backend 对携带 session 的 backend RPC 要求可信 `rpc.stateful-route-mode`：`assigned` 必须同时携带与本次 canonical UID（或 frontend ID + session ID）identity 精确匹配的 `rpc.stateful-owner-key`/`rpc.stateful-owner-epoch`，`direct` 和 `fanout` 不得携带 assignment token；不一致或缺失会返回 `constants.ErrStaleStatefulOwner`。registration owner token 仍在所有模式下独立校验。新 frontend 发给旧 backend 时旧进程不会执行新 admission，旧 frontend 发给新 backend 时请求会因缺失 mode/token 被拒绝，所以不能混跑。未带 `uid`/`session` 域的旧 assignment key 也不会被新 resolver 复用：升级前应先授权新 prefix、停止新增用户状态命令并 drain 在途请求，再协调升级所有使用同一 stateful server type 的路由端和 backend，最后恢复流量；确认旧 identity 已无入口和在途写后，才可按保留策略删除其孤立 key。自定义 RPC transport 必须调用 `cluster.PrepareOutboundRPCContext` 并传播 route mode、fanout、registration 和 assignment 字段；直接构造 session RPC 的代码应改走 `RemoteService`。

`cluster.StatefulOwnerResolver` 是 service discovery 的可选能力。它的 `ResolveStatefulOwner`/`ValidateStatefulOwner` identity 参数现在接收 `cluster.StatefulOwnerRoutingKey` 产生的 canonical identity，而不是 legacy consistent-hash 文本；resolver 产生的 fence 必须使用 `cluster.StatefulOwnerAssignmentKey(serverType, routingIdentity)` 定义的 exact identity。未实现该能力的 custom discovery 保持旧路由行为，也不获得 assignment、缺失-token admission 或跨 server ID fencing 保证。需要受控迁移时，可对实现了 `cluster.StatefulOwnerHandoff` 的 discovery 调用 `HandoffStatefulOwner`，以当前 assignment key/epoch 和 live successor 做 CAS。该调用只负责激活新 epoch，不会自动停止入口、drain 旧 keyed lane 或提交业务数据库版本；调用方必须先 quiesce/drain 旧 owner，并让状态存储以新 epoch 建立写屏障后再恢复流量。

handler 和自定义 `IdempotencyStore` 可通过 `service.RPCStatefulOwnerInfoFromContext(ctx)` 读取 `AssignmentKey`、`Epoch`、`ServerID` 和 `ServerType`。这是非传播的当前-hop typed value；原始 assignment、route-mode 和前一跳 typed admission info 在 store/handler 前移除，嵌套 RPC 会重新解析自己的 owner。带 idempotency key 的请求会在 store 真正取得执行权时再次校验 registration 与 exact assignment；framework-extensible `ReceiverProvider`、`BeforeHandler` 和 `TaskExecutor` 排队后的真实 interceptor/handler 调用前也分别执行紧邻 guard，关闭 wait/queue -> handoff -> callback/handler start 的窗口。`IdempotencyStore` 实现若收到 `service.IsRPCAdmissionRejected(err)==true`，必须释放 claim 且不得缓存该结果，包含 custom resolver 普通错误或 deadline 的最终 guard rejection；内置 Memory/Redis store 已遵守此合同。已经进入旧 callback/handler 的代码仍无法被框架强制终止，因此有外部副作用的业务事务必须按 assignment key 保存最高已接纳 epoch，并拒绝更低 epoch；仅依赖 admission 不能阻止旧代码的迟到写。该协议也不替代 command idempotency、sequence 或 exactly-once 事务。

Worker 现在用稳定的 Redis JID 写入 `constants.IdempotencyKey`（`rpc.idempotency-key`）。Worker 同时在进程内附加不可由外部应用构造的 typed marker；出站边界据此写入 `rpc.reliable-job=<JID>` 和 `rpc.reliable-metadata-sha256=<digest>`。digest 对入队时的原始 ReliableRPC routing metadata 做保留 JSON number 精度的规范化 SHA-256，并在调用 `ServerDiscovery` 前生成，因此 callback 对 metadata map 的修改不会改变命令身份。`ServerDiscovery` 继续通过普通 `json.Unmarshal` 接收历史 `float64` 数值，不会因内部摘要改成 `json.Number`。

`RemoteService.Call` 对相同 key 和请求指纹合并并发执行、缓存响应和 error；同 key 不同请求、不同 ReliableRPC routing metadata 或普通/Reliable 模式混用返回 `constants.ErrIdempotencyConflict`。请求已经到达 handler 后产生的 transient error 也会在 result TTL 内复用，不会靠 Worker retry 再次执行副作用。

只有通过上述 marker 验证的 ReliableRPC 指纹才忽略每次重试所在执行器的 `FrontendID`、`peer.id` 和 `peer.service`，因此同一 Redis job 被共享 namespace 中的另一 worker 实例接管时仍能 replay 已完成结果。普通 keyed RPC 会保留这三个调用方身份字段；相同 key 从另一 frontend/peer 到达时会冲突，而不会复用其他调用方的缓存响应。ReliableRPC 需要让业务来源参与冲突判定时，必须在入队 metadata 中传入显式、稳定的业务字段。

该指纹格式与旧版本不兼容：新版本把 ReliableRPC marker 和 routing-metadata digest 纳入指纹，普通 keyed RPC 也恢复 caller identity。滚动升级前应停止新增和重试所有携带 `rpc.idempotency-key` 的请求；对 ReliableRPC 还要排空 Worker 的 queued、scheduled/retry 和 in-progress job。确认不再产生旧格式完成记录后，至少等待 idempotency result TTL（默认 15 分钟）让最后一条旧记录过期，再统一启动新版本。无法执行该窗口的混合版本部署或残留旧记录必须接受同一 key 在 TTL 内返回 `constants.ErrIdempotencyConflict` 的 fail-closed 行为，不能通过换 key、换 prefix 或换 store version 无条件重放业务副作用。

Cluster Builder 默认启用新的 `RedisIdempotencyStore`，复用 `pitaya.storage.redis` 的连接、TLS 和 ACL 配置。默认 key prefix 为 `pitaya:rpc:idempotency:`，完成结果保留 15 分钟，running lease 为 30 秒，每 10 秒续租，等待者每 25 毫秒检查一次，每次 Redis 操作最多 3 秒。全部参与同一 RPC scope 的进程必须连接同一 Redis 并使用同一 prefix。生产 ACL 除 key 范围外必须允许 `EVAL`，以及脚本调用的 `EXISTS`、`HGET`、`HSET`、`HDEL`、`PTTL` 和 `PEXPIRE`。新增配置位于 `pitaya.cluster.rpc.idempotency.*`；所有 duration 必须为正，且 `heartbeatinterval + operationtimeout < leasettl`。

Redis claim 基础设施错误现在对 RPC peer 返回稳定的 `service.ErrIdempotencyStoreUnavailable`；commit 不确定和 lease 丢失分别返回既有的 `constants.ErrIdempotencyCommitUnknown` 与 `constants.ErrIdempotencyLeaseLost`。底层 Redis 地址、TLS、ACL 和连接错误只保留为内部 cause，不再进入 wire `Status.Message`。依赖匹配底层错误文本的调用方必须改用 `errors.Is`。

运行 owner 由随机 token 和 TTL 标识；续租、完成写入和旧 owner fencing 都通过单 key Lua CAS 完成。进程在 handler 完成前退出时，lease 到期后另一个进程会接管。续租失败或 lease 到期只会 cooperative cancel 旧 handler 的 context；无法强制终止忽略 cancellation 或正在执行不可取消 I/O 的 callback，因此新 owner 可能在旧 callback 仍运行时开始。旧 owner 的 completion CAS 不能覆盖新结果，但该 CAS 只 fence Redis 完成记录，不能撤销或阻止旧 callback 的业务写入。已完成结果可跨进程 replay，同 key 不同 fingerprint 仍 fail-closed。调用返回 `constants.ErrIdempotencyLeaseLost` 时，当前执行者已不能证明自己仍拥有 key；返回 `constants.ErrIdempotencyCommitUnknown` 时，业务函数已经返回，但完成脚本的结果未知；Redis 中出现不完整或未知 schema 时返回 `constants.ErrIdempotencyStoreCorrupt`。这些失败都返回明确的非成功 RPC status，不会把原成功 payload 交给调用方。调用方应使用同一个 key 查询/retry 或执行业务侧 reconciliation，不能把 commit-unknown 当作“业务一定未执行”。

该 Redis 状态机不能与任意业务数据库原子提交。进程在业务副作用完成后、Redis completion CAS 前退出时，lease 到期后的接管会再次执行，因此总体仍有明确的 at-least-once 窗口，不提供 exactly-once。需要事务级效果去重的 handler 可调用 `service.RPCIdempotencyInfoFromContext(ctx)` 取得 opaque `StoreKey` 和 `Fingerprint`，并在自己的状态事务内建立 unique/version 检查；该值只在当前本地 handler context 可见，不会自动传播到嵌套 RPC。每个嵌套业务命令仍须显式设置自己的稳定 idempotency key。

直接通过 `NewRemoteService*` 构造的服务仍默认使用 `MemoryIdempotencyStore`：它仅进程内有效，默认 TTL 15 分钟、最多 10,000 项；容量全部被在途 key 占用时返回 `constants.ErrIdempotencyStoreFull`。Cluster Builder 可把 `pitaya.cluster.rpc.idempotency.enabled` 设为 false 以使用该进程内 fallback，或设置非 nil `Builder.IdempotencyStore` 覆盖默认 Redis store；显式 store 始终优先于 enabled 开关。默认 store 会在 `Build` 时读取当前 `Builder.Config` 和 `Builder.Redis`，因此构造 Builder 后、Build 前对 enabled、prefix 或 duration 的修改会生效。其他构造路径使用 `NewRemoteServiceWithOptions(..., service.WithIdempotencyStore(store))` 注入自定义实现。自定义 store 同样不能把 Redis 风格的完成记录误称为业务 exactly-once。

idempotency key 必须是非空字符串且不超过 256 bytes；框架传给 store 的 key 是固定长度、带版本的 opaque hash，普通路由按 server type 作用域去重，fanout 则按目标 server ID 独立去重。自定义 store 不得解析或自行重组该 key。fanout 传输会新增内部 metadata `rpc.idempotency-fanout` 和 `rpc.stateful-route-mode=fanout`；自定义 RPC transport 必须通过 `cluster.PrepareOutboundRPCContext` 生成并完整保留到目标入口。

owner fencing、stateful assignment、route mode、idempotency key、fanout marker、ReliableRPC marker 和 metadata digest 都是当前 RPC hop 的 admission metadata：目标完成逐值验证和 store scope 计算后，会删除 `rpc.owner-server-id`、`rpc.owner-epoch`、`rpc.stateful-owner-key`、`rpc.stateful-owner-epoch`、`rpc.stateful-route-mode`、`rpc.idempotency-key`、`rpc.idempotency-fanout`、`rpc.reliable-job` 和 `rpc.reliable-metadata-sha256`，并消费进程内 typed marker。`IdempotencyStore.Execute` 和业务 handler 都收到这份清理后的 context；自定义 store 不能再从 propagate context 读取这些保留字段，应使用显式的 `key`、`fingerprint` 参数及 `RPCStatefulOwnerInfoFromContext`，deadline 和 cancellation 仍会保留。handler 发起嵌套 RPC 时不会自动继承父命令的去重、route mode 或 assignment 身份；每个需要去重的嵌套业务命令都必须显式附加独立、稳定的 idempotency key，stateful owner 和 mode 则由下游 target 重新解析。否则嵌套调用按普通无 key 请求执行。新的 fanout 会由框架重新添加 marker。无 idempotency key 却出现 ReliableRPC wire marker、marker/JID 不一致、摘要格式非法，或 request metadata 与目标 context 不一致时都会返回 `constants.ErrIdempotencyKeyInvalid`，不会进入 store 或 handler。对不携带这些保留字段的普通无 key RPC，执行语义保持不变；入口只增加保留字段预检。新增 exported Builder 字段也意味着外部 unkeyed `Builder` literal 必须改成 keyed literal。

### 群组分页广播 API

`Pitaya` interface 新增以下方法，自定义 facade、fake 和生成 mock 必须同步 method set：

```go
GroupMembersPage(context.Context, string, groups.MemberPageOptions) (groups.MemberPage, error)
GroupBroadcastWithOptions(context.Context, string, string, string, any, groups.BroadcastOptions) (groups.BroadcastResult, error)
```

`GroupMembersPage` 按 UID 字典序返回有界页面，cursor 是 opaque token，只能原样回传；默认 page size 256，允许 1..4096。内置 memory/etcd backend 原生分页，使用有界的 live keyset view，不保证一次多页遍历对应同一不可变快照：遍历期间删除成员不会造成重复，新增且排序位置不大于当前 cursor 的成员可能不会出现在本轮结果中。需要严格成员快照的业务应在应用层冻结名单或使用提供快照语义的自定义 backend。memory backend 现在在增删成员时维持字典序，分页只二分定位并复制当前页，因此直接调用 memory `GroupMembers` 的返回顺序也从历史插入顺序改为字典序。依赖旧顺序的调用方必须自行定义排序规则。

自定义 `PagedGroupService` 返回的成员必须非空、严格递增且不超过请求 limit；非末页必须同时返回非空且前进的 cursor，多页 cursor 循环会被拒绝为 `constants.ErrGroupPageCursor`。只实现旧 `groups.GroupService` 的自定义 backend 会在一次完整 `GroupMembers` 或 `GroupBroadcastWithOptions` 操作开始时全量读取、校验并排序一个不可变快照，后续页面复用该快照，不会逐页重复全量读取和排序，但仍不具备后端内存上界；大群组实现必须提供原生 `PagedGroupService`。快照构建会周期检查 context cancellation。

旧 `GroupBroadcast` 现在也通过分页和 UID 确定性分片执行。默认 page size 256、8 shards；`BroadcastOptions` 可设置 page size、1..64 shards、0..1,000,000 的 aggregate starts-per-second rate，以及用于恢复的 opaque `Cursor`。`BroadcastResult` 即使返回 error 也保留 `Attempted`、`Succeeded`、排序后的 `FailedUIDs` 和 `NextCursor`。用 `NextCursor` 作为下一次调用的 `Cursor` 可跳过已经完整完成的页面；页内取消会返回该页起点，因此恢复时最多重放一个页面，业务副作用仍必须幂等。

NATS server 的 `pitaya.cluster.rpc.server.nats.services` 默认值从 30 改为 1，并且现在只接受 1。该字段过去声称控制多个 message processor，但运行时始终只有一个 dispatcher；并行 dispatcher 还会破坏同 key 请求进入 keyed executor 前的顺序。显式配置其他值的部署必须删除覆盖或改为 1；RPC handler 的并行度继续由 keyed executor 和对应 pool 配置控制。

### 内部所有权和支持矩阵

session backend map/generation/mutex、remote fanout worker、etcd callback queue 分别收拢到单一 owner 对象。公共 facade 保持不变，但依赖未导出字段、反射或 callback queue 时序的测试不再受支持。

`Session.GetOnCloseCallbacks()`、`GetSubscriptions()` 和 `GetDataEncoded()` 现在返回防御性副本。修改 getter 返回的 slice 不再改变 Session 内部状态；应使用对应 setter 或受支持的 Session API。过去依赖共享 backing array 的代码虽然可能继续编译，但行为已经改变。

Linux/amd64 仍是唯一完整 runtime 和 release-verification 平台。Darwin/amd64 与 Windows/amd64 新增 `CGO_ENABLED=0` compile-only CI；这只证明 root、CLI 和 xk6 当前可交叉编译，不代表外部服务、signal、Docker 或生产运行支持。

### Prometheus exporter 生命周期

默认 Builder 不再通过全局 registry、`http.DefaultServeMux` 和后台 `ListenAndServe` 启动 Prometheus。collector 构造只写入当前 App 的私有 `prometheus.Registry`；exporter 使用自己的 mux，并通过 `prometheus.Gatherers{pitayaRegistry, prometheus.DefaultGatherer}` 保留 Go/process 以及应用注册到 default registry 的 collector。两边出现重复 FQName 时 scrape 会返回可诊断错误，不会静默选择其中一份。

`pitaya.metrics.prometheus.enabled=true` 现在表示启动必要能力。`NewBuilder`/`Build` 不占用端口；`App.Start` 在其他 module、Acceptor 和 `running=true` 之前同步 bind `pitaya.metrics.prometheus.port`。collector 配置或 bind 失败会使启动失败，不能再以“App 已运行但没有 metrics”的状态继续。`Serve` 在运行期意外退出会触发 dependency-failure shutdown；正常 `App.Shutdown` 先停止接受 scrape，再以有界 HTTP shutdown 释放 listener，重复关闭保持幂等。

启用默认 Prometheus 后，`Builder.MetricsReporters` 必须保留 Builder 创建的 Prometheus reporter；可继续追加其他 reporter，但删除该实例会在 dependency graph preflight 中失败，避免 exporter 与 App reporter manager 指向不同图。需要完全自管 metrics 的应用应关闭默认 Prometheus，并把自己的 reporter/exporter 作为显式 module 注册。

`CreatePrometheusReporter` 和 `metrics.GetPrometheusReporter` 仅作为 deprecated 兼容入口保留。它们仍在返回前完成同步 bind 并立即可抓取，但进程内所有调用共享同一个 reporter/exporter owner。调用方必须在全部共享引用停止上报后统一调用新增的 `ShutdownPrometheusReporter`（或 `metrics.ShutdownPrometheusReporter`）；该关闭是进程级、幂等操作，不能按单个 consumer 分别管理。默认 Builder 不使用这个 singleton。

### StatsD duration 与限流日志

Reporter 接口中两个内建 duration 指标 `response_time_ns` 和 `handler_delay_ns` 继续使用纳秒，Prometheus 的名称和值也不变。StatsD adapter 现在只对这两个精确常量除以 1,000,000 后调用 `TimeInMilliseconds`：200ms 的 wire 值从错误的 200,000,000 修正为 200。现有 StatsD dashboard、SLO 和告警阈值若按旧错误值配置，升级时必须除以 1,000,000。指标名暂不改动，避免同时产生新旧 series。

自定义 summary 的数值合同没有变化；即使名称以 `_ns` 结尾也不会自动换算。需要毫秒语义的自定义 reporter 仍应由调用方在自己的 adapter 边界明确处理，不能依赖名称后缀推断单位。

连接限流的预期拒绝不再以 ERROR 记录完整消息。新日志为 Debug，只包含 `message_length`、`limit`、`interval` 和固定的 rate-limit error；`exceeded_rate_limiting` counter 仍是告警和容量分析的主信号。依赖拒绝日志 payload 或 ERROR 数量的日志消费者必须迁移，原始业务、认证或用户数据不会再进入该日志路径。

## 验证工具迁移

### Redis integration

`make test-integration` 不再只运行 session Redis round trip。harness 会在同一个隔离、digest-pinned Redis 上分别编译并运行 session 与 service tagged test binary，精确校验两个 test inventory 和两个独立 completion proof。service suite 覆盖跨进程 RPC idempotency replay、heartbeat、stale owner CAS、live callback overlap、子进程崩溃接管，以及业务写入后 completion 前退出所暴露的 at-least-once 窗口。自定义 CI wrapper 不能只筛选旧的 `TestFlushFrontendDataRedisRoundTrip`；应调用 `make test-integration` 并保留 harness 的 sentinel、proof、timeout 和 owned cleanup 契约。

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

当前保守 baseline 是 `9178/11806 = 77.74%`，由同一 clean commit 的两份独立 green stamped profile 取逐包较低值生成。绝对门禁已经启用：overall 不得低于 70%；`cluster` 为 `2258/2742 = 82.35%`、`co` 为 `241/282 = 85.46%`、`service` 为 `1214/1469 = 82.64%`、`session` 为 `771/870 = 88.62%`，四个核心包均不得低于 80%。overall、逐包、package set 和文件清单不能回退；新增或修改的 executable production 行要求 90% diff coverage。刷新 baseline 只接受 clean checkout 上同一 commit 的独立 green stamped profile。

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
17. 为 etcd `stateful-owners/v1/` prefix 配置 read/put/transaction ACL 和退役 key 清理策略，按实际 routing-key 基数与每请求 transaction 放大做容量验证；停止并 drain 用户状态流量后协调升级同一 stateful server type 的全部路由端和 backend。恢复前验证缺失/伪造 mode、跨 routing-key token 和旧 assignment epoch 均 fail-closed，并让业务存储按 `RPCStatefulOwnerInfo` 拒绝低 epoch 写入。
18. 使用生产 Redis ACL 回归 Worker enqueue、fetch、确认、retry 和 scheduled job；确认允许 `EVAL` 及脚本内的 `ZSCORE`、`LPUSH`、`ZREM`，并验证关停时成功 job 不会在重启后重放。
19. 只有在 fork tag 发布，并且无 `replace` 的 consumer 能从公共 Go proxy 下载该 exact tag 后，才发布应用升级。
