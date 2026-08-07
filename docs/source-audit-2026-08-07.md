# go-cli 源码审计报告（2026-08-07）

> 审计方法：以 `interactive.go` 为入口，沿 `assemble.go` 全链路静态跟踪运行时接线，仅以源码为准，忽略一切文档。
> 对标基准：成熟 CLI Agent（Claude Code / Codex CLI / Aider / pi）。
> 审计范围：17+ 子系统，56 项功能映射。

---

## 0. 结论摘要

本项目是一个**基于 Go 标准库的零外部依赖 AI Agent CLI 框架**，以 `interactive` 交互命令为入口，经 15 步 `AssembleAgent` 统一组装，将 ReAct 循环、工具执行、审批门、生产化治理、子智能体、会话持久化、压缩、TUI 渲染、MCP/Skill 扩展等子系统有机组合为可生产化的 Agent 运行时。

**功能映射最终状态**：

| 状态 | 数量 | 说明 |
|------|------|------|
| ALIGNED | 44 | 源码验证：完整实现并接线运行时 |
| GAP-ENHANCE | 7 | 基础实现存在，需增强 |
| GAP-FULL | 3 | 完全缺失（Cron / MCP OAuth / 多模态） |
| GAP-PARTIAL | 2 | 部分实现，语义不完整 |
| 运行时缺口 | 1 | LLMMemoryExtractor 已构造但运行时未调用 |

---

## 1. 入口链路与运行时接线图

```
main() -> config.Load() -> newTracing() -> cli.Run()
  -> interactiveCmd.Run() -> AssembleAgent（assemble.go 统一组装）
    -> Step 1:  buildModel(LLM)                                          // assemble.go:225
    -> Step 1c: ModelCycler（round_robin/weighted/cost_priority）         // assemble.go:240-260
    -> Step 1b: Tracing（Tracer + RedactingExporter 三级脱敏）            // assemble.go:266-281
    -> Step 2:  ToolRegistry + DeferredToolRegistryAdapter                // assemble.go:284-286
    -> Step 2:  RegisterDefaults（bash/read/write/edit/grep/find/ls/...）  // assemble.go:336
    -> Step 2b: CustomTools（config 驱动自定义命令工具）                   // assemble.go:344
    -> Step 3:  registerMCPTools（Stdio/SSE + RegisterDeferred 懒加载）    // assemble.go:347
    -> Step 3b: LSP 工具（单服务器/MultiLSPClient 按扩展名路由）           // assemble.go:358-373
    -> Step 3c: RemoteBash（SSH 多主机远程执行）                           // assemble.go:376-385
    -> Step 4:  registerSkillTools（YAML frontmatter + RegisterDeferred）  // assemble.go:388
    -> Step 4b: Extension 系统（Plugin Load->Init->Shutdown）              // assemble.go:398-419
    -> Step 4c: HookChain（扩展 Hooks 桥接）                               // assemble.go:425
    -> Step 5:  ApprovalMiddleware（deny-first + y/n/a + 跨会话缓存）      // assemble.go:427-447
    -> Step 5:  FileMutationQueue（per-file 串行化）                       // assemble.go:443-458
    -> Step 6:  ProductionModelWrapper（cost/stats，无 retry）             // assemble.go:466-473
    -> Step 6a: 7层模型中间件链（Failover->Retry->Timeout->Sanitize       // assemble.go:479-487
                ->LoopDetection->Validate->Overflow）
    -> Step 6b: CircuitBreaker                                            // assemble.go:490-498
    -> Step 6c: IdempotentCache + AuditLog + Telemetry                    // assemble.go:501-519
    -> Step 6d: HookAwareToolMiddleware + PathNormalizer + SchemaValidator // assemble.go:529-537
    -> Step 7:  OutputGuardChain（PII + CodeInjection + Length）           // assemble.go:540-544
    -> Step 8:  RealSubAgentFactory（真实 LLM + 工具的子智能体）           // assemble.go:547-554
    -> Step 8b: ACP（stdio/gRPC，配置驱动）                                // assemble.go:560-581
    -> Step 9:  extraTools（todo/task/goal/web/ask_user/plan_mode/git_pr） // assemble.go:608-633
    -> Step 10: LoopAgent（WithLLM + WithTools + WithModelWrapper          // assemble.go:636-652
                + ExecutionModeParallel + Tracer + SteeringChannel + ThinkingConfig）
    -> Step 10b: FileMemoryStore + LLMMemoryExtractor（Extractor 死接线）  // assemble.go:658-695
    -> Step 10c: SystemPromptBuilder（AGENTS.md/CLAUDE.md + Skills + Memories） // assemble.go:698-720
    -> Step 11: 6层 Agent 中间件链（Logging->LoopDetector->PlanMode       // assemble.go:754-769
                ->SystemReminder->FailureSynthesis->Hook + ACP + Extension）
    -> Step 12: Compaction（Unified/Micro/Summary/Truncating + MidTurn）   // assemble.go:772-785
    -> Step 13: SessionStore（JSONLSessionStore）                          // assemble.go:788-795
    -> Step 14: Resume（从 JSONL 重建历史）                                 // assemble.go:822-828
    -> Step 15: AgentImpl + HarnessImpl + TurnRunner                       // assemble.go:837-847
  -> TUI 渲染（手写 ANSI 零依赖 + BridgeEvents + Steer/Pause/Resume）
  -> 每轮写 SessionStore -> 下一轮
```

---

## 2. 优势（超出同类平均水平）

### 2.1 7层模型中间件链已完整接线运行时

**源码证据**：
- `assemble.go:479-487`：`llm.NewStandardMiddlewareChain(...)` 构造 7 层链
- `assemble.go:548,640`：经 `newModelWrapperWithChain(pw, modelChain, circuitBreaker, guardChain, telemetry)` 注入 SubAgent 和 LoopAgent
- `output_guard_adapter.go:186-214`：组合 5 层包装（pw cost/stats -> chain 7层 -> telemetry -> breaker -> output guard）
- `loop.go:243-249`：每次 Run 启动时对裸模型动态应用 wrapper

**关键发现**：ProductionModelWrapper **并未替代**中间件链。assemble.go:466-473 构造 pw 时只传入 CostTracker/StatsRegistry/ModelName/SessionID，**未传入 RetryPolicy**，因此 pw 不做 retry。retry 职责由链中 RetryModelMiddleware 承担。两者互补而非替代。

### 2.2 TUI 真正的两阶段 Markdown AST 解析

**源码证据**：
- `markdown/ast.go:6-23`：14 种 NodeType 的完整 AST 节点定义
- `markdown/block_parser.go`：块级解析（heading/codeBlock/HR/blockquote/list/table/paragraph），引用块递归解析
- `markdown/inline_parser.go`：行内解析（bold/italic/strikethrough/inline code/image/link），支持反斜杠转义和嵌套强调
- `markdown/renderer.go:44-59`：渲染器遍历 AST 节点树

**结论**：这是真正的自研 AST 解析器，非正则统一着色。不依赖 goldmark/blackfriday，不支持 setext 标题等 CommonMark 边角特性，但结构上是真正的 AST。

### 2.3 语法高亮支持 10 种语言

**源码证据**：
- `highlighters/specs.go:179-198`：注册 10 种语言（go/python/javascript/typescript/rust/java/bash/json/yaml/sql）+ 别名
- `highlighter.go:18-24`：零依赖 ANSI escape 着色，识别关键字/字符串/注释/数字

**结论**：比"统一着色"强（有语言感知的关键字集），但非真正 token 级高亮（无语法分析、无 token 分类）。

### 2.4 StreamingBash 是默认 bash 路径

**源码证据**：
- `streaming_bash.go:113-145`：`cmd.StdoutPipe()` + `cmd.StderrPipe()` 双管道 + 2 个 goroutine 并发读取
- `builtins.go:112`：`NewStreamingBashTool(bashOpts...)` 注册为默认 bash 工具（非 `NewBashTool`）
- `loop.go:587-606`：LoopAgent 做接口类型断言，若工具实现 `StreamingBashTool` 且有 EventStream，走 `ExecuteStreaming` 流式推送

**结论**：生产环境中 `BashTool.Execute`（阻塞 cmd.Run）不会被调用。StreamingBash 是真正的双管道并发流式实现。

### 2.5 Tracing 三级 RedactingExporter 脱敏

**源码证据**：
- `exporter.go:86-149`：`RedactionLevel` 三级（full/redact/off）
  - `full`：原样导出
  - `redact`（默认）：掩码 `Sensitive` 标记的属性
  - `off`：剥离所有属性
- `assemble.go:272-276`：从 config 读取 `RedactionLevel` 并包装 exporter

### 2.6 DeferredToolRegistry 已用于 MCP/Skill 懒加载

**源码证据**：
- `assemble.go:285-286`：`tools.NewDeferredToolRegistryAdapter(underlyingReg)` 创建懒加载适配器
- `assemble.go:972`：MCP 工具通过 `tr.RegisterDeferred(ctx, toolName, factory)` 注册
- `assemble.go:1064`：Skill 工具通过 `tr.RegisterDeferred(ctx, name, factory)` 注册

**结论**：MCP 和 Skill 工具在首次调用时才实例化，减少启动开销。

### 2.7 其他已验证的核心能力

| 能力 | 证据 |
|------|------|
| ReAct 循环（Pause/Resume/Steer） | `loop.go` + `interrupt.go` + `turnrunner.go` |
| 6层 Agent 中间件链 | `assemble.go:754-769` Logging->LoopDetector->PlanMode->SystemReminder->FailureSynthesis->Hook |
| 4级审批（y/n/a + 跨会话缓存） | `assemble.go:427-447` + `approval/callback.go` + `approval/middleware.go` |
| SubAgent 真实执行栈 | `assemble.go:547-554` `NewRealSubAgentFactory` + `dispatch_subagent` 工具 |
| 渐进式 Compaction | `assemble.go:772-785` + `agent.go:113-126` 回写 `a.history` |
| Session 持久化 + 恢复 | `assemble.go:788-828` + `interactive.go:411-455` |
| 生产化治理全套 | `assemble.go:460-544` Retry/CircuitBreaker/IdempotentCache/AuditLog/Telemetry/OutputGuard/CostTracker |
| BashSandbox | `assemble.go:293-304` 路径白名单+命令黑名单+ulimit |
| FileMutationQueue | `assemble.go:443-458` per-file 串行化 |
| Undo/Checkpoint | `file_tracker.go` Backup/Restore（最多 50 个 FIFO） |
| Diff 预览 | `diff.go` LCS 算法 + Git fallback |
| LSP 多语言 | `lsp_multi.go` 按扩展名路由 + JSON-RPC 2.0 |
| Git 深度集成 | 17 高级操作 + 会话分支联动 + PR 创建 |
| 15 个斜杠命令 | `slash.go:79-95` help/cost/compact/clear/tools/model/session/undo/diff/plan/config/history/save/load/memory |
| Box/Spinner/Progress | `box.go` + `spinner.go` + `renderers.go:338-351` |
| Steer 输入模式 | `keyboard.go:21-27` TAB进入/Enter提交/Esc取消/Ctrl+A/E/W |
| Token 状态栏 | `app.go:362-375` Tokens: X/Y (Z%) | Cost: $N + 80% 黄色警告 |
| 跨会话记忆（Store） | `assemble.go:658-688` FileMemoryStore 注入系统提示 + `/memory` 命令 |
| ModelCycler | `assemble.go:240-260` round_robin/weighted/cost_priority + session affinity |
| 6级 ThinkingLevel | `assemble.go:202-209` + `thinking.go` None/Minimal/Low/Medium/High/Max |
| 并行工具执行 | `assemble.go:641` `WithExecutionMode(ExecutionModeParallel)` |
| MCP 三传输 | Stdio/SSE/StreamableHTTP + Hot Reload |
| Skill 系统 | YAML frontmatter + 渐进式披露 |
| Extension 系统 | `assemble.go:398-419` Plugin Load->Init->Shutdown |
| ACP 协议 | `assemble.go:560-581` stdio/gRPC 配置驱动 |

---

## 3. 劣势与缺口

### 3.1 运行时缺口（已实现但未完全接线）

#### LLMMemoryExtractor 死接线

- `assemble.go:694`：`memExtractor = memory.NewLLMMemoryExtractor(model, memStore)` 构造并暴露到 `AgentAssembly.MemoryExtractor`
- **但运行时无任何代码调用 `.Extract()`**：全局搜索 `assembly.MemoryExtractor` / `.MemoryExtractor.Extract` / `memExtractor.Extract` 在非测试代码中找不到任何调用点
- `slashContext`（interactive.go:181-196）也没有 memoryExtractor 字段
- **影响**：自动事实提取功能虽已实现且已"半接线"，但在当前代码里不会自动触发。记忆只能通过 `/memory add` 手动写入，或通过注入系统提示被读取
- **FileMemoryStore 本身完整接线**：assemble.go:658-688 加载记忆注入系统提示，`/memory` 命令（list/add/delete/search/clear）可用

### 3.2 GAP-FULL（完全缺失，3项）

| ID | 能力 | 说明 |
|----|------|------|
| F-019 | Cron 定时任务 | 低优先级，trae 独有 |
| F-036 | MCP OAuth | MCP 客户端无认证流程 |
| F-037 | 多模态/图片支持 | `Message.Content` 为 string，`DowngradeImages` 将图片标记降级为文本。无法发送截图/图片给视觉模型 |

### 3.3 GAP-ENHANCE（需增强，7项）

| ID | 能力 | 说明 |
|----|------|------|
| F-007 | Tool Search | 静态实现，缺动态工具发现、搜索触发阈值与结果注入机制 |
| F-010 | Session Tree CLI | 有完整会话树数据结构，但 CLI 缺 `/tree` `/fork` `/clone` `/resume` `/export` 导航命令 |
| F-029 | RPC Mode | ACP 仅最小适配，缺 30+ RPC 命令中的大部分 |
| F-040 | StopLength | OverflowRecovery 有 context_length_exceeded 检测，缺基于 FinishReason 的精确截断检测和自动续写 |
| F-045 | 信任加载顺序 | 有信任检查但加载顺序未严格保证"信任前仅全局配置" |
| F-047 | /session resume | 标志存在但空树未与实际存储打通 |
| F-049 | PrepareArguments | 无参数预处理钩子（ToolExecutorWrapper 可拦截但无标准化预处理机制） |

### 3.4 GAP-PARTIAL（部分实现，2项）

| ID | 能力 | 说明 |
|----|------|------|
| F-031 | Event/Hook 语义 | 仅 BeforeToolCall/AfterToolCall，缺完整生命周期 Hooks + EventObserver |
| F-039 | Error Handling | 有错误定义，但缺 busy/rejected 等标准错误码与重试/拒绝语义 |

### 3.5 架构级缺口（对比 codex/adk-go/pi）

| 缺口 | 说明 |
|------|------|
| 无 OS 级沙箱 | 仅应用层 BashSandbox（路径白名单+命令黑名单+ulimit），无 Linux namespace/seccomp 隔离 |
| 无 OAuth/PKCE 认证 | 无完整认证流程 |
| 无完整 Hooks 系统 | 仅 BeforeToolCall/AfterToolCall，缺完整生命周期 |
| 模型 Provider 少 | 4 个（OpenAI/Claude/Gemini/Eino）vs pi 30+ |
| 无 IDE 集成协议 | 无 LSP 服务端/DAP 集成 |
| 无用户扩展系统 | 仅 Go plugin .so（跨平台脆弱） |
| OTel 非标准 SDK | 自研 tracing 非标准 OpenTelemetry SDK |
| 无自更新/onboarding | 无版本自更新机制和首次引导流程 |
| 无 Workflow 引擎 | 无 DAG 工作流编排 |
| 无 A2A 服务端 | 仅 ACP 客户端，无服务端 |
| 无部署能力 | 无容器化/部署支持 |

---

## 4. 优先修复路线

按对客户价值的排序：

1. **LLMMemoryExtractor 接线**：在会话结束或轮次结束时调用 `assembly.MemoryExtractor.Extract()`，将提取的事实写入 FileMemoryStore，打通自动事实提取链路
2. **TUI 实时窗口占比**：在 `/cost` 或 TUI 状态栏中显示当前上下文窗口占用百分比
3. **多模态支持**：扩展 Message.Content 支持 image content，接入视觉模型
4. **完整 Hooks 生命周期**：扩展为完整的事件观察者（EventObserver）+ 生命周期 Hooks
5. **OS 级沙箱**：增加 Linux namespace/seccomp 隔离
6. **MCP Resource/Prompt**：补齐 MCP Resource 和 Prompt 能力
7. **Session Tree 导航命令**：在 CLI 暴露 /tree/fork/clone/resume/export

---

## 5. 审计证据索引

| 结论 | 证据 |
|------|------|
| 7层模型中间件链已接线 | `assemble.go:479-487` NewStandardMiddlewareChain；`assemble.go:548,640` newModelWrapperWithChain 注入；`output_guard_adapter.go:186-214` 组合；`loop.go:243-249` 运行时执行 |
| ProductionModelWrapper 不做 retry | `assemble.go:466-473` 未传入 WithWrapperRetryPolicy；`llm_integration.go:99-102` retryPolicy 为 nil 时不包装 |
| TUI Markdown AST 解析 | `markdown/ast.go:6-23` 14种 NodeType；`markdown/block_parser.go` 块级解析；`markdown/inline_parser.go` 行内解析+嵌套 |
| 语法高亮 10 语言 | `highlighters/specs.go:179-198` go/python/js/ts/rust/java/bash/json/yaml/sql |
| StreamingBash 默认路径 | `builtins.go:112` NewStreamingBashTool；`streaming_bash.go:113-145` 双管道+goroutine；`loop.go:587-606` 接口断言走流式 |
| Tracing 三级脱敏 | `exporter.go:86-149` RedactionLevel full/redact/off；`assemble.go:272-276` 从 config 读取包装 |
| DeferredToolRegistry 已使用 | `assemble.go:285-286` NewDeferredToolRegistryAdapter；`assemble.go:972,1064` RegisterDeferred |
| LLMMemoryExtractor 死接线 | `assemble.go:694` 构造但运行时无 Extract 调用 |
| FileMemoryStore 完整接线 | `assemble.go:658-688` 加载+注入系统提示；`interactive.go:195` slashContext.memoryStore；`slash_memory.go` /memory 命令 |
| 15 个斜杠命令 | `slash.go:79-95` help/cost/compact/clear/tools/model/session/undo/diff/plan/config/history/save/load/memory |
| Box/Spinner/Progress | `box.go:21-47`；`spinner.go:14-19` 10帧 braille；`renderers.go:338-351` [█░] 进度条 |
| Steer 输入模式 | `keyboard.go:21-27` TAB/Enter/Esc/Backspace/Ctrl+A/E/W；`app.go:176-182` 状态管理 |
| Token 状态栏 | `app.go:362-375` Tokens: X/Y (Z%) | Cost: $N + 80%黄色警告 |
| 手写 ANSI 零依赖 | `theme.go:1-7` 明确声明零依赖；`app.go:235-288` 手写 select 事件循环；`keyboard.go:49-112` 手写键盘解码 |

---

*审计完成于 2026-08-07。所有结论均基于源码静态跟踪，未参考任何现有文档。*
