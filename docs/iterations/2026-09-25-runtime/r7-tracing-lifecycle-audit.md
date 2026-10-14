# R7 tracing 生命周期剩余审计与有界实施方案

状态：只读源码审计与建议切片；本提交不改产品源码、执行台账或受保护文件。审计基线为 main `a245a37c3cbcde6c49c246f16bfd69e245ad1524`。遵照当前任务边界，不重复审查已在 `5ec` 修复的 stdout preflight。

对照：[R7 生命周期设计](r7-lifecycle-design.md) 的 Trace identity、递送、关闭章节，以及 [execution-tasks.json](execution-tasks.json) 中 R7-BLD-02。台账当前将 BLD-02 的 `execution_scope` 限定为 telemetry 部分切片，明确排除了 tracer/exporter 生命周期与 CLI close/error wiring；下文是供 root 另行批准的 bounded follow-up 设计，不把这些源码工作描述为已授权或已验收。

## 已对齐与未闭合部分

根 invocation 已在 `cmd/cli/main.go:76-96` 创建 tracer，并把其 context 传入 `cli.Run`；`internal/cli/assemble.go:820-822` 的 `assembleTracing` 从 context 取回同一 tracer。因此 R7 设计中“root 与 assembly 各创建一个 tracer/exporter、trace ID 断裂”的旧观察在此基线已修复，不应再为此重做双 exporter wiring。以下剩余项来自当前源码，而非旧设计推断。

| Finding | 当前源码证据 | 影响 |
|---|---|---|
| **R7TRACE-01 — AsyncExporter 的 shutdown 不受 deadline 约束，且关闭后仍接受 span** | `internal/tracing/async.go:57-67` 无 open/closing 状态；queue 满时仅 `slog.Warn` 并返回 nil。`Shutdown` (`:119-125`) 只用 `sync.Once` 关闭 `done`，随后直接 `wg.Wait()`，ctx 不参与等待；每个并发/重复调用随后都会各自调用一次 `inner.Shutdown`。worker 的 `flush` (`:75-84`) 为每个 span 新建 `Background` 5 秒 ctx；忽略 ctx 的自定义 sink 可永久阻塞 worker。`spanCh` 从未关闭，因此 shutdown 后的 `ExportSpan` 仍可能返回 nil 并把 span 留在已退出 worker 的队列中。 | 一个卡住的 exporter 会卡住整个 CLI shutdown；调用方 deadline 不能返回；shutdown 后新 span 被报告为成功但无人消费；重复 close 可重复关闭底层 sink。现有 `TestAsyncExporterDoubleShutdownNoPanic` 只检查不 panic，未检查恰好关闭一次。 |
| **R7TRACE-02 — 丢弃与递送失败不可从 owner 查询** | `AsyncExporter.ExportSpan` 对满队列返回 nil；worker 对 inner `ExportSpan` 错误只写日志并清空 batch (`async.go:75-85`)；`AsyncExporter.Shutdown` 只返回 inner `Shutdown` 错误。`TraceExporter` 注释仍声称 at-least-once (`internal/tracing/exporter.go:17-26`)，与有界丢弃行为冲突。没有 `ShutdownReport`、delivery/drop/failure/abandoned 计数或可读 pending/closed 状态。 | 默认 `Span.End` 面对 AsyncExporter 只知道成功入队或日志警告；`cmd/cli` 的 finalizer 无法区分完成递送、队列丢弃、sink 写失败与仍在清理。R7-BLD-02 不能据此诚实报告 trace 递送成功。现有 `TestAsyncExporterDropsWhenFull` (`async_test.go:83-110`) 反而把“返回 nil、静默丢弃”固定为当前语义。 |
| **R7TRACE-03 — root finalizer 把 signal context 用作 shutdown context** | `cmd/cli/main.go:44` 用 `signal.NotifyContext` 创建命令 ctx；`:96` 等 `cli.Run` 返回后，`:109-118` 调 `tracer.Flush()` 再用同一 `ctx` 调 `exporter.Shutdown(ctx)`。SIGINT/SIGTERM 时这个 ctx 已取消。`Tracer.Flush` 在 `internal/tracing/span.go:114-116` 是空函数；真正排空只发生在 exporter shutdown。 | cooperative sink 收到已取消 ctx 后可能立即放弃 flush；non-cooperative sink 则会被 R7TRACE-01 的 `wg.Wait` 永久卡住。当前 root 虽在 exporter shutdown 返回 error 时把成功 exit 改成非零，但没有独立清理预算或 pending report。 |
| **R7TRACE-04 — assembly 关闭无 deadline、无错误返回，命令调用方无法传播失败** | `AgentAssembly` 只暴露 `Cleanup func()` (`internal/cli/assemble.go:497`)；`runCleanup` (`:681-685`) 逆序直接调用 `[]func()`，没有 once、ctx、错误聚合或 cleanup-pending 状态。`prompt.go:165` 与 `repl_session.go:135` 只 defer 该函数。`serve_cmd.go:256-267` 的 HTTP stop error 仅 slog warning，随后同步调用无界 `cleanup()`，最终仍返回 nil。`assembleExtensions` 将 `pm.Shutdown(context.Background())` 的错误丢弃 (`assemble.go:968-987`)。此外最后 append 的 `appendFinalCleanup` 会先执行，先关闭 EventBus/session/memory，再运行更早登记的 extension Shutdown；这不是按 producer→sink 依赖关闭。 | uncooperative extension/连接可卡住 prompt、REPL、serve；关闭失败不能使调用方返回非零；并发或重复 Cleanup 没有单一共享结果。source 声明的“已停 producer 后才关 sink”没有被当前 disposer 顺序证明。 |
| **R7TRACE-05 — 命令 span 在失败路径记为 OK** | `prompt.go:127-130` 的 defer 无条件给 `prompt.run` 设 OK；`:192-199` 在检查 `stream.Result()` 的 err 前就给 dispatch span 设 OK。`repl_session.go:612-613` 无条件把 turn span 设 OK，`Run` 的 defer (`:131-134`) 也无条件把 interactive span 设 OK。 | provider、tool、关闭路径失败时，即使 root span 或 loop 子 span 是 ERROR，用户关注的 prompt/turn 操作仍可显示成功。R7-BLD-02 的 error-span 验收需要覆盖这些实际命令 span，而不能只测 root usage error。 |
| **R7TRACE-06 — 缺少统一关闭 admission 的现有 R1 fence** | 当前 `internal/core/run_slot.go` / CLI source 中没有 `RunAdmissionCloser` 或 `CloseAdmission`；`RunStatusHandle` 和 R1 `runOwner` 存在，但没有 shutdown 前拒绝新 run 的共享 fence。 | assembly shutdown 不能先证明 `Harness.Submit` 与 `TurnRunner.RunTurn` 已停止接收新工作。此 seam 应由 R7-BLD-01 在共享 R1 guard 上实现，BLD-02 只消费它，不创建第二个 run supervisor。 |

这些缺口的范围限于实际生命周期：默认 JSONL/stdout 都由同一个 AsyncExporter 包装；JSONL/Stdout 实现自身也不消费 `ctx`（`internal/tracing/exporter.go:310-325,351-371`）。外部 OTLP/Kafka conformance 不在此 follow-up 中；它们继续按 R7 support table 处于 unavailable。

## 最小实施切片

**A. tracing queue/close owner，R7-BLD-02。** 只保留一层 bounded queue/delivery owner。对 enqueue 与 begin-close 使用同一 admission 状态同步：close 一旦线性化，后续 `ExportSpan` 返回稳定 `ErrExporterClosed`；满队列返回 `ErrQueueFull`。被接受的 spans 有确定的 FIFO drain/abandon accounting。inner export 的失败按稳定类别计数，不把 span/prompt/错误原文放入 diagnostics。`TraceExporter` 的 at-least-once 注释改为 best-effort。`Tracer.Flush()` 当前为兼容空方法，不应被当作递送证据；一次 invocation finalizer 的 shutdown/drain 是唯一默认 flush owner。

`Shutdown(ctx)` 只开始一次关闭；所有 caller 等同一个关闭 future/result。它在 ctx deadline 内停止接收并排空；若 inner exporter 忽略取消，调用方有界地拿到 `cleanup_pending`/quarantined report，但原 worker/owner 必须继续持有 inner sink 和完成句柄，不能并发关闭正在使用的 sink或把资源忘掉。in-flight call 真正返回后，原 owner 按剩余 deadline 标记无法递送的队列项、只关闭 inner 一次并发布最终结果。`ShutdownReport` 由最外层 invocation finalizer 一次性返回，至少区分 accepted、delivered、queue drop、sink failure、close 后拒绝、shutdown abandon、pending/closed 与 cleanup errors；只包含 sink 名和稳定类别/计数/trace-span ID，不包含 span payload。不要另建默认 stats polling 或 logger-only 计数路径。

**B. assembly 与调用方 close 传播，BLD-01 先、BLD-02 串行接。** R7-BLD-01 先完成 Assembly 资源 disposer/extension 生命周期及共享 R1 admission fence。之后 BLD-02 仅在 root 逐段批准时编辑 `assemble.go` 的 trace owner/close-error hunk；不与 BLD-01 并发覆盖同一文件。用一个 `AgentAssembly.Close(ctx) error`（或同等窄 close handle）替代默认生产调用中的裸 `Cleanup func()`：首次关闭关闭共享 admission、停止 producer 并用 R1 `RunClaim`/status/quiescence 等待已接纳 run，再逆依赖关闭扩展/连接与 sinks，最后返回聚合错误；并发/重复 Close 等同一个结果。caller deadline 只结束 caller 等待；R1 owner、未完成 callback/resource 与仍依赖的 trace sink 要由原 owner 保持可达，超时返回 `cleanup_pending`，不能释放成 quiescent。保留 `Cleanup` 只作旧调用的兼容 wrapper，不供 prompt、REPL、serve、root 正常关闭使用。

`prompt.go`、`repl_session.go`、`serve_cmd.go` 必须显式调用带 context 的 close 并把其 error join 到 command result。serve 同时 join `server.Stop` 与 assembly close errors；不再 warning 后返回 nil。root `main.run` 在 `cli.Run` 和其 assembly closes 完成后结束 root span，再使用从已取消 signal ctx 派生的**新 bounded cleanup ctx**关唯一 exporter owner；不要把已经取消的 command ctx 直接用于 shutdown。Shutdown report/error 转成 stderr 上安全的稳定 error 分类和非零 exit code。错误消息不可包含 raw span/status/prompt 或任意 sink 错误内容。清理 deadline 必须有界且明确；不要每个 exporter/assembly disposer 自己另开无界 `Background` 等待。

状态写入用按真实结果 finalize 的 defer：成功才 OK，返回 error 才 ERROR；status message 使用安全分类而非 raw error。至少修复 prompt `stream.Result` 的 OK-before-error 顺序与 REPL turn/run 的无条件 OK。

**责任边界与前置项。** `internal/tracing/async.go`、tracing exporter/runtime 的 shutdown-report 部分、`cmd/cli/main.go` 和 tracing tests 属 BLD-02。`assemble.go` 的 extension/skill acquire/dispose 与 `RunAdmissionCloser` 属 BLD-01；BLD-02 只在其稳定后串行改 tracing close-owner hunk。`prompt.go`/`repl_session.go` 与 R2/R6 会有会话/auth调用点重叠，先由 root 分配具体 hunk；不要覆盖它们的 session lease / auth cleanup。RunClaim、worker/quiescence owner 归 R1，只通过既有 handle/fence 协作。`execution-tasks.json` 当前 BLD-02 scope 只批准 telemetry partial slice，所以本设计本身不授权以上源码修改。

## TDD 与可观察验收

先以真实 gated fake exporter 做 RED/green；所有故意阻塞点都必须由 test cleanup 放行并等待原 owner 结束，避免用泄漏 goroutine 证明“bounded”。建议覆盖：

| Regression | 当前预期 RED / 完成条件 |
|---|---|
| `TestTraceShutdownDoesNotAcceptOrphanedSpan`、`TestAsyncExporterRejectsAfterShutdown` | 当前 close 后 enqueue 可返回 nil。并发竞争 submit 与 shutdown；线性化后的提交全部明确拒绝，shutdown 前被接受的每条要么 delivered、要么计入 abandoned。 |
| `TestAsyncExporterReportsQueueFull`、`TestTraceFailedBatchReported` | 当前 queue-full 与 inner export fail 均不可由 shutdown caller 查询。完整覆盖 queue overflow、inner error、stats/报告一次且不泄露 payload。 |
| `TestAsyncExporterShutdownCallsInnerOnce` | 两并发/重复 Shutdown 仍只调用 inner Shutdown 一次；所有调用观察同一结果。 |
| `TestUncooperativeExporterCloseRetainsSinkUntilProducerStops` | fake exporter 忽略 ctx 并阻塞。短 deadline 后 Shutdown 在预算内返回 cleanup-pending；尚未关闭/丢弃 sink；释放 gate 后原 owner drain/close exactly once，重复 Close 获得最终相同状态。 |
| `TestAssemblyCloseClosesSharedAdmissionAndRetainsR1Owner`、`TestAssemblyCloseJoinsSetupAndCleanupErrors` | 竞争 Harness/TurnRunner 新 claim 与 close；close fence 后没有新 claim；已有 R1 claim 由原 owner 收尾。注入资源清理错误并确认错误链/Report 可见，extension shutdown 完成前 trace sink 仍可用。 |
| `TestPromptAndREPLFailureSpansAreError` | 假 provider 返回 typed error；实际 prompt dispatch/run 和 REPL turn spans 都标 ERROR，不含 raw provider body。 |
| `TestDefaultCloseCallersPropagateErrors` | 实际 prompt、non-TTY REPL、serve 调用 close 失败分别返回/join error；serve stop failure 也不能成功返回。 |
| `TestCLITraceShutdownFailureExitsNonzero` | 子进程 helper 走 production `run`/CLI dispatch，仅注入失败 exporter factory；成功命令遇 exporter close/delivery failure 时 OS process exit 必须非零，stderr 有安全分类且无 error payload sentinel。另保留一条实际编译的 `cmd/cli` binary JSONL success/error smoke，证明生产 BuildTracingRuntime 与文件输出链仍接通。 |

固定源码后运行有针对性的 race：

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 ./internal/tracing ./internal/cli ./cmd/cli
rtk go build -o <temp>/go-cli ./cmd/cli
```

真实命令验证要读取 trace JSONL 和 exit code，不用只读 `SpanData`/mock assembly 代替：使用真实 `cmd/cli` invocation、隔离 trace/log 目录、可注入的 test-only exporter failure、prompt 的 loopback provider、serve stop fixture；分别验证成功、provider error、assembly close error、exporter delivery/shutdown error。要做 `SIGINT` 验收时，确认命令 ctx 已取消后仍能使用独立 cleanup budget；阻塞 sink deadline 前后分别观测非零 exit/pending report 和资源 owner 的真实完成。此文档阶段不运行这些测试，也不把当前状态报为通过。

## Gate 状态

`BuildTracingRuntime` 已严格限制默认 exporter 为 JSONL/stdout/none，assembly 已复用 root tracer；stdout 输出冲突 preflight 不在本轮审计。R7 async delivery、bounded close、错误报告、assembly/CLI close propagation、run-admission fence 及上述命令级证据仍待实现与独立复审。当前 `R7-BLD-02` execution scope 不能被本设计隐式扩大；需 root 为 tracing/CLI 生命周期 follow-up 批准独立源码切片，再按上述责任边界串行集成。R7-REV-01 与阶段 gate 仍未完成。
