# R5 TUI 交互与 Transcript 设计

状态：designer handoff draft，待独立 reviewer 审查后交 builder。本文定义 R5 的交互与验收合同，不代表功能已实现或验收通过。

依据：`docs/audits/2026-09-24/05-ui-review.md`、`docs/audits/2026-09-24/10-runtime-iteration-plan.md`、当前 `docs/iterations/2026-09-25-runtime/execution-tasks.json` 的 R5 任务，以及当前 `internal/tui`、`internal/cli` interactive/REPL 源码。执行任务文件是验收项和角色边界的直接依据；05 是现状证据；10 是 R5/R8 切分依据。R1 合同提供运行状态和规范 transcript 边界，R2 任务提供 session/branch/entry 身份及恢复边界，R4 决定工具能力与授权作用域。

## 目标和范围

R5 为默认本地交互 CLI 建立一个可信的会话视图：用户能看到自己提交的输入、Agent 和工具做了什么、哪些交互正等待操作，以及本次运行是否停止、结果是否持久化。所有可见状态都必须从对应 Runtime/Session/Interaction 身份投影，TUI 不创建自己的运行结果或历史事实。

当前默认路径是 `DefaultLineEditor.ReadLine` 收集一条输入，`REPLSession.executeTurn` 为每轮创建 EventStream、启动 `TurnRunner.RunTurn`、经 `BridgeEvents` 建立 `BubbleteaApp` 并运行真实 `teaModel.Update`。`renderTUI` 每轮重建 app；会话 ID 和审批/取消/steer 回调已传入。LineEditor 位于 TUI 外部，运行中的 steer/follow-up 是另一套较简化的单行输入。R5 可以调整 REPL/TUI 的 projection seam，但不把这两套输入描述成已经统一的编辑器。

R5 覆盖执行任务中的 UI-0/1/2：HITL/审批、消息与工具 transcript、队列与取消状态、会话 branch 投影、权限说明、事件背压和快捷键帮助。R5 不拥有 Runtime outcome/cleanup 语义、durable session schema、工具授权策略或 `/resume` 的底层恢复修复；它们分别由 R1/R2/R4 决定。R5 builder 需消费经 review 的合同，不能在 TUI 内补造这些规则。

R8 拥有通用 transcript viewport、历史搜索、多行/外部编辑器、可配置键位、ANSI/Unicode/CJK cell-width 正确性、长 transcript 增量性能和平台/无障碍矩阵。R5 为 approval 弹层做必要的有界详情区与固定操作行，以免长参数/差异把决策键挤出屏幕；这不是 R5 完成通用 transcript viewport 的声明。R5 仅修正执行任务明列的 steer caret 插入位置行为，不承诺 R8 的完整编辑器功能。

Plan Mode 退出仍受 `r4-policy-design.md` 中未决 D-01 用户决策约束。TUI 不新增退出快捷键，也不自行选择退出策略。待最终决策前，若收到退出确认交互，界面应明确显示“退出请求待确认”和当前仍生效的 Plan Mode 状态；决策落定后，界面严格反映 R4 最终合同中的状态变更和授权范围。未确认时不得显示为已退出或允许更改操作。

## 身份与数据来源合同

TUI 把 session history、运行时观察事件、交互请求、输入队列建模为不同来源。稳定标识来自 R1/R2/交互边界，不从显示位置、时间戳、工具名或参数哈希推导主键。

| 可见对象 | 身份键 | 权威来源 | R5 投影要求 |
|---|---|---|---|
| 会话/当前分支 | R2 定义的 `SessionID`、分支/当前叶身份 | SessionTree/JSONL 恢复结果 | 加载哪个 branch 就显示哪个；分支切换或恢复后不能混入旧分支行。使用 R2 的正式字段，不在 TUI 发明 ID 转换。 |
| 持久 transcript entry | `SessionID + BranchID + EntryID` | R2 durable store 与 R1 typed canonical transcript | 用户、assistant、tool receipt、steer/follow-up 按 canonical 顺序各出现一次；行可标为已保存的前提是 store 确认了对应记录。UI 以 durable `EntryType` 投影行类型，不从 provider role 或 EventStream 推断。 |
| compaction marker | `SessionID + BranchID + EntryID`，`EntryTypeCompaction` | R2 durable compaction entry；provider `SessionView.Messages` 是独立的有效模型上下文投影 | 显示为压缩摘要/历史边界，保留 durable entry identity；summary 不能画成新提交的 user prompt。Provider 恢复可将同一 summary 投影为带 untrusted-summary 前缀的 `Role="user"` 消息，但 UI transcript 不得复用该 role 推断原始行类型。 |
| 本轮运行 | R1 `RunID` | TurnRunner/Run owner 状态快照 | 运行状态、logical outcome、cleanup/execution state 分开显示；`RunID` 不以 session ID 代替。 |
| 工具调用及输出 | `RunID + assistant/group transcript ordinal + ToolCallID`，stdout/stderr 另带 stream；执行并持久化后由 R2 映射到真实 `ActionID` | R1 typed tool call/receipt；EventStream 仅提供实时观察；R2 typed commit sink 提供权威 durable 映射 | 同一 Run 的不同 provider completion 可能复用 `call_1`，不能只用 `ToolCallID` 归组。按 canonical assistant/group transcript 顺序区分调用，不能从文本、参数或时间猜身份；持久化后使用 R2 返回的真实 `ActionID`。并行完成顺序不能改变请求/结果配对。 |
| approval / ask_user | 各自唯一的 `RequestID`；TUI 队列 token 只作进程内撤回定位 | 对应 request/response channel 和 request context | 活动项与排队项均保留各自身份；取消一个请求不能替代、覆盖或答复另一项。 |
| steer / follow-up 输入 | 目标由 R1 提供的 opaque `InputID`；`InputKind` 取 R1 `user/steering/follow_up`。目前 `Steer/FollowUp` 只接收 string，canonical transcript 也没有 queue/input ID，不能当作已有能力。 | R1 TurnRunner/Loop 的接受与消费 seam；R2 持久化后返回真实 `EntryID` 映射 | 先显示本地 `submitting`；只有 R1 接受回执后标记 `queued/accepted`；消费并持久化后用 R2 的真实 ID 映射为 transcript entry。重复文本按 ID/队列顺序关联，绝不按内容匹配。 |
| 实时事件 | R1 `RunID` 与其定义的事件序号/缺口状态 | 有界 EventStream | 只用于实时进度，不用于重建 durable history；检测到 gap 必须可见。 |

当前 steer/follow-up 接口只有 string 入参，R1 canonical transcript 的已定义关联键是 tool `ToolCallID`；当前不存在可供 TUI 使用的 Runtime queue/input ID。要提供逐条 `queued → consumed → durable` 状态，依赖一个窄 seam：R1 的 TurnRunner/Loop queue owner 增加向后兼容的 correlated-input 接收/回执，把 opaque `InputID` 和 `InputKind` 随队列项带到 canonical transcript；原 `Steer`/`FollowUp` string API 可保留为兼容入口。`internal/cli/repl_session.go`/`internal/tui` 仅持有临时的 UI submission token 来关联提交回调，不能把它说成 Runtime 或 session ID。R2 session owner 在真正持久化时提供 `InputID → (SessionID, BranchID, EntryID)` 映射；provisional 行只凭此权威映射转成 durable。相同文本可重复提交，不能以字符串内容、下标或时间戳关联。若 R1/R2 seam 未落地，R5 只能显示明确为本地的 submitting 状态和非逐条的聚合计数，不能声称某条输入已被 Runtime 消费或持久化；这项是 R5 per-item queue/transcript 验收的阻断依赖。

R1 定义 `RunOutcome = success/error/cancelled/unknown`，并将其与 `RunExecutionState = running/cancel_requested/draining/cleanup_pending/quiescent/quarantined` 分离。实际字段/状态句柄以 R1 builder/reviewer 最终落定的 API 为准。TUI 不能因收到 `done` event、LLM 文本结束、cancel callback 返回或 EventStream 关闭，就推导 worker 已退出或 transcript 已提交。

建议的最小展示记录是一个仅用于 UI 的投影值：`identity`（session/branch/entry 或 run/tool/request/queue ID 之一）、`kind`、`content/detail reference`、`delivery state`（provisional/durable/gap）、可选的 tool status、`RunOutcome`、`RunExecutionState`。它是 view model，不是另一个持久化合同；持久 transcript 仍由 R1/R2 管理。对于 R1 没有正式序号或 R2 没有稳定 entry ID 的数据，不伪造字段，builder 应先将接口缺口交回对应 owner。

### 暂存、持久化和结束状态

- 流式 assistant delta 与工具 stdout/stderr 是 provisional。可实时展示，但必须与已保存行有明确区分；将它们合并进 durable entry 时按稳定身份更新原行，不能再追加一份。
- 用户输入、tool call/receipt、steer/follow-up、compaction marker 和最终 assistant 由 R1 canonical transcript、R2 typed durable entries 提供。Provider 有效上下文 projection 与 UI durable transcript 分离；R2 将 compaction summary 作为 `Role="user"` 恢复给模型，不改变对应 durable 行的 `EntryTypeCompaction`。EventStream 丢失/重复不改变权威历史。TUI 不从 `AgentEvent` 或 provider message role 拼 session transcript。
- `done` 只更新运行状态，不再将完整 assistant 文本作为第二条状态消息渲染。完整 assistant answer 只保留一条，且其 `EntryID` 与 durable store 对应。
- 仅当 R1 确认 `Outcome=success`、`ExecutionState=quiescent` 且 R2 对最终 transcript 的语义提交成功，才显示“完成 · 已保存”。失败/取消时，仍单独展示已保存的 partial transcript 状态；`unknown` 必须写明结果未确定，不暗示成功或重放。
- 状态对应关系：`running` 显示运行中；`cancel_requested` 显示已请求停止；`draining` 显示正在等待工具/reader 收尾；`cleanup_pending` 显示调用方已返回而后台清理未完；`quarantined` 显示仍无法证明资源已停止。若取消后状态句柄继续更新，界面可以更新状态，但不能发送第二个 terminal result。
- `EventGapCount > 0` 时显示“实时事件缺口”及数量；该轮实时视图标记为不完整。只有 R2 canonical history reload/reconciliation 确认补齐后，才可撤下不完整标记；事件通道后来恢复不构成补齐证据。
- 不可恢复的持久化错误、status handle 不可用或运行结果未知不能降级为绿色完成。若远端/旧入口不提供 required state，明示“状态不可用/未支持”，并在 R5 default-path 验收中视为未闭环。

## 主要交互旅程

| 旅程 | 状态变化与用户可见内容 | 通过条件 |
|---|---|---|
| 新 prompt | 提交后 transcript 立即有带当前 session/branch 身份的 user 行；先标暂存，store 确认后显示已保存。本轮状态独立显示。 | Prompt 只出现一次；store 失败不显示“已保存”；Run 关联正确 session。 |
| Agent 响应 | 增量文本追加到同一 provisional assistant 行；完成事件只更新状态。 | delta + final + done 只有一份 answer；行可关联到 `RunID` 和 R2 entry。 |
| 工具执行 | `pre_tool_call/tool_call` 显示工具名、稳定 call ID、简明参数摘要和 waiting/running 状态；stdout/stderr 分流归组；结果显示 success/error/interrupted/unknown receipt。 | 不因并行完成乱配；工具参数不丢；敏感字段按 R4 policy 脱敏；未收到 receipt 不显示成功。 |
| 审批 | 使用明确的“工具权限请求”弹层，始终展示工具、具体操作摘要、request ID/排队数、审批作用域和可见决定键。审批不与 ask_user 混用。 | 每请求唯一往返；一次允许、持久允许和拒绝的说明遵循最终 R4 scope；没有可执行 Always Allow 时不展示该键。 |
| ask_user | 使用“Agent 需要回答”标题，不使用 permission/allow 颜色语义；显示问题和选项，支持键盘选择、提交和取消。 | 正确响应只回到对应 RequestID；取消释放 waiter；另一个排队 interaction 继续可答。 |
| 并发审批/HITL | 所有 interaction 按接收顺序 FIFO 进入一个有 ID 的等待队列；焦点只在 active 项，footer 显示剩余数量和下一项短摘要。 | 两个或更多审批逐个都可回答/撤回；不同类型 modal 混排仍按序；取消队列项不移除 active 项。 |
| steer / follow-up | 输入时明确标注将 steer 当前 run 或排队为后续输入；提交后先展示本地 submitting，R1 correlated-input 接口确认后才标 queued/accepted；消费时以 `InputID` 进入 canonical history，R2 返回 `EntryID` 后成为 durable 行。 | 重复文本按 opaque ID 而非内容关联；若 R1/R2 seam 未就绪，只显示无逐条 claim 的本地/aggregate 状态；队列满/回调失败能看见；编辑光标与插入位置一致。 |
| cancel / cleanup | 取消动作显示 intent，之后显示 owner 提供的 draining/cleanup_pending/quarantined/quiescent 状态；工具 receipt unknown 时保留 unknown。 | cancel 不伪装 quiescent；清理待完成时不显示 done、不释放可见的 active 状态；已有或排队交互全部收敛。 |
| resume / fork | R2 返回实际选择的 session 与 branch 后再渲染该分支 durable transcript；新 run 绑定此 branch。fork 作为 R2 已确认操作的分支身份切换投影，不复制 UI 内存历史作为 fork。 | `TestTUITranscriptRestoresSelectedBranch`；含 `EntryTypeCompaction` marker、marker 后 suffix 和 source boundary 的 fixture，断言 UI 行仍为 compaction 类型、summary 不成为新 user turn、EntryID/BranchID 不变，并分别验证 durable transcript 与 provider 有效上下文 projection；无旧 branch 行混入，tool groups 仍按 ID 完整。R2 `/resume` 修复和审查是前置。 |
| export | 若最终 R2/CLI 提供 export，导出对象是所选 durable branch，而非截断后的可见窗口或未提交 delta；显示目标和完成/失败回执。没有可调用能力时，help/command surface 标记 unavailable。 | 不声称 TUI buffer 等于完整 session；导出失败可见。R5 不实现新 export 格式，通用 export workflow 属 R8/既有 CLI capability。 |

## 工具进度、审批可辨识和长内容

工具行按 `RunID + assistant/group transcript ordinal + ToolCallID` 形成一个调用组；不同 provider completion 可复用 `call_1`，因此 `ToolCallID` 只在其 assistant/group 内唯一。显示 `tool name + concise args summary + lifecycle status + stdout/stderr + final receipt/error`。执行并持久化后，R2 typed commit sink 提供真实 durable `ActionID` 映射；UI 不自行生成权威 ID，也不按相同文本、参数或时间猜测对应关系。stdout/stderr 详情只引用相应 executor 按自身 source-capture contract 产生的有限 captured-payload；例如 Bash 使用其 `MaxOutput`，不以 R1 的 400,000-byte provider-candidate cap 代替工具输出上限。UI 不另存或复制一份无界原始输出供展开。若上游没有提供有界 captured-payload 引用或对应来源截断标记，R5 必须先将该接口缺口交回 executor owner，不能由 TUI 自己补一个不受限缓存。审批出现时，主状态变为“等待授权”，调用本身不能被误画成正在运行。并行工具各自有 spinner/state，不能用全局单一 spinner 覆盖每个工具状态。Todo/Subagent 只在默认 local bridge 收到有类型、身份稳定的来源后列为支持功能；无 Runtime/CLI 来源时显示 unavailable，不凭空增加空的进度栏或子代理结论。

审批与 ask_user 必须在文本标题、色彩/图标、字段和按键提示上都能区分。approval 显示 R4 的效果类别与权限 scope（例如仅本请求、可复用规则的确切范围、拒绝/取消）；授权参数必须来自 R4 prepared call 的同一份完整规范化执行参数，并按 R4 决定脱敏。stdout/stderr 的 source-capture cap 不限制或替代 tool-call args，也不能因格式化/摘要把审批内容改成另一条授权命令。始终允许应明确范围与持久期，完全跟随 R4 最终决策，不能借用竞品用语推导本项目缓存规则。Plan Mode exit request 待定时单独标成 confirmation pending，同时保留 current mode 指示。

长参数、命令和 diff 不能把操作行推到视口之外。弹层有有限高度的详情区，操作行固定可见；默认显示摘要，展开只能查看同一个有界 detail reference，不另存无界原文副本。工具 stdout/stderr 或 diff preview 只有在对应 executor/preview producer 的 source-capture contract 明确截断时才标 `source capped`；`view truncated` 表示当前弹层暂未显示、但仍在该 bounded capture 内。行数/字节数只有上游提供时才报告。审批参数则必须是 R4 prepared call 中完整、规范化并按 policy 脱敏的实际执行参数；不能把 provider candidate 限额或普通输出 source cap 当作参数上限。若 args 因 view truncation 无法完整检查，Allow 不得作为可见/有效操作，须由权限边界拒绝或等待用户在完整 detail viewer 中查看。该窄交互区不提供通用 transcript 搜索/浏览。

普通工具输出沿用可折叠详情，展开时引用同一份由 executor source-capture contract 限界的 Runtime capture，不复制 raw payload。若只有部分 capture 可见，明确区分 `source capped` 与 `view truncated`：前者说明对应工具 source-capture 层已舍弃/未捕获的数据不可恢复；后者说明当前折叠/窗口隐藏的数据仍在受限 capture 中。R1 的 400,000-byte CompleteResponseGuard 限制完整 provider candidate，并不定义任何 stdout/stderr 工具 source cap；source cap 必须按具体 executor/configuration 验证。不将任一摘要保存成 receipt。若 `Send` 或 EventStream 背压导致状态丢失，必须按 R1 的 gap 状态显示 incomplete，并交由 R2 canonical history reconciliation 收敛。`BubbleteaApp.Send` 在满队列时返回可检查错误；critical callsite 记录/展示错误或使用受控重试，不能静默丢弃。关键 Run/approval/transcript 生命周期不得只经 best-effort `Send` 发送。

## 键盘模式与可发现性

所有快捷键由一个 action registry 定义，key handler、modal footer 和 `?` help 由同一绑定表生成；help 只列当前 mode 可触发的操作及结果，不展示没有实现的占位操作。下表是 R5 目标语义，底层 permission policy 仍以 R4、运行状态以 R1 为准。

| 模式 | 键与动作 | 约束 |
|---|---|---|
| 主视图 | `Tab` 打开 steer 输入；`f` 打开 follow-up；`Space` pause/resume；Up/Down 选择历史条目；Enter 展开/折叠；`e/c` 全部展开/折叠；`?` help | 上/下当前是 accordion selection，不写成 transcript scroll。若 Runtime 未提供 pause，则 help 不显示此动作。 |
| 主视图运行中 | `Esc` 请求取消当前 Run 并继续显示 draining 状态；`Ctrl+C` 同样走取消/全部等待交互收敛路径；`q` 开始有界的会话退出流程 | 退出先停止接收新输入、结清 approval/HITL waiter、请求取消并将当前 outcome/execution state/未决 action quarantine 按 R2 落盘。只等待 R1 规定的 bounded cleanup interval；超时后以 `cleanup_pending/quarantined` 和已持久化 quarantine 状态退出，不得无限等待，也不得显示成 quiescent。不得新增 force-kill 行为。 |
| steer/follow-up 输入 | 左右移动、Backspace、Ctrl+A/E/W/U、Enter 发送、Esc 放弃输入 | R5 测 caret 插入位置和发送回执；不要承诺多行、外部编辑器或 completion 行为。 |
| approval modal | `y` 仅本请求允许；`a` 仅在 R4 给出可复用 scope 时允许；`n` 拒绝；`d` 显示/隐藏已有 diff；`Esc` 取消/撤回当前请求；`Ctrl+C` 取消当前 Run 并收敛其所有交互 | footer 给出完整文本与 scope。Esc 不能被 UI 当作 Allow；若 R4 响应类型把 cancel 映射成 deny，结果标签按 API 回执显示，不伪称为用户 reject。 |
| ask_user modal | 有选项时 Up/Down 选择、Enter 或已标注数字选择；自由回答时按当前 input contract 编辑并提交；Esc 取消本次问题；Ctrl+C 取消 Run | question/permission 的键作用域不能串用；modal queue 保留后续请求。 |
| help modal | `?`/Esc 关闭；全局取消动作仍按主视图/交互模式处理 | help 内容与实际 handler 同源；不得吞掉能取消 Run 的键，也不能把隐藏按键写成 force quit。 |

`q` 的退出时序由 `internal/cli/repl_session.go` 与 R1/R2 的窄接口协作实现：REPL 请求取消、让 approval/HITL waiter 都获得明确终态，再调用 R1 有界等待并读取最新 status；不能用无期限 `<-turnDone` 等待 worker。R2 store 在退出前持久化可确认的 terminal state，或持久化未决 action 与 quarantine 记录，并返回提交结果。若 R1 deadline 到期，UI 显示 cleanup_pending/quarantined，然后在 R2 quarantine 记录确认后正常退出；进程退出不代表 worker/resource quiescent。R2 恢复据该记录阻止冲突的 mutation，直至按 R2 合同观察到资源退出或人工 reconciliation。不得因为用户按了 `q` 就强杀进程/子进程，也不得无期限卡住来掩盖 cleanup 未完成。若状态/未决 action 无法持久化，必须给出退出/保存错误，不能宣称干净关闭；具体失败闭环使用 R2 最终 store/recovery 合同。

这不是对所有键做全局重映射的提议。R5 先固定模式优先级与真实行为，通用可配置 keybinding 属 R8。模态输入优先于主视图；cancel 等全局安全动作的优先级明确且有测试，普通键不得穿透到后层 modal/输入框。`d` 仅当当前 request 带有 diff 时出现。

## Resize、布局与截断最低要求

`tea.WindowSizeMsg` 更新尺寸后，R5 视图需保留焦点、选中 request、队列顺序、编辑文本和 caret；resize 本身不能提交输入/授权/拒绝。窄于 120 列走单列，当前 120 列以上工具双栏布局可以保留；窄窗口隐藏次要信息前先保留 active run 状态、session/branch 短 ID、modal 标题和决策键。长 modal 的正文滚动限于它自己的详情区，操作行固定。R5 PTY/model acceptance 至少覆盖 60/80/120/200 列 resize，并在 stream、approval、ask_user 各状态 resize 一次。

R5 做可视区域的有界摘要和明确截断提示，不承接 R8 cell-width 算法重写。CJK、emoji/ZWJ、ANSI/OSC 对齐和 grapheme-safe truncation 由 `TestTranscriptViewportCJKAndANSI` 及 R8 终端矩阵验收；R5 文字摘要不得据此宣称 Unicode/ANSI 已正确。viewport 搜索、10k event benchmark、跨平台 TTY lifecycle 同样属于 R8/其平台验收。

## Pi / OpenCode 工作流参考

参考只用于用户任务和状态可发现性，不要求按 feature count 做 parity。本轮未安装或实际运行 Pi/OpenCode。官方文档于 2026-09-25 重新读取；代码快照沿用 05 审计当日固定的官方仓库 commit，链接可复核。

| 来源 | 已核实内容 | 对本设计的借鉴与边界 |
|---|---|---|
| [Pi terminal usage](https://pi.dev/docs/latest/usage)、[Pi keybindings](https://pi.dev/docs/latest/keybindings)、[Pi source snapshot `d5629e2`](https://github.com/earendil-works/pi/tree/d5629e20489ccf770ed90b5a33941cb3b7ef24d0) | 最新终端文档描述 transcript 包含用户输入、assistant、工具调用/结果/错误，footer 展示 session/model/context/usage；运行时支持调整任务、排队 follow-up、撤回队列和 Escape 中断；`/hotkeys` 与命名 action 可见。代码快照来自 Pi interactive/keybinding/session 实现。 | 借鉴把工具工作、输入队列和 active session 作为可见对象；不借用 Pi 不逐次询问工具的权限模型，也不复制 Pi 的 session tree、命令和键位集合。 |
| [OpenCode TUI](https://opencode.ai/docs/tui/)、[OpenCode keybinds](https://opencode.ai/docs/keybinds/)、[OpenCode permissions](https://opencode.ai/docs/permissions/)、[OpenCode source snapshot `0f54984`](https://github.com/anomalyco/opencode/tree/0f549842ee746e400b1f72516b0b2e292e267e2c) | 官方 TUI 文档列出 session/details/thinking/editor 等工作流和 `tui.json` 配置；配置覆盖 scroll speed/acceleration、diff、cursor、mouse；keybinds 有 leader/action；permission 文档清楚区分 once、always（工具建议 pattern、当前 session）和 reject。 | 借鉴将状态和授权范围用文字说清，键位按 action/modal 区分；本项目的 `AlwaysAllow` scope 由 R4 决定，与 OpenCode 文档定义不等同。长审批正文的动作行固定、可达是本设计验收项；外部问题也表明长命令可遮住审批按钮，故 R5 必须测长内容。 |

参考来源的版本边界：Pi 文档显示 Latest，链接按官方文档当日内容；OpenCode TUI 文档标注 last updated Sep 24, 2026。以上只验证官方说明/代码快照，不称为对竞品安装包的运行验证。OpenCode permission scope 和 Pi queue semantics 不是当前 Runtime 的事实来源。

## 实施顺序与验收

R5 builder handoff 需在 `R1-REV-01` 和 `R2-REV-01` 完成后进行。审批文案和 Always Allow scope 只消费 R4 最终合同；触及 D-01 的 Plan Mode exit 路径在用户决策落定前保持 gated，其余 R5 切片不因此改变自身合同。分工按执行任务：`R5-BLD-01` 落交互队列、canonical projection、tool args/status、Send 背压和 default bridge；`R5-BLD-02` 落 editor caret、模式键表、cancel 及 cleanup_pending 视觉状态；`R5-REV-01` 独立做真实模型、CLI 集成、PTY 与 race 审核。任何需要改变 R1/R2/R4 公共合同的意见先回到合同 owner，不在 TUI 侧旁路实现。

执行任务中明列的测试名均是计划项，builder 必须逐个先用 `go test -list` 确认准确注册，再跑 `-run`；零匹配不是通过证据。R5 主要 model/integration cases：

- `TestTUIHandleHITLMessage`、`TestTUIApprovalQueueDoesNotLoseRequests`、`TestApprovalCanBeCancelled`：驱动生产 `teaModel.Update`，包含两个 approval、不同类型 modal 交错、按 ID 响应与取消，检查所有 waiter 最终收敛、无覆盖且 active/queued 顺序正确。
- `TestTurnDoneDoesNotDuplicateAssistantText`、`TestTUIEventProjectionPreservesToolArgs`、`TestTUIWiresTodoAndSubagentEvents`：实际 bridge→model event journey；answer 唯一，ToolCallID/参数/stdout/stderr/error 关联正确。Todo/Subagent 有正式来源则验证接线；无来源则验证产品表面明确 unavailable，不伪造能力。
- `TestTUITranscriptRestoresSelectedBranch`：从 R2 提供的真实 SessionTree fixture hydration，断言选定 branch 的 user/assistant/tool entries 与 `EntryTypeCompaction` marker、marker 后 suffix 和 source boundary，与 store 快照一致；压缩 summary 保持 typed compaction 行，并与 provider `Role="user"` context projection 分离。
- `TestTUISendReportsBackpressure`：占满 queue 后 caller 收到明确 error；关键运行状态不被静默丢弃，UI 有 gap/error 说明。
- `TestSteerEditorCaretTracksInsertion`、`TestHelpKeysMatchModalBindings`：中间插入后 caret 正确；测试按 registry 枚举的动作对照各 modal handler，help 无死键/漏键/误导性“Force quit”。
- `TestTUIStopWaitShowsCleanupPending`：Cancel 后受控未退出 worker；在 `cleanup_pending` 时 TUI 仍显示停止未收敛，worker quiescent 之后才显示终止状态。

必须从默认 local CLI 路径测试，不使用仅能证明 reducer 的自造 model 替代 production `teaModel`。增加 PTY 子进程旅程（名称由 builder 按仓库约定登记）：提交 prompt → tool approval → tool receipt → ask_user → 并行审批 → steer/follow-up → cancel/cleanup_pending → stop/quiescent → resume selected branch。保留输入字节、窗口 resize 和输出 transcript，断言 terminal/raw mode 最终恢复、无双重 answer、长 approval 操作行始终可达。PTY 至少用 60/80/120/200 列，覆盖 active approval 时 resize 与超长 args/diff。

Reviewer 应先确认上述每个计划测试准确注册，再跑完整受影响 TUI、CLI、core、session race suites 和 PTY subprocess。小/大参数、两个以上并发审批、不同 modal 混排、流式结束与 durable commit 先后竞争、Send/event gap、取消后 worker 延迟退出都必须覆盖。05 审计中的旧 model/channel render tests 不作为 PTY 或视觉 E2E 证据；人工截图/跨终端 UI quality 另记 R8。

R5 通过条件是：所选 SessionTree branch 的 transcript 与 JSONL/canonical records 一致；所有可见消息都有稳定来源身份与 durable/provisional 标签；approval/HITL 并发与取消不丢 waiter；参数/审批 scope/工具结果可解释且敏感字段按 R4 脱敏；取消期间状态忠于 R1；help 与 modal/action 实际键一致；队列/backpressure 和事件缺口可见；真实 PTY 旅程在 resize 下保持这些行为。R8 的 viewport/Unicode/performance/platform quality 不计入 R5 完成，也不能被 R5 的通过结果替代。
