# 怎样评测一个 Agent Harness

这里是公开指南与本项目的阶段记录。最重要的区别是：**模型会回答问题，不等于一个Agent能稳定交付；一次工具回归通过，也不等于编码任务已经通过。**

建议按顺序阅读：

1. [入门：题库、框架与可靠性](primer.md)：用业务语言理解在测什么、哪些分数可比较。
2. [我们的评测方法与执行步骤](methodology.md)：控制条件、运行、验收、成本与失败怎样记录。
3. [第2轮真实结果](harness-certification-2026-10-28/round-02.md)：Go D补丁官方1分，但CLI关停失败、完整交付分0；是单题开发回归，没有同期重跑Pi。[第1轮](harness-certification-2026-10-28/round-01.md)的旧B0/Pi1独立保留。
4. [第3轮工程验收](harness-certification-2026-10-28/round-03.md)：R1b两进程长链已通过，Bash长命令与Linux孤儿回收有局部证据；当前Darwin组合已通过fullverify及5项tagged验收；Linux平台门和更多真实任务分别记录。
5. [当前发布审核](../../scripts/harness_eval/reports/release-review.md)、[版本迁移](harness-certification-2026-10-28/version-migration.md)与[辅助策略审计](../../scripts/harness_eval/reports/auxiliary-policy-audit.md)：当前基于最新main，公开实际配置、摘要和原生工具策略的差异。
6. [分轮验收计划](harness-certification-2026-10-28/plan.md)和[现有覆盖与缺口](harness-certification-2026-10-28/coverage-baseline.md)：后续工具、压缩、恢复和新任务如何逐步检验；计划不是完成声明。

[可复现脚手架](../../scripts/harness_eval/README.md)包含自有执行器、合同测试、依赖锁和固定任务元数据。[AA参考核查](../../scripts/harness_eval/reports/aa-reference.md)解释固定轨迹性能回放与真实闭环能力的区别。更广的资料搜集另保留在本地研究记录，当前公开页优先保证方法、代码与实际阶段结果清楚对应。

本项目以同一模型比较Go/Pi，保留各自原生工具、session和压缩，并公开实验profile的调整。当前main单任务profile显式关闭默认开启的自动个人记忆，避免跨任务状态干扰；small_model为空，Go不做LLM摘要，需要时采用截断降级。D的Nav开发回归已有独立官方成绩，不继承旧B/Pi分数，也不据一题宣布整体胜负。R1b的合成长链机制已有独立验收，不能当真实模型长项目成绩；当前Darwin组合的全量校验及长任务tagged验收已通过；Linux原生ARM局部验证不能替代amd64/Rosetta，更多未见题与真实模型长项目仍待完成。
