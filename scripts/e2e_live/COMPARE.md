# 广州持续对话真实对比测试

`compare.py` 运行两棵独立工作树的真实 race CLI 二进制，将同一固定提示清单交给本机 Magpie 的 `codex/gpt-6.1-sol`。不生成、替代或修复模型产物，不使用模拟模型。每棵工作树使用独立 HOME、工作目录、配置、持久化会话和 trace 目录。

```sh
python3 scripts/e2e_live/test_compare.py
python3 scripts/e2e_live/compare.py \
  --develop /absolute/develop/worktree \
  --baseline /absolute/main/worktree \
  --prompts tests/fixtures/live-eval/guangzhou-2026-10-15/prompts.json \
  --fixtures tests/fixtures/live-eval/guangzhou-2026-10-15/sources.md \
  --output /absolute/new/evidence/directory
```

Reusable prompt manifests, source snapshots and acceptance oracles are kept in
[`tests/fixtures/live-eval/guangzhou-2026-10-15/`](../../tests/fixtures/live-eval/guangzhou-2026-10-15/).
The 2026-10-24 cleanup removed the old per-turn responses, sessions, traces,
screenshots, rendered decks and raw gate logs after extracting their findings
to [`docs/research/2026-10-24-live-qa-digest.md`](../../docs/research/2026-10-24-live-qa-digest.md).

严格共同 profile 默认每次模型调用 total 30 秒、stream idle 10 秒。默认每用户轮装置上限150秒用于多次工具往返和审批。顺序逐轮交替 AB/BA；上一组 proxy 请求未静默结束时不提交另一组，避免测试本身同时占用上游。配置路径不同，标准化配置哈希替换组根目录后必须一致。所有审批均仅放行一次，拒绝轮的每次审批均拒绝。

截图来自真实 CLI PTY 字节进入 xterm.js 后的浏览器渲染，不是操作系统应用窗口截图。忙时是 CLI 默认 Bubbletea TUI，结束时回到 readline。每轮保存 busy、可观察到的首次内容、最终画面；审批保存完整审阅分页。最终截图无法显示长文本时，旁边 `actualAnswer` 和独立 `Tnnn-answer.txt` 保存真实上游文本。原始 PTY 不进行重绘修饰。截图中的 Branch 是持久化会话分支；Git 修订以 metadata 和网页列标题为准。

每轮 `completed` 必须有本轮新的成功 durable terminal、非空实际模型文本、完成的 HTTP 200 流和真正返回待输入状态。它仅表示对话完成；任务内容、来源准确性、记忆、目标工具是否执行、审批语义和 PPT 验收由独立 Reviewer 判断。`not_run` 不计作完成或失败尝试。进程退出后保留该组断裂，存活组继续；不暗中重启会话。

速度主指标为两组均完成、且 kind 为 short-text 的轮对：必须没有工具/审批、恰好一次上游请求；排除重试和非流式摘要。全部完成轮耗时另供参考。elapsed 包含装置 idle 稳定等待；另提供首次内容 TTFT、UI 可见内容、首次 idle 和审批耗时，防止将快速报错当作速度优势。服务缓存、后台负载、模型随机性和实际输出长度仍是干扰变量。

证据：`manifest.json`、每组 `metadata.json`、`ledger.json`、`bridge.json`、`magpie-audit.json`、`pty.raw`、原始 trace 和 session 文件；PPT 创建/五次更新轮保存原文件快照与 SHA-256。`evidence-index.html` 本地直接打开，支持轮号筛选和失败筛选，原始截图并排并链接完整逐轮数据。

扩展开发分支的 `10m/4m` 实用长任务 profile 必须写入单独输出目录与表格，不能与 main 固定 `30s/10s` 混为公平速度比较。
