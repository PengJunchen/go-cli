# 辅助摘要与思考强度：冻结配置的有效行为

核查日期：2026-10-29。依据 Go CLI 提交 `5aa063ee647f753f7f0fc7282a3baf5b6e691932` 和锁定安装的 Pi 1.4.0 源码；本次核查没有发模型请求，也没有修改 NavD 配置。这是源码调用链判断，辅助路径是否实际触发仍须结合 wire/trace。

当前 Go profile 设置 `small_model.model=""`，因此没有接入 LLM 摘要器。`micro_first` 保持原生实现，但在需要摘要且没有摘要器时采用 truncating 降级。不能把这项配置描述成“仍调用同一个主模型做摘要”。

| 路径 | 当前有效行为与源码定位 |
|---|---|
| Go 主循环与子 Agent | 主循环传入 configured thinking；受管子 Agent 显式继承父级思考强度和默认模型。参见 [`managed_subagent.go:61`](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/cli/managed_subagent.go#L61)。task 指定其他模型仍受固定模型门禁限制。 |
| Go 摘要接入 | [`assemble.go:911`](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/cli/assemble.go#L911) 仅在 small_model 非空时构建辅助模型；[2072行](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/cli/assemble.go#L2072) 仅在模型存在时注入摘要器。[unified.go:57](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/compaction/unified.go#L57) 明确缺摘要器时跳过摘要；持久历史分支也采用相应截断投影。 |
| Go 摘要的 effort | 如果后续启用辅助模型，[`assemble.go:2800`](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/cli/assemble.go#L2800) 调用 Generate 时没有 WithThinking。原生请求可能省略 effort；这与主循环显式 high 被编码层漏传是两个不同问题，不能仅凭不同就称为同一 bug。 |
| Pi 标准 compaction | 安装包 `dist/core/agent-session.js:248-276,2209` 将当前 session thinking 传入摘要；`dist/core/compaction/compaction.js:463-467` 设置 reasoning，Pi AI OpenAI 兼容层再映射 effort。当前物理模型/high 下预期仍为 high。 |
| Pi 分支摘要 | 安装包 `dist/core/compaction/branch-summarization.js:188,224` 未把 thinkingLevel 放入摘要 options，可能省略 effort。当前 print 模式单任务没有主动分支导航；后续分支长任务需另核实际 wire。 |
| Pi 子 Agent | 锁定包的原生 tools 集合没有 dispatch_subagent，本 profile 禁 extensions/MCP；不把自定义多 Agent 扩展当成此次原生功能。 |

NavD 保持该配置，单独报告 Go 内部128000压缩预算与 Pi500000容量/摘要策略差异。是否产生压缩及其影响，依据本次记录判断，不能从源码差异推定本题输赢。R1b 的 truncating 压力测试只能证明截断、工具、持久会话和续接机制，不覆盖真实 LLM 摘要的质量、费用或 effort 合同。

后续正式长任务应先选定并公开以下一种对照，再运行：

- **全部请求统一 high**：主循环、子 Agent、摘要都受同一限制。先用无费真实触发测试确认兼容。原生辅助请求被严格门禁拒绝时，报告协议/对照条件不兼容，不能将其归为纯模型能力差。
- **主 high、辅助保留原生策略**：分别登记摘要/子 Agent 的模型、允许 effort（包括省略）和预算。若要用同底模启用 Go 摘要，可另设 `small_model.model` 为同一主模型；这仍是新 profile，须公开。辅助角色必须来自可信调用点或可核事件，不能只凭请求正文自报来绕过门禁。

两种方案都计算全部辅助、重试及失败调用的时间、token和可见费用；不静默补写 gateway 参数，不在看到结果后切换口径。当前冻结 NavD 的模型、配置、来源与预算均不变。
