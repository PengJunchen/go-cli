# 04: 用户可见的能力选择刷新与失败恢复

**Publication:** unpublished-to-tfs

**Status:** verified-local

**User story:** 作为终端用户，我需要查看、搜索、加载和刷新能力，以便在长会话中确定当前可用版本。

**Goal:** capabilities/skills/mcp slash命令与真实prompt工具输入联通；配置刷新在idle边界生效，同代际首次MCP激活影响下一模型request。

**What to build:** capabilities/skills/mcp slash命令与真实prompt工具输入联通，状态显示实际预算、来源类别与生效时机。

**Blocked by:** 01: 确定性代际进入真实运行; 02: 标准Skill发现与按需正文进入模型; 03: MCP按服务器发现加载与受控调用

## Acceptance criteria

- [x] AC-04-1: 能力状态输出显示discovered/loaded/failed/refresh-required与generation、预算、profile和来源类别；不可热换的更新返回restart-required，不输出秘密或完整prompt。
- [x] AC-04-2: 刷新成功下一run采用新计划，当前run继续固定旧计划；非法更新保留旧generation和诊断。
- [x] AC-04-3: slash help/completion可发现新命令，选择后prompt/schema与dispatch一致。
- [x] AC-04-4: 空Skill目录后加入首个能力可reload；prompt预算更新使用新plan；MaxMCPServers、代码/provider/session配置明确要求重启，named MCP刷新不跨project grant套用旧owner。
- [x] AC-04-5: dynamic+eager MCP不后台替换registry；存在MCP来源/owner时明确restart-required，不能仅更新metadata。无MCP来源的Skill/prompt刷新及默认dynamic=false/eager原路径保留。

## Executable verification

以下为复跑入口；本轮实际执行的完整 `make verify` 已覆盖对应race测试。

- `go test -race -count=1 ./internal/cli ./internal/tui -run "Capabilit|Skill|MCP|Slash|Dynamic"`

## Verified evidence

`logs/make-verify-final3.log`（exit 0）中实际CLI dynamic reload/cold表、hook plan不变性、prompt预算增大/候选拒绝、空启动、跨grant、source身份与eager保守合同全部通过；`TestSlashMCPHandlerRoutesRegisteredSubcommands`、`TestSlashMCPCompletionUsesCanonicalSubcommands`、`TestSlashMCPHandlerPreservesBackendErrors` 与现有slash registry/help通过。安全字段投影与同源loader接线经静态复核。这里验证的是idle配置更新和next-request新增schema，代码/模式/provider/session/MaxMCPServers不声称热更。

## Rollback

移除本轮交互handler投影并禁用dynamic，旧slash路由继续工作。

## Rule mapping

Interface-First; Trace Logging; existing Mock contracts; go-cli-infra Go validation and verify rules. No protected-file changes.
