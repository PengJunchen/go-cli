# UI 源码审计与迭代计划

审计日期：2026-09-24  
范围：`internal/tui`、`internal/term` 四个跨平台源码文件，以及 CLI 输入、REPL 调度、核心事件到 UI 的投影、配置和现有文档。  
结论：UI 有可继续演进的组件基础，但当前真实用户路径还不是完整的会话式 coding-agent TUI。已有流式输出、工具折叠、审批、暂停和转向等构件；其中 ask_user 接线、结束事件投影和并发审批存在可从源码闭合的功能缺陷。长输出、键盘编辑、会话浏览、终端单元格宽度与真实终端验证也有明显差距。建议先修 P1 交互闭环，再完善会话工作流、滚动/编辑/事件投影，最后做性能与终端兼容验收。

## 审计边界与证据

实际默认旅程分成两段：`DefaultLineEditor.ReadLine` 先在独立的 raw-mode 编辑器收集输入；`REPLSession.executeTurn` 再为每轮建立 EventStream、启动 Agent、把事件接到 Bubble Tea。TUI 状态随每轮新建，`renderTUI` 只接当前轮的本地 BridgeEvents 或远程 ACP SSE，并传入模型名、轮数、会话 ID、主题、审批通道和回调（`internal/cli/repl_session.go:590-597,648-718,722-775`）。因此审计以这条 CLI→Runtime event→TUI 路径为准；单独存在的 renderer、Todo panel 或 Subagent 转换函数不能直接当作用户已获得的功能。

本报告以源码静态追踪为主，没有调用真实 LLM，没有启动真实终端作视觉验收，也没有在本子任务中重新运行测试。仓库现有 `internal/tui/*e2e*_test.go` 和 `tui_e2e_test.go` 操作的是 model/channel/render 字符串；它们不是屏幕截图测试或完整 TTY 用户旅程测试。CLI 有 Linux/Darwin line-editor PTY 测试，但针对输入 raw-mode，而非 Bubble Tea transcript、审批、resize 和整段 REPL 的视觉验收。`internal/term` 未发现测试文件。

审阅的 TUI 生产文件：`accordion.go`、`app.go`、`approval_entry.go`、`bridge.go`、`box.go`、`http_client.go`、`renderer.go`、`renderers.go`、`spinner.go`、`streaming_markdown.go`、`terminal.go`、`theme.go`、`todo_panel.go`。审阅的 TUI 测试文件：`accordion_test.go`、`app_extended_test.go`、`app_hardening_test.go`、`app_test.go`、`approval_entry_test.go`、`box_test.go`、`bridge_subagent_test.go`、`bridge_test.go`、`color_profile_test.go`、`e2e_render_test.go`、`help_overlay_test.go`、`http_client_test.go`、`lipgloss_test_setup_test.go`、`mocks_test.go`、`registry_test.go`、`renderers_edge_test.go`、`renderers_extended_test.go`、`renderers_markdown_test.go`、`renderers_security_test.go`、`renderers_test.go`、`spinner_test.go`、`split_layout_test.go`、`steer_statusbar_test.go`、`streaming_markdown_codeblock_test.go`、`teamodel_test.go`、`theme_edges_test.go`、`theme_extended_test.go`、`theme_test.go`、`todo_panel_test.go`、`tool_status_test.go`、`tui_e2e_test.go`。`internal/term` 审阅 `term_size.go`、`term_size_darwin.go`、`term_size_linux.go`、`term_size_other.go`。

CLI 与 Runtime 接线审阅 `internal/cli/interactive.go`、`repl_session.go`、`line_editor.go`、`interrupt.go`、`resize_unix.go`、`resize_other.go`、`slash.go`、`slash_handlers.go`、`slash_context.go`、`completer.go`，TUI 配置定义与校验，`internal/core/types.go`、`internal/core/loop.go` 事件构造，以及 README 中英文版、`docs/integration-audit.md`、`docs/source-audit-2026-08-07.md` 对 TUI 的描述。未对其他 Runtime 子系统做本子审计的完整证明；表中只记录其传给 UI 的可见事件与调用链。

## 竞品基准与比较方式

竞品能力依据 2026-09-24 可访问的官方文档；本次没有安装或实际操作 Pi、OpenCode。Pi 官方代码来源使用已迁移到 `earendil-works/pi` 的仓库，记录当日 `main` commit `d5629e20489ccf770ed90b5a33941cb3b7ef24d0`；OpenCode 记录当日 `anomalyco/opencode` `dev` commit `0f549842ee746e400b1f72516b0b2e292e267e2c`。代码快照链接：[Pi commit](https://github.com/earendil-works/pi/tree/d5629e20489ccf770ed90b5a33941cb3b7ef24d0)、[OpenCode commit](https://github.com/anomalyco/opencode/tree/0f549842ee746e400b1f72516b0b2e292e267e2c)。Pi 文档标记为 Latest，OpenCode TUI 文档显示最后更新 Sep 24, 2026；文档能证明官方说明了某能力，不等于本次对竞品运行时的独立实测。

| 用户任务 | 本项目当前源码路径 | Pi 官方文档能力 | OpenCode 官方文档能力 |
|---|---|---|---|
| 输入较长任务、改写光标中间内容 | REPL 前置 raw-mode `LineEditor` 有历史、补全、反向搜索、左右/Home/End 等；运行中的 TUI steer/follow-up 是另一套简化输入状态 | 主编辑器支持多行 `Shift+Enter`、`Ctrl+G` 外部编辑器、`@` 文件查找、Tab 路径补全；见 [Use Pi in the terminal](https://pi.dev/docs/latest/usage)、[Keybindings](https://pi.dev/docs/latest/keybindings) | `/editor` 使用 `$EDITOR` 编写消息；`tui.json` 可配键位；见 [TUI](https://opencode.ai/docs/tui/) |
| Agent 执行中改变方向或停止 | Tab steer、`f` follow-up、Space pause、Esc/Ctrl+C cancel；审批弹层期间 Esc/Ctrl+C 不通 | Enter 排队转向、Alt+Enter 排队后续任务、Alt+Up 取回队列、Esc abort；见 [Use Pi in the terminal](https://pi.dev/docs/latest/usage) | 官方 TUI 说明 Esc 中断；命令经 Ctrl+X leader 发现和执行；见 [TUI](https://opencode.ai/docs/tui/)、[Keybinds](https://opencode.ai/docs/keybinds/) |
| 检查工具执行与模型思考 | 工具调用/结果按 call ID 归组、可折叠，执行中显示 spinner；常规 tool_call 事件只携带名称，参数不进入此 UI；message 实际走普通 assistant renderer | transcript 显示工具调用和结果，Ctrl+O 折叠工具输出、Ctrl+T 显隐 thinking | `/details` 显隐工具细节，`/thinking` 控制 thinking 块显示；见 [TUI](https://opencode.ai/docs/tui/) |
| 批准或拒绝有副作用操作 | TUI 有 `y` once、`a` always、`n` reject 和 diff 预览，但单槽审批可能丢掉并发问题；`d` 只是无操作占位 | 官方终端使用说明明确不要求逐次工具审批，并强调用户审阅命令和文件改动；不把它描述成与本项目同形的审批 UI | permission 提示提供 once、always（采用工具建议的匹配规则）和 reject；见 [Permissions](https://opencode.ai/docs/permissions/) |
| 回看长对话和切换会话 | 每轮新建 UI model；默认窄屏单列渲染全量 entries，没有实际 transcript 滚动/搜索；`/session` 命令已注册，恢复闭环另见总审计 04 | 普通 scrollback 或 fullscreen transcript；`/new`、`/resume`、`/session`、`/tree`、`/fork`、`/clone`、`/compact` | `/new`、`/sessions`、`/undo`、`/redo`；撤销和重做文件改动依赖 Git；见 [TUI](https://opencode.ai/docs/tui/) |
| 让终端行为适合个人习惯 | 有主题、word_wrap、diff_style 配置；快捷键主要是硬编码，diff_style 当前未消费 | 可按命名 action 在 `keybindings.json` 配置/关闭快捷键，`/hotkeys` 查看；支持 transcript 搜索、翻页和滚动 | `tui.json/jsonc` 与配置服务/runtime 的 `opencode.json` 分离，可配 theme、leader、scroll、diff、cursor、mouse、attention；见 [TUI](https://opencode.ai/docs/tui/) |

比较时不采用基础设施设计文档中未核实的星数、旧版功能主张或兼容性推断。官方文档条目引用如下：[Pi terminal 使用说明](https://pi.dev/docs/latest/usage)、[Pi 键位参考](https://pi.dev/docs/latest/keybindings)、[OpenCode TUI 使用说明](https://opencode.ai/docs/tui/)、[OpenCode 键位](https://opencode.ai/docs/keybinds/)、[OpenCode Permissions](https://opencode.ai/docs/permissions/)。

## 目前合理、值得保留的设计

- TUI model、renderer registry、terminal size provider 分层，Bubble Tea 拥有 TTY render loop；颜色遵循 `NO_COLOR`/`CLICOLOR`，非 TTY 路径关闭终端渲染（`internal/tui/app.go:349-367`、`color_profile_test.go`）。
- 工具结果可通过 ToolCallID 归到发起调用下方，保留 stdout/stderr、错误标记、执行状态、时长、折叠态；工具结果的可见行数有默认上限 20 行（`internal/tui/accordion.go:104-134,218-268,319-339`、`app.go:1110-1153`）。宽度达到 120 列且有工具时启用左右面板，终端 resize 会更新 model 宽高（`app.go:614-624,674-737`）。
- 面向外部交互已有暂停/恢复、steer/follow-up、取消回调、状态栏和主题管理；这些都接到了 TurnRunner/LoopAgent，不只是孤立按键（`repl_session.go:777-821`）。
- 前置 CLI 输入编辑器比 TUI steer editor 完整：raw-mode 保底 defer 恢复、Bracketed Paste、历史、Tab completion、Ctrl+R 搜索、光标编辑、Resize 监控，以及 Ctrl+C 分级处理；无法设置 raw mode 时退回普通输入（`line_editor.go:47-80,526-586,645-849`）。
- `/session` 的 `tree/fork/resume/branches/clone/switch` 命令已在 CLI 注册，持久化路径也已接线；这只证明命令/组件存在，不证明恢复可用。总审计 04 已确认真实 `/resume` 路径把 fileID 当作 entryID，导致恢复失败；UI 审计不重复判断该 Runtime 缺陷。主题可跨轮共享（`slash_handlers.go:222-258`、`repl_session.go:735-765`）。

## 已确认缺陷与功能缺口

### UI-01 · P1：TUI 的 ask_user/HITL 请求无人接收，Agent 会等到取消

`cliHITLEmitter.Emit` 在程序已设置时用 `tea.Program.Send(HITLMessage)` 投递问题，然后等待 `ResponseCh` 或 context 结束（`internal/cli/interactive.go:246-289`）；REPL 的 `wireHITL` 确实在 Bubble Tea ready 后设置 Program（`repl_session.go:824-845`）。但 `teaModel.Update` 没有 `HITLMessage` 分支，未知消息进入 default 分支后只计数并等待内部 `msgCh`，不会显示问题或填写 response（`internal/tui/app.go:596-663`）。所以默认 TTY 用户触发 ask_user 后，工具链无法拿到回答，通常停到 turn context 被取消。现有 `internal/cli/hitl_test.go` 用自己的 `hitlTestModel` 检验 Program.Send，不覆盖真实 `teaModel.Update`，因此没有发现这个集成断点。

### UI-02 · P2：每个成功 TTY turn 都把最终答案作为 done 状态再显示一次

核心 streaming 先发 `Kind:"message", Incremental:true` 分片，再发完整 `Kind:"message"`（`internal/core/loop.go:584-593,813-831`）。UI 对有 streaming 的完整 message 会跳过重复（`internal/tui/app.go:1047-1060`）。但 REPL 结束时又发 `Kind:"done", Content: turnResult.Message`（`internal/cli/repl_session.go:707-718`）；bridge 把 `done` 映射为 status（`internal/tui/bridge.go:21-43`），status 事件没有去重，最终走 `addEntry`（`app.go:1090-1108,1170`）。因此 TTY transcript 末尾会再出现一条内容等于答案的 status entry。非 TTY 有单独的最终打印分支，不应据此推断也出现相同 UI 现象。

### UI-03 · P1：并行审批请求用单个 pending 字段，后到请求会覆盖前一个

`approvalRequestMsg` 每次都把 `m.pendingApproval` 替换成新请求，并继续从 buffered approval channel 读取（`internal/tui/app.go:602-611`）；REPL 建立的审批 channel 容量为 32，底层 Agent 配置支持并行工具（`repl_session.go:268-271,684-693`）。两个工具同时要求审批且第二个消息先被处理时，第一个 `ResponseCh` 永远没人写，相关 middleware 等待不结束；用户看到的只会是最后一项。审批请求需要按请求 ID 排队或显式限制并行，并保证取消时每个响应都能闭合。

### UI-04 · P2：核心事件投影丢掉工具参数与 pre-tool 信息

`CoreEventToAgentEvent` 文档称 mapping lossless，但只复制 Kind/Content/Incremental/ToolCallID/Stream/IsError 和 `TokenUsage`；没有投影 `core.AgentEvent.Usage`、`ToolCalls`、`PreToolCall`、`Timestamp`（`internal/tui/bridge.go:46-68`；字段定义 `internal/core/types.go:80-95`）。`tool_call` Content 只有 `tc.Name`，参数放在 `ToolCalls`/`PreToolCall` 字段里；bridge 丢弃这些字段，常规工具面板就没有参数。`pre_tool_call` 又未被 `KindToContentType` 识别，映射成 status 事件并可能插入空状态项（`internal/core/loop.go:607-625,669-687`、`bridge.go:21-43`）。

### UI-05 · P2：Markdown、Todo、Subagent 组件未接入标准本地事件路径

`message` 映射为 `ContentTypeAssistant`，不是 `ContentTypeMarkdown`；默认 assistant renderer 只加 `AI:` 风格前缀并 wrap 文本，而 `handleIncremental` 的 Markdown 增量缓存只用于 markdown/streaming/streaming_code 类型。故 README 提到的 Markdown/语法高亮能力虽在 registry 有实现，当前本地默认 message 主路径并未到达这些 renderer（`internal/tui/bridge.go:21-27`、`app.go:1222-1308`、`renderers.go:551-568`）。

`todo` 没有 core→TUI event 映射；`TodoUpdateMsg` 仅在 UI 中处理，当前代码检索未找到生产调用方。Subagent converter 仅被测试调用，标准 BridgeEvents 不转发子 Agent 事件；`ContentTypeSubagent` 也未纳入 registry content type 清单（`bridge.go:71-84,99-126`、`renderer.go:71-81`、`todo_panel.go`）。这些应标作“组件存在、默认体验未接线”。

### UI-06 · P2：会话 UI 每轮重建，未投影 user prompt 或历史分支

TUI 每轮创建新 BubbleteaApp 和空 accordion，当前轮 EventStream 也在本轮创建；`REPLSession` 不向这条 UI stream 投递 user prompt 或历史 session transcript（`repl_session.go:684-718,722-775`）。line editor 会在自身输入路径显示刚键入的文本，session store 也有持久化调用，但 UI 不能将它们作为可滚动、可搜索、可选择的同一 transcript。`/session` 命令已注册；其真实 resume 失败见总审计 04 的 Runtime/session finding，本报告不将命令注册等同于恢复可用。

### UI-07 · P2：steer/follow-up 显示的光标不随插入点移动，且缺少左右移动

TUI steer/follow-up 只实现 Backspace、Ctrl+A/E/W/U 和可打印 rune 插入，没有 `tea.KeyLeft/Right` 分支；View 无论 `steerCursor` 在哪里都把光标符号拼在完整输入末尾（`internal/tui/app.go:752-764,1488-1578`）。例如 Ctrl+A 后输入字符会正确插入到开头，但光标仍画在末尾。当前前置 LineEditor 有左右键，因此用户会遇到同一产品里两套编辑体验不一致。steer/follow-up 还没有多行或外部编辑能力。

### UI-08 · P2：历史 transcript 没有稳定的滚动/搜索窗口

默认窄屏或没有工具时，`renderViewLocked` 调用 `AccordionModel.Render()` 并输出全部 entries；只有宽度至少 120 且有工具时才走 `RenderView`，后者截断到最后若干行，并没有维护 transcript scroll offset（`internal/tui/app.go:674-738`、`accordion.go:279-306`）。Up/Down 改的是选中 entry，Enter 只折叠/展开；小屏用户无法翻页查看早期回答，宽屏路径也可能把选中项裁出视口。底部状态/输入/审批提示增加时面板只预留两行，可能挤出终端高度。长对话需要独立 viewport、滚动位置、follow-tail 状态和恢复焦点。

### UI-09 · P2：逐 token 全量 render 与重绘，累计 transcript 也不设上限

`handleIncremental` 每个 chunk 都取出完整累计内容，并对 `assistant` renderer 重新 render 全文；只有 ContentTypeMarkdown/Streaming/StreamingCode 才用稳定行缓存，而默认 bridge 类型为 assistant（`internal/tui/app.go:1222-1294,1297-1308`）。这里不假设 `strings.Builder.String()` 必然复制底层 buffer；性能问题是高频增量不断重复全文渲染，宽度限制触发 wrap 后会重复扫描/分配全文，而且 Bubble Tea 每次 Update 都要重建包含全历史 entries 的 View。累计 transcript 不设上限，工具结果的“20 行”只限制显示、不释放原文。长输出下渲染工作量可达 O(n²)。Transcript 虚拟化、chunk 合并/节流、行级缓存和保留策略需要一起设计。

### UI-10 · P2：Unicode 截断和单元格宽度处理不可靠

- `summarizeFirstLine` 文档说按 rune 截断，实际用 `len` 和 byte slice；含多字节 UTF-8 的长摘要可能在 code point 中间截断（`internal/tui/app.go:1383-1399`）。
- `BoxRenderer` 用 rune 数当终端宽度，CJK 双宽字符会造成边框错位（`internal/tui/box.go:19-44`）。`wrapWidth/stripANSI` 对 lipgloss 产生的 ANSI 序列不跳过控制码，函数名和注释称忽略 ANSI；按 rune 计列，既可能在样式转义序列中插入换行，也不会正确处理 CJK/组合字符/emoji grapheme（`internal/tui/renderers.go:33-59`）。
- 前置 line editor 自己用手写 `runeWidth` 近似 CJK 双宽；组合标记、肤色修饰符、ZWJ emoji 等按 rune 累加，显示宽度仍不准确（`internal/cli/line_editor.go:1203-1231`）。现有字符测试未建立针对 SGR 边界、CJK、emoji grapheme 的终端列宽基准。

### UI-11 · P2：审批交互有不能兑现的按键和缺失的撤销/取消出口

渲染字符串告诉用户 `[d] toggle diff`，但 handler 的 `d` 是 no-op；diff 实际始终展示（`internal/tui/approval_entry.go:86-106`、`app.go:1635-1657`）。pending approval 状态优先截走所有按键，handler 只接受 y/a/n，Esc、Ctrl+C、q 均无动作，因此审批等待期间用户不能通过 TUI 取消 turn；有问题的 ask_user 也没有 UI 答复通路（`app.go:1437-1441,1635-1657`）。

`a` 被命名为 always，但 middleware 的缓存 key 是 permission mode、tool name 和 args hash；它缓存的是相同调用，而非像 OpenCode 文档描述那样采用工具建议的 glob pattern。虽然 `ApprovalResponse.Pattern` 字段存在，TUI handler 不写此字段（`internal/approval/middleware.go:213-230`、`internal/tui/approval_entry.go:38-44`、`app.go:1643-1655`）。精确调用缓存是更窄的授权，但 UI 应显示其真实范围或实现 pattern 交互，不能让“always”语义含糊。审批还需支持取消并滚动查看大 diff。

### UI-12 · P2：帮助中的退出提示与 modal 实际行为矛盾

快捷键帮助列出 Ctrl+C “Force quit”（`internal/tui/app.go:797-816`），但帮助 overlay 只响应 Esc 或 `?`，其它输入都被忽略；测试还明确要求 overlay 中 Ctrl+C 不退出（`app.go:1442-1448`、`internal/tui/help_overlay_test.go:154-167`）。这不是测试与实现偶然不一致，而是可发现文案与行为冲突：应删除该 overlay 中的 Ctrl+C 声明，或实现全局中断并更新测试及取消契约。审批 overlay 也需要独立说明是否允许退出。

### UI-13 · P2：`diff_style` 声明配置但运行时不消费；主题也只覆盖部分组件

`TUIConfig.DiffStyle` 支持 unified/split/auto，REPL 将配置传给 `WithDiffStyle`，但构造后只存入 `diffStyle` 字段；源码除选项赋值和测试外没有读取点（`internal/config/types.go:433-446`、`internal/cli/repl_session.go:750-756`、`internal/tui/app.go:219-221,285,512,573`）。同日总审计 AST 扫描也记录 `WithDiffStyle` 只由 `_test.go` 引用（`docs/audits/2026-09-24/evidence/make-scan.log:852`）。审批渲染永远按同一种统一 diff 输出。

主题 manager 会更新通过主题参数渲染的普通 renderer，但 help、approval、accordion tool icon、todo panel 等仍有模块级硬编码颜色（例如 `app.go:791-795`、`approval_entry.go:46-56`、`accordion.go:346-350`）。因此配置主题并不覆盖整屏；应把样式统一收进 Theme。无颜色/低对比场景需要单独验收。

### UI-14 · P2/P3：终端尺寸回退和跨平台验证不足

`internal/term` 的 Darwin/Linux 实现使用 `TIOCGWINSZ`；其余 GOOS 返回 `(0,0)`，`GetSizeOrDefault` 默认 80x24（`internal/term/term_size.go:4-29`、`term_size_darwin.go:1-28`、`term_size_linux.go:1-28`、`term_size_other.go:1-8`）。`DefaultTerminalSizeProvider` 在无法查询时同样回退 80x24；Bubble Tea 启动后可收到 WindowSizeMsg，但非 Darwin/Linux 的首次布局只依赖回退值（`internal/tui/terminal.go:18-75`、`app.go:614-624`）。不能据此说 Windows 完全不能工作，但得补充 Windows 和其他目标 OS 的真实终端验收。当前 `internal/term` 零测试，CLI 的 SIGWINCH 文件也只覆盖 Unix/other build tags。

### UI-15 · P3：无障碍和纯 ASCII terminal fallback 没有验收合同

静态源中未发现屏幕阅读器/辅助技术语义、无障碍键盘模式、纯 ASCII glyph 开关或高对比度验收。此项是验证缺口，不是本次在屏幕阅读器上实测失败；应纳入产品兼容策略后测试。

### UI-16 · P3：公共 UI Send 队列满时静默丢弃消息

`BubbleteaApp.Send` 的内部 `send` 在 `msgCh` 满时走 default 分支直接丢消息，调用者没有返回状态、重试或日志；`app_extended_test.go` 明确把“queue full drops”作为当前行为验收（`internal/tui/app.go:985-992`）。核心 EventStream 不走这条队列，因此当前主要影响可选/外部推送消息；后续若把 Todo、审批、错误等必须展示事件迁入此 API，应提供可观测的背压或可靠交付语义。

## 关键旅程的行为与投影闭环

1. **输入提示 → 提交**：raw-mode LineEditor 的左/右/Home/End、删除、历史、completion、Ctrl+R 均在 `internal/cli/line_editor.go`；它能提示命令补全，但与运行中的 TUI input box 不共享状态。用户输入和历史 session 未构成同一个 TUI transcript。
2. **发起 Agent → 流式输出**：核心 `message` incremental 被 bridge 投影为 assistant，结果进入 AssistantRenderer 而非 MarkdownRenderer。流完成后完整 message 被跳过，但 REPL 的 `done` 又携带完整答案进入 StatusRenderer，因此视觉输出不止一份。
3. **工具调用 → 查看结果**：核心发送 `pre_tool_call`（空 Content，业务信息放结构体字段）和 `tool_call`（Content 只有工具名）；bridge 未把结构体字段保留下来。UI 能按 ToolCallID 把 tool_output 和 tool_result 归组，但调用参数只在另一路 `ApprovalRequest.Args` 审批时显示。事件类型字典必须与 Runtime 合同一起修，不能只加 renderer。
4. **需要审批/补充问题 → 回答或拒绝**：approval channel 接到了 Bubble Tea，但单槽 pending 状态不支持并发队列、不可取消；HITL 使用 Program.Send 的另一种 message，却没有 Update 分支，用户无法回答。Approval 和 HITL 应共享可发现、可取消且请求 ID 明确的交互状态机。
5. **继续/恢复会话 → 回看上下文**：Session slash handler 已注册树、分支、恢复等 CLI 命令；`renderTUI` 每轮重建 App，只呈现本轮事件，不从 SessionTree 构建完整视图。总审计 04 已验证实际 `/resume` 因 fileID/entryID 使用错误而失败，因此此处只评价 TUI transcript 暴露，不把已注册命令写成可用恢复。状态栏显示 session ID 前 8 位、model/turn 及有数据时的 token/max 百分比和 cost（`app.go:856-899`）。旧 `integration-audit.md` 称状态栏没有实时窗口占比（`docs/integration-audit.md:20,167,213`），此结论已过期；现在显示的是 token input+output 相对 max token 占比，并不必然等价于精确 provider context usage。

## 迭代计划与验收门槛

### UI-0：闭合交互和事件合同（最高优先级）

- 为 Core→TUI 定义并版本化单一展示合同，明确 message/delta/done、tool call arguments、pre-tool、tool output/result、todo、subagent、approval、HITL、usage、error、cancel 和时间戳的投影策略；status 只表达状态，不承载最终答案正文。
- 实现 HITL 的 Bubble Tea Update 路径和可取消问答状态；审批做排队或显示并发请求清单，任意取消、退出、超时与通道关闭都要回复/解除每个等待者。删除 `[d]` 假操作或做出真实 toggle；帮助键表要和实际 key handler 由同一注册表生成。
- 保证最终 assistant 输出恰好出现一次；approval 和 ask_user 有请求 ID、焦点、错误/取消提示，Agent context 取消后能释放 UI 等待，不留原始终端模式。
- 验收：用 mock EventStream 和真实 `teaModel.Update` 写端到端 model-flow 测试，覆盖增量+最终 message+done、HITL answer/cancel、两个并发 approval 的往返、工具 error 和 turn cancel。所有成功和失败旅程都断言最终展示次数、等待 channel 已闭合、terminal restore 被调用。该项仍属于 model/integration test，不称视觉 E2E。

### UI-1：完善实时会话浏览和编辑

- 让每轮 TUI 显示用户 prompt 和 Runtime assistant/tool 事件；在总审计 04 修复真实 `/resume` 的 fileID/entryID 缺陷后，再从恢复出的 SessionTree 重建当前分支。增加独立 viewport、PageUp/PageDown/首页/底部、可选 transcript search、follow-tail；保留工具折叠和当前焦点。
- 统一输入体验：steer/follow-up 光标左右移动且显示位置准确，支持多行/Bracketed Paste 和外部 editor；明确 Enter、Shift+Enter、Tab completion 与 Esc/Ctrl+C 在输入、审批、搜索和运行状态的含义。将 session tree/resume 从 slash-only 提升为可发现的选择器或命令 palette，仍保留脚本/CLI slash 操作。
- 验收：mock 有历史 session + 三轮任务 + 多工具调用，测试 restored branch、当前 user message、回看/搜索、队列中止/继续；人工 PTY walkthrough 覆盖空输入、长行中间编辑、CJK 输入、paste、ctrl-c/esc、approval focus 和 60/80/120/200 列 resize。

### UI-2：渲染正确性与工具/审批信息

- assistant message 在标准主路径调用 MarkdownRenderer/StreamingMarkdownRenderer；显示工具 arguments（敏感参数按既有 Runtime policy 脱敏）、执行状态、stdout/stderr、error 和结果；todo/subagent 仅在 Core/CLI 有明确事件来源后呈现，不把未接线组件计为完成。
- 把 diff style 做成真正可配置 renderer，统一主题 token；对输出的 ANSI sequence 做状态机解析或先剥离再 wrap，所有宽度按 terminal cells，而不是 byte/rune 数。对摘要用 grapheme-safe 截断。
- 验收：golden terminal-cell cases 覆盖控制色码、OSC hyperlink、CJK、组合音标、emoji/ZWJ、ANSI 清屏文本；统一 theme / NO_COLOR 断言。`diff_style=unified|split|auto` 都以配置驱动的实际 preview 结果验收。

### UI-3：大输出性能、可访问性与平台验收

- 采用 viewport/行缓存或受控历史上限，合并高频 token repaint，避免每 token 全量扫描、render、重绘 transcript。为完整 tool payload 和旧 transcript 定清可见窗口与可取回存储策略。
- 明确 Unicode glyph fallback、色彩/对比度、键盘可发现性与辅助技术支持范围。为不支持 TIOCGWINSZ 的平台提供可靠动态尺寸路径或限制说明，并验证 Bubble Tea 和 LineEditor 的 terminal lifecycle。
- 验收：benchmark 逐步增长到 10k events 与超长 streamed message，记录 render CPU/alloc、峰值内存及输入延迟；PTY 验收 Linux/macOS/Windows Terminal（含窄屏 resize、SIGINT、正常退出、panic/error restore）；若声明支持 screen reader，再加入至少一种终端辅助技术的手工验收记录。

交付门槛应由测试层次分别记录：model/renderer 单元测试、CLI×TUI 集成测试、真实 PTY 用户旅程、终端视觉截图/人工验收。现有被命名为 E2E 的 renderer/channel 测试只能支撑前两类，不能替代后两类。

## 文档对齐

README 中英文版将 TUI 描述为 Bubble Tea、按 ContentType dispatch、支持 streaming renderer，并称有 26 个 built-in renderers（`README.md:250-252`、`README.zh-CN.md:250-252`）。组件层面的描述大致成立，但它隐藏了实际默认事件投影只给 assistant/status 等类型、ask_user 无 handler、markdown renderer 未进入默认路径等差异；建议把“可注册 renderer 能力”和“默认 REPL 已接线能力”分开。

`docs/source-audit-2026-08-07.md` 仍称主 UI 为“手写 ANSI 零依赖”，还引用不存在的 `keyboard.go` steer 路径（该文档 `64,145,245-247`），与当前 Bubble Tea `app.go` 和前置 `line_editor.go` 双层输入架构不符。`repl_session.go:674-679` 的注释也仍称 Esc 由 `keyboard.go` 的 `keyboardLoop` 处理，和当前 `renderTUI` 把 stdin 交给 Bubble Tea 的代码不匹配（`repl_session.go:722-728`、`app.go:349-367`）。`docs/integration-audit.md` 是 2026-08-07 静态审计，关于上下文占比、Subagent/TUI 接线等语句应依当前源代码与此报告重核，不宜原样当作当前状态。建议在总审计中标出旧文档基线日期并集中维护唯一 UI contract。
