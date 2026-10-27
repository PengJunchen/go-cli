# go-cli

以 **Harness → Agent → AgentLoop** 为核心的 Go AI Agent CLI，集成模型、工具、持久会话、上下文管理和终端界面。独立可执行文件无需另装虚拟机；模型端点、MCP 服务和扩展 worker 仍有各自的运行要求。

项目及其公开扩展 API 目前均为实验性。

[English](README.md) · [运行时指南](docs/runtime-guide.md) · [扩展 worker API](pkg/extensionapi/v1/README.md)

## 关于本项目

架构演进分为三个阶段：

1. **基础 CLI：** 以 Harness、Agent、AgentLoop 分层，通过明确接口连接模型、工具、会话和上下文压缩。
2. **可靠执行：** 加入审批与项目信任、持久会话、取消与资源生命周期、链路追踪和 TUI，使执行过程可检查、可控制。
3. **动态轻量扩展：** 提供渐进式 Skill、惰性 MCP 发现、空闲边界刷新和公开 JSONL worker API，支持完整、headless 与 `no_plugin` 构建。独立的 `edge` profile 提供能力限额与惰性加载。

相关项目 [go-agent-core-v1](https://github.com/PengJunchen/go-agent-core) 是接口驱动的 Agent 核心 SDK。本仓库定位为完整 CLI 框架；公开 worker API 面向子进程扩展，不是通用 Agent 核心嵌入 SDK。

作者主页：[pjcmice.com](https://www.pjcmice.com)

## 快速开始

需要 Go 1.24+；确切最低版本以 [go.mod](go.mod) 为准。

```sh
go mod download
make build
./bin/go-cli --help
```

发送提示前先配置模型端点。以下示例适用于 OpenAI 兼容端点：

```sh
export GO_CLI_PROVIDER_NAME=eino
export GO_CLI_PROVIDER_BASE_URL=https://api.openai.com/v1
export GO_CLI_PROVIDER_API_KEY="your-api-key"
export GO_CLI_MODEL_NAME=your-model-id

./bin/go-cli prompt "解释这个仓库的结构"
./bin/go-cli interactive
```

本地 `EinoProvider` 通过标准库 `net/http` 发送 OpenAI 兼容请求，并未导入 Eino 框架。

CLI 以用户/全局配置和环境设置为基础。只有项目来源清单通过信任验证后，才会叠加已捕获的项目配置；未获授权时，继续仅使用用户/全局配置。

可在用户配置 `~/.go-cli/config.yaml` 中启用持久会话：

```yaml
provider:
  name: eino
  base_url: https://api.openai.com/v1
model:
  name: your-model-id
session:
  store_path: .go-cli/sessions
tracing:
  enabled: true
  exporter: jsonl
```

API 密钥应保存在环境变量或已配置的凭据存储中。

## 构建组合

`headless` 与 `no_plugin` 是相互独立的构建标签：

| 构建 | Go 标签 | 终端界面 | 原生 Go `.so` 加载器 |
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

四种构建共享 Agent 核心、Skill、MCP 集成和 JSONL worker。Headless 保留多轮输入、最终响应、SIGINT 取消和同步文本审批；全屏布局、主题、模态审批及执行中的键盘引导需要富界面。

### Edge profile

运行时 profile 独立于构建标签，可与上述四种构建任意组合：

```yaml
runtime:
  profile: edge
  # 可选的正数覆盖值：
  # max_skills: 128
  # max_mcp_servers: 2
  # max_prompt_bytes: 131072
```

`edge` 启用动态能力、渐进式 Skill 和惰性 MCP 加载。限额为零或未设置时，采用默认的 **128 个 Skill、2 个 MCP 服务，以及 128 KiB** 能力提示词/schema 预算。它改变加载方式与资源限额，不会移除二进制中链接的包。

已验证的项目 Skill 来源仍需捕获有界内容以完成信任检查。元数据优先发现不代表完全不读取 Skill 正文。详见[运行时指南](docs/runtime-guide.md#build-tags-and-runtime-profile-are-separate-axes)。

## 常用命令

```sh
./bin/go-cli --help
./bin/go-cli --version
./bin/go-cli doctor
./bin/go-cli models
./bin/go-cli prompt --output json "介绍这个项目"
./bin/go-cli interactive
./bin/go-cli interactive --resume-session <physical-session-id>
```

交互会话中的常用命令：

| 命令 | 用途 |
|---|---|
| `/help` | 查看命令与当前能力边界 |
| `/tools` | 列出当前会话注册的工具 |
| `/reload` | 在空闲时刷新支持的 Skill、提示词与 hook 计划 |
| `/mcp list` | 查看配置的 MCP 服务 |
| `/mcp load <server>` | 在惰性模式下激活服务 |
| `/mcp reload <server>` | 在惰性模式下于空闲时替换服务目录 |
| `/session tree`、`/session pending` | 检查持久会话与未决动作 |
| `/plan enter`、`/plan exit` | 由用户显式切换 Plan Mode |

持久会话需要配置 `session.store_path`；启动恢复使用确切的物理会话 ID。持久模式不支持 fork、clone、分支切换、快照和 schema 迁移。`/memory` 管理用户录入的记录，不会自动提取记忆或注入模型提示词。Goal、task 和 todo 状态只保留到当前进程退出。

## 架构与扩展边界

```text
CLI：配置 → 追踪 → 命令分发
                   ↓
            Harness → Agent → AgentLoop
                       ↓
               TurnRunner 生命周期
                   ↓
          模型 · 工具 · 会话 · 上下文压缩
                   ↓
          信任 · 审批 · 追踪 · 扩展
```

- **Harness** 通过事件流暴露异步执行；**Agent** 管理会话状态；**AgentLoop** 执行思考 → 行动 → 观察。**TurnRunner** 管理取消、引导和后续输入。
- **模型与工具** 使用可注入接口。Provider 按 Extension → Config → Builtin 优先级选择。可用工具取决于当前会话注册表和配置。
- **会话与上下文** 提供 JSONL 持久化、上下文重建和 Micro → Summary → Truncating 压缩。CLI 的持久会话流程比内部会话树 API 更窄。
- **执行控制** 包含审批、项目信任、重试、熔断、审计、输出守卫，以及贯穿命令、模型和工具调用的追踪。

支持的动态变更先准备并校验候选，再于空闲边界发布；候选失败时保留上一有效代。Provider/会话配置、原生代码、运行时 profile、加载模式、活动 MCP 限额及部分信任/来源变化需要重启。

MCP 集成目前为 **tools-only**，提供 Stdio、SSE 和 StreamableHTTP 适配器。发现与 `tools/list` 可以暴露远端 schema，但持久 CLI 当前会在分发前拒绝远端 `tools/call`。连接成功或 schema 可见不代表已支持远端执行。

实验性的 [JSONL 扩展 worker API](pkg/extensionapi/v1/README.md) 支持工具、hook、命令、提示词贡献和完整响应 Provider。Provider 的流式适配只返回一个最终响应，不提供 token 增量。Worker 与用户共享操作系统权限，子进程隔离不等于操作系统沙箱。原生 `.so` 扩展在进程内运行，不能卸载。

信任、刷新和协议边界详见[运行时指南](docs/runtime-guide.md#know-which-extension-path-you-are-using)。

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
