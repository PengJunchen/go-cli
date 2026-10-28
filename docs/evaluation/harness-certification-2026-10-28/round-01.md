# 第1轮阶段报告：接入检查与Navidrome首对尝试

历史运行日期：2026-10-28；版本边界更新：2026-10-29。状态：**B首次结果已完成；C只有无费验收、未评分；新main变体D待重新冻结验收**。本报告中的统计仍是旧B/Pi首对数据，不能当新main性能。方法与后续门槛见[分轮计划](plan.md)、[版本迁移](version-migration.md)，可复现源包见[评测入口](../../../scripts/harness_eval/README.md)。仅发布安全标量、哈希和故障类别，不含答案、完整模型输出、原始轨迹或隐藏验收。

## 任务、环境与版本

固定任务ID：`instance_navidrome__navidrome-1e96b858a91c640fe64e84c5e5ad8cc0954ea38d`。业务上测Navidrome的受信反向代理认证及相关兼容行为；是否通过由官方验收决定。

官方Pro V2资产提交为`66f92766bba642462d4bbe5479e83f91f9211862`。本机为Mac ARM，任务用Linux amd64仿真，1CPU/4GiB、3000秒，串行执行；10GiB只声明而未硬限。保留双方原生prompt、工具与压缩，agent使用internal网络和固定模型relay，评分另起fresh镜像，按官方public网络执行官方patch replay和verifier。使用自有Docker薄驱动接官方Harbor任务资产，**不是完整官方榜单运行条件或全榜分数**。

双方使用同一模型`codex/gpt-6.1-sol`、有效`high`，主调用输出上限16384，原生内部较低上限允许且记录；每次尝试最多80个上游请求，含重试/摘要/子Agent。没有整题token/美元硬限，未知usage会阻断后续请求。Magpie为共同网关。seed为20261028，实际先Pi后Go；各一份全新容器、一次CLI调用，没有挑最好一次。

| 变体 | 含义 | 本轮状态 |
|---|---|---|
| A | 原始源码`379134d679dcae5c996b730eeae464fdb61fe853` | 无费接入检查发现Eino路径漏传thinking；未做真实基准，**不计任务分数** |
| B | A加通用thinking传参修复，源码差异SHA下列 | Nav首次真实尝试，保留原始失败与官方0分 |
| Pi | `@earendil-works/pi-coding-agent@1.4.0`，Node22.22.2 | 同题官方1分 |
| C | 旧379上的Thinking+Git只读资源生命周期修复 | 已有无费单/并行CLI证据，未做真实模型任务评分；不混合版本均分 |
| D | main5736e88加本轮Thinking修复及评测集成 | 发布commit/binary与新base实际验收待冻结，无任务成绩 |

## 已完成的结果

| 指标 | Go B | Pi1.4.0 |
|---|---:|---:|
| 官方fresh clean reward | **0** | **1** |
| Agent墙钟秒 | 21.13 | 711.89 |
| relay观测的模型请求耗时累计秒 | 19.01 | 407.21 |
| 独立评分墙钟秒 | 224.72 | 351.17 |
| CLI退出码 | 1 | 0 |
| 上游请求数 | 3 | 35 |
| HTTP200数 | 3 | 35 |
| 输入token累计 | 24,687 | 1,145,143 |
| 其中缓存输入token累计 | 16,128 | 1,093,504 |
| 输出token累计 | 207 | 9,752 |
| reasoning token累计 | 0 | 1,958 |
| 缺失usage的请求数 | 0 | 0 |
| wire契约拒绝数 | 0 | 0 |
| 任务超时 | 否 | 否 |
| 美元费用 | 未取得（null） | 未取得（null） |

Agent墙钟包含其模型调用、工具、持久化与运行时开销；relay模型耗时是每个请求开始到结束的累计，属于Agent阶段内部观察，**不能再与Agent墙钟相加**，也不能简单用差值认定纯工具时间。评分耗时单列，镜像/依赖准备不计入Agent阶段。本轮未采集可信的逐token TTFT，不补造该值。

输入量累计了多轮重复上下文，缓存输入是其中一部分；不要把缓存token再加到总输入。reasoning按上游usage字段原样累计，不再加到输出制造新总量。B的reasoning计数0不改变已验证high的请求事实，也不证明远端权重或内部推理过程。双方缓存命中不同，本轮没有证明远端冷缓存完全相同，不能据这些token直接推导美元成本或作等工作量速度排名。

**Go较短、较省token源于提前失败，不能称效率胜出。** 四个并行工具虽返回success receipt，但`git_status`未向核心提供已收尾的完成证明，核心按既有规则停止。B在业务源码修改前退出，候选仅有运行审计文件；这是有效的Harness运行时失败。Pi完成了业务修改，fresh官方验收通过。该结果支持修复工具生命周期合同，不支持“同一模型不具备解题能力”的结论。

旧B源码上的独立无模型临时仓库复现进一步定位status/diff/log的prepared completion缺口；这是历史定位，不代表PR9后的main仍存在该缺口。根因与安全边界见[独立运行时审查](../../../scripts/harness_eval/reports/runtime-quiescence-review.md)，Thinking修复接入证据见[传参审查](../../../scripts/harness_eval/reports/thinking-fix-review.md)。修复不得放宽核心fail-closed或直接给任意GitTool授予完成证明。

## 绑定哈希

| 输入/产物 | SHA256 |
|---|---|
| B源码差异 | `4fb22ef54bb55d89b22c3e8d415fc0c14eb863d8af3381c1f0d2e0954d43639a` |
| B Linux amd64二进制 | `749416f8a5bc20eb8d875aa54ed413a09e9c4602dbace04b51f3d294bd0777b5` |
| Pi npm锁文件 | `5d508d747bc5d893fce2d247b845aa3861e188d4b02aedace36b2f6c84988123` |
| 任务指令 | `2dda914e5953615aec5a09c7efe233da46db297ee4d0a0235091b776168d50e8` |
| task.toml | `64ee1ca1ac83ec93c769cf6478631a9efc641aee8ede6c37935b7ff37b0df637` |
| 任务镜像leaf digest | `b45146e8c3ce77bdc2294fdaa8fa1d7ae2eb8680b3c832e5c41b24de87910d6f` |
| 官方patch replay脚本 | `5c8b344de37164e9723f1724c2573013957fb644c3913a64c12c1718c737d876` |
| Go候选patch | `dfcf7ff31cf8cf08effe4c75101bd9373253b545bfa7bbd8b48da7229942bdde` |
| Pi候选patch | `0959d6ff5285c667cf51e5575a2d57a24df6d15909eb6ad340458ffd1e6dbe4e` |
| Go原result.json | `a9160a856928a64ae8cda43dedf91dfd8ec44873aa975fa893501567476e4520` |
| Pi原result.json | `d9481f41f69b6e45ec47a58e235217cfb39651ffa1dc8cb94be3c82028ff8b29` |
| Go独立grade.json | `7df2e4b17010f119890afc446e27e457b605a557876af0b8223e087093ebe158` |
| Pi独立grade.json | `adf1e8b9690bcb186bb03bce728c3aa5631d645963d6bdbb88cf5fccff41483c` |

双方五个verifier文件hash相同；clean image与only model patch replay均确认，patch应用成功。哈希用于绑定本地证据，不代表远端服务身份已被独立认证。原始私有工作材料保留于相邻工作区`go-cli-eval/jobs/nav-pilot-20261028/`，公开源包不包含答案/隐藏测试；不以本地目录作为GitHub超链接。

## 下一步与未完成项

- 旧C不再补跑付费成绩；新main已有更广Git/state/worker/子Agent等修复，D先重新冻结、跑现有无费二进制/受管旅程/120轮及目标负例，再做Nav开发回归。不能把旧B故障当新main仍存在。
- Nav仍为开发回归题；原冻结Flipt/Vuls待D门禁及fresh环境控制后作未见验证，不重跑已知故障B，也不因迁移把Nav重标holdout。
- [额外10题](pro-v2-expansion-lock.json)排除原三题，3开发+7留出，分批解封；尚未运行，不列入本轮成功率。
- 旧Flipt预检因暂停导致计时失效，已归档infra-interrupted并清理，reward为null；不算empty0、正式能力失败或已通过环境门。
- 长程、压缩续接、同题三次与固定轨迹性能回放仍按[六轮计划](plan.md)逐步实施。本轮单题不提供总体显著性结论，不报告不存在的长任务认证或全量通过。
