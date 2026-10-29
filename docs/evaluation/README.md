# 怎样评测一个 Agent Harness

这里是公开指南与本项目的阶段记录。最重要的区别是：**模型会回答问题，不等于一个Agent能稳定交付；一次工具回归通过，也不等于编码任务已经通过。**

建议按顺序阅读：

1. [入门：题库、框架与可靠性](primer.md)：用业务语言理解在测什么、哪些分数可比较。
2. [我们的评测方法与执行步骤](methodology.md)：控制条件、运行、验收、成本与失败怎样记录。
3. [第4轮：Flipt单题配对结果](harness-certification-2026-10-28/round-04.md)与[白名单JSON](harness-certification-2026-10-28/round-04-results.json)：两边均官方1/1、CLI正常交付；Go本次用时较短，费用未知。公开原生策略差异、过程错误与解释边界，不据一题排名；另有R2文件子链工程验收。
4. [第2轮历史结果](harness-certification-2026-10-28/round-02.md)：Go D补丁官方1分，但CLI关停失败、完整交付分0；是单题开发回归，没有同期重跑Pi。[第1轮](harness-certification-2026-10-28/round-01.md)的旧B0/Pi1独立保留。
5. [第3轮工程验收](harness-certification-2026-10-28/round-03.md)：R1b两进程模拟长链已通过，Bash长命令与Linux孤儿回收有局部证据；该轮Darwin组合通过fullverify及5项tagged验收。历史快照不回填后续结果。
6. [发布审核记录](../../scripts/harness_eval/reports/release-review.md)、[版本迁移](harness-certification-2026-10-28/version-migration.md)与[辅助策略审计](../../scripts/harness_eval/reports/auxiliary-policy-audit.md)：公开实际配置、摘要和原生工具策略的差异，各记录按其冻结版本解读。
7. [分轮验收计划](harness-certification-2026-10-28/plan.md)和[现有覆盖与缺口](harness-certification-2026-10-28/coverage-baseline.md)：后续工具、压缩、恢复和新任务如何逐步检验；计划不是完成声明。

[可复现脚手架](../../scripts/harness_eval/README.md)包含自有执行器、合同测试、依赖锁和固定任务元数据。[AA参考核查](../../scripts/harness_eval/reports/aa-reference.md)解释固定轨迹性能回放与真实闭环能力的区别。更广的资料搜集另保留在本地研究记录，当前公开页优先保证方法、代码与实际阶段结果清楚对应。

本项目以同一模型比较Go/Pi，保留各自原生工具、session和压缩，并公开实验profile的调整。Flipt本轮使用受控`main-bash300-personal-memory-off-v2`：Go personal memory关闭、small_model为空、Bash窗口300秒、outer supervisor关闭；Pi保留自身timeout及context策略。产品binary来源f4，6b仅修测试夹具，native CI13/13与产品字节等价分别留证。R1b模拟长链和R2 Darwin文件链已有独立验收，仍不能当自主长项目或全部工具认证；6b加3份新R2测试的本机整合fullverify已通过，主门9740 pass/9 skip，不能冒称新提交远端CI。Vuls、额外10题解封和真实模型长项目尚待后续批准。

当前执行收敛为Go-only工程修复、全部Go测试和新增标准题；Pi不再阻断Go验收，也不继续为Pi排障，未来没有同期Pi的结果明确标为单臂、非paired，历史Flipt配对不变。
