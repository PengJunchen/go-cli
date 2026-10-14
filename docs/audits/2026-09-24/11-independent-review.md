# 独立复核：源码发现与迭代计划

日期：2026-09-24  
基线：`40692e35e4815f0dff1c9f91b13905008f7221a4`。  
范围：独立复核高风险跨模块发现、触发条件与证据边界；检查作者对争议项的修订。没有修改产品源码，也没有重跑全套测试。临时复现源码和日志保存在 `evidence/`，不由本报告重新执行。

## 复核结论

报告中的主要高风险结论有源码闭环或已归档最小复现支持。若干原始表述需要按当前实际调用范围降级：库级全局状态不等于默认单 Assembly CLI 故障；已存在的能力不等于当前入口已接线；静态恢复投影缺口不等于当前 CLI 已持久化并丢失该数据；配置没有声明的产品承诺也不能自动定为实现缺陷。

### 已独立确认

| 项目 | 复核结果与适用范围 | 证据 |
|---|---|---|
| 全局 tool interceptor 串 Assembly | **确认，P2，限多 Assembly/嵌入式复用。** 每次 Assembly 把局部 PlanMode/Hook interceptor 注册进进程全局集合，没有对应注销；A 实例的 active policy 可拦截 B 的工具调用。单进程通常只创建一个 Assembly 的 prompt/REPL/serve 路径不因此自动串会话。 | `internal/core/pre_tool_call.go`、`internal/cli/assemble.go:1383-1385`；`evidence/plan-mode-cross-assembly-repro.go.txt` 与 `.log`。 |
| Cancel 与实际 worker 生命周期 | **确认，P1/P2 依实际工具而定。** 并行工具取消分支可在 worker 未退出时返回；`TurnRunner.Cancel` 也能先报告 Canceled/Done，而 RunTurn 与工具仍未收敛。危险不是“取消请求本身”，而是调用者误以为可安全重试/开始下一项副作用。合作式 context 测试不覆盖无视 context 的工具。 | `internal/core/parallel_tools.go:71-125`、`turnrunner.go:222-239`、`turn.go:65-73`。 |
| Agent history 的 tool-call/result 闭合 | **确认，但影响条件要保留。** 取消/工具错误后，最后的 assistant tool-call 可能进入 `AgentImpl.history`，对应 tool-result event 留在 Loop 局部消息/事件中；后续请求就可能携带悬空 call。完整 tool transcript 是否需要保留应由会话协议决定；对“下一模型请求不得含悬空 call”的不变量，修复与验收是必要的。 | `internal/core/agent.go:124-153`、`loop.go:486-507`；`evidence/canceled-history-pairing-repro.go.txt` 与 `.log`。 |
| continuation 失败变成功结果 | **确认，P1，partial-as-success。** `finish_reason=length` 后续流错误/空响应走 break 路径，Loop 返回已有部分响应且 `nil` error。不能让 UI 或 Session 把这当完整完成。 | `internal/core/loop.go:933-972`；`evidence/continuation-error-repro.go.txt` 与 `.log`。 |
| 新 session / `/resume` ID | **确认，P1。** 新 JSONL 文件在已有树建立后才创建，首条记录可以引用旧文件 leaf；`/resume` 列表 ID 是 session file ID，handler 却按 entry ID 在当前树中 `MoveTo`。两者均有保存的直接复现。 | `internal/cli/repl_session.go:289-314,905-924`、`internal/session/slash_commands.go:176-236`；`evidence/session-new-file-orphan-repro.*`、`session-resume-file-id-repro.*`。 |
| `ApprovalAllow` 的一次授权被复用 | **确认，P1。** TUI `y` 是 Allow once，但 `decide` 将任何 Ask callback 的 Allow 写到 session map 与 store；同一 middleware/store 生命周期内相同 mode/tool/args 会跳过第二次 callback。当前 `AlwaysAllow` 缓存不持久化；不能仅凭标签推断跨进程永久授权。 | `internal/tui/app.go:1636-1651`、`internal/approval/middleware.go:169-198`、`internal/approval/callback.go:21-29`；`03-tools-approval.md` APR-001。 |
| Plan mode 对未知副作用开放 | **确认，P1。** 只读策略依赖四个工具名，漏掉多个 git mutation、`remote_bash` 和 MCP/custom/extension 工具；单纯继续补工具名会重复产生缺口。建议把 effect/capability 绑定到解析后的 executor，未知 effect 在 Plan Mode 中保守拒绝。 | `internal/core/plan_mode.go:28-35`、`internal/core/plan_mode_middleware.go`、`internal/tools/builtins.go:153-173`；`03-tools-approval.md` APR-002。 |
| wildcard `--no-auth`、ACP identity 与 stream 路由 | **确认，限明确触发条件。** `:port` 的空 host 被判 loopback，但 listener 绑定 wildcard；配合显式 `--no-auth` 可暴露无认证服务。仓库 ACP adapter 与 serve 的默认 auth/sender/session key 也不兼容；跨并发 task 的共享 EventBus SSE 没有过滤 task identity。事件混流问题限定为同一服务/凭据下路由隔离，不据此宣称跨权限租户越权。 | `internal/acp/http_server.go:194,483,584-630`、`internal/acp/grpc_adapter.go:208-251`、`internal/cli/serve_cmd.go:93,119`；`06-cli-config-acp.md` CLI-01..03。 |
| MCP 重放、取消与 origin | **源码路径确认，未做恶意远端/阻塞 pipe 动态复现。** 通用 reconnect 对 `tools/call` 可在响应丢失后重放相同参数；静默 `ReadString` 不能被 context 取消唤醒；HTTP SSE endpoint event 可给出绝对 URL，后续请求会带 adapter 配置凭据而未校验 origin。跨源风险触发依赖已配置/受控 MCP endpoint 返回恶意地址，不能描述成任何普通 MCP 响应都会外泄。底层 adapter API 支持 Headers/Bearer/OAuth，但 CLI 用户配置 schema 没暴露这些字段；Assembly cleanup 也没有完整 MCP disconnect owner。 | `internal/mcp/adapter.go:84-99,359-395,439-443`、`http_adapter.go:124-157,249-267,330-343`、`internal/cli/assemble.go:1677-1717,631-655`；`07-extensions-tracing.md` MCP-01..04。 |
| TUI HITL / 并发审批 / done 投影 | **确认，TTY 默认路径。** 生产 `teaModel.Update` 没有 `HITLMessage` case；多审批只存一个 `pendingApproval`，后到请求可覆盖前一个；REPL 把最终答案放在 done status event，再次渲染。现有测试没有覆盖完整真实 TTY 用户旅程。 | `internal/cli/interactive.go:246-289`、`internal/tui/app.go:596-663,602-611,1090-1108`、`internal/cli/repl_session.go:707-718`；`05-ui-review.md` UI-01..03。 |
| tracing 明文与 `none` | **确认，默认 tracer 的隐私边界需要特别说明。** `cmd/cli` 的 main tracer 直接持有 exporter，没有 `RedactingExporter`；interactive turn span 在该上下文中创建，`SensitiveAttribute` 标志本身不会脱敏。Assembly 随后另建带 redaction 的 tracer，Loop 内覆盖 context，不能回头保护已由 main tracer 导出的 turn span。Prompt 命令也将原始 args 放入 main span；Harness 写入未标记 sensitive 的 `user_message` 和 trace logger `msg`。此外 exporter=`none` 在 main builder 被视为关闭，但 Assembly builder 的 default 分支仍可构造 JSONL exporter。 | `cmd/cli/main.go:117-147`、`internal/cli/repl_session.go:231,649`、`internal/cli/assemble.go:761-773,2188-2215`、`internal/core/harness.go:123-135`、`internal/tracing/exporter.go:120-150`；`07-extensions-tracing.md` TRACE-01..03。 |
| Docker 目标架构 | **静态确认，P2，限显式非 amd64 构建目标。** Dockerfile 固定编译 `GOARCH=amd64`，runtime `FROM alpine:3.20` 又随目标平台选择镜像；面向 ARM64 构建时会得到架构不匹配的镜像。默认 amd64 路径不受影响，未执行 buildx/ARM 镜像。 | 根目录 `Dockerfile`；`09-verification.md` VFY-11（未运行跨架构镜像构建）。 |

### 降级、限定或改写

- **Compaction recovery：** `EntriesToAgentMessages` 会跳过 compaction entry，但当前 CLI 没有创建 durable `EntryTypeCompaction` 的生产接线。因此这是恢复投影缺口/未来 durable writer 的验收门槛，不能表述成“普通 `/compact` 后重启必然丢失已持久化摘要”。`04-session-compaction.md` SES-03 已限定。
- **JSONL torn tail：** 要区分三种情况：有效 JSON 只有末尾 newline 未落盘时应补 separator 并保留 receipt；可证明被截断的 JSON 才可安全隔离/截除；完整但 malformed 或中间损坏不能盲目当尾部撕裂删除。任何计划任务若只写“truncate 最后一行”都不够安全。`04-session-compaction.md` SES-06 当前区分正确。
- **Native Claude/Gemini：** provider resolver 未接入原生 adapter 是当前 CLI 的选择/配置缺口；原生 tool-call 协议不完整是直接调用者或未来接入的门槛。报告和总览不应把不可达 adapter 的协议问题算作默认 CLI 当前故障。
- **预算：** `NewCostTracker(nil)` 没有预算并不是在缺少硬预算产品承诺时的 runtime defect；stream usage 没进入 `/cost` 是确认的 P2 统计不一致。`02-llm-production.md` 已拆分 PRD-04/PRD-10。
- **OutputGuard：** streaming wrapper 当前确实旁路 guard；已经显示的文本无法靠事后撤回变成安全拦截。迭代计划要明确用户可见前的 buffer/delay 或显式 typed stream policy，不能将“显示后尝试撤回”验收为 guard 闭环。
- **Bash `allowed_paths`：** 独立复核后建议由 P1 降为 P2 的配置语义/能力边界缺口。配置注释“restricts command execution to these directories”有歧义，但 `PathWhitelist` 与 `BashSandbox.Validate` 的包级契约指向 working directory；sandbox 源码也明确文本过滤不是完整安全边界，权威隔离需放在 execution layer。能确认的是该选项只限制 cwd、不限制命令目标路径，尚不足以认定当前产品明确承诺了文件系统隔离。应修清注释/help，或在产品确实要求该边界时接入 OS 级 sandbox；不能列为已确认的默认 CLI 越权漏洞。
- **gRPCAdapter：** 实现为 JSON-over-HTTP，但源码注释已说明该解释；问题是名称/跨端契约容易造成误解与实际认证/session 不互通，不应称作隐藏的真实 gRPC 实现。
- **Extension hooks：** `Hook.Before/After` 已由 `HookMiddleware` 接入 CLI loop；不得保留“Hook 完全未触发”断言。真实缺口是 Hook action 折叠成 generic error、事件数据浅共享、注册顺序不确定等，见 `07-extensions-tracing.md` EXT-01/05/06。
- **全局 interceptor：** 复现证明多 Assembly 时存在跨实例影响；默认 CLI 一次装配的常规流程不触发，严重度/发布阻断表要保留此限制。
- **OTLP：** 不兼容结论需要落在当前 payload schema，而不是“它用 JSON 所以不是 OTLP”。当前实现发送自定义 `{"spans":[SpanData...]}` envelope，与 OTLP JSON `resourceSpans`/`scopeSpans` 结构不同；Kafka exporter 是源码明示的 length-prefixed JSON-over-TCP。尚无标准 collector/broker 互通实测。

### 未动态复现或本轮未验证

- 本轮没有调用真实模型，也没有真实终端视觉、Pi/OpenCode 操作、浏览器旅程、Windows/其他 GOOS、标准 MCP server、OTLP collector、Kafka broker 的实测。相关结论必须继续标静态路径/fixture/官方文档证据，不得升级为全平台或产品 UAT 结论。
- Docker 架构判断来自 Dockerfile 静态审读；没有运行 `docker buildx` 或目标架构容器，因此只说明 amd64 编译产物与非 amd64 runtime image 的配置不匹配风险，不声称已执行失败或要求产品支持 ARM。
- MCP 跨源凭据转发、无数据 pipe 取消、执行后响应丢失重放是按源码闭环推导，当前没有恶意 HTTP peer/真实子进程故障注入运行日志。应在实施时补具备 side-effect counter 和 secret canary 的回归测试。
- 非协作 Go tool worker 可能继续副作用是 Runtime 并行取消路径的生命周期结论；具体影响取决于工具实现，本轮没有对每个工具逐项制造实际副作用。
- 竞品比较以 Pi/OpenCode 官方文档和固定代码快照为基准，报告明确没有安装或实际操作竞品。UI 相对差距可以据文档对照提出，但不能声称完成产品级横向试用。

## Runtime 计划复核状态

已完成对 [10-runtime-iteration-plan.md](10-runtime-iteration-plan.md)、[proposed-tasks.json](proposed-tasks.json) 与 [findings-to-iterations.csv](findings-to-iterations.csv) 的复核。计划保留当前 AssembleAgent/Loop/SessionTree 组合根，按 R0 止血、R1 运行终态、R2 可恢复 transcript、再到 provider/权限/UI/入口闭环迭代；没有发现要求为本轮 MVP 全量重写或先建分布式 lease 框架的过度设计。

任务图有 **44 项**，ID 唯一、依赖都存在且无环；每项有正确 `phase_id`。规则映射包含 **149 个发现 + D-01**，CSV 正好 **150 行数据**，两边 ID 集合完全相同；逐行核对后，CSV 的 phase、task、builder 角色都与 JSON 中的 `rule_mapping` 展开一致，所有发现至少有一个具体 builder。CSV 对应 **137 项 proposed、9 项 deferred、2 项 product_decision、2 项 pre_enablement_gate**，决策/延期没有被伪装成已确认产品缺陷。特别核对了 TOOL-001 的 cwd 语义边界、PRD-04 预算能力差距、native provider 与 MCP/协议启用门槛、UI dormant 功能，以及 D-01 产品决策；优先级保留来源报告分类，没有把静态闭环写成动态运行通过。CSV `evidence_status` 仍注明其来自来源报告，`confirmed` 可能仅表示源码链已证实；本计划没有重跑相应复现。

44 项中 24 项 builder 任务列出共 **143 个计划新增的测试名**，没有空 builder 或重名测试；验收约束要求先通过 `go test -list` 精确确认测试存在，再跑受影响包的 race suite，避免 `-run` 零匹配假绿。它们是计划验收门，不是本次已执行测试。本轮没有执行任何任务计划中的产品测试。

关键运行时验收正确区分 Outcome 与 ExecutionState：`unknown` 不代表 worker quiescent，用户有界停止等待不能释放仍在运行的 mutation slot；不可协作的 Go goroutine 进入可见 `cleanup_pending/quarantined`，而不是无限 join。R2 对父 CLI 被 SIGKILL 后可能仍活着的 child 增加隔离要求：必须验证 child 退出，或阻断同 session/workdir 冲突写并提供 reconciliation；不要求分布式 lease。R0 已纳入 UI-03 并发审批单槽及 APR-002 Plan Mode 最小止血，完整 capability/effect 合同留在后续阶段。R4 对未知 effect 的 fail-closed 规则允许显式确定为只读的网络能力按 policy 放行。

计划估算已更正为 R0–R8 合计 **71–117 单人有效工程日**，R0–R2 为 **25–42 日**；两者是工作量估计，不是测试结果或日期承诺。默认发布闭包传递包含 41 项非延期任务，3 项 R9 自动续跑任务明确 deferred。以上 DAG、ID/角色、CSV 双向映射和测试门禁为本次手工复核结果；主审另行报告 artifact validator 通过，本 Reviewer 未重复运行该脚本。全量 `make verify` 与当前基线测试结果仍以 09 号报告/日志为准。

## 复核边界

本报告验证和校正分项审计证据，不替代各模块逐文件 manifest。全量 `make verify` 与测试结果以 [`09-verification.md`](09-verification.md) 和保存日志为准；本 Reviewer 没有重复执行全套测试。复现日志中的 PASS 表示“断言观察到当前缺陷”，不表示产品符合目标。覆盖未被分项报告声明的文件或行为仍按 `source-coverage.csv` 标为未审或仅盘点。
