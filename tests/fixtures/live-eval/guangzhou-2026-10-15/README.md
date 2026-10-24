# 广州持续对话评测输入

这些是重跑 2026-10-15 真实评测时使用的输入，不是模型输出或运行日志。原始输出、session/trace、PPT 快照和截图已于 2026-10-24 清理；结论及限制见 [`docs/research/2026-10-24-live-qa-digest.md`](../../../../docs/research/2026-10-24-live-qa-digest.md)。

| 文件 | 用途 |
|---|---|
| `prompts.json` | 104 轮真实工具与旅行 PPT 实验输入 |
| `prompts-text-only.json` | 同条件纯文本 104 轮配对输入 |
| `prompts-v1-original.json` | pilot 发现检索问题前的最初提示词版本 |
| `pilot-prompts.json`, `pilot3-prompts.json` | 前置 pilot 输入 |
| `approval-source-live-prompts.json`, `tool-search-live-prompts.json` | 审批来源及中文工具检索窄场景回归输入 |
| `resume-neutral-prompts.json` | 恢复副本实验的中性输入 |
| `sources.md` | 旅行任务固定引用资料及来源 |
| `acceptance-oracle*.json` | 评测器使用的任务验收规则，不能作为模型输入 |
| `prompts*.sha256` | 对应提示清单的原始校验和 |

`compare.py` 的 profile、完成状态定义、轮次配对和统计限制见 [`scripts/e2e_live/COMPARE.md`](../../../../scripts/e2e_live/COMPARE.md)。
