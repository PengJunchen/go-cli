# 第4轮：Flipt 单题配对交付与文件工具链验收

2026-10-29。本题两边均为**官方通过1/1、CLI退出0、交付指标1**。Go这一次用时更短；费用仍未知，不能从总token较少推断更便宜。这是一个预选任务、固定顺序、每边一次的工程pilot，不是总体排名。

[白名单结果JSON](round-04-results.json)保留可复算指标和来源SHA，不发布机器本地路径、账号、响应ID、原始提示词或题解。[第3轮](round-03.md)和旧Nav不同版本结果原样保留。

## 同一场考试，分别独立交付

题目为SWE-bench Pro V2的公开Flipt任务 `instance_flipt-io__flipt-b2170346dc37cf42fda1386cd630f24821ad2ac5`。题库提供需求、起始项目与验收资产；自有runner负责环境、调用原生CLI、计量和收取补丁；评分器在另一个干净容器只重放该补丁，用官方PatchReplayAgent/verifier判卷。使用的是固定官方资产加已披露的薄Docker driver，不能称为完整官方排行榜运行。

模型均为`codex/gpt-6.1-sol`，实际请求均为high、主输出上限16384。双方共同使用3000秒任务时限、最多80次上游请求、1CPU/4GiB、同镜像及fresh HOME/工作区。请求总账包含原生重试、子Agent等调用；缺usage时阻止后续请求。任务容器仅可访问固定模型relay，官方评分在独立公共网络环境执行。10GiB是任务声明，当前没有硬磁盘限额；任务镜像是Mac ARM Docker上的linux/amd64仿真环境。

顺序在执行前固定为**Go→Pi，各一次attempt和一次fresh评分**。没有人工补提示、向另一边转交解法、评分反馈后续跑或多次取最好。环境事先通过空补丁0、黑箱参考补丁1的校准；参考解只用于隔离环境验收，不用于提示或调优。

共同控制外部条件，同时保留并披露原生策略差异：

| 策略 | Go | Pi |
| --- | --- | --- |
| 上下文 | 128k配置；micro-first，无small model时原生截断回退 | 模型元数据500k，原生压缩与余量策略 |
| Bash窗口 | 宿主配置300秒，模型schema仅command | 原生每调用可选timeout，本题实际指定10–60秒；未施加共同300秒单命令限额 |
| 工具与会话 | 保留核心原生工具、子Agent、session及compaction | 保留自身原生工具与会话策略 |
| 跨任务状态 | Go personal memory显式关闭，双方fresh HOME；按受控profile不加载可选extensions/skills/MCP | 同受控隔离原则，不要求存在Go同名模块 |

这比较的是两套冻结配置下的产品，不能说所有内部策略完全相同，也不代表全部默认可选功能已验收。本轮两边容器都没有init，**Go outer supervisor明确OFF**。

## 结果与用量

| 指标 | Go | Pi |
| --- | ---: | ---: |
| 官方补丁通过/尝试 | 1/1 | 1/1 |
| CLI退出 / delivery_v1 | 0 / 1 | 0 / 1 |
| Agent墙钟，秒 | 432.108 | 1115.276 |
| 独立评分墙钟，秒 | 751.240 | 782.097 |
| 真实上游请求 | 28 | 34 |
| 累计input tokens | 469726 | 706749 |
| 累计output tokens | 5950 | 4692 |
| input+output | 475676 | 711441 |
| cached input（input子集） | 377344 | 650496 |
| 未缓存input（input−cached，派生值） | 92382 | 56253 |
| reasoning（output细分） | 430 | 811 |
| 缺usage请求 | 0 | 0 |
| 美元费用 | unknown / null | unknown / null |

Agent墙钟是原生CLI执行与收尾，评分耗时单列。累计input包含多轮重复上下文，cached input和reasoning不再加入总量。Pi的未缓存input与output反而更少，定价和实际结算没有导出，因此**不能宣布Go更省钱**。

`delivery_v1`是预登记的官方正确性与CLI/协议终态指标，不是跨Harness的全部资源闭合认证。两边均正常交付，原生过程证据的格式仍各不相同。

## 为什么耗时不同：可见流程，不是因果结论

Go保留4个已派发executor error及2个未派发Bash error，主流程最终恢复交付；子会话错误没有被抹去。后验审查区分了迭代上限、覆盖保护、缺少命令、依赖网络失败，以及不受schema支持的参数。这些类别不能一概称为产品bug，错误条数也不是可靠率。

Go最长一次Bash从execution到quiescent约220.429秒，最终因依赖下载/DNS网络限制返回error，**不是220秒测试通过**。后续自选窄化检查有成功记录，也不等于整个仓库测试通过。Pi有6个测试调用明确在60秒超时，另1个日志轮询在55秒超时；后台启动或轮询返回成功同样不能代替测试通过。两边都没有可据以宣称全仓测试成功的记录。

共同镜像并不保证最终shell和环境相同：两边环境传递/启动方式不同，双方均有rg缺失记录，正式Go最终PATH/cache未导出，因此不能把缺命令或依赖下载失败归因于某个已确认环境变量；另做无费对照，本轮分数与既有错误计数不变。

relay记录的API请求生命周期累计为Go201.276秒、Pi237.164秒；本题这些区间没有重叠。这不是纯思考时间，其差约35.888秒，不能解释约683.168秒的CLI墙钟差。工具、局部验证、重试、轮询与收尾共同变化，现有口径不能硬相加成完整因果分解；创建子Agent也不等于本题实现了并行推理。

Go的51个已派发action各自有资源闭合与typed resolution，未闭合为0；信号前collector为0 observed/0 unknown。Pi同口径观察到31个Z、0 unknown：它们是已退出但未回收的进程表记录，**不是31个活后台任务**。缺少完整tool-call映射，不能把它们全部归因于超时。最终所属容器和网络均清除。没有因此追溯改分，也不把Go的0归功于本轮关闭的outer supervisor；Pi无需伪造Go私有proof格式。

## 版本、工程验收与下一步

真实pair仍用`f4a6b12…`产品源、Linux/amd64 binary `914d1e16…`，runner为`0962aa7…`、25源manifest `f14539b5…`。后来的`6b56970…`只修改Linux测试夹具，native CI为13/13通过；生产文件集合一致，独立重建binary字节也相同。发布binary来源仍是f4，CI源码是6b，不能改写来源或声称CI直接执行了该发布binary。原f4失败记录保留。

R2新增文件链已在Darwin原生race CLI独立验收：9个实际广告工具调用，CLI约3.013秒，9个action资源闭合、pending0；真实错误edit→回读未改→正确edit→最终独立磁盘验收通过。初次v3因fixture使用不支持的YAML空flow列表，在0个HTTP请求时配置失败，原件保留；v4省略冗余空项后，以新源、新OUT唯一执行通过。首失效不计工具能力失败或模型attempt。该YAML子集不支持flow空列表，空项可省略，或使用JSON配置；本轮没有扩展生产解析器。

随后主工作树`6b56970…`加3份已审R2测试的本机整合`make verify`已通过：502.230秒、exit0，主race门9749 started/9740 pass/0 fail/9 skip、31包，另log10/leak6通过，lint0 issues、vet/build/scan通过，可选goimports跳过。1365份Go源SHA及mode前后一致，尚不编造新的提交或远端CI身份。整合verify自动运行R2用例并通过，子项约7.32秒**含构建/race开销**，与此前独立纯CLI约3.013秒来自不同运行和口径，不混作模型性能或重复能力样本。

这仍只是R2文件子链，不是全部工具或Git链认证。R1b的120轮、跨进程续接与多次压缩是synthetic工程夹具，不是真实模型自主长项目。本题双方实际压缩执行次数均未知，不能补0。Vuls本轮没有拉取新镜像或派发新模型，fresh校准仍待做；额外10题锁保持未解封，真实长项目仍pending。

后续按[计划](plan.md)逐批验收。当前只公布“两边本题均正确交付，以及本次时间、用量和资源观察”，不据一题一次推总体胜负、重复稳定性、p95、成本排名或R4全面认证。

当前执行收敛Go-only工程修复、全部Go测试和新增标准题；Pi不再阻断Go门，不继续为Pi排障，未来没有同期Pi时按单臂、非paired记录，历史Flipt配对保持不变。

## 可审计来源

原始安全摘要与独审保留在本地证据库，公开文档仅发布以下内容SHA及[白名单JSON](round-04-results.json)。不发布原始提示、完整轨迹、补丁、隐藏评分输出或机器路径。后验流程note写入时Pi评分未终态，最终分数以之后的Pi终态独审为准，旧快照不回填。

| 证据 | SHA256 |
| --- | --- |
| 匹配pair安全摘要 | `3c27166b981fb8e1482eb00ac5aee1a78626355028039d0d3859d14c49385a59` |
| Go官方评分独审 | `1f45bb45b36181dad11afadf8436a29f3e864cc0a4055887692ffcbb165f047c` |
| Pi官方评分及pair终态独审 | `2a5bdfe476702d92412c1050de3c0d62ad935de76cbf6c4a2f3d1c5ad8db37c2` |
| 后验流程审查 | `fe05544c0c30f4e6d3a759b20f4516e3136f780923de7246c5fbb0f9899f75cb` |
| shell/env源审计（有效态未知） | `b4929f1b1a95de015b0b6edd137401e8bb3cce330644a5f615454e43f0bb20a2` |
| R2文件链v4独审 | `1e30415df109272419b2454509363e9ff3fab295295e0924c948fe30851cb4b6` |
| 整合fullverify安全摘要 | `cec0a0ffd1500131dab9da299d5e758847a10adcb0d21ccc14acb2506ccc30dd` |
| 整合fullverify日志 | `c24b0936ddaf2feb9d9ed20ce60408ee76041208f6a00513fae508b15efa9b0e` |

公开执行入口见[评测源包](../../../scripts/harness_eval/README.md)与[执行约束](../../../scripts/harness_eval/runner/RUNNER.md)。本轮未增加其它收费任务或重复样本。
