# R2 源码可实现性校准

状态：源码可实现性预研；供 R2 contract reviewer 批准前对照。只审查设计与当前源码接线，不改产品实现，不代表任何测试已运行。

源码基线：主分支 `ba92e45`（R1 三条实现 lane 尚未整合）。下文“当前”仅指该快照；最终接口及批次粒度以评审通过的 `r2-contract.md` 和 `r4-prepared-dispatch-design.md` 为准。

## 结论

R2 的 JSONL + SessionTree 方向可以沿用，contract 中的稳定身份、追加同步、无自动重放、完整工具调用组和 compaction source boundary 也能映射到现有项目。但它不是只在 JSONL store 加一个 AppendAndSync 就能落地：当前 CLI 还没有物理 session 选择/切换运行时，AgentImpl 还没有消费 typed transcript，LoopAgent 还没有 CanonicalLoop 实现，工具分发也逐个执行。实现顺序必须先让 BLD-01 提供“锁住且已验证的 SessionView/store handle”，再让 BLD-02 接真实 run 和 CLI，不能让两条 lane 同时改 JSONL 内部。

优先级最高的实际闭环是：选定或创建物理文件并持锁 → 恢复并验证树及模型历史 → 提交用户输入 → provider → 提交整组 tool call 与 dispatch marker → 执行 → 提交每个 receipt → 提交 compaction projection 与 terminal → 再向用户报告已保存/完成。任何一步持久化失败，都必须停止后续 provider/tool 工作，返回可见的持久化错误。

## 实际入口与合同间差距

| 路径 | 当前源码行为 | R2 必须接的最小改动 |
|---|---|---|
| REPL 组装 | assembleSession 在 assemble.go:1469-1477 打开 store；打开失败只告警并把 store 置空。目录模式在 repl_session.go:311-313 才以 assembly.SessionID 调 SetSessionID。assemble.go:504-506 将配置 Session.ID 或 agentName 作为 sessionID。 | 准备 SessionRuntime 时区分用户可见 label 与随机物理 SessionID。启用持久化却无法开锁/恢复时组装失败，不能静默退化成无持久化 run。新建文件必须独占创建，不能复用同秒同 label 路径。 |
| 启动恢复 | assemble.go:348-355 用 loadSessionHistory(FilePath()) 另读一条历史且忽略错误；interactive.go:127 起的辅助读取器会跳过坏行，并只投影 user/assistant。repl_session.go:287-305 又从 store 建 tree 后调用 EntriesToAgentMessages。 | 只保留一套经校验的 SessionView 恢复结果，携带 Revision、分支和每条有效消息的 SourceEntryID。禁止旁路读取、忽略错误或跳过损坏记录。 |
| /resume | slash_commands.go:210-237 展示的是物理文件列表 ID；176-203 将参数传给当前 SessionTree.MoveTo，BuildContext 也只针对当前树。它没有切换物理文件。 | 分成物理文件选择与当前文件内 branch/entry 选择。/resume <session-id> 应先准备、加锁、恢复目标物理文件，再原子切换；树内跳转应使用明确的 branch/entry 命令或参数。失败时旧 runtime 保持可用。 |
| 新 session | JSONLSessionStore.SetSessionID 在 jsonl.go:150-191 先关旧文件、清内存，再按时间戳和传入 ID 创建文件；打开选项没有 O_EXCL。失败可使旧 runtime 已关闭、新 runtime 未就绪。 | 先在独立 handle 上创建并完整校验新 root，再交换活动 runtime 指针，成功后才关闭旧 handle。ParentID 为空，树和 Agent history 都从新 root 构建。 |
| REPL 持久化 | repl_session.go:897-960 在 turn 完成后才拼 user/final assistant 两条普通 entry；先 Append tree，后 enqueue/flush；错误只记日志，Save 错误被忽略。639 行处最终文本可能已输出，然后才 persistSession。它完全没有记录完整 typed transcript/tool receipts。 | 由 per-run commit sink 写入 semantic boundary；tree 只接收 AppendAndSync 返回的 canonical rows。最终同步失败要返回未保存状态，允许此前 streamed token 已显示，但不能报 durable ack。删除或限制一秒 flush ticker 对权威记录的写入责任。 |
| 一次性 prompt | prompt.go:124-130 只有 --fork 时才启用 session；153-176 使用 Harness.Submit。fork 的选取/恢复见 277-306。 | 对启用了 session 的 prompt 使用相同 SessionRuntime 和 sink；未启用 session 的 prompt 不宣称 durable。若产品要求所有 prompt 都先落盘，必须显式把 prompt 接入 session，而非假定已有链路。 |
| Core run | AgentImpl.Run 在 agent.go:124-153 先将 user 加到内存 history，调用 a.loop.Run，之后再从 lossy AgentEvent 拼一条 assistant。CanonicalLoop 目前只是 optional interface（canonical_loop.go:46-52），LoopAgent 没有 RunCanonical。Harness.run 与 TurnRunner 最后都直接走 Agent.Run（harness.go:171-181；turnrunner.go 的 Agent 路径），RunClaim.ID 尚未作为运行身份注入 context。 | 等 R1 typed transcript consumer 落地后，AgentImpl 每次只消费一份 CanonicalRunResult；run owner 在 Harness/TurnRunner 注入 RunID 和 commit sink。Agent history 与 SessionView provenance 在同一快照边界捕获，不能由 events 重建。 |
| 工具批次 | loop.go:625-735 的并行模式先筛 active calls 再开 worker；顺序模式每个 call 经 interceptor 后立刻 executeTool。两条路径都没有“整组 preflight + 持久化完整批次 + dispatch-ready 后才执行”的边界。 | 把一次 assistant response 作为原子组：完整验证 ID 与参数，完成所有 pre-dispatch approval，再一次提交 assistant/tool-call entry、全部 intent 和 denial receipt；session batch 与全局 dispatch marker 均 sync 成功后才启动首个 executor。执行后每个 receipt 必须先持久化，再允许下一次 provider request。 |
| compaction | mid-turn compaction 在 loop.go:738-755 内就地替换 provider messages；当前 session projection 在 derive.go:13-35 按最后一条 compaction row 的物理位置裁剪，context.go:192-219 还会跳过 tool 和 compaction。 | Loop 在实际使用 summary 前提交独立 typed CompactionProjection。restore 按 CompactedThroughEntryID 的原始源边界选择 summary 并保留其后到当前 leaf 的未覆盖 suffix；不得按 marker row 的 ParentID/物理位置或有效数组下标猜边界。 |

## BLD-01 / BLD-02 的最小 ownership seam

合同 ownership 表已将 JSONL schema/open/recovery 与 CLI/run integration 排成顺序 lane（r2-contract.md:145-149），这个约束必须保留。最小接法如下：

1. BLD-01 独占 internal/session/types.go、jsonl.go、durable_jsonl.go、tree/context/derive/snapshot 与 session 测试。它负责新 schema、物理 session 身份、文件锁、恢复/修复、序列号和 branch/tree 投影，并公开足够窄的 prepared/open 接口。
2. BLD-01 应先提供等价于 PrepareNew(label) / PrepareResume(physicalSessionID) 的结果：不可变的 SessionView{SessionID, BranchID, Revision, Messages, SourceEntryIDs} 加上持锁、已恢复、可 DurableSessionWriter 写入的 store handle。准备失败不触碰当前 live handle。不能让 BLD-02 直接读取 JSONL 内部 fields 或重写 ensureLoaded。
3. DurableSessionWriter 实现也由 BLD-01 在 durable_jsonl.go 完成。AppendAndSync 输入稳定 EntryID，先验证整批、分配持久 Seq，追加 newline 完整行，Flush + Sync 后返回 canonical entries；失败后 poison 此 writer，恢复前禁止继续写。树只接受返回的 canonical entries，避免内存 Seq 与磁盘 Seq 分叉。
4. BLD-02 在 BLD-01 集成后只消费上述接口，实现 core sink、workspace index、Agent/Loop sink 接线、REPL/prompt 原子 swap 和 acknowledgement。它不编辑 jsonl.go、types.go 或 BLD-01 的 store fields。Agent/Loop 改动还须等 R1 生产 transcript owner handoff。
5. 普通 PendingSessionWrites 不能再负责 user、tool、receipt、compaction、terminal 等权威边界。可先保留兼容类型，但 savepoint 要改为绝对 enqueue watermark，且计数仅在 Save/Sync 成功后推进；并行 ticker 不得与 semantic commit sink 竞争同一个 store 的顺序/ACK。

现有 SetSessionID 会破坏式地切文件，不能兼作 PrepareNew。DirectoryStore 也必须先解析确定目标文件，再对该文件持锁，然后 load/validate；目前 ensureLoaded 在 jsonl.go:420 先 loadEntriesLocked、427-435 后 flock，存在另一进程在读取与加锁之间追加/修改文件的窗口。锁和恢复应该由 BLD-01 的一个入口完成，不能分别由 CLI 与 writer 再锁一次。

## 恢复与 cursor 到 EntryID 的确定性映射

合同要求 cursor 使用运行开始时 prior history + 本轮 transcript 的坐标，SessionView 提供有效消息对应的来源 entry（r2-contract.md:89-112）。现有 SetHistory 只接收 []AgentMessage（agent.go:208-224），EntriesToAgentMessages 会丢掉 tool/compaction rows 和来源 ID（context.go:192-219），所以仅加 cursor struct 仍无法正确 commit projection。

建议的最窄映射边界：

- BLD-01 输出 SessionView，Messages 与 SourceEntryIDs 同长度、按同一 branch 的有效投影顺序排列；Compaction summary 消息映射到其 compaction marker EntryID，同时保留 CompactedThroughEntryID 用于进一步定位覆盖边界。History Revision 随 SetHistory、branch/session 切换及已提交历史变更变化。
- R1/R2 run owner 在运行开始捕获 immutable prior snapshot/revision。每个本轮 TranscriptIndex 只由 CanonicalRunResult delta 的真实消息顺序映射至 AppendAndSync 返回的 EntryID。tool receipt 通过 RunID + AssistantTranscriptOrdinal + ToolCallID 配对；ID 唯一性仅限一个未闭合 assistant group，不得跨 run/history 全局去重。
- compaction cursor 的 PriorHistoryCount 只能索引 run-start SessionView；TranscriptCount 只能索引 canonical current-run delta。初始 user 只在 delta 出现一次。projection commit 将 cursor 转成精确 source EntryID，再写 CompactedThroughEntryID；不能从最终 events、role/content 比较、Agent 当前已压缩 history 或当前 leaf 推测。
- 当前 derive.go:14-19 用 marker 的 row index 丢弃 marker 之前所有记录。新 marker 可晚于它覆盖的 source boundary（ParentID 指向当前 leaf），故恢复必须沿目标分支/Seq 查 boundary，并构造“summary + boundary 之后的有效 suffix”；旧 marker 不重复投影。完整 assistant tool-call group 不可被边界切开。

当前 DefaultSessionTreeBuilder 会跳过坏/孤儿行并按 Timestamp 选择 latest leaf（tree_builder.go:35-103）；这与 R2 的 fail-closed 校验、父链/Seq 顺序权威冲突。tree.Append 对 Seq=0 自增自身计数（tree.go:60-99），但外来非零 Seq 不推进 counter，恢复后追加可能重号。BLD-01 应由 loader 验证唯一 EntryID、单文件父链、严格 Seq、合法分支/组闭合后返回错误；禁止跳过损坏后暴露部分历史。writer 赋 Seq，tree 接受 canonical Seq，不重复分配。

## Partial batch 和文件尾恢复必须落实到协议

合同已经覆盖 session intent prefix 与全局 dispatch-ready marker（r2-contract.md:65-87、114-123），但当前 JSONL Append 每条单独写且不 sync，随后 Save 才 Flush/Sync（jsonl.go:247-270、336-363）。新 AppendAndSync 的 partial prefix 可以留下完整 JSON 行，因此 writer 与 loader 必须按 ID/ActionBatchID 做幂等核对。

- 固定批内顺序为 canonical assistant tool-call entry 优先，然后按原始 ToolCallID 顺序写 intent/预拒绝 receipt，最后 batch prepared/checkpoint。这样只要有 intent 前缀，loader 就能恢复完整 call 列表；如果 assistant row 尚未落盘且无 marker，证明没有 executor 开始，但不能凭不完整信息构造成功历史。
- loader 先读可解析的完整 rows，不先要求 tool group 闭合；根据全局 index 确认 marker 是否存在，再为未闭合 call 持久化 interrupted（可证明无 marker）或 unknown（marker 存在/索引不可验证则 fail closed/quarantine），最后再做严格的 group validation。session lock 与 index lock 的获取顺序须一致，避免多进程死锁。
- 文件最后一行若是完整 JSON 但缺 LF，只补 LF 并 sync；不能当 torn JSON 丢弃。只有能由 parser 证明为截断的尾部才复制原始 suffix 到 sidecar 并 sync 后修复；完整但格式错误行和中段损坏必须失败关闭。当前 jsonl.go:500-521 通过 Scanner 跳过坏行，需由 BLD-01 替换为能保留原字节、识别完整边界的 reader。
- 检测到同步错误的调用端必须 reopen/recover 后按稳定 EntryID + canonical payload 判断实际落盘状态；不能依赖 Append 返回值、FlushedCount、内存 tree 或事件流判定。dispatch-ready marker 即使发生在 Sync 返回错误之前也可能已经持久，所以恢复时 marker 存在等价于“动作可能已开始”。

## 全局 workspace quarantine 的可执行边界

R2 contract 把跨 session mutation quarantine 建在可信 EffectSet、WorkspaceID 与 ExecutionIdentity 上；contract 已指出 EffectSet 依赖 R4-EFFECT-01（r2-contract.md:81-85）。当前 ToolDefinition 只有 Execute(ctx, call)（tools.go:49-58），PlanMode 按工具名 allowlist（plan_mode.go:24-42），BashTool 直接用 CommandContext(...).Run()（bash.go:187-205），没有稳定 child/process-group 身份的持久化回调。MCP/文件工具也没有统一的进程身份协议。

因此：

- R2 可以先做 fail-closed 的 workspace index：所有未知 EffectSet 的未决 action 都视为 mutating；在确认终止或 durable reconciliation checkpoint 前持续 quarantine，绝不依据 PID 猜测或自动重放。
- 可安全 read-only 放行必须等待 R4 trusted executor EffectSet seam 与独立验证，不能依据工具名，也不能把自定义 registry 同名工具当成内建可信工具。
- 若 R2-REV 要证明 SIGKILL 后 Bash child 的真实存活/退出与安全 release，必须在 R2 实现受限的 child identity seam，例如由可信 Bash executor 在 Start 后报告 OS generation + PID/start token + process-group identity；否则该 acceptance 应验证“身份缺失时一直 quarantine、无 PID kill”，不能声称验证了自动识别 child 生命周期。远程 MCP action 即使本机已退出仍保持 unknown；确认进程结束不等于 action 成功。

## 必要接线验收

以下应作为真实路径回归，而不仅是 helper 单测。合同已有同名或等价测试时可直接复用，逐项验证实际 CLI/Loop 集成：

1. new → 两次快速创建得到不同物理文件和空 root；resume list 中物理 session ID 能切换到该文件，失败时当前 session 仍能继续；resume 的 branch/entry 跳转只能在目标文件树内发生。
2. 两进程同时打开同一物理文件：第二个进程须在读取/修复前等待或失败；AppendAndSync 后 reopen 两次，ID、payload、Seq、ParentID、当前 leaf 一致。对不同 session 同时追加时，各自身份不串线。
3. 注入 user Append/Flush/Sync 错误，mock provider 调用数必须为 0；注入 assistant batch / index marker 错误，mock executor 调用数必须为 0；注入 receipt sync 错误，Loop 不得开始下一次 provider request。UI 允许已显示 delta，但必须报告持久化失败，不能显示已保存/完成。
4. sequential 与 parallel 工具模式都必须在第一个 executor 前看到完整 assistant group、所有 intent、denial receipts 和 dispatch marker 已 sync；每个 receipt 保存原 provider ID，后续 group 或新 RunID 可以合法复用 call_1。重复/冲突 receipt、missing receipt、tool call ID 被错误跨 run 去重都要覆盖。
5. 对一个 tool group 的每个写入前缀注入 crash：无 marker 时合成 interrupted 且不执行；marker 存在时缺失结果转 unknown 且不重放；合法完整 JSON 缺 LF 保留 receipt；明确 truncated EOF 才隔离尾部；完整 malformed/middle row 导致打开失败。
6. compaction 生成后、projection sync 前 crash 应恢复原始 transcript；sync 后重启应只见一条 untrusted summary + CompactedThroughEntryID 之后至 leaf 的全部 suffix，保留工具调用/receipt 配对。多个 projection 不得重复或重编号 cursor。
7. 运行覆盖 REPL 和 --fork prompt 两个真实入口。未启用 session 的 prompt 单独断言为 ephemeral，不能被报告成 durable path。

这里只完成源码可实现性审阅，没有运行测试。合同给出的 session、CLI 与子进程 restart suites 应在 BLD 阶段按上述路径验证；静态源码映射不能作为 R2 功能通过的证据。
