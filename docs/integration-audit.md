# go-cli 集成审计报告（Integration Audit）

> 初次审计日期：2025-08（基于早期 main 分支代码）
> 复审计日期：2026-08-06（基于当前 main 分支代码，全链路重新静态跟踪）
> 审计方式：以 `interactive` 交互入口为起点，沿 `assemble.go` 全链路静态跟踪运行时接线
> 审计目的：客观回答 —— 各功能模块是否真正"有机组合"并服务于客户，包括 SubAgent 等子系统的真实使用情况；对照成熟 CLI Agent 的差距清单。

> **复审说明（2026-08-06）**：本项目自初次审计以来已显著改善。初次审计标记的大部分关键缺口（SubAgent、审批、会话持久化、压缩回写、中间件链、Registry）均已修复并接入运行时。当前状态：核心 ReAct Loop + 工具 + 审批 + 子智能体 + 会话 + 压缩 + 生产化治理 + 中间件链均已接线。剩余缺口集中在 ACP、FileMutationQueue、PluginLoader、Extension 系统。

---

## 0. 结论摘要

本项目已从初版"**设计完成度极高、集成完成度极低**"的工程样板，演进为**设计完成度高、集成完成度显著提升**的可生产化 CLI Agent。

当前运行时已有机组合的能力：核心 ReAct Loop + 内建工具 + MCP/Skill 工具 + 审批门（deny-first）+ 生产化治理（重试/熔断/幂等缓存/审计/Telemetry）+ 输出守卫 + 真实 SubAgent + 6 层中间件链（日志/环路检测/计划模式/系统提醒/失败合成/Hook）+ 压缩回写 agent history + 会话持久化与恢复 + TUI 流式渲染 + 全链路 Tracing + 并行工具执行。

以客户视角，当前可用能力 ≈ 一个**带审批、带会话恢复、带子任务委派、带重试熔断、上下文有压缩上限**的"多轮对话 + 工具执行"Agent，已具备进入生产的基本条件。

**剩余缺口**：ACP 协议层完全孤立；`FileMutationQueue` 已实现但未接线（仅 `MutationWrapper` 被使用）；`DeferredToolRegistry` 未使用；PluginLoader 返回 stub；Extension 系统无宿主驱动。

---

## 1. 入口链路与运行时实际接线图

**入口**（`cmd/cli/main.go` → `internal/cli`）：

```
main() → config.Load() → newTracing() → cli.Run()
  → interactive.submit / prompt → AssembleAgent（assemble.go 统一组装）
    → buildModel(LLM)                                          // assemble.go:175
    → ToolRegistry + RegisterDefaults（bash/read/write/edit/…）// assemble.go:203-218
    → registerMCPTools（stdio + HTTP/SSE）                     // assemble.go:221-223
    → registerSkillTools（YAML frontmatter + Adapter）         // assemble.go:226
    → ApprovalMiddleware（deny-first + Classifier + TrustStore
      + Callback + Cache + PermissionModeResolver）            // assemble.go:229-244
    → MutationWrapper（写/编辑串行化，注：非完整 FileMutationQueue）// assemble.go:244
    → ProductionModelWrapper（Retry + CostTracker + Stats）    // assemble.go:248-261
    → CircuitBreaker（连续失败保护）                           // assemble.go:264-272
    → IdempotentCache + AuditLog + Telemetry（工具调用层）     // assemble.go:275-299
    → OutputGuardChain（PII + 代码注入 + 长度守卫）            // assemble.go:303-307
    → RealSubAgentFactory（真实 LLM + 工具的子智能体）         // assemble.go:309-317
    → LoopAgent（WithLLM + WithTools + WithModelWrapper
      + ExecutionModeParallel + Tracer + SystemPromptBuilder）  // assemble.go:382-421
    → 6-layer MiddlewareChain 包裹 LoopAgent：
        1. LoggingMiddleware
        2. LoopDetectorMiddleware（环路检测 → SystemReminder）
        3. PlanModeMiddleware
        4. SystemReminderInjector
        5. FailureSynthesisMiddleware（可恢复错误重试）
        6. HookMiddleware（pre/post-turn hooks）              // assemble.go:445-452
    → CompactionHook（compactor + estimator，回写 agent history）// assemble.go:454-503
    → SessionStore（JSONLSessionStore，if enabled）           // assemble.go:470-478
    → Session Resume（从 session 文件恢复 history）           // assemble.go:493-499
    → AgentImpl + HarnessImpl                                 // assemble.go:508-509
  → TUI 渲染（BubbleteaApp + BridgeEvents）
  → 每轮写 SessionStore → autoCompact（现已真正作用于 agent history）→ 下一轮
```

**该链路上真正"有机组合"的模块**（✅ 经代码验证接线）：

| 模块 | 状态 | 证据 |
|---|---|---|
| Config 五层合并加载（Default<File<Env<Flag<Override） | ✅ 真接线 | `main.go` 调用 `config.Load`，Loader 完整实现 |
| Tracing（span + JSONL/OTLP/Kafka exporter + Async + slog） | ✅ 真接线 | `assemble.go:191-200` 建 tracer，全链路打 span |
| LLM（Eino/OpenAI 兼容 HTTP + Native 三厂商原生 HTTP 客户端） | ✅ 真接线 | `assemble.go:175` `buildModel` → `model.Stream` 流式调用 |
| ReAct Loop（think→act→observe，max-iterations 防护） | ✅ 真接线 | `internal/core/loop.go` |
| 内建工具（bash/read/write/edit/grep/find/ls/search/todo/task/goal/web/git/plan_mode） | ✅ 真接线 | `assemble.go:211-379` 注册全部工具 |
| MCP 工具（stdio + HTTP/SSE 适配） | ✅ 真接线 | `assemble.go:221-223` `registerMCPTools` |
| Skill 技能（YAML frontmatter 加载 + Adapter） | ✅ 真接线 | `assemble.go:226` `registerSkillTools` |
| 审批门（ApprovalMiddleware deny-first + 交互确认） | ✅ 真接线 | `assemble.go:229-244` 完整审批链接入工具注册表 |
| 生产化治理（Retry + CircuitBreaker + IdempotentCache + AuditLog + Telemetry） | ✅ 真接线 | `assemble.go:248-299` 全部接入模型包装与工具包装 |
| 输出守卫（PII + 代码注入 + 长度） | ✅ 真接线 | `assemble.go:303-307` `OutputGuardChain` |
| SubAgent 真实执行（委派子任务，复用 LLM + 工具） | ✅ 真接线 | `assemble.go:309-317` `NewRealSubAgentFactory` |
| 6 层中间件链（洋葱模型） | ✅ 真接线 | `assemble.go:445-452` `MiddlewareChain.Wrap` |
| 压缩回写 agent history（真正作用于 LLM 上下文） | ✅ 真接线 | `agent.go:113-126` compactionHook 回写 `a.history` |
| 会话持久化 + 恢复（JSONLSessionStore + --resume） | ✅ 真接线 | `assemble.go:470-499` + `interactive.go:321-342` |
| Registry（DIP 中心，已实例化并注册子系统） | ✅ 真接线 | `assemble.go:166` `core.NewRegistry()` |
| 并行工具执行（ExecutionModeParallel） | ✅ 真接线 | `assemble.go:386` 默认并行，config 可覆盖为顺序 |
| TUI（accordion 流式渲染、主题、增量打字机效果） | ✅ 真接线 | `tui.BridgeEvents` → `BubbleteaApp` |
| 动态系统提示词（AGENTS.md/CLAUDE.md + Skills 注入） | ✅ 真接线 | `assemble.go:397-419` `SystemPromptBuilder` |
| 环路检测（重复编辑/测试失败/相同工具调用 → SystemReminder） | ✅ 真接线 | `assemble.go:424-432` + 中间件链 |
| 失败合成（可恢复错误 → 合成重试消息） | ✅ 真接线 | `assemble.go:435` `FailureTurnSynthesizer` |
| Git 工具（diff/status/commit） | ✅ 真接线 | `assemble.go:367-369` |
| Mock 框架 / 测试基建（2.8 万行实现 vs 4.8 万行测试） | ✅ 使用充分 | mock LLM/Tool/MCP server、conversation template |

---

## 2. 关键发现

> 判定方法：`grep` 非测试代码的包级 import / 函数调用 / Registry 方法调用，并逐行核对 `assemble.go`。

### ✅ 2.1 SubAgent —— 已修复

- `assemble.go:309-317` 使用 `core.NewRealSubAgentFactory(model, tr, ...)` 创建真实子智能体工厂，复用主模型的 LLM 和工具注册表。
- `core.RegisterSubAgentFactory(subAgentFactory)` 注册到全局工厂；`tr.Register(ctx, core.NewSubagentTool(dispatcher))` 将子智能体委派工具注册到工具表。
- 子智能体不再是模拟器占位，而是具备真实 LLM 调用 + 工具执行能力的执行体。AGENTS.md 宣称的"子智能体"理念已落地。

### ✅ 2.2 Approval（审批/安全层）—— 已修复

- `assemble.go:229-244` 接入完整审批链：`ApprovalMiddleware`（deny-first）+ `SafetyPolicyClassifier`（bash 为危险操作）+ `InMemoryApprovalStore` + `InteractiveApprovalCallback`（os.Stdin 交互确认）+ `ApprovalCache` + `DefaultPermissionModeResolver`。
- 工具注册表通过 `tools.NewMiddlewareToolRegistry(tr, approvalMW.WrapToolCall, tools.NewMutationWrapper())` 包装，**所有工具调用都经过审批门**。
- Bash 工具现在会触发审批确认，`--auto-approve` 可通过 `approval.WithAutoApprove(false)` 配置。
- 配套的 `production` 层（重试/熔断/幂等缓存/审计/Telemetry）也全部接入运行时（`assemble.go:248-299`）。

### ✅ 2.3 Session（持久化/恢复/续聊）—— 已修复

- `assemble.go:470-478`：当 `enableSession` 且 `rc.Session.StorePath` 非空时，创建 `JSONLSessionStore` 并 `Open`。
- `assemble.go:493-499`：当 `resumeFlag` 为真时，从 session 文件加载历史消息并注入 `AgentImpl` 的初始 history。
- `interactive.go:321-342`：每轮对话结束后，将用户输入和 AI 回复以 `SessionEntry` 形式 `Append` 到 session store 并 `Save`。
- config 中的 `session.id`、`session.store_path` 现已被消费。退出不再失忆，支持 `--resume` 续聊。

### ✅ 2.4 Compaction（压缩回写）—— 已修复

- `assemble.go:503`：`core.WithCompactionHook(newCompactionHook(compactor, estimator, ac.maxTokens))` 将压缩钩子挂入 `AgentImpl`。
- `agent.go:113-126`：`AgentImpl.Run` 在每轮结束后调用 `compactionHook`，**压缩结果直接回写 `a.history`**（`a.history = compacted`）。
- 下一轮 LLM 调用时，`submission.History = historyCopy`（`agent.go:92`）使用的是已压缩的 history，**压缩真正作用于请求上下文**，不再无限增长。

### ✅ 2.5 Middleware / Hook / 扩展机制 —— 已修复

- `assemble.go:445-452`：`core.NewMiddlewareChain(...).Wrap(loop)` 将 6 层中间件按洋葱模型包裹 LoopAgent：
  1. `LoggingMiddleware`（日志追踪）
  2. `loopDetectorMiddleware`（环路检测 → SystemReminder 注入）
  3. `PlanModeMiddleware`（计划模式拦截）
  4. `SystemReminderInjector`（系统提醒注入下一轮）
  5. `FailureSynthesisMiddleware`（可恢复错误 → 合成重试消息）
  6. `HookMiddleware`（pre/post-turn hooks，通过 `HookChain` 扩展）
- LoopAgent 不再是裸环，中间件链完整生效。
- **注**：`ExtensionRegistry` 框架仍存在但无宿主驱动（见 §2.7）。

### ✅ 2.6 核心 Registry（DIP 中心）—— 已修复

- `assemble.go:166`：`reg := core.NewRegistry().(*core.DefaultRegistry)` 在生产路径中实例化。
- 后续通过 `reg.RegisterModelProvider`、`reg.RegisterApprovalClassifier`、`reg.RegisterApprovalStore`、`reg.RegisterToolRegistry`（两次）、`reg.RegisterCompactor`、`reg.RegisterTokenEstimator`、`reg.RegisterTraceExporter` 等注册子系统。
- Registry 中的默认实现不再是死代码，`AgentAssembly.Registry` 暴露给调用方用于依赖注入。

### 🟠 2.7 MutationQueue / DeferredTools / ACP / Plugin / Extension —— 仍为剩余缺口

- **FileMutationQueue**：`internal/tools/mutation.go` 的 `DefaultFileMutationQueue`（写/编辑按文件串行化、symlink 归并、per-file worker goroutine）有完整实现，但 `assemble.go:244` 只使用了 `tools.NewMutationWrapper()`（轻量互斥锁包装），**未调用 `tools.WithMutationQueue`** 接入完整的队列。队列的高级能力（异步排队、per-file worker、结果 channel）在运行时未生效。
- **DeferredToolRegistry**：`internal/tools/deferred.go` 的懒加载工具注册表有实现，但 `assemble.go` 不使用，生产路径中所有工具在启动时全量注册。
- **ACP 协议层**（`internal/acp`：gRPC/stdio 适配 + middleware）：整个包零外部生产引用，完全孤立。仅在 e2e 测试和 AGENTS.md 文档中被引用。
- **PluginLoader**：仍返回 `errPluginsUnsupported`（`internal/core/stubs.go`），插件热加载未实现。
- **Extension 系统**：`internal/extension` 的 `Manager`/`Registry`/`Coordinator` 框架存在，但无宿主驱动，`assemble.go` 不引用 `internal/extension`。

---

## 3. 对照成熟 CLI Agent（Claude Code / OpenHands / Codex CLI）的差距清单

### 3.1 基础设施 / 交互体验类（直接影响可用性）

| # | 能力 | 现状 |
|---|---|---|
| 1 | 危险操作人工确认（bash/删改文件 y/n 提示、auto-approve 档位） | ✅ 已接线。`assemble.go:229-244` ApprovalMiddleware 完整接入 |
| 2 | 会话恢复/续聊（`--resume`、`--continue`） | ✅ 已接线。`assemble.go:493-499` + `interactive.go:321-342` |
| 3 | 会话分支与回滚问答 | ❌ 分支树 `session/tree.go` 仍为 dead code，未接入交互 |
| 4 | 斜杠命令（`/help` `/compact` `/undo` `/model` `/cost`） | ✅ 已接线。`internal/cli/slash.go` + `slash_handlers.go` + `slash_registry.go` |
| 5 | Undo / Checkpoint | 🟡 部分。`FileTracker` 已接线用于备份/恢复，但无斜杠命令暴露 undo |
| 6 | 写文件前 diff 预览 | ✅ 已接线。`DiffGenerator` 注入 WriteTool/EditFileTool |
| 7 | Token 用量 / 成本统计 | ✅ 已接线。`CostTracker` + `StatsRegistry` + `Telemetry` |
| 8 | 上下文占用可视化（已用窗口 %） | 🟡 部分。压缩已回写 history，但 TUI 未暴露实时窗口占比 |

### 3.2 Agent 能力类（直接影响任务质量）

| # | 能力 | 现状 |
|---|---|---|
| 9 | SubAgent 真实执行（委派子任务） | ✅ 已接线。`assemble.go:309-317` `NewRealSubAgentFactory` |
| 10 | 压缩真正作用于请求上下文 | ✅ 已接线。`agent.go:113-126` 回写 `a.history` |
| 11 | LLM 失败重试 / 指数退避 | ✅ 已接线。`assemble.go:248-261` ProductionModelWrapper + RetryPolicy |
| 12 | 熔断器（连续失败保护） | ✅ 已接线。`assemble.go:264-272` CircuitBreaker |
| 13 | 输出质量守卫（空回复/格式违规拦截） | ✅ 已接线。`assemble.go:303-307` OutputGuardChain（PII + 代码注入 + 长度） |
| 14 | 窗口溢出时切小模型 / 降级重试 | ❌ 无。溢出中间件 `llm/middleware_overflow.go` 已实现但未接入 |
| 15 | 并行工具执行（ExecutionModeParallel 已定义） | ✅ 已接线。`assemble.go:386` 默认并行 |
| 16 | 工作区沙箱 / 路径白名单 | 🟡 部分。`BashSandbox` 已接线，但 FileMutationQueue 未接入（仅 MutationWrapper） |

### 3.3 生态类

| # | 能力 | 现状 |
|---|---|---|
| 17 | 插件热加载 | ❌ PluginLoader 仍为 stub（`errPluginsUnsupported`） |
| 18 | 扩展规范被运行时驱动 | ❌ ExtensionRegistry/Manager 从未被 `assemble.go` 调用 |
| 19 | ACP 协议互通 | ❌ `internal/acp` 完全孤立，零外部生产引用 |
| 20 | FileMutationQueue 完整接入 | ❌ `DefaultFileMutationQueue` 已实现，仅 `MutationWrapper` 被使用 |
| 21 | DeferredToolRegistry 懒加载 | ❌ 已实现，生产路径不使用 |

---

## 4. 正面评价（不应被忽视）

- **工程品位高**：分层清晰（core/tools/llm/session/…），接口抽象漂亮；`Registry` 的 DIP 设计、`MiddlewareChain` 洋葱模型、`TurnRunner` 生命周期管理、`EventStream` 背压与竞态防护（`SetResult` 幂等、send-after-close 容忍）都是教科书级设计。
- **集成完成度大幅提升**：自初次审计以来，`assemble.go` 已将审批、生产化治理、SubAgent、压缩回写、会话持久化、6 层中间件链全部接线。`AssembleAgent` 作为统一组装入口消除了 interactive/prompt 命令的组装重复，确保两者接线一致。
- **代码质量扎实**：实现 2.8 万行 + 测试 4.8 万行（约 1.7:1）；race 测试、错误链 `%w`、goroutine 生命周期可控、`limitedWriter`/JSONL 行长上限等防御细节到位。e2e 测试覆盖 31 个场景文件，含 wiring 专项验证。
- **Tracing 是真实可用的亮点**：`cli.invocation` → `command.dispatch` → `loop.run` → `tool.call` 全链路 span；JSONL/OTLP/Kafka 三种 exporter + Async 排空；`cli.invocation` 退出码/状态属性完整。`assemble.go:191-200` 从 config 动态构建 exporter。同类项目中少见。
- **零第三方依赖实现 Gomes/OpenAI/Claude/Gemini 原生 HTTP 客户端** —— 务实且可靠。
- **Mock 基建成熟**：mock LLM server / conversation template / tool server / MCP server / trace exporter —— 测试纵深超过许多商业 CLI Agent。
- **Makefile 管线完整**：`make build / test / check / verify / scan`，CI 前置检查齐全。
- **生产化治理完整接入**：Retry（指数退避）+ CircuitBreaker（连续失败保护）+ IdempotentCache（工具调用幂等）+ AuditLog（JSONL 审计）+ Telemetry（指标采集）+ CostTracker（成本统计）形成完整的可观测与韧性闭环。
- **动态系统提示词**：`SystemPromptBuilder` 支持从 AGENTS.md/CLAUDE.md 加载项目上下文、注入已注册 Skills、自定义/追加提示词，使 Agent 能适应不同项目环境。

---

## 5. 优先修复路线（从"能进生产"到"成熟生态"的进阶路线）

按对客户价值的排序：

1. **FileMutationQueue 完整接线**：在 `assemble.go` 中用 `tools.WithMutationQueue(NewDefaultFileMutationQueue(), ...)` 替换或补充 `MutationWrapper`，使写/编辑操作经过 per-file worker 串行化队列，获得异步排队、symlink 归并、结果 channel 等高级能力。
2. **ACP 协议集成**：将 `internal/acp` 的 gRPC/stdio 适配器接入运行时，使 go-cli 能作为 ACP 客户端/服务端与其他 Agent 运行时互通，打开多 Agent 协作生态。
3. **PluginLoader + Extension 系统**：实现插件热加载（替换 `errPluginsUnsupported` stub），将 `internal/extension` 的 Manager/Registry 接入 `assemble.go`，使第三方扩展能通过规范接口注入工具/中间件/模型。
4. **Steer 全集成**：将中断/转向（interrupt/steer）能力与中间件链深度集成，支持运行时动态修改 Agent 行为（已有 `interrupt.go` 基础，需与 HookChain 联动）。
5. **多智能体编排**：在 SubAgent 真实执行的基础上，实现多 Agent 并行编排与结果聚合（复用 SubAgentFactory + EventStream），落地 AGENTS.md 的"三角色"研发范式。
6. **Git 深度集成**：扩展 Git 工具至分支管理、冲突解决、PR 创建，与 Session 分支树联动实现问答级回滚。
7. **LSP 集成**：接入语言服务器协议，为 read/edit/grep 工具提供语义级代码理解能力（符号跳转、引用查找、类型信息）。
8. **窗口溢出降级**：将 `llm/middleware_overflow.go` 接入模型中间件链，实现上下文溢出时自动切小模型或降级重试。

---

## 附：审计证据（关键文件:行号）

| 结论 | 证据 |
|---|---|
| SubAgent 已真实接线 | `assemble.go:309-317` `NewRealSubAgentFactory` + `RegisterSubAgentFactory` |
| SubAgent 工具已注册 | `assemble.go:315` `tr.Register(ctx, core.NewSubagentTool(dispatcher))` |
| Approval 完整接入 | `assemble.go:229-244` ApprovalMiddleware + Classifier + Store + Callback + Cache + ModeResolver |
| 审批门包裹工具调用 | `assemble.go:244` `NewMiddlewareToolRegistry(tr, approvalMW.WrapToolCall, ...)` |
| 生产化治理全接入 | `assemble.go:248-299` Retry + CircuitBreaker + IdempotentCache + AuditLog + Telemetry |
| 输出守卫接入 | `assemble.go:303-307` `OutputGuardChain`（PII + 代码注入 + 长度） |
| 6 层中间件链 | `assemble.go:445-452` `MiddlewareChain(Logging, LoopDetector, PlanMode, Reminder, FailureSynthesis, Hook).Wrap(loop)` |
| 压缩回写 agent history | `agent.go:113-126` `a.history = compacted` |
| 压缩钩子已注入 | `assemble.go:503` `WithCompactionHook(newCompactionHook(...))` |
| Session 持久化 | `assemble.go:470-478` `JSONLSessionStore` 创建 + Open |
| Session 恢复 | `assemble.go:493-499` `loadSessionHistory` → `WithHistory` |
| Session 每轮写入 | `interactive.go:321-342` `SessionStore.Append` + `Save` |
| Registry 已实例化 | `assemble.go:166` `core.NewRegistry().(*core.DefaultRegistry)` |
| Registry 注册子系统 | `assemble.go:179,242-243,245,300,467-468` 多个 RegisterXxx 调用 |
| 并行工具执行 | `assemble.go:386` `WithExecutionMode(core.ExecutionModeParallel)` |
| 动态系统提示词 | `assemble.go:397-419` `SystemPromptBuilder` + 项目上下文 + Skills |
| 环路检测 | `assemble.go:424-432` `LoopDetector` + 中间件链注入 SystemReminder |
| FileMutationQueue 未接线 | `assemble.go:244` 仅用 `NewMutationWrapper()`，未用 `WithMutationQueue` |
| DeferredToolRegistry 未使用 | `assemble.go` 不引用 `tools.DeferredToolRegistry` |
| ACP 完全孤立 | `grep -rln "internal/acp"` 非测试仅 AGENTS.md（文档） |
| PluginLoader 仍 stub | `internal/core/stubs.go` `errPluginsUnsupported` |
| Extension 无宿主驱动 | `assemble.go` 不 import `internal/extension` |
