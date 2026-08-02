# go-cli

Go CLI 工具框架。

> 完整行为约束、工作流、验收规范等详见 [go-cli-infra](../go-cli-infra/) 仓库：
> - [CLAUDE.md](../go-cli-infra/CLAUDE.md) — LLM 行为约束
> - [AGENTS.md](../go-cli-infra/AGENTS.md) — 跨工具标准
> - [design/](../go-cli-infra/design/) — 设计文档
> - [rules/](../go-cli-infra/rules/) — 验证规则
> - [infra/](../go-cli-infra/infra/) — 任务规划与工作流

## 四原则

1. **Think Before Coding** — 显式声明假设，不确定就问，该反驳就反驳
2. **Simplicity First** — 不超出需求加功能，单次使用不做抽象
3. **Surgical Changes** — 不改进相邻代码，每行改动可追溯至用户请求
4. **Goal-Driven Execution** — 需求转测试，测试先行通过

## 三大横切关注点

1. **接口优先** — 核心抽象先定义接口，再提供默认实现
2. **链路追踪日志** — 每个关键操作发出 tracing span
3. **TDD Mock Server** — 测试先写 mock server 验证行为

详见：[go-cli-infra design/](../go-cli-infra/design/)

## 子智能体优先

分项任务委派子智能体执行，主上下文只做调度。详见 [go-cli-infra CLAUDE.md](../go-cli-infra/CLAUDE.md)。

## 三阶段闭环

规划 → TDD → 执行。详见 [go-cli-infra workflow](../go-cli-infra/infra/)。

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

go-cli-infra/                  # 基础设施仓库 — 约束、规则、规划
├── design/                    # 设计文档
├── infra/                     # 任务规划与工作流
├── rules/                     # 验证规则
├── CLAUDE.md                  # LLM 行为约束
└── AGENTS.md                  # 跨工具标准
```
