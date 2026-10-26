# 动态能力、边缘构建与扩展使用

本轮实现维持默认兼容配置，边缘profile显式启用渐进能力。验证和性能结果见本目录最终验证记录；命令可用性以该记录为准。

## 启用 edge profile

在已信任项目的 `.go-cli.json` 或用户 `~/.go-cli/config.json` 合并配置；provider API key建议用现有环境/认证配置，不写进项目文件。

```json
{
  "provider": {"name": "openai", "model": "your-model", "base_url": "https://your-compatible-endpoint/v1"},
  "runtime": {"profile": "edge"},
  "skill": {"dir": ".go-cli/skills"},
  "mcp": {
    "servers": [
      {"name": "local-tools", "command": "/absolute/path/to/mcp-server", "args": [], "env": {}}
    ]
  }
}
```

`edge`生效值为 `dynamic=true`、`skill.progressive=true`、`mcp.loading=lazy`，默认预算128个Skill、2个MCP server、131072字节 prompt + schema。预算字段为 `runtime.max_skills`（最大1024）、`max_mcp_servers`（最大128）、`max_prompt_bytes`（最大16777216）；零值使用profile默认。默认profile仍使用legacy Skill/eager MCP装配；可单独配置 `skill.progressive`、`mcp.loading` 和 `runtime.dynamic`。

启动原有CLI即可：

```bash
export GO_CLI_PROVIDER_API_KEY='your-key'
go-cli interactive
# 或相同agent装配的非交互入口
go-cli prompt --provider openai --model your-model 'Complete the task'
```

项目来源仍沿用既有trust决策。未受信任项目不能通过新能力目录绕过执行/审批规则。

## 查看、搜索、加载与刷新

在交互会话使用：

```text
/capabilities
/skills
/skills search pdf
/skills load pdf-processing
/mcp list
/mcp load local-tools
/mcp reload local-tools
/reload
```

`/capabilities`显示当前profile、目录revision、metadata预算与能力状态。`/skills`搜索metadata，不主动执行脚本。`/mcp list`列出配置和连接/错误状态；`load`只连接选中的server并激活其工具schema，新的schema从下一模型请求生效。`/mcp reload <server>`在idle时构建替换连接/工具目录，候选失败保留旧的有效状态。`/reload`通过启动时相同loader重取配置与verified project sources，idle时编译Skill目录、prompt和hook候选；成功后下一run使用新快照。候选失败或新来源仍需trust时保留旧代。启用dynamic但MCP仍eager时，MCP来源保持cold，相关/reload返回restart-required；要同时刷新MCP与Skill/prompt，应启动时选择lazy，eager→lazy需重启。lazy MCP descriptor可更新，已连接server配置变化需单独 `/mcp reload <server>`；provider、session、扩展代码、active MCP 数量上限等变化返回restart-required。prompt字节预算随新计划热更新；降低预算导致候选超限时保旧。

模型侧工具为 `skill_search({"query":"pdf"})`、`skill_load({"name":"pdf-processing"})` 和 `mcp_discover({"server":"local-tools"})`。不传server的MCP discovery只列descriptor；首次选择server才连。schema激活后普通MCP工具继续经过审批、project trust、动态generation和operation guard。MCP内部分页拉取与模型schema投影是两个步骤；本版按server激活，不声称逐工具语义检索排名。

当前stdio `tools/list_changed`将server标为需要refresh，避免在途run被异步替换；使用显式 `/mcp reload <server>`。HTTP尚无change subscription。本版仍声明MCP `2025-06-18`；它与现代2026-07-28差异见[上游证据](../../research/iteration-2026-10-22/upstream.md)，不因tool名带discover就声称实现了现代标准全部扩展。

## 添加标准 Skill

```text
.go-cli/skills/
└── pdf-processing/
    ├── SKILL.md
    └── references/
        └── forms.md
```

`SKILL.md`示例：

```markdown
---
name: pdf-processing
description: Read and fill PDF forms. Use when a task involves PDF files.
---
Read references/forms.md when form details are needed.
Use approved file tools for inputs and outputs.
```

目录使用当前可信项目的 `.go-cli/skills`，或用户固定目录 `~/.config/go-cli/skills`；任意项目外路径不能绕过来源限制。目录名和frontmatter name一致；目录与模型投影先披露name/description，完整正文在skill_load时materialize，辅助文件继续按需读取。用户目录的完整正文按需读并缓存到 `/reload`；项目Skills沿既有trust流程启动时已完整捕获有界正文，load使用已验证captured bytes，因此不能声称所有项目正文启动零磁盘读取或零内存。热加载不能读取未经trust的新字节。`allowed-tools`元数据不替代宿主权限。

## 构建与部署

```bash
# 完整终端UI
go build -trimpath -ldflags='-s -w' -o /tmp/go-cli ./cmd/cli

# 相同核心agent/会话/Skills/MCP，省去富终端绘制和native .so
CGO_ENABLED=0 go build -trimpath -tags headless,no_plugin -ldflags='-s -w' -o /tmp/go-cli-edge ./cmd/cli
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -tags headless,no_plugin -ldflags='-s -w' -o /tmp/go-cli-linux-arm64 ./cmd/cli
```

运行期edge profile降低后台/metadata成本，build tags减少被链接的绘制/native loader；两者分别选择。build tag `no_plugin`只排除 native `.so` loader，标准 stdio/HTTP MCP 与 public SDK JSONL worker 继续可用。headless保留行式多轮交互、SIGINT取消、同步审批、会话提交与最终响应；全屏绘制、主题、运行中键盘steer/modal使用完整构建。Linux交叉编译不代表已在目标边缘设备完成运行实测。

## 外部SDK与RPC worker

公开SDK是 [`pkg/extensionapi/v1`](../../../pkg/extensionapi/v1/README.md)，独立worker示例 [`tests/external-extension/main.go`](../../../tests/external-extension/main.go)只import公开SDK和标准库。

```bash
go build -o /tmp/go-cli-example-worker ./tests/external-extension
shasum -a 256 /tmp/go-cli-example-worker
```

将输出SHA填入单独的 `/absolute/path/example.rpc.json`：

```json
{
  "command": ["/tmp/go-cli-example-worker"],
  "artifact_sha256": "replace-with-the-64-character-sha256",
  "cwd": "/absolute/path/to/workspace",
  "env": {},
  "max_inflight": 8,
  "timeout_millis": 30000
}
```

然后在可信配置启用：

```json
{"extensions":{"enabled":true,"plugin_paths":["/absolute/path/example.rpc.json"]}}
```

该worker贡献 `extension_echo` 工具、`go-cli extension-hello` 命令与prompt片段。worker用 `api.Serve(ctx, os.Stdin, os.Stdout, manifest, handler)`处理 `tool.call`、`command.run` 等请求，stdout专用于JSONL帧。API major为v1，frame上限1MiB。SDK支持已声明的工具、hook、命令、prompt与完整响应provider；不支持RPC Skill贡献，Skills走标准文件目录。

Provider贡献在manifest声明provider/models并处理 `provider.generate` 返回 `api.Message`；用户通过 `go-cli prompt --provider <声明名称> --model <声明模型> "任务"` 选择它。echo示例不声明provider；完整provider声明和实际CLI消费可参考[公开SDK验收worker](../../../cmd/cli/testdata/extension-worker/main.go)与[入口旅程](../../../cmd/cli/extension_entry_journey_test.go)。本版Stream投影为单个最终response。宿主通过显式credential-free `llm.NewExtensionProvider` adapter注册，模型只能选manifest声明值，仍由resolver候选和operation permit授权；worker不会隐式得到宿主API keys、endpoint、credential handle或环境。worker需要的环境通过descriptor `env`显式声明。进程与hash生命周期边界详见SDK README。

## 测量

[可复现测量指南](../../research/iteration-2026-10-22/benchmark.md)提供默认/headless同真实CLI固定任务、PI/OpenCode官方发行对照以及full/headless长会话stream benchmark。本轮[实测结果](../../research/iteration-2026-10-22/results.md)与[机器记录](../../research/iteration-2026-10-22/measurements.json)已持久化。固定输入比较框架成本与真实模型任务成功率是两项实验；未执行部分不填性能排名。
