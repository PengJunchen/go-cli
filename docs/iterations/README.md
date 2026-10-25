# 迭代报告与架构图

## 2026-10-25：Sol runtime 与 MCP 合同

**状态：本轮 build、verify 和 E2E gates 已通过，真实 live E2E 验收仍在进行。** 2026-10-22 的历史报告不能替代本轮结果；当前 live 报告是阶段记录，Dynamic、Skill、Hook 和 MCP 动态路径尚未验收。完整终态资料待 owner 完成后更新。

- [迭代设计](2026-10-25-sol-runtime/design.md)
- [本轮交互式架构图](2026-10-25-sol-runtime/architecture-iteration.html)：切换 379134d 基线与本轮路径，区分已新增边界、仍 unsupported 的 durable 远端 MCP，以及沿用的 Skill/Hook 与 edge 能力。
- [MCP 合同复审](2026-10-25-sol-runtime/mcp-contract-review.md)
- [运行时验证记录](2026-10-25-sol-runtime/verification.md)：记录 build 与验证门禁结果。
- [审批行为说明](2026-10-25-sol-runtime/approval-behavior.md)：记录审批行为与当前候选范围。
- [真实 Sol 实测计划](2026-10-25-sol-live-e2e/plan.md)
- [阶段性独立证据审查](2026-10-25-sol-live-e2e/independent-review.md)：范围限于已审的基础场景，不代表整体验收。
- [真实 Sol 阶段报告](2026-10-25-sol-live-e2e/report.md)：动态场景仍待验收，不将未完成项标记为通过。

## 2026-10-22：动态能力运行时与真实模型 PTY E2E

- [真实模型与 PTY E2E 报告](2026-10-22-live-e2e/report.md)：记录场景、结果、失败修复和证据边界。
- [动态能力运行时迭代总结](2026-10-22-dynamic-runtime/README.md)：设计、验证和使用说明。
- [交互式架构图](2026-10-22-dynamic-runtime/architecture-iteration.html)：展示动态能力加载前后的架构和逐步流程。
- [脱敏 live 证据 JSON](2026-10-22-live-e2e/live-evidence.json)、[审批失败记录](2026-10-22-live-e2e/failure-iteration-294245.json)、[hook parser 失败记录](2026-10-22-live-e2e/failure-dynamic-43d47.json)。

以下记录和图片来自 **2026-10-22 的历史验收**。本次归档在 2026-10-25 从 Git commit `d813dba` 原样恢复了 24 张截图和以上 3 个 JSON 文件；恢复工作没有重新运行测试，也没有更改报告记载的验收结果。

### 交付物保留边界

报告引用的审阅截图、脱敏证据 JSON 和架构图是交付内容。清理构建产物、缓存、raw PTY 或临时 fixture 时，必须保留这些文件，不得一并清理：`2026-10-22-live-e2e/screenshots/`、`2026-10-22-live-e2e/{live-evidence.json,failure-*.json}` 和 `2026-10-22-dynamic-runtime/architecture-iteration.html`。

### 完整截图

- [01：trust manifest](2026-10-22-live-e2e/screenshots/01-trust-manifest.jpg)
- [02：probe fullpage resize](2026-10-22-live-e2e/screenshots/02-probe-fullpage-resize.jpg)
- [03：probe empty assistant headings](2026-10-22-live-e2e/screenshots/03-probe-empty-assistant-headings.jpg)
- [04：final basic tools](2026-10-22-live-e2e/screenshots/04-final-basic-tools.jpg)
- [05：ask write approval](2026-10-22-live-e2e/screenshots/05-ask-write-approval.jpg)
- [06：failure deny durable publication](2026-10-22-live-e2e/screenshots/06-failure-deny-durable-publication.jpg)
- [10：final3 basic tools](2026-10-22-live-e2e/screenshots/10-final3-basic-tools.jpg)
- [11：final3 ask approval](2026-10-22-live-e2e/screenshots/11-final3-ask-approval.jpg)
- [12：final3 denial survives](2026-10-22-live-e2e/screenshots/12-final3-denial-survives.jpg)
- [20：final approval review](2026-10-22-live-e2e/screenshots/20-final-approval-review.jpg)
- [21：final edit diff](2026-10-22-live-e2e/screenshots/21-final-edit-diff.jpg)
- [22：final escape recovery](2026-10-22-live-e2e/screenshots/22-final-escape-recovery.jpg)
- [23：final auto tools bash](2026-10-22-live-e2e/screenshots/23-final-auto-tools-bash.jpg)
- [24：final deny policy](2026-10-22-live-e2e/screenshots/24-final-deny-policy.jpg)
- [30：dynamic skill v1](2026-10-22-live-e2e/screenshots/30-dynamic-skill-v1.jpg)
- [31：dynamic needs trust](2026-10-22-live-e2e/screenshots/31-dynamic-needs-trust.jpg)
- [32：failure hook refresh parser](2026-10-22-live-e2e/screenshots/32-failure-hook-refresh-parser.jpg)
- [40：repaired dynamic v1](2026-10-22-live-e2e/screenshots/40-repaired-dynamic-v1.jpg)
- [41：repaired skill v2, same process](2026-10-22-live-e2e/screenshots/41-repaired-skill-v2-same-process.jpg)
- [42：repaired hook v2, same process](2026-10-22-live-e2e/screenshots/42-repaired-hook-v2-same-process.jpg)
- [43：invalid skill refresh rejected](2026-10-22-live-e2e/screenshots/43-invalid-skill-refresh-rejected.jpg)
- [44：invalid skill restored, same process](2026-10-22-live-e2e/screenshots/44-invalid-skill-restored-same-process.jpg)
- [45：dynamic hook denies read](2026-10-22-live-e2e/screenshots/45-dynamic-hook-denies-read.jpg)
- [46：project revocation stops admission](2026-10-22-live-e2e/screenshots/46-project-revocation-stops-admission.jpg)
