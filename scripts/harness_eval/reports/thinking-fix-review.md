# Thinking 编码修复：独立审核

**这是旧379基线的发现记录。最新main仍确认同一编码缺陷，本次移植与验证状态见[当前发布审核](release-review.md)；历史二进制与新版本不混称。**

日期：2026-10-28。此修复在收费任务开始前，由无模型费用的真实 CLI 预检发现；不包含任何标准题的解法、任务专属提示或评分器修改。

## 问题与范围

go-cli 的 CLI `openai` 路线实际使用 Eino 的 `HTTPChatModel`。原版 `379134d679dcae5c996b730eeae464fdb61fe853` 已接收 high 配置，但 `buildBody` 忽略了传下来的 Thinking options，实际请求中没有 `reasoning_effort`。本地审计 relay 拒绝这些请求，没有转发给真实模型，因此原版没有可报告的同 high 正式任务成绩。

生产实现仅改 `internal/llm/provider.go` 一行：JSON 序列化后复用已有 `injectThinking(data, genOpts, OpenAIThinkingAdapter{}, "")`。工具、采样、模型选择、原生提示、循环与压缩行为均不在此修复范围内。

## 独立验证

- 修复前亲跑回归：Generate/Stream 的 high、medium 四子例实际字段 nil 而失败；显式 none 两例通过。
- 修复后独立补充未传 `WithThinking` 的 unset 情况，形成 Generate/Stream × high/medium/none/unset 八子例。独立 `go test -race ./internal/llm -run '^TestHTTPChatModelThinkingOnWire$' -count=1` 全通过，耗时 1.756 秒。
- 同时检查 max_tokens、temperature、tool schema、tool_choice，以及流式 include_usage 没有丢失。
- 新二进制通过真实 CLI 的无费 read→final fixture：两次 exact model/high/16384 请求、HTTP 200 和完整终止标记，真实 read 工具返回产物，exit 0、0 契约拒绝。它证明编码链路已通，不证明解决了真实标准题。

## 兼容边界

HTTP 兼容路径此前静默忽略 thinking，现在会传递已有显式或默认 Thinking 配置。本次模型支持 high。对不接受 `reasoning_effort` 的兼容端点，应显式配置 `thinking:none`；不新增能力注册表或静默参数降级。未设置 `WithThinking` 的直接库调用维持原请求形状，不新增该字段。

## 身份与后续检查

修复后二进制 SHA256 为 `749416f8a5bc20eb8d875aa54ed413a09e9c4602dbace04b51f3d294bd0777b5`；生产实现相同，补充测试后的源码补丁身份独立记录在 `thinking-fix-reviewed.patch` 及 payload manifest，不能混称原版 379 的能力成绩。

CI 同版 golangci-lint v2.12.2 已从官方发布资产下载、验证 SHA256，并通过本仓库配置校验。全量 make verify 在首 pair 空档运行；截至本记录仍为 pending，不把 targeted/race 通过写成全量通过。
