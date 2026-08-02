# go-cli

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> **实验性软件** — 本项目处于活跃的实验性开发阶段。由于时间和经费有限，尚未经过充分测试。风险自负，欢迎贡献和反馈。

纯 Go 实现的 AI Agent CLI 框架。零外部运行时依赖，依赖倒置驱动，全链路 tracing。

**[English](README.md)**

---

## 关于本项目

这个项目的时间线读起来像一个兼职开发者的日记：

- **2026 年 1 月** — 想法成型，但组织空气里弥漫着……不置可否。于是它等了等。
- **2026 年 3 月** — 买了 CodePlan 订阅开始实践，只在周末写代码。Go 理论从头学起（学完基本忘光）。搞通了 ReAct Agent + Eino 框架的原型——SSE 流式输出、工具集成，全套。所有提交时间都在凌晨到周一清晨之间。
- **2026 年 4 月** — 重拾多智能体编排和软件工程学，进行了多次快速迭代实验——记忆系统、上下文管理、MCP 集成、Hook 系统。用血泪教训验证了六个系统塞进一个 Agent 上下文窗口等于一场灾难。还学到了「未完全验证」不等于可以部署。
- **2026 年 5 月** — 周末被工作全面接管，项目心电图拉直线。
- **2026 年 6-7 月** — 在存在主义迷茫中徘徊：重复造轮子到底有没有意义？与此同时探索 Loop Engineering 和 Graph Engineering 的边界，眼看着行业飞速跑过——SubAgent 隔离、Git Worktree 并行、动态工作流……
- **2026 年 8 月 1 日** — 受 DeepSeek Harness 评测激励，决定不再想太多，开始动手。四个月的教训浓缩成一个项目：**go-cli**。核心洞察凝结为一条公式：**Agent = Model + Loop + Harness**。剩下的都是架构。

**声明**：会继续迭代，但得看有没有时间。也就是说——更新频率不保证，但心意是真的。

**关于我**：[pjcmice.com](https://www.pjcmice.com)

> Agent 工程正在进入与模型紧密协作的时代——循环不再只是基础设施，它是副驾驶的神经系统。

---

## 架构概览

```
┌──────────────────────────────────────────────────────┐
│  CLI (cmd/cli)                                       │
│  signal → config → tracing → command dispatch        │
└──────────────┬───────────────────────────────────────┘
               │
┌──────────────▼───────────────────────────────────────┐
│  Harness ── Agent ── AgentLoop (ReAct)               │
│  Submit→EventStream   Run→Result   think→act→observe │
│              │                                       │
│         TurnRunner (cancel/steer/follow-up)           │
└──────┬───────┬──────────┬──────────┬─────────────────┘
       │       │          │          │
  ┌────▼──┐ ┌──▼───┐ ┌───▼────┐ ┌──▼──────┐
  │  LLM  │ │Tools │ │Session │ │Compaction│
  │Compose│ │Reg.  │ │Tree    │ │Unified   │
  └───────┘ └──────┘ └────────┘ └──────────┘
       │
  Extension > Config > Builtin
```

### 核心分层

| 层 | 职责 | 关键接口 |
|---|---|---|
| **core** | 接口定义、Agent/Harness/Loop | `Agent`, `Harness`, `AgentLoop`, `TurnRunner` |
| **llm** | LLM 通信与 Provider 组合 | `BaseChatModel`, `ModelProvider`, `ProviderComposer` |
| **tools** | 工具注册与执行 | `ToolDefinition`, `ToolRegistry` |
| **session** | 会话树与持久化 | `SessionStore`, `SessionTree`, `ContextManager` |
| **compaction** | 上下文压缩 | `Compactor`, `TokenEstimator` |
| **extension** | 扩展生命周期 | `Extension`, `Hook`, `Middleware` |
| **production** | 生产弹性 | `CircuitBreaker`, `RetryPolicy`, `AuditLog`, `OutputGuard` |
| **approval** | 审批与信任 | `ApprovalClassifier`, `TrustManager` |
| **config** | 配置加载 | `Loader`, `Config` |
| **tracing** | 全链路追踪 | `Tracer`, `TraceExporter` |
| **mcp** | MCP 协议集成 | `MCPClient`, `MCPToolAdapter` |
| **acp** | Agent 通信协议 | `ACPClient`, `StdioAdapter`, `gRPCAdapter` |
| **skill** | Skill 加载与匹配 | `SkillDefinition`, `SkillLoader`, `SkillRegistry` |
| **tui** | 终端 UI | `App`, `Renderer`, `RendererRegistry` |
| **verify** | 验证框架 | `Scanner`, `LogCapturer`, `GoLeakChecker` |

## 快速开始

### 前置条件

- Go 1.23+

### 构建与运行

```bash
go mod download
make build           # 编译到 bin/go-cli
./bin/go-cli         # 运行
```

### 开发常用命令

```bash
make test            # 单元测试 (含 race)
make test-cov        # 覆盖率报告
make test-log        # 日志捕获验证
make test-leak       # goroutine 泄漏检测
make scan            # AST 扫描 (mock/硬编码绕过)
make lint            # golangci-lint
make fmt             # gofmt + goimports
make verify          # 全量校验 (fmt+vet+lint+build+test+scan+log+leak)
make check           # 提交前检查 (fmt+vet+lint+build+test)
```

## 核心架构细节

### Agent 执行循环

三层架构：**Harness → Agent → AgentLoop**

- **HarnessImpl**: 异步门面，`Submit()` 非阻塞返回 `EventStream`，后台 goroutine 执行 agent
- **AgentImpl**: 有状态包装，维护消息历史，线程安全 (`sync.Mutex`)
- **LoopAgent**: 纯 ReAct 循环 (think → act → observe)，注入 `BaseChatModel` + `ToolRegistry`
- **TurnRunner**: 单轮生命周期，支持 `Cancel`/`Steer`/`FollowUp`

### LLM Provider 组合

三层优先级：**Extension > Config > Builtin**

| 层 | 来源 | 示例 |
|---|---|---|
| Builtin | 预注册 | eino, openai, claude, gemini |
| Config | 配置文件 | 用户自定义 Provider |
| Extension | 扩展注册 | 插件注入 Provider |

同名冲突：高优先级层胜出；同层内高 Priority 胜出。

EinoProvider 基于 `net/http` 实现 OpenAI 兼容协议，零外部依赖。

### 内置工具

| 工具 | 说明 |
|---|---|
| `bash` | 执行 shell 命令 |
| `read` | 读取文件 |
| `write` | 写入文件 |
| `edit` | 差异编辑 (old/new 替换) |
| `grep` | 正则搜索 (支持纯 Go 模式) |
| `find`/`ls` | 文件查找与列表 |
| `search` | 语义搜索 |

### 压缩策略

按成本递增自动路由：**Micro → Summary → Truncating**

| 策略 | 机制 | LLM 开销 |
|---|---|---|
| Micro | 替换旧工具结果为短占位符 | 无 |
| Summary | LLM 驱动语义切割 + 摘要 | 有 |
| Truncating | 从最老条目开始丢弃 | 无 |

**UnifiedCompactor**: 路由层，任何策略失败则升级到下一策略。

**MidTurnCompact**: 溢出自动压缩守卫，token 估算超过阈值比例时触发。

### Session 管理

- **SessionTree**: append-only 树结构，支持分支 (`Branch`)、移动 (`MoveTo`)、上下文重建 (`BuildContext`)
- **JSONLSessionStore**: 文件持久化，每条记录一行 JSON，追加写入
- **BranchSummary**: 离开分支时自动生成摘要

### 扩展系统

扩展生命周期：`Init` → 运行 → `Shutdown`

注册构建块：
- **Hook**: 事件钩子，支持 `pass`/`block`/`terminate`/`replace` 动作
- **Middleware**: 请求/响应拦截，可包装 Agent 和 Model 调用
- **Tool / Command / Provider**: 扩展注册

### 生产弹性

| 组件 | 说明 |
|---|---|
| **CircuitBreaker** | 三状态机 (Closed → Open → HalfOpen)，支持 fallback |
| **RetryPolicy** | 抖动指数退避，错误分类 (Transient/RateLimit/Timeout/Fatal) |
| **IdempotentCache** | FIFO 幂等去重缓存 |
| **AuditLog** | JSONL 审计日志，支持时间范围/操作名/工具名过滤 |
| **OutputGuard** | 输出守卫链：正则阻止 → PII 检测 → 代码注入检测 → 长度截断 |

### 审批与信任

- **ApprovalClassifier**: 工具调用分级 (Allow / Deny / RequireApproval)
- **ApprovalMiddleware**: deny-first 门控，拒绝的调用不抵达执行器
- **TrustManager**: 项目级信任门控，SHA-256 指纹 + 过期机制

### ACP (Agent Communication Protocol)

进程间 Agent 通信协议，两种适配器：

| 适配器 | 传输 |
|---|---|
| **StdioAdapter** | 换行分隔 JSON (io.Pipe) |
| **gRPCAdapter** | JSON-over-HTTP |

消息类型：`connect` / `disconnect` / `message` / `response` / `ack` / `error`

### MCP 工具集成

将 MCP 服务端工具适配为 `ToolDefinition`，注册到工具注册表即可被 Agent Loop 调用。

工具名规范化：`mcp__{server}__{tool}`

支持传输：Stdio (子进程)、SSE、StreamableHTTP

### Skill 系统

YAML frontmatter 格式加载：

```yaml
---
name: my-skill
description: skill description
version: 1.0.0
category: coding
prompt: |
  You are a coding assistant.
tools:
  - bash
  - read
trigger_hint: "fix bug"
parameters:
  max_attempts: 3
---
optional body markdown
```

- **YAMLSkillLoader**: 纯标准库解析，支持 `.md`/`.yaml`/`.yml`
- **SkillAdapter**: 将 SkillDefinition 映射到 ToolDefinition
- **渐进式披露匹配**: 从名称 → 触发提示 → 类别逐层匹配

### TUI

Bubbletea 驱动渲染循环，按 ContentType 分派到 Renderer。支持流式渲染器 (替换最后一帧) 和非流式渲染器 (追加)。内置 24 个渲染器。

### 全链路 Tracing

Span 层级贯穿整个执行链：

```
cli.invocation
  └─ command.dispatch
       └─ prompt.run
            └─ harness.start
                 └─ agent.run
                      └─ loop.run
                           ├─ llm.request
                           └─ tool.call (tool_name=edit|grep|...)
```

导出器：`stdout` / `jsonl` (文件) / `otlp` (HTTP)

### 验证框架

| 工具 | 说明 |
|---|---|
| **Scanner** | AST 扫描，13 条规则检测 mock 导入、硬编码凭据、测试 URL 等 |
| **LogCapturer** | 拦截 slog 全局 handler，断言日志条目/序列 |
| **GoLeakChecker** | goroutine 泄漏检测，2 秒轮询超时 |
| **VerifyRunner** | 26 条验证规则 (VQ/VT/VS/VC/VH/VP/VG/VE) |

## 配置

### YAML 格式

```yaml
provider:
  name: eino
  base_url: http://localhost:9999
  temperature: 0.7
  max_tokens: 4096
model:
  name: gpt-4
  max_tokens: 2048
tools:
  builtin:
    - bash
    - read
    - edit
    - grep
tracing:
  enabled: true
  exporter: jsonl
  level: info
compaction:
  strategy: micro_first
  max_tokens: 1000
approval:
  mode: deny_first
session:
  store: jsonl
  path: ~/.go-cli/sessions
```

### 优先级

升序覆盖：`Default` → `File` → `Env` → `Flag` → `Override`

## 端到端测试

| Phase | 验证范围 |
|---|---|
| Phase 2 | 100+ 轮长对话稳定、自动压缩、Session 恢复、Trace 链完整性、质量指标 |
| Phase 3 | 审批门控、MCP 调用、循环检测、熔断降级、重试、幂等去重、审计记录 |
| Phase 4 | Skill 加载、扩展注册、SubAgent 生成、TUI 渲染、YAML 配置、Provider 组合、ACP 通信、OTLP 导出 |

运行：

```bash
go test -race -count=1 -v -run "TestPhase2|TestPhase3|TestPhase4" ./tests/...
```

## 项目结构

```
go-cli/
├── cmd/cli/                # CLI 入口
├── internal/
│   ├── acp/                # Agent Communication Protocol
│   ├── approval/           # 审批门控与信任管理
│   ├── cli/                # CLI 命令解析
│   ├── compaction/         # 上下文压缩策略
│   ├── config/             # 配置加载 (YAML/JSON)
│   ├── core/               # 核心接口与 Agent/Harness/Loop
│   ├── extension/          # 扩展生命周期
│   ├── llm/                # LLM 通信与 Provider 组合
│   ├── mcp/                # MCP 协议集成
│   ├── mock/               # 测试 Mock (仅测试使用)
│   ├── production/         # 生产弹性 (熔断/重试/审计/输出守卫)
│   ├── session/            # 会话树与持久化
│   ├── skill/              # Skill 加载与匹配
│   ├── tools/              # 工具注册与内置工具
│   ├── tracing/            # 全链路追踪
│   ├── tui/                # 终端 UI
│   └── verify/             # 验证框架 (Scanner/Leak/Log)
├── tests/                  # 端到端集成测试
│   ├── phase2_e2e_test.go  # 长对话 + 压缩 + Session
│   ├── phase3_e2e_test.go  # 弹性 + 审批 + MCP
│   └── phase4_e2e_test.go  # Skill + 扩展 + SubAgent + TUI + ACP
├── .github/workflows/      # CI/CD
├── Makefile                # 构建/测试/验证
└── go.mod                  # Go 1.23+, 仅测试依赖
```

## 设计原则

- **依赖倒置**: `core` 包零向下依赖，仅定义接口；实现由 service 层注入
- **零外部运行时依赖**: LLM 通信基于标准库 `net/http`，JSON 解析基于标准库
- **全链路 Tracing**: 每个操作发射 span，trace_id 贯穿整个执行链
- **生产就绪**: 熔断、重试、幂等、审计、输出守卫开箱即用
- **扩展优先**: Hook/Middleware/Tool/Command/Provider 均可通过扩展注入

## License

[MIT](LICENSE)
