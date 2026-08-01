# AGENTS.md

> 跨工具开放标准（Codex / Cursor / Gemini CLI / Claude Code 等兼容）。
> 完整规范详见 [go-cli-infra](../go-cli-infra/) 仓库：
> - [CLAUDE.md](../go-cli-infra/CLAUDE.md) — 完整 LLM 行为约束
> - [AGENTS.md](../go-cli-infra/AGENTS.md) — 完整跨工具标准

## Setup

```bash
go mod download    # 依赖
make build         # 编译
make check         # 提交前检查
make verify        # 全量校验
```

Go 版本：1.23+。

## 核心原则

- **子智能体优先**：分项任务委派子智能体执行，主上下文只做调度
- **三角色**：designer → builder → reviewer，每阶段产出是下阶段输入
- **TDD 驱动**：先写测试再实现，验收标准可执行验证

详见：[CLAUDE.md](../go-cli-infra/CLAUDE.md) 和 [workflow.md](../go-cli-infra/infra/workflow.md)

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
├── cmd/cli/                   # CLI 入口
├── internal/cli/              # CLI 核心实现
├── internal/config/           # 配置系统
├── internal/verify/           # 校验框架
├── .github/workflows/         # CI/CD
├── CLAUDE.md
└── AGENTS.md

go-cli-infra/                  # 基础设施仓库 — 完整约束、规则、任务
├── design/                    # 设计文档
├── infra/                     # 任务规划
├── rules/                     # 验证规则
├── CLAUDE.md                  # 完整 LLM 行为约束
└── AGENTS.md                  # 完整跨工具标准
```
