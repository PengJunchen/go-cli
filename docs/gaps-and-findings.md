# go-cli 关键发现与差距清单

> 摘自 `docs/integration-audit.md` 的 §2 与 §3，单独成篇便于跟踪整改。
> 判定方法：`grep` 非测试代码的包级 import / 函数调用 / Registry 方法调用。

> **更新说明（2026-08-06）**：本文档已根据代码核实重新审计。原报告中标记为 🔴/🟠 的
> 6 项核心发现在 `internal/cli/assemble.go` 重构后已全部接入运行时（标记为 ✅）。
> 审计中还发现 production 弹性层（重试 / 熔断 / 幂等 / 输出守卫 / 审计 / Telemetry）、
> 斜杠命令、Undo / Checkpoint、Diff 预览、成本统计、会话分支等能力也已一并落地。
> 剩余 4 项仍未解决（标记为 ❌）。下方审计证据表已同步更新至最新行号。

---

## 一、关键发现

### ✅ 已修复项（原 🔴/🟠 → ✅）

#### ✅ 1. SubAgent —— 已接入真实执行栈

- `assemble.go:309-317` 调用 `core.NewRealSubAgentFactory(model, tr, ...)`，构建真实的
  LoopAgent → AgentImpl → HarnessImpl 执行栈，并通过 `core.RegisterSubAgentFactory` 注册到全局。
- `dispatch_subagent` 工具已注册到工具表（`assemble.go:315` `NewSubagentTool(dispatcher)`），
  LLM 可通过该工具委派子任务。
- `simulatedRunnerFactory` 仍作为 `subagent.go:145` 的默认 fallback 存在，但生产装配路径
  已被 `NewRealSubAgentFactory` 覆盖，不会命中。
- **结论**：SubAgent 不再是模拟器占位。AGENTS.md 宣称的"子智能体优先 / 三角色"研发范式
  在运行时已具备执行能力。

#### ✅ 2. Approval（审批 / 安全层）—— 已接入，production 弹性层同步落地

- `assemble.go:229-244` 创建 `SafetyPolicyClassifier`、`ApprovalMiddleware`（带
  `WithCallback`、`WithCache`、`WithPermissionModeResolver`），并通过
  `tools.NewMiddlewareToolRegistry(tr, approvalMW.WrapToolCall, ...)` 包装工具注册表。
- 危险操作（如 bash）现在经过审批门，支持交互式 y/n 确认与跨会话决策缓存。
- **原报告中"配套 production 层零接入"的问题也已解决**：`assemble.go:247-307` 完整接线
  production 弹性层：
  - **重试**：`ProductionModelWrapper` + `RetryPolicy`（MaxAttempts: 3，指数退避）
  - **熔断**：`DefaultCircuitBreaker`（`assemble.go:263-272`）
  - **幂等**：`FIFOIdempotentCache`（`assemble.go:275-276`）
  - **审计**：`DefaultAuditLog`（`assemble.go:281-293`，可配置路径）
  - **Telemetry**：`DefaultTelemetry`（`assemble.go:278-279`）
  - **输出守卫**：`OutputGuardChain`（PII / CodeInjection / Length，`assemble.go:302-307`）
  - **成本追踪**：`CostTracker` + `StatsRegistry`（`assemble.go:253-254`）

#### ✅ 3. Session（持久化 / 续聊 / 分支）—— 已接入

- `assemble.go:470-478`：当 `enableSession` 且配置了 `StorePath` 时创建
  `JSONLSessionStore` 并打开。
- `interactive.go:321-342`：每轮对话结束后将 user / assistant 消息持久化到 session store，
  即使中断也会保存（使用 `spanCtx` 而非可被取消的 `turnCtx`）。
- `assemble.go:492-499`：支持 `--resume`，从 session store 加载历史消息注入 agent。
- **会话分支**也已接入：`interactive.go:135` 创建 `SessionTreeBuilder` + `SessionTree`，
  斜杠命令 `/session tree|fork|resume` 可操作分支树（`slash_handlers.go:155-186`）。
- **结论**：退出不再失忆，`--resume` 可续聊，分支树不再是 dead code。

#### ✅ 4. Compaction —— 已生效，真正回写 agent history

- `agent.go:113-126`：`compactionHook` 在每轮结束后被调用，压缩结果**直接回写**
  `a.history`（`agent.go:124`：`a.history = compacted`）。
- `agent.go:160-168`：`Compact()` 方法（供 `/compact` 斜杠命令调用）同样回写 history。
- `assemble.go:503`：通过 `WithCompactionHook(newCompactionHook(compactor, estimator, ac.maxTokens))`
  将压缩钩子注入 AgentImpl。
- **结论**：发给 LLM 的上下文不再无限增长，compaction 不再仅产出日志。

#### ✅ 5. Middleware / Hook —— 已接入，6 层中间件链

- `assemble.go:445-452`：用 `core.NewMiddlewareChain(...)` 包装 loop，形成 6 层链：
  `LoggingMiddleware` → `LoopDetectorMiddleware` → `PlanModeMiddleware` →
  `SystemReminderInjector` → `FailureSynthesisMiddleware` → `HookMiddleware`。
- HookMiddleware 接入 `hookChain`，扩展钩子在 pre/post-turn 生效。
- **结论**：LoopAgent 不再是裸环，中间件 / 钩子机制已全部包入执行路径。

#### ✅ 6. 核心 Registry（DIP 中心）—— 已实例化并贯穿装配

- `assemble.go:166`：`core.NewRegistry().(*core.DefaultRegistry)` 创建 Registry 实例。
- 装配过程中持续注册组件：`RegisterModelProvider`（`:178`）、`RegisterApprovalClassifier`
  （`:242`）、`RegisterApprovalStore`（`:243`）、`RegisterToolRegistry`（`:245,300`）、
  `RegisterCompactor`（`:467`）、`RegisterTokenEstimator`（`:468`）等。
- **结论**：Registry 不再是死代码，15+ 个默认实现通过注册机制接入运行时。

### ❌ 仍未解决项

#### ❌ 7. ACP 协议层 —— 完全孤立的死代码

- 整个 `internal/acp/` 包（gRPC / stdio 适配 + middleware）**零外部引用**，
  在 `assemble.go` 及所有生产路径中从未被 import 或调用。
- **结论**：ACP 协议互通能力完全未接线，是纯死代码。

#### ❌ 8. FileMutationQueue —— 仅打标，未串行化

- `tools/mutation.go` 有完整的 `FileMutationQueue` 实现（写 / 编辑按文件串行化、
  symlink 归并），但 `assemble.go:244` 只使用了 `NewMutationWrapper()`（打标），
  **未使用 `WithMutationQueue()`（串行化包装）**。
- 并行工具执行已启用，但同文件的写 / 编辑操作存在竞态风险。

#### ❌ 9. PluginLoader —— 仍返回 stub 错误

- `core/stubs.go:145-147` 的 `DefaultPluginLoader` 仍返回 `errPluginsUnsupported`。
- `extension/plugin.go` 中有 `DefaultPluginLoader` 的真实实现（Go plugin + HTTP），
  但 assemble 路径未注册该实现，运行时仍命中 stub。

#### ❌ 10. Extension 系统 —— 无宿主驱动

- `extension/manager.go` 中 `Manager` / `Registry` / `Hook` 均已定义，
  但 assemble 路径不调用 `extension.Manager`。
- 扩展生命周期（Init → running → Shutdown）无宿主驱动，ExtensionRegistry 从未被
  运行时调用。

---

## 二、差距清单（对照 Claude Code / OpenHands / Codex CLI）

> 状态说明：✅ 已接入 ｜ ⚠️ 部分接入（有残留风险） ｜ ❌ 未接入

### 2.1 基础设施 / 交互体验类（直接影响可用性）

| # | 能力 | 现状 |
|---|---|---|
| 1 | 危险操作人工确认（bash / 删改文件 y/n 提示、auto-approve 档位） | ✅ `assemble.go:229-244` ApprovalMiddleware 已接线，支持交互确认 + 缓存 + 权限模式 |
| 2 | 会话恢复 / 续聊（`--resume`、`--continue`） | ✅ `assemble.go:492-499` 支持 `--resume`；`interactive.go:321-342` 逐轮持久化 |
| 3 | 会话分支与回滚问答 | ✅ `interactive.go:135` SessionTree 已实例化；`/session tree\|fork\|resume` 可操作分支树 |
| 4 | 斜杠命令（`/help` `/compact` `/undo` `/model` `/cost` 等） | ✅ `slash.go:66-99` 注册 13 个命令：`/help` `/cost` `/compact` `/clear` `/tools` `/model` `/session` `/undo` `/diff` `/plan` `/config` `/history` `/save` `/load` |
| 5 | Undo / Checkpoint | ✅ `slash_handlers.go:192-218` UndoHandler + FileTracker，可恢复最近文件检查点 |
| 6 | 写文件前 diff 预览 | ✅ `slash_handlers.go:220-269` DiffHandler + DiffGenerator，`/diff` 展示最近文件变更 unified diff |
| 7 | Token 用量 / 成本统计 | ✅ `slash_handlers.go:43-69` CostHandler + CostTracker + StatsRegistry，`/cost` 显示总成本、调用次数、Turns、TokensIn/Out |
| 8 | 上下文占用可视化（已用窗口 %） | ⚠️ `/cost` 显示 Token 计数（TokensIn / TokensOut），但未显示"窗口占用百分比"。需配合 maxTokens 做百分比展示 |

### 2.2 Agent 能力类（直接影响任务质量）

| # | 能力 | 现状 |
|---|---|---|
| 9 | SubAgent 真实执行（委派子任务） | ✅ `assemble.go:309-317` `NewRealSubAgentFactory` 构建真实 LoopAgent→AgentImpl→HarnessImpl 栈；`dispatch_subagent` 工具已注册 |
| 10 | 压缩真正作用于请求上下文 | ✅ `agent.go:113-126` compactionHook 直接回写 `a.history`；`assemble.go:503` `WithCompactionHook` 接线 |
| 11 | LLM 失败重试 / 指数退避 | ✅ `assemble.go:248-261` RetryPolicy（MaxAttempts: 3, BaseDelay: 1s, MaxDelay: 10s）经 ProductionModelWrapper 接线 |
| 12 | 熔断器（连续失败保护） | ✅ `assemble.go:263-272` DefaultCircuitBreaker 接线，阈值 / 恢复超时可配置 |
| 13 | 输出质量守卫（空回复 / 格式违规拦截） | ✅ `assemble.go:302-307` OutputGuardChain（PII / CodeInjection / Length）接线 |
| 14 | 窗口溢出时切小模型 / 降级重试 | ❌ 无自动模型切换机制。Compaction 会压缩 history，但不会在窗口溢出时降级到更小模型 |
| 15 | 并行工具执行（ExecutionModeParallel 已定义） | ⚠️ 并行执行已启用，但 `assemble.go:244` 仅用 `NewMutationWrapper()`（打标），未用 `WithMutationQueue()`（串行化）。同文件写 / 编辑存在竞态风险 |
| 16 | 工作区沙箱 / 路径白名单 | ⚠️ Approval 中间件提供权限模式 + 确认门，但 bash 工具仍无目录限制 / 路径白名单沙箱 |

### 2.3 生态类

| # | 能力 | 现状 |
|---|---|---|
| 17 | 插件热加载 | ❌ `core/stubs.go:145-147` 仍返回 `errPluginsUnsupported`。`extension/plugin.go` 有实现但 assemble 路径未注册 |
| 18 | 扩展规范被运行时驱动 | ❌ `extension/manager.go` Manager/Registry/Hook 已定义，但 assemble 不调用 `extension.Manager`，生命周期无宿主驱动 |
| 19 | ACP 协议互通 | ❌ 整个 `internal/acp/` 包零外部引用，完全孤立死代码 |

---

## 附：审计证据（关键文件 : 行号）

| 结论 | 证据 |
|---|---|
| SubAgent 已接入真实执行栈 | `assemble.go:309-317` `NewRealSubAgentFactory` + `RegisterSubAgentFactory`；`assemble.go:315` `NewSubagentTool` 注册 `dispatch_subagent` |
| SubAgent 模拟器仅作 fallback | `core/subagent.go:145` `simulatedRunnerFactory` 保留，但被 `NewRealSubAgentFactory` 覆盖 |
| Approval 已接线 | `assemble.go:229-244` `ApprovalMiddleware`（WithCallback/WithCache/WithPermissionModeResolver）+ `NewMiddlewareToolRegistry` 包装 |
| Production 弹性层已接线 | `assemble.go:247-307` RetryPolicy + CircuitBreaker + IdempotentCache + AuditLog + Telemetry + OutputGuardChain + CostTracker |
| Session 持久化已接线 | `assemble.go:470-478` `JSONLSessionStore`；`interactive.go:321-342` 逐轮持久化；`assemble.go:492-499` `--resume` |
| 会话分支已接线 | `interactive.go:135` `SessionTreeBuilder` + `SessionTree`；`slash_handlers.go:155-186` `/session tree\|fork\|resume` |
| Compaction 回写 history | `agent.go:113-126` `a.history = compacted`（:124）；`agent.go:160` `Compact()` 回写；`assemble.go:503` `WithCompactionHook` |
| Middleware 6 层链已接线 | `assemble.go:445-452` `NewMiddlewareChain(Logging, LoopDetector, PlanMode, SystemReminder, FailureSynthesis, Hook).Wrap(loop)` |
| Registry 已实例化 | `assemble.go:166` `core.NewRegistry().(*core.DefaultRegistry)`；后续 `RegisterModelProvider`/`RegisterApprovalClassifier`/`RegisterToolRegistry`/`RegisterCompactor` 等 |
| 斜杠命令已实现 | `slash.go:66-99` 注册 13 个命令；`interactive.go:198` `handleSlashCommand` 分发 |
| Undo / Checkpoint 已实现 | `slash_handlers.go:192-218` `UndoHandler` + `FileTracker.Restore` |
| Diff 预览已实现 | `slash_handlers.go:220-269` `DiffHandler` + `DiffGenerator.Generate` |
| 成本 / Token 统计已实现 | `slash_handlers.go:43-69` `CostHandler` + `CostTracker.Total/Calls` + `StatsRegistry.GetSessionStats`（TokensIn/Out） |
| ACP 完全孤立 | `grep -rln "internal/acp"` 非测试零外部引用 |
| FileMutationQueue 仅打标 | `assemble.go:244` `NewMutationWrapper()` 而非 `WithMutationQueue()`；并行执行下同文件写 / 编辑有竞态风险 |
| PluginLoader 仍 stub | `core/stubs.go:145-147` 返回 `errPluginsUnsupported`；`extension/plugin.go` 有实现但未注册 |
| Extension 系统无宿主驱动 | `extension/manager.go` Manager/Registry/Hook 已定义；assemble 路径不调用 `extension.Manager` |
