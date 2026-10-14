# R2 BLD01 Tree / Projection 独立源码复审

## 2026-09-25：57d5f58 修复提交的独立复审

复审基于固定提交 `57d5f58ef7a9218289506b413bebd09ed9bd4428` 的干净 detached worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-tree-review-57d5f58`；没有在 builder 的 dirty `r2-session` worktree 上跑测试。先以 `go test ./internal/session -list '<5 个精确测试名>'` 确认命中，再运行 `GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/session -run '<exact names>'`，通过（`ok ... 1.066s`）。5 个目标为 `TestGetBranchReturnsDefensiveCopiesForSharedPrefix`、`TestTreeBuildContextReturnsDefensiveCopyForNestedArgs`、`TestSessionTreeCloneDoesNotAliasNestedArgs`、`TestTreeBuilderRestoresBranchMetadataFromDurableCheckpoint`、`TestTreeBuilderRestoresActiveLeafCheckpointPointer`。其中 branch metadata 测试用 `DurableJSONLSessionPreparer` 写入并 sync user entries 与 branch checkpoint，关闭文件，再用 `JSONLSessionStore.Open` 从真实 JSONL 重开并调用 `DefaultSessionTreeBuilder.BuildFromStore`，验证 active leaf、shared-prefix 顺序和 `BranchMetaFor/ListBranches`。

- **R2TREE-01：原公开读出口已修复。** `GetBranch` 在锁内用 `SessionEntry.clone` 返回深快照，`BuildContext` 也不再浅拷贝内部 entry；新测试改变嵌套参数、content block、ToolCallID 与 ActionID 后重读 source/fork 均保持 canonical 值。它们保留 Clone 的内部 shared prefix 指针语义，同时阻断普通调用方通过 accessor 写回 canonical history。
- **R2TREE-02：checkpoint metadata reload 已修复。** V1 builder 现在从 durable `CheckpointBranch` 重建 `BranchMeta`，实际 JSONL close/reopen 回归覆盖只从 session 文件恢复、不依赖 branch metadata sidecar 的路径。对原来“恢复到 checkpoint leaf 但 `BranchMetaFor/ListBranches` 为空”的具体缺陷，可标为已关闭。
- **R2TREE-01 residual — `MoveTo` 仍把内部嵌套 payload 交给外部 summarizer。** `DefaultSessionTree.MoveTo` 在 `internal/session/tree.go:194-200` 只对每个内部 entry 做浅 struct copy，随后把 `[]SessionEntry` 传给可注入的 `BranchSummary.Summarize`（`:231-232`）。一次性复现用合法 `SetBranchSummary` 实现修改 `entries[0].ToolCalls[0].Args`，随后从 `GetBranch` 读到 canonical args 已变；断言失败，临时复现测试已删除。因为该回调在锁外执行，修改也绕过 tree mutex。修复方法是在锁内对 `departEntries` 使用 `e.clone()`，并添加 mutation regression。该类外部回调可再次破坏 append-only/shared-prefix 不变量，所以 R2TREE-01 尚不能完全关闭；这不是 5 个通过测试覆盖的既有公开 accessor 行为。

因此 57d5f58 关闭了原报告的两条具体树问题，但 tree slice 仍有上述一个 P1 别名出口。这个残项与 R2 storage 的 branch identity 检查分开：unknown/mismatched `ParentBranchID`、active-leaf branch 不匹配及重复 branch checkpoint 仍记录在 [r2-storage-review.md](r2-storage-review.md) 的 R2STO-03；没有把 storage blocker 混成 57d5f58 的树测试失败。

状态：**固定快照 `6176be3` 未通过 BLD01 复审**。source-aware transcript projection、shared-prefix Clone、tool-call/action ID 保留、compaction source-boundary 处理和 V1 sequence/topology 基础整体方向正确；但两个可复现问题破坏 canonical immutable prefix 与 branch 重启恢复，修复并在新固定 hash 复验前不得把本 slice 标为通过。本报告不评估尚未完成的 durable JSONL/recovery-authority 接线。

## 固定快照与验证

- 复审 worktree：从 `4a8d138` 基线隔离，固定在 `6176be3ddb93276795cbb4a4e5f621d48ed8ecc1`；审查增量提交 `0528bf5`（source-aware projection/tree validation）与 `6176be3`（shared-prefix branch checkpoint）。未在 builder 的可变 worktree 上测试。
- 定向 `go test -list` 确认全部 19 个已选测试命中；对这些目标运行 `go test -race -count=1 ./internal/session -run '<exact names>'`，通过（1.032s；额外 sequence counter/range 两项 race 通过，1.026s）。目标包括 source ID/compaction、clone 身份与嵌套参数、checkpoint apply/active leaf、legacy V0 builder、V1 sequence/损坏树及不递增 sequence。
- 父任务另在同一固定快照报告 `GOMAXPROCS=2 go test -p 2 -race ./internal/session -count=1` 全包通过（1.301s）；本报告没有重复跑全包。
- 两个一次性复现测试分别证实下述缺陷：`TestReviewGetBranchExposesSharedEntryPayload`、`TestReviewBranchCheckpointRestoreOmitsBranchMetadata` 均通过其“缺陷确实出现”断言，临时测试文件已从隔离 worktree 删除；未修改产品源码或台账。

## 发现

### R2TREE-01 — GetBranch / BuildContext 可修改 shared canonical prefix

**级别：P1，阻断。** `DefaultSessionTree.GetBranch` 将 `walkBranchLocked` 返回的内部 `*SessionEntry` 直接交给调用者（`internal/session/tree.go:310-328,376-399`）。新 Clone 有意让 fork 路径共用这些 entry 指针；现有 clone 测试也用 `assert.Same` 明确证明此别名（`internal/session/clone_toolcall_test.go:74-77,107-112`）。因此调用者只要修改返回 entry 的 `ContentBlocks`、`ToolCalls.Args`、receipt 字段或任意嵌套 map/slice，就会同时改写原 branch 与 fork 的内存 canonical 记录。

另一个出口 `DefaultSessionTree.BuildContext` 在锁内把 map entry 浅拷贝进 `SessionContext.Messages`（`tree.go:331-355`）；结构体虽复制，嵌套 slices、usage pointer 和 tool-call JSON 参数仍共享同一底层值。由此返回 context 的消费者也能在锁外静默改写内部记录。该行为违背 `SessionEntry` append-only/immutable 注释，且并发写会绕过 tree mutex。树节点在 `Append` 时 clone 入参（`tree.go:85`）只能隔离入参，不能保护这些读出口。

**触发与后果：** Clone 后从 fork `GetBranch` 取 shared assistant entry，修改 `Args["nested"].(map)["value"]`；再次读取 source `GetBranch` 会看到同一修改。独立复现通过；源码亦说明 shared pointer 是当前 Clone 实现，而不是数据值偶合。历史 receipt/action payload 和压缩前的 canonical 内容可能在没有 append/Sync 的情况下被变更。

**最小修复与回归：** 保留树内部对同一 durable `EntryID` 的共享，不复制/rekey 持久化 prefix；公开 `GetBranch`、tree `BuildContext` 以及其他可变 `SessionEntry` 投影改为深快照，确保 ContextManager 返回值与内部树隔离。把 `assert.Same` 改为断言源/克隆 branch 的相同 entry ID、ToolCallID、ActionID、内容与独立未来 append。测试分别修改 `GetBranch` 与 `BuildContext` 返回的嵌套参数/ContentBlocks/Usage/receipt 后，重读 source 与 clone 均不变，并在 `-race` 下并发读快照/写快照。

### R2TREE-02 — V1 branch checkpoint 重启后没有重建 BranchMeta

**级别：P1，阻断。** 同进程 `ApplyBranchCheckpoint` 在成功 append 后写入 `t.branches[checkpoint.BranchID]`（`internal/session/branch.go:357-375`）；但 `DefaultSessionTreeBuilder.buildVersionOneTree` 只将恢复条目泛化地送入 `tree.Append`，之后只恢复 `leafID`，从未按 `CheckpointBranch` 重建 branch metadata（`internal/session/tree_builder.go:176-208`）。`tree.Append` 也不注册 BranchMeta。故 durable checkpoint 可正确选择 active leaf，却不能恢复对应 branch 的名字、父关系和 base leaf 元数据；`BranchMetaFor`/`ListBranches` 返回空，基于它们的 branch list/switch journey 在重启后丢失。单独的旧 `BranchStore` cache 偶然能遮住症状，但 R2 合同规定它可重建且不是 branch authority。

**触发与后果：** 通过 `NewBranchCheckpoint` 构造 V1 feature checkpoint，模拟持久化后仅从 session entry store 调 `BuildFromStore`。独立复现确认 `CurrentLeaf()` 恢复到 checkpoint，但 `BranchMetaFor("feature")` 不存在。该复现通过其“重建缺失”的断言，临时文件已删除。

**最小修复与回归：** V1 builder 在校验并 append checkpoint 时从 `CheckpointBranch` 恢复 `BranchMeta`（branch ID、parent branch/source、created time、base leaf），拒绝冲突 branch ID 或不一致 checkpoint；BranchStore 只补充可重建缓存字段，不决定是否存在 branch。新增只含 session JSONL/EntryLister、没有 BranchStore 文件的 reload 测试：clone prefix entry IDs 与 ToolCallID 不变、checkpoint 为 active leaf、`BranchMetaFor` 与 `ListBranches` 可找到 fork，且再次 append 后分叉正确。

## 已确认通过的部分

- `NewBranchCheckpoint` 追加单个 checkpoint，让 fork 共享原 durable prefix，不复制/rekey `EntryID`、provider `ToolCallID`、receipt `ToolCallID` 或 `ActionID`；`ApplyBranchCheckpoint` 构造阶段不切换 leaf，应用 canonical checkpoint 后才激活。目标测试确认 clone prefix IDs/值一致、fork 后续 append 分叉。
- V1 source projection 将 `Usage` 与 source `EntryID` 一起保留；压缩 marker 作为独立的 untrusted summary provider message 投影，清空其 `InputKind`、tool calls、receipt 身份；按 `CompactedThroughEntryID` 保留 boundary 后的 suffix，即使其物理 append 在 marker 之前（`derive.go:26-59,135-188,267-307`）。`TestRestoreIncludesCompactionSummary` 与 `TestRestoreProjectionUsesSourceBoundaryAfterLeafSuffix` 覆盖这些语义。
- V1 tree builder 对重复 entry/sequence、未知版本、缺失 parent、错误 parent/child sequence、环、不可映射 compaction boundary 失败关闭；分支排序及当前 leaf 选择以 Seq 为准，不以倒退或重复 timestamp 为准（`tree_builder.go:119-208,211-255`）。`TestTreeBuilderSelectsVersionOneLeafBySequenceNotTimestamp`、corrupt-parent/sequence 测试和 append sequence-counter 测试通过。
- 全 V0 的 legacy tolerant builder 路径仍可重建现有有效记录；`TestTreeBuilder_WithEntries`、`TestTreeBuilder_MemoryStore` 与既有 V0 branch/context 用例通过。V1 tree 恢复 active-leaf checkpoint 的独立测试通过（`TestTreeBuilderRestoresActiveLeafCheckpointPointer`）。这不替代后续实际 JSONL 旧文件与 V0→V1 追加迁移验收。

## 后续测试门槛

- **R2TREE-GATE-01 — tool-call group closure 的专门覆盖缺失。** `validateProjectedToolGroups` 源码覆盖未配对、重复 occurrence、孤立/重复 receipt 和组间 user/assistant 次序，但新增测试没有直接命中这些拒绝路径（`rg` 未找到对应 error assertion）。在 storage 恢复通过 gate 前补精确测试：同一未闭合 assistant group 内重复 ToolCallID、缺 receipt、重复或孤立 receipt、ToolName 冲突、receipt 乱序/跨 RunID、后续 user/assistant 提前出现都应 fail；允许不同闭合 group/run 复用 provider `ToolCallID`。
- **R2TREE-GATE-02 — session revision、JSONL reader、锁与 durable ack 仍未包含在本 slice。** 这里只复审给定 tree/projection commit。完整 resume、lock-before-load、部分写入修复、RecoveryAuthority、无 tool/provider replay、真实 CLI new/resume 必须按后续 fixed storage hash 另行验证。

## 最终判定

在 `6176be3`，shared-prefix ID 保持、compaction source boundary 和 sequence/tree 基本校验符合合同；但公共读出口能突变 canonical prefix，且 V1 checkpoint branch metadata 不随 session tree 重建。两项都是行为缺陷而非测试文案问题，当前快照不能获 BLD01 tree/projection 通过。待 builder 提交最小修复后，应在新的固定快照复验这两条回归，再将结果附于本报告或单独增补明确的复审 section。

## Follow-up：固定快照 `ece43a8` tree closure 复审

**tree/projection delta：通过。** 固定提交 `ece43a862b48b97a6453814a02a4a16a1d87af77` 的 parent 正是 storage 修复快照 `c4a6534f2b0bd87efc414cdef3be0b78efce4a03`；独立 worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-tree-review-ece43a8` 为干净 detached snapshot。先用 `go test ./internal/session -list` 确认以下 7 个精确测试命中，再用 `GOMAXPROCS=2 go test -p 2 -race ./internal/session -count=1 -run '<7 exact names>'` 复验并通过：`TestTree_MoveToBranchSummaryCannotMutateStoredEntries`、`TestContextManagerBuildContextOutputsDoNotAliasNestedArguments`、`TestDeriveMessagesReturnsDefensiveCopies`、`TestTreeBuilderRestoresActiveLeafCheckpointPointer`、`TestTreeBuilderRejectsBranchCheckpointOutsideParentBranch`、`TestTreeBuilderRejectsActiveLeafFromUnrelatedBranch`、`TestTreeBuilderRestoresBranchMetadataFromDurableCheckpoint`。没有重复全 session race。

- **R2TREE-01 residual — `MoveTo` 外部 summarizer 可突变内部 canonical entries：已关闭。** `DefaultSessionTree.MoveTo` 现在在持有 tree mutex 时用 `e.clone()` 建立深快照，再于解锁后调用外部 `BranchSummary.Summarize`（`internal/session/tree.go:192-203,219-232`）。新回归让 summarizer 修改嵌套 tool args、provider ToolCallID 和 content block，随后从 tree 重读原分支，canonical 值不变（`branch_summary_test.go:130-160`）。
- **ContextManager 输出别名：已关闭。** `BuildContext` 分别克隆逆序 `Traversed` 与投影输入 `branchVals`，因而返回的 `SessionContext.Messages` 和 `Traversed` 既不指回 tree 内部，也不共享彼此的嵌套参数、content blocks（`context.go:73-85`）。新测试分别改写 Messages 与 Traversed 后确认另一个投影保持原值（`context_test.go:425-448`）。
- **`DeriveMessages` 输入别名：已关闭。** 函数现在对投影结果逐 entry 深克隆再返回（`derive.go:20-29`）；测试覆盖嵌套 map/slice、content block 和 Usage（`derive_test.go:80-101`）。
- **V1 branch-journal restore validation：已接入并通过定向验证。** `buildVersionOneTree` 在 append 至树、恢复 metadata 前对排序后的完整记录调用 `validateDurableBranchJournal`（`tree_builder.go:176-189`），拒绝 branch checkpoint base leaf 脱离声明 parent branch、active-leaf 指向不相关 branch；有效 JSONL checkpoint round-trip 仍重建 branch metadata。测试夹具把此前无 checkpoint 的 `left-branch` 坐标修正为 `main`，与 validator 所要求的 durable branch authority 一致。

ece43a8 关闭了本报告先前记录的 tree/projection 残项，tree delta 可批准。这个判断只覆盖 tree/projection：该提交以 c4 为 parent，因此也包含 R2STO-04 的 system-row穿插缺陷，不能据此把 JSONL storage 或整个 BLD-01 标为通过；该项仍跟踪在 [r2-storage-review.md](r2-storage-review.md)。完整 RecoveryAuthority 跨行 ActionID 配对仍是后续 R2 gate。

## Combined follow-up：`599045d` + `deb13e9`

后续 storage 修复提交 `599045d` 和 `deb13e9` 位于 ece43a8 之后。R2STO-04 的 system-row interleave 与 compaction/visibility 后 orphaned 或 unclosed tool group 现在在原始 journal 和 provider-ready projection 两侧分别失败关闭；其固定快照、精确 race 与 compaction 对抗证据记录在 [r2-storage-review.md](r2-storage-review.md)。因此截至 `deb13e9`，已复核的 BLD01 storage + tree/projection substrate 可作为 **partial slice** 接受。完整 RecoveryAuthority 对 ActionID/intent/receipt 跨行配对、恢复决策与 resource quarantine 仍未在该 partial 判定内通过。
