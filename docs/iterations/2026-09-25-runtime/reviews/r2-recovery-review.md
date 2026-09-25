# R2 RecoveryAuthority 独立源码复审

状态：**固定快照 `b18e78bf7c0c231d487d7b0085fc419538806611` 通过本次 RecoveryAuthority 本地恢复切片复审。** Session 锁内的日志/动作关联校验、resolution checkpoint 证据关联、RecoveryAuthority 调用顺序、完整 pending 引用与 marker 决策匹配，以及 absent/present/resolved/unverifiable 的恢复语义均符合已批准的 R2 合同。本结论不代表 R2 全阶段或跨进程 crash gate 完成。

## 范围与证据

- 固定源码：`b18e78bf7c0c231d487d7b0085fc419538806611`，独立 detached worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-recovery-review`，审阅期间无源码改动。该快照基于 `deb13e9`；root 已将其集成到后续主线。
- 只审阅 `internal/session/durable_jsonl.go`、`internal/session/durable_jsonl_test.go`、`internal/session/durable_recovery.go`、`internal/session/durable_recovery_test.go`，并以主仓已批准的 `docs/iterations/2026-09-25-runtime/r2-contract.md` 为准。
- `rtk env GOMAXPROCS=2 go test -p 2 ./internal/session -list '^(TestPrepareResume.*|TestValidatePendingActionDecisionsRequiresExactOrderedResults|TestDurableWriterPersistsOpenToolCallWithoutPublishingPartialView)$'` 确认 20 个目标命中。
- 对同一选择器运行 `rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^(TestPrepareResume.*|TestValidatePendingActionDecisionsRequiresExactOrderedResults|TestDurableWriterPersistsOpenToolCallWithoutPublishingPartialView)$' ./internal/session`，通过，`ok ... 1.526s`。覆盖锁先于读取、锁等待取消、物理文件拒绝、authority 决策异常形状、未预留 occurrence、marker 恢复、resolution 验证与 authority 顺序、authority 错误 fail-closed，以及 open tool-call 写入后不发布半成品 view。

## 结论依据

- `PrepareResume` 先取得 session 文件排他锁，随后读/修复 JSONL、校验 durable entries/branch/view 和完整 action journal；只有本地验证成功后才进入 authority。若有 pending action 或 resolution checkpoint 却没有 authority，会在构造 `PreparedSession` 前失败。调用次序是 `ApplyDurableResolutions` 后 `InspectDispatchMarkers`，恢复 receipt 经过 `AppendAndSync` 后才重建并发布最终 view（`durable_jsonl.go:117-214`）。失败 defer 释放锁并关闭文件。
- action journal validator 从 assistant 的有序 tool-call 列表建立 occurrence，再用 `(SessionID, RunID, AssistantTranscriptOrdinal, ToolCallID)` 关联 intent、execution、receipt 与 checkpoint；拒绝重复 ordinal/index、重复 occurrence、跨 occurrence/错误 branch 链、批次混入、ActionID 重用、孤立/重复 receipt 和不匹配的完整 action 引用（`durable_recovery.go:38-124,126-228,230-282`）。append 路径也在写入前通过同一 `analyzeDurableActionJournal`（`durable_jsonl.go:423-425`），因此恢复校验没有另一套较弱的 append-only 规则。
- `PendingActionRef.Validate` 只接受完整 occurrence key，reservation 的 `ActionBatchID` 与 `ActionID` 必须同为空或同非空。authority 返回值必须逐项、原顺序回显完整 struct；长度、字段、重复 occurrence/ActionID、非法状态或未预留 occurrence 声称有 marker 均失败关闭（`durable_contract.go:84-101,151-189`）。
- 未预留 open call 只有在确认没有 intent 时才以双空 reservation ID 交给 authority；Absent 生成双空 ID 的 `interrupted/not_dispatched` receipt，Present/Resolved 对 reserved action 生成 `unknown/dispatched`，而 Unverifiable 不会形成可用 view。已有 receipt 必须与 marker 的 dispatch state 一致，恢复不会重写它；重复 resume 使用确定性 receipt ID 与 durable writer 幂等性避免重复收据（`durable_recovery.go:284-313,321-438`）。未发现任何工具/provider 执行或重放路径。
- resolution checkpoint 必须绑定精确 intent 与较早 receipt、位于正确 parent 链且每 action 唯一。NeverDispatched 引用匹配的 not-dispatched receipt；LocalResourcesStopped 引用更早且 action identity 完整匹配、具有合法 `ExecutionIdentity` 的 execution row；RemoteTerminal 只能引用 dispatched 的已知终态 receipt；OperatorReconciled 引用匹配的后续 reconciliation checkpoint（`durable_recovery.go:248-282,498-531`）。这验证的是 durable identity/evidence 的结构与链接，不把 PID shape 本身当作进程已退出证明，也不在此 session loader 中 signal 或探测进程。

## 阶段边界

接受的是本快照的 **BLD01 本地 RecoveryAuthority seam 与 session journal recovery**。全局 workspace-index adapter 的 marker 完整匹配、resolution append+Sync/idempotency、并发跨 session quarantine，以及真实父进程 SIGKILL 后子进程存活的 R2-REV-01 矩阵属于后续 BLD02/独立 restart gate；本次没有验证，也不应据此标记为完成。对本地 quiescence，本 loader 只验证 checkpoint 和 durable execution identity 之间的对应关系；运行时是否已正面证明资源停止由受信执行 owner/adapter 提供的 durable typed resolution 负责。
