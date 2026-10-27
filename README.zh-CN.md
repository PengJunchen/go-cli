# go-cli

用 Go 编写的终端 AI 助手。你可以让它阅读仓库、修改代码、运行命令，或把独立任务交给子代理。它连接你配置的模型，通过审批、会话记录和链路追踪展示执行过程。

CLI 及公开扩展 worker API 目前均为实验性。可执行文件无需另装虚拟机；模型端点、MCP 服务和 worker 仍有各自的运行要求。

[English](README.md) · [运行指南](docs/runtime-guide.md) · [扩展 worker API](pkg/extensionapi/v1/README.md)

## 关于本项目

架构演进分为三个阶段：

1. **基础 CLI：** 以 Harness、Agent、AgentLoop 分层，通过明确接口连接模型、工具、会话和上下文压缩。
2. **可靠执行：** 加入审批与项目信任、持久会话、取消与资源生命周期、链路追踪和 TUI，使执行过程可检查、可控制。
3. **动态轻量扩展：** 提供渐进式 Skill、惰性 MCP 发现、空闲边界刷新和公开 JSONL worker API，支持完整、headless 与 `no_plugin` 构建。独立的 `edge` profile 提供能力限额与惰性加载。

相关项目 [go-agent-core-v1](https://github.com/PengJunchen/go-agent-core) 是接口驱动的 Agent 核心 SDK。本仓库是完整 CLI；公开 worker API 面向子进程扩展，内部 Go 接口不是公开嵌入 SDK。

作者主页：[pjcmice.com](https://www.pjcmice.com)

## 快速开始

需要 Go 1.24+；确切最低版本以 [go.mod](go.mod) 为准。

```sh
go mod download
make build
./bin/go-cli --help
```

先配置 OpenAI 兼容模型端点，再开始对话：

```sh
export GO_CLI_PROVIDER_NAME=eino
export GO_CLI_PROVIDER_BASE_URL=https://api.openai.com/v1
export GO_CLI_PROVIDER_API_KEY="your-api-key"
export GO_CLI_MODEL_NAME=your-model-id

./bin/go-cli prompt "解释这个仓库的结构"
./bin/go-cli interactive
```

本地 `EinoProvider` 使用 Go 标准库 HTTP 客户端，并未导入 Eino 框架。API 密钥应保存在环境变量或已配置的凭据存储中。

## 配置与项目信任

个人设置放在 `~/.go-cli/config.yaml`。例如，启用对话保存、JSONL 追踪，并补充一条工作习惯：

```yaml
session:
  store_path: .go-cli/sessions
tracing:
  enabled: true
  exporter: jsonl
agent:
  append_system_prompt: "修改前先解释重要变更。"
```

`agent.append_system_prompt` 会在默认助手提示词后追加要求。只有想替换基础提示词时才使用 `agent.system_prompt`。CLI 不会自动发现 `SYSTEM.md`。

项目设置放在 `.go-cli.yaml` 或 `.go-cli.json`。项目配置及捕获的扩展来源，需要先审阅并授权：

```sh
# 在项目目录执行，审阅来源后按提示输入 TRUST。
./bin/go-cli trust grant
./bin/go-cli trust list
./bin/go-cli interactive
# 撤销时使用 trust list 显示的规范化项目路径：
./bin/go-cli trust revoke /absolute/project/root
```

授权在后续启动或受支持的空闲刷新验证通过后生效，不会直接改变当前运行；它只针对已捕获的来源，不覆盖该目录将来的所有文件。来源变更可能需要重新授权，部分情况还需要重启。没有有效授权时，CLI 继续使用个人配置。信任项目来源与批准具体工具操作是两件事。

## 日常使用

```sh
./bin/go-cli doctor
./bin/go-cli models
./bin/go-cli prompt --output json "描述这个项目"
./bin/go-cli interactive --resume-session <physical-session-id>
```

交互模式中的常用命令：

| 命令 | 用途 |
|---|---|
| `/help` | 查看命令及当前限制 |
| `/tools` | 查看本次会话已注册的工具 |
| `/plan enter`、`/plan exit` | 进入或退出 Plan Mode |
| `/session tree`、`/session pending` | 查看保存的会话和结果未定的操作 |
| `/reload` | 空闲时刷新支持的 Skill、提示词和 Hook |
| `/mcp list` | 查看已配置的 MCP 服务 |
| `/mcp load <server>` | 在惰性模式中连接指定服务 |
| `/mcp reload <server>` | 惰性模式下，空闲时刷新服务工具列表 |

保存对话需要 `session.store_path`。恢复时使用确切的物理会话 ID，包括子代理返回的 ID。若操作结果是 `unknown`，应先检查待处理记录再决定是否重试；取消或断线并不能证明外部操作已经停止。

### 把任务交给子代理

可以让助手委派独立的调查、实现、审查或测试任务。`dispatch_subagent` 工具支持单任务，也支持多个任务并行执行并汇总结果。你可以指定角色、模型、工具列表和轮数上限。

每个子代理都有独立对话、工具注册表、审批和执行资源。默认 `context_mode: independent` 只从任务说明开始；`context_mode: inherit` 额外提供派发前的父对话快照作为参考，不复制权限或执行记录。启用会话保存后，文本结果会显示各子代理的物理会话 ID，结构化结果元数据也会保留这些 ID，可用于单独恢复子会话。

当前工具会等待任务完成，不提供后台代理管理界面。父任务取消时，也会取消并等待子资源收尾；无法确定的外部结果仍保留为 `unknown`。详见[任务示例与边界](docs/runtime-guide.md#delegate-work-to-child-agents)。

## 如何扩展

增加工作方法和行为习惯，通常从文件与配置开始。连接外部工具可以用 MCP；需要独立程序和语言依赖时可以用 worker。

| 需求 | 选择 | 加载位置 |
|---|---|---|
| 可复用的操作说明或工作流 | Skill 文件 | `skill.dir` 或默认 Skill 目录 |
| 个人或团队的行为要求 | `agent.append_system_prompt` | 个人配置或已授权项目配置 |
| 自己输入的快捷提示词 | Markdown 命令 | `commands.dir` 或默认命令目录 |
| 在指定事件执行动作 | Shell Hook | 主配置或 `.go-cli/hooks.yaml` |
| 调用其他程序或服务的工具 | MCP 服务 | `mcp.servers` |
| 自定义工具、Hook、命令、提示词或模型提供方 | JSONL worker | `extensions.plugin_paths` |

若希望不选 edge 也能刷新支持的文件与设置，可开启 `runtime.dynamic: true`；否则重启生效。渐进式 Skill 和惰性 MCP 也会开启动态能力。

### Skill 与快捷命令

Skill 用来教助手完成特定任务。标准文件可以放在 `.go-cli/skills/review/SKILL.md`：

```markdown
---
name: review
description: 检查变更的正确性和缺失测试。
---
阅读 diff，说明具体风险，并建议有针对性的检查。
```

默认使用第一个存在的目录：先 `.go-cli/skills`，再 `~/.config/go-cli/skills`。可通过 `skill.dir` 指定目录。旧的平铺 Markdown 和 YAML Skill 定义继续支持。普通模式在启动时读取 Skill；想先列出简介、使用时再加载说明，可主动开启：

```yaml
skill:
  dir: .go-cli/skills
  progressive: true
```

如果只是想自己输入一个快捷提示词，可在 `.go-cli/commands` 或 `~/.config/go-cli/commands` 放置 `review.md`，之后输入 `/review`。文件中的 `$ARGUMENTS` 会替换成命令后面的文字。`commands.dir` 可覆盖默认目录。命令在交互命令表初始化时加载，修改后需要重启。详见[文件示例](docs/runtime-guide.md#skills-prompts-and-custom-commands)。

### Hook

Hook 在 `session_start`、`pre_tool_use`、`post_tool_use` 或 `session_end` 事件执行 Shell 命令。例如，在个人配置中记录已完成的工具事件：

先在当前项目创建日志目录：

```sh
mkdir -p .go-cli
```

```yaml
hooks:
  hooks:
    - event: post_tool_use
      command: "cat >> .go-cli/tool-events.jsonl"
      timeout: 5s
```

命令从标准输入接收事件 JSON。Post-tool Hook 在原工具批次结果确定后，作为独立 Bash 动作运行，有自己的审批和会话记录。Hook 失败不会重跑原工具。你可以从容阅读审批请求，脚本执行超时从实际执行阶段开始计时。普通 pre-tool Hook 启动错误可能允许工具继续执行；强制限制应使用审批策略。[Hook 指南](docs/runtime-guide.md#shell-hooks)说明了触发时机及项目独立文件的不同格式。

### MCP 工具

MCP 让助手调用另一个程序或服务提供的工具。在个人配置中添加服务：

```yaml
mcp:
  loading: lazy
  servers:
    - name: docs
      url: https://your-mcp-service.example/mcp
```

将 URL 替换为实际服务地址。省略 `loading` 时默认 `eager`，启动即连接并获取工具列表。`lazy` 模式下，助手通过 `mcp_list` 查看本地服务信息，再用 `mcp_discover` 连接选定服务；你也可以输入 `/mcp load docs`。工具说明会在下一次模型请求中出现。发现工具不等于批准调用。

内建 HTTP 和宿主管理的 stdio 连接支持在持久会话中实际调用工具，仍需经过正常审批。断线、超时或非法响应可能使结果未定，CLI 不会自动重放。普通客户端包装器不会自动获得这项支持。详见[MCP 配置与执行](docs/runtime-guide.md#mcp-setup-and-execution)，包括本地命令和项目来源限制。

### 扩展 worker

需要用独立程序实现功能时，可以注册可信的本地 `.rpc.json` 描述文件：

```yaml
extensions:
  enabled: true
  plugin_paths:
    - ./plugins/helper.rpc.json
```

CLI 启动时拉起 worker 并交换 `initialize` 握手，在 Agent 开始前注册功能。修改程序或描述文件后需要重启。[worker API 指南](pkg/extensionapi/v1/README.md)提供完整描述文件和协议。

Worker 可以加载，但它提供的工具目前还不能在持久会话中执行。Worker 与你共享操作系统权限，并不是沙箱；其模型提供方只返回最终响应，不提供逐 token 流式输出。

## 构建选择

普通构建包含富终端界面。`headless` 改为按行交互；`no_plugin` 只移除可选的原生 Go `.so` 加载器。

| 构建 | Go 标签 | 终端界面 | 可选原生 Go 插件加载器 |
|---|---|---|---|
| 完整版 | 无 | 富 TUI | 可用 |
| 完整版，无原生插件 | `no_plugin` | 富 TUI | 禁用 |
| Headless | `headless` | 按行交互 | 可用 |
| Headless，无原生插件 | `headless,no_plugin` | 按行交互 | 禁用 |

```sh
go build -o ./bin/go-cli ./cmd/cli
go build -tags no_plugin -o ./bin/go-cli-no-plugin ./cmd/cli
go build -tags headless -o ./bin/go-cli-headless ./cmd/cli
go build -tags headless,no_plugin -o ./bin/go-cli-headless-no-plugin ./cmd/cli
```

四种构建共享 Agent 核心、文件扩展、MCP 和 worker，并各自遵守上文限制。Headless 保留多轮输入、最终响应、SIGINT 取消和文本审批；全屏布局和执行中的键盘引导需要富界面。

### 按需加载能力

`edge` 运行时 profile 与构建标签独立：

```yaml
runtime:
  profile: edge
```

它启用动态能力、渐进式 Skill 和惰性 MCP。默认限额为 **128 个 Skill、2 个活跃 MCP 服务、128 KiB** 能力提示词/schema 预算，可用正数 `max_skills`、`max_mcp_servers`、`max_prompt_bytes` 覆盖。它改变加载策略，不改变链接到程序中的包；项目 Skill 正文仍需为信任检查捕获。详见[profile 与刷新](docs/runtime-guide.md#build-tags-and-runtime-profile-are-separate-axes)。

### 高级可选项：原生 Go 插件

编译进宿主的 Go 工具和 Hook 已能在进程内直接调用，不需要 `.so`。原生插件用于运行时加载 Go 代码，普通扩展不依赖它，也不代表它比编译入宿主的代码更快。只有确实需要动态装载，且实测收益值得承担限制时才考虑。

原生插件受平台、工具链和依赖一致性限制，拥有宿主权限，故障可能影响整个进程，不能卸载，更新需要重启。详见[运行指南](docs/runtime-guide.md#optional-native-go-plugins)与 [Go plugin 文档](https://pkg.go.dev/plugin)。

## 当前限制

持久会话支持确切 ID 恢复，但不支持持久化的 fork、clone、分支切换、快照和 schema 迁移。部分历史命令也不可用，以 `/help` 为准。`/memory` 只管理手工记录，不会自动提取或注入提示词。Todo、task、goal 状态仅在当前进程保留。

普通 custom/worker 工具、`git_pr_create` 等部分已注册工具，目前还不能在持久会话中执行。`/tools` 只显示已注册名称，实际执行限制见[运行指南](docs/runtime-guide.md)。MCP 目前只集成工具；worker 不能通过 RPC 协议提供 Skill。

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
| [OpenCode v2](https://github.com/anomalyco/opencode) | 显式服务所有权、生命周期作用域与刷新失败边界 |
| [DeepSeek Harness / Cordis](https://github.com/deepseek-ai/deepseek-harness) | Harness 组合、依赖所有权与资源清理 |
| [Magpie](https://github.com/yetone/magpie) | 插件所有权，以及 hook 与子进程扩展的职责划分 |
| [HashiCorp go-plugin](https://github.com/hashicorp/go-plugin) | 版本化子进程协议、握手与进程所有权 |
| [CloudWeGo Eino](https://github.com/cloudwego/eino)、[Google ADK-Go](https://github.com/google/adk-go) | Agent 与模型组合思路 |

## License

MIT。
