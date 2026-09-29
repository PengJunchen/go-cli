# R8 产品能力与默认入口合同

状态：产品选择冻结稿，供 R8-BLD-02 实现和 R8-REV-01 验收使用。仅选择现有范围内的能力，不增加产品功能；本文不是测试报告或发布结论。Memory manual-only、Serve fail-closed、远程 TUI unavailable、硬费用预算 unavailable、Plan Mode 仅用户切换等是本轮明确选择。

源码基线：本次静态复核的 checkout `ea8721d`。相对早先 `1144e1e`，当前 HEAD 还包含 R1 stream ownership 和 Memory JSONL file-store followup；本稿的 capability observations 已按当前源码核对。`docs/iterations/2026-09-25-runtime/r8-remaining-source-contract.md` 以 `07dddc1` 为基线，部分缺口已变化：当前已有 transcript viewport primitive 和对应单测，但默认 TUI 尚未接入。`4de967cf3459a99eeed38f8ae69c231141f8dfd2` 是不在当前 checkout 的 durable CLI builder 候选，仍须独立 review；本稿把它当作待审核合同，不记作当前入口行为或验收。R5 app wiring 与 R5/R6/R7 reviewer 前置项仍需各自结案。未运行测试、PTY 或 Docker。

## 选择规则

默认入口链是 `cmd/cli.main` → `cli.runCLI` → `interactiveCmd.Run` → `REPLSession.setupAgent` → `AssembleAgent`。能力以这条链最终装配后的 tool registry、REPL、持久化 sink 和用户可执行命令为准。单有 package、handler、renderer 或配置字段不构成产品能力。配置型能力须标清启用条件；没有实现的能力应从对外说明中移除，或给出明确 unavailable/unsupported 结果。`/tools` 应继续读取当前真实 registry。

公开状态沿用 R8 原合同：`supported` 要有所声称入口的实际 integration/restart 证据；`partial` 必须在操作前显出限制；`unavailable` 表示当前没有接受的默认闭环或产品承诺。此稿冻结的是选择和验收边界，不把候选 commit、库 API、配置字段或目标测试名升级成 supported。表中手动选择的 unavailable/降级项优先于 9 月 25 日设计建议中的开放选项。

当前入口发现面并不是 capability status 系统：根 `--help`/`help` 列出 CLI commands，REPL `/help` 列出 slash handlers，`/tools` 列出实际 tool registry，`/config` 只打印少量配置摘要。`doctor` 运行环境、配置和连接诊断，不装配 Agent，也不验收用户可执行能力。R8 实现须在现有 `HelpHandler` 的 `/help` 输出中增加一个静态、人工维护的“能力边界”说明段，并让相应 slash/tool descriptions、config errors 和用户文档与之保持一致；不得从 package 是否存在或 config 字段是否能 parse 推导 live capability。该文字段不是状态 registry，不被代码当作第二套 capability 权威，也不替代实际注册结果和 typed errors。R8 验收须分别核对这些真实输出与 unavailable/partial 调用结果；本文矩阵当前不声称已出现在入口输出中。沿用 R8 禁止空壳成功和误导性命令描述的要求：`/tools` 只列真实 registry，工具以真实装配条件、工具描述和实际调用错误为准；没有可调用入口的 unavailable 项仍须由 `/help` 和用户文档说明。不得把 `doctor` 的通过解释成某项 Agent 功能可用。

`/help` 的静态“能力边界”说明须使用用户能直接理解的文字，并至少说明：

- Memory 只由用户通过 `/memory` 管理；不会在后台自动提取，也不会把已有记录自动发送给模型。
- Goal、Task、Todo 数据是临时的，当前进程结束后不保留。
- Session resume 在候选 builder 未独立 review、接入和验收前标为 unavailable；之后也只支持配置 session directory 后按明确 ID 恢复。Fork、branch switch、snapshot、migration 不支持。
- 默认不能读图，也没有可用的 Web Agent UI；未配置真实搜索 provider 时，Web Search 不可用。
- Remote TUI 不可用。`serve` 不支持 `ask` 审批；`serve --help` 不得继续把 `ask` 标为默认可用模式，必须要求显式选择 `auto` 或 `deny`。
- Hard cost budget 不可用；usage 与 cost 显示必须区分实际值、估算值和未知值，不能把未知显示成零。
- 只有用户明确执行 `/plan enter|exit` 才能切换 Plan Mode；模型不能替用户切换。
- Theme 只覆盖已接线的界面区域；diff 仅支持 unified，ASCII 模式不可用。
- MCP server 配置热更新尚不支持；列明已验证的平台/终端范围，不把未经 PTY 验收的平台描述为支持。

根 `--help`/`help` 继续只列真实 CLI commands，`/config` 只说明已存在的配置值/限制且不能暗示 unsupported mode 可用；`doctor` 不显示 capability-ready 假状态。显式调用已暴露但不支持的操作必须返回可识别的 typed unsupported/error；无 provider 的工具须明确说明配置条件，不得仅凭 package 名、默认注册或 `mock` 配置宣称真实外部服务可用。

| 能力 | 当前源码行为 | 本轮选择与最小动作 |
|---|---|---|
| `image_read` | `ImageReadTool` 有实现和单测，但 `RegisterDefaults` 与 `assembleExtraTools` 都未注册它。TUI 的 `ImageRenderer` 只渲染已有 image 内容，不会读取文件。 | **默认不可用。** 不注册占位工具，也不把 renderer 说成读图。默认 `/tools` 中不得出现 `image_read`。只有另行选定 multimodal 默认旅程后，才做 provider 端到端验证。 |
| Goal / Task / Todo | `assembleExtraTools` 每次 assembly 新建 `TodoStore`、`TaskStore` 和 `NewDefaultGoalStore("")`；这些 store 都在内存中。工具能在同一次 assembly/REPL 生命周期内创建、更新、列出。 | **仅当前进程/assembly 有效。** 不承诺重启恢复，不为本轮增加持久化。命令或说明必须明确其生命周期；若产品以后选择重启保留，再新增文件路径、迁移和重启验收。 |
| Memory 手动操作 | 有 home 目录时使用 `~/.go-cli/memories.jsonl`；`/memory list|add|delete|search|clear confirm` 操作此文件。`Search` 接受 limit，手动搜索最多返回 10 条。无可用 home/store 时命令不可用。当前 file store 在 `bf11439` 已加入 append/delete 故障处理和 1 MiB 行限制，但 REPL 仍默认抽取并注入 memory，真实 CLI 子进程重启旅程尚未验收。 | **保留手动持久化。** 保留当前 JSONL 文件层修复与 store-failure tests，不引入数据库或新的存储后端；补真实 CLI 正常退出/重启回读验收。该验收不证明多进程并发或断电级耐久，也不等于 automatic extraction/injection 被接受。 |
| Memory 自动化 | 设置 SessionStore 时，`persistSession` 会异步抽取对话事实并写入 store；装配时又对 `List` 全量读取，作为固定 `MemoryEntries` 注入 system prompt。默认注入没有条数/token 预算，也没有按本轮 query 检索；`MemoryRetriever`/`Search(limit)` 未接到该入口。 | **按 manual-only 发布。** 默认路径不调用自动 extractor，也不把现存 memory 注入 prompt；保留 `/memory` 手动 CRUD/search。此项是明确的隐私与上下文边界选择，不是因为 bounded selector 尚未接线；它明确 supersede `r8-capability-design.md` 先前保留自动注入的建议。无新 `memory.auto_inject` 或预算配置键，也无隐式自动 fallback。 |
| Session resume | 当前 `REPLSession` 只解析 legacy `--resume` 布尔值；`assembleSession` 仍直接用 `session.store_path` 构造 JSONL store。`durable_cli_session.go` 中有 helper，但未连接 prompt/REPL；另有 `4de967cf…` builder 候选不在当前 checkout。 | **只选择精确物理 session resume。** 以 `4de967cf…` 候选为待验收实现，不声称当前 `ea8721d` 入口已具备。review 须核对 `session.store_path` 目录语义与既有 JSONL 文件路径兼容；旧文件路径不得被静默当目录。当前 `--resume` 不得暗中选择“最新” session；空路径明确说明 session persistence 未配置。 |
| Branch / fork / switch | session tree、branch/clone、snapshot 与 migration API 存在；Slash registry 也有 `/session tree|fork|resume|branches|clone|switch`，但新 durable boundary 不支持 branch selection/mutation。 | **Durable fork、branch switch 与 session mutation 本轮 unavailable。** 保留 typed unsupported 错误并让 CLI 原样给出可理解结果；不能因旧 slash 命令或 package API 存在就宣称可用。 |
| Snapshot / migration | `tools.SnapshotManager` 是 Git 文件修改的 `/revert` 辅助，不是 session checkpoint。`internal/session` 有 snapshot/migration API，但无已验收的默认 CLI 保存/恢复/迁移旅程。 | **不承诺 session snapshot/migration。** 区分文件 revert 与对话分支。未选定迁移策略前，不自动改写既有 session 文件。 |
| Web Agent UI | `internal/web.WebServer` 可独立启动并提供页面/health；chat 与 stream handler 明确返回 503，且默认 CLI 没有启动它的入口。 | **默认 Web UI unavailable。** 不把页面、health endpoint 或 API package 宣传成可用 Agent UI；无需加空壳成功响应。 |
| ACP / `serve` / remote Ask | `serve` 提供私有 `go-cli-acp-json/1` HTTP profile；路由为 `/connect`、`/send`、`/disconnect`、`/stream`、`/events`，无远程 approval answer 路由。当前 `--approve` 零值为 `ask`，flag help 也宣传 `(default ask)`；assembly 失败或 config 不可用会进入 echo/ack 模式。认证 token 目前在 assembly 前生成并可落盘。 | **只承诺私有 HTTP profile；不宣称官方 ACP 或 durable session。** `serve` 必须先成功装配真实 Agent runtime，再生成/写入 token 或启动 listener；assembly 失败立即返回清楚错误，不得启动 echo/ack 或输出 Agent 成功暗示。没有真实远程 approval 路由时，remote `ask` unavailable；无论是隐含默认还是显式 `--approve ask`，都应在 provider/runtime/token/listener side effect 前给出清楚错误，要求显式选择 `--approve auto` 或 `--approve deny`。同时修正 `serve --help`，不能继续宣传 `ask` 是默认支持模式。`auto` 仍须满足现有认证约束。持久 session、fork 等 typed unsupported 必须在 HTTP 请求边界透传，不能以 HTTP 成功伪装完成。 |
| Remote TUI | 已有 `tui.mode`/`tui.remote_url` 配置和 `ACPStreamAdapter`。REPL 在 remote 模式把 SSE GET 事件作为 TUI 输入，但 `runAgentGoroutine` 仍对本地 `TurnRunner` 调用 `RunTurn`；adapter 没有远端提交/会话/审批方法。 | **Remote TUI unavailable，fail closed。** 在任何 provider 或 tool assembly 前拒绝 `tui.mode: remote`，不能把该模式当作本地 agent 的远程 UI，也不能静默退回 local。保留 `ACPStreamAdapter` 为只读 library adapter；只有完整 authenticated submit/session/approval dispatch contract 及真实端到端验收后才能启用现有配置路径。 |
| Web search / fetch | 默认 `web_search` 使用无 provider 的 `WebSearchTool`，调用返回 `ErrWebSearchProviderUnavailable`；不会返回 mock 示例结果。`brave`/`fetch` 是显式配置路径。`web_fetch` 是独立 URL fetch 工具，不等同搜索。 | **默认搜索 unavailable，配置后才可用。** 保留 fail-closed；`web_search` 的工具说明和调用错误要显示 provider 未配置。`/tools` 可以如实列出已注册定义，但不得把注册当成 live search。测试路径使用显式 mock，不得把 mock 结果展示为真实搜索。 |
| MCP reload | 可从配置或 `.go-cli/mcp.json`、`~/.config/go-cli/mcp.json` 加载 server。当前源码为已连接 client 建立 per-client `HotReloader`，但该运行路径不是已验收的配置 generation 更换。`mcp-generation-design.md` 是独立待 review 的设计，尚非已接受 runtime 行为。 | **本轮不把 per-server reconnect 或文件编辑后的工具更新列入受支持产品能力。** `/mcp reload` 只能根据本次确实注册的 reloader 回报结果；没有对象时明确提示，command/help description 不得暗示完整配置 reload 已受支持。server 增删改、完整 config reload、工具 generation publication、旧 dispatch lease 与错误可见性留在独立设计 review/实现/验收之后。 |
| Skills / plugins | Skill 可从配置 `skill.dir` 加载，或发现项目 `.go-cli/skills` / 用户 `~/.config/go-cli/skills`；项目自动发现受 trust gate 约束，工具 deferred-register。Plugin 仅在 `extensions.enabled=true` 且提供 `plugin_paths` 时加载；Load/Init 错误会使 assembly 失败。扩展接口是本项目接口。 | **条件能力，不是空环境默认功能。** `/tools` 和实际 prompt 为准。Skill 加载失败要留可诊断日志；plugin 错误不能报成成功。不要称本项目 plugin API 为 Pi/OpenCode 或其他官方扩展协议，也不以 package 存在宣称标准互通。 |
| Theme / diff / ASCII | `theme` 配置会进入 TUI theme manager 和 renderer，但 help、approval、accordion icon、Todo panel 等仍有独立样式。`diff_style` 被存到 app/model，但渲染路径不读取；`TUIConfig` 还把空值默认注释为 `auto`，validator 当前接受 `split`/`auto`。实际 diff 输出为 unified。无 `ascii` 配置；`NO_COLOR` 只关闭颜色。 | **Theme 标记为 renderer 子集支持。** 只承认 unified diff：显式 `split`/`auto` 必须在配置加载时清楚拒绝，空值/default 解析为 `unified`；更新字段注释/帮助以消除 `auto` 默认说法，不能静默 no-op。ASCII glyph mode 标记 unsupported；plain/`NO_COLOR` 不等于 ASCII 或屏幕阅读器支持。 |
| Tracing / export | 由 R7 lifecycle/exporter 合同权威描述；R8 不重做 exporter 协议或生命周期审计。 | **仅引用 [R7 lifecycle/export 合同](../2026-09-25-runtime/r7-lifecycle-design.md)。** 不在本文或运行时新建第二套能力 registry；本行也不把 R7 的目标状态记成实现证据。 |
| Hard cost budget / usage report | `Config` 没有 cost budget 字段；assembly 创建 `NewCostTracker(nil)`，但未设置 positive limit。CostTracker 库虽有 `SetBudgetLimit`，并不代表 CLI 可执行 hard cap。生产 wrapper 仅记录带 usage 的成功 `Generate`；stream 路径不记录。当前 `/cost`/token status 对未知用量或价格仍可能显示数值零。 | **Hard cost budget 本轮 unsupported。** 不新增配置键，不把 unknown usage/price 当作 0 或金额门限可用；若接入层收到声称 hard budget 的配置，必须在 provider dispatch 前清楚拒绝或明确降级。R3 per-operation accounting 仍是必需工作，不随本次 budget 选择 defer：每个 operation 都须报告已知 actual-usage subtotal 和 unknown-usage attempt count，并保留实际 provider/model/provenance；可计算价格单独标 estimated/provenance，unknown price 不能变成 `$0`。 |
| Plan Mode transitions | 默认装配把 `enter_plan_mode`/`exit_plan_mode` 注册为 trusted controls，prepared policy 也将二者列入 Plan Mode allowlist；`/plan enter|exit` 可由用户显式调用。 | **只允许用户显式切换 Plan Mode。** 保留 `/plan enter|exit`；首选从模型可调用 tool registry 移除两个 mode-transition tools；若为兼容仍保留定义，则须在现有 prepared-policy seam 对这两个 model calls 作无条件 deny，且在 `auto_full` 分类前生效。单从 trusted-control allowlist 移除不够，因为当前 `auto_full` 分类会直接允许所有 control effects。模型尝试不改变状态。保留该 seam 的现有 fail-closed 语义，不增加新 policy framework。验收须覆盖模型控制调用在 `auto_full` 下仍被拒与用户 slash 操作成功两路。 |
| Transcript viewport | 当前有 `internal/tui/transcript_viewport.go` 及 `TestTranscriptViewportSearchAndScroll` 等 primitive 测试；`app.go` 与 `repl_session.go` 未接入该 viewport。父级已接受 renderer/backend viewport primitive 和 Send backpressure，R5 app wiring 仍待 review/验收。 | **默认 UI 暂不宣称 transcript search/scroll 已完成。** 先完成真实 REPL app wiring 和 R5 reviewer 前置，再做 PTY 旅程；primitive 单测不等于可用功能或长 transcript 性能证据。 |

## Memory JSONL 文件层与 CLI 持久化验收边界

当前 `internal/memory/file_store.go` 的文件层已包含 `bf11439` 的修复：`Add` 检查 1 MiB 行上限、完成短写并在更新内存 index 前同步；append 或 sync 失败时尝试 truncate+Sync rollback，rollback 任一项不能确认就以 typed poison error 阻止后续写入。此时内存不发布失败记录，但磁盘物理记录状态保持 unknown。`Delete` 在内存状态不变的情况下构造候选记录，完整写入并同步临时文件，再以 rename 为提交点；rename 后 reopen 失败保留与新文件一致的 index，并将 store 标记为 unavailable。rename 前失败保留旧 index；若恢复 append handle 也失败则 store unavailable。文件层没有新增恢复 ledger、跨进程锁或父目录 fsync 承诺。

源码树已包含下列 fault-injection tests，但本设计复核没有执行它们，也不把 test registration 当成通过证据。R8 builder/reviewer 仍须在目标 checkout 记录所需验证结果，并补上真实 CLI 子进程重启旅程：

1. `TestMemoryStoreAppendFailureDoesNotPublishMemory`、`TestMemoryStoreAppendCompletesShortWrites` 与 `TestMemoryStoreAppendFailurePoisonsWhenRollbackIsUncertain`：覆盖确认 rollback 后的原状态、短写完成、rollback truncate/Sync 不能确认时 poison 和 typed fail-closed。当前实现的 poison gate 阻止后续 `Add/Delete`；`Get/List/Search` 仍返回故障前的 index，因此不能将当前读结果解释为磁盘物理记录不存在。确认 rollback 后可关闭重开读到旧状态；未确认时不得声称重开后失败记录不会出现。
2. `TestMemoryStoreRejectsOversizeRecord`：超过 1 MiB JSON row limit 时，在写盘和更新 index 前拒绝；正好达到上限的行（LF 不计入 row limit）可重开读取，超限既有行必须拒绝加载。
3. `TestMemoryStoreDeleteReopenFailureConsistent` 与相关 delete failure tests：强制 rename 成功、reopen 失败；当前实例与重开 store 对记录存在性一致，并阻止不可用 handle 上的后续写入。Rename 前失败保留旧 index；rename 后不能恢复旧 index。
4. 真实 CLI 子进程旅程仍待补：进程 A `/memory add` 后正常退出；进程 B `/memory list` 和 `/memory search` 读回相同记录。此项验证手动持久化，不证明自动注入或断电耐久。

当前实现使用 store-local 的小型 file-operation seam 做故障注入；不扩成通用存储框架。按本稿选择，验收默认 REPL 不调用 extractor、不把现存记录注入新 prompt，手动 search 仍可用。

## Session 与协议验收边界

待 review 的 durable CLI builder 应以真实 `cmd/cli` 子进程进入，而不是直接调用 store API：进程 A 在目录型 `session.store_path` 下完成一轮并退出；进程 B 使用完全相同的物理 session ID 启动 `--resume-session`，确认 PreparedSession 的历史进入下一轮 prompt/transcript、commit sink 继续写入同一物理会话，且没有重放不确定的工具副作用。若传入 branch ID 或请求 fork/switch，必须在变更前返回 typed unsupported，磁盘不多出分支或半提交。

该验收仅支持“目录启用后按精确物理 ID resume”。它不证明 `/session fork`、跨进程自动 action replay、session snapshot、migration 或 ACP durable restore；R9 replay 继续 deferred。执行前需审查旧 `session.store_path` JSONL 文件配置的兼容行为。

## 终端、平台和容器矩阵

| 目标 | 当前可证事实 | 本轮公开边界与所需验收 |
|---|---|---|
| macOS Darwin arm64 | 有 Darwin `TIOCGWINSZ` 路径；既有 R5 记录含一次本机 native PTY/plain 旅程，覆盖 approval/detail scrolling、`ask_user` Unicode input、cooked-mode handoff 和 terminal restore。该证据不覆盖 R8 长 transcript viewport/search，也不是本次复测或 GUI/终端模拟器人工视觉检查。 | 可如实保留“有一项限范围的 native Darwin PTY 证据”；只有补完本轮长 transcript/viewport PTY journey 后才能把 R8 对应交互能力列为 supported。release reviewer 记录 OS/build、shell、locale、TERM、颜色变量、宽高，并分别报告 PTY 和 pipe/plain 结果。 |
| Linux amd64 | 有 Linux `TIOCGWINSZ` 和 Linux line-editor 测试源；本轮没有 Linux native PTY 结果。 | 暂列 source-present/unverified。取得真实 Linux PTY REPL/resize/interrupt 结果后才可列 supported。 |
| Windows | `internal/term` 走 other 平台的 80x24 回退；line-editor cancel 文件为 unsupported build；没有 native Windows PTY 结果。 | 不作交互 TTY 支持承诺；交叉编译不能替代 native PTY。 |
| 其他 GOOS | 尺寸实现返回 0x0，再由调用者回退 80x24。 | source fallback only；未验收前不列支持平台。 |
| Docker | `Dockerfile` 固定 `GOOS=linux GOARCH=amd64`，Alpine runtime；没有 ARM64 target 声明或镜像 smoke 证据。 | 只声明 linux/amd64 构建目标。可用 build metadata 验证目标；要宣称运行支持则在 amd64 容器中做 CLI `--help`/pipe smoke。不得暗示 ARM64。 |

pipe/plain 与交互 PTY 是不同产品行：pipe 应无 ANSI 控制序列；`NO_COLOR=1` 只约束颜色。二者都不能替代 CJK/emoji PTY 单元格、resize、IME 或屏幕阅读器验收。真实 terminal 支持不能单从 Darwin/Linux 的 build-tag 源码推导。

## Pi / OpenCode 工作流比较

仅参考截至 2026-09-29 可访问的官方文档，不引用二手评测，也不按功能数量追平：

- Pi 官方 terminal 与 session 文档描述多行/外部编辑器和 `@` 文件/图片输入、工具调用与结果显示、执行中转向/排队/取消、自动保存、`/resume`、`/tree`、`/fork`、`/clone` 与 session 文件定位：[Use Pi in the terminal](https://pi.dev/docs/latest/usage)、[Sessions and Context](https://pi.dev/docs/latest/sessions)。这验证的是 Pi 官方说明，不是本项目需要复制全部功能。
- OpenCode 官方 TUI 文档描述 `@` 文件引用、`!` shell、`/editor`、`/sessions`/`/resume`、导出、undo/redo、theme 和 details；权限文档说明 once/always/reject 语义：[TUI](https://opencode.ai/docs/tui/)、[Permissions](https://opencode.ai/v2/docs/permissions)。
- 本轮优先级应是：入口帮助说真话、选定 durable resume 可复验、长 transcript 在默认 REPL 真正可浏览、交互配置不静默失效。Fork、丰富 keybinding、Web UI 或扩展协议仅因竞品有而不自动进入 R8。

## Builder / Reviewer 执行门

1. R8-DES-01 已冻结本稿中的产品选择；builder 仍须核对 `session.store_path` 新目录语义与旧文件路径兼容、unified diff 默认值，以及 manual-only Memory 对原自动行为的显式替代。`4de967cf…` 候选必须先完成独立 review，再按提交核验后接入。
2. R8-BLD-02 按选择逐项验收根 `help`、REPL `/help`、`/tools`、`/config`、`doctor`、实际调用、进程重启和失败结果；不增加 runtime capability registry。待 review 的 durable CLI commit 必须先完成独立 review，再合入本分支并核验对应提交。
3. Memory fault contract 与正常重启旅程逐项验收：检查确认 rollback 后重开状态、rollback 未确认时 poison/typed fail-closed、1 MiB scanner 边界及 rename 后 reopen 失败。物理记录是否存在的 unknown 不得被写成“无半行”通过。保留 JSONL 作为单一文件实现。
4. R8-BLD-01 先由 R5 完成 viewport app wiring；再验证默认 REPL 的 search/scroll/resize、ANSI/CJK、streaming 增量和 ASCII 不支持说明。primitive/helper 级通过不替代实际 PTY。
5. 每项验收明确区分静态源码、单元测试、runtime assembly、子进程重启、协议 peer 和真实 PTY。`execution-tasks.json` 中标为 Planned 的测试名不代表已存在或已通过；精确测试注册检查、受影响包 race suite 与真实终端证据由 R8-REV-01 完成。

本稿冻结能力选择和最小验收合同。当前 checkout 的静态设计/源码检查不是测试通过、跨平台支持、协议 conformance 或 R8 release readiness 证明。
