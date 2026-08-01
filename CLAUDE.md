# go-cli

Go CLI 工具框架。

> 完整行为约束、工作流、验收规范等详见 [go-cli-infra](../go-cli-infra/) 仓库：
> - [CLAUDE.md](../go-cli-infra/CLAUDE.md) — 完整 LLM 行为约束
> - [AGENTS.md](../go-cli-infra/AGENTS.md) — 完整跨工具标准
> - [design/](../go-cli-infra/design/) — 设计文档
> - [rules/](../go-cli-infra/rules/) — 验证规则
> - [infra/](../go-cli-infra/infra/) — 任务规划

## 四原则

1. **Think Before Coding** — 显式声明假设，不确定就问，该反驳就反驳
2. **Simplicity First** — 不超出需求加功能，单次使用不做抽象
3. **Surgical Changes** — 不改进相邻代码，每行改动可追溯至用户请求
4. **Goal-Driven Execution** — 需求转测试，测试先行通过

## 三大横切关注点

1. **接口优先** → [design/04-interface-first-design.md](../go-cli-infra/design/04-interface-first-design.md)
2. **链路追踪日志** → [design/05-trace-logging-design.md](../go-cli-infra/design/05-trace-logging-design.md)
3. **TDD Mock Server** → [design/06-tdd-mock-server-design.md](../go-cli-infra/design/06-tdd-mock-server-design.md)

## 子智能体优先

分项任务委派子智能体执行，主上下文只做调度。详见 [CLAUDE.md](../go-cli-infra/CLAUDE.md)。

## 三阶段闭环

规划 → TDD → 执行，由 milestone.md 和 tasks.json 驱动。详见 [workflow.md](../go-cli-infra/infra/workflow.md)。

## 快速命令

```bash
make build      # 编译
make test       # 测试（含 race 检测）
make check      # 提交前检查（fmt+vet+lint+build+test）
make verify     # 全量校验（check+scan+test-log+test-leak）
make scan       # AST 扫描（mock/硬编码检测）
```

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
