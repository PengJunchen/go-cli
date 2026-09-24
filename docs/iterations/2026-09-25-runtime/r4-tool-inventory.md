# R4 Tool Inventory

状态：源码清单与建议 effect，供 R4-DES-01 / R4-BLD-01 / R4-BLD-02 使用；不是 effect 实现或 R4 验收。

源码快照：`codex/runtime-r1-history`，`f8c95d1`。本清单以 `internal/cli/assemble.go`、`internal/tools/builtins.go` 和对应 definition 的执行路径为准。R4 策略输入见 `r4-policy-design.md` 与 `execution-tasks.json` 的 R4 任务。下文的 effect 是供设计使用的保守建议，不表示当前注册表已有可信 capability metadata。

## 默认注册工具

`assembleTools` 调用 `tools.RegisterDefaults`，并总是传入共享的 `*DefaultGitTool`，因此默认集合含 7 个核心工具和 17 个 Git 工具。配置 `tools.builtin` 只过滤这 24 个默认项；后续额外工具和动态工具不受该过滤器影响。注册冲突会记录 warning。`read_file` 等 `ReadTool` 名称别名不是 assembly 实际注册名。

| 注册名 | definition / 实际执行器 | 建议 effect 与参数影响 | `Parameters()` |
| --- | --- | --- | --- |
| `read` | `*ReadTool`；读取本地文件 | `workspace_read`；路径决定读取范围 | 缺失 |
| `bash` | `*StreamingBashToolImpl`，内含 `*BashTool`；执行 `bash -lc` | `unknown` / 任意进程能力；命令与 cwd 由参数决定，不能按命令文本降级为 read | 缺失 |
| `write` | `*WriteTool`；写入本地文件 | `workspace_write`；路径和内容由参数决定 | 有 |
| `edit` | `*EditFileTool`；替换本地文件内容 | `workspace_write`；路径和替换文本由参数决定 | 有 |
| `grep` | `*GrepTool`；搜索文件内容 | `workspace_read`；路径、glob 和查询文本由参数决定 | 缺失 |
| `find` | `*FindTool`；枚举匹配路径 | `workspace_read`；根目录和筛选器由参数决定 | 缺失 |
| `ls` | `*LSTool`；列出目录 | `workspace_read`；目录由参数决定 | 缺失 |
| `git_diff` | `*GitDiffTool` → 共享 `*DefaultGitTool.Diff` | `workspace_read`；revision/path 参数限定查询范围 | 有 |
| `git_status` | `*GitStatusTool` → `DefaultGitTool.Status` | `workspace_read` | 有 |
| `git_commit` | `*GitCommitTool` → `DefaultGitTool.Commit` | `git_mutation`；提交信息等由参数决定 | 有 |
| `git_log` | `*GitLogTool` → `DefaultGitTool.Log` | `workspace_read`；revision/limit 参数限定查询 | 有 |
| `git_branch` | `*GitBranchTool` → `DefaultGitTool.Branch` | `workspace_read`；列举分支 | 有 |
| `git_checkout` | `*GitCheckoutTool` → `DefaultGitTool.Checkout` | `git_mutation` + `workspace_write`；分支由参数决定 | 有 |
| `git_blame` | `*GitBlameTool` → `DefaultGitTool.Blame` | `workspace_read`；文件和行范围由参数决定 | 有 |
| `git_push` | `*GitPushTool` → `DefaultGitTool.Push` | `git_mutation` + `network_write` + `external_effect`；remote/branch/force 参数影响目标与范围 | 有 |
| `git_create_branch` | `*GitCreateBranchTool` → `DefaultGitTool.CreateBranch` | `git_mutation`；分支名和 base 由参数决定 | 有 |
| `git_merge` | `*GitMergeTool` → `DefaultGitTool.Merge` | `git_mutation` + `workspace_write`；目标分支由参数决定 | 有 |
| `git_stash` | `*GitStashTool` → `DefaultGitTool.Stash` | `git_mutation` + `workspace_write`；会改变暂存区/工作区状态 | 有 |
| `git_stash_pop` | `*GitStashPopTool` → `DefaultGitTool.StashPop` | `git_mutation` + `workspace_write`；可能产生冲突 | 有 |
| `git_reset` | `*GitResetTool` → `DefaultGitTool.Reset` | `git_mutation`；模式参数可能同时改变 index/工作区，按参数定 effect | 有 |
| `git_revert` | `*GitRevertTool` → `DefaultGitTool.Revert` | `git_mutation` + `workspace_write`；目标 commit 由参数决定 | 有 |
| `git_fetch` | `*GitFetchTool` → `DefaultGitTool.Fetch` | `network_read` + `git_mutation`；remote 由参数决定，fetch 会更新本地 refs/objects | 有 |
| `git_pull` | `*GitPullTool` → `DefaultGitTool.Pull` | `network_read` + `git_mutation` + `workspace_write`；remote/branch 由参数决定 | 有 |
| `git_remote` | `*GitRemoteTool` → `DefaultGitTool.Remote` | `workspace_read`；列出 remote 配置 | 有 |

上述分类是工具直接请求的 Git 操作。执行器通过 Git CLI 运行命令；仓库配置的 hooks / filters 可能再启动任意程序。R4 需要把这类派生执行纳入可信能力边界，无法约束时应保守升级 effect，不能只按子命令名字视为安全。

## Assembly 额外工具

这些定义在 `assembleSubAgent` / `assembleExtraTools` 注册，不受 `tools.builtin` 过滤。它们使用下列直接 executor：

| 注册名 | definition / 实际执行器 | 建议 effect 与参数影响 | `Parameters()` |
| --- | --- | --- | --- |
| `dispatch_subagent` | `*tools.SubagentTool`（由 `core.NewSubagentTool` 构造）→ dispatcher → real subagent factory；子代理共享 assembly 的工具 registry | `unknown` / 复合 effect；role、task 和后续工具调用由参数/模型生成决定，必须继承父级审批及 Plan 限制 | 有 |
| `todo_write` | `*TodoWriteTool` → assembly 内存 `TodoStore` | `control`；修改当前运行的 todo 状态 | 缺失 |
| `task_create`, `task_update` | `*TaskCreateTool`, `*TaskUpdateTool` → assembly 内存 `TaskStore` | `control` 写；字段和值由参数决定 | 缺失 |
| `task_get`, `task_list` | `*TaskGetTool`, `*TaskListTool` → 同一 `TaskStore` | `control` 读；筛选参数决定返回范围 | 缺失 |
| `goal_create`, `goal_update` | `*GoalCreateTool`, `*GoalUpdateTool` → `DefaultGoalStore` | `control` 写；goal 内容及状态由参数决定 | 缺失 |
| `goal_get`, `goal_list` | `*GoalGetTool`, `*GoalListTool` → goal/task stores | `control` 读；ID/筛选参数决定返回范围 | 缺失 |
| `web_fetch` | `*WebFetchTool`；HTTP GET，assembly 注入 HTML converter | `network_read`；URL、redirect 目标和响应内容由请求决定，endpoint 必须进入授权 scope | 缺失 |
| `web_search` | `*WebSearchTool`；配置 `brave` / `fetch` 时为真实 provider，默认构造为 `MockSearchProvider` | 配置真实 provider 时为 `network_read`，query 是出站数据；默认 mock 应由 R4 明确标成 unavailable，而不能视作成功搜索 | 缺失 |
| `ask_user` | `*tools.AskUserQuestionTool`（由 `core.NewAskUserQuestionTool` 构造）→ HITL emitter | `control`；向用户提出问题并等待回答 | 缺失 |
| `enter_plan_mode`, `exit_plan_mode` | `*EnterPlanModeTool`, `*ExitPlanModeTool` → assembly 的 Plan controller | `control`；改变当前 assembly 的 Plan Mode 状态。D-01 仍待产品决策，当前行为由 R0 保留 | 缺失 |
| `git_pr_create` | `*GitPRCreateTool`；运行 `gh` CLI 创建 PR | `network_write` + `external_effect`，并可能读取本地 Git 状态；repo/title/body/base/head 参数决定目标 | 有 |
| `tool_search` | `*ToolSearchTool` → 当前 registry 的 `List` | `control` / registry metadata read；查询和 category 参数只影响列出的工具，不构成 effect 声明 | 缺失 |

## 条件注册和动态注册

| 注册名/来源 | definition / 实际 executor | 建议 effect 与参数影响 | schema / 注册特点 |
| --- | --- | --- | --- |
| `lsp_query`（配置并成功启动 LSP 后） | `*lspTool` → 配置的 `LSPClient` | `workspace_read`；URI、operation 和 language server 影响查询对象。rename 只返回 `WorkspaceEdit`，不应用编辑 | 有；definition 来自 LSP server 配置 |
| `remote_bash`（配置 SSH hosts 后） | `*RemoteBashTool` → `SSHClient` | `unknown` / 任意远端执行；host、command、cwd 和环境参数会改变 endpoint 与副作用 | 有；host scope 必须绑定实际客户端/endpoint |
| 配置自定义工具（`tools.custom_tools`） | `*CustomCommandTool` → `exec.CommandContext` 运行配置的 executable | `unknown` / 任意本地进程；配置的命令、模型参数、cwd、环境决定能力 | 有；只对受信任项目配置注册，schema 不能把任意命令变成可信 effect |
| `mcp__<server>__<tool>` | `*mcp.MCPToolAdapter` → MCP client `CallTool`；支持 stdio/SSE，可能从项目或用户配置自动发现并热重载 | `unknown`；server endpoint、tool identity 和 JSON 参数都可变，需由可信注册边界审核 | adapter 暴露 `Parameters()`，实际 schema 可缺失；通过 deferred loader materialize，热重载可替换同名工具 |
| skill 名称（发现技能目录后） | `*skill.SkillAdapter` → 返回 skill prompt/instructions | 直接 executor 为 `control` / 指令加载；内容会影响后续模型行为，但后续工具 effect 仍按实际工具判定 | 无 schema；经 deferred loader 注册，技能内容不是 capability 声明 |
| Extension plugin tools（extensions enabled 且配置 PluginPaths） | 插件提供的任意 `ToolDefinition.Execute` | `unknown`；插件可定义任意副作用 | 无受信任 effect 契约；注册到同一 registry |

`image_read` 有独立 definition 和 schema，但当前 `assembleTools` / `assembleExtraTools` 没有注册它，不属于默认或条件 assembly 工具。`MCPToolAdapter`、skills、LSP、remote bash、custom tools 与 extension tools 的具体集合取决于配置、server 响应或插件内容，因此不能用一份静态名字清单替代运行时注册快照。

## Schema 与执行 wrapper 现状

`tools.Parameterized` 是可选接口。当前明确实现该接口的 assembly 工具是 `write`、`edit`、全部 17 个 Git builtins、`git_pr_create`、`dispatch_subagent`，以及条件注册的 `lsp_query`、`remote_bash`、自定义命令、MCP adapter。`read`、`bash`、`grep`、`find`、`ls`、todo/task/goal、web、HITL、Plan、tool_search 和 skill 没有该接口。实现接口也不保证 schema 非 nil：MCP input schema 可以缺失。当前 schema validator 只对存在 `Parameterized` 的 definition 有可用 schema，R4-BLD-02 需补齐模型 schema 与执行校验的一致性。

Assembly 在 `assembleApproval` 用 `MiddlewareToolRegistry` 安装 Approval 和 mutation queue；随后 production wrapper 增加 prompt injection、Plan、参数准备/schema validation、audit/idempotency/telemetry、result masking 与 post-tool hook。Loop 路径另安装 Plan `ToolInterceptor`，用于在 executor 前产生 `tool_canceled` 语义；executor wrapper 是最终兜底。

当前 `MiddlewareToolRegistry.Get` 返回只实现 `ToolDefinition` 的 `wrappedToolDef`，不转发 `Parameterized`、streaming 等可选接口；其 `List` 直接返回未包装的原 definition。Loop 从 `List` 构建模型工具定义，而执行时通过 `Get`；因此 schema 查询和受控执行路径经过不同对象。直接拿 `List` 返回值执行也能绕过这些 wrappers。Deferred registry 的未加载项以无 schema 的 stub 出现在 `List`，实际工具仅在 `Get` 时加载。

## Plan Mode 与待 R4 闭合项

R0 临时按名称允许以下现有只读/控制工具：`read`、`grep`、`find`、`ls`、`lsp_query`、`tool_search`、`ask_user`、`git_diff`、`git_status`、`git_log`、`git_branch`、`git_blame`、`git_remote`、`web_fetch`、`web_search`、`goal_get`、`goal_list`、`task_get`、`task_list`、`enter_plan_mode`、`exit_plan_mode`。其余名字在该临时 controller 下被阻止。该表是过渡止血，不是可信 effect metadata；同名 definition 替换仍可能继承名字对应的允许结果。D-01 未决，R0 只保留现有 `exit_plan_mode` 行为。

R4-BLD-01 应用可信注册来源和 executor identity 为 effect 加权，并逐工具测试 Plan/审批；不能将 Bash、Remote Bash、MCP、自定义命令、扩展或复合 subagent 按名称当作只读。保留明确批准的 `web_fetch` / 已配置真实 provider 的 `web_search` 网络读取策略，同时将默认 mock search 明确失败/不可用。R4-BLD-02 应为缺 schema 工具补齐 schema 与执行校验，并覆盖参数改变 endpoint/path/effect 的情况。当前清单不宣称这些 R4 行为已实现或验证。
