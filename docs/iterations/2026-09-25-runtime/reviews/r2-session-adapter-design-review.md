# R2 Session RunCommitSink Adapter 设计复审

**当前结论：修订后的设计通过；实现尚未验收。** 初次评审对象为文档提交 `4bb628f737a08a8bf6a31764c8b5a733fb7e1e3e`，当时结论为需修订。现已按固定修订 `22a94594d96b44f1d9e15040d24e6bc41a067cdf`、源码基线 `7ed603e` 复审；本报告末尾的复审结论取代初审的设计阻断。两轮均为源码可实施性审查，不修改产品代码、不运行测试，批准不代表实现验收。

总体接线可落地：一个 run-scoped sink 复用已准备的 `PreparedSession`、同一 handle 的 `DurableWriter` 和 `WorkspaceQuarantineIndex`，session `AppendAndSync` 成功后才碰全局 index；派发标记成功后才进入同一次 prepared invocation 的最终派发栅栏；resolution checkpoint 成功后才将其真实 entry ID/Seq 交给 index。`PreparedSessionSnapshot` 提供当前 branch/leaf、所有 durable rows 与有来源 ID 的 `PublishedView`；`AppendAndSync` 返回规范化行，能取得 writer 分配的 ID/Seq；workspace index API 也能按 handoff 的调用次序工作。R2 contract 冻结的 error-only `RunCommitSink` 不需要为了这些链路扩展接口。

以下六项记录初审时的阻断及其当时理由；逐项复审结果见本文末尾。

## R2-ADP-01 — `EvidenceRef` 必须来自已同步事实的真实 EntryID

**严重度：阻断。证据：接口与校验源码直接确认。** Handoff 第 35 行要求 resolution 引用精确的 receipt、execution 或 reconciliation proof row；但 `RecordExecution`、`RecordReceipt` 和 `ResolveExecution` 都只返回 `error`（`internal/core/run_commit.go:61-69`）。session writer 的 `AppendAndSync` 已返回带 writer 分配 ID/Seq 的规范化行（`internal/session/durable_contract.go:55-62`），而恢复校验要求 `EvidenceRef` 非空且能匹配精确 proof 行（`internal/session/durable_recovery.go:506-525`）。文档目前没有说明核心如何取得这个真实 ID。

**最小修订：** 保持已批准的 sink API 不变。session adapter 在每次成功 `AppendAndSync` 后用返回行建立 run-scoped、可由 reopen snapshot 重建的 proof 索引。索引按完整 action occurrence 和 row kind/state 绑定，至少包含 `SessionID/WorkspaceID/RunID/AssistantTranscriptOrdinal/ToolCallID/ActionBatchID/ActionID/OwnerGeneration`；不能只按 `ActionID`、`ToolCallID` 或正文查找。`ResolveExecution` 缺少 `EvidenceRef` 时，adapter 只能从唯一且类型匹配的已同步 proof 行回填实际 `EntryID`；非空传入值必须与它完全相同。缺失、重复、歧义或冲突都须在写 checkpoint 前 fail closed。Operator reconciliation 引用必须指向先前独立持久化的 reconciliation checkpoint，不得自引用。Resolution checkpoint 自身 append+sync 后，使用 `AppendAndSync` 返回的 checkpoint `EntryID/Seq` 调用 `ApplyDurableResolutions`。

**验收：** 覆盖四种证据类别的真实 row-ID 回填；reopen 后从 `DurableEntries` 重建相同索引；伪造、跨 occurrence、缺失及冲突 ref 在任何 checkpoint/index 写入前拒绝；确认 index 只收到刚同步 checkpoint 的 ID/Seq。

## R2-ADP-02 — 并行混合拒绝与执行结果必须按 canonical TranscriptIndex 物理落盘

**严重度：阻断。证据：handoff 与当前 projection 实现直接冲突。** Handoff 第 32 行允许 `BeginIntentBatch` 将所有 intent 和已知拒绝 receipt 一起 append。并行组中拒绝调用可能位于尚未完成的较早调用之后，因此 denial receipt 会先于较小 `TranscriptIndex` 的实际执行 receipt 落盘。Session projection 沿 root-to-leaf branch 顺序保留 entry 顺序：`SessionViewFromEntries` 遍历传入 branch entries（`internal/session/derive.go:35-71`），`projectSessionEntries` 依原 slice 顺序筛选（`:131-168,221-242`），最后按该顺序生成 `Messages` 与 `SourceEntryIDs`（`:58-66`）；它不会按 `TranscriptIndex` 排序。于是可恢复的模型历史顺序与 R1 canonical transcript 不同。

**最小修订：** `BeginIntentBatch` 在并行路径只同步 assistant 和所有已批准 intent，不抢先写会跳过较早 transcript index 的 denial receipt。等整组结果齐备后，包括 pre-dispatch denial 在内的 receipts 全部按 `TranscriptIndex` 递增顺序通过 `RecordReceipt` 提交，再开始下一 provider request。顺序执行可在实际执行到该调用时提交其 denial receipt。Sink 维护当前 RunID 的下一个 transcript index，只接受连续 canonical 顺序；assistant 的 cursor 分配还必须保持同一 `(RunID, AssistantTranscriptOrdinal)` 的 sequential 重试幂等，不能用文本或 provider call ID 推断位置。

**验收：** 并行组混合一个或多个 denial 与成功/错误结果；让 worker 完成顺序与 tool-call 顺序相反；验证 receipts 的物理 append 顺序及重启后的 `SessionView.Messages` 都严格等于 canonical `TranscriptIndex` 顺序。另验证相同 `ToolCallID` 在不同 assistant ordinal 下不串线。

## R2-ADP-03 — Resolution checkpoint 只能在当前没有 open tool-call group 时 append

**严重度：阻断。证据：恢复 validator 直接确认。** Handoff 第 19 行要求 sink 接受 cleanup 中较晚到达的 `ResolveExecution`，并用 mutex 串行写入，但串行化本身不保证合法落盘位置。当前 `validateToolCallJournalTail` 在未闭合 assistant tool-call group 中只允许该组匹配的 intent/execution 行与 receipt；其他行一律报错（`internal/session/derive.go:74-125`）。若完成信号早于 Loop 写入 receipt，或较晚 resolution 落在下一组已经打开之后，`Type=checkpoint` 会令 reopen 失败。Completion proof readiness 与 transcript receipt 完成是并发的两个事实。

**最小修订：** Resolution 仅能在当前 durable branch 没有 open tool-call group 的 canonical safe point 写入，通常是本组所有 receipts 已 append+sync 后、下一个 assistant tool-call row append 前。清理 goroutine只登记 proof-ready，不在 dispatch worker/receipt 生产链中等待 receipts，也不提前写 checkpoint。Loop/consumer 在不依赖 executor 的 safe point drain ready proofs；若此时已有下一组 open，则延后到该组闭合后再 drain。不要让 adapter 阻塞 worker 等待由该 worker 后续产生的 receipt，也不要绕开当前 validator。

**验收：** completion signal 先于 receipt、receipt 与 proof 并发、proof 晚于本组闭合、proof 晚于下一组打开等场景均不得死锁；resolution 只在合法 safe point append；重新打开 session 能完成全 journal/view 校验，index 未收到 checkpoint sync 前保持 quarantine。

## R2-ADP-04 — `CommitTerminal` 只接受物理顺序正确的连续 transcript 前缀

**严重度：阻断。证据：当前 projection 以 branch append 顺序为准。** Handoff 第 37 行说比较已有 `(RunID, TranscriptIndex)` 行并“只 append 缺失行”，但未限制已有行必须构成正确前缀。若存在索引洞或较大索引已在较小索引前 append，补写较小索引会在 JSONL/branch 中形成非 canonical 物理顺序；当前 session loader 检查重复 index 与递增 Seq，但不负责将 transcript 行按 index 重排（`internal/session/durable_recovery.go:58-73`；`internal/session/derive.go:171-198`）。之后 projection 会照 branch 顺序暴露它们。

**最小修订：** `CommitTerminal` 必须验证当前 run 的 transcript 行按物理 `Seq` 构成 `TranscriptIndex=0..k` 的精确连续前缀，且各行 payload 与最终 canonical transcript 对应位置完全相同。只可追加缺失连续 suffix `k+1..n`，再在同一 `AppendAndSync` batch 中 append terminal checkpoint。任何洞、越序、重复或 payload 冲突都拒绝并 withholding ACK；不得把散落缺行按新 Seq 补在后面。

**验收：** 完整前缀加 suffix 可提交；缺索引、后序存在而前序缺失、物理顺序错、相同 index 不同 payload 均拒绝且不 ACK；模拟 terminal batch 部分可见后 reopen，以相同 checkpoint ID/payload 重试保持幂等。

## R2-ADP-05 — 本地 quiescence 必须由 typed CompletionProof 验证

**严重度：阻断。证据：已批准 proof 合同与当前恢复校验直接不一致。** Handoff 第 78 行已说明旧 schema/validator 尚未接入 completion proof，且不能以 `ExecutionIdentity` 当停止证据；但适配器落地要求不仅是能写出 proof 字段，还要求 loader 从历史 JSONL 重新验证它。当前 `ActionExecution` 只有 dispatch/resource state 与可选 `ExecutionIdentity`（`internal/core/run_commit.go:114-127`）；恢复分支把 identity 存在当作 `ResolutionLocalResourcesStopped` 的必要且充分执行行检查（`internal/session/durable_recovery.go:514-517`）。这会把“有进程身份但没有 joined/completion 证明”的旧或伪造 resolution 通过恢复验证并交给 index 清 quarantine。

**最小修订：** 将 `CompletionProof` 同时加入 core execution payload 与 durable execution row；恢复 validator 必须匹配同一 intent/action occurrence、execution row 顺序和认可的 closed proof enum。`ExecutionIdentity` 保留为可选诊断字段，只验证形状，不能替代 proof。旧 identity-only checkpoint 必须 fail closed，adapter 不能仅靠写入新合法字段来声称历史安全。

**验收：** 有合法 identity、无 completion proof 的 execution row 配 local-resolution checkpoint，reopen 必须拒绝且不得调用 `ApplyDurableResolutions`；认可的 `inline_pipeline_joined` proof 无 identity 时可通过；未知/空 proof 拒绝；每种 proof 都绑定正确 occurrence，并证明执行 proof 位于 intent 后、resolution 前。

## R2-ADP-06 — 通用成功 receipt 不能作为远端终止证明

**严重度：阻断。证据：quiescence contract 与恢复 validator 不一致。** R2 contract 明确要求 `ResolutionRemoteTerminal` 有协议 adapter 认可的 authoritative terminal signal，并说明普通 success receipt 不能清除 marker（`docs/iterations/2026-09-25-runtime/r2-contract.md:68,87,91`）。但 handoff 第 35 行只称“trusted terminal receipt”，而当前 `RunToolReceipt` 只有通用 message、`DispatchState` 和 `ReceiptOutcome`（`internal/core/run_commit.go:129-140`）；session entry 无 protocol-terminal typed fact。更严重的是当前恢复校验仅要求引用同一 dispatched receipt 且 `ReceiptOutcome != unknown`（`internal/session/durable_recovery.go:519-521`），通用 `success` 因而会被误接受为 remote terminal。

**最小修订与范围：** 本 adapter handoff 不新增 remote schema。它必须注明当前 `ResolutionRemoteTerminal` fail closed，直到独立设计并实现可恢复验证的 typed protocol-terminal fact。后续可通过已批准的 receipt method 扩展，但须另行审查 schema：事实要绑定可信 protocol adapter/identity 和精确 occurrence，EvidenceRef 指向该 receipt 的真实 ID；恢复 validator 必须校验同一 typed fact，缺失或普通 success 一律拒绝。不能只在 adapter 端拒绝，因为 reopen 仍会受当前 validator 的接受路径影响。

**验收：** 普通 dispatched success receipt 加 `remote_terminal` checkpoint，`PrepareResume` 必须失败且不调用 index resolution、保持 quarantine；只有未来通过单独设计批准并持久化 typed terminal fact 的测试，才能覆盖 `TestTrustedRemoteTerminalEvidenceResolvesQuarantine`（这是 contract 中要求但当前源码未具备的能力）。本 slice 不宣称任何现有协议已支持 remote terminal resolution。

## 已确认可沿用的实现边界与待办前置

- 一个 prepared handle 的 `Snapshot`/`DurableWriter` 足以实现适配器，不需要第二次 `PrepareResume`。`DurableSessionWriter.AppendAndSync` 返回规范行，adapter 能在 sync 后取得 writer 生成的 entry ID 与 seq。
- `WorkspaceQuarantineIndex.BeginDispatchBatch` 明确规定调用方先持有 session lock，只有 `CommittedNow` 才可走后续 prepared invocation dispatch；`AlreadyExists` 不是 replay permit。`ApplyDurableResolutions` 只镜像已被 session loader 验证的 checkpoint，符合 `session sync → index sync` 顺序（`internal/session/workspace_index.go:229-238,300-303`）。
- Mid-turn compaction 的 run-start snapshot、`SessionView.Revision` 与 `SourceEntryIDs` 可作为稳定来源；游标映射应仅用 cursor/source IDs 转到真实 entry ID，不比较 message 文本。Compaction sync 必须早于下一次 provider 请求。
- Terminal checkpoint 必须在最终 transcript、projection 和 outcome sync 成功后才产生持久化 ACK。已流出的 token 可见不等于已保存；Append/flush/sync 失败时要明确返回 persistence error。
- 实际 CLI 入口可达 `AssembleAgent` 同时创建 Harness 和 TurnRunner；prompt 使用 Harness，REPL 使用 TurnRunner（`internal/cli/assemble.go:394-411`、`internal/cli/prompt.go:153`、`internal/cli/repl_session.go:687-708`）。两条入口都需接同一 durable authority。首个 provider 请求前的 `CommitInput`、工具派发前的 session+index 提交、receipt 后再调用 provider、terminal sync 前不报告 saved，需留在真实 assembly/CLI 集成验收中。
- 文档第 3 行和第 77 行仍以 `bf073e8` 描述“等待 parent approval”及“runOwner 只注入 claim ID”。这与已批准的 `c1edd4d` 不一致：该提交已有 `WithRunCommit`/`RunCommitFromContext` 原子绑定，并在 callback 前由真实 runOwner 注入 `RunID`、覆盖并验证 `OwnerGeneration`（`internal/core/run_commit.go:34-93`、`internal/core/run_owner.go:251-271`）。请更新状态/基线并删除这一条过时缺口；生产 sink call site 和 Loop/CLI durable consumer 未实现仍可作为后续实现工作保留。
- CompletionProof 仍是必要前置：旧 `bf073e8` 的 `ActionExecution` 尚无该字段，不能靠 `ExecutionIdentity` 替代本地 quiescence proof。Storage reopen 后 exact-duplicate retry 是否确认文件 Sync 也是外部前置，目前由独立 RED/GREEN 修复覆盖；本文不重复评审该修复，也不把它称为已合入。
- `PreparedSession.Close` 会释放该 handle 与文件锁；run status handle 暴露的是可查询快照。集成实现需在 owner 仍可能晚写 resolution 时保留对应 prepared handle，不能仅因 Harness 返回 `cleanup_pending` 或 stream close 就关闭它；session 切换/退出也要走同一生命周期约束。
- `validateExecutionResolutionEvidence` 的 OperatorReconciled 分支会读取 `occurrence.receipt.Seq`。当前生产调用链在调用它之前先于 `internal/session/durable_recovery.go:262-268` 拒绝 nil receipt，所以已审 source path 不会因此 panic；若抽取或重用该 helper，应保留此调用前置或在 helper 内显式拒绝 nil receipt。

**复审覆盖：** handoff 文档全篇；`r2-contract.md`、`r2-local-quiescence-proof.md`；固定 `4bb628f` 源码中的 `internal/core/run_commit.go`、`loop.go`、`internal/session/durable_contract.go`、`durable_jsonl.go`、`durable_recovery.go`、`derive.go`、`workspace_index.go` 与 CLI `assemble.go`/`prompt.go`/`repl_session.go`；已批准 `c1edd4d` 的 run binding/owner 实现及 R4 prepared-dispatch 设计。未跑测试：此项为 source-based 文档设计审查，且 session adapter 尚未实现。

## 固定修订复审：`22a94594d96b44f1d9e15040d24e6bc41a067cdf`

**结论：设计通过，可以进入依赖实现；不是代码验收。** 此次固定文档位于 `r2-session-run-commit-adapter-handoff.md`，其父提交 `f4e1fb2` 基于源码 `7ed603e`。修订后六项初审问题均有闭合约束或明确的实现前置，没有剩余设计阻断。

- **ADP-01，真实证据行 ID：已闭合。** 七方法的 error-only `RunCommitSink` 保持不变。adapter 只在 `AppendAndSync` 成功后，用返回的规范行 `EntryID`/`Seq` 建立 occurrence + row-kind/state 索引；reopen 从同一 prepared handle 的 `DurableEntries` 重建。`EvidenceRef` 缺失时仅允许唯一匹配回填，传入值必须完全相等，其他情况在写 resolution 前拒绝。
- **ADP-02，并行拒绝与回执顺序：已闭合。** `BeginIntentBatch` 只写 assistant 与获准的 intent，不提前落盘 `KnownReceipts`。Loop/coordinator 把拒绝和执行结果一起按每 run 的 canonical `TranscriptIndex` 缓冲及串行提交；sink 对跳号/倒序拒绝写入。handoff 给出 i0 success、i1 denied、i2 success 且 worker 以 i2/i0/i1 完成的具体例子，物理 receipt 仍为 i0/i1/i2。
- **ADP-03，sequential 同组后续调用：初审拒绝已由精确例外替代。** 固定设计认识到 workspace index 在任何 unresolved marker 存在时阻止下一批，因此不引入 same-run 豁免。它允许的仅是当前 open assistant group 内、完整 occurrence/action reference 匹配且先前 intent 已落盘的 execution metadata，以及目标 receipt 已同步、证据行先于 checkpoint 的同组 resolution。其他 checkpoint、transcript interleaving、跨组迟到 proof 仍拒绝/延后。两个文档样例分别覆盖 proof 早于 receipt、晚于 receipt；两个样例都在 call 1 的 session checkpoint 和 workspace resolution 完成后才尝试 call 2 marker，没有 worker 等待 receipt 的环。
- **`SessionViewFromEntries` 和未闭合组：边界明确且可实现。** 同一个纯 durable-action analyzer 应被 append validation、recovery 与公开 view 构造复用；直接 view 调用先校验精确证据关系，再因同组尚有未回执调用而返回 `errUnclosedToolCallGroup`，不发布部分 assistant/tool 消息。prepared handle 的 `PublishedView` 保持上一个完整视图直到整组回执闭合。实现时保留输入域区别：当前 `PrepareResume` 把完整 durable journal 交给 recovery analyzer，以发现非 active branch 的未决 marker；公开 `SessionViewFromEntries` 只校验调用方选定的 root-to-leaf branch。可复用相同纯校验逻辑，但不能因此把 recovery 扫描缩成只看 active branch。该修改可围绕现有 `analyzeDurableActionJournal` 与 `validateToolCallJournalTail` 收敛，不需要改 projection 顺序或 workspace admission 语义。
- **ADP-04，terminal transcript：已闭合。** 只接受物理 Seq 上与最终 canonical transcript 完全相同的连续前缀，之后仅写连续 suffix 与 terminal checkpoint 的同一批 sync；洞、后索引先出现、重复或 payload 不同都在 append 前拒绝。
- **ADP-05，本地停止证明：设计将其保留为明确前置。** approved closed `PreparedCompletionProof` 必须进入 execution durable schema，recovery loader 对 occurrence、row kind、证据顺序与 proof enum 做校验；identity-only 历史行不授权本地 resolution。当前 source 尚未实现该字段，handoff 明确禁止在 schema/validator handoff 落地前启用 local `ResolveExecution`，所以这不是本 adapter 文档声称已解决的能力。
- **ADP-06，远端 terminal：当前普通成功被明确禁止。** handoff 规定现有 schema 不支持远端终止证明；generic success receipt 加 `remote_terminal` checkpoint 必须在 reopen 被拒绝并保持 marker 未解决。源码 `validateExecutionResolutionEvidence` 目前仍会接受 dispatched 且 outcome 非 `unknown` 的普通 success，因此实现不能只在 adapter 的 `ResolveExecution` 拒绝：必须同时修共享 loader/analyzer，使当前 schema 下远端 resolution 全部 fail closed；只有以后单独评审并持久化 typed protocol-terminal fact 才能开放。这是文档列明的实现门槛，不是批准 adapter 越权推断远端终止。

**顺序/生命周期核对：** sink 仍持有同一个 prepared session handle；会话先 AppendAndSync，随后才写 workspace index；workspace index 不回调 session。worker 与 cleanup 只向 owner 的 nonblocking readiness queue 报告完整 ref 和已关闭 proof，不写 checkpoint、不等待 receipt；owner 在 receipt sync 后负责写证据/resolution，并在 index resolution sync 前保持 quarantine。崩溃只留下未解决 marker 或可由同一已验证 checkpoint 幂等恢复的状态，不产生 replay permit。当前 handoff 也继续拒绝未审查的 durable child/delegation 方案。

**复审覆盖与限制：** 固定 `22a94594d96b44f1d9e15040d24e6bc41a067cdf` 文档、父提交链、R2 contract/local-quiescence proof，以及基线 `7ed603e` 的 `internal/session/{derive.go,durable_recovery.go,durable_jsonl.go,workspace_index.go}` 和 core sink 接口。没有运行测试：本轮只批准设计可实施性；shared analyzer、proof schema/recovery validator、Loop receipt buffer 与 adapter 仍需各自的实现和测试验收。
