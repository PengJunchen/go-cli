# go-cli 集成审计报告（Integration Audit）

> 初次审计日期：2026-08（基于早期 main 分支代码）
> 复审计日期：2026-08-07（基于当前 main 分支代码，全链路重新静态跟踪）
> 审计方式：以 `interactive` 交互入口为起点，沿 `assemble.go` 全链路静态跟踪运行时接线
> 审计目的：客观回答 —— 各功能模块是否真正"有机组合"并服务于客户，包括 SubAgent 等子系统的真实使用情况；对照成熟 CLI Agent 的差距清单。

> **复审说明（2026-08-07）**：本项目自初次审计以来已显著改善。初次审计标记的全部关键缺口（SubAgent、审批、会话持久化、压缩回写、中间件链、Registry、ACP、FileMutationQueue、PluginLoader、Extension、BashSandbox、会话分支、Undo/Checkpoint）均已修复并接入运行时。Phase 24-27（M27-M30）也已全部完成：跨会话记忆（FileMemoryStore+TF-IDF）、6级ThinkingLevel+ModelCycler、TUI Markdown两阶段AST+10语言高亮、StreamingBash实时流。源码复审新发现：7层模型中间件链（Failover->Retry->Timeout->Sanitize->LoopDetection->Validate->Overflow）已在 assemble.go:479-487 通过 NewStandardMiddlewareChain 构造，经 newModelWrapperWithChain 接线运行时（loop.go:243），F-020 从 GAP-ENHANCE 修正为 ALIGNED。Tracing 三级 RedactingExporter 脱敏已实现（exporter.go:100-149, assemble.go:272-276），F-048 修正为 ALIGNED。DeferredToolRegistry 已用于 MCP/Skill 懒加载（assemble.go:285-286）。新发现运行时缺口：LLMMemoryExtractor 在 assemble.go:694 构造但运行时无 Extract 调用，自动事实提取链路断开。

---

## 0. 结论摘要

本项目已从初版"**设计完成度极高、集成完成度极低**"的工程样板，演进为**设计完成度高、集成完成度显著提升**的可生产化 CLI Agent。

当前运行时已有机组合的能力：核心 ReAct Loop + 内建工具 + MCP/Skill 工具 + 审批门（deny-first）+ 生产化治理（重试/熔断/幂等缓存/审计/Telemetry）+ 输出守卫 + 真实 SubAgent + 6 层中间件链（日志/环路检测/计划模式/系统提醒/失败合成/Hook）+ 压缩回写 agent history + 会话持久化与恢复 + TUI 流式渲染 + 全链路 Tracing + 并行工具执行。

以客户视角，当前可用能力 ≈ 一个**带审批、带会话恢复、带子任务委派、带重试熔断、上下文有压缩上限**的"多轮对话 + 工具执行"Agent，已具备进入生产的基本条件。

**剩余缺口**：LLMMemoryExtractor 死接线（assemble.go:694 构造但运行时无 Extract 调用，自动事实提取链路断开）；TUI 未暴露实时窗口占用百分比。架构级缺口：无 OS 级沙箱、无 OAuth/PKCE 认证流程、无完整 Hooks 生命周期系统、无多模态支持、模型 Provider 较少（4 个 vs pi 30+）、无 IDE 集成协议。

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
    → MutationQueueWrapper（FileMutationQueue 完整队列串行化）  // assemble.go:359-363
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
| BashSandbox（路径白名单 + CPU/Memory 限制） | ✅ 真接线 | `assemble.go:222-233` `NewDefaultBashSandbox` + `:248` `WithRegisteredBashSandbox` |
| FileMutationQueue（per-file worker 串行化） | ✅ 真接线 | `assemble.go:359-363` `NewDefaultFileMutationQueue` + `NewMutationQueueWrapper` |
| ACP 多 Agent 通信（stdio/gRPC，配置驱动） | ✅ 真接线 | `assemble.go:448-473` ACPClient + ACPMiddlewareAdapter；`:604-606` 中间件链 |
| Extension 系统（Plugin Load→Init→Shutdown） | ✅ 真接线 | `assemble.go:318-341` PluginManager + Tools/Hooks/Middleware 桥接 |
| 会话分支树（/session tree\|fork\|resume） | ✅ 真接线 | `interactive.go:135-141` SessionTreeBuilder + `slash_handlers.go:155-174` |
| Undo / Checkpoint（/undo 恢复文件检查点） | ✅ 真接线 | `slash_handlers.go:192-218` UndoHandler + FileTracker.Restore |
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
- 工具注册表通过 `tools.NewMiddlewareToolRegistry(tr, approvalMW.WrapToolCall, tools.NewMutationQueueWrapper(mutationQueue))` 包装，**所有工具调用都经过审批门 + 变更队列串行化**。
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

### ✅ 2.7 MutationQueue / ACP / Plugin / Extension / BashSandbox —— 已全部接入（2026-08-06 源码复审）

- **FileMutationQueue**：`assemble.go:359-363` 使用 `tools.NewDefaultFileMutationQueue(...)` 创建完整队列实例（per-file worker goroutine、symlink 归并、异步排队），并通过 `tools.NewMutationQueueWrapper(mutationQueue)` 包装工具注册表。`assemble.go:368-374` cleanup 闭包调用 `cq.Close()` 刷新待处理变更并释放 worker goroutine。
- **ACP 协议层**：`assemble.go:448-473` 当 `rc.ACP.Transport` 和 `rc.ACP.Endpoints` 非空时，创建 ACPClient（stdio 或 gRPC），连接后构建 `ACPMiddlewareAdapter`。`assemble.go:604-606` 将适配器追加到中间件链最内层，路由入站 ACP 消息到 SubagentDispatcher。
- **PluginLoader**：`assemble.go:321` 使用 `extension.NewDefaultPluginLoader()`，对应 `extension/plugin.go`（build tag `!no_plugin`）为默认编译的真实实现，支持 Go .so 插件和 JSON-over-HTTP 端点。`extension/plugin_stub.go`（build tag `no_plugin`）仅在显式 `-tags no_plugin` 时生效。
- **Extension 系统**：`assemble.go:318-341` 当 `rc.Extensions.Enabled` 且 `PluginPaths` 非空时，创建 PluginManager，执行 Load → Init 生命周期，将扩展提供的 Tools / Hooks / Middleware 桥接进运行时。扩展 Hooks 通过 `newExtensionHookAdapter` 转为 `core.Hook` 注入 HookChain；扩展 Middleware 通过 `newExtensionMiddlewareAdapter` 追加到中间件链。cleanup 闭包调用 `pm.Shutdown` 管理完整生命周期。
- **BashSandbox**：`assemble.go:222-233` 从 config 构建 `tools.NewDefaultBashSandbox(sandboxOpts...)`，支持 `WithAllowedPaths`（默认 cwd 白名单）+ `ResourceLimits`（MaxCPU/MaxMemory）。`assemble.go:248` 通过 `WithRegisteredBashSandbox(bashSandbox)` 注入 bash 工具。
- **DeferredToolRegistry**：`assemble.go:285-286` 使用 `tools.NewDeferredToolRegistryAdapter(underlyingReg)` 创建懒加载适配器。MCP 工具（assemble.go:972）和 Skill 工具（assemble.go:1064）通过 `RegisterDeferred` 注册，在首次调用时才实例化，减少启动开销。

---

## 3. 对照成熟 CLI Agent（Claude Code / OpenHands / Codex CLI）的差距清单

### 3.1 基础设施 / 交互体验类（直接影响可用性）

| # | 能力 | 现状 |
|---|---|---|
| 1 | 危险操作人工确认（bash/删改文件 y/n 提示、auto-approve 档位） | ✅ 已接线。`assemble.go:229-244` ApprovalMiddleware 完整接入 |
| 2 | 会话恢复/续聊（`--resume`、`--continue`） | ✅ 已接线。`assemble.go:493-499` + `interactive.go:321-342` |
| 3 | 会话分支与回滚问答 | ✅ 已接线。`interactive.go:135-141` SessionTreeBuilder + SessionSlashHandler；`slash_handlers.go:155-174` `/session tree\|fork\|resume` |
| 4 | 斜杠命令（`/help` `/compact` `/undo` `/model` `/cost`） | ✅ 已接线。`internal/cli/slash.go` + `slash_handlers.go` + `slash_registry.go` |
| 5 | Undo / Checkpoint | ✅ 已接线。`slash_handlers.go:192-218` UndoHandler + FileTracker.Restore，`/undo` 恢复最近文件检查点 |
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
| 14 | 窗口溢出时切小模型 / 降级重试 | ✅ 已接线。7层模型中间件链在 assemble.go:479-487 构造（含 OverflowRecoveryMiddleware），经 newModelWrapperWithChain 接线运行时。Overflow 中间件检测 context_length_exceeded 后裁剪旧消息重试（最多 2 次，裁剪 30%） |
| 15 | 并行工具执行（ExecutionModeParallel 已定义） | ✅ 已接线。`assemble.go:386` 默认并行 |
| 16 | 工作区沙箱 / 路径白名单 | ✅ 已接线。`assemble.go:222-233` `NewDefaultBashSandbox` + `WithAllowedPaths`（默认 cwd）+ `ResourceLimits`（CPU/Memory）；`:248` `WithRegisteredBashSandbox` |

### 3.3 生态类

| # | 能力 | 现状 |
|---|---|---|
| 17 | 插件热加载 | ✅ 已接线。`assemble.go:321` `NewDefaultPluginLoader()` 为真实实现（`extension/plugin.go`，build tag `!no_plugin`） |
| 18 | 扩展规范被运行时驱动 | ✅ 已接线。`assemble.go:318-341` PluginManager Load → Init → Shutdown，Tools/Hooks/Middleware 桥接进运行时 |
| 19 | ACP 协议互通 | ✅ 已接线（配置驱动）。`assemble.go:448-473` ACPClient（stdio/gRPC）+ ACPMiddlewareAdapter；`:604-606` 追加到中间件链 |
| 20 | FileMutationQueue 完整接入 | ✅ 已接线。`assemble.go:359-363` `NewDefaultFileMutationQueue(...)` + `NewMutationQueueWrapper(mutationQueue)` |
| 21 | DeferredToolRegistry 懒加载 | ✅ 已接线。`assemble.go:285-286` `NewDeferredToolRegistryAdapter` 创建 dtr，MCP 工具（assemble.go:972）和 Skill 工具（assemble.go:1064）通过 `RegisterDeferred` 懒加载 |

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

1. **LLMMemoryExtractor 接线**：在会话结束或轮次结束时调用 `assembly.MemoryExtractor.Extract()`，将提取的事实写入 FileMemoryStore，打通自动事实提取链路。当前 assemble.go:694 构造了 extractor 但运行时无调用点。
2. **TUI 实时窗口占比**：在 `/cost` 或 TUI 状态栏中显示当前上下文窗口占用百分比（需配合 maxTokens 计算）。
3. **多模态支持**：Message.Content 当前为 string，无法携带图片。需扩展为支持 image content，以接入视觉模型（2026 年 CLI Agent 标配）。
4. **完整 Hooks 生命周期**：当前仅 BeforeToolCall/AfterToolCall，需扩展为完整的事件观察者（EventObserver）+ 生命周期 Hooks。
5. **OS 级沙箱**：当前仅应用层 BashSandbox（路径白名单+命令黑名单+ulimit），需增加 OS 级隔离（如 Linux namespace/seccomp）。
6. **MCP Resource/Prompt 支持**：当前 MCP 仅覆盖 Tools，需补齐 Resource 和 Prompt 能力。
7. **Session Tree 导航命令**：会话树数据结构已实现，需在 CLI 暴露 /tree/fork/clone/resume/export 导航命令。

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
| FileMutationQueue 已完整接入 | `assemble.go:359-363` `NewDefaultFileMutationQueue(...)` + `NewMutationQueueWrapper(mutationQueue)`；`:368-374` cleanup Close |
| DeferredToolRegistry 已接线 | `assemble.go:285-286` `NewDeferredToolRegistryAdapter(underlyingReg)`；MCP assemble.go:972 + Skill assemble.go:1064 通过 `RegisterDeferred` 懒加载 |
| 7层模型中间件链已接线 | `assemble.go:479-487` `NewStandardMiddlewareChain(Failover, Retry, Timeout, Sanitize, LoopDetection, Validate, Overflow)`；`assemble.go:548,640` 经 `newModelWrapperWithChain` 注入；`output_guard_adapter.go:186-214` 组合 pw+chain+breaker+guard+telemetry；`loop.go:243-249` 运行时执行 |
| Tracing 脱敏已实现 | `exporter.go:100-149` RedactingExporter 三级（full/redact/off）；`assemble.go:272-276` 从 config 读取 RedactionLevel 包装 exporter |
| LLMMemoryExtractor 死接线 | `assemble.go:694` 构造 `NewLLMMemoryExtractor(model, memStore)` 但运行时无 `.Extract()` 调用。FileMemoryStore 完整接线（assemble.go:658-688 注入系统提示 + /memory 命令），自动事实提取链路断开 |
| ACP 已接线（配置驱动） | `assemble.go:448-473` ACPClient（stdio/gRPC）+ ACPMiddlewareAdapter；`:604-606` 追加到中间件链 |
| PluginLoader 真实实现已生效 | `extension/plugin.go`（build tag `!no_plugin`）为默认编译；`assemble.go:321` 使用 `NewDefaultPluginLoader()` |
| Extension 已有宿主驱动 | `assemble.go:318-341` PluginManager Load → Init → Shutdown；Tools/Hooks/Middleware 桥接进运行时 |
| BashSandbox 已接线 | `assemble.go:222-233` `NewDefaultBashSandbox` + `WithAllowedPaths` + `ResourceLimits`；`:248` `WithRegisteredBashSandbox` |
| 会话分支已接线 | `interactive.go:135-141` SessionTreeBuilder + SessionSlashHandler；`slash_handlers.go:155-174` `/session` |
| Undo / Checkpoint 已接线 | `slash_handlers.go:192-218` UndoHandler + FileTracker.Restore |
