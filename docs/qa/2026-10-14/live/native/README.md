# 本机 Magpie 界面截图

这些 PNG 是通过 CUA 从本机运行的 `/Applications/magpie.app` 原生窗口直接截取，未生成或修改像素。

- `01-magpie-gateway-protocol-probe.png`：安装后最初的网关协议探测，不能代替 Dot 工具验收。
- `02-magpie-real-dot-diagnostic-requests.png`：2026-10-14 15:35 的真实 Dot 诊断轮，请求由透明观察代理转发，所以 UI 客户端显示 Python-urllib。包含 `codex/gpt-6-luna` 的实际 200 响应及先前 `codex/gpt-6.1-sol` 被 CLI 超时取消的 499 请求。Luna 诊断不替代默认 6.1 验收，200 推理也不等于整个工具流程通过。

“尚无 Agent 经由此路由”是 Magpie 内置 Agent 接入状态；本次 Dot 使用项目独立 OpenAI-compatible endpoint，最近调用和 per-request 路由/用量才是实际请求证据。没有切换本机 Codex 自己的 provider 配置。
