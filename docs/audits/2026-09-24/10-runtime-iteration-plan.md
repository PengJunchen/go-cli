# Runtime 与产品能力迭代计划

日期：2026-09-24  
状态：审计与计划已完成独立复核；41 项任务 proposed，R9 三项 deferred；未实施产品修改。  
基线：commit 40692e35e4815f0dff1c9f91b13905008f7221a4；810 个 tracked 文件，其中 321 个生产 Go 文件、470 个 Go 测试文件、2 个非 Go source。逐路径基线与审计范围分别见 [source-inventory.csv](source-inventory.csv) 和 [source-coverage.csv](source-coverage.csv)。

## 决策摘要

按现有 Go 执行链补足所有权、终态、持久化与 UI 投影合同；不因组件多或历史任务为 done 而重写 Runtime，也不把 Pi/OpenCode 的功能逐项照抄。继续使用 AssembleAgent、Agent/Loop、TurnRunner、Harness、SessionTree、JSONL 与当前真实装配中的共享 RunSlotGuard。Runtime/RunRequest 可作为逐步收敛的协调 API 名称，但不是新造组装框架或一次性迁移前置条件。

首个可交付目标（R0–R2）是：用户可准确看到一个 Run 是成功、失败、取消请求仍清理中，还是结果未知；会话可在重启后完整恢复；崩溃后已开始但结果不明的外部副作用被标为 unknown / reconciliation required，默认绝不自动重放。此 MVP 不承诺任意 shell、MCP 或插件动作在进程重启后自动继续。

全计划使用“支持 / 部分 / 未闭环 / 暂不支持”描述能力，不给代码成熟度总分。每个发现都在 [findings-to-iterations.csv](findings-to-iterations.csv) 映射到任务、修复或有理由的延期。CSV 的 evidence_status 是来源报告的分类；confirmed 可以表示源码链已证实，不代表本计划重新运行过测试。是否有动态复现须查看对应报告引用的 reproducer、执行日志和证据等级。源清单中仅盘点而未被分项报告逐文件审阅的路径继续标记为未审，不能推断无问题。

## 当前判断与需保留的边界

| 项目 | 当前判断 | 迭代约束 |
|---|---|---|
| Go Runtime、AssembleAgent、Loop/Agent、TurnRunner、SessionTree | 支持基础组件，端到端终态与恢复仍未闭环 | 保留现有组合根和执行层，在其边界补合同；先修能复现的用户失败，再整理抽象 |
| RunSlotGuard | 实际 CLI Assembly 已把共享 guard 注入相关执行构件 | 继续复用并补证据；不得再以旧 no-op/未装配入口断言当前主链没有共享 guard |
| EventStream | 有界事件投影，默认 timeout 为 30 秒；不是权威历史 | 保留投影角色。让调用方观察超时、drop、sequence gap；不把它升格为无差别事件溯源，也不忽略发送错误 |
| SessionTree / JSONL | 树与追加式 JSONL 有可复用能力，身份、durable commit、文件恢复仍有明确缺陷 | 增量增加 session / entry / tool-call identity 与 journal commit 点；不迁移到 SQLite，不重写树 |
| TUI | Bubble Tea、LineEditor、审批卡片、工具折叠、steer/cancel 等已有可用构件 | 先修交互闭环与同一 transcript，再做编辑器、viewport、渲染性能与可访问性 |
| provider / tools / extension | 库内功能比默认 CLI 实际接线更宽 | 在实际入口统一 resolver/capability；未完成端到端合同的能力明确拒绝或标暂不支持 |
| Phase 48/49 与 verify | 任务为 done 不等于当前 HEAD 的 gate 可复现；原始 make verify 有失败 | 原始失败与 NO_COLOR 清除后的 race 通过、scan 20 ERROR、CI 过滤差异都保留为基线；关闭必须有当前 commit 的命令与日志 |

## Runtime 必须满足的不变量

1. **运行是否结束与外部结果分开。** ExecutionState 表示运行中、cancel_requested、draining、quiescent 或 quarantined；Outcome 表示 success、error、cancelled 或 unknown。unknown 只说明动作效果不可证明，不说明 worker 已停止。cancel_requested 不是 terminal outcome。
2. **资源由唯一 owner 收敛。** 每个 Run 有明确 owner 负责 reader/worker/child process、事件发布、session commit 与 shutdown。并行工具被取消后，不可仅因等待者返回或将结果标 unknown 就释放 workspace mutation slot。允许用户在有界时间后停止等待；未协作的 goroutine/进程进入 cleanup_pending / quarantined 并持续受监督。只有确认 worker 退出，或 killable 子进程被杀并 join，或工作区被真实隔离，才允许后续相同边界上的变更动作。不可无限 join 一个不可杀的 Go goroutine并拖死全局 Runtime。
3. **副作用先有意图、后有结果。** 工具派发前持久化带稳定 ToolCallID / ActionID 的 intent；动作后保存结果 receipt。启动后无法判定效果时存 unknown；恢复流程不得自动重放 shell、MCP、remote、Git 或其它副作用。后续显式自动恢复必须有 action verifier/receipt、owner-generation fence、幂等/对账策略和隔离边界。
4. **历史是合法的对话协议。** 每个模型 tool call 最终对应同 ID 的 tool result，或明确的 interrupted/unknown 封口投影。恢复和 compaction 保持 tool call/result 成组、final/error 与真实终态一致，不能留下模型无法继续的半开消息。
5. **落盘确认先于成功确认。** UI 可以流式显示 provisional token，但在 session append/flush/sync 的约定 commit 点成功前不能显示“已保存/已完成”。持久化失败必须成为可见 save error / uncommitted outcome。每个语义记录或事务提交时 flush/sync，不逐 token fsync。
6. **恢复不猜测。** 新会话明确创建新的物理 session 文件及 root parent；resume 使用 session/file ID 定位 session，再以 entry ID 定位树节点。加载完整投影，包括 compaction 摘要、工具闭合记录、可见 user/final 内容。锁在读/修复/加载之前取得并贯彻单写者。合法 JSON 但无结尾换行时保留记录并补 delimiter；只有证实截断的 JSON 尾可以隔离/裁剪，完整但非法的记录 fail closed；不能删掉可能代表已完成副作用的 receipt。父 CLI SIGKILL 后如果 child/process group 可能仍活着，以可验证的执行身份判断是否已退出；禁止根据裸 PID 杀进程。若不能证明旧执行已停，恢复同 session/workdir 的冲突写操作保持 quarantined/blocked，允许安全读操作并提供人工 reconciliation；不靠清空内存 RunSlot 证明安全。
7. **同一能力语义贯穿入口。** Prompt、REPL、TUI、serve、ACP、subagent 共用 provider/credential scope、tool capability/effect、approval scope、session identity 与 terminal outcome。Unsupported 明确报错，不得伪造结果或把未知工具默认为安全。
8. **重试遵守可见性与副作用边界。** 可以在内容/工具动作尚未对用户可见且无副作用的阶段重试；输出已流出、tool intent 已派发或外部结果不明后，除非请求带幂等标识并有 receipt/verifier，不得透明重放。

## 迭代顺序、门槛与估算

依赖图、角色、交付物与可执行验收项见 [proposed-tasks.json](proposed-tasks.json)。每片均按 designer → builder → reviewer；builder 每个闭环一个有意义的 commit，reviewer 不沿用 builder 的单测通过作为独立证明。以下天数是单人有效工程日范围，涵盖实现、契约与测试；不含外部凭据申请、跨团队产品决策等待与正式人工视觉验收。

| 阶段 | 目标 / 关键发现 | Exit gate | 估算 |
|---|---|---|---:|
| R0 止血与验证可信度（并行前置） | 有独立回归用例后立即修复：一次 Allow 误缓存、缺少 content 导致文件清空、wildcard + no-auth、provider key 错投、TUI ask_user 无人接收/并发 approval 覆盖、Plan mode 对未知/写能力临时 fail closed、Streaming Bash 截断后卡管道、MCP 跨源凭据（仅直接 adapter API 配 Header/Bearer/OAuth 时）/断线重放、默认 prompt trace 泄露；并修 VFY-01..10 gate | 热修在稳定 Run API 之前即可独立合并；各具体触发条件有端到端反例；未知 provider/配置 fail closed；本机与 CI 对同一 gate 有相同阻断结论；scanner 崩溃/坏 JSON 非零退出；不因零测试或静态字符串计数而 PASS。原始 make verify 失败、NO_COLOR 清除后的 race 通过与 scan 20 ERROR 证据全部保留；修复后日志绑定新 commit | 8–15 |
| R1 Run 安全终态 | provider stream 终态 / guard；cancel、join、quiescence、历史闭合；资源和输出上限（RT-01..10、LLM-03..05、TOOL-006/008） | failure injection 证明一次 Run 恰有一个 outcome；取消后同工作区 mutation slot 仍被占用直到真正退出或隔离；stream 断流/timeout/guard failure 均不能变 success；不合作 worker 到期转 quarantine 并可观察，进程不挂死 | 8–13 |
| R2 可恢复 transcript MVP | session/file/entry identity、writer lock、append durability、恢复、tool intent/result（SES-01..14、RT-11、CLI-11） | CLI 子进程 SIGKILL 位于输入/intent/tool result/final/sync 各边界；重启能恢复已提交记录；未完成 side effect 显示 unknown 且执行计数仍为 1；parent SIGKILL 后可能存活的 child 未被证明退出前，同 session/workdir 的冲突写保持 quarantined/blocked；安全读可继续并提供人工对账；保存错误无“成功已保存”ACK；合法大行与 torn tail 行为明确 | 9–14 |
| R3 provider、loop 与上下文协议 | provider resolver/capabilities、SSE、retry/circuit、cost、compaction/message grouping、memory 搜索（LLM-01..16、PRD-01..04/09/10、CMP-01..04、MEM-01/02） | httptest fixture 覆盖真实 CLI factory 到 adapter 的请求与错误终态；凭证不会跨 provider/origin；原生 adapter 未过双向 tool-call/image/system/schema 合同前不可启用；compaction 保持 call/result 组，首个 provider 请求前检查上下文窗口/token 预算。费用硬预算只在 PRD-04 产品决策明确后做 | 8–13 |
| R4 工具与权限策略 | capability/effect、plan mode、审批缓存/unknown enum、文件安全/schema/输出限制、搜索缺省（TOOL-001..006/008/009/011、APR-001..005/008、D-01） | 对所有注册内建/扩展工具按 effect 生成策略矩阵；缺失字段与被产品承诺的 workspace 边界在真实 middleware 后不落盘；一次 allow 仅授权一次；unknown effect/classification fail closed；Plan mode 不存在未拦截副作用路径。TOOL-001 按 P2 契约澄清：配置若只限制 cwd 就如实命名说明；若产品要文件系统隔离，作为 OS sandbox 独立目标，不能以 shell 文本解析冒充隔离 | 8–13 |
| R5 TUI 主旅程 | 并发审批、取消、真实 transcript 与事件投影、多轮输入（UI-02..08/11/12/16；UI-01 的止血在 R0） | PTY journey 覆盖输入→tool approval / ask_user→结果→cancel/resume；并行两审批均有唯一请求 ID、无需覆盖；TUI 与 JSONL 对话相符；仅展示 durable terminal 为完成；help/key 实际相符 | 7–11 |
| R6 CLI / ACP / MCP / Web 入口闭环 | 配置/trust、session/SSE隔离、stdio protocol、MCP reload/teardown/version contract（CLI-02..17、MCP-01..09、APR-006；CLI-01/MCP-01/02 的止血在 R0） | 真实本地子进程/fixture servers 覆盖各入口身份、timeout、事件隔离、credential origin 与非幂等断线；禁 auth 与 wildcard 组合必须拒绝或仅绑定 loopback；未接 Agent 的 Web/不兼容 ACP 不能宣称可用 | 9–15 |
| R7 扩展、Skill、Tracing 和审计 | 生命周期、schema、公开能力边界、trace 敏感信息/parent/status/queue、审批审计与统计（EXT-01..07、SKILL-01..04、TRACE-01..06、PRD-05..08、MEM-05、RT-09） | 并发 init/stop 和 event/drop 故障注入无静默成功；trace/log 不含原始 prompt、secret、tool args；失败 status 真实；none 彻底无 exporter；扩展和 skill 能力矩阵可执行或明确 unsupported | 8–13 |
| R8 UI 质量、能力声明与发布复验 | 大 transcript renderer、Unicode/ANSI、终端平台/accessibility、产品 dormant capability、Docker build/runtime architecture 声明与最终 gate（VFY-11） | 有真实 PTY 或受支持终端矩阵证据；交互延迟/内存随增长有上界；所有用户文档/help 中的能力有 default-path smoke，未接线者显式 unavailable；Docker 架构要么明确限制 amd64，要么让 build target 与 runtime image platform 一致；release gate 复跑无假绿 | 6–10 |
| R9 任意跨进程动作自动续跑（可选、暂缓） | 限定小集合的可验证 action resume；shell/MCP 不纳入默认重放 | 用户显式开启；每种 action 有稳定 receipt、验证器、generation fence、重复触发安全证明和人工 unknown reconciliation。任何一项不成立则保留 R2 行为 | 10–20（单独批准范围后估算） |

## 发布波次与并行关系

上表是工作流编号，不表示逐阶段串行。先按用户损害止血，再让运行时合同与持久化成为主干；其余入口、权限、provider 与 UI 在共享合同稳定后由不重叠工作区并行。建议按四个发布波次验收：

| 波次 | 包含阶段 | 发布判断 | 关键依赖 |
|---|---|---|---|
| A 止血 | R0 高风险热修 + 验证 gate 前置 | 修复一次授权重复使用、损坏工具参数清空文件、无认证 wildcard、provider 凭证错投、HITL 死等/并发请求覆盖、Plan mode 临时阻断未知写能力、MCP 凭证跨源/调用重放、Stream Bash 卡死及 trace 原文泄露；每项先有失败测试 | R0 热修互相独立；gate 工程并行，不要求等完整 scanner 重构才修用户风险 |
| B Run 与恢复 MVP | R1 + R2 | 明确终态/清理中状态，完整恢复已提交对话，崩溃后外部副作用标 unknown 且不重放 | R1 合同先于 R2 commit/ACK；故障注入与子进程重启成为发布门槛 |
| C 能力与交互闭环 | R3 / R4 / R5 / R6 | provider/config、compaction、permission/capability、CLI/ACP/MCP、TUI 旅程闭环 | 各 lane 共用 R1 terminal、R2 session、R4 capability contract；契约审定后可并行，禁止多 lane 复制单独策略 |
| D 兼容与发布 | R7 + R8 | 扩展/skill/tracing 公开边界、UI 可用性/性能/跨平台、文档和 release gate 复验 | 依赖 C 中真实 default-path 已达成的合同；未集成能力下架/标 unavailable |

R0 与 Runtime 合同设计可并行；R1 完成后，R3/R4/R6/R7 的设计与无冲突 builder 可平行推进。R2、R5 和入口的执行接入须消费稳定的 terminal/session/capability 合同。R0–R2 单人 MVP 约 25–42 工程日；R0–R8 阶段区间直接相加为 71–117 工程日（不是日历时间预测）。两个不重叠 builder 在 R1 合同落定后预计 9–15 周日历时间；时长对跨入口故障测试与跨平台 TUI 复验最敏感。真正日历关键路径以 JSON 任务 DAG 为准：例如 R6 adapter builder 还等待 R4 权限审查，R7 等待 R6 支持版本决策，不能假设这些 lane 可完全同步开工。

## UI 单独路线：对标工作流而不是外观

对标依据是 [05-ui-review.md](05-ui-review.md) 中引用的 Pi 与 OpenCode 官方文档/代码快照；不是声称本轮安装、运行或逐像素比较了竞品。Pi 的 session tree/fork、消息 steer/queue、slash/@/! 完成、键位与程序化入口；OpenCode 的 session/history、权限 once/always/reject、工具详情、model 选择、编辑器与 undo/redo/export，可作为用户旅程参考。实际 UI 目标是同一份 Runtime transcript 的可观察性与可信控制：

| 路线 | 目标 | 验收旅程 |
|---|---|---|
| UI-0 / R5 | 交互收敛 | ask_user 有明确答复；两个并行 approval 都能处理/取消；Ctrl+C 与 modal/帮助一致；结束状态不重复渲染 final；Send 满时返回 backpressure/error |
| UI-1 / R5 | 会话闭环 | 显示当前 session 的 user/assistant/tool/warning/approval；从历史恢复、fork/切换要和真实 session identity 对齐；steer 与队列行为有显式 keybinding/help |
| UI-2 / R5 | 工具与权限可解释 | 保留 tool 参数/摘要、审批依据、diff 和执行结果；显示 provisional 与 durable 状态；未实现的 d 键移除或真正支持 |
| UI-3 / R8 | 编辑与浏览 | 多行编辑/外部编辑器、稳定 viewport/search、可配置 keybinding、undo/redo/export；不是把每个 Pi/OpenCode 命令都搬进来 |
| UI-4 / R8 | 渲染质量 | ANSI/Unicode/CJK 单元格边界、窗口 resize、长 transcript 有界增量 render；目标终端 PTY 与 ASCII/accessibility fallback 有明确支持矩阵 |

规划的关键新测试（均为未来新增，非宣称已有）：TestTUIHandleHITLMessage、TestTUIApprovalQueueDoesNotLoseRequests、TestTurnDoneDoesNotDuplicateAssistantText、TestTUIEventProjectionPreservesToolArgs、TestTUITranscriptRestoresSelectedBranch、TestTUIStopWaitShowsCleanupPending、TestTranscriptViewportCJKAndANSI、TestStreamRenderBoundedLargeTranscript。验收 runner 必须先从 go test -list 输出中精确找到预期测试名并断言数量，再执行包级 race suite；仅用 -run 模式不可作为验收，以免零测试假绿。归档中的 reproducer 通过通常表示它成功证明了旧缺陷仍存在；把它作为修复验收前，先改写为正确行为断言，确认该断言在审计基线真红，再验证修复后的变绿结果。不能将“复现 bug 的 PASS”报告成修复 PASS。

## I_WORK_MATE 的稳定化经验如何落到本计划

历史证据见 [08-iworkmate-lessons.md](08-iworkmate-lessons.md)。按 author/commit 日期限定，朱一辰 8 月 20–21 日主要修复运行时资源/SSE 生命周期问题，8 月 29–30 日有 Pi/Harness 合同工作；稍后的 durable worker/checkpoint/replay 主要由其他提交者推动，不能统一归因给朱一辰。本计划移植的是验证方法和可迁移约束，不移植前端、SQLite 或声称复刻其实现：

- R1 的关闭路径按 reader、事件源、worker、child process 的真实资源所有权修正；用静默阻塞读、心跳、坏帧、取消和 shutdown 注入验证，不能仅验证重连成功。
- Runtime 回归从实际 AssembleAgent/Loop/TurnRunner/registry/approval/session 路径进入；Fake runner 或 mock ToolServer 只做单元边界证据，不能代表真实闭环（VFY-08）。
- observer/hook 错误要隔离并被有界报告器观察；报告器自身失败也不能导致主执行或 shutdown 卡死。
- R2 采用 action ID、intent、receipt、owner/generation；提交前发送可见事件会造成“用户见到了但崩溃后没有”的裂缝，结果未知时进入 reconciliation，不偷偷重放。
- 对完整 CLI 使用子进程 SIGKILL 注入，恢复后断言 session cursor 单调、同一 action 不重复、每个 tool call 恰有一个结果/unknown receipt、每个 Run 恰有一个 terminal projection；同时留存反例日志，防止假绿。

## 延期与显式产品决策

- 任意 shell/MCP 跨进程自动恢复：R9；没有 verifier/receipt 和隔离边界时只提供中断/unknown。
- native Claude/Gemini 协议：R3 的启用门槛；默认 CLI 当前未接 adapter 时，不把协议缺口描述为默认主路径故障。每个协议须完成 tool call/result、system、images、schema、finish reason 和错误 fixture 后才可出现在支持列表。
- hard cost budget：PRD-04 暂无已声明的硬预算承诺，先保留该产品决策；PRD-10 stream usage 低报需按 R3 修复。不可将“没有硬预算”伪装成 bug 已确认。
- ImageRead、Goal/Task/Todo 持久化、Memory 自动检索、Session branch/snapshot/migration、Extension commands/providers/remote RPC、Skill progressive registry、Web Agent UI：当前路径未完整接线或产品承诺未确认。R6/R7/R8 先让默认 CLI/help/docs 明确 unavailable；只有产品选择上线后才增加调用链与真实重启/协议测试。
- Plan mode 退出由模型触发还是需用户确认（D-01）、TUI Bash 是否可经人工确认执行、未知 tool effect 的策略，由 R4 设计者与产品 owner 在实现前给决策表；未知能力默认拒绝写操作。
- MCP/OTLP/Kafka 的标准互通：先冻结产品实际支持的协议版本和完整 wire contract；在对应版本 conformance fixture 或真实 collector/server 通过前只承诺明确子集。审计报告可能记录在写作时较新的协议能力，但缺少最新版本支持本身不自动构成 bug；名字或 adapter 类型也不能代替符合已声明版本。

## 统一验收规约

1. 所有命令记录 commit、Go 版本、平台、关键环境变量与退出码；保留 make verify 原始日志，另存隔离 NO_COLOR 后日志；不要覆盖或改写历史失败证据。
2. 单元测试之外，每个 R1–R7 builder 都至少加一条走生产装配的测试。需要外部协议时使用本机 httptest/pipe fixture 验证真实 adapter；provider 凭据和外部网络不作为默认 CI 前提。
3. lifecycle、durability、approval、event loss 必须注入失败点：provider read/parse/timeout、tool start/output/return、Append/Flush/Sync、cancel during blocked I/O、shutdown concurrent with reconnect、event queue full、approval callback fail。
4. R1/R2 验收要求真实命令行子进程与可杀 child-process seam。Go goroutine 不可杀：测试有界等待和 quarantine 状态，不能以无限 Wait 等待作为修复。
5. 所有新增 run/session/tool event 带稳定 identity 与单调序号；投影侧可以丢失并报告 gap，权威 transcript 不得丢失已提交记录。
6. `go test -run` 必须配套测试注册检查及包级 race suite；所有 `t.Skip` 有清单、原因、拥有者和解除条件。
7. Rule、Makefile、CI、infra tasks/rules 等受保护文件依 go-cli-infra 的审批链执行。此提案不修改它们；未来 builder 必须先提交具体 diff 与证据，再走保护流程，不可将本计划当成批准。

## 关联报告

计划映射保留全部稳定发现 ID 和来源优先级：

- [01-runtime.md](01-runtime.md)、[02-llm-production.md](02-llm-production.md)、[03-tools-approval.md](03-tools-approval.md)
- [04-session-compaction.md](04-session-compaction.md)、[05-ui-review.md](05-ui-review.md)、[06-cli-config-acp.md](06-cli-config-acp.md)
- [07-extensions-tracing.md](07-extensions-tracing.md)、[08-iworkmate-lessons.md](08-iworkmate-lessons.md)、[09-verification.md](09-verification.md)
