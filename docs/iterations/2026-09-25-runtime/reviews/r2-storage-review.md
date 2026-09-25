# R2 BLD01 JSONL Storage 独立源码复审

状态：**固定快照 `48023ce` 未通过 storage slice 复审。** JSONL 锁、读修复、尺寸边界、一般 I/O 错误 poison 等基础符合合同；但当前 writer 拒绝 dispatch 前必须落盘的 open tool-call intent batch，动态 payload 可在写入开始后序列化失败而留下未 poison 的前缀，branch checkpoint 还可把不存在的 branch 发布为 active view。修复并在新固定快照复验前，不得将 BLD01 storage 标为通过。

本报告限于 BLD01 storage；不评估尚未接通的 action recovery authority、CLI/Loop durable commit 接线、workspace quarantine、真实进程恢复或完整 R2。三个 blocker 已在固定快照中通过一次性回归复现，临时测试已从隔离 worktree 删除，未修改产品代码或 execution ledger。

## 固定快照与验证

- 固定源码：`48023ce81e1588df7f9a2c073ec0f8fdd5bd2941`，独立 detached worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-storage-review-48023ce`；源码状态干净。
- 先以 `go test ./internal/session -list '<18 个精确测试名>'` 确认命中，再对同一 18 个测试运行 `GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/session -run '<exact names>'`：通过，`ok ... 1.500s`。测试覆盖 durable/legacy lock-before-load、锁等待取消、symlink 与非普通文件拒绝、valid no-LF 与 torn-tail repair、完整 malformed 行拒绝、exact 1 MiB 写/读边界、一般部分 I/O/sync 失败 poison 与按 ID 重开、legacy buffered Close 与幂等 Close。
- 五个一次性源码复现断言通过，分别记录了 R2STO-01/02/03、有效但语义无效的 no-LF 行以及 branch checkpoint 两种错误坐标；它们是“缺陷已复现”的测试，不是实现通过。临时 `review_tmp_test.go` 已删除。
- 未运行 session 全包、CLI 包、其他包或全仓库；不将未命中的测试名称计入验证结果。

## 阻断发现

### R2STO-01 — 二次序列化可在部分 batch 写入后失败，但 writer 未 poison

**级别：P1，阻断。** `AppendAndSync` 在准备阶段先为每条记录调用 `canonicalEntryPayload`（`internal/session/durable_jsonl.go:313-324`），随后 marshal 检查行大小（`:351-363`），开始写入后又在循环里 marshal 同一条记录（`:389-393`）。`SessionEntry.clone` 复用 protocol-value clone；它有意不克隆任意 struct/pointer（`internal/session/types.go:156-201`；`internal/core/agent_history.go:58-98`），而 `ToolCall.Args` 是 `any`。因此自定义 `json.Marshaler` 虽能满足当前 `json.Marshal` 接口，却仍是可变对象，writer 并未冻结最终 bytes。

临时复现将第一条 user row 放入同一 batch，再让 assistant tool-call 的 `Args.MarshalJSON` 在第三次调用时报错。第三次调用发生在 user row 已进入 `bufio.Writer` 之后；`AppendAndSync` 返回 serialization error，但该错误分支没有设置 `s.poisoned`（只在 Write/Flush/Sync 失败时设置，`:394-409`）。`Close` 对未 poison 的 writer 仍 Flush，但不 Sync（`:429-446`），复现中失败 batch 的前缀因此在 Close 时被写到文件，`Close` 仍返回 nil。不同调用输出不同 JSON 的 Marshaler 还可能使已计算的 `PayloadDigest` 与磁盘行内容不一致。

**最小修复：** 所有输入在第一次写之前完成一次规范化/冻结，预先生成每条最终 JSONL byte slice 并验证字节长度；后续写循环只写已冻结的 bytes，不再调用用户可实现的 Marshaler。digest 必须基于与落盘相同的规范化 payload。若不支持自定义 Go 值，应在写入开始前拒绝。任何实际写入已开始后的非预期序列化错误也必须 poison，但不能用 poison 替代全 batch 预序列化。

**回归验收：** `TestAppendAndSyncSerializesWholeBatchBeforeFirstWrite` 注入 stateful Marshaler，在第二次调用后返回不同内容或报错；断言错误发生时 file/write hook 仍为零调用、writer 未产生部分 batch；另对一次调用成功但内容稳定的合法嵌套 JSON 参数断言 retry digest 与重开文件一致、每个 entry 只按冻结 bytes 落盘。保留现有 partial Write/Flush/Sync poison 及 reopen-by-ID 测试。

### R2STO-02 — 预派发 open tool-call intent batch 被 ready-view closure 校验拒绝

**级别：P1，阻断。** writer 在写文件之前构造整条候选分支和 `SessionView`（`durable_jsonl.go:368-384`）；`SessionViewFromEntries` 强制调用 `validateProjectedToolGroups`（`internal/session/derive.go:29-42`）。该校验正确拒绝向 provider 投影未闭合 tool-call group：assistant 开出调用、journal-only intent 被跳过后，若尚无 receipt，最终 `len(open)>0` 即返回 error（`derive.go:213-270`）。但同一 writer 也将此“model-ready projection 必须完整闭合”的规则用于持久化批次预检。

临时复现先 sync user input，再对 `AppendAndSync(assistant(tool call), ActionStateIntent)` 提交合同规定的 dispatch 前批次；返回 `session: validate durable session view: ... 1 unreceipted tool calls`，没有 receipt 前无法继续。R2 合同要求完整 assistant tool-call 与 intents 在 executor 前 append+sync；receipt 则是在 executor 后单独 sync。因此当前顺序无法实现 durable-before-dispatch。只有错误地把 receipt 与 intent 一起预写，或等执行结束后再写全部行，才能绕过 closure 检查。

**最小修复：** 将 writer 的 durable append/chain 校验与“可供下一次 provider 请求的完整 transcript view”校验分开。允许将 assistant occurrence 与其 prepared intent/denial checkpoint 先 sync 为 pending 状态；该状态不能被误当成 ready provider history，也不能丢掉其 durable authority。receipt sync 后闭合 occurrence，再发布新的 ready `SessionView`；恢复路径在 action marker inspection/reconciliation 完成前仍不得返回 ready session。无需放宽最终投影校验或引入新 EventStore。

**回归验收：** `TestToolIntentDurableBeforeDispatch` 验证 session intent batch sync 成功后才进入执行器，且重开能看见完整 assistant+intent 与未闭合 occurrence；随后 receipt sync 后 view 恢复为完整 pair。分别覆盖 sequential 单 call batch、parallel 全组 reservation/denial batch、写入失败不 dispatch，以及重启恢复前 pending view 不会交给 provider。保留缺失/重复/错配 receipt 的 ready-view 拒绝测试。

### R2STO-03 — writer 与 loader 接受不存在或不匹配的 branch 坐标

**级别：P1，阻断。** writer 仅检查普通 `ParentID` 存在（`durable_jsonl.go:343-349`），然后无条件将每个新增 row 的 `BranchID` 作为 active branch（`:368-376`）。checkpoint 投影验证只检查 active-leaf 非空，branch checkpoint 检查 BranchID/BaseLeaf/ParentID/ActiveLeaf 自洽（`internal/session/tree.go:130-142`）；它不验证 `ParentBranchID` 是否已知、base leaf 是否属于该父分支、branch ID 是否冲突。loader 同样把每条记录 `BranchID` 赋给 active branch，active-leaf checkpoint 再照抄 checkpoint 的 `BranchID`（`durable_jsonl.go:597-608`），不核对目标 leaf 的所属分支。

两个临时复现均被当前 writer 接受：

- 以 `main` root 为 `BaseLeafEntryID` 的 `CheckpointBranch` 指向 `ParentBranchID="ghost"`，writer sync 成功，view active branch 变成 `feature`。
- `CheckpointActiveLeaf` 指向现存 main root，但其 `BranchID="ghost"`，writer sync 成功，view 暴露 `BranchID="ghost"` 搭配 main root 的 `SourceEntryIDs`。

这会把无法由 canonical branch checkpoint 证明的 branch 标识持久化为 active authority；坏坐标经重启仍会被 loader 接受。类似地，非 checkpoint row 可以直接以任意非空 `BranchID` 切换 active branch。`NewBranchCheckpoint` 检查 source leaf 存在，但接收的 `parentBranchID` 不做对照（`internal/session/branch.go:277-323`），不能替代持久层校验。

**最小修复：** 在 writer 与加载恢复使用同一 branch-state 校验器：从 canonical branch checkpoints 建立可重建的 branch-ID/source-leaf 映射；新 branch 必须引用已知 parent branch，base leaf 必须确属该父 branch，branch ID 不得被另一 checkpoint 重用；普通 transcript row 必须属于当前 active branch，只有合法 branch/active-leaf checkpoint 能切换。active-leaf checkpoint 的目标 leaf 与选择的 branch 必须匹配。未知 branch、冲突 checkpoint 或错配目标均在任何写入前拒绝，重开也失败关闭。

**回归验收：** 添加 unknown `ParentBranchID`、base leaf 与 source branch 冲突、重复 branch ID、普通 row 试图切换未知 branch、active-leaf pointer 目标/BranchID 不匹配的 writer 与 loader 测试；有效 fork 要覆盖 sync 后重开、shared-prefix source ID 不变、active branch 正确、父子关系可查询，以及主分支/新分支后续追加互不串线。与 `R2TREE-02` 的 `BranchMeta` 重建测试一起检查 session-only reload，不依赖 branch sidecar cache。

## 已通过或符合合同的 storage 行为

- **锁先于 load/repair。** 新 `PrepareResume` 先解析受限 physical ID、open no-follow 文件并取独占 flock，随后才 `readDurableJSONL`（`durable_jsonl.go:113-150`）；legacy adapter 同样先锁后读（`jsonl.go:475-490`）。已命中 lock-before-load 与 context cancellation 定向测试。新建文件采用随机 physical ID、`O_EXCL`，文件 sync 后 sync sessions directory（`durable_jsonl.go:75-108`）。
- **1 MiB 边界对称。** writer 上限为 `1 << 20` 且排除 LF（`durable_jsonl.go:20,351-359`）；reader 接受最多同样大的 JSON payload 加 delimiter（`:708-737`）。现有测试覆盖 `max−1/max/max+1` 的读取，以及 writer exact max、max+1 拒绝且不消耗序号；目标 race 测试通过。
- **torn-tail 修复保全证据。** 只有 `unexpected EOF` 类尾部语法错误才被识别为 truncated（`durable_jsonl.go:796-801`）；原始 suffix 先写 sidecar、sync sidecar、关闭并 sync sidecar 目录，再 truncate source、sync source（`:753-768,804-838`）。完整 malformed 行不修改源文件；现有目标测试通过。此实现没有 rename/replacement，故不需要针对 repair 再 sync source parent directory。
- **合法 JSON 但缺终止 LF 的处理符合合同。** `readDurableJSONL` 先完成单行 JSON/schema decode，再只追加 LF 并 sync（`durable_jsonl.go:732-751`）；完整 parent/sequence/digest/tree 校验在 `PrepareResume` 随后的 `validateLoadedDurableEntries` 执行（`:146-152,555-611`）。一次性复现验证：完整 JSON、缺 LF 但 parent 不存在的记录先被原样 delimiter，之后因 missing parent 被语义验证拒绝，除 LF 外 payload 未改。这个顺序符合“有效 JSON no-LF 保留并分隔；完整无效树仍 open fail”的合同，不能把该尾记录当 torn suffix 截掉。建议保留这条跨 reader+loader 回归。
- **Close 与普通 I/O 错误。** durable writer 在写、delimiter、flush 或 Sync 失败时 poison（`:394-409`）；poisoned close 不再 Flush，close 对已关闭 handle 再调用返回 nil（`:429-446`）。legacy `JSONLSessionStore.Close` 会 Flush+Sync 后释放锁且支持重复 close（`jsonl.go:402-436`）；partial write/Sync failure、legacy close/flush 的定向 race 测试通过。需要补 durable handle 重复 Close 的直接回归，但源码行为本身具幂等 guard。
- **no-follow 的实际边界清楚。** Unix opener 对 `sessions` 最终目录组件使用 `O_NOFOLLOW`，通过 `openat` 对物理文件名使用 `O_NOFOLLOW|O_NONBLOCK` 并 `fstat` 限制为 regular file（`session_open_unix.go:13-64`）；测试拒绝 symlinked sessions directory/file 与非 regular entry，并确认 symlink target 不被 repair。它不禁止配置 root 的祖先路径本身是 symlink；当前合同与测试保护的是 store 下 `sessions` 目录和物理文件，不是通用全路径沙箱。非 Darwin/Linux 的 lock/no-follow helper 失败关闭（`flock_other.go`、`session_open_other.go`）；本次只读源码，未 cross-compile 非目标平台。
- **legacy adapter 的边界。** `JSONLSessionStore.Append` 保持兼容的 buffered `SessionStore` 语义，实际 flush+sync 在 `Save`/`Close`；错误会 poison（`jsonl.go:271-312,365-436`）。它能复用 reader、flock、尺寸限制，但不是 `DurableSessionWriter`，因此不得用它的 `Append` nil 返回作为新 runtime 的 durable acknowledgment。当前报告不把旧 API 本身列为缺陷；R2 BLD02 必须证明生产 assembly 使用新的 `AppendAndSync` sink。

## 未纳入本 slice 通过判定的后续门槛

该快照在发现 action journal / execution-resolution 时仍会返回 recovery-authority adapter required（`durable_jsonl.go:154-158`）。按本次委托，未将尚未完成的 action recovery authority 当作 storage slice 新 blocker；它仍是整个 R2 变成可恢复 Runtime 的硬性后续门槛。还需在 BLD02/REV 验证：session lock 保持到 resolution replay 和 marker inspection 结束；未知 action 不自动重放；CLI 输入、intent、receipt、terminal 的 ACK 时序；真实进程重启与跨 session quarantine。

## 最终判定

固定 `48023ce` 的锁、尺寸、torn-tail 证据保存及一般 I/O poison 路径已有良好基础。R2STO-01 到 R2STO-03 直接阻断 durable-before-dispatch、batch no-ACK 的可靠性和 branch identity authority，应先修复并在新固定快照执行精确 race 回归；本结论不替代后续 storage integration 与真实 CLI restart 验收。

## Follow-up：固定快照 `c4a6534` 复审

**最新状态：仍未通过 storage slice，新增 blocker R2STO-04。** 本节是对上面历史快照结论的追加，不修改 `48023ce` 当时的 findings。固定源码为 `c4a6534f2b0bd87efc414cdef3be0b78efce4a03`，独立 worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-storage-review-c4a6534`，复核结束后已删除临时测试且 worktree 干净。

在同一固定快照先以 `go test ./internal/session -list` 确认 8 个精确测试均命中，再以 `GOMAXPROCS=2 go test -p 2 -race ./internal/session -count=1 -run '<8 exact names>'` 运行并通过。5 个正式回归为 `TestDurableWriterPersistsOpenToolCallWithoutPublishingPartialView`、`TestDurableWriterRejectsInvalidHistoryBeforeWriting`、`TestDurableWriterSerializesWholeBatchBeforeWriting`、`TestDurableWriterRejectsBranchCheckpointsWithUnknownMetadata`、`TestDurableWriterAcceptsSharedPrefixFromAncestorBranch`；另 3 个一次性对抗复现为 `TestReviewUnrelatedSystemRowCannotBeHiddenDuringOpenGroup`、`TestReviewReceiptMustMatchIntentActionID`、`TestReviewDuplicateActionIntentForOneOccurrence`。后 3 个测试以“当前错误行为被观察到”为断言，测试通过表示缺陷已复现，不是实现已正确。未重跑全 session race 或全仓库。

### 已关闭的旧 storage findings

- **R2STO-01 — 整批写入前冻结序列化结果：已关闭。** `durable_jsonl.go` 先拒绝非支持的 JSON 值（包括自定义 `json.Marshaler`），计算 digest 并将所有最终行序列化进 `serializedBatch`，之后才首次写入文件（`:300-324,399-416`）。因此没有第二次动态序列化在部分 batch 写盘后失败、却未 poison writer 的路径。`TestDurableWriterSerializesWholeBatchBeforeWriting` 证明拒绝后文件大小仍为 0。测试使用 `failAt=3`，但没有断言 Marshaler 调用次数；由于前置 JSON-value validator 会先拒绝它，这个名字对“第三次调用失败”的覆盖偏弱，建议后续断言自定义方法调用数为 0 或写 hook 调用数为 0。该测试强度改进不影响源码路径已消除部分写入风险的判断。
- **R2STO-02 — durable open tool-call intent：已关闭。** writer 允许 assistant-call 与 journal-only intent 先 append+sync，同时保留旧的 provider-ready `SessionView` 和 revision；receipt 到达后才发布含完整 assistant/tool pair 的新 view。正式回归 `TestDurableWriterPersistsOpenToolCallWithoutPublishingPartialView` 验证了这段 ACK 顺序。
- **R2STO-03 — branch journal authority：在 c4 writer/loader 路径已关闭。** `validateDurableBranchJournal` 现在同时用于 append 候选树和 durable load，checkpoint 引入 branch 前验证 parent branch、base leaf ancestry、唯一 BranchID、普通 entry branch lineage 和 active-leaf branch 一致性（`durable_branch.go:13-77`）。正式回归拒绝未知父 branch 和未知 active branch，并验证共享祖先前缀的合法 fork 可 append、关闭后重开并恢复正确 branch/view。ece43a8 又把同一 validator 接入通用 version-one tree builder；其独立结论记在 [r2-tree-projection-review.md](r2-tree-projection-review.md)。

### R2STO-04 — 未闭合 tool-call group 中可持久化并随后暴露 system entry

**级别：P1，阻断。** `validateProjectedToolGroups` 对 assistant tool-call group 未闭合期间只拒绝插入 `user` 或另一条 `assistant`，却让 V1 `system` entry 穿过（`internal/session/derive.go:215-265`）。这不是可留在文件尾部的 action journal intent：普通 system entry 会进入可见历史。writer 随后把 `errUnclosedToolCallGroup` 一律当作允许持久化但推迟更新 view 的状态（`internal/session/durable_jsonl.go:390-394`），所以它接受该 system row；当匹配 receipt 之后到达，完整投影不再报 unclosed-group，system row 便随 receipt 一起进入 ready `SessionView`。恢复/树投影若按同样的 validator 处理，也会接受 assistant → system → tool 的非法配对次序。

**触发及后果：** 临时对抗用例先持久化 user、assistant(tool call) 与 intent；随后以该 intent 为 ParentID append `EntryTypeSystem`，writer 返回成功且 view 暂不更新；最后 append matching tool receipt，writer 再次返回成功，ready view 的 source 顺序为 assistant、system、receipt。system message 在 call 与 receipt 之间改变了 provider history，破坏 canonical assistant/tool group 原子配对。复现源码 `review_tmp_test.go` 已清理；准确的路径条件与断言保留在本段和发给 builder 的修复要求中。

**最小修复与验收：** tool-call group 未闭合时，任何非该 occurrence 的 assistant-call/receipt 结构之外的 transcript-visible row（包括 system）都应返回普通结构错误，不能走“合法 open tail”延迟发布分支。仅在尾部允许 assistant group 与其 journal intent/execution rows 作为待 receipt 状态落盘。增加 append 回归：system/compaction/新的输入或其他可见 transcript row 穿插时，调用返回错误、文件字节不变、view/revision不变；增加 reopen/builder 回归：同样的完整记录必须失败关闭；合法 open tail intent 仍可 durable 保存而不暴露，完整 receipt 到达后再发布。当前 c4 快照未通过这一验收，已将精确触发和边界发给 BLD-01 builder。

### 延后到唯一 RecoveryAuthority journal validator 的跨行 action 校验

一次性对抗复现还确认 c4 当前可接受与该 occurrence intent `ActionID` 不同的 receipt，以及为同一 occurrence 写入两个不同 `ActionID` 的 intent。完整 R2 必须拒绝这两种记录，但依当前阶段合同，**不把它们计作 c4 storage slice 的新增 blocker，也不表示它们已通过**：合同规定 receipt 的 ActionID 必须对应同一 occurrence 的 durable intent（`r2-contract.md:30`），恢复先读取 assistant 与可能只有合法前缀的 intent rows，再按 `ActionBatchID` 和 occurrence key 关联并向 authority 检查（`r2-contract.md:188-190`）；BLD-01 loader 在缺少 authority 时对 action history 明确失败关闭（`durable_jsonl.go:154-158`）。按 root 的阶段裁定，跨行 ActionID 配对与 occurrence 重复由 BLD-01 `RecoveryAuthority` 统一 validator 负责，并在 append 接入时复用同一验证器；BLD-02 不另实现一套。此项仍是完整 R2 recovery gate。

**本快照判定：** R2STO-01/02/03 在 c4 修复；R2STO-04 仍阻断 storage slice。lock、torn-tail、1 MiB、一般 I/O poison 等原 48023ce 结论没有在本次重复全量复测。后续复验必须基于 builder 固定修复 hash；不应以 ece43a8 的 tree/projection 通过替代本 storage blocker。

## Follow-up：固定快照 `599045d` + `deb13e9` 复审

**最新判定：R2STO-04 已关闭；BLD01 storage + tree/projection 可作为 partial slice 接受。** `599045d74cf1fd4078f437c56664fe7d796c6e2b` 直接基于 `ece43a862b48b97a6453814a02a4a16a1d87af77`；`deb13e9aea494d351f5b0b129f77c60e644fb1ef` 基于 599。使用 detached worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-storage-review-deb13e9` 固定在 deb13e9；测试期间没有在 builder dirty worktree 上运行，结束后临时测试已删除、worktree 干净。

先用 `go test ./internal/session -list` 确认 7 个正式精准目标及 1 个临时 compaction 对抗目标均命中；随后运行 `GOMAXPROCS=2 go test -p 2 -race ./internal/session -count=1 -run '<8 exact names>'`，通过。正式目标为 `TestDurableWriterRejectsSystemRowInsideOpenToolCallGroup`（包含伪造 JSONL 后 resume 失败关闭子测试）、`TestDurableWriterRejectsProjectedUnclosedGroupInsteadOfDeferring`、`TestDurableWriterPersistsOpenToolCallWithoutPublishingPartialView`、`TestDurableWriterRejectsInvalidHistoryBeforeWriting`、`TestDurableWriterSerializesWholeBatchBeforeWriting`、`TestDurableWriterRejectsBranchCheckpointsWithUnknownMetadata`、`TestDurableWriterAcceptsSharedPrefixFromAncestorBranch`。临时目标 `TestReviewCompactionCannotCreateOrphanedReceipt` 也通过其“应拒绝且不写入”的断言后已移除。未重跑 session 全包 race；builder 的全包 race 结果单独由其报告提供，不混作本次独立测试证据。

**修复路径符合 ACK 与 projection 边界。** `SessionViewFromEntries` 现在先验证原始 branch journal，再执行 compaction/visibility 投影（`internal/session/derive.go:42-50,78-125`）。raw validator 只允许未闭合组中出现同一 `(RunID, AssistantTranscriptOrdinal, ToolCallID)` 与 ToolName 匹配的 V1 intent、execution 或 receipt；system、compaction、user/assistant、legacy、其他 action 或无关 receipt 都返回普通结构错误。只有 raw branch 本身是合法未闭合尾部时才返回 `errUnclosedToolCallGroup`，因此 writer 可以安全持久化 pending tail 但保持旧 view。随后若 raw group 已闭合、但 compaction/SurfaceOp 投影仍让 tool-call group 不完整，`SessionViewFromEntries` 将 projected unclosed 错误包装为非 sentinel（`derive.go:52-58`）；writer 的 `errors.Is(err, errUnclosedToolCallGroup)` 延迟分支只会接收 raw pending tail，不会把损坏的 model projection 当作合法 open tail。

正式测试证明：open assistant group 内插入 system 会在写前失败、磁盘字节不变，伪造磁盘记录也无法恢复成可用 view；合法 assistant+intent open tail 仍可 sync 而不更新 view，receipt 闭合后才更新 view。新增 hidden-receipt 回归证明 projected history 因 receipt 被隐藏而重新成为 unclosed group 时，append 失败且文件和旧 view 均不变。临时对抗还验证 raw assistant+receipt group 已闭合、但 compaction boundary 只覆盖 assistant 并把 receipt 留在投影后时，projected orphan receipt 以硬错误拒绝，未被 deferred-view 逻辑吞掉；若 compaction boundary 覆盖整个已闭合 group，则整组被 summary 替代是合法投影，不构成 orphan。

此前报告中的 **ActionID ↔ intent 配对、同 occurrence 重复 intent** 仍属 R2 完整 RecoveryAuthority gate，按 root 裁定由 BLD01 单一跨行 journal validator 负责，并在 append 接入时复用；当前 storage 仍对未经 authority 恢复的 action history fail closed。本次因此只批准 session JSONL/tree 的 partial substrate，不批准完整 R2 restart safety、action recovery、workspace quarantine 或实际 CLI durable ACK 闭环。组合 tree/projection 复审见 [r2-tree-projection-review.md](r2-tree-projection-review.md)。
