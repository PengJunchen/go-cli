# AGENTS.md

> 跨工具开放标准（Codex / Cursor / Gemini CLI / Claude Code 等兼容）。
> 完整规范详见 [go-cli-infra](../go-cli-infra/) 仓库。

## Setup

```bash
go mod download    # 依赖
make build         # 编译
make check         # 提交前检查
make verify        # 全量校验
```

Go 版本：1.24+（与 go.mod 对齐）。

## 核心原则

- **子智能体优先**：分项任务委派子智能体执行，主上下文只做调度
- **三角色**：designer → builder → reviewer，每阶段产出是下阶段输入
- **TDD 驱动**：先写测试再实现，验收标准可执行验证

详见：[go-cli-infra CLAUDE.md](../go-cli-infra/CLAUDE.md) 和 [go-cli-infra AGENTS.md](../go-cli-infra/AGENTS.md)

## Code Style

- PascalCase/camelCase，缩写词全大写或全小写
- 导入分三组，组间空行
- 错误用 `%w` 包装保留错误链
- goroutine 生命周期须可控

详见：[go-validation-rules.md](../go-cli-infra/rules/go-validation-rules.md)

## Testing

```bash
make test       # 单元测试（含 race）
make verify     # 全量校验
make scan       # AST 扫描
```

详见：[verify-rules.md](../go-cli-infra/rules/verify-rules.md)

## PR

- 分支：`main` / `develop` / `feature/*` / `fix/*`
- Commit：`<type>(<scope>): <description>`
- 受保护文件禁止 AI 自行修改

## 项目结构

```
go-cli/                        # 本仓库 — 代码实现
├── cmd/cli/                   # CLI 入口（含 cli.invocation Span）
├── internal/core/             # 核心层：AgentLoop, Agent, Harness, TurnRunner, Hooks, Middleware, SubAgent, Registry, EventStream
├── internal/llm/              # LLM 层：Provider, ModelRegistry, Composer, Native/Eino 适配
├── internal/tools/            # 工具层：Bash, Read, Write, Edit, Grep, Find, Ls, MCP 适配, Deferred, Mutation
├── internal/session/          # 会话层：SessionTree, Store, JSONL 持久化, Branch, Context 重建
├── internal/compaction/       # 压缩层：Micro, Summary, Truncating, Unified, Quality, Midturn
├── internal/approval/         # 审批层：Classifier, Store, Trust, Permission, Middleware
├── internal/skill/            # 技能层：SkillDefinition, Loader, Registry, Adapter
├── internal/production/       # 生产化：CircuitBreaker, Retry, LoopDetection, Idempotent, OutputGuard, Audit, Telemetry
├── internal/tracing/          # 链路追踪：Tracer, Span, Exporter(JSONL/OTLP/Kafka), Async, Slog 集成
├── internal/extension/        # 扩展层：Extension, Middleware, PluginLoader, Registry, Manager
├── internal/mcp/              # MCP 集成：Adapter, Registry, ToolAdapter, HotReload
├── internal/mock/             # Mock 框架：LLMServer, ToolServer, MCPServer, ConfigProvider, TraceExporter 等
├── internal/tui/              # 终端 UI
├── internal/acp/              # ACP 协议
├── internal/cli/              # CLI 核心实现（命令路由, prompt 交互）
├── internal/config/           # 配置系统：Types, Loader, Validator, Settings, YAML
├── internal/verify/           # 校验框架：Scanner, AST 规则, VQ/VG 规则, Goroutine 泄漏检测, 日志捕获
├── tests/                     # 集成测试与 E2E 测试
├── .github/workflows/         # CI/CD
├── CLAUDE.md
└── AGENTS.md

go-cli-infra/                  # 基础设施仓库 — 约束、规则、规划
├── design/                    # 设计文档
├── infra/                     # 任务规划与工作流
├── rules/                     # 验证规则
├── CLAUDE.md                  # LLM 行为约束
└── AGENTS.md                  # 跨工具标准
```
