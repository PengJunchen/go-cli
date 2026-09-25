# R2 PreparedSessionSnapshot → tree 独立复审

**结论：拒绝固定提交 `3ff2d0cab22dfdc052a6f2700c9986af7d02620e` 的 snapshot→tree slice。** 独立 detached review worktree 位于 `/Users/pengjunchen/Documents/github/go-cli-r2-snapshot-tree-review-3ff2d0c`，基于 `009f31c`；复审 delta 限于 `internal/session/tree_builder.go` 与 `internal/session/tree_snapshot_builder_test.go`。这只判定该 slice，不代表 CLI 接线或整个 R2 闭合。

## 阻断发现

### R2SNAPTREE-01 — legacy snapshot 被套用 V1 branch 与父序约束

**级别：P1，阻断本 slice 的 legacy compatibility 要求。** `validatePreparedSnapshotForTree` 只从 `main` 初始化 `branchParents`，并且只从 V1 `CheckpointBranch` 行填充它（`internal/session/tree_builder.go:170,225-227`）；随后不分记录版本就要求 `snapshot.ActiveBranchID` 存在于该表（`:240-241`）。一个合法的 V0 JSONL 文件只含 `BranchID="feature"` 的记录时，`PrepareResume` 成功，`Snapshot()` 给出的 `ActiveBranchID` 和 `PublishedView.BranchID` 都是 `feature`，但 `BuildFromSnapshot` 以 `snapshot active branch "feature" is not in durable branch metadata` 失败。

同一个无版本门控的预校验也要求每一条记录的 `ParentID` 必须引用更早的物理行（`:192-195`）。共享的旧 `buildFromEntries` 路径本身会拓扑排序 legacy 行，并跳过孤儿行；但 snapshot 调用在进入该路径前就失败。一次性对抗测试验证了 `BuildFromStore` 对 child-before-parent 与 orphan 两例都按旧行为成功，而 `BuildFromSnapshot` 分别以 `missing or forward parent "root"` 和 `missing or forward parent "missing"` 拒绝。

**修复方向：** 将分支 ancestry、物理父顺序等严格 V1 校验限制在 V1 记录语义；V0 snapshot 沿用原有的拓扑排序和坏/孤儿行容错，同时保留对 snapshot 身份、确切 active leaf 与 source 引用的校验。补真实 PreparedSession V0 自定义 BranchID round-trip，以及 V0 forward-parent/orphan 与 `BuildFromStore` 行为一致的回归。

## 已验证行为

- 旧 `SessionTreeBuilder` interface 未增加新方法；`BuildFromSnapshot` 是 `DefaultSessionTreeBuilder` 的具体方法。`BuildFromStore` 复用抽出的 `buildFromEntries`。
- 新增测试以 `DurableJSONLSessionPreparer` 创建准备句柄、经 `DurableWriter().AppendAndSync` 写入 terminal checkpoint 和 V1 branch/checkpoint，再取 snapshot 构树；也覆盖真实 open-journal tail 与落后的 `PublishedView` 分离、按 snapshot 精确恢复 legacy leaf、身份/branch/leaf/MaxSeq/source 错误拒绝及嵌套 payload 深拷贝。
- `BuildFromSnapshot` 不再读文件或创建 writer；它用传入 snapshot 构导航树，并保留 `PublishedView` 作为独立模型历史投影。

## 验证命令与结果

先列出新增目标测试，再跑 session race：

```text
rtk proxy go test ./internal/session -list '^TestTreeBuilderBuildFromSnapshot'
exit 0；命中 6 个 TestTreeBuilderBuildFromSnapshot* 测试。

rtk go test -race -count=1 ./internal/session
exit 0；Go test: 399 passed in 1 packages。
```

临时对抗测试 `TestReviewSnapshotPreservesLegacyBranchID` 从真实 legacy JSONL 调用 `PrepareResume` 和 `Snapshot`，确认准备阶段成功而 builder 拒绝：

```text
rtk proxy go test ./internal/session -run '^TestReviewSnapshotPreservesLegacyBranchID$' -count=1
exit 1；session: snapshot active branch "feature" is not in durable branch metadata。
```

临时对抗测试 `TestReviewSnapshotPreservesLegacyTreeBuilderTolerance` 比较 legacy `BuildFromStore` 与 snapshot 路径，确认 forward parent 与 orphan 两个行为差异：

```text
rtk proxy go test ./internal/session -run '^TestReviewSnapshotPreservesLegacyTreeBuilderTolerance$' -count=1
exit 1；forward-parent 子测试报 missing or forward parent "root"；orphan 子测试报 missing or forward parent "missing"。
```

临时测试文件已从 detached review worktree 删除，builder 实现未改；review worktree 最后状态为干净 detached HEAD `3ff2d0cab22dfdc052a6f2700c9986af7d02620e`。主仓报告之外的既有改动未触碰；没有 stage 或 commit 主仓文件。


## Follow-up 复审：固定提交 `c4c1352d15b6ca372c73d7598949e3807cbb8f63`

**本 follow-up delta：接受。** 复审从 `3ff2d0cab22dfdc052a6f2700c9986af7d02620e` 比较到 `c4c1352d15b6ca372c73d7598949e3807cbb8f63`，在独立干净 detached worktree `/Users/pengjunchen/Documents/github/go-cli-r2-snapshot-v0-review-c4c1352` 核验。上文拒绝结论保留为首轮固定快照的历史记录；本段记录的修复关闭了 `R2SNAPTREE-01`。

`validatePreparedSnapshotForTree` 先扫描整个 snapshot 识别是否含 V1 行；只有存在 V1 时才要求父行按物理顺序出现、序号递增、branch metadata/ancestry 成立（`internal/session/tree_builder.go:174-180,199-229,251-270`）。没有 V1 行的 V0 snapshot 不触发父序、序号顺序和 active-branch ancestry 这三组 gate，因此可进入原 legacy tree builder 的拓扑排序和孤儿行容错。physical session identity、`MaxSeq` 对 durable 最大序号的核对、确切 active leaf 存在性，以及 `PublishedView.SourceEntryIDs` 存在/可投影/位于 leaf 路径的校验仍保留（`:193-197,242-243,265-283`）。重复 Entry ID 的拒绝保持无条件；V1 mixed snapshot 的重复 ID 仍失败。

新增正式回归覆盖真实 `PrepareResume` 产生的 V0 `BranchID="feature"` snapshot（`tree_snapshot_builder_test.go:76-102`）；child-before-parent 与 orphan 用合成 `PreparedSessionSnapshot` fixture 对比 `BuildFromStore`，验证 snapshot tree 保持旧行为（`:104-135`）。物理 preparer 本身拒绝缺失/forward parent 行，所以该 fixture 专门覆盖 builder API 的 legacy 容错。V1 parent 顺序仍有正式测试；另用三项临时混合记录对抗测试确认 V0+V1 mixed snapshot 的重复 Entry ID、forward parent 和逆序 Seq 仍失败关闭。V1 branch mismatch、MaxSeq/source 等原有拒绝测试也随 session 全包 race 通过。

先列目标，再验证：

```text
rtk proxy go test ./internal/session -list '^TestTreeBuilderBuildFromSnapshot'
exit 0；列出 9 个 TestTreeBuilderBuildFromSnapshot* 测试。

rtk go test -race -count=1 ./internal/session -run '^(TestTreeBuilderBuildFromSnapshot|TestReviewV1Mixed)'
exit 0；Go test: 22 passed in 1 packages（含 3 个临时混合 V1 对抗测试）。

rtk go test -race -count=1 ./internal/session
exit 0；Go test: 402 passed in 1 packages（临时测试文件删除后的干净固定快照）。
```

三项临时测试文件已删除，独立 review worktree 最终 clean detached HEAD 为 `c4c1352d15b6ca372c73d7598949e3807cbb8f63`；没有在 review worktree 修改 builder 实现。本次没有把 builder lane 的 vet/lint 结果计作独立证据，也没有运行全仓 race。结论限于 snapshot→tree slice；CLI 接线与整体 R2 仍需各自验证。主仓仅追加本 follow-up 报告，未 stage 或 commit。
