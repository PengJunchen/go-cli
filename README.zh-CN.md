# go-cli

用 Go 编写、可按工作方式配置的终端 AI 助手。选择自己的模型、提示词、规则、工具、Skill、Hook 和外部服务，用审批、持久会话与追踪掌握执行过程。

CLI 与公开扩展 worker API 目前仍处于实验阶段。

[English](README.md) · [运行指南](docs/runtime-guide.md) · [扩展 worker API](pkg/extensionapi/v1/README.md)

## 按你的工作方式配置

| 想调整什么 | 可用方式 |
|---|---|
| 模型与回答方式 | 选择 OpenAI 兼容端点、模型、采样参数、思考级别与轮数上限 |
| 工作规则 | 追加或替换系统提示词，把团队要求放进已授权的项目配置 |
| 可复用流程 | 加载 Markdown/YAML Skill，支持标准 `SKILL.md`；添加自己的 `/命令` 提示词 |
| 可调用工具 | 选择内置工具，封装本地命令，或接入标准 MCP 工具 |
| 工作流自动化 | 通过工具执行前后的 Shell Hook 接收事件 JSON、检查或处理结果 |
| 其他语言的扩展 | 用 JSONL worker 提供工具、Hook、命令、提示词或完整响应的模型提供方 |
| 执行与记录 | 配置审批、上下文压缩、超时、会话保存与链路追踪 |

个人设置放在 `~/.go-cli/config.yaml`；项目设置放在 `.go-cli.yaml` 或 `.go-cli.json`，需要先审阅并授权来源。多数配置在启动时加载；动态模式下，支持的 Skill、提示词和 Hook 可在空闲时 `/reload`，命令文件、自定义命令工具和 worker 则需重启。详见[配置与加载方式](docs/runtime-guide.md#choose-an-extension-by-what-you-need)。

普通定制从文件与配置开始。[原生 Go `.so`](docs/runtime-guide.md#optional-native-go-plugins) 是高级可选项，适用于确实需要运行时加载、进程内集成的可信代码。

## 高效使用

- **按需暴露能力。** 渐进式 Skill 先提供简介，选中后再加载操作说明；惰性 MCP 在选中服务时连接，默认 eager 模式则在启动时连接。项目 Skill 正文仍会为信任检查捕获。
- **接续对话，尝试不同方案。** 保存对话后可按确切物理会话 ID 恢复，分叉或克隆分支，重试已完成回答，或压缩较早上下文。历史会保留；重试不会撤销之前的工具效果。
- **记住项目习惯。** 默认开启自动记忆，可将明确表达的偏好和事实用于同项目后续对话。提取会增加模型请求，正常退出最多等待 30 秒；队列有界、尽力保存，不保证每轮都留下记忆。设置 `memory.enabled: false` 可关闭自动提取和注入。
- **委派独立任务。** 可以让一个或多个子代理并行处理任务，分别指定角色、模型、工具和轮数。子代理有独立资源与审批，当前工具会等待完成。
- **选择轻量界面。** Go 可执行文件无需另装语言虚拟机；`headless` 使用按行交互，`no_plugin` 移除可选原生加载器。外部服务和 worker 仍有各自依赖。

可选 `edge` profile 同时开启动态刷新、渐进式 Skill、惰性 MCP，并提供可配置的能力预算；它与构建标签独立。这些选择控制启动工作量与上下文大小，不代表模型响应或整体性能领先其他 Agent。

Skill 目录复用预处理的检索文本，减少反复搜索时的计算和临时分配，代价是多保留一份检索元数据。

可选 `.so` 可省去子进程通信，但编译进 CLI 的 Go 代码本来就在进程内直接运行，插件并不天然更快。它要求平台、工具链与依赖匹配，共享宿主崩溃风险，不能可靠卸载，更新需要重启。详见[构建选择与取舍](docs/runtime-guide.md#choose-a-build)。

## 快速开始

需要 Go 1.24+；确切最低版本以 [go.mod](go.mod) 为准。

```sh
go mod download
make build
./bin/go-cli --help

export GO_CLI_PROVIDER_NAME=eino
export GO_CLI_PROVIDER_BASE_URL=https://api.openai.com/v1
export GO_CLI_PROVIDER_API_KEY="your-api-key"
export GO_CLI_MODEL_NAME=your-model-id

./bin/go-cli prompt "解释这个仓库的结构"
./bin/go-cli interactive
```

本地 `EinoProvider` 使用 Go 标准库 HTTP 客户端，并未导入 Eino 框架。API 密钥应保存在环境变量或已配置的凭据存储中。

在个人配置中保存对话，并补充一条工作规则：

```yaml
session:
  store_path: .go-cli/sessions
agent:
  append_system_prompt: "修改前先解释重要变更。"
```

`append_system_prompt` 在默认提示词后追加要求，`system_prompt` 则替换它。CLI 不会自动发现 `SYSTEM.md`。相对会话路径以当前工作目录为基准，也可使用绝对路径；自动项目记忆与会话保存独立。

使用项目配置和扩展前，先审阅并授权来源：

```sh
# 在项目目录执行，审阅摘要后按提示输入 TRUST。
./bin/go-cli trust grant
./bin/go-cli trust list
./bin/go-cli interactive
# 撤销时使用 trust list 显示的规范化项目路径：
./bin/go-cli trust revoke /absolute/project/root
```

授权绑定审阅过的来源内容，不覆盖该目录将来的所有文件。变更后可能需要重新授权，再重启或执行支持的刷新；来源授权不代替具体操作的审批。

## 日常使用

```sh
./bin/go-cli doctor
./bin/go-cli models
./bin/go-cli prompt --output json "描述这个项目"
./bin/go-cli session list --store .go-cli/sessions
./bin/go-cli interactive --resume-session <physical-session-id>
```

| 交互命令 | 用途 |
|---|---|
| `/help`、`/tools` | 查看可用命令与已注册工具 |
| `/plan enter`、`/plan exit` | 进入或退出 Plan Mode |
| `/session tree`、`/session branches` | 查看对话条目和分支 |
| `/session resume`、`/session resume <physical-id>` | 列出保存的会话，或打开确切会话 |
| `/session fork feature`、`/session switch main` | 分叉尝试另一方案，再回到主分支 |
| `/clear`、`/retry`、`/compact` | 开始新对话、重试已完成回答或压缩较早上下文 |
| `/session pending` | 检查结果未定的操作 |
| `/memory list`、`/memory add <text>` | 查看或手工添加记忆 |
| `/reload` | 空闲时刷新支持的动态来源 |
| `/mcp list`、`/mcp load docs`、`/mcp reload docs` | 查看、连接或刷新惰性 MCP 服务 |

保存历史需要 `session.store_path`。物理会话 ID 对应保存的对话文件，entry ID 对应该文件中的一个位置。`@session:<entry-id>` 引用当前物理会话的可见历史，也可引用其中另一分支，不会恢复会话。克隆分支、prompt 分叉、旧格式导入与 Git 联动示例见[会话与恢复](docs/runtime-guide.md#sessions-and-recovery)。

取消或丢失响应可能让外部操作结果未定。重复执行前应先检查 pending 记录；切换历史不会解决这些记录。

## 添加工作流或外部工具

标准 Skill 文件 `.go-cli/skills/review/SKILL.md`：

```markdown
---
name: review
description: 检查变更的正确性和缺失测试。
---
阅读 diff，说明具体风险，并建议有针对性的检查。
```

默认使用第一个存在的 Skill 目录：先 `.go-cli/skills`，再 `~/.config/go-cli/skills`；`skill.dir` 可覆盖。若想先展示简介、使用时再加载说明：

```yaml
skill:
  progressive: true
```

其他需求可以选择对应扩展：

| 需求 | 指南 |
|---|---|
| 在正文后追加参数的 `/review` 快捷提示词 | [Markdown 命令](docs/runtime-guide.md#markdown-slash-commands) |
| 配置本地命令的参数、环境变量、目录和超时 | [配置命令工具](docs/runtime-guide.md#configured-command-tools) |
| 工具执行前检查，或结果确定后处理 | [Shell Hook](docs/runtime-guide.md#shell-hooks) |
| 调用现有外部服务的工具 | [MCP 配置与真实调用](docs/runtime-guide.md#mcp-setup-and-execution) |
| Python、JavaScript、Go 等语言的子进程扩展 | [JSONL worker](docs/runtime-guide.md#jsonl-extension-workers) |
| 单任务或并行子代理 | [子代理](docs/runtime-guide.md#delegate-work-to-child-agents) |

内建 MCP 连接、宿主管理的 worker，以及 CLI 管理的命令/Git 工具，可在各自支持的完成判定范围内用于持久会话。任意原生回调、替换注册表或普通客户端包装器不会自动获得该支持。出现在 `/tools` 中不代表所有模式都能执行。

## 当前边界

自动记忆按项目隔离，子代理与 Plan Mode 不自动学习。旧的无项目归属记录仍可在使用其存储时手工查看，不会自动跨项目注入。存储与调用成本见[项目记忆](docs/runtime-guide.md#project-memory)。

Todo、task、goal 状态仅保留在当前进程；保存或恢复对话会保留记录，但不会恢复这些存储。会话快照和通用 schema 迁移仍不支持，旧 JSONL 需要显式严格导入。MCP 目前集成工具，不提供完整 resources/prompts 接口；worker 的模型提供方返回最终响应，不提供逐 token 流式输出，也不能通过 RPC 声明 Skill。

## 关于本项目

三个阶段概括架构的演进：

1. **可配置核心：** 以 Harness、Agent、AgentLoop 分层，通过明确接口连接模型、工具、压缩、Skill、MCP、审批、会话、原生插件、追踪和 TUI。
2. **可恢复执行：** 在既有会话能力上强化持久执行记录与确切恢复，以来源绑定的项目信任、受控取消和资源所有权管理操作。
3. **动态按需扩展：** 能力世代与空闲刷新、渐进式 Skill、惰性 MCP、跨语言 JSONL worker，以及 headless/edge 选择适配不同工作负载；当前会话分支与项目记忆进一步支持日常接续。

相关项目 [go-agent-core-v1](https://github.com/PengJunchen/go-agent-core) 是 Agent 核心 SDK。本仓库是完整 CLI，内部 Go 接口不是公开嵌入 SDK。作者主页：[pjcmice.com](https://www.pjcmice.com)。

## 开发与维护

```sh
make test           # 自动化测试，含 race 检测
make test-e2e       # 带 e2e 构建标签的测试
make test-no-plugin # extension/CLI 包中的原生加载器桩覆盖
make test-cov       # 覆盖率报告
make scan           # AST 规则
make lint           # golangci-lint
make fmt            # gofmt 与 goimports
make check          # 格式、vet、lint、编译、测试
make verify         # check 加 AST、日志与 goroutine 检查
```

生产代码位于 `cmd/`、`internal/` 和 `pkg/`。自动化集成测试及必要 fixture 位于 `tests/`；构建与验证任务见 [Makefile](Makefile)。[文档入口](docs/README.md) 汇集当前产品与 API 指南。

## 设计参考

以下项目为部分设计提供启发，并非必需的运行时依赖：

| 项目 | 借鉴点 |
|---|---|
| [Pi Coding Agent](https://github.com/earendil-works/pi) | 共享 Agent/会话核心与渐进式能力暴露 |
| [OpenCode v2](https://github.com/anomalyco/opencode) | 服务所有权与生命周期组织（V2 研究参考） |
| [DeepSeek Harness / Cordis](https://github.com/deepseek-ai/deepseek-harness) | Harness 组合、依赖所有权与资源清理 |
| [Magpie](https://github.com/yetone/magpie) | 请求 Hook 与插件扩展的职责划分 |
| [HashiCorp go-plugin](https://github.com/hashicorp/go-plugin) | 版本化子进程协议、握手与进程所有权 |
| [CloudWeGo Eino](https://github.com/cloudwego/eino)、[Google ADK-Go](https://github.com/google/adk-go) | Agent 与模型组合思路 |

## License

MIT。
