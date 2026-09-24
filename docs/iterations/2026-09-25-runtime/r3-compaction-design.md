# R3-BLD-03 设计：请求前预算检查与完整工具调用组压缩

状态：builder 设计稿，待 R3-DES-01 与 R2-REV-01 放行后实现。本文只定义 R3 compaction 的最小改造边界，不修改 Runtime 架构、session writer 或产品源码。

## 目标与边界

保留现有 `LoopAgent`、`MidTurnCompact`、`Compactor` 与 JSONL/session tree。将已有“工具执行后检查阈值”的压缩接到每次真实 provider 请求之前；把 assistant tool calls 与按 `ToolCallID` 匹配的所有 terminal tool receipts 作为不可拆组；修正 small-model summarizer 的真实 CLI 装配；让 summary 能利用有界的非文件工具结果。

R1 `CanonicalRunResult.Transcript` 仍是当前 run 的唯一 transcript delta；R3 不从 `AgentEvent` 或 `EventStream` 重建 history，也不把 summary 冒充普通 assistant transcript message。R2 负责将 canonical transcript durable append 到 JSONL、写入 `EntryTypeCompaction`/`Summary`/`CompactedThroughEntryID`，以及恢复时应用 compaction 边界。R3 只保证送往 provider 的内存消息投影合法，并使用 R2 定义的 typed seam：`CompactionProjection{ExpectedHistoryRevision, Through: CompactionCursor{PriorHistoryCount, TranscriptCount}, Summary}`。Cursor 坐标固定在 run-start 的原始 prior history + canonical transcript delta；不得经多轮 projection 重编号，边界不得拆开 tool-call group。Projection 是 transcript 的替代上下文投影，不是 transcript/action 事实；R2 将其 revision 校验并映射到真实 session entry ID。未知副作用以 R1/R2 的显式 unknown receipt 表达，禁止作为待执行调用重放。

## 当前 CLI 实际链路与核实结果

默认 CLI 由 `AssembleAgent` 创建 Loop，再调用 `assembleCompactor`；后者在有 compactor 时通过 `SetMidTurnCompaction` 注入 `s.ac.maxTokens`。默认 compaction ceiling 为 8000，来源是显式 assemble 参数、`Compaction.MaxTokens` 或 fallback（`internal/cli/assemble.go:42-45,314-341,481-487,1409-1454`）。运行时 Loop 生成 system prompt、复制 submission history、补入当前 user input（`internal/core/loop.go:438-525`），随后每轮先消费 steering/follow-up，再构造实际 `iterOpts`（包括可能过滤过的工具 schema），然后直接进入 `generateWithContinuation`（`internal/core/loop.go:535-590`）。

已有阈值检查在工具执行完以后（`internal/core/loop.go:731-759`），因此不能处理超限的首个请求；provider 错误路径直接返回，不会调用 compactor（`internal/core/loop.go:590-603`）。`MidTurnCompact` 的 80% threshold 与 incremental counter 可继续使用，但调用点必须前移到每次请求之前，且要在实际 `iterOpts` 已确定之后。

原审计中的 compaction 发现复核仍成立：`CMP-03`（首请求前无 preflight）、`CMP-01`（截断/summary 不按 ID 保留完整调用组）、`CMP-02`（显式 `summary` strategy 未注入 small model）和 `CMP-04`（summary 忽略非文件 tool outputs）。其证据仍见 [04-session-compaction.md](../../audits/2026-09-24/04-session-compaction.md)。

## R3 最小实现

### 1. 每个 provider 请求前做一次 preflight

在 `internal/core/loop.go` 的每轮发送请求前加入 `preflightContext`。调用顺序固定为：消费 steering/follow-up → 应用 dynamic tool filtering 得到最终 `iterOpts` → 估算完整请求 → 必要时 compaction → 验证 compacted request → provider call。该路径覆盖首个请求、tool receipt 后续请求、steering/follow-up 之后的请求。完成前不得再依赖 turn 结束后的 Agent compaction hook。

保留 `MidTurnCompact` 的增量估算和 compactor 路由，但在 preflight 中避免 80% margin 被重复应用：先算出安全 `messageBudget`，再以 threshold ratio `1.0` 检查 `messages` 是否超过该值，超出时压到同一个 `messageBudget`。其余 turn-end/旧触发点移除，保证真正送 provider 的调用前只有一个权威检查。Compaction error、输出仍超预算、mandatory prompt 本身超预算或 transcript 校验失败时返回明确的 context/protocol error，并且不调用 provider。当前 submission 与已消费的本 run steering/follow-up 是不可静默丢弃的输入；若这些保留输入连同 system prompt 和工具 schemas 仍不能装入预算，向用户明确报 oversized context。

有效 provider request 预算按以下公式解析并打日志（整数向下取整）：

1. 若 active model 明确提供 `InputTokenLimit`，`rawLimit = min(Compaction.MaxTokens (默认 8000), InputTokenLimit)`；否则若 `ContextWindow` 已知，`outputReserve = 当前 generation 的显式 output cap`，未配置时使用 metadata `MaxOutputTokens`，再算 `rawLimit = min(Compaction.MaxTokens, ContextWindow - outputReserve)`。metadata 缺失时仅能使用配置 ceiling，必须标记模型限额未知，不可声称保证符合 provider context。
2. `safeRequestLimit = floor(rawLimit * 0.80)`。这个 20% margin 复用当前 `MidTurnCompact` 默认 80% 触发意图，补偿 tokenizer 与 provider 序列化差异；它不是新的 output cap。
3. `messageBudget = safeRequestLimit - estimated(final iterOpts.Tools) - estimated(protocol framing)`。tool schemas 在 compaction 预算外，因此必须先扣除；余下消息预算包含 system/history/user/tool receipts/summary 和多模态内容。若 `messageBudget <= 0` 或强制保留内容已超预算，不发 provider 请求。
4. 当 `InputTokenLimit` 是直接输入上限时不再二次扣 output reserve；只有从 `ContextWindow` 推导输入上限时才扣除 reserve。模型限制与 output reserve 均未知时，配置 ceiling 仍提供本地上界，但日志要显式标为 estimated/unknown。
5. 继续使用 assembly 已创建的 `CompositeTokenEstimator`。对 core 的 `llm.Message` 与最终 `llm.ToolDefinition` 先做 `json.Marshal`，再估算规范化 request structure，确保 JSON args、ToolCallID、role/name、schema 字段与消息 framing 都不再零成本；compaction 的 `TurnItem` 估算走相同字段清单，不能把 compaction-only ID/cache 元数据算入 prompt。图片不能再按固定 85 tokens 当成安全上界：使用当前 provider 支持的 modality/detail 与可得图像尺寸/patch 信息估算；若 input limit、图像大小或 provider image accounting 未知，则把预算状态标成多模态未知并使用 provider 配置的保守 per-image reserve，缺少可配置保守界时不能声称预检保证 provider 不 overflow。20% margin 只覆盖普通文本 tokenizer/serializer 偏差，不能宣称覆盖高分辨率图像。先估算 provider 实际将收到的过滤后 schema，而非 tool registry 中未发送的工具。
6. 当前 estimator 是模型无关启发式，不是精确 tokenizer。记录 `estimated_prompt_tokens`、`tool_schema_tokens`、`output_reserve_tokens`、`request_limit_tokens` 与 `safety_margin_tokens`；不能记录为 provider 实际 usage。只在 provider 已报告 usage 后以实际 usage 更新状态。

`maxTokens` 仍表示 compaction/request budget，不将它改解释为生成输出长度。若 active model 的 metadata 在 model switch 后无法同步到 Loop，本迭代必须明确将该请求限制标记为“未知、使用本地 ceiling”，并保留一个后续验收项；不能沿用旧模型 context window 冒充当前值。

### 2. 工具调用与结果必须以 ToolCallID 闭合组裁剪

增加一个 compaction 内共享的分组/校验 helper，供 `TruncatingCompactor`、`SummaryCompactor.findCutPoint` 与 provider preflight 使用。按单个 assistant tool-call group 做闭合验证，不在整段 history 建一个全局 ID 集合：call ID 只需在这个未闭合 group 内非空且唯一；durable identity 是 `(RunID, ToolCallID)`，provider projection 则以当前 assistant group 的 ToolCallID scope 绑定。不同已闭合 group/不同 run 可以合法复用如 `call_1` 的 ID。后续必须恰有一个同 group `RoleTool` receipt 对应每个 ID。多调用并行时允许 receipts 次序不同，始终按 ID 配对；原始顺序可以保留。Receipt 的 `ToolName` 缺失时，若 call ID 已唯一匹配，可从对应 call 确定性补齐；仅非空且冲突的 name 拒绝。未知/中断/失败是 terminal receipt 的语义，不改变 group closure。没有 receipt、重复 receipt、空 ID、孤立 tool result 或冲突 ID/name 都是 malformed transcript，preflight fail closed，不可猜测修复或 dispatch。

边界规则：assistant(tool calls)+全部 matching receipts 是原子 group。截断策略只允许整组保留或整组丢弃；summary cut 只能落在 group 外。被摘要覆盖的完整组可以转成纯文本 summary；summary entry 不保留可执行 `ToolCalls` 或 `ToolCallID`。普通无工具 assistant/user 消息可按现有 message boundary 处理。系统提示、当前 run 的 user/steering/follow-up 输入不可被 truncation 静默删除。System prompt 自身超过可用预算时返回明确错误，不返回一个仍超预算的“最佳努力”prompt。

`MicroCompactor` 只替换旧 tool output 内容并保留原 ID，因此继续可用；更新 `ToolResult` 后必须同步重算/清理 `EstimatedTokens` 缓存。summary/truncation 选出的结果在回写成 `llm.Message` 后、provider call 前再执行一次 closure 校验，防止 compactor 的内部缺陷越过边界。

### 3. Summary strategy 与 summary payload

`assembleCompactor` 当前 small-model 注入只允许空值、`unified` 与 `micro_first`（`internal/cli/assemble.go:1427-1433`），遗漏显式 `summary`。保留 factory 为唯一 compactor 构建入口，只把有效 `summary` 加入注入条件；有 small model 时工厂生成的 `SummaryCompactor` 必须实际调用 `llmModelSummarizer`。无 small model 时继续显式失败或让 `unified` 按既有 fallback 降级，不能静默输出空摘要。

`buildSummaryPrompt` 当前只收录 read/write/edit/grep 结果；其它工具的 `ToolResult` 与 structured call metadata 丢失（`internal/compaction/summary.go:178-212`）。摘要 prompt 应按 chronological order 含用户/assistant 消息、已存在 compaction summary、每个工具组的工具名及 terminal outcome，并从文件、shell、搜索、web 与其它成功/失败工具结果中挑选有用文本。每条输出与总 prompt 都需用同一个 estimator 做确定性 token 上限；超长输出做明确的头尾截断并保留 `[truncated]` 标记，避免摘要模型请求被原始 tool logs 撑爆。不要把摘要输出变成工具调用，也不要把 raw tool arguments 当成摘要指令；若纳入 args，只允许经过已有 redaction/限长处理。

摘要仍通过既有 `maxSummaryTokens` clamp 限制输出大小。当前 `SummaryCompactor` 把 summary 生成为 `RoleSystem`（`internal/compaction/summary.go:148-163`），会把受用户/工具内容影响的派生文本放到高权限系统指令层；R2 已定恢复投影为固定 user-role 的 untrusted-context 包装（`InputKind` 留空，不生成普通 user row，UI 不显示成新请求），R3 的 provider 投影也必须使用同一安全语义。产出的 compacted `TurnItem` 必须保留 summary 的 compaction 标记与内容；Loop 将实际 compacted source prefix、summary 与输入 history revision 送入上述 `CompactionProjection`。`Through` 使用 run-start 的原始坐标：`PriorHistoryCount` 表示被覆盖的既存 history 前缀，`TranscriptCount` 表示被覆盖的当前 run delta 前缀；有 transcript 覆盖时 prior count 必须精确匹配 source prior 总数，多个 projection 继续使用该原始 provenance 坐标。R2 根据 `ExpectedHistoryRevision` 校验并映射成真实 `CompactedThroughEntryID` 后 durable commit。Agent 当前的 turn-end `CompactionHook` 不能替代这个 seam：它只看到 turn 之后的 `Agent.history`，而 Loop 请求前 compact 的 `messages` 只在本次 Run 局部存在；当 compaction 只因 tool schemas/output reserve 才触发时，turn-end hook 可能不触发，导致重启后无法恢复实际使用过的 summary。R3 不写 session entries、不自行另造摘要存储协议。

## 计划修改边界

| 文件 | 计划职责 |
|---|---|
| `internal/core/loop.go` | 每次 request 前调用 preflight；估算最终 provider message/options；budget/protocol 错误 fail closed；保留现有 run 与 provider 接口。 |
| `internal/compaction/compactor.go`、`token.go` | ToolCall metadata 估算、复用的完整组划分/closure 校验及 typed 错误。 |
| `internal/compaction/tool_groups.go`（新） | 共享的按 ToolCallID 原子分组/验证 helper；避免把组边界规则复制到不同 strategy。 |
| `internal/compaction/truncating.go`、`summary.go` | 只在完整组边界裁剪；有界并按组构造摘要输入。 |
| `internal/compaction/micro.go` | 若修改 result 后缓存失效，清理/更新 token cache；其它行为维持。 |
| `internal/cli/assemble.go` | 让显式 `summary` 接收配置 small-model summarizer；传递可证明的当前模型预算元数据。 |
| 对应 `*_test.go` | Loop 首请求 preflight、compaction group ID、真实 CLI summary strategy wiring、summary non-file output 与预算单测/集成测试。 |
| R1/R2→R3 canonical seam | 使用 R2 的 `CompactionProjection{ExpectedHistoryRevision, Through: CompactionCursor{PriorHistoryCount, TranscriptCount}, Summary}`；Loop 请求前实际替换 history 时产出，cursor 固定 run-start 原始坐标。R2 校验 revision 并映射至 append-only compaction entry 的 `CompactedThroughEntryID`；恢复后验证 boundary 后 tool groups 闭合。R1 transcript 本身保持仅事实 delta。 |
| `internal/session/*` | R3 不拥有源码修改；R2 完成 projection durable append、restore 与 writer/ACK 事务。R3 integration test 依赖 R2 可恢复摘要契约。 |

不新增 EventStore、通用 Runtime 抽象、第二套 loop、自动 tool replay 或第二份 session history。`memory` 的 `MEM-01/02` 搜索语义测试虽同列在 R3-BLD-03 execution row，但属于独立 memory 改动面，应由该任务负责人单独落地；不能让它们延迟或改变 compaction 接口。

## 验收与回归

必需测试沿用 execution task 中的名称，并补全结构断言：

- `TestContextBudgetCheckedBeforeFirstProviderCall`：用真实 CLI assembly、可记录 fake provider 和极小 budget，证明 compaction 在首个 provider call 之前运行；返回 request 估算不超上限、当前 user input 保留一次、没有 tool 执行。另构造仅因实际 tool-schema/output reserve 超限的 history，验证发出的 `CompactionProjection` 携带明确 source boundary/revision；mandatory prompt 超预算时 provider call 数为 0。R2 durable restore 集成需消费这同一 typed record，禁止从 events 重新推导。
- `TestCompactionKeepsToolCallResultTogether`：构造一个 assistant 请求两个 ID、result 大小不等且顺序相反的 fixture；所有 compactor 的输出要么保留两个 calls 和对应两个 receipts，要么将完整组全部转为 summary，禁止半组。缺失/重复/错误 ID fixture 必须在 provider call 前失败；额外证明另一个已闭合 group 和另一个 run 可合法复用同一 ToolCallID，以及 legacy missing ToolName 被唯一匹配后补齐、冲突 name 被拒绝。
- `TestSummaryUsesConfiguredModel`：通过 `AssembleAgent`/`assembleCompactor` 的实际 factory 装配，配置 `Compaction.Strategy: summary` 与 fake small model，证明确实调用该模型；无 small model 的降级行为也有独立断言。
- `TestSummaryIncludesNonFileToolOutput`：summary 输入同时含 file、bash/search/web 与 error receipt；验证相关非文件结果出现且超长输出/总输入被限制、ToolCallID 不会以 runnable call 形式进入 summary item。
- 在 R2 restore 集成测试中验证 compaction summary 与 `CompactedThroughEntryID` 边界重启后仍出现，边界后的 tool group 完整；R3 单元测试不伪造 SessionStore durability。
- 验证 CJK、小文本与序列化 tool arguments 的预算 fixture；测未过滤与过滤工具 schemas，确认预算只计实际 request 中的 schemas；覆盖不同 provider/image detail/未知图像尺寸，未知时验证显式 reserve/unknown 状态，不得把固定 85-token 当硬界。Estimator 错误必须采用既有 fallback 并留 observability，不得让请求超过已算出的本地预算。
- 对 `internal/compaction`、`internal/core`、`internal/cli` 运行精准测试与受影响包 race suite；保留旧 JSONL fixtures。任何测试需先确认 `go test -list` 有目标名，并先以编译成功且行为失败的红测证明缺陷。

若实现 provider overflow fallback，仅识别协议层明确的 context-limit error；最多一次 `CompactTriggered` + retry，且必须满足本 run 尚未 dispatch tools、没有 visible assistant delta、没有 transcript durable ACK。通用 provider/network error、已出现 partial visible output、tool intent/receipt/unknown 后都不 retry。若本实现未加此分支，preflight 不应被描述成能消除模型 tokenizer/serializer 偏差导致的所有 overflow。
