# Session、Compaction 与 Memory 源码审计

审计日期：2026-09-24  
范围：`internal/session`、`internal/compaction`、`internal/memory`，以及 CLI/Core 中把这些模块接入真实交互运行时的相关代码。  
审计方式：逐文件审查目标模块 production source，检查相关测试与 CLI/Core 接线；未运行仓库测试或全量构建/校验。另运行两个临时 `go run` 复现程序并清理源文件：session A→B 跨文件父节点复现输出 `old_leaf="entry-a" new_file_entries=1 recovered_leaf=""`；列出两个 session 后 `/resume session-a` 输出 `listed_old_session=true ... resume_error=session: resume failed: session: leaf entry not found`。文中“高”表示由代码控制流直接支持；复现结果为直接执行证据；不把静态判断冒充全量运行验证。

## 结论

Session 的 append-only JSONL、树形分支和 compaction 策略拆分方向合理，但当前 CLI 接线没有形成可恢复的 durable turn：会话文件切换顺序会制造孤儿记录，`/resume` 把文件 ID 当树节点 ID 使用，若 compaction entry 被持久化，恢复投影会丢掉其摘要（当前 CLI durable compaction writer 尚未接通）；正常交互中的用户输入、工具意图/结果也没有在副作用发生前持久化。存储追加/同步错误也不会阻止响应被输出。以上问题需要先于 UI 或新能力迭代解决。

建议保留现有 JSONL 与 session tree，将迭代重点放在单写者、明确的会话选择、严格 append/flush/sync 错误传播、可恢复的 turn/tool 记录协议，以及 provider 请求前的完整 tool-call/result 校验。暂不需要把整个模块替换为新的通用 EventStore。

## 稳定发现

### SES-01 · P1 — 新会话沿用旧树叶节点，导致新 JSONL 文件记录成为孤儿

- **类别 / 证据：** 已确认接线缺陷；源码链条高置信度，并由临时最小复现验证。
- **位置：** [`repl_session.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:289)、[`jsonl.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/jsonl.go:164)、[`repl_session.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:905)、[`tree_builder.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/tree_builder.go:84)。
- **触发：** directory-mode 下已有会话，启动一个新会话。
- **证据链：** `setupSlashContext` 先用当前 `SessionStore` 构建 `sessionTree`（`repl_session.go:289-296`），然后才调用 `SetSessionID(..., newSession=true)`（`311-314`）。该调用关闭旧文件并清空 store entry map、创建新文件（`jsonl.go:164-190`），但不替换 CLI 已建的树。之后 `persistSession` 把新 user entry 的 `ParentID` 设为树当前 leaf（`repl_session.go:905-924`）。新文件中没有这个旧 leaf；重启构树时 `tree.Append` 对缺失 parent 返回错误，builder 静默跳过该记录（`tree_builder.go:84-89`）。临时复现执行结果：`old_leaf="entry-a" new_file_entries=1 recovered_leaf=""`，临时程序已删除。
- **后果：** 新会话的输入和回答可能写入 JSONL，却在重启后整条新会话分支不可见/不可恢复；builder 的“跳过坏记录”又让故障看起来像空会话。
- **验收：** 在同一 directory store 中先创建 A，再启动 B；确认 B 的第一条记录没有跨文件 ParentID，进程重启后 B 的全部条目可构树且成为当前会话。确保 A 仍可通过显式 resume 选择。
- **最小修复：** 先选定/创建 session 文件，再针对该文件加载或初始化树；新 session 从空树开始。禁止在文件边界之间复用 entry parent。

### SES-02 · P1 — `/resume` 显示的是文件会话 ID，却按 entry ID 移动当前树

- **类别 / 证据：** 已确认接线缺陷；静态高置信度。
- **位置：** [`slash_commands.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/slash_commands.go:176)、[`slash_commands.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/slash_commands.go:206)、[`repl_session.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:289)。
- **触发：** `/resume` 不带参数列出历史文件，再按提示执行 `/resume <session-id>`。
- **证据链：** 列表由 `SessionLister.ListSessions` 返回的 `SessionMeta.ID` 组成，并提示传入 session-id（`slash_commands.go:206-236`）；handler 随后把该字符串原样传给 `tree.MoveTo`（`176-190`），而 `MoveTo` 只接受已经载入 tree 的 entry ID（`tree.go:117-129`）。CLI 只从当前 Store 内容构建一棵树（`repl_session.go:289-296`），handler 没有按 SessionMeta 选择文件、切换 Store、重建树的路径。临时复现执行结果：已列出的旧 ID `session-a` 在当前树上返回 `session: resume failed: session: leaf entry not found`，当前文件仍是 session B；临时程序已删除。
- **后果：** 列表承诺的跨文件恢复不可用；通常报 `ErrLeafNotFound`，偶然 ID 重合时也会错误地恢复当前树节点，而不是指定文件。
- **验收：** 创建至少两个 session 文件，列出后选择较旧 session ID，验证 store 路径、session ID、entry 树、agent history 全部对应所选文件。entry/branch ID 的树内跳转应有不同参数语义。
- **最小修复：** 把 `SelectSessionFile(sessionID)` 与 `MoveTo(entryID)` 分成两个明确操作；选文件后关闭/切换/锁定目标文件，再加载目标文件构树并恢复 history。

### SES-03 · P2 — 持久化 compaction entry 恢复后摘要被 Agent history 投影丢弃

- **类别 / 证据：** 已确认的恢复投影缺陷；静态高置信度，但当前 CLI compaction durable wiring 未接通，因此不是“普通 `/compact` 后重启必然丢已落盘摘要”。
- **位置：** [`context.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/context.go:119)、[`context.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/context.go:192)、[`repl_session.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:304)、[`derive_test.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/derive_test.go:121)。
- **触发：** store/tree 中存在 `EntryTypeCompaction`，之后经 CLI resume/fork 恢复。
- **证据链：** `BuildContext` 把 `EntryTypeCompaction.Summary` 转成 `SessionContext.Messages` 中的 compaction entry（`context.go:119-130`）；CLI `/resume` 将该消息列表传给 `EntriesToAgentMessages`（`repl_session.go:304-306`），fork 也走同一转换（`prompt.go:298-305`）；命令行 `--resume` 则由 `loadSessionHistory` 仅投影 user/assistant entry（`interactive.go:124-153`）；转换器 default 分支跳过 `EntryTypeCompaction`（`context.go:192-219`）。测试 `derive_test.go:121-149` 断言压缩 entry 不输出。另一方面，全仓生产代码没有创建 `EntryTypeCompaction` entry 的调用点，当前 `persistSession` 只持久化 user 与最终 assistant（`repl_session.go:894-957`），说明压缩 durable 写入本身尚未接通。
- **后果：** 一旦 compaction 摘要进入持久化 session，恢复投影会丢失唯一替代旧历史的摘要。当前功能状态应标为“持久化恢复链不完整”，不能宣称普通 compaction 已形成此持久化损失。
- **验收：** 当 durable writer 接入后，含压缩摘要的 branch 重启恢复时，Agent history 中摘要恰好出现一次，且保留压缩边界之后的完整消息；用真实 provider message 约束校验 role/order。
- **最小修复：** 明确摘要的模型投影形式（system/developer 或专门标记的 user-visible summary），并更新当前测试语义；不要把 compaction entry 无条件过滤。

### SES-04 · P1 — 持久化发生在 turn 完成后，工具意图/结果不进入 SessionStore

- **类别 / 证据：** 已确认恢复能力缺口；源码直接支持，高置信度。
- **位置：** [`repl_session.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:617)、[`repl_session.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:894)、[`loop.go`](/Users/pengjunchen/Documents/github/go-cli/internal/core/loop.go:463)、[`agent.go`](/Users/pengjunchen/Documents/github/go-cli/internal/core/agent.go:133)。
- **触发：** 模型请求/工具执行期间进程退出、崩溃，或用户中断一个已产生工具副作用的 turn。
- **证据链：** REPL 在输出最终回答或显示中断之后才调用 `persistSession`（`repl_session.go:617-640`）；非中断 turn 若 `turnErr` 存在，会在 `617-624` 提前返回，也不保存 user entry。该函数仅入队 user entry 和可选最终 assistant 文本，不写 assistant `ToolCalls`、tool args/result、compaction 标记或执行阶段（`894-957`）。Core loop 产生并分派 tool calls（`loop.go:463-507`），但 Agent history 追加的是 assistant message（`agent.go:133-153`），工具执行的恢复状态未进入该持久化路径。
- **后果：** 崩溃后丢失本轮 user 请求；如果工具已改变文件或外部状态，没有 durable 意图/结果记录判断是否执行过。历史不完整且无法安全自动重试。
- **验收：** kill-point 验证：provider 请求之前输入已落盘；每个外部工具 side effect 前其 intent 已落盘；执行后匹配的 result 已落盘；恢复时未知结果明确标成 outcome-unknown 且绝不自动重放；所有 provider 请求拒绝含未闭合 tool calls 的 history。
- **最小修复：** 在现有 JSONL 中追加 typed entries/状态字段：user input → assistant tool intent → tool result（精确 ToolCallID）→ final assistant。把记录与执行串行化，先 append+flush+sync 成功再开始下游阶段。

### SES-05 · P1 — Session append/flush/sync 失败被吞掉，已输出的回答无法保证可恢复

- **类别 / 证据：** 已确认错误传播缺陷；静态高置信度。
- **位置：** [`repl_session.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:635)、[`repl_session.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:918)、[`repl_session.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:949)、[`jsonl.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/jsonl.go:376)。
- **触发：** store 写入、buffer flush 或 Save 失败（磁盘满、I/O 错误等）。
- **证据链：** REPL 先把回答输出，再调用 `persistSession`（`635-640`）；`persistSession` 先 enqueue 并将 tree 前移（`918-947`），flush 出错只 Warn（`951-953`），Save 的错误直接丢弃（`955`），函数无错误返回值。store close 时也忽略 buffered writer Flush 错误且未 `Sync`（`jsonl.go:376-387`）。
- **后果：** 非 TTY 最终回答已输出，TTY 已呈现流式内容，但失败后未标明未保存；store/tree 仍可能不一致，后续子项 parent 可能引用未持久化节点，形成孤儿。Buffered flush 成功也不等于掉电持久化。
- **验收：** 注入 Append、Flush、Sync 失败；错误返回到 turn 状态且不输出“完成”；durable tree 仅在 JSONL append+flush+sync 成功后推进；重试不重复写入或丢失后缀。
- **最小修复：** `persistSession` 返回 error；将 store append 和 sync 作为运行时阶段的成功门槛；同步错误不吞并，不能 ACK 当前阶段或继续触发依赖它的 provider/tool 操作。

### SES-06 · P1 — JSONL 尾部 torn write 后，新追加记录与坏尾拼接并一起丢失

- **类别 / 证据：** 源码确定的恢复缺陷；崩溃触发本身未实测，中高置信度。
- **位置：** [`jsonl.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/jsonl.go:488)、[`jsonl.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/jsonl.go:500)、[`jsonl.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/jsonl.go:524)。
- **触发：** 进程/机器在 JSON encoder 写完最后一条 JSON、但换行未完整落盘时中断；随后重新打开文件并 Append。
- **证据链：** loader 对每一行 `json.Unmarshal` 失败后只跳过该行并继续（`jsonl.go:503-517`）；打开文件使用 `O_APPEND`，追加 encoder 不检查尾部是否以换行结束，也不修复尾部（`jsonl.go:423-430`、`524-530`）。因此后续有效 JSON 会直接接在不完整 JSON 后，扫描器将二者作为同一坏行跳过。
- **后果：** 撕裂点及其后的有效 session 记录可能永久不可见；日志告警而非 fail-closed 会继续运行，扩大损坏范围。
- **验收：** 构造无 newline 的不完整 JSON 尾，重启打开并 append；恢复只保留完整前缀，修复/隔离坏尾后新记录必须单独可读；不能把中间损坏静默解释成成功恢复。
- **最小修复：** 获得该文件 single-writer lock 后检查最后 newline 后的尾缀：完整合法 JSON 仅补 newline；仅当解析能证明是被截断的 JSON 时才截除该尾缀并记录恢复告警；完整但 malformed/语义无效的记录 fail closed 或隔离，不能按普通 torn tail 丢弃。每个 checkpoint 显式 Flush + `file.Sync()`，需要时同步父目录元数据。

### CMP-03 · P1 — 长上下文首个 provider 请求前不做预算检查，overflow 直接结束 turn

- **类别 / 证据：** 已确认的运行时缺口；静态高置信度。
- **位置：** [`loop.go`](/Users/pengjunchen/Documents/github/go-cli/internal/core/loop.go:438)、[`loop.go`](/Users/pengjunchen/Documents/github/go-cli/internal/core/loop.go:562)、[`loop.go`](/Users/pengjunchen/Documents/github/go-cli/internal/core/loop.go:713)、[`agent.go`](/Users/pengjunchen/Documents/github/go-cli/internal/core/agent.go:156)、[`midturn.go`](/Users/pengjunchen/Documents/github/go-cli/internal/compaction/midturn.go:11)。
- **触发：** 恢复的 history 已超出 provider context window，或 system prompt/tool schema 加入后首次请求超限。
- **证据链：** `LoopAgent.Run` 组装 system prompt、全量 history 与本轮 user message 后直接进入 `generateWithContinuation`（`loop.go:438-480, 542-567`）；threshold compaction 只在一轮 tool 执行之后才检查（`713-730`）。当前 item 估算覆盖 Content、ToolResult、ContentBlocks，但不计结构化 ToolCalls/args（`compactor.go:121-148`）；可见的 tool schemas 作为 `iterOpts` 单独传给模型（`loop.go:542-560`），不进入该估算。Agent 的 compaction hook 在 `Run` 返回之后执行（`agent.go:156-170`），不能挽救失败的首请求。`TriggerOverflow` 枚举存在，但生成错误分支直接返回，没有观察 overflow 后调用 `CompactTriggered` 重试（`midturn.go:22-24`, `loop.go:562-567`）。
- **后果：** 长会话达到窗口上限后，模型报错就终止；下一次 turn 仍使用同样未压缩的 history，形成可重复失败。按 token 字符启发式可能低估 tool-call args、tool schema 与输出预算。
- **验收：** 首请求前对有效总输入预算做 preflight；构造 provider context overflow 错误，验证在未执行任何 tool 的前提下最多进行一次 compact+retry；工具副作用发生后不得因 overflow 自动重跑 turn。
- **最小修复：** 在 agent/loop 请求前统一做 context preflight；错误分类后仅对可安全重试的“尚未执行工具”阶段执行一次 compact+retry，并记录 overflow trigger。

### CMP-01 · P1 — Compaction 的截断/摘要逻辑没有按 ToolCallID 保持调用与结果成组

- **类别 / 证据：** 已确认不变量缺失；中高置信度（取决于 provider 对 malformed tool history 的处理）。
- **位置：** [`truncating.go`](/Users/pengjunchen/Documents/github/go-cli/internal/compaction/truncating.go:53)、[`truncating.go`](/Users/pengjunchen/Documents/github/go-cli/internal/compaction/truncating.go:85)、[`summary.go`](/Users/pengjunchen/Documents/github/go-cli/internal/compaction/summary.go:79)、[`context.go`](/Users/pengjunchen/Documents/github/go-cli/internal/session/context.go:192)。
- **触发：** 一个较大的 tool result 超出截断预算，或 truncation/summarization 切点落在一组 tool calls 之间；恢复历史中包含 assistant tool calls。
- **证据链：** `TruncatingCompactor` 按单个 `TurnItem` 从最新消息倒序择取，然后只调用 `removeDanglingToolResults`（`truncating.go:53-69`）；该函数只看邻接角色是否为 assistant/tool，不核对 `ToolCallID`、assistant 中每一个 call 是否有 result（`85-96`）。Summary 的切点只避开 `RoleTool` 行（`summary.go:79-106`）；session projection 跳过 tool entry 但保留 assistant `ToolCalls`（`context.go:196-216`）。
- **后果：** provider 收到悬空 tool call、没有来源的 tool result 或错误 ID 配对；某些供应商拒绝请求，另一些会产生语义偏差。
- **验收：** 用一个 assistant 发多个 calls、结果大小不同、结果顺序乱序、缺少一个结果及错误 ID 的 fixture；所有 compactor 输出以及恢复/发 provider 前校验必须只包含闭合且 ID 一致的 call/result 组。
- **最小修复：** 以完整 assistant-tool-result turn bundle 为 compaction 原子边界；provider 请求前按 ToolCallID 建立 outstanding set 并 fail closed。

### CMP-02 · P1 — 配置 `summary` strategy 时小模型 summarizer 未接入

- **类别 / 证据：** 高置信度配置缺陷，静态推断。
- **位置：** [`assemble.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:1414)、[`assemble.go`](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:1423)、[`summary.go`](/Users/pengjunchen/Documents/github/go-cli/internal/compaction/summary.go:63)。
- **触发：** 配置 `Compaction.Strategy: summary` 且 small model 已配置。
- **证据链：** summarizer 注入条件只允许空值、`unified`、`micro_first`（`assemble.go:1423-1427`），但 strategy 会原样交给 factory（`1430`）；SummaryCompactor 收到 nil summarizer 会换成必然返回错误的 `noopSummarizer`（`summary.go:63-76`）。
- **后果：** 显式选择摘要压缩时没有可用摘要模型；压缩失败或被路由到 fallback，策略行为与用户配置不符。
- **验收：** 对所有有效 strategy 做表驱动组装测试，验证 `summary` 会收到 small model summarizer，且无小模型时错误/降级清晰可观测。
- **最小修复：** summarizer 注入条件包含 `summary`；factory 维持唯一 compactor 构建入口。

### CMP-04 · P2 — SummaryCompactor 摘要输入忽略非文件工具结果

- **类别 / 证据：** 摘要质量不完整；静态高置信度。
- **位置：** [`summary.go`](/Users/pengjunchen/Documents/github/go-cli/internal/compaction/summary.go:176)、[`loop.go`](/Users/pengjunchen/Documents/github/go-cli/internal/core/loop.go:744)。
- **触发：** Unified/summary strategy 摘要包含 web/search/test/其他非文件工具结果的较早历史。
- **证据链：** core adapter 将 tool message 的内容放进 `TurnItem.ToolResult`、将 tool name 放入 `ToolName`（`loop.go:744-760`）。`buildSummaryPrompt` 只把满足 `isFileOp(ToolName)` 的 ToolResult 加到摘要 prompt 的 `[file operations]` 区；其他 ToolResult 被忽略，ToolCall identity/args 也未被序列化（`summary.go:176-212`）。
- **后果：** 旧 search/test/MCP 结果中未重复到普通 assistant 文本的事实不会进入 summary，压缩后模型可能遗失刚验证过的事实或决策依据。
- **验收：** 含 search、test、MCP 与 file tools 的对话分别构造关键结果只存在 tool result 的 fixture；summary 必须保留后续任务所需结论或带可引用的精简结果。
- **最小修复：** 为所有工具结果生成有来源/ToolCallID 的摘要输入；文件操作可以加结构化摘要，但不能作为唯一被保留的工具输出。

## 其他不完善点

- **SES-07 · P2 — 目录模式新 store 直接 Append 不可用：** `jsonl.go:412-417` 在无 session 文件时标记 loaded、无 writer；`Append` 最终到 `writeJSONLine` 返回 `store writer is not open`（`526-529`）。注释声称 Append 会创建文件但代码路径没有做。API 使用方若未显式 `SetSessionID` 会失败；当前 CLI 显式 SetSessionID，因此需结合接口使用范围定优先级。
- **SES-08 · P2 — session 文件读取与 flock 顺序不安全：** `ensureLoaded` 先 `loadEntriesLocked` 再打开并获取 flock（`jsonl.go:420-430`）。第二个进程可能读到锁持有者尚未完成/尚未 flush 的陈旧映射；flock 只约束写入文件句柄，不会刷新另一个进程已加载的 map。需贯彻单写者；避免把并发进程误当多写者安全。
- **SES-09 · P2 — Tree 返回值及 clone 未完全防御性：** `GetBranch` 释放 RLock 后返回内部 entry 指针（`tree.go:250-268`），与 `SessionEntry` immutable/defensive copy 契约不符；`SessionEntry.clone` 只复制 `ToolCalls` slice（`types.go:82-105`），`llm.ToolCall.Args` 为 `any`，其 map/slice 嵌套会共享。调用方可能绕过锁修改 tree，或产生数据竞态。
- **SES-10 · P2 — Tree Seq 未回写调用方/持久化副本：** tree append 将 `Seq` 写在内部 clone 上（`tree.go:79-84`）；REPL 先把零 Seq entry 放入 pending buffer，再 Append tree（`repl_session.go:918-923`、`939-944`），所以 JSONL 中 Seq 为 0。Tree builder 以 Timestamp 选择当前 leaf（`tree_builder.go:90-99`），时间相同时没有稳定 tie-break；active leaf 没单独落盘。
- **SES-11 · P2 — 分支/快照/迁移部件未进入生产装配：** `SetBranchStore`、snapshot store、migration chain 均有实现/测试，但当前 `internal/cli` 未发现生产构造或 wiring（初步 `rg`：`SetBranchStore`/`NewJSONLBranchStore` 只出现在 session 源码和测试；snapshot/migration 构造只在源码和测试）。`/fork` 元数据跨进程持久化、branch selection、snapshot 与实际 session JSONL 的关系需核实；标为未接线，不把纯 dormant capability 冒充已运行 bug。
- **MEM-01 · P2 — Memory search 小库 TF-IDF 返回空：** `file_store.go:255-285` 的 IDF 是 `ln(N/(1+df))`，一个文档且命中时为负，两个文档且只命中一篇时为零，而结果只接受正分（`273-285`），所以这些常见小库查不到本来存在的 memory。测试数据规模从 3+ 起，未覆盖 N=1/2。建议检索 IDF 改为非负平滑公式并增加边界测试。
- **MEM-02 · P2 — Memory 中文 tokenizer 不支持子词匹配：** `file_store.go:409-425` 将连续汉字作为一个 token；查询中文句子中的子串不会命中。未发现 CJK 检索测试。
- **MEM-03 · P2 — Memory retrieval 没有注入 Agent context：** `MemoryRetriever` 目前是接口定义，`rg` 未发现生产实现/接线；CLI 当前有抽取写入、`/memory` 手工搜索，但无每轮 prompt 自动检索（`repl_session.go:968-1001`、`slash_memory.go:135`）。跨 session continuity 目标尚未闭环。
- **SES-12 · P2 — Pending savepoint 是一次性位置而非稳定序号：** `FlushToSavepoint` 保存的是当时 `pending` 的 slice index（`pending_writes.go:74-82`），flush 删除前缀后若复用旧 savepoint，会切到错误写集合；现有使用点只在测试/接口，当前 CLI 未使用该 API。
- **SES-13 · P2 — session 文件名只有秒精度，快速新建会话会复用文件：** timestamp format 为 `20060102-150405`（`jsonl.go:35-37`），交互 session ID 默认恒为 `interactive`（`repl_session.go:259-264`、`assemble.go:497-500`）。新会话用 `O_CREATE|O_WRONLY|O_APPEND` 创建，不用 `O_EXCL` 或更高精度/随机文件名（`jsonl.go:170-190`），因此同一秒创建两次 session 会打开相同文件并续写，两个逻辑会话合并。临时复现输出 `same_path=true ... reopened_entries=2`；源码与复现证据在 `evidence/session-same-second-collision-repro.{go.txt,log}`。验收：同一秒/同一 session ID 新建两次，得到不同物理文件且每文件仅含其本 session entry。
- **SES-14 · P2 — JSONL append 允许超过 1 MiB 的 entry，但 reopen 读取失败：** writer 对 entry 没有大小上限（`jsonl.go:524-530`），loader 的 `bufio.Scanner` 最大行只有 1 MiB（`jsonl.go:21-23,500-519`）。一条合法的大用户输入/assistant 内容 append 成功后，重启 `Open` 返回 scanner too long 错误；CLI `assembleSession` 会将 store 置 nil（`assemble.go:1457-1461`），该 session 后续无法正常恢复/继续保存。验收：边界长度（1 MiB-1/1 MiB/1 MiB+1）存取一致；若保留限制，Append 必须先拒绝并向调用方传播。
- **MEM-05 · P3 — Memory extractor 解析失败时将原始模型输出写入 warning log：** `extractor.go:83-91` 把 `resp.Content` 原文放入日志字段。抽取内容来自用户对话上下文，解析失败时日志系统会额外留存原始内容。验收：日志不含原始对话/模型回复，仅保留错误类型与安全摘要。
- **MEM-04 · P3 — Memory Store 文件原子性有限：** `FileMemoryStore.Add` append 单行 JSONL 后更新内存索引，但不调用 `Sync`（`file_store.go:133-144`）；Delete 通过同目录 temp+rename 重写文件（`316-369`），rename 已成功而 reopen 失败时上层会回滚内存删除（`file_store.go:194-203`），造成内存与磁盘状态不一致。属于低优先级故障边界，需注入 reopen 错误验收。

## 合理的设计与现有基础

- JSONL 每条记录独立编码，SessionEntry 以 ParentID 建树；append-only 记录与树投影可保留，适合用小步改造实现 durable turn。
- `JSONLSessionStore` 有 advisory file lock，`SessionTree.Append` 校验 ID/ParentID/EntryType，context 通过 parent chain 重建；核心骨架具备恢复所需的原语，但锁时序、跨文件选择与错误传播未闭合。
- Compaction 采用 Micro、Summary、Truncating 分层及 Unified fallback，职责拆分清晰；summary 通过 `findCutPoint` 避免从 tool-result 行开始保留。短文本 tokenizer 可用 rune 级估计，长输入提供 composite fast path；需改进消息组边界、实际模型 token 预算与策略装配。
- Memory store 暴露简洁的 Add/Get/Search/Delete/Close，带本地文件读写锁、JSON 解码失败返回错误并在读入时重建索引；MemoryExtractor 使用显式接口，可独立测试。

## Runtime 最小迭代建议（保留 JSONL/session tree）

### R0：会话选择、单写者与日志完整性

1. 把 session 文件选择/创建提升为启动第一步，再针对唯一当前文件加载 entry map 与 tree；新文件使用空 tree；文件名使用足够唯一的随机 ID/高精度时间及排他创建；resume 先按文件 session ID 选文件，再按 entry ID 选择 branch leaf。
2. 对当前 JSONL 文件实行 single writer：进程获得排他锁后才能加载、校验尾部、恢复 tree；并发实例不能各持有 stale entries map 后交错 append。暂不需要跨文件共享一个新 EventStore。
3. 修复尾部 torn JSON：锁内检查最后 newline 之后的尾缀；完整合法 JSON 但缺分隔 newline 时补 newline 保留 entry，能证明被截断的 JSON 才修复/移除尾缀，完整但 malformed 的记录 fail closed/隔离；对中间 corrupt line 采用清晰 fail-closed/隔离语义，避免静默跳过后继续写。
4. 定义 checkpoint 成功为 append → buffered flush → file.Sync 成功；任何错误必须穿过 SessionStore、REPL turn 状态，tree 只能在 durable append 成功后投影推进。保留最终 close 的错误并报告。

### R1：现有 JSONL 上的 durable turn/tool 协议

1. 记录 user input 后才能发送 provider request；失败则向用户返回明确 persistence error，不将其报告为成功 turn。
2. 每条 assistant tool intent（完整 ToolCallID、name、args）在调用工具之前持久化；工具完成后追加带同一 ID 的结果。单 writer 按 entry 顺序更新 tree。
3. 恢复时按 ToolCallID 解析 outstanding calls。没有结果意味着 outcome unknown，绝不能自动再执行；把显式 unknown result 放进可继续的合法模型 history，或者要求用户决定。只有结果明确且闭合的对话才发 provider request。
4. stream token 可以继续展示，但 durable checkpoint 失败后 UI 必须明确标记“未保存/turn failed”，不得显示持久化成功的完成状态；写盘失败时取消尚未开始的后续副作用。
5. 对已完成 turn 写最后 assistant message；interrupted turn 保留已经 durable 的阶段记录和取消原因。

### R2：模型上下文与 compaction 恢复闭环

1. 把 compaction summary 做成明确定义且可恢复的模型消息；压缩时同步持久化摘要与被替代 entry 边界，确保重启投影结果等价。
2. 用完整 call/result bundle 作为压缩/裁剪单位；provider request 前统一校验所有 ToolCallID 的闭合性和顺序语义。
3. 首个 provider request 前做 context budget preflight（包括 system/developer prompt、tool schema 与预留 completion tokens），不能只在工具执行后才 compact。
4. 正确接入 `summary` strategy；对 provider overflow 做可分类的单次安全压缩重试，且只有在尚未执行本 turn tools 时自动 retry。工具副作用未知时禁止重跑整 turn。
5. 明确恢复 active leaf、branch metadata、sequence/order 与 schema migration 策略；先接好现有分支 store，或将未接线接口标为未支持。

### Runtime 验收不变量

- 每次 provider request 的可见历史都有完整且精确匹配的 ToolCallID/tool-result pairs。
- 任一外部副作用开始前存在同步落盘的 intent；未确认的 in-flight tool 恢复为 outcome unknown，绝不自动重放。
- user input 与最终 ACK 有可追踪的持久化阶段记录；append/flush/sync 错误不能静默通过。
- 重启后通过确定 session file + active leaf 重建相同分支；compaction 后重启保留 summary 与压缩后的上下文语义。
- 尾部撕裂可安全恢复；写入者唯一，读者看到的数据与当前锁定文件一致。

## 覆盖状态与待补项

本稿没有运行仓库测试，因此验收项仍是待实现的回归标准；两项 `go run` 是临时集成复现，不是单元/集成测试。下面列出审计范围与实际覆盖，不把文件存在误当成测试覆盖。

### 已审阅的生产源码

- Session：`internal/session/{types.go,store.go,jsonl.go,tree.go,tree_builder.go,context.go,derive.go,branch.go,branch_summary.go,snapshot.go,slash_commands.go,pending_writes.go,queue.go,migration.go,flock_unix.go,flock_other.go}`。
- Compaction：`internal/compaction/{compactor.go,factory.go,micro.go,summary.go,quality.go,truncating.go,unified.go,midturn.go,split_turn.go,token.go}`。
- Memory：`internal/memory/{memory.go,extractor.go,file_store.go}`。
- 真实接线与消息契约：`internal/cli/{assemble.go,interactive.go,compaction_adapter.go,prompt.go,repl_session.go,slash_memory.go}`、`internal/core/{agent.go,agent_compaction.go,loop.go,turn.go,turnrunner.go,types.go}`、`internal/llm/llm.go`。另用 `rg` 全仓检索 `EntryTypeCompaction`、branch/snapshot/migration 构造和 MemoryRetriever/Search 注入调用点。临时复现源码、命令与日志：`evidence/session-new-file-orphan-repro.go.txt` / `evidence/session-new-file-orphan-repro.log`、`evidence/session-resume-file-id-repro.go.txt` / `evidence/session-resume-file-id-repro.log`、`evidence/session-same-second-collision-repro.go.txt` / `evidence/session-same-second-collision-repro.log`，重跑命令见 `evidence/session-repros.cmd.txt`。

### 测试文件清单与实际覆盖

- Session 测试文件：`internal/session/{branch_persistence_test.go,branch_summary_test.go,branch_test.go,clone_deep_test.go,clone_test.go,clone_toolcall_test.go,context_compaction_test.go,context_test.go,context_tokens_test.go,derive_test.go,extra_test.go,extra2_test.go,jsonl_corrupt_test.go,jsonl_test.go,migration_test.go,pending_writes_test.go,queue_test.go,slash_commands_test.go,snapshot_test.go,store_test.go,tree_builder_test.go,tree_test.go,bench_test.go}`。
- Compaction 测试文件：`internal/compaction/{compactor_test.go,factory_test.go,helpers_test.go,micro_test.go,midturn_test.go,quality_test.go,split_turn_test.go,summary_test.go,token_fast_test.go,token_test.go,truncating_test.go,unified_test.go,bench_test.go}`。
- Memory 测试文件：`internal/memory/{extractor_test.go,file_store_test.go,e2e_test.go}`。
- 已抽查的关键断言：`derive_test.go:121-149` 保留 assistant tool calls、跳过 tool results；`jsonl_corrupt_test.go:17-51` 只覆盖带 newline 的完整坏行；`truncating_test.go:182-235` 只按相邻角色断言 tool result，未断言 ID closure；`file_store_test.go:197-249,317-329` 覆盖 N=3/4 与有意不返回零分 common term；未覆盖 N=1/2 或 CJK 分词。

### 明确的测试缺口

1. 新文件先选后建树；新 session 不引用旧文件 ParentID；重启恢复同一会话。
2. `/resume` 按 SessionMeta ID 切换并恢复指定文件；session ID 与 entry/branch ID 的类型边界。
3. last-line 缺 newline 的完整 JSON、被截断 JSON、完整 malformed JSON 和中段损坏分别恢复；确保合法工具 receipt 不丢。
4. 多进程读者先锁再加载或明确单写者；陈旧 cache、进程锁等待后的重载与重复 ID。
5. append/flush/sync 失败的注入、tree 更新和 UI “未保存”状态；turn 中断和 process kill 点。
6. user→assistant tool intent→tool result→final assistant 的 durable kill-point；不自动重放 outcome unknown；provider request 前所有 ToolCallID 完整匹配。
7. compaction summary 实际写入 JSONL 后 resume/fork 的恢复；tool-call/result bundle 截断；首请求 overflow preflight 与安全重试。
8. strategy=`summary` 的 small-model wiring；估算中包含系统 prompt、tool schemas 与 completion reserve。
9. `GetBranch` 返回值与嵌套 ToolCall.Args 的隔离；Seq 持久化和相同 Timestamp 的 deterministic active-leaf 选择；savepoint 复用。
10. Memory N=1/N=2、CJK substring query、跨 session 自动 retrieval wiring、抽取失败时的敏感日志处理；JSONL Add 的 sync 失败与 Delete rename 后 reopen 失败。
11. 同秒 session filename collision 与 JSONL 单行 1 MiB 上限边界。

### 不应被报告为已验证的事项

- 没跑 `go test`、race、AST scan、make check/verify 或 provider/tool E2E；源代码覆盖与测试已通过是两种证据。
- compaction summary 的 session-entry 恢复问题目前是接口/投影层缺陷；当前 CLI 尚无 compaction entry 的持久写入调用点。
- `SetBranchStore`、snapshot 与 migration 缺少 CLI wiring 是能力未接通，不代表这些独立组件的实现/单测失败。
