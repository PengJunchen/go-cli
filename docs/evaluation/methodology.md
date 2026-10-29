# 本项目如何公平评测：方法与执行步骤

目标是回答：**在固定模型、任务与预算下，Go CLI和Pi各自能否正确、稳定而经济地交付？** 单模型小样本只支持这个条件内的结论，不推导整体冠军。

## 一、冻结一张实验卡

开始前固定模型/端点、reasoning、输出cap、任务/镜像/评分器版本、Go和Pi身份、外部资源、网络、总预算、重复次数、先后顺序、成功定义与失败分类。固定共同预算上限，不强迫实际消耗相同；节省token本来就是价值。固定seed或temperature=0也不保证远端确定性。

主比较保留各自原生prompt、工具、session与压缩。去掉所有差异会改变被测产品。需要解释组件作用时，另开一次只改一个因素的消融实验。当前main单任务profile显式关闭自动个人记忆、fresh容器不挂用户HOME，公开说明它不同于产品默认；跨任务记忆以后另测。

当前脚手架用同一 `codex/gpt-6.1-sol`、high、主cap16384；1 CPU/4GiB、3000秒、80次上游请求。所有重试、后台与子Agent计入请求总数；缺usage停止后续请求，没有token/美元硬限。磁盘10GiB仅声明，未硬限。Samplings记录实际presence，不替服务端猜默认值。实际profile与版本以[执行说明](../../scripts/harness_eval/runner/RUNNER.md)和每次manifest为准。

模型目录声明500k，但Go没有读取容量元数据，当前保留CLI128k/micro_first及其约0.8触发预算；Pi内部使用500k/16kreserve。共同API、输出与墙钟条件不等于两套Harness内部上下文策略相同。这是本轮原生比较的公开差异，后续统一容量应另冻profile，不静默调整；见[上下文审计](../../scripts/harness_eval/context-budget-audit.md)。

## 二、先验证考场，再花模型费用

1. 按[源包入口](../../scripts/harness_eval/README.md)导出新外部工作区、安装锁定依赖、取得固定官方资产与digest镜像。
2. 跑合同测试、网络probe和本地mock的真实CLI单/并行工具链。断言工具结果确实进入后续请求、每个动作有资源完成证明、session无悬而未决动作；不只匹配DONE。
3. 对任务做空解失败、reference成功的隔离黑箱控制。reference/gold与隐藏验收不能进入Agent或修复者上下文。
4. 官方源、任务、镜像与verifier必须验证或明确假设边界；仅记录哈希不等于检查它符合官方锁版本。资产门未通过就不发模型。

无费mock的token是合成值，不能算模型成本或能力分数。镜像恢复、包安装与编译不计为模型解题耗时，但环境准备的总业务成本可另列。

## 三、运行原生Agent，独立验收产物

每次用全新工作目录和session，一次CLI调用；任务原文不加题目专用提示。Go/Pi先后顺序按冻结seed交错，避免某个版本总吃到相同时间或缓存条件。冷/热缓存预先定义；换HOME不能保证清除服务端缓存。

任务结束后保留失败、超时、部分patch和usage。将完整patch重放到fresh同一镜像，运行原官方验收，避免Agent改过的解释器/测试污染判卷。CLI exit0不代表任务通过；官方分数和失败原因分别保存。发现运行时bug先安全收尾与评分，再以通用合成题TDD修复，另记版本，不能覆盖原失败。

## 四、怎样多跑几题而不自欺

按仓库、语言、任务类型和预先定义的难度层抽样；先有开发题，再冻结留出题。开发反馈可用于修复，但该题之后不再是新版本的未调优验证。每批冻结版本，完整保留未完成批次；不能只发表已经通过的题。

配对同题重复，公布所有attempt。pass@1是平均一次表现，pass@k是至少成功一次，pass^k强调每次都成功，三者不能互换。正式样本量由pilot方差和预定的最小业务收益决定；不武断声称30题一定显著。区间按任务聚类、保留配对结构，小样本不追求几分差的名次。第二阶段可做两Harness×两模型，检查模型与Harness交互。

## 五、把业务结果与原因并排报告

| 字段组 | 最少保存 |
|---|---|
| 身份 | task/repeat/arm、profile、source revision/patch/binary、模型有效请求、task/image/verifier/runner哈希 |
| 结果 | 官方reward、完成状态、退出码、失败分类、人工接管或越权 |
| 资源 | 共同collector信号前进程快照、每PID状态与身份、未知覆盖率；与CLI退出/工具内部proof分别保存 |
| 耗时 | 完整attempt墙钟、模型累计请求、评分耗时分别列；timeout按截尾解释，成功任务耗时另列 |
| 用量 | input/output/reasoning/cache-read/cache-write、缺失覆盖率；未知填null，不能填0 |
| 费用 | 所有失败/重试/摘要/子Agent的实际账单或明确估计；摊销每成功交付费用=全部费用/成功数 |

同一失败可有官方0分，同时原因属于环境、预算、协议、Harness运行时或纯解题错误；不能因不利就删除，也不能把考场损坏当模型不会。LLM裁判应匿名、锁版本、用人工抽审校准；无法复核的项保留不确定性。

当前自定义delivery_v1采用已经约定的CLI退出与协议条件，保留运行时失败的主分母；它不等于跨Harness完整资源闭合认证，CLI exit0也不能替代操作系统资源证据。历史B/Pi/D外部collector信号目标数分别为0/22/2，信号前/proc状态全部unknown；不把这些目标数解释成活进程数、僵尸数或证明某Harness资源管理更强，原始0/1与attempt均不改。

未来使用共同 `processes-before-cleanup.json` 在STOP/KILL前采PID/state/PPID/PGID/starttime/comm，失败观测明确unknown，作为单独资源维度报告。新观测链已有52个导出合同及自有合成容器检查；历史缺失不回填，若将资源条件加入成功定义，应先预注册新的指标版本。实际例子与边界见[第2轮](harness-certification-2026-10-28/round-02.md)。

## 六、下一阶段怎样做

先在已提交新main上做无费工具写链、Linux Bash/取消与持久化；再检验实际多次压缩、重启、事实保留和不重复副作用；最后才用新任务检验真实长程交付。AA固定轨迹回放单列服务性能，不能替代真实闭环评分。具体门槛见[分轮计划](harness-certification-2026-10-28/plan.md)与[长任务设计](harness-certification-2026-10-28/long-task-acceptance-design.md)。

发布时至少给出固定配置、抽样规则、全部attempt清单、官方评分、失败/超时、用量覆盖率、成本算法、执行资源和剩余限制。当前旧B/Pi的首题结果见[第1轮报告](harness-certification-2026-10-28/round-01.md)；当前新main的门禁状态见[发布审核](../../scripts/harness_eval/reports/release-review.md)。不把计划、mock或单次成功写成全部认证完成。
