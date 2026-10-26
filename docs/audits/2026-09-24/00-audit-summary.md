# Go CLI 源码审计总览

> Retention note (2026-10-24): raw command logs under this audit were removed after the audit summaries retained outcomes, failure causes, and proposed work. Linked log paths are historical references only.


日期：2026-09-24  
状态：本总览定稿；分项结论、任务级发现映射与建议迭代计划已汇总。定稿仅表示审计材料完成，不表示产品缺陷已修复、全量验证通过或达到发布条件。  
代码基线：`40692e35e4815f0dff1c9f91b13905008f7221a4`（验证报告采样时工作树干净）。  
审计材料：本目录；逐路径清单见 [`source-inventory.csv`](source-inventory.csv)，覆盖映射见 [`source-coverage.csv`](source-coverage.csv)。

## 总体判断

本项目已有可保留的 Go 运行时骨架：`AssembleAgent` 组合根、`LoopAgent`/`Agent` 执行链、`SessionTree`/JSONL 持久化、工具注册与中间件边界、Bubble Tea TUI 都有真实实现。现有证据支持在这些边界上补齐运行契约和实际接线，不支持为审计而大规模重写。当前不能把“组件存在”“测试文件很多”或历史任务标成 done 当成用户可用与稳定的证明。

用户最容易受损的路径集中在三件事：一是模型流错误、超时或取消被当作正常完成，导致半截回答、工具调用或状态被提交；二是副作用执行与持久化没有一个可恢复的终态，可能丢失结果或留下不完整历史；三是安全策略、审批和远端执行入口在不同 adapter/TUI 间没有一致的主体、请求与取消合同。先保证一次 Run 有唯一、可观察、可持久化的终态，再扩充 provider、TUI 和插件能力。

Runtime 方向是保留现有 Go 与 `AssembleAgent`/`LoopAgent`/`SessionTree`，在现有协调边界补齐 Run、事件、工具调用、session 与资源清理合同；不以新增大而全的 Runtime 封装作为前置。入口负责输入输出适配；运行路径统一绑定模型、工具、能力策略、审批、session、事件与清理。每条事件应有 run/session/correlation identity 和序号；tool intent、tool result、final/error/cancel 由一个权威 transcript 协调器提交。`actualAssembly` 已有 `sharedRunSlotGuard`，默认 `EventStream` 有 30 秒设置；这是当前实现证据，不应被其它旧 no-op 入口或库默认值的引用掩盖。`Runtime`/`RunRequest` 可作为接口命名候选，不是架构重写要求。仍须验证共享 Agent 的重入、关闭与所有入口的一致性。

取消协议必须把**执行状态**和**动作结果**分开：`cancel_requested`、draining、quiescent、quarantined 表示 worker 生命周期；success/error/cancelled/unknown 表示 Run 或外部副作用结果。`unknown` 不代表 worker 已停止。取消等待有界；非协作 worker 未退出时进入可观察的 quarantine/cleanup-pending，并继续占住同一工作区冲突写入的 admission slot；不要无限 join 不可杀的 Go goroutine，也不要仅因 UI 停止等待就放行下一次冲突副作用。可管理的子进程需 kill+join 后才能释放资源。

## 功能判断矩阵

“合理可保留”表示结构值得延续，不代表端到端已验收；“缺陷”仅用于分项报告有源码链或复现支持的项目；“未验证”表示证据不足，不推断功能失败。

| 模块/用户能力 | 合理可保留 | 实现不完善 | 已确认缺陷 | 未验证/边界 | 证据 |
|---|---|---|---|---|---|
| Core Agent/Loop/Turn | `AssembleAgent`、AgentLoop、TurnRunner、Harness、EventStream/EventBus 分层可演进 | Agent history 是消息投影，未统一为权威 Run transcript；取消合成的 tool receipt 未进入下一次 Agent/session history；Run outcome 与 worker quiescence 需要分开管理 | 默认路径可见取消后 tool history 不成对、并行取消未 join、continuation error 可返回截断成功、stream 事件可能丢失；直接 `TurnRunner.Cancel` 提前终态与多 Assembly interceptor 共享列为 P2 API/多实例边界 | 并发/关停压力与完整 runtime E2E 尚未覆盖 | [01-runtime](01-runtime.md)、[04-session-compaction](04-session-compaction.md)、[06-cli-config-acp](06-cli-config-acp.md) |
| LLM / Production | Generate/Stream 统一接口、provider 分层、结构化错误、retry/circuit/guard 的独立策略 | CLI resolver 与已实现 adapters 不一致；stream usage 没进入 `/cost`；是否提供硬预算由产品决策 | 默认 stream 错误/timeout 可静默成功；CLI streaming 绕过 output guard；默认 loop/retry/attempt 不按声明工作 | 真实 provider、原生协议完整性须分 default path 与显式接入分别验收 | [02-llm-production](02-llm-production.md) |
| Tools / Approval / 安全 | Bash/文件工具的基础校验、原子替换、per-file mutation queue、Ask 默认和 URL 拨号检查可保留 | 工具 schema、输出限制、MCP reload、Capability/Plan Mode 与 executor 契约不一致 | 缺失 Write/Edit 参数可破坏已有文件；Stream Bash 超输出不 drain 可挂起；一次 `y` 可复用授权；Plan mode 未封闭 Git/扩展副作用 | `allowed_paths` 是 cwd 检查，不是文件系统隔离；跨平台命令约束与权限闭环仍需验收 | [03-tools-approval](03-tools-approval.md) |
| Session / Compaction / Memory | append-only JSONL、树形分支、分层压缩有可复用原语 | durable turn、摘要恢复、memory retrieval 还未连成主路径 | 新 session 可能引用旧文件 leaf；`/resume` ID 类型错配；保存错误被吞、torn tail 可连带丢记录；tool history/compaction 的协议闭合不足 | 当前 CLI 未启用 durable compaction writer；snapshot、branch store、migration 有库能力但接线未证实 | [04-session-compaction](04-session-compaction.md) |
| CLI / Config / ACP / Web | CLI 入口及配置分层存在；serve 有 token/body 限制等保护 | 入口自行拼 Harness/TurnRunner/Dispatcher、审批与事件语义分裂；Web 未接 Agent | 显式 wildcard + no-auth 可监听全网卡；ACP 认证/session/event 身份不兼容；并发事件会混流；部分 stdin/stdout 与 ask 路径破坏协议/交互 | 远端 ACP 和 browser journey 未执行；默认关闭与显式启用需分开判断 | [06-cli-config-acp](06-cli-config-acp.md) |
| TUI / 终端交互 | Bubble Tea、renderer、工具折叠、状态栏、前置 LineEditor 和部分 interrupt/steer 构件可留 | transcript、键位、长输出、单元格宽度和默认 renderer/事件投影不完整 | ask_user 无 Update 接收路径；并行 approval 单槽覆盖；done 可能重复展示最终答案 | 未做真实 PTY 视觉验收、Windows/其他平台终端验收及无障碍验证 | [05-ui-review](05-ui-review.md) |
| Extension / MCP / Skill / Tracing | registry、adapter、schema、SSRF/body-size 防护、JSONL/redaction 等部件可复用 | 多项库 API 没接默认 runtime；错误状态、取消、delivery/reload 和资源生命周期欠完整 | MCP 跨源 credential 只在特定 endpoint redirect 条件成立；阻塞/重放和 trace 隐私边界见报告；远端 extension 当前是 no-op/unsupported | 非标准 wire 格式没有标准 collector/broker 互通测试；本地插件不构成沙箱 | [07-extensions-tracing](07-extensions-tracing.md) |
| Mock / Verify / 测试治理 | mock servers、race 与验证工具提供一定测试底座 | gate 与 CI/本机目标不一致，若干验证目标只测 helper 自身 | 原始 `make verify` 失败；scan 仍有 20 ERROR；规则过滤不能代表零 error | mock contract 不能证明真实 provider/MCP/runtime 集成稳定 | [09-verification](09-verification.md) |
| 非 Go 构建/配置/文档资产 | CI workflow、Makefile、Dockerfile、Web HTML、模块元数据和部分主文档已在验证/CLI/UI 报告中核对 | tracked 文档/配置没有逐项产品语义审计；Dockerfile 仅有静态 target-platform 检查 | Docker build 设 `GOARCH=amd64`、运行镜像未 pin platform；ARM64 target 可能错配，默认 amd64 不受影响 | 未执行 Docker/Buildx 或 ARM64 smoke；不因此要求新增 ARM64 支持 | [09-verification](09-verification.md)、[source-coverage.csv](source-coverage.csv) |

## 发布阻断项（按用户影响与触发条件）

下表将“默认可达”“显式配置/入口触发”和“库能力 dormant”分开。P1 是发现报告的局部优先级，不代替产品发布决策；修复前需以对应回归场景关闭。

| 优先级 / 可达性 | 阻断问题 | 触发与用户后果 | 依据 |
|---|---|---|---|
| P1 · 默认 CLI 运行可达 | Stream 缺少唯一 terminal outcome | provider 断流、解析错误或默认 timeout 后，部分 content/tool args 可被当完整回复，错误终态也无法一致传播 | LLM-03/04、RT-04 |
| P1 · 默认 CLI 运行可达 | 流式输出安全策略不在同一执行路径 | 主 agent streaming 绕过 output guard；guard 错误可 fail-open，用户可见未过滤输出 | LLM-05 |
| P1 · 默认 CLI 运行可达 | 并行工具取消可能在工具 goroutine 收敛前返回；取消后 history 留下 assistant tool call 无对应 receipt | 副作用仍可能运行，下一次模型上下文又不完整 | RT-02/03、CLI-05 |
| P1 · 默认 CLI 配置下可达 | Loop detection 和 Retry 的声明与实际执行不符 | loop detector 的事件合同不匹配且观察太晚；结构化瞬时 provider error 可能不重试；MaxAttempts 可多一次 | PRD-01..03 |
| P1 · 默认 tracing 配置可达 | 完整用户 prompt 会写入 JSONL trace，root tracer 的 sensitive 标记没有被 redactor 消费 | 隐私内容可能落盘；优先关闭默认明文 prompt 记录或落实字段级脱敏 | TRACE-01 |
| P1 · TTY + HITL/并发审批/重复工具调用触发 | 交互等待者无法可靠答复/取消；一次 `y` 可被复用 | ask_user 消息没人处理；第二个 approval 可覆盖第一个；一次允许的工具调用可能被 session/store 当作后续重复授权 | UI-01/03/11、APR-001 |
| P1 · 内置工具主路径 | 超过上限的 Streaming Bash 可卡到 timeout；缺少 `write.content` / `edit.new_string` 可将文件内容清空或删除 | 前者有有限复现；后者由参数缺失处理与 mutation queue 路径直接证明 | TOOL-002/003 |
| P1 · Plan Mode | 计划模式依工具名白名单拦截，Git 与自定义/MCP 等副作用工具可绕过只读承诺 | `/plan` 暴露的只读语义未由统一 effect/capability 约束 | APR-002 |
| P1 · 显式 `serve` wildcard + `--no-auth` | 服务可无认证监听所有网卡 | 安全暴露由明确配置组合触发，应 fail closed 或限制配置 | CLI-01 |
| P1 · 用户显式选择 `claude` 并提供 key、但未设 BaseURL | 当前 resolver 仍构造 OpenAI-compatible Eino provider，凭证可能发往 OpenAI endpoint；无 key 时则解析失败 | 配置/入口触发，不是 native adapter 默认可达；必须统一 provider factory 与凭证域 | LLM-01 |
| P1 · 显式 MCP stdio / HTTP 工具调用 | `tools/call` 在远端可能已执行、但响应丢失时会 reconnect 并重放相同副作用；stdio 无数据阻塞读不会因 context cancel 唤醒 | 只有启用并调用 MCP 才触发；来源码闭环确认，但本轮未做执行后断线或静默 pipe 动态复现。结果未知时不能透明重放 | MCP-02/03 |
| P1 · 直接 MCP adapter API 配置凭据并使用不可信 endpoint | adapter 接受 SSE 中指向异源的绝对 endpoint，后续请求可能把配置的 Header/Bearer/OAuth 凭据带到该源 | 触发需要调用者通过库 API 配置凭据，且已配置/受控 endpoint 返回异源地址；当前 CLI MCP 配置 schema 未暴露这些凭据字段，因此不是当前 CLI 的普通可达路径。本轮没有恶意 peer 动态复现；应在 CLI 暴露认证配置前修复 origin 校验 | MCP-01/04 |
| P1 · 配置持久化/新会话触发 | Session file 与 tree identity/写入事务未闭合 | 新 session 可能不可恢复；`/resume` 选择文件 ID 后按 entry ID 移动失败；保存失败可能仍显示回答 | SES-01/02/04/05/06、CLI-11 |
| P1 · ACP HTTP/多任务触发 | 身份、session、SSE 路由不一致 | 客户端认证/session identity 与服务不匹配；同一服务/凭据下并行 task 事件可能混入；当前证据不支持跨租户越权结论；远程审批没有明确应答通路 | CLI-02/03/04 |
| P1 前置门槛 · 仅直接调用/未来接入 native adapter | Claude/Gemini tool-call 协议未闭合，当前默认 CLI 不会选择 native adapter | 不可按默认 CLI 故障宣传；在 adapter 接入前必须完成 tool schema 与 tool-result 往返测试 | LLM-02/07/12/15 |
| R0 · 所有迭代并行的发布验证前置 | 本机 `make verify` 在隔离 `NO_COLOR` 后仍因 20 scan ERROR 失败；CI scanner 可忽略 SCAN-012 并吞 scanner/JSON 错误 | Phase 48/49 标 done 不能代替当前 commit 的可重放验收，CI 可能假绿 | VFY-04/05/09 |
| P2 · 直接 API/多 Assembly 边界 | `TurnRunner.Cancel` 可提前标终态；多 Assembly interceptor 共享；tracer identity、`none` 关闭和同步/异步导出契约不一致 | 这些不是默认单 Assembly CLI 的 P1 发布阻断 | RT-01/05、TRACE-02/03/04 |
| P2 · 成本与安全边界声明 | `Stream` usage 不计入 cost，`/cost` 低报；`sandbox.allowed_paths` 只限制 cwd | Stream usage 是确认的统计缺口；是否需要硬预算是产品决策，`allowed_paths` 语义需澄清，不应称作默认越权漏洞 | PRD-10、TOOL-001 |
| P2 · 发布平台声明 | Dockerfile 固定生成 amd64 binary，但 runtime stage 不声明 platform | 只有面向 ARM64 target build 时可能不匹配；先声明/约束支持平台，若承诺 ARM64 再加架构匹配构建与 smoke | VFY-11 |
| P2 · 显式配置/非默认入口 | MCP 错误映射、热重载、teardown 与其它工具/API 边界仍有缺口 | 只在相应传输/server 配置触发；可能出现配置未生效、工具错误不可见或资源残留 | [03-tools-approval](03-tools-approval.md)、[07](07-extensions-tracing.md) |
| P2 · 库能力但默认路径未接 | Extension HTTP/RPC、Skill registry/match、snapshot/migration、部分 tracer/exporter 能力 | 组件存在不能视为已接入产品；需先决定实现、标记不支持或移除声明 | [04](04-session-compaction.md)、[07](07-extensions-tracing.md) |

## UI 单独评审：以 Pi 与 OpenCode 的实际用户旅程为参照

UI 有可复用的终端组件，但尚未达到成熟 coding-agent TUI 的会话闭环：当前输入编辑器与运行中 steer/follow-up 是两套状态；默认屏幕只投影本轮事件，缺 transcript 浏览、搜索与恢复分支；工具调用的常规事件未显示参数；键位说明与行为存在冲突；Unicode/ANSI 单元格宽度和大历史渲染也未充分处理。

相对 Pi 与 OpenCode 官方文档列出的多行/外部编辑器、可配置键位、队列中断、滚动/搜索、工具详情与会话命令，本项目应先完成 HITL/approval 和结束事件的正确性，再建设可回看 session 的 transcript/viewport、消息编辑与 keybinding 注册表，随后优化渲染和终端兼容。竞品参考来自官方文档与记录的代码快照；本次没有安装或实际操作竞品，也没有真实终端视觉 E2E。完整逐项证据和验收标准见 [05-ui-review.md](05-ui-review.md)。

## 从 I_WORK_MATE 稳定化历史得到的教训

[08-iworkmate-lessons.md](08-iworkmate-lessons.md) 的结论按 Git author/commit 和代码差异限定：不能把整个 Runtime 稳定化归于朱一辰；实名与 commit 邮箱的关联若仅凭拼音属于推断。报告区分 8 月改动、后续行为修正/测试、9 月仍修边界的证据，也区分 commit message 的宣称与本次可验证的代码/测试。

可迁移的工程做法是将核心运行时合同拆成可独立复现的生命周期、事件归一、持久化恢复、snapshot 和工具执行边界；每个失败路径都要有后续修正与测试证据。不可照搬 I_WORK_MATE 的实现细节作为 go-cli 已有保证，也不可把存在的共享 guard、30 秒 EventStream 默认值、单元测试数量当成稳定性证明。I_WORK_MATE 的历史最多是设计与验证方法的证据，不是当前 go-cli 用户路径的验收。

## 迭代路线

阶段任务、依赖图、责任边界和验收门槛以 [10-runtime-iteration-plan.md](10-runtime-iteration-plan.md) 为准；跨模块证据与优先级限定见 [11-independent-review.md](11-independent-review.md)。

**R0 止血与验证可信度（并行前置）：** 以独立回归用例先修复一次授权误缓存、缺少文件内容参数导致清空、wildcard+no-auth、provider key 错投、TUI ask_user 死等和并发审批覆盖、Plan Mode 对未知写能力临时 fail closed、Streaming Bash 输出上限卡管道、MCP `tools/call` 断线重放；另在向 CLI 暴露 HTTP 凭据配置前，修复直接 adapter API 的 endpoint 跨源校验（当前 CLI 配置尚未暴露这些凭据字段）；同时修默认 prompt trace 明文落盘、`NO_COLOR` 测试污染和 CI scanner fail-open，校准 SCAN-012 适用条件与逐项豁免。gate 与这些互不依赖的热修并行，后续每个迭代持续使用同一可重放 gate。

1. **R1：Run 安全终态。** 统一流式 complete/error/timeout/cancel 结果、tool-call/result 闭合、资源归属与有界清理。worker cancel 后尚未 quiescent 时进入 quarantine/cleanup-pending；outcome unknown 不代表 worker 停止，也不能释放冲突写入 slot；不可强杀的 Go goroutine 不能被无限 join。
2. **R2：可恢复 transcript MVP。** 复用 SessionTree/JSONL，修正 session/file/entry identity、单写者、append durability 和恢复；提交 tool intent/result receipt，崩溃后外部副作用结果未知时明确标记 unknown，绝不透明重放。
3. **R3–R4：provider/context 与工具权限闭环。** 先定义实际 provider/credential route 和 stream 终态，修 tool-call/result 与 compaction bundle；hard cost budget 留作产品决策，PRD-10 stream usage 则修统计。工具和计划模式采用 capability/effect 元数据、一次授权只允许一次、未知 effect fail closed；`allowed_paths` 先澄清 cwd 语义。
4. **R5–R7：在共享合同稳定后闭合 TUI、CLI/ACP/MCP/Web、Extension/Skill/Tracing。** TUI 先完成真实 transcript、HITL/并发审批/取消，再加浏览、编辑和性能；多入口共享身份、审批和事件隔离；未接能力要明确 unavailable。
5. **R8–R9：兼容和发布复验。** 做真实 PTY/受支持终端、Unicode/ANSI、长 transcript 性能和能力文档 smoke。任意 shell/MCP 跨进程自动续跑属于可选暂缓项目；没有 receipt、验证器、generation fence 与隔离策略时保持中断/unknown。

## 验证事实与限制

- 清单基线有 **810 个 tracked 文件**，包括 **791 个 Go 文件**：321 个 production Go 源、470 个 Go 测试；Go 源行数 71,649、测试行数 123,593。321 个 production source 均映射到分项报告：179 个源文件由报告明确声明逐文件或全 production-source review，142 个源文件由显式 manifest 与模块/调用路径 scope 覆盖，未映射或无审计范围声明的 production source 为 0；审阅深度仍按 ledger 逐文件区别。
- 470 个测试路径都映射到报告：335 个有相关测试/断言阅读或逐文件测试覆盖说明，135 个主要进入 inventory 与执行目标/标签盘点。按本轮记录的宿主和 build-tag 命令，有 10 个测试文件未执行：`internal/core/e2e_stoplength_test.go`、`internal/core/e2e_streaming_pipeline_test.go`、`internal/cli/integration_test.go`、`internal/cli/line_editor_pty_linux_test.go`、`internal/cli/resize_other_test.go`、`internal/llm/e2e_cycler_test.go`、`internal/llm/e2e_thinking_test.go`、`internal/memory/e2e_test.go`、`internal/tools/e2e_streaming_test.go`、`internal/tui/e2e_render_test.go`。另外 `tests/` 有 10 个文件共 18 个 `t.Skip`/`t.Skipf` 调用点；不能按文件被编译就说其中全部场景已通过。详细 gate 与 tag 结果见 [09-verification](09-verification.md) 和 ledger。
- 原始环境 `make verify` 因 37 个依赖 ANSI 的 TUI 断言失败；主机设置了 `NO_COLOR=1`。清空 `NO_COLOR` 后 race 测试通过，但随后 `make verify` 在 scan 的 20 个 ERROR 处失败。因此不能称 verify 全绿。
- `make test` race 和补跑 `cmd/cli` race 均通过；覆盖率门槛报告为 84.2%。这些不能证明所有模块的行为稳定。
- `make scan` 报 20 ERROR、1273 WARN；CI 对 SCAN-012 有过滤，过滤不等于扫描无错误。
- `make test-log` / `make test-leak` 只跑 `internal/verify` 的 helper 自测，不是全产品日志或 goroutine 泄漏验收。mock/E2E-tagged 结果按报告中环境分别记录。
- 未执行真实 provider 请求、标准 MCP/OTLP/Kafka 服务互通、跨平台终端验收、浏览器用户旅程或真实 TUI 视觉 E2E。测试源码文件数量不能替代这些证据。
- 主审将当前 810 个基线 tracked 路径重新计算 SHA-256，与 `source-inventory.csv` 比对差异为 0；`git diff --stat` 为空，产品源码没有变更，本轮只新增未跟踪审计文档。
- `source-inventory.csv` 是代码基线的路径/行数/hash 快照；`source-coverage.csv` 标注分项报告、文件覆盖层级和测试执行状态。全量 Go production source 都有范围声明，但这不代表每个文件逐行审阅；非 Go 资产也只做 inventory/报告所列范围阅读，未审项不会被推断为“无问题”。

## 报告导航

| 编号 | 内容 | 状态 |
|---|---|---|
| 00 | 本总览与阅读入口 | 定稿；结论与证据边界见本文 |
| 01 | Core Runtime | [01-runtime.md](01-runtime.md)；文件 manifest：[01-runtime-files.txt](01-runtime-files.txt) |
| 02 | LLM 与 Production | [02-llm-production.md](02-llm-production.md) |
| 03 | Tools、Approval 与安全 | [03-tools-approval.md](03-tools-approval.md) |
| 04 | Session、Compaction 与 Memory | [04-session-compaction.md](04-session-compaction.md) |
| 05 | UI 与 Pi/OpenCode 对照 | [05-ui-review.md](05-ui-review.md) |
| 06 | CLI、Config、ACP 与 Web | [06-cli-config-acp.md](06-cli-config-acp.md) |
| 07 | Extension、MCP、Skill 与 Tracing | [07-extensions-tracing.md](07-extensions-tracing.md) |
| 08 | I_WORK_MATE 历史经验 | [08-iworkmate-lessons.md](08-iworkmate-lessons.md) |
| 09 | 测试与验证覆盖 | [09-verification.md](09-verification.md) |
| 10 | Runtime 迭代计划 | [10-runtime-iteration-plan.md](10-runtime-iteration-plan.md)；[任务 DAG](proposed-tasks.json) 共 44 项（41 主线、3 延期），[发现映射](findings-to-iterations.csv) 覆盖 149 个稳定 finding ID 与 D-01 |
| 11 | 独立复核与裁决 | [11-independent-review.md](11-independent-review.md) |

## 审计材料的状态边界

迭代路线是根据发现映射与依赖关系编排的建议计划，尚未实施。此次没有修改产品源码；源码基线 hash 与 inventory 一致。计划中的修复须各自经过 builder、reviewer 与可重放验收；本文中的 gate 失败、未执行测试、未做真实 provider、跨平台和视觉 E2E 的限制仍有效。因此本次审计材料定稿不构成“无缺陷”、测试全绿或产品可发布结论。
