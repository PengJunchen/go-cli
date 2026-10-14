# R2 core commit sink seam review

## 结论

批准 `98a4cafb04564e7201fe57b454bea379855aa87f` 的 core sink seam，范围仅限接口、payload/context API 和共享 run-owner 的 RunID 注入。未发现需要阻止该 seam 合入的问题。该结论不代表 R2 持久化完成：此快照没有生产代码读取 `RunCommitSinkFromContext` 或调用 sink 方法，session adapter、Loop commit 顺序、append/sync 错误传播、child durable consumer 和恢复闭环仍需后续实现与验收。

本次只把 `98a4caf` 作为被审改动；父提交 `36107ec` 的 core-owned execution identity 类型和 `1f44aa8` 的既有 fixture 修复是前置条件，不计为本次新增能力。

## 源码核对

- [run_commit.go](../../../../internal/core/run_commit.go:9) 将 RunID/sink 放在独立 context key；`WithRunCommitSink(ctx, nil)` 写入遮蔽值，查找只读最近一层值，因此不会回退到父 sink。接口正好覆盖合同中的 input、intent batch、execution、receipt、resolution、compaction、terminal 七个语义提交点。
- [run_commit.go](../../../../internal/core/run_commit.go:73) 的 payload 保留合同需要的坐标：输入/receipt 有独立 `TranscriptIndex`；assistant/tool-call 分组用 `AssistantTranscriptOrdinal`；ActionIntent 有 `SchemaDigest`、`ArgumentDigest`、原始 ToolCallID 和独立 ActionID。执行状态复用现有 `DispatchState`、`RunExecutionState`、`ExecutionIdentity`；结果复用现有 `RunOutcome`，没有另造重复状态枚举。
- ActionIntent 的 `ToolIdentity`/`EffectSet` 是运行期信任值并标为 `json:"-"`，注释要求 adapter 只落审计事实，不能从 DTO 恢复授权能力；无原始参数字段。Core 只依赖 `internal/tools` 来承载这些既有可信类型，没有引入 `internal/session` 反向依赖。
- [run_owner.go](../../../../internal/core/run_owner.go:249) 在共享 callback 边界把当前 `RunClaim.ID` 写入 worker-tracked context。Harness 和 TurnRunner 均通过同一个 `runOwner` 启动 callback，因此当前 claim 覆盖调用者遗留的 ancestor RunID，不需要再各自创建或注入第二个 owner。此 hunk 不改变取消、cleanup、claim release 或 backpressure 分支。
- 新增的 `TerminalCheckpointID`、`CompactionCursor` 和 typed projection 对齐 R2 的幂等终结与压缩来源边界；没有看到多余的 types/checkpoint 字段扩张。

## 验证

在固定 detached snapshot `98a4cafb04564e7201fe57b454bea379855aa87f` 执行。先用 `go test -list` 确认下列 7 项全部命中，再运行：

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=5 ./internal/core -run '^(TestRunCommitSinkContextLookupAndMasking|TestHarnessRunContextUsesOwnedRunID|TestTurnRunnerRunContextUsesOwnedRunID|TestRunOwnerCancellationKeepsClaimUntilWorkerStops|TestRunOwnerNormalCallbackCanExceedCleanupTimeout|TestTurnRunnerSteerDoesNotRecordWhenQueueFull|TestTurnRunnerFollowUpDoesNotRecordWhenQueueFull)$'
```

结果：`ok github.com/pengjunchen/go-cli/internal/core 1.526s`。新增三个测试分别覆盖 sink lookup/nil masking、Harness 当前 claim RunID、TurnRunner 当前 claim RunID；重复 run 也验证 RunID 不复用。另四项既有回归覆盖取消期间 claim 保留、正常 callback 超过 cleanup timeout、Steer/FollowUp 队列满时不误记输入。该定向结果与 builder 已完成的 full core/session race 相互补充；本次没有重跑全包。

## 后续集成门

`RunCommitSinkFromContext` 在本快照中仅由新测试调用，证明的是可发现/可遮蔽 seam，不是 durable commit 已工作。R2 Loop consumer 接线时仍需验证真实 Loop 读取该 sink、run-start Input 在首个 provider 请求前提交、完整 tool-call/intent batch 在 executor dispatch 前同步、receipt 在下一次 provider 请求前同步、terminal 持久化失败不 ACK，以及子任务显式 mask/绑定正确物理 session。不得把本报告当作这些集成门已通过的证据。
