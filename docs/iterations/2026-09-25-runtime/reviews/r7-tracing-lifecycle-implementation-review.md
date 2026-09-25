# R7 tracing lifecycle implementation review

## 结论

**接受固定实现 `0e2fa53ff19fe690634045aaef4aff3c9d813cef` 的本切片。** 在复审范围内没有发现阻断问题：queue admission 与 shutdown 线性化，关闭只有一个 owner；drain、worker join 与底层 exporter shutdown 分成两个有界阶段；waiter 的 context 只限制 waiter；CLI 在结束 root span 后用独立 20 秒预算等待，并以安全分类输出失败。未把本结论扩展为 Assembly、REPL/serve producer 生命周期或外部 exporter 协议已闭环。

## 固定范围与证据

- 在独立 detached worktree 审查固定提交 `0e2fa53ff19fe690634045aaef4aff3c9d813cef`；产品源码未改。审查了获准设计 `r7-lifecycle-design.md`、此前的 `r7-tracing-lifecycle-design-review.md`，以及 `internal/tracing/async.go`、对应测试、exporter 接口和 `cmd/cli/main.go` / 测试。
- 固定实现前的设计阻断已落地：共享 drain deadline 从首次关闭开始；每个 export callback 最长 5 秒且不超过 drain 剩余时间；关闭前已经 active 的 callback 保留其既有 callback context；drain 最长 10 秒；worker 实际 join 后才启动单次、独立 10 秒 `inner.Shutdown`；CLI 最大等待预算为 20 秒。
- 针对测试：

  - `rtk go test -race -count=5 ./internal/tracing -run 'TestAsyncExporter|TestAsyncExporter|TestTracingFinalizer'`：通过，80 个测试实例。
  - `rtk go test -race -count=10 ./cmd/cli -run '^TestTracingFinalizer'`：通过，30 个测试实例。
  - 临时 adversarial probe `TestReviewLifecycleKeepsSingleOwnerAcrossUncooperativeCallbacks`：`rtk go test -race -count=20 ./internal/tracing -run '^TestReviewLifecycleKeepsSingleOwnerAcrossUncooperativeCallbacks$'` 通过。probe 同时阻塞一个不理会 context 的 `ExportSpan` 与 `Shutdown`：drain deadline 过后 report 保持 `OwnerFinished=false`、`Pending=1`，底层 shutdown 在 worker 真正返回前未调用；其后底层 shutdown 阻塞期间多个 waiter 仍只观察同一 owner，底层 shutdown 最终恰好调用一次。临时测试文件已删除，review worktree clean。

## 审查结果

| 不变量 | 源码 / 回归证据 | 结论 |
| --- | --- | --- |
| 提交与关闭 admission 同步，关闭后拒绝新 span | `async.go:155-173` 在同一 `mu` 下检查 `closing`、检查容量、入队和计数；`startCloseLocked` 在同一把锁内设置关闭状态和 deadline（`:260-267`）。`TestAsyncExporterRejectsAfterClose` 断言 typed error 与计数。 | 通过 |
| 只有一个 close owner，所有 waiter 读取同一结果 | 首个调用在锁内取得 `startOwner` 才启动 goroutine；之后调用不创建第二 owner（`async.go:311-339`）。已有 `TestAsyncExporterConcurrentShutdownCallsInnerOnce`；临时 gated probe 进一步覆盖底层 `Shutdown` 长时间不返回时重复 waiter 不会再次调用底层关闭。 | 通过 |
| drain 是一个共享预算，不为后续 callback 重置 | `startCloseLocked` 只创建一次 `drainDeadline`；每个新 callback deadline 取 callback budget 与 drain deadline 的较早者（`async.go:194-216,260-267`）。`TestAsyncExporterSharedDrainDeadlineDoesNotResetPerSpan` 验证后续 span 获得的是剩余时间，截止后队列尾部被记为 abandoned。 | 通过 |
| 已 active callback 不会因 shutdown 重建/延长原有 deadline | callback 的 context 在调用 sink 前只创建一次；shutdown 只限制尚未开始 callback 的 deadline，不重写在途 callback context（`async.go:202-220`）。共享 deadline 测试用先于 shutdown 开始的 callback 与调用时记录的 deadline 断言这一点。 | 通过 |
| 超时不伪造 callback 已停止，且不与 callback 并发关闭 sink | drain timer 到期只清空尚未派发队列，并等待 `workerDone` 后才调用 inner `Shutdown`（`async.go:269-285`）。不合作 callback 会使原 owner 持续 pending；临时 probe 验证 `Pending`、`OwnerFinished`、底层 shutdown 调用时机，释放 gate 后同一 owner 才完成。 | 通过 |
| waiter 取消不取消/替换 owner | `ShutdownWithReport` 的 ctx 只出现在 waiter 的 select；owner 的 export deadline 和 inner shutdown context 均独立创建（`async.go:215-216,282-285,311-339`）。`TestAsyncExporterWaiterTimeoutRetainsOwnerUntilCallbackReturns` 和 CLI canceled-context finalizer 测试覆盖 waiter pending 后再次观察同一完成结果。 | 通过 |
| worker join 后有单独的 bounded inner shutdown | inner shutdown 只在 `workerDone` 后调用，并使用独立 `context.WithTimeout(context.Background(), closeTimeout)`（`async.go:272-285`）。临时 probe 覆盖该阶段不合作时依然保留 owner、不并行重试，恢复后恰好一次结束。 | 通过 |
| 计数、错误传播和 stderr 分类与合同相符 | accepted span 由 delivered、sink failed、shutdown abandoned 或 pending 构成；`Pending` 由这四项计算（`async.go:31-51,341-354`）。首次 sink error 与 shutdown error 通过 `errors.Join` 只在 owner 完成时返回（`:287-299`）。CLI 只输出 `pending` / `failed` 与计数，不格式化 callback error（`cmd/cli/main.go:125-157`）。对应 accounting 与 canary 测试覆盖失败上报和不泄漏。 | 通过 |
| CLI root span 先结束，cleanup 使用不继承 signal cancellation 的预算 | `run` 先设置根 span 的退出属性/状态并 `End`（`main.go:99-109`），之后才 `Flush` 并调用 finalizer（`:110-117`）。finalizer 基于 `context.WithoutCancel(ctx)` 设置 20 秒预算，并在 pending/failed 时返回非零码且只输出稳定安全信息（`:125-157`）。`TestTracingFinalizerWaitsDespiteCanceledSignalContext`、deadline 与 callback error canary 测试通过 race 重复运行。 | 通过 |

## 测试夹具变化

`cmd/cli/main_test.go` 新增带 mutex 的 `synchronizedBuffer`，并只替换 stdout tracing 会由异步 worker 并发写入的测试缓冲区，修复测试本身的 race。`internal/tracing/exporters_extra_test.go` 将异步失败测试从“必须写 slog warning”改为“shutdown 返回 sink failure 且计数准确”；这和获准的 owner 报告合同一致，没有放宽成功/失败断言。shutdown error fixture 改为 sentinel，额外验证 `errors.Is` 保留错误链。

## 有界说明

`TraceExporter` callback 仍是 in-process Go 调用；callback 若 panic，当前代码不 recover，符合 Go 的常见 panic 传播语义。此次默认 CLI 只能构造内建 JSONL/stdout exporter，未知 exporter 在 `newTracing` fail closed，没有配置或插件路径把任意 exporter callback 注入该 async worker，因此我没有把新增 panic recovery 列为本切片 blocker。若未来开放自定义 exporter 注入，应先明确 panic 是否需要隔离为一次 sink failure；不要把 panic recover 当成强制停止不合作 callback 的替代品。

本次只证明 AsyncExporter 与 CLI 根 finalizer 这段。Assembly close-admission、停止所有 span producer、prompt/REPL/serve 错误闭环和正式默认装配故障注入仍按 R7 迭代计划作为后续 integration gate；本报告不宣称它们已验收。
