# 上下文与压缩预算审计

审计对象：新主线 `5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1` 加 Thinking 编码修复的 D 配置。只读源码、已保存的无费 fixture 和历史 wire usage；无推理请求，不回写历史任务。

## 当前事实

- 本机模型目录 `GET http://127.0.0.1:3425/v1/models` 返回 200，选中 `codex/gpt-6.1-sol` 存在，声明上下文为 **500000**、最大输出为 **128000**。这是网关声明；本轮没有通过长输入推理独立实证极限。两 CLI 主请求实际输出上限均为 16384。
- Pi 1.4.0 的冻结 `models.json` 写入 `contextWindow=500000`，与目录声明一致。原生压缩启用、`reserveTokens=16384`、`keepRecentTokens=20000`，触发条件为估算上下文 `> 500000 - 16384`，即约 **483616**。这不是每题累计 token 限额。
- Go 当前 CLI 模型目录只从配置模型名称建立 `ModelInfo{Name: ...}`，没有读取 relay 的 `/v1/models`。其 `ContextWindow` 和 `InputTokenLimit` 为 0，模型容量保持**未知**；不能宣称 Go 内部已经收到 500k 元数据。默认 `model_registry` 未启用，单纯打开外部 models.dev 查询也不能假定会把网关别名正确注入当前请求的容量元数据。
- Go 正常 **ConfigLoader** 默认是 `compaction.max_tokens=128000`、`strategy=micro_first`。原生 0.8 阈值和请求消息预算约为 **102400**；这是本地估算的消息预算，不能与真实完整请求的 provider usage 直接划等号。工具 schema、协议 framing 与 provider 容量校验是另一些项。当前 small model 为空，未注入 LLM summarizer；实际使用原生 Unified 路由及其降级行为。

源码依据：`internal/config/loader.go` 的默认配置；`internal/cli/assemble.go` 的 `appendConfiguredModel`、`newCLICompatibleProvider`、`assembleModel`、`assembleCompactor`；`internal/core/request_envelope.go`；`internal/compaction/midturn.go`。Pi 依据冻结安装包的 `dist/core/settings-defaults.js` 与 `dist/core/compaction/compaction.js`。

## 更正旧的 8k 解释

`assemble.go` 的 **8000 是没有配置预算时的裸 Assembly fallback**，不是本轮真实 CLI 经过 ConfigLoader 后的默认值。旧 `379134d` 的 loader 也默认 128000，B 保存的配置没有覆盖 compaction。因此旧 B“实际使用 8k”没有证据，应更正解释，不能据此宣称 D 把预算从 8k 提升到 128k。原始运行文件和成绩保持不变。

离线读取旧 B/Pi wire 的标量结果：

| 历史候选 | 有 input usage 的请求 | 每请求 `prompt_tokens` 最大值 | 超过 128000 |
|---|---:|---:|---|
| Go B | 3/3 | 8372 | 否 |
| Pi | 35/35 | 47114 | 否 |

这些是 provider 报告的输入 usage，不是本地压缩估算，也不包括推测的缺失请求；不能仅用该表断言没有压缩。历史 Go B 的停止原因仍是已记录的资源完成证明故障，不能改归因为容量耗尽。

## 本轮决策与限制

D 保留 Go 128k/micro_first 与 Pi 500k/原生策略。**同一模型 API 不等于两套 Harness 内部预算相同**：这项差异是当前配置与实现的一部分，报告须单列。Go 缺少容量元数据也须标为未知，不能拿本地 compaction 上限冒充真实模型窗口。

不降低 Pi 的真实窗口、不静默放大 Go 预算，也不把两边策略改成同一种。若后续要比较共同 500k 容量配置，先设计通用模型元数据传入能力与测试，另冻结 profile、运行无费参数验收，再独立评分；不能覆盖本轮 D 或旧 B/Pi 的结果。现有短 fixture 没有触发长上下文边界，因此尚未实测双方长程压缩后的有效性。
