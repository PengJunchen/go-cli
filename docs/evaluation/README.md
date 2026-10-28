# 怎样评测一个 Agent Harness

这里是公开指南与本项目的阶段记录。最重要的区别是：**模型会回答问题，不等于一个Agent能稳定交付；一次工具回归通过，也不等于编码任务已经通过。**

建议按顺序阅读：

1. [入门：题库、框架与可靠性](primer.md)：用业务语言理解在测什么、哪些分数可比较。
2. [我们的评测方法与执行步骤](methodology.md)：控制条件、运行、验收、成本与失败怎样记录。
3. [第1轮真实结果](harness-certification-2026-10-28/round-01.md)：旧基线Go B在Navidrome官方0分、Pi为1分；只有一题，不能据此宣布整体胜负。
4. [当前发布审核](../../scripts/harness_eval/reports/release-review.md)与[版本迁移](harness-certification-2026-10-28/version-migration.md)：当前基于最新main，保留上游能力，只移植仍存在的Thinking缺陷修复与通用CLI回归。
5. [分轮验收计划](harness-certification-2026-10-28/plan.md)和[现有覆盖与缺口](harness-certification-2026-10-28/coverage-baseline.md)：后续工具、压缩、恢复和新任务如何逐步检验；计划不是完成声明。

[可复现脚手架](../../scripts/harness_eval/README.md)包含自有执行器、合同测试、依赖锁和固定任务元数据。[AA参考核查](../../scripts/harness_eval/reports/aa-reference.md)解释固定轨迹性能回放与真实闭环能力的区别。更广的资料搜集另保留在本地研究记录，当前公开页优先保证方法、代码与实际阶段结果清楚对应。

本项目统一用同一模型比较Go/Pi，保留各自原生工具、session和压缩，并公开实验profile的调整。当前main单任务profile显式关闭默认开启的自动个人记忆，避免跨任务状态与后台请求干扰；它尚无新的官方任务成绩，不继承旧B/Pi分数。真实模型批次、完整校验和长任务验收的状态以各阶段报告为准。
