# go-cli 关键发现与差距清单

> 摘自 `docs/integration-audit.md` 的 §2 与 §3，单独成篇便于跟踪整改。
> 判定方法：`grep` 非测试代码的包级 import / 函数调用 / Registry 方法调用。

---

## 一、关键发现：已实现但从未接入运行时的模块

### 🔴 1. SubAgent —— 零使用，且默认实现是模拟器

- `internal/core/subagent.go` 的 `DefaultSubAgent` 默认 runner 为 `simulatedRunnerFactory`，**只返回 `"response-1"` 之类的假串**，不调用 LLM、不调用工具。
- `LoopAgent.Run` 循环中**没有任何代码调用 SubAgent**；`GetSubAgentFactory` / `RegisterSubAgentFactory` 在运行时从未被调用。
- 源码注释自认：*"The real Harness/layer wiring is intentionally stubbed... so a real harness can be dropped in later."*
- **结论**：SubAgent 是只有 API 外壳、无真实执行能力的占位实现。**AGENTS.md 宣称的"子智能体优先 / 三角色"研发范式在本仓库代码中完全不存在。**

### 🔴 2. Approval（审批 / 安全层）—— 零接入

- `internal/approval` 有完整实现：`ApprovalMiddleware`（deny-first）、`Classifier`、`PermissionMode`、`TrustStore`、会话 / 跨会话决策缓存。
- 但 `LoopAgent.executeTool` 直接 `def.Execute(ctx, call)`，**没有挂任何审批门**。
- **后果**：**Bash 工具可以不加确认地执行任意 shell 命令**。对一个声称"生产化"的 CLI Agent，这是最严重的安全缺陷。
- 配套的 `production` 层（熔断 / 重试 / 幂等 / 输出守卫 / 审计 / Telemetry）同样**零接入**：LLM 调用失败直接报错，无重试、无熔断、无限流退避。

### 🔴 3. Session（持久化 / 分支 / 上下文重建）—— 零接入

- `MemoryStore` / `JSONLSessionStore` / `SessionTree` / `Branch` / `ContextBuilder` / `BranchSummary` 均为真实实现，但 interactive 会话**既不保存也不恢复** —— 退出即失忆。
- config 中的 `session.id`、`session.store_path` **无任何代码消费**。

### 🟠 4. Compaction —— 装饰性实现，实际无效

- `interactive.go` 调用 `autoCompact`（基于 `MidTurnCompact`，0.8 阈值），但其操作的 `turnItems` 是**与 LLM 真实上下文平行的本地数据结构**。
- 压缩后的结果只替换局部变量 `turnItems`，**从不回写 `AgentImpl.history`**（后者经 `submission.History` 才真正发给 LLM；且 `AgentImpl` 没有 `SetHistory / ReplaceHistory` 方法）。
- **后果**：发给 LLM 的上下文随对话**无限增长**，超过窗口必然溢出，compaction 仅产出漂亮的 span 日志。

### 🟠 5. Middleware / Hook / 扩展注册机制 —— 零应用

- `MiddlewareChain`、`LoggingMiddleware`、`ToolMiddleware`、`ModelMiddleware`、`HookChain`、`ExtensionRegistry` 全部定义完毕，但 **LoopAgent 是裸环**，上述机制无一被包进去。
- PluginLoader 返回 `errPluginsUnsupported`（插件加载未实现）；`extension.Manager` 框架存在但无宿主驱动。

### 🟠 6. 核心 Registry（DIP 中心）—— 从未被实例化

- `core.NewRegistry()` 在生产路径中 **0 处调用**（仅自身文件与测试使用）。
- `interactive.go` 绕过 Registry 直接 new 具体类。
- Registry 中 15+ 个默认实现（`SessionStoreImpl`、`SafetyPolicyClassifier`、`NoopTraceExporter`、`DefaultModelProvider`…）在真实运行中全部是死代码。

### 🟠 7. MutationQueue / DeferredTools / ACP

- `FileMutationQueue`（写 / 编辑按文件串行化、symlink 归并）有完整实现，但 loop 不经过它的 `WithMutationQueue` 包装。
- `DeferredToolRegistry`（懒加载工具）有实现，interactive 不使用。
- **ACP 协议层**（gRPC / stdio 适配 + middleware）：整个包零外部引用，完全孤立。

---

## 二、差距清单（对照 Claude Code / OpenHands / Codex CLI）

### 2.1 基础设施 / 交互体验类（直接影响可用性）

| # | 能力 | 现状 |
|---|---|---|
| 1 | 危险操作人工确认（bash / 删改文件 y/n 提示、auto-approve 档位） | ❌ 无。`approval` 包未接线 |
| 2 | 会话恢复 / 续聊（`--resume`、`--continue`） | ❌ 无 |
| 3 | 会话分支与回滚问答 | ❌ 分支树是 dead code |
| 4 | 斜杠命令（`/help` `/compact` `/undo` `/model` `/cost`） | ❌ interactive 只认 `exit` |
| 5 | Undo / Checkpoint | ❌ 无 |
| 6 | 写文件前 diff 预览 | ❌ 无 |
| 7 | Token 用量 / 成本统计 | ❌ 无 |
| 8 | 上下文占用可视化（已用窗口 %） | ❌ 无 |

### 2.2 Agent 能力类（直接影响任务质量）

| # | 能力 | 现状 |
|---|---|---|
| 9 | SubAgent 真实执行（委派子任务） | ❌ 模拟器占位，AGENTS.md 核心理念落空 |
| 10 | 压缩真正作用于请求上下文 | ❌ 压缩结果不回写 agent history |
| 11 | LLM 失败重试 / 指数退避 | ❌ production.Retry 未接线 |
| 12 | 熔断器（连续失败保护） | ❌ production.CircuitBreaker 未接线 |
| 13 | 输出质量守卫（空回复 / 格式违规拦截） | ❌ production.OutputGuard 未接线 |
| 14 | 窗口溢出时切小模型 / 降级重试 | ❌ 无 |
| 15 | 并行工具执行（ExecutionModeParallel 已定义） | ❌ loop 仅顺序执行 |
| 16 | 工作区沙箱 / 路径白名单 | ❌ bash 无目录限制、无权限限制 |

### 2.3 生态类

| # | 能力 | 现状 |
|---|---|---|
| 17 | 插件热加载 | ❌ PluginLoader stub（`errPluginsUnsupported`） |
| 18 | 扩展规范被运行时驱动 | ❌ ExtensionRegistry 从未被宿主调用 |
| 19 | ACP 协议互通 | ❌ 完全未接线 |

---

## 附：审计证据（关键文件 : 行号）

| 结论 | 证据 |
|---|---|
| SubAgent 默认模拟器 | `internal/core/subagent.go:378` `simulatedRunnerFactory` |
| SubAgent 无调用方 | `grep -rn "GetSubAgentFactory\|SubAgentFactory" --include=*.go`（非测试仅自包） |
| Approval 未接线 | `internal/core/loop.go` `executeTool` 直接 `def.Execute` |
| approval / production 包零外部引用 | `grep -rln "internal/approval\|internal/production"`（非测试仅自身 / mock） |
| Compaction 不回写 | `internal/cli/interactive.go:189,263`；`internal/core/agent.go` 无 `SetHistory` |
| Registry 未被实例化 | `grep -rn "core.NewRegistry()"` 非测试路径 0 命中 |
| Session 零接入 | `grep -rln "internal/session"`（非测试仅 mock_branch_summary.go） |
| PluginLoader stub | `internal/core/stubs.go` `errPluginsUnsupported` |
