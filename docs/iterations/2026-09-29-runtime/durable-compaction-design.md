# R3 durable compaction source and persistence design

状态：面向下一轮 builder 的源码约束与最小实施契约。基线源码为 HEAD 17ab7b76ef0b7636abff1e5a055de7acf6f6bb31。本文只写设计，不表示 compaction 已接通、持久化、测试或恢复闭环。

依据：[`r3-compaction-design.md`](../2026-09-25-runtime/r3-compaction-design.md)、[`r3-production-remaining-contract.md`](../2026-09-25-runtime/r3-production-remaining-contract.md)、[`r3-breaker-loop-handoff.md`](../2026-09-25-runtime/r3-breaker-loop-handoff.md)、[`r2-core-consumer-handoff.md`](../2026-09-25-runtime/r2-core-consumer-handoff.md)；reviews [`r3-summary-safety-review.md`](../2026-09-25-runtime/reviews/r3-summary-safety-review.md)、[`r3-compaction-groups-review.md`](../2026-09-25-runtime/reviews/r3-compaction-groups-review.md)、[`r2-session-run-commit-review.md`](../2026-09-25-runtime/reviews/r2-session-run-commit-review.md)、[`r2-core-loop-consumer-review.md`](../2026-09-25-runtime/reviews/r2-core-loop-consumer-review.md)、[`r2-core-consumer-design-review.md`](../2026-09-25-runtime/reviews/r2-core-consumer-design-review.md)。规则依据主仓与 go-cli-infra 的 AGENTS/CLAUDE、`rules/go-validation-rules.md` 和 `rules/verify-rules.md`。

## 当前源码与边界

当前 RunCommitSink 已是七方法 error-only 接口，其中包含 CommitCompaction；CompactionProjection 已有 ExpectedHistoryRevision、原始坐标 Through 和 Summary；RunTerminal 也已有 Projections 字段（internal/core/run_commit.go:120-132,270-292）。SessionRunCommitSink 已捕获不可变 run-start PublishedView，并能校验 revision、把 cursor 映射到真实 SourceEntryIDs 或当前 run 的 TranscriptIndex row、校验闭合 tool group，再 AppendAndSync EntryTypeCompaction（internal/session/run_commit.go:49-97,660-673,833-930）。durable session view 在完整 source boundary 后恢复一条 user-role compaction summary 和 suffix（internal/session/derive.go:230-280,368-390；internal/session/durable_jsonl.go:430-500,802+）。

当前 core 尚未把这些 seam 接起来：RunCanonical 的 terminal defer 以 nil projection list 调 CommitTerminal（internal/core/loop.go:372-435）；runCommitConsumer 没有 CommitCompaction 调用入口；durable consumer 下 Loop 会把实际 compactor 换成 unavailableRunCommitCompactor，触发时返回 ErrRunCommitCompactionProjectionUnavailable（internal/core/loop.go:907-920, internal/core/run_commit_consumer.go:20,867-872）。这条路径当前 fail closed，是已核实能力缺口。不能根据 session sink 单测声称 durable compaction 已接通。

Compaction 的组边界与摘要角色已有独立接受的内存层实现，不在本设计重写：groupTurnItems 校验 call/result ID 组；SummaryCompactor 保留真实系统消息，产生 RoleUser + IsCompaction 摘要；Loop 与 session restore 都用 UntrustedSummaryContext 包装派生摘要（internal/compaction/tool_groups.go:19-83, internal/compaction/summary.go:198-220, internal/core/loop.go:1091-1107, internal/session/derive.go:368-390）。旧 summary-system-role P1 已被后续代码修复，本设计不把它误报成当前缺陷。

当前仍有两个必须直接封住的来源缺口：

1. Loop 在工具处理后才调用 midTurnCompactor.CompactIfNeeded（internal/core/loop.go:907-920）；首个 provider request 没有 compaction preflight。新实现必须在每次真正 provider request 前检查，包括首请求和 tool/steering/follow-up 后的 request。
2. messagesToTurnItems 生成 msg-N 临时 ID，并没有传 AgentMessage.ReceiptOutcome（internal/core/loop.go:1050-1088）。TurnItem 本身有 ReceiptOutcome 字段，但 groupTurnItems 当前只校验 group closure；unknown terminal receipt 仍可传给摘要/占位/截断策略（internal/compaction/compactor.go:34-58, internal/compaction/tool_groups.go:19-83, internal/compaction/summary.go:250-310）。本设计按本轮明确要求增加 unknown/pending compaction 拒绝；保留已接受的 group closure 行为，并保留 known error/interrupted 的真实 outcome。

## 唯一持久化事实与最小 API

不增加 event store、compaction ledger、session 表、第二份 transcript 或新的 RunCommitSink 方法。复用现有 RunCommitSink.CommitCompaction 与 RunTerminal.Projections。core 只生成 typed projection 并调用 sink；session adapter 仍是唯一能把 cursor 转为真实 entry ID 并写 JSONL/index 的层。

durable Loop 在每次 provider dispatch 前，按以下顺序工作：消费 steering/follow-up 并先同步 canonical input；完成 dynamic tool filtering 得到实际 iterOpts；构造带来源标记的有效消息；检查完整 ToolCallID 组和 unknown/pending 禁止条件；若超预算则调用 compactor；从该次精确结果取得 source boundary 与原文摘要；调用 CommitCompaction 并等待返回成功；只有成功后才替换当前请求上下文并允许 provider dispatch。Compaction append/sync/refresh 失败时，当前 provider request 数必须保持不变，run 结果不得被记作 success/已保存成功。

projection 的 Summary 必须等于该次 compactor 返回的 IsCompaction item 原始摘要内容，不包含 UntrustedSummaryContext 包装。provider prompt 与 session restore 分别且恰好一次把它投影成 user-role untrusted context，不写成 system 指令，不添加普通 user transcript/input row。durable PublishedView 在 marker sync 后应呈现同一摘要和相同的未覆盖 canonical suffix；动态 system prompt 与 SystemReminder 临时贡献可继续参与本次 request，但不进入 durable projection，也不推进 cursor。

当前 CompactionProjection 只表达“以一条摘要替换一个完整的 source 前缀”。Micro 的原地 tool-result 占位和 Truncating 的静默删除都不能按现有 schema 精确还原。第一版 durable 模式只接受含一个明确 IsCompaction summary 和 source boundary 的 compactor result；如果 Summary/Unified 最终退化成只占位或丢弃的结果，则以 typed unsupported/compaction error 在 provider dispatch 前 fail closed。不要伪造一个摘要声称来自 compactor，也不要让 prompt 使用一个 session restore 无法重建的结果。无 durable binding 的旧 compaction 路径不受此限制。

## Source provenance：包括同一 run 多次压缩

Cursor 永远在 run-start 坐标空间：PriorHistoryCount 是捕获的 PublishedView 前缀数量；TranscriptCount 是当前 run canonical transcript 中从第一个 entry 起被覆盖的数量。若 TranscriptCount 大于零，PriorHistoryCount 必须等于 run-start prior source 总数。SessionRunCommitSink 再用它自己捕获的同一 PublishedView.SourceEntryIDs 和已同步的 run TranscriptIndex rows 求真实 CompactedThroughEntryID；任何 revision、坐标、branch 或 closed-group 不匹配都失败。绝不靠内容相等、Agent.History 长度或压缩后数组下标选择 source row。

Loop 必须为有效 prompt history 保存与消息一一对应的 core-only provenance sidecar：

    prior published item: exact SourceEntryID + run-start PriorHistory ordinal
    current run item: exact canonical TranscriptIndex
    ephemeral system contribution: no durable source cursor
    derived summary: original source-through cursor covered by that exact summary

来源由 durable session hydration 提供，并随 Agent history clone 与 SystemReminderInjector 显式保留/标注。一次 compaction 结果中的 summary 接收本次实际被替换 source prefix 的 Through cursor。后续同一 run 再压缩时，summary 保留它原先的 Through；后缀保留各自最初的 SourceEntryID 或 TranscriptIndex。第二次 projection 的 cursor 根据被替换项的 original provenance 得出，不能根据当前 messages/TurnItems 长度重编号。msg-N 只能是局部调试/分组 ID，不是 durable source。

建议把 per-item source marker 留在 core/compaction 的运行期结构，不写进 provider wire message 或 RunCommit action ledger。Compaction package 只需要知道来源类型与 opaque SourceThrough；core 把返回边界转换为现有 core.CompactionCursor。RunCommitBinding 目前只包含 SessionID、WorkspaceID、OwnerGeneration（internal/core/run_commit.go:33-40），不足以定位 run-start view revision；最小补充是一个与同一 sink 原子绑定的可选 RunCommitCompactionSource 能力，返回 captured history revision、prior source 数与按 PublishedView 顺序的 source IDs。SessionRunCommitSink 直接从已有 baseView 实现此能力。若 Agent history source IDs 与 captured view 不一致、来源缺失或出现无法解释的 source gap，durable compaction 在 provider 前拒绝。

同一 run 的每次 CommitCompaction 都使用原始坐标和原始 revision，按 source-through 顺序推进。第一次产生的 summary item 即使位置变化、下一轮再次成为 compact input，也继续带第一次真实 Through；新 summary 覆盖后缀时只能把 Through 推进到被它实际覆盖的最后一个原始 source。terminal CommitTerminal 必须携带与所有成功同步 projections 完全相同、顺序一致的不可变 projection list。若 session append 已可能 sync 但调用返回 error，不推测成功、不发 provider、不发 success ack；重试交给 session stable-ID/idempotency 约束，不创建另一套 projection ledger。

## Unknown 与 pending 输入

closed tool group 仍由 ToolCallID 配对；已知 success、error、interrupted 维持各自 outcome。ReceiptOutcomeUnknown 即使在结构上已有 terminal RoleTool row，也不能被 Micro placeholder、SummaryCompactor 或 Truncating 覆盖，不能送入 summarizer 变成“工具已经完成”的派生叙述。Loop 在调用任一 compactor 前显式检查；同时保留 outcome 到 TurnItem，保证策略不会因转换丢字段而误判。缺失 receipt、open assistant group、重复/错配 receipt 属于 pending 或 malformed group，沿用 group validation fail closed，不能 compaction、commit projection 或 dispatch 下一条 provider request。拒绝时保留 canonical transcript/journal 原事实，不尝试自动解决 unknown。

## 最小 API/文件 owner

| Owner | 文件 | 最小改动 |
|---|---|---|
| R3-BLD-03 core owner（待 BUILD-1 结束） | internal/core/loop.go、run_commit_consumer.go、run_commit.go、agent_history.go、types.go | 在 Loop 维护 prompt-item provenance；每次 provider 前 preflight；从 exact compactor result 创建 projection；调用 commitCompaction；terminal 传同一 projection list。给 canonical history 的运行期来源增加 sidecar，不改变 transcript/action row schema。 |
| R3-BLD-03 compaction owner | internal/compaction/compactor.go、tool_groups.go、summary.go、midturn.go | 只加 source-through result metadata、unknown/pending eligibility gate 和能返回 durable summary projection 的 typed result；不重写已接受的组算法。 |
| R2 durable CLI/session owner | internal/cli/durable_cli_session.go、repl_session.go、internal/session/run_commit.go | 将 captured PublishedView 来源带到同一 run binding；SessionRunCommitSink 从 baseView 提供 immutable provenance。已有真实 ID 映射、AppendAndSync 和 PublishedView 更新应直接复用；除非 RED 暴露明确缺口，不改 ledger/schema。 |
| R2/reviewer | internal/session/durable_jsonl.go、derive.go、run_commit_test.go | 验证真实 entry ID 映射、append/sync 后 PublishedView、restore/reopen 与 terminal projection equality；只在测试找到当前缺陷时由 session owner 修。 |

依赖：R2-BLD-01/02 真实 durable prompt/REPL 连接和 R2-REV-01 restart-safety；R3 已接受的 memory group/call-ID compaction slices；当前 streamjoin BUILD-1 完成后再编辑 internal/core/loop.go。R3 builder 不重做 session lease、RunOwner lifecycle、tool-call group algorithm，不碰 protected infra 或 shared task ledger。

## 必须先写的 RED 合约

- TestDurableCompactionRunsBeforeFirstProviderRequest：用真实 SessionRunCommitSink、临时 JSONL 与 loopback/fake local provider；首请求预算超限时 server 首次收到 request 前，projection 已 AppendAndSync，PublishedView 已呈现同一摘要+suffix。
- TestProjectionUsesCapturedPublishedViewSourceIDs：重复文本、系统行、旧 compaction summary 和普通 suffix 同时出现；cursor 只按来源 sidecar映射，sink 落到预期真实 boundary EntryID，不做内容匹配。
- TestMultipleCompactionsKeepRunStartCoordinates：一个 run 连续 compact 两次；第二次输入含第一次生成的 synthetic summary 与新 canonical transcript suffix。第二个 Through 来自 summary 保存的原始 cursor + 被覆盖 suffix 的原始 source，不从缩短后的 slice 重新计数；terminal 与 reopen 都有两条精确且顺序一致的 projection。
- TestSystemReminderDoesNotAdvanceCompactionCursor：临时 reminder 插入 prompt 前缀；真实 PublishedView source 与 cursor 不偏移，restore 不保留 reminder。
- TestUnknownReceiptCannotBeCompactedOrSummarized：ReceiptOutcomeUnknown 达到预算阈值时，三种策略都拒绝；Summary summarizer 调用数为零，unknown 原文/outcome 不被改成 known，不写 projection，不发后续 provider 请求。
- TestPendingToolGroupBlocksCompaction：assistant ToolCalls 缺 terminal receipt 或 ID 错配时，provider、summarizer、CommitCompaction 都为零。
- TestDurableSummaryIsUntrustedUserInLiveAndRestoredViews：live provider 消息与 reopen PublishedView 对摘要应用相同且仅一次的 UntrustedSummaryContext/user-role 投影；没有普通输入行或 system-role 摘要。
- TestCompactionSyncFailureStopsNextProviderDispatch：CommitCompaction append/sync/refresh 失败后无下一 HTTP dispatch、无 success terminal ack，marker 状态依现有 RunOwner/session policy fail closed。
- TestUnsupportedCompactionResultCannotReachProvider：durable 模式得到 Micro-only placeholder 或 Truncating-only drop 结果时，在 provider dispatch 前明确失败。

测试必须先检查真实 local HTTP request count、session durable row IDs、PublishedView 与 reopen 结果；单独比较 compacted []llm.Message 不足以证明持久恢复相同。设计阶段没有运行测试。
