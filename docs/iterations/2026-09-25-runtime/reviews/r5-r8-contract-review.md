# R5 / R8 设计合同独立审查

状态：**通过设计**（在 `d2afc75e05372e33c2ff687eaa07819c44842d45` 定向复核）。初审的三个合同问题均已在 R5/R8 设计中修正；总体分工、终端范围、能力降级和运行中交互与已审阅的 R1/R2 语义一致。此结论仅批准设计，不表示实现或 PTY 验收完成。

审查范围：`r5-ui-design.md`、`r8-capability-design.md`，R1/R2/R7 当前设计合同及其已审阅语义，当前 `internal/tui` / `internal/term` 中的 bridge、model、approval、HITL、颜色与尺寸实现，以及既有 `docs/audits/2026-09-24/05-ui-review.md` 证据。R2/R4 的设计批准记录在 `4f8d1a0d72eebc25fcf06c1fcb897d64bf73d41c`；该批准是合同设计批准，不代表这些依赖已经实现。此次是静态设计审查，没有运行测试或真实 PTY。

## 初审发现与修订闭环

### R5R8-01 — R5 必须将 durable compaction entry 与模型上下文投影分开

**级别：P1，R5 合同遗漏。**

**状态：已关闭。** `d2afc75e05372e33c2ff687eaa07819c44842d45` 为 R5 加入 `EntryTypeCompaction` durable row 与模型上下文 projection 分离，并将 compaction marker、suffix、source boundary 纳入 selected-branch fixture 验收。复核文件 SHA256：`1755fa1546855fb20b8b9f3c1f789b6e5944b5b3520b0ea4479f62c751283829`。

R5 的身份表和恢复验收只枚举 user、assistant、tool receipt、steer/follow-up（`r5-ui-design.md:23-33,61,112-116`），没有 `EntryTypeCompaction`。当前 R2 合同则明确规定：压缩记录在 durable store 中保留为 `EntryTypeCompaction`，provider 兼容恢复时才投影为带 untrusted-summary 前缀的 `Role="user"` 消息；UI 必须保留 compaction 类型，不能把该消息显示成新提交的 user prompt（`r2-contract.md:140-142`）。若 UI 直接复用 provider `SessionView.Messages`，它会把 role 当 transcript 行类型；若只按 R5 当前列举的 entry 类型渲染，则没有 compaction 行的归属规则。这会让一次合法压缩在恢复视图中被误画为用户新请求，或完全消失。

在 R5 增加一个窄的 typed projection 规则：UI 的 durable transcript 行以 `SessionID + BranchID + EntryID` 和 durable `EntryType` 为准，compaction 行显示为摘要/压缩边界；provider 的有效上下文仍由 R2/R3 projection 提供，两者不得互相冒充。UI 不从 EventStream 或 `Role="user"` 推断行类型，也不要求重写 R1 canonical transcript。将 `TestTUITranscriptRestoresSelectedBranch` 扩展为包含 compaction marker、marker 后 suffix 和 source boundary 的 fixture：断言行仍是 compaction 类型、summary 不成为新 user turn、EntryID/branch 不变，并验证显示 transcript 与 provider 有效上下文各自符合其合同。R2/R3 seam 落地后再执行该集成验收。

### R5R8-02 — 400,000 字节不是通用工具输出捕获上限

**级别：P1，R8 验收语义错误；R5 的 cap 用语也需收窄。**

**状态：已关闭。** R8 将 400,000-byte provider candidate guard 与 executor-specific output capture 拆成不同测试；R5 改为引用具体 executor 的 capture contract，并明确审批使用 R4 prepared call 的完整参数。复核 R8 文件 SHA256：`10a85eb454889be0d992c86131e9300f4b81cc65d15568d9257a7c613940ee30`。

`r8-capability-design.md:52-53` 已正确把 1 MiB 大流量列为 renderer-only fixture，但随后把真实 Runtime 400,000-byte 上限称为 `source-capture` 上限，并要求检查源端 truncation 标记和 R2 中的 tool payload。R1 的 400,000-byte 限制实际是：安装 `CompleteResponseGuard` 时，对跨 continuation 的完整 **provider candidate**（文本、tool-call 名称/ID/参数等累计负载）进行上限检查；溢出时不得发布候选或 dispatch tool call。它不定义 stdout/stderr 的捕获大小（`r1-contract.md:144`）。R0 对 Streaming Bash 的独立合同则是按工具 `MaxOutput` 限制合并 stdout/stderr 源字节；当前默认值为 1 MiB（`r0-contract.md:10`、`internal/tools/bash.go:24-25,64-66`）。其他工具的捕获边界也由各自 executor 合同决定。

因此，当前 `TestRuntimeStreamHonors400KBSourceCap` 会把 provider candidate 限额误验成工具输出截断限额，不能按原描述作为有效验收。拆成两个窄测试：

1. provider candidate guard 在安装该 guard 的真实 Runtime 路径下覆盖 399,999 / 400,000 / 400,001 字节；超限断言为不显示任何候选 delta、不 dispatch 工具调用，并返回合同规定的已知错误状态。这项不要求工具 stdout/stderr 被截成 400,000 字节。
2. 工具 source-capture 测试明确选定 executor 和配置，按它自己的 `MaxOutput`/捕获合同验证 retained bytes、truncation metadata、继续 drain 及最终 receipt；只有 R2 receipt 确实保存这类 payload 时才同时断言 transcript 中的值。

同步把 R5 `r5-ui-design.md:47,66,68-72` 的“R1/source cap”泛称改成字段/来源具体的 Runtime capture 引用及其 owner cap。审批必须展示和授权同一份完整、经 R4 决定脱敏后的执行参数；普通 stdout/stderr 的 capture cap 不得被解释成 tool-call args cap，也不得让 view truncation 改变 receipt。

### R5R8-03 — R8 唯一公开能力矩阵缺少 tracing

**级别：P2，R8 能力表不完整。**

**状态：已关闭。** R8 能力表现已添加 Tracing/export 行，并引用 R7 canonical exporter matrix，明确没有 interactive trace viewer。复核 R8 文件 SHA256：`10a85eb454889be0d992c86131e9300f4b81cc65d15568d9257a7c613940ee30`。

R8 将第 59-79 行定义为推荐的唯一公开能力矩阵，并让它成为 `/help`/能力状态的输入，但表中没有 tracing/export。R7 已给出用户可感知的默认路径与明确的降级边界：当前默认 JSONL；`none` 在 assembly 中仍会误建 JSONL；OTLP 名称对应的 body 不是标准 OTLP；所谓 Kafka exporter 实为自定义 TCP，不是 Kafka 协议（`r7-lifecycle-design.md:104-119`）。这些配置不能只存在于生命周期设计里而缺席面向用户的能力表；否则用户看不出 tracing 是实际启用、partial、关闭还是不可用，也可能把 exporter 库误当成产品能力。

在 R8 表内增加 tracing/export 行，或指向 R7 的一个规范能力行（避免复制两份会漂移的协议矩阵）。分别说明默认 JSONL/stdout 的实际状态、`none` 修复前后的语义、未支持的 OTLP/Kafka，以及本项目没有 interactive trace viewer 的事实。状态以当前默认 assembly 为准；R7 修复与验证完成前不把目标行为标成 supported。

## 已确认无须再改合同的部分

- **权威来源分离正确。** R5 明确 EventStream 只作实时观察，不能从 `AgentEvent` 重建历史；provisional delta 与 durable row 分开，并要求由 canonical transcript / R2 durable mapping 对齐（`r5-ui-design.md:21-47,66,72`）。这符合当前 `BridgeEvents` 的转发角色（`internal/tui/bridge.go:99-126`），也没有把 UI event buffer 提升成第二份历史权威。
- **审批、HITL、取消与恢复旅程基本闭合。** R5 将 approval 与 ask_user 分开，要求 FIFO、唯一关联、取消单项不误答其他项；退出要求 waiter 收敛、读取 R1 最新状态并按 R2 持久化未决/quarantine 信息，不自动重放 unknown action（`r5-ui-design.md:49-62,81-89`）。当前 app 已有单 active + FIFO queued 的基本交互结构（`internal/tui/app.go:1809-1853`），但这不构成 R5 要求的 Runtime/durable 完成证据。R5 对 `/resume`/branch 的修复和重启旅程采取了明确 gate，处理正确。
- **R5/R8 的职责边界合理。** R5 保持最小 interaction 和会话投影，通用 scroll/search、cell width/grapheme、性能及终端矩阵交给 R8；主视图 Up/Down 仍标为 accordion 选择而不是伪装成 transcript scroll（`r5-ui-design.md:15,80,91-95`）。
- **终端边界诚实。** R8 将 Linux/macOS 列为待真实 PTY 验证的 partial，将 Windows/其他 GOOS 保持 unavailable；区分 ANSI 交互 TTY 与无控制序列的 pipe/plain 路径，并明确 `NO_COLOR` 只关颜色、`tui.ascii` 只换 chrome glyph，不改写用户 UTF-8（`r8-capability-design.md:21-42`）。这些要求与 `term_size_other.go` 的 80×24 fallback 和现有 `NO_COLOR` 颜色解析方向一致；当前颜色单测不等同于 R8 所要求的完整 PTY 证据。
- **renderer 与 Runtime 预算原本已拆开。** 1 MiB、1,000 chunk、10k 历史是明确标注的 renderer-only 压力场景，性能阈值也标为目标预算而非已有测量（`r8-capability-design.md:50-57`）。只需修正上一 finding 中 400,000-byte 限额所代表的 payload 类型。
- **能力降级方向可靠。** Goal/Task/Todo 明示当前进程内状态；memory 保留已有手工能力及默认自动注入，但要求 R3 bounded selection 和非零 budget；`image_read`、session snapshot/migration、Web UI 和条件装配的插件/Skill 均不因库类型存在就标成默认可用（`r8-capability-design.md:65-79`）。这避免了把“实现存在”写成“默认旅程可用”。

## 复核结论

`R5R8-01`、`R5R8-02`、`R5R8-03` 已在上述固定设计快照闭合，R5/R8 设计可进入 builder。此次闭环仅以精确文档 diff 与其 SHA256 核对；没有重跑功能测试。结论不代表 Runtime、session recovery、PTY 矩阵、renderer 性能或 R7 exporter 行为已经实现或测试通过。
