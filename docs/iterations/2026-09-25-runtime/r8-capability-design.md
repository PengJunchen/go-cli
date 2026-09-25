# R8 终端质量与默认能力声明设计

状态：designer handoff draft，待独立 reviewer 审查后交 builder。本文是设计和验收合同，不表示终端矩阵、能力闭环或性能目标已经验证。

依据：`docs/audits/2026-09-24/05-ui-review.md`、`docs/audits/2026-09-24/10-runtime-iteration-plan.md`、`docs/audits/2026-09-24/proposed-tasks.json` 中 R8 任务、当前 `docs/iterations/2026-09-25-runtime/execution-tasks.json` 中 R8-DES-01/R8-BLD-01/R8-BLD-02/R8-REV-01，以及当前 `internal/tui`、`internal/term`、`internal/cli` 装配源码。任务文件是验收与范围权威；05 给出缺陷基线；10 给出阶段切分。

R8-DES-01 依赖 R5-REV-01、R6-REV-01、R7-REV-01。渲染和能力实现还消费 R1 的流/终态合同、R2 的 durable transcript 与真实身份映射、R3 的 memory/query 能力、R4 的授权策略。builder 等待这些接口经过 review；TUI 不为缺失接口补造身份、持久性、功能或授权结论。

## 当前源码基线

- `internal/term/term_size_darwin.go` 和 `term_size_linux.go` 查询 `TIOCGWINSZ`；`term_size_other.go` 返回零尺寸，`GetSizeOrDefault` 和 `DefaultTerminalSizeProvider` 回退到 80×24。Bubble Tea 收到 `WindowSizeMsg` 后可再调整窗口，但 R8 前没有真实 TUI PTY 平台矩阵，`internal/term` 也没有测试。此证据不能推导 Windows 一定不能运行，也不能据此宣称其交互尺寸可靠。
- `resolveColorProfile` 遵循 `NO_COLOR`、`CLICOLOR`、`CLICOLOR_FORCE` 和 TTY/profile；`color_profile_test.go` 覆盖了关键环境优先级和 `NO_COLOR=1` 无 ANSI 行为。`NO_COLOR` 只承诺关闭颜色，不等于 ASCII glyph 模式或屏幕阅读器支持。
- `internal/tui` 已有默认 renderer registry、ANSI 样式和 `WindowSizeMsg` 更新，但 05 确认 transcript viewport/search 缺失、逐 token 对 assistant 全量重渲染、历史 View 重扫、byte/rune 截断和列宽误差、`diff_style` 未消费及主题覆盖不完整。默认 `ImageRenderer` 只说明 renderer registry 能处理 image content type，不代表 CLI 注册了 `image_read` 工具。
- `internal/cli/assemble.go:1227-1259` 注册 Goal/Task/Todo/Web 等默认工具；Todo/Task store 是进程内存，Goal store 用空路径创建（空路径代表仅内存）。它们在当前进程可用，但没有重启持久化合同。
- 同一装配路径创建 `~/.go-cli/memories.jsonl`，启动时把 `MemoryStore.List()` 的全部记录传给 system prompt，并在 turn 后启动 extractor 写入。`/memory` 命令提供 list/add/delete/search/clear。`MemoryStore.Search` 已有带 limit 的 TF-IDF 实现，`MemoryRetriever` 接口也存在，但默认 assembly 未接线选择器，仍把全部 `List()` 结果注入；因此不能把存储层 search 说成默认 prompt 已实现有界检索，也不能以默认路径尚未使用 search 为由删除已有自动注入行为。
- `internal/tools/image_read.go` 有实现和单元测试，但默认 `assembleExtraTools` 没有注册它。`SessionHandler` 暴露 tree/fork/resume/branches/clone/switch；05 已记录默认 `/resume` 闭环缺陷，身份与恢复要以 R2 结果复验。`internal/session.DefaultSnapshotStore` 是内存库，默认 CLI 未接线；`MigrationChain` 也未在默认恢复路径使用。文件写入的 pre-mutation snapshot/`/undo` 是文件回滚能力，不是 session snapshot。
- 扩展插件仅在配置启用且有 plugin path 时装配 tools/hooks/middleware；Skill 从配置或本地发现目录加载并以 deferred tool 注册。这些条件路径不能当作所有默认运行都具备的能力。05/10 还指出 extension commands/providers/remote RPC、Skill progressive registry、Web Agent UI 未形成默认端到端承诺。

这些是源码和审计基线，不是本轮运行测试结果。R8 builder 须用默认 CLI 装配测试重新确认最终状态，不应复制本段当作通过证据。

## 终端与编码支持矩阵

推荐将下表作为 R8 后唯一公开矩阵。当前状态是源码能证明的上限；“目标验收”通过前不得升级为 supported。手工 PTY 证据要注明 OS 版本、终端版本、shell、locale、TERM、色彩环境和列宽。

| 目标 | 当前状态 | R8 通过后可声明 | 必要证据 |
|---|---|---|---|
| Linux 原生交互 TTY（xterm-compatible PTY；至少一个常见终端及 tmux 代表环境） | partial：有 ioctl 尺寸路径，无 TUI PTY 矩阵 | 在列出的具体终端上 supported；未测的 emulator 不自动继承 | PTY 旅程覆盖输入、approval/HITL、长 transcript、缩放/resize、Ctrl+C、正常退出及终端状态恢复；ANSI 与 UTF-8 fixture；记录精确环境 |
| macOS 原生交互 TTY（Terminal.app；建议另测 iTerm2） | partial：有 ioctl 尺寸路径，无 TUI PTY 矩阵 | 仅对实际验收的终端版本 supported | 同一组 PTY/手工检查；尤其检查 CJK、组合字符、emoji、resize 后焦点/滚动锚点 |
| Windows 原生交互终端（Windows Terminal + PowerShell） | unavailable：现有 `internal/term` 分支回退 80×24，暂无 native Windows PTY 证据 | 只有 native Windows PTY 证明初始化尺寸、动态 resize、按键与 console restore 后才可升级；交叉编译不能替代 | native Windows CI/手工 PTY 记录；测试 WindowSizeMsg 更新、滚动/选择稳定、Ctrl+C 和退出复原；未通过时文档明确不支持 interactive TUI |
| 其他 GOOS 交互 TTY | unavailable：通用分支无动态尺寸实现/证据 | 逐平台验收后再逐项增加，不按 Darwin/Linux 结果外推 | 对应 GOOS 的尺寸、键盘、resize、render PTY journey |
| stdin/stdout 重定向、CI pipe | supported 为纯文本/非交互消费模式，不是 TUI | 维持无意外 cursor/alternate-screen/control sequence 的 plain output；颜色仅按已有环境规则显式覆盖 | 非 TTY subprocess 测试捕获 stdout/stderr；`NO_COLOR` 和默认 pipe 输出分别断言；不把 80×24 fallback 描述成可交互 resize |
| UTF-8 文本与 ANSI SGR/OSC-8 | partial：基础字符串/样式存在，列宽、裁剪和控制序列边界未达合同 | supported 仅表示已验证的 UTF-8 和允许序列子集；非法/其他编码无承诺 | cell-width golden cases、截断边界、SGR reset 和 OSC-8 开闭配对；确保用户内容不能注入未处理的 terminal control sequence |
| 颜色 | profile/environment 行为有单测；TUI ANSI布局还未通过 | `NO_COLOR`、默认非 TTY、强制颜色遵循现有解析合同；具体色深与终端能力需逐项验证 | 保留 `NO_COLOR=1` 产品行为测试；truecolor/256/无色终端 PTY 观察；颜色不是审批/错误等状态的唯一信号 |
| 非 UTF-8 locale、鼠标、inline image、terminal-specific modifier protocol | unavailable，除非另立可复验矩阵 | 不列为 R8 支持项，不由“terminal works”概括承诺 | 只有产品明确选择且有协议/终端 fixture 后才增加 |

将 Linux/macOS 候选列为“partial”不是当前支持声明。R8-REV-01 应把实测 emulator/version 写进最终支持行；Windows 或其他平台没拿到 real PTY 证据时直接列 unavailable，而不是注明“可能可用”。Pipe mode 不应启动需要终端控制序列的全屏 UI。

### Accessibility 与纯 ASCII fallback

R8 的最小 fallback 合同为：所有状态通过文字标签和布局表达，颜色/图标/动画均非唯一语义；键盘可完成 transcript 浏览、搜索、展开工具详情、审批/答题和退出；纯 ASCII fallback 中框线、进度和装饰 glyph 使用 ASCII。这里的 glyph fallback 与终端传输协议分开：受支持交互 TTY 仍可使用已验证的 ANSI cursor/render 控制序列；非 TTY 或明确不支持交互控制序列的 plain path 不得发送这些序列。用户的 UTF-8 内容必须保留，fallback 不得静默改写 transcript 文本。`NO_COLOR` 继续只控制颜色，不触发隐藏的用户环境更改。

使用独立配置 `tui.ascii` 控制 ASCII glyph fallback，默认 `false`；遵循现有配置命名惯例装配，但不得把 `NO_COLOR` 复用成 glyph 开关。`NO_COLOR` 继续只控制颜色，设置 `tui.ascii: true` 不改变颜色策略，反之亦然。屏幕阅读器、鼠标、inline image 和高对比度主题只有在明确声明后才加入人工验收；本轮不宣称 screen-reader 兼容。

## Viewport、Unicode 与性能验收合同

1. Transcript viewport 以 R2 durable `EntryID` 作为滚动锚点；用户向上浏览时新 token 不得把视图强制拉回底部，回到底部后恢复 follow-tail。resize 保持当前锚点、选择、搜索命中、展开态和编辑 caret。viewport 只保留可见行、少量 overscan 和受控渲染缓存；完整历史由 R2 durable source 分页读取。若 R2 没有分页/搜索 seam，只能将搜索范围明确限制在已加载窗口并显示提示，或把全历史搜索标 unavailable，不得把全 transcript 第二份复制进 TUI 内存来假装 bounded。
2. 工具 payload 的可展开来源是各 executor 按其自身 source-capture contract 捕获的有界内容；UI 视口截断与 executor/source truncation 分别标记，滚动到更多已捕获内容不等于重新读取源头。R1 `CompleteResponseGuard` 的 400,000-byte 限制覆盖跨 continuation 的完整 provider candidate（包括候选文本和结构化 tool-call 字段），不定义 stdout/stderr 等工具 source-capture 上限。历史/窗口裁剪不得改变持久 transcript、tool receipt 或 approval 内容。
3. 宽度以 terminal cell/grapheme cluster 计算：CJK fullwidth、组合音标、variation selector、肤色修饰和 ZWJ emoji 不切在 grapheme 中间；ANSI SGR/OSC-8 占零列，换行和裁剪不插入序列内部；未允许的控制序列须剥离/可见转义，不能透传执行。box、diff、Markdown/code、工具行、approval detail 和 line editor 共用同一 cell-width/truncation 合同。
4. `diff_style=unified|split|auto` 必须驱动真实 diff preview；如果某值没有实现就从配置与帮助中移除并在加载时给清晰错误，不能保留静默 no-op。主题覆盖 transcript、工具、help、todo 与 approval；`NO_COLOR` 下所有这些组件仍可辨认。
5. 新增 `TestTranscriptViewportSearchAndScroll`：构造至少 10k transcript entry，覆盖头/中/尾滚动、follow-tail、搜索非当前屏内容、无匹配和重复内容命中。搜索命中必须回到真实 EntryID；重复文本不能导致错跳。全历史搜索只在 R2 store 接口支持时验收。
6. 新增 `TestTranscriptViewportCJKAndANSI`：golden table 逐行核对 cell columns 和裁剪结果，覆盖 ANSI SGR、OSC-8、中文宽字符、组合字符、旗帜/肤色/ZWJ emoji、边缘列截断、控制字符和空串；断言没有半个 UTF-8 rune、半个 grapheme 或未配对终端控制序列。
7. 新增 `TestStreamRenderBoundedLargeTranscript`，其中的 10k entries + 1 MiB assistant stream（分 1k 个 chunk）是 **renderer-only 压力 fixture**：直接把合成 chunk 交给 renderer，不穿过 Runtime/source capture，明确不代表真实工具或 R1 stream 上限。测渲染累计 CPU、每 chunk allocations/latency、滚动键响应和 retained heap。每次增量只允许工作于新增 chunk、当前 viewport 与有限 cache，不得重新 render/扫描全部旧正文。目标 p95 键响应 <100ms；以 `-benchmem`/PTY 原始数值记录 1k、5k、10k 三档，render 更新时间/alloc 不随已持久化历史长度线性增长；超出预算时先修复增长阶数，不靠放宽上限。这个阈值是 R8 初始验收预算，不是当前性能数据。
8. 拆分两个不同边界的验收。`TestCompleteResponseGuardHonors400KBProviderCandidateCap` 走安装 `CompleteResponseGuard` 的真实 Runtime candidate 路径，覆盖完整逻辑 provider candidate 的 399,999、400,000、400,001 bytes；在超限时断言不发布任何候选 delta、不 dispatch tool call，并返回 R1 合同规定的已知错误状态。它不检查 stdout/stderr 截断，也不把 provider candidate cap 当成工具 source cap。另按各 executor 的实际 capture contract 分别测试工具输出；例如 `TestBashSourceCaptureHonorsConfiguredMaxOutput` 使用 Streaming Bash 的 `MaxOutput`，验证 stdout/stderr 合并后的保留字节、source truncation metadata 和超出保留上限后的继续 drain。只有 R2 实际持久化相应 payload 时，才同时断言 receipt 内容。UI 视口截断与每个 executor 自己的 source 截断分别标示，展开只查看该 bounded capture，不能为 UI 展开保留第二份无界原文。
9. 新增 `TestTerminalResizeKeepsViewportStable`：通过真实或测试 PTY 在窄到宽、宽到窄（60/80/120/200 列）切换，断言所选 durable EntryID、搜索命中、active approval/request、输入文本及 caret 不变；当前 window height 变化不得吞掉固定操作行。
10. 新增 `TestASCIIFallbackHasNoControlGlyphDependency`：检查默认 `tui.ascii=false`，再显式启用 `tui.ascii=true`，断言可见 UI chrome 不依赖非 ASCII 装饰 glyph、每个状态仍有文字。若该 fixture 运行交互 TTY，可允许矩阵内的 ANSI cursor/render protocol，再单独验证 glyph 输出；非 TTY/plain fixture 必须无 escape/control sequence。独立 `NO_COLOR=1` 产品测试保证只关闭颜色、不改变 glyph fallback 配置。测试夹具必须 hermetic，不可清理掉唯一的产品 `NO_COLOR` 覆盖。

性能结论必须以固定机型/OS/Go 版本、PTY emulator、数据规模、环境变量和原始结果为依据。benchmark 单测证明局部成本；增长 transcript 的真实 PTY 证明键盘延迟、尺寸变化和 stdout 行为。一个 renderer golden test 不能代替 PTY 旅程。

## 默认能力与公开声明矩阵

状态语义：**supported** 表示默认入口真实可用且声明范围有 integration/restart 测试；**partial** 表示有一段默认路径，但限制必须在用户操作前可见；**unavailable** 表示本版没有可用闭环或没有产品承诺。库里有类型、renderer、store 或单元测试不足以标为 supported。

推荐能力状态表由默认 CLI 装配与 `/help` 共用，`/tools` 只列真实可调用工具；partial 项在工具/命令描述中写明限制，unavailable 项在帮助状态区和用户文档明示。不要注册一个看似可执行、实际空操作的 placeholder。显式调用当前命令名但能力未启用时，应返回含 `unsupported` 的明确结果，而不是假成功或只打印 generic unknown。Auto-completion 与 ToolSearch 不得推荐 unavailable 项。R8 可用一个轻量 capability manifest 承载状态，但它只能镜像真实 default registry 和 reviewed owner contract，不能成为第二套能力权威。

| 能力 | 当前默认路径状态 | R8 推荐公开状态与边界 | 成为 supported 的证据 |
|---|---|---|---|
| `image_read` | unavailable：tool implementation/unit tests exist；未在 CLI default registry 注册。ImageRenderer 不等于读图工具 | 本 release 默认 unavailable；不注册伪工具、不在 `/tools` 显示为可调用 | 若产品明确选择：default CLI registry smoke、受控目录/格式/大小边界、真实 provider image roundtrip 与失败输出测试（R3/R6 owner） |
| Tracing / export | partial：默认配置指向本地 JSONL；main 与 assembly 当前会分别创建 tracer/exporter，assembly 的 `none` 仍可能写 JSONL。命名为 `otlp` 的实现不是标准 OTLP；所谓 Kafka exporter 使用自定义 TCP framing。默认能力没有 interactive trace viewer | `/help` 与此矩阵引用 [R7 的 canonical tracing exporter 矩阵](r7-lifecycle-design.md#tracing-exporter-支持矩阵)，不复制第二套协议 authority。R7 的 single-owner、关闭/flush 和 `none` 修复验收前保持 partial；OTLP、Kafka 保持 unavailable，不能按库类型或配置名宣称支持。明确不存在 interactive trace viewer | 以 R7 exporter matrix 为唯一支持范围；default CLI 验证单一 owner、JSONL/stdout 的真实输出与 shutdown/flush，`none`/disabled 零文件零输出；OTLP/Kafka 只有相应真实协议 conformance 通过后才可升级 |
| Goal / Task / Todo 当前运行期工具 | partial：默认注册；Goal 可选 JSONL，但装配传空路径；Task/Todo 是新建内存 store | 明示“仅本进程/当前会话”；不得称 persistent。默认 release 不把 persistence 宣称为 supported | 若选中持久化：真实默认 CLI 子进程执行→退出→重启→同 ID/state 读取的 `TestGoalTaskTodoSurviveRestart`；写失败时不回报成功 |
| Memory store 与 `/memory` 手工操作 | partial：文件存储、list/add/delete/search/clear 与 turn 后 extractor 已接线 | `/memory` 已实际实现的范围可以保留；所有错误必须可见。记录路径和自动提取/删除行为须清晰说明 | default CLI subprocess restart roundtrip；append/delete/reopen 故障下索引与文件状态一致，执行 `TestMemoryStoreAppendFailureDoesNotPublishMemory` 与 `TestMemoryStoreDeleteReopenFailureConsistent` |
| Memory 自动 prompt injection | partial：默认 assembly 把全部 `List()` 注入；`MemoryStore.Search` 有 TF-IDF+limit，但默认 prompt 未消费它 | 保留默认自动注入，改为对已有 memory records 做 R3 定义的 bounded selection；这不承诺新 memory backend 或新的 query feature。`memory.auto_inject` 默认 `true` 且用户可关闭；`memory.injection_token_budget` 必须有 R3 选定的非零 shipped default，并使用同一 token accounting。每条注入项保留 memory ID、category、来源类型（`auto` 或 `manual`）等可解释来源；达到预算时明确提示可能省略部分记录。有 query match 时可按 R3 选出的现有 selector 排序；无 match 时在同一预算内用 `List()` 的最近记录作为 fallback，并以 ID 打破时间并列，明确不保证 query relevance。不得将 query selector 暂缺当作移除全部自动注入的理由，也不得回退到全量 `List()`。R8-BLD-02 应用默认 assembly 验证开/关、预算边界、无匹配与重启；复用 R3 selector 并说明限制，不把 `TestMemoryRetrievalInjectedWhenEnabled` 扩写成新检索能力承诺。保留 `/memory` list/add/delete/search/clear 手工操作，并单独验证存储失败。|
| Session tree / fork / resume / switch | partial：slash 命令注册；05 已发现默认 `/resume` 身份闭环问题，R2 正在定义真实 durable mapping | 保留有真实行为的命令并标出未闭环限制；不将 fork/switch UI 命令存在等同于可靠恢复 | R2 的真实 session/branch/EntryID 映射、SIGKILL recovery 与 default CLI restart journey 通过后，R5/R8 再按操作逐项升级；UI 不猜 ID |
| Session snapshot / schema migration | unavailable：有未接线的内存 `DefaultSnapshotStore`/`MigrationChain` 库；文件 mutation checkpoint `/undo` 不是 session snapshot | 默认不可用并在 capability status 明确说明；不要复用“snapshot”宣传文件级 undo | 明确产品接受后，接入 durable session migration/snapshot API 并进行旧/新数据 subprocess restart tests |
| Web Agent UI | unavailable：未找到默认接线的 Agent browser UI；HTTP/ACP/MCP/web-fetch 能力不代表 UI | 默认 unavailable；不以 WebFetch/WebSearch、ACP server 或 remote TUI 替代名称承诺 | 明确选中且端到端 default/remote entrypoint smoke、身份/权限/事件/退出合同全过后再更改 |
| WebFetch / WebSearch CLI tools | partial：WebFetch 默认注册；WebSearch provider 受配置控制，未配置时不能承诺可搜索 | `/tools` 根据实际装配列出；WebFetch 与 WebSearch 分别标清默认/配置要求，绝不称为 Web UI | CLI 默认装配本地 fixture smoke；无 provider/超时/错误明确；凭据仅送到所选 provider（R0/R6 网络合同） |
| Local Skill tools | partial/configured：扫描配置目录或 `.go-cli/skills` / 用户目录，项目自动发现受 trust check；有文件才 deferred-register | 只显示本次装载的真实 Skill；无目录时说明未启用。Progressive registry 未按默认承诺单独宣传 | fixture 项目 trust/配置/启动/调用/工具搜索全链 smoke；其余 progression 属 R7 明确范围 |
| Plugin tools/hooks/middleware | partial/configured：必须 `Extensions.Enabled` 且提供 PluginPaths；加载和初始化错误目前被忽略 | 只宣称配置启用后实际加载的项目；错误须在 R7/R8 前显式可见。Extension commands/providers/remote RPC 默认 unavailable | R7 生命周期/加载错误/关闭测试及默认 CLI plugin fixture；commands/providers/RPC 分别有真实 adapter 才能升格 |
| 默认内建 renderer/content type | partial as a library capability；实际可见内容仍取决于 Runtime bridge 是否发送类型化事件 | UI 文档把“可注册 renderer”与“默认 CLI 已接线内容类型”分开；无事件来源的 Todo/Subagent/Image 明示 unavailable | 走生产 `assemble → bridge → renderer` 的 smoke 逐 content type 验证，不接受 registry 单测代替 |

如果最终产品 owner 对某项 capability 没有明确承诺，表中推荐 unavailable/受限文案优先于“可能能用”的乐观描述。实现中有明确 default path 的现有命令，要么用默认入口证明并保留/收窄原文案，要么返回可读 unsupported；不能仅删文档而留下看似成功的 no-op。

## Pi / OpenCode 工作流参考

只借鉴任务设计，不按功能数量做 parity。Pi 官方 terminal guide 将 prompt、assistant、tool calls/results/errors 放进 transcript，footer 展示 folder/session/model/usage；运行中输入区分 steer、follow-up queue 和 interrupt，并提供 `/resume`、`/tree`、`/fork` 与 active hotkeys。其官方键位文档把动作名与实际键位关联，可让用户查看和配置按键。OpenCode 官方 TUI/keybind 文档也把 session navigation、tool/detail 操作和 keybind 配置公开成实际入口。这些只说明用户旅程的可发现性值得检查，不能证明本项目已有相同功能或授权策略。[Pi terminal guide](https://pi.dev/docs/latest/usage)、[Pi keybindings](https://pi.dev/docs/latest/keybindings)、[Pi terminal setup](https://pi.dev/docs/latest/terminal-setup)、[OpenCode TUI](https://opencode.ai/docs/tui/)、[OpenCode keybinds](https://opencode.ai/docs/keybinds/)（于 2026-09-25 核对当前官方文档）。

R8 的用户价值限定为：历史可定位、长 transcript 不拖慢输入、Unicode 列宽正确、resize 不扰乱操作、错误/部分能力不伪装完成。R8 不承接外部编辑器/多行编辑器的 parity 项，不新增 session share、web UI、marketplace、remote RPC、native image pipeline、自动分支/回放或新模型能力。

## Builder 与最终 reviewer 的验收闭环

- R8-BLD-01 必须在 `go test -list` 精确确认执行任务要求的新测试以及 `TestCompleteResponseGuardHonors400KBProviderCandidateCap` 和选定的 executor-specific source-capture tests 均已注册，按各自 owner boundary 命中测试后再执行 TUI/CLI 受影响包 race suite。`TestStreamRenderBoundedLargeTranscript` 的 1 MiB 数据只能作为 renderer-only fixture；日志保留 test discovery、benchmark/PTY 原始输出和 `tui.ascii`/`NO_COLOR=1` 分开的执行结果；不能通过全局清空用户 `NO_COLOR` 才让 ANSI 断言通过。
- R8-BLD-02 对每一项 selected capability 加 default CLI assemble smoke；涉及 persistence 必须是真子进程重启，不以同进程 store 测试替代。未选 capability 用 `TestDormantCapabilityReportsUnsupported` 验证用户看到 unsupported/状态表不列为可执行；所列 memory store failure tests 仅在 memory persistence 被保留时必须运行。
- R8-REV-01 记录精确提交、Go 版本、OS、PTY emulator、shell、locale、TERM/NO_COLOR/COLOR vars、测试注册数、race 状态、1k/5k/10k 的 latency/alloc/heap 指标、能力矩阵变更和未覆盖的终端。测试命令零匹配、TTY 人工截图或 renderer golden 不能单独作为 release pass。
- Docker 架构沿 R8 execution task 单独验收：要么 build metadata/文档明确限制目标架构，要么在每个承诺的 runtime platform 上 build+smoke；未选择并验证 ARM64 前不宣传 ARM64 支持。这项不借终端矩阵结果代替。

完成条件是支持矩阵中的每个 supported 行有真实终端证据，viewport/Unicode 测试与增长负载 PTY 证明交互工作有界，默认 help/registry 与 capability 状态表相符；所有 partial 限制在操作前可见，所有 unavailable 项在帮助/文档/调用错误中明确。来源 review、单测、生产装配、子进程重启和真实 PTY 的证据层级分别记录。
