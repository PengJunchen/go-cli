# R5 keyed transcript projection design review

**结论：需两项窄修订后进入 DTO/adapter builder。** 这是静态设计复核；没有改产品代码，也没有运行测试。R5 默认 CLI/TUI lifecycle 和 R8 viewport 集成仍须等待各自接线与验收。

## 复核快照

- 当前主仓源码：`575fc5f746f702256a9b5d46150b955951a71066`
- R5 设计 worktree：`74cf8de7f406947cd9b619f90f7f94985bb862bf`
- `r5-keyed-transcript-projection-design.md` SHA256：`4d5dc9975de9b5fe9614db98f95d10b5fc943ea3c567b77c6133e9812a0079ea`
- `r5-transcript-implementation-handoff.md` SHA256：`46d0aa3a5cd73087e642755f657eb516db020657ef7de7f87815f8a588149b33`
- 已审 R5/R8 contract review SHA256：`ad23a7fc9247f65c475b78eda2e08d5324a5699cf9c47ff5d7adae023ea45c97`
- 已审 R8 viewport review SHA256：`0259b3b331bceb2c19128b17389f040b771493cd39f1d636c8f42d01072bbc40`

设计文档标注的源码基线仍是 `ebb73e11...`，handoff 标注 `a44b19de...`；本次按任务要求对照当前主仓 `575fc5f` 核实。实现前应刷新过时的源码行号与基线，避免 builder 将历史锚点当成当前事实。

## 必须修正

### R5KP-01：checkpoint 可见性不能单独证明 saved-success

设计 §3 的 `RunObservation` 把 terminal row 描述为 synced checkpoint；§“Provisional overlay and termination”第 157 条在发现该 row 时 retire overlay，第 158 条虽要求成功 outcome 和 quiescent，却没有要求 `EventGapCount == 0`。同一文件第 160 条要求保留 gap warning，但没有明确禁止带 gap 的 run 显示 “complete · saved”。

当前源码将 durable row 与 owner acknowledgement 分开：`RunStatus` 暴露 outcome、execution state、pending workers 和 gap count；run owner 在要求 terminal sync 但未成功时会将结果设为 unknown；consumer 仅在 `CommitTerminal` 成功返回后标记 terminal-synced。见 `internal/core/run_status.go:26-43`、`internal/core/run_owner.go:830-855`、`internal/core/run_commit_consumer.go:285-296`。sink 的 terminal 写入及同句柄刷新在 `internal/session/run_commit.go:771-779,1379-1426`。因此同一快照中看到精确 `run_terminal` 行只能证明该快照观察到了 row，不能覆盖 owner 的 Error/Unknown，也不能证明之后的 index/terminal acknowledgement 成功。prepared handle 遇到 sync/write 故障后可变为 poisoned，后续 `Snapshot()` 会报错；接口契约见 `internal/session/durable_contract.go:90-99`，现有失败用例见 `internal/session/run_commit_test.go:863-900`。

请在设计中分开“durable row 已观察到”和“run 已确认 saved”两项事实。`ProjectRunObservation` 只传递 exact handle 的状态、gap 和同一快照中的 terminal row，不返回自行推断的 `Saved`/ack 字段。显示 saved-success 必须同时满足：同一 run ID、owner outcome success、execution quiescent、`EventGapCount == 0`、selected path 上该 run 的 terminal outcome 为 success；并明确当前 core owner 的 success 状态只有在 terminal sink acknowledgement 成功后才成立。任何 `cleanup_pending`、quarantined、error、cancelled、unknown、非零 gap 或 terminal/index sync failure 都不能显示 saved-success。若 checkpoint row 可见但 owner 不确定，应允许更新可验证的 canonical projection，但继续显示 unsaved/unknown 状态；不要让 row 本身抹掉该失败事实。

增加两种验收：真实 prepared handle 的 terminal sync/ack 失败后，即使 row 可见也不能 saved；同一 handle 已 poisoned、`Snapshot()` 失败时不得用旧快照或旧 canonical 状态冒充成功。另加 `EventGapCount > 0` 且快照含成功 terminal row 的拒绝 saved 用例。现有 terminal sync failure session 测试可作为故障夹具基础；R5 测试还需验证真实 owner status 与 UI/adapter 的组合结果。

### R5KP-02：验证 canonical message 与 exact source entry 的类型一致

设计的 `CanonicalTranscriptSnapshot.Validate()` 只列出 key 唯一性、enum 和 compaction-kind 检查，投影第 3、5 条则把 `PublishedView.Messages[i]` 与 `SourceEntryIDs[i]` 配对后从 source row 选 `Kind`。这没有明确拒绝一个结构合法但 role 与 source 类型矛盾的 snapshot。

当前 `SessionView.Validate()` 只校验 session/branch、消息与 source ID 数量、revision 和 source ID 唯一性（`internal/session/durable_contract.go:28-52`）。`BuildFromSnapshot` 会确认 source 存在、model-visible 且位于 active leaf path（`internal/session/tree_builder.go:272-282`），但不会比较该位置的 canonical message 与 source entry 的投影是否一致。因此，手工构造或损坏的 snapshot 可以带有效 source ID，却把 user source 配成 assistant message；调用树验证并不能拦住这种错配。

投影时要对每一对 exact source/message 校验 `EntryType → TranscriptRowKind` 和允许的 `AgentMessage.Role` 一致，再检查建立身份所需的类型字段。注意 compaction 的 durable kind 是 compaction，而其 provider-history role 合法地为 user；tool receipt 至少需与 source 的 tool-call ID/name/outcome 对应。DTO `Validate()` 也应拒绝不允许的 row-kind/role 组合。任何一处不匹配都返回错误并保持当前 projection 原样，不做部分 replacement。

增加 malformed snapshot 回归：有效 source ID 对应 user row、view message 却为 assistant；加入 compaction 保持 typed row 但 user role 合法，以及 tool receipt identity 不匹配的用例。不能仅依赖 `PublishedView.Validate()` 或 `BuildFromSnapshot()` 通过作为 source/message 一致性的证明。

## 已确认可沿用的设计

- durable key 使用物理 session、selected view branch 与 `PublishedView.SourceEntryIDs[i]` 的真实 EntryID；共享前缀的 source row 所属 branch 不替代 selected branch。用 `BuildFromSnapshot` active-leaf parent path 校验 source/journal/checkpoint membership 是正确的。
- 工具 occurrence 只按完整 `(RunID, AssistantTranscriptOrdinal, ToolCallID)` 关联，journal fact 保留实际 EntryID/ActionID；不从 ToolCallID、文字、时间、位置或 event 顺序猜测映射。
- compaction durable row 与 provider history 中的 user-role summary 分开；journal-only rows 不合成 transcript 消息，open tool group 继续使用上一个完整 `PublishedView`。
- provisional event generation 仅整体绑定 exact run，不给单事件补造 durable identity；在同一 `EventStream` 上检查 status source，并在 run return 时复核，可覆盖 bridge 先于 owner handle 安装而启动的竞态。
- DTO 防御性深拷贝、whole-snapshot replacement、同一 retained prepared handle 刷新，及把 remote ACP 排除在本地绑定保证之外，边界清楚。

两项修订完成后，纯 DTO/adapter checkpoint 可以先于 Bubble Tea、REPL lifecycle 和 R8 viewport 接线实现。builder 的测试结论不得扩展成 R2 CLI hydration 或默认 PTY journey 已完成。
