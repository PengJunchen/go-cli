# 采样参数的轻量追加核查

**这是旧profile的源码核查。新main与新Pi无费fixture已经增加实际wire标量观测，当前结果见[迁移审核](main-migration-review.md)；不回填旧日志缺失项。**

日期：2026-10-28。范围：已冻结配置与本地源码链路；未读取题解，未修改正在运行的 pair，没有新增真实模型调用。当前 relay 没有记录下列全部标量，因此这里区分源码证据与实际 wire 证据。

| 字段 | go-cli 冻结配置与编码链 | Pi 1.4.0 冻结配置与编码链 | 可以下的结论 |
|---|---|---|---|
| `temperature` | profile 未设；`defaultConfig()` 不填温度，presence 标记为真；`applySamplingSettings` 只转发显式值；HTTP 编码在未设置时省略 | custom eval model 未设 sampling 参数；Agent loop 不填 temperature；SDK 仅显式设置时写入请求 | 两边源码链均省略，不能声称 Go 默认发 0.7 或 Pi 实际取 1 |
| `top_p` | 当前 HTTP 请求类型没有该字段，原生调用选项不注入 | `samplingParams` / thinking-level sampling 未设置，兼容编码器无默认赋值 | 两边省略；服务端有效值未知 |
| `seed` | 当前请求类型不注入 | 本 profile 无 samplingParams，兼容编码器不注入 | 两边省略；AB/BA 的 seed 20261028 是运行顺序种子，不是模型采样 seed |
| `parallel_tool_calls` | 当前请求类型不注入；本地工具并发策略另属 Harness 行为 | 本 profile 无 samplingParams，Chat Completions 编码器不默认填此字段 | 两边省略；不能把本地工具并行能力等同该 API 字段 |

## 本地证据入口

- go-cli：`internal/config/loader.go` 的 `defaultConfig`、`internal/cli/assemble.go` 的 `applySamplingSettings`、`internal/llm/provider.go` 的 `openAIRequest` 与 `samplingTemperature`。
- Pi：固定 payload 的 `pi-coding-agent/dist/core/provider-composer.js` 中 `modelFromJson` 只读取 definition 中的 sampling 配置，不从查找出的 defaults 继承它；`pi-agent-core/dist/agent.js` 的 `createLoopConfig` 未设上述值；`pi-ai/dist/api/simple-options.js` 只透传已设置的选项；`pi-ai/dist/api/openai-completions.js` 仅显式 temperature 或非空 samplingParams 才写入相关字段。
- 相关 profile JSON 哈希、Pi 依赖锁及二进制身份记录在每次 trial 的 manifest 与 payload manifest。

这些证据支持“当前双方采用共同服务端默认采样条件”，不支持“已观测服务端内部温度完全相同”。当前 pair 不修改、不中断；后续若增强审计，可把这些安全标量加入记录，并在新一轮 profile 中冻结，不能回填未记录的历史 wire。

## 官方参数支持边界

[OpenAI GPT-6 指南](https://developers.openai.com/api/docs/guides/latest-model)明确要求：推理强度不是 none 时移除 temperature、top_p 等不支持的采样字段。因此不应为了形式上的对齐，向本次 high 请求强行添加 0.7 或 1；也不能把“不支持”写成“服务端保证忽略”。

[Chat Completions 参考](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)将 seed 标为 deprecated，且尽力确定性也不保证复现；parallel_tool_calls 是通用接口中的工具控制项。具体模型支持仍以模型指南为准。

指南同时说明 GPT-6.1 Sol 的原生工具调用使用 Responses。当前实验采用用户指定的共同 Chat Completions 兼容入口，按该服务契约记录 actual model/high/usage；不把兼容入口接受请求等同于已验证所有原生 API 参数内部生效，也不为本次另行研究网关实现。
