# go-cli 集成审计报告（Integration Audit）

> 审计日期：2025-08（基于当前 main 分支代码）
> 审计方式：以 `interactive` 交互入口为起点，全链路静态跟踪运行时接线
> 审计目的：客观回答 —— 各功能模块是否真正"有机组合"并服务于客户，包括 SubAgent 等子系统的真实使用情况；对照成熟 CLI Agent 的差距清单。

---

## 0. 结论摘要

本项目是"**设计完成度极高、集成完成度极低**"的工程样板：15+ 子系统各自实现完整（甚至过度完整），但以 interactive 为入口的用户路径只串起了约 1/3 —— 核心 ReAct Loop + 内建工具 + MCP/Skill + TUI + Tracing 是真实可用的；而 **SubAgent、审批、会话持久化、生产化治理（熔断/重试/守卫）、真正生效的压缩、扩展机制全部是"陈列品"** —— 它们有完整实现，但从未接入运行时。

以客户视角，当前可用能力 ≈ 一个**无审批、无会话、无子任务、无重试、上下文无上限**的"单次对话 + 工具执行"原型。

---

## 1. 入口链路与运行时实际接线图

**入口**（`cmd/cli/main.go` → `internal/cli`）：

```
main() → config.Load() → newTracing() → cli.Run()
  → interactive.submit → buildModel(LLM) → registerMCP/Skill 工具
  → core.NewLoopAgent → core.NewAgentImpl → core.NewHarnessImpl
  → TUI 渲染（BubbleteaApp + BridgeEvents）
  → autoCompact（仅统计，见 §2.5） → 下一轮
```

**该链路上真正"有机组合"的模块**（✅ 经代码验证接线）：

| 模块 | 状态 | 证据 |
|---|---|---|
| Config 五层合并加载（Default<File<Env<Flag<Override） | ✅ 真接线 | `main.go` 调用 `config.Load`，Loader 完整实现 |
| Tracing（span + JSONL/OTLP/Kafka exporter + Async + slog） | ✅ 真接线 | `main.go` 建 tracer，interactive/prompt/loop/tool/mcp/skill 全链路打 span |
| LLM（Eino/OpenAI 兼容 HTTP + Native 三厂商原生 HTTP 客户端） | ✅ 真接线 | `buildModel` → `model.Stream` 流式调用 |
| ReAct Loop（think→act→observe，max-iterations 防护） | ✅ 真接线 | `internal/core/loop.go` |
| 内建工具（bash/read/write/edit/grep/find/ls/search） | ✅ 真接线 | `tools.RegisterDefaults` 注册，loop 中 `executeTool` 执行 |
| MCP 工具（stdio + HTTP/SSE 适配） | ✅ 真接线 | `internal/cli/interactive.go` `registerMCPTools` |
| Skill 技能（YAML frontmatter 加载 + Adapter） | ✅ 真接线 | `registerSkillTools` |
| TUI（accordion 流式渲染、主题、增量打字机效果） | ✅ 真接线 | `tui.BridgeEvents` → `BubbleteaApp` |
| Mock 框架 / 测试基建（2.8 万行实现 vs 4.8 万行测试） | ✅ 使用充分 | mock LLM/Tool/MCP server、conversation template |

---

## 2. 关键发现：已实现但**从未接入运行时**的模块

> 判定方法：`grep` 非测试代码的包级 import / 函数调用 / Registry 方法调用。

### 🔴 2.1 SubAgent —— **零使用，且默认实现是模拟器**

- `internal/core/subagent.go` 的 `DefaultSubAgent` 默认 runner 为 `simulatedRunnerFactory`，**只返回 `"response-1"` 之类的假串**，不调用 LLM、不调用工具。
- `LoopAgentRun` 循环中**没有任何代码调用 SubAgent**；`GetSubAgentFactory`/`RegisterSubAgentFactory` 在运行时从未被调用。
- 源码注释自认："*The real Harness/layer wiring is intentionally stubbed... so a real harness can be dropped in later.*"
- **结论**：SubAgent 是只有 API 外壳、无真实执行能力的占位实现。**AGENTS.md 宣称的"子智能体优先 / 三角色"研发范式在本仓库代码中完全不存在。**

### 🔴 2.2 Approval（审批/安全层）—— **零接入**

- `internal/approval` 有完整实现：`ApprovalMiddleware`（deny-first）、`Classifier`、`PermissionMode`、`TrustStore`、会话/跨会话决策缓存。
- 但 `LoopAgent.executeTool` 直接 `def.Execute(ctx, call)`，**没有挂任何审批门**。
- **后果**：**Bash 工具可以不加确认地执行任意 shell 命令**。对一个声称"生产化"的 CLI Agent，这是最严重的安全缺陷。
- 配套的 `production` 层（熔断/重试/幂等/输出守卫/审计/Telemetry）同样**零接入**：LLM 调用失败直接报错，无重试、无熔断、无限流退避。

### 🔴 2.3 Session（持久化/分支/上下文重建）—— **零接入**

- `MemoryStore`/`JSONLSessionStore`/`SessionTree`/`Branch`/`ContextBuilder`/`BranchSummary` 均为真实实现，但 interactive 会话**既不保存也不恢复** —— 退出即失忆。
- config 中的 `session.id`、`session.store_path` **无任何代码消费**。

### 🟠 2.4 Compaction —— **装饰性实现，实际无效**

- `interactive.go` 调用 `autoCompact`（基于 `MidTurnCompact`，0.8 阈值），但其操作的 `turnItems` 是**与 LLM 真实上下文平行的本地数据结构**。
- 压缩后的结果只替换局部变量 `turnItems`，**从不回写 `AgentImpl.history`**（后者经 `submission.History` 才真正发给 LLM；且 `AgentImpl` 没有 `SetHistory/ReplaceHistory` 方法）。
- **后果**：发给 LLM 的上下文随对话**无限增长**，超过窗口必然溢出，compaction 仅产出漂亮的 span 日志。

### 🟠 2.5 Middleware / Hook / 扩展注册机制 —— **零应用**

- `MiddlewareChain`、`LoggingMiddleware`、`ToolMiddleware`、`ModelMiddleware`、`HookChain`、`ExtensionRegistry` 全部定义完毕，但 **LoopAgent 是裸环**，上述机制无一被包进去。
- PluginLoader 返回 `errPluginsUnsupported`（插件加载未实现）；`extension.Manager` 框架存在但无宿主驱动。

### 🟠 2.6 核心 Registry（DIP 中心）—— **从未被实例化**

- `core.NewRegistry()` 在生产路径中 **0 处调用**（仅自身文件与测试使用）。
- `interactive.go` 绕过 Registry 直接 new 具体类。
- Registry 中 15+ 个默认实现（`SessionStoreImpl`、`SafetyPolicyClassifier`、`NoopTraceExporter`、`DefaultModelProvider`…）在真实运行中全部是死代码。

### 🟠 2.7 MutationQueue / DeferredTools / ACP

- `FileMutationQueue`（写/编辑按文件串行化、symlink 归并）有完整实现，但 loop 不经过它的 `WithMutationQueue` 包装。
- `DeferredToolRegistry`（懒加载工具）有实现，interactive 不使用。
- **ACP 协议层**（gRPC/stdio 适配 + middleware）：整个包零外部引用，完全孤立。

---

## 3. 对照成熟 CLI Agent（Claude Code / OpenHands / Codex CLI）的差距清单

### 3.1 基础设施 / 交互体验类（直接影响可用性）

| # | 能力 | 现状 |
|---|---|---|
| 1 | 危险操作人工确认（bash/删改文件 y/n 提示、auto-approve 档位） | ❌ 无。`approval` 包未接线 |
| 2 | 会话恢复/续聊（`--resume`、`--continue`） | ❌ 无 |
| 3 | 会话分支与回滚问答 | ❌ 分支树是 dead code |
| 4 | 斜杠命令（`/help` `/compact` `/undo` `/model` `/cost`） | ❌ interactive 只认 `exit` |
| 5 | Undo / Checkpoint | ❌ 无 |
| 6 | 写文件前 diff 预览 | ❌ 无 |
| 7 | Token 用量 / 成本统计 | ❌ 无 |
| 8 | 上下文占用可视化（已用窗口 %） | ❌ 无 |

### 3.2 Agent 能力类（直接影响任务质量）

| # | 能力 | 现状 |
|---|---|---|
| 9 | SubAgent 真实执行（委派子任务） | ❌ 模拟器占位，AGENTS.md 核心理念落空 |
| 10 | 压缩真正作用于请求上下文 | ❌ 压缩结果不回写 agent history |
| 11 | LLM 失败重试 / 指数退避 | ❌ production.Retry 未接线 |
| 12 | 熔断器（连续失败保护） | ❌ production.CircuitBreaker 未接线 |
| 13 | 输出质量守卫（空回复/格式违规拦截） | ❌ production.OutputGuard 未接线 |
| 14 | 窗口溢出时切小模型 / 降级重试 | ❌ 无 |
| 15 | 并行工具执行（ExecutionModeParallel 已定义） | ❌ loop 仅顺序执行 |
| 16 | 工作区沙箱 / 路径白名单 | ❌ bash 无目录限制、无权限限制 |

### 3.3 生态类

| # | 能力 | 现状 |
|---|---|---|
| 17 | 插件热加载 | ❌ PluginLoader stub（`errPluginsUnsupported`） |
| 18 | 扩展规范被运行时驱动 | ❌ ExtensionRegistry 从未被宿主调用 |
| 19 | ACP 协议互通 | ❌ 完全未接线 |

---

## 4. 正面评价（不应被忽视）

- **工程品位高**：分层清晰（core/tools/llm/session/…），接口抽象漂亮；`Registry` 的 DIP 设计、`MiddlewareChain` 洋葱模型、`TurnRunner` 生命周期管理、`EventStream` 背压与竞态防护（`SetResult` 幂等、send-after-close 容忍）都是教科书级设计。
- **代码质量扎实**：实现 2.8 万行 + 测试 4.8 万行（约 1.7:1）；race 测试、错误链 `%w`、goroutine 生命周期可控、`limitedWriter`/JSONL 行长上限等防御细节到位。
- **Tracing 是真实可用的亮点**：`cli.invocation` → `command.dispatch` → `loop.run` → `tool.call` 全链路 span；JSONL/OTLP/Kafka 三种 exporter + Async 排空；`cli.invocation` 退出码/状态属性完整。同类项目中少见。
- **零第三方依赖实现 Gomes/OpenAI/Claude/Gemini 原生 HTTP 客户端** —— 务实且可靠。
- **Mock 基建成熟**：mock LLM server / conversation template / tool server / MCP server / trace exporter —— 测试纵深超过许多商业 CLI Agent。
- **Makefile 管线完整**：`make build / test / check / verify / scan`，CI 前置检查齐全。

---

## 5. 优先修复路线（把"能跑 demo"变成"能进生产"的及格线）

按对客户价值的排序：

1. **审批提醒循环**（补齐安全底线）：把 `approval.ApprovalMiddleware` 挂进 `LoopAgent.executeTool`；bash/写删文件默认 RequireApproval；提供 `--yes`/`auto-approve` 配置。
2. **SubAgent 真实执行**：把 `simulatedSubAgentRunner` 替换为真实 `AgentLoop` 实例（复用现有工厂/注册表），并让 Loop 在收到委派型任务时生成子代理（AGENTS.md 理念落地）。
3. **Session 持久化 + 恢复**：interactive 每轮写 `JSONLSessionStore`，启动支持 `--session`/`--resume`，把 `AgentImpl.history` 与存储打通。
4. **Compaction 真正生效**：给 `AgentImpl` 增加 `SetHistory`，compaction 结果回写 agent history 后再进入下一轮 LLM 调用。
5. **生产化接线**：LLM 调用包 `Retry + CircuitBreaker + OutputGuard`；工具执行包 `MutationQueue`。
6. **UX 收敛**：斜杠命令系统（`/help` `/compact` `/undo` `/model` `/cost`）、token 统计、diff 预览（可参考 `production` 的审计/输出守卫增量做）。

---

## 附:审计证据（关键文件:行号）

| 结论 | 证据 |
|---|---|
| SubAgent 默认模拟器 | `internal/core/subagent.go:378` `simulatedRunnerFactory` |
| SubAgent 无调用方 | `grep -rn "GetSubAgentFactory\|SubAgentFactory" --include=*.go`（非测试仅自包） |
| Approval 未接线 | `internal/core/loop.go` `executeTool` 直接 `def.Execute` |
| approval/production 包零外部引用 | `grep -rln "internal/approval\|internal/production"`（非测试仅自身/mock） |
| Compaction 不回写 | `internal/cli/interactive.go:189,263`；`internal/core/agent.go` 无 `SetHistory` |
| Registry 未被实例化 | `grep -rn "core.NewRegistry()"` 非测试路径 0 命中 |
| Session 零接入 | `grep -rln "internal/session"`（非测试仅 mock_branch_summary.go） |
| PluginLoader stub | `internal/core/stubs.go` `errPluginsUnsupported` |