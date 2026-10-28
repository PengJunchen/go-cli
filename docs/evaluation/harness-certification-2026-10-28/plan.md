# Harness 分轮验收与公开评测计划

创建：2026-10-28；发布基线更新：2026-10-29。Designer交付；每轮由Builder实现、Reviewer按证据放行。当前发布工作基于已合并PR9的main `5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1`，不再基于旧379开发历史。此计划不修改infra的任务表、里程碑、规则或CI。目标是逐步回答：工具是否可靠、长任务能否续接、同一模型下是否正确交付，以及为交付花了多少时间和token。六轮有依赖，不能用一次工具成功替代整套认证，也不一次性启动大批收费任务。

## 当前事实与版本边界

- 模型固定为 `codex/gpt-6.1-sol`，双方有效 `high`；主调用输出上限 16384，原生内部较低上限允许但记录。Magpie只作为共同网关，核对实际请求参数即可。
- 原始版本 A 的 Eino 请求路径漏传 thinking；无费 fixture 先发现，通用修复后冻结 B。不能声称 A 已做真实基准。
- Navidrome 首次真实尝试：**B：官方 clean reward 0；Pi：1**。B 运行21.13秒、3个成功HTTP请求后因 `git_status` 缺完成证明退出，未修改业务代码；Pi运行711.9秒、35次调用后交付。独立评分分别224.72/351.17秒，均fresh镜像、官方 patch replay、相同verifier资产。B提前退出不代表效率胜出。
- C是旧379基线上的Thinking+Git只读生命周期修复变体；无费真实CLI单/并行fixture已有绿结果，**没有真实模型任务评分**。旧本地checkpoint `65cf537`保留调查材料，不作为新main的发布基线，也不推送旧开发历史。
- PR9已包含更广的受管Git写/读、state、worker、子Agent、MCP、post-tool hook、会话管理与PTY修复。旧“未实现/不受管”判断只对旧版本成立；先复用新main已有测试，不重做已合并能力。来源与重新验收门见[版本迁移说明](version-migration.md)、[当前覆盖盘点](coverage-baseline.md)。
- 新变体D为**main5736e88+本轮Thinking修复及评测集成**；完整发布commit、dirty diff/binary SHA和无费验收尚待冻结，当前不得称D已评分。后续若有新通用修复，另记E或新版本，不能混用B/C/D成绩。原冻结Flipt/Vuls及额外10题未解封，待D实际门禁通过后才运行。
- 旧单工具复现证实成功receipt不等于prepared completion；该安全原则继续适用，但不将旧缺口移植成新main事实。Nav后续属于开发回归，不能覆盖B首次失败。

公开复现入口为主仓库的[评测源包](../../../scripts/harness_eval/README.md)、[执行约束](../../../scripts/harness_eval/runner/RUNNER.md)、[AA分层核查](../../../scripts/harness_eval/reports/aa-reference.md)与[首轮脱敏报告](round-01.md)。本地完整证据另保留于相邻目录 `go-cli-eval/jobs/nav-pilot-20261028/`，不发布原始日志、题解或密钥。

## 全程通用规则

1. 首批采用受控profile `main-5736e88-personal-memory-off-v1`：双方fresh HOME，关闭个人memory及第三方extensions/skills/MCP，保留核心原生工具、session与compaction路径，子Agent等实际可用能力按导出清单记录。它偏离产品全部默认配置，不能宣称所有可选能力均已评测。共同控制外部模型、预算、容器、工作目录初态、网络与评分；不能删掉故障核心工具后仍冒称完整原生工具比较。
2. 每次尝试记录产品commit+未提交diff SHA、二进制SHA、Pi包版本/依赖锁、runner SHA、任务/镜像/verifier SHA、模型请求有效值、seed和实际“Go先/Pi先”顺序。开始后不得覆盖原job。
3. 首批Pro profile沿用3000秒、1CPU、4GiB、最多80次上游请求（重试/压缩/子Agent全计），未知usage后冻结后续请求。agent仅能经固定relay访问模型，verifier按官方public网络独立评分。10GiB是任务声明，当前Docker overlay未硬限，须披露。Mac amd64仿真、总约7.65GiB Docker内存，重型容器串行。
4. `api_contract_stop`、预算上限、未知测量、infra失效、Harness运行时错误与纯解题失败分开编码；官方任务分仍原样报告，不能删除不利attempt。中断预检不能当有效负对照。
5. 每批发现失败先停止发新模型请求，完成现有attempt的安全收尾/独立评分。先判是否通用工程故障；可修复的问题先红→绿→独立审查→新版本冻结→开发题回归，再开下一批未见题。纯模型能力不足不强造代码修复，也不读gold找提示。
6. 每个修复集及时做小粒度git提交与推送，附失败复现、验证和剩余限制；不得捎带保护文件、凭据、缓存、镜像层、私有prompt或未经脱敏的原始轨迹。独立Reviewer确认暂存范围；依仓库要求执行`make verify`，缺环境依赖/未执行步骤如实记录，不能写“全绿”。主协调者安排verify资源，避免与评分并行抢资源。
7. 切换base、memory/worker/子Agent/hook或模型路径后，重新跑真实入口门禁。新main产品默认自动memory开启，但**本次首批无费fixture及正式评测均显式`memory.enabled=false`、fresh HOME**，用于控制个人记忆；该差异写入profile/manifest。memory-on另设profile后测，不混分。所有实际发生的辅助模型调用仍纳入请求/usage总账，不能仅凭`auxiliary_model_configured=false`推断绝无后台调用；扩展/Skill/MCP的有效未加载状态也须在R0导出核对。

## R0：模型与测量接入

**进入**：冻结网关模型目录、双方CLI版本与外部profile；只接本地无费fixture。

**可执行AC**：

- 真实Go/Pi CLI都走安装后的入口，经审计relay完成至少两轮：工具调用→工具结果→最终答复。不是只测HTTP客户端或直接调用工具函数。
- 覆盖stream与非stream实际启用路径、原生/Eino实际启用路径；断言接受的每个主请求模型精确一致、有效high、主cap16384。内部call来源可辨识、较小cap原样记录。缺usage、错误模型、effort降档、重试越过80次都触发预期停止。
- 拒绝不合规响应/无授权转发；任务不能直连外网/宿主网关端口，relay不能CONNECT或任意URL代理。无费用fixture验证子Agent、摘要请求也被同一总计数覆盖。
- `run.json`与wire合计、失败分类、单调时钟耗时一致；缓存字段缺失为null，不伪记零；真实上游凭据不落任务容器/公开日志。任务中的合成评测key不具有直接上游访问权限。

**退出**：Reviewer在D实际发布commit上复现以上合同并冻结manifest。B/C旧证据不继承为D通过。任何契约失败不进入收费阶段。

## R1：新main能力回归与首次故障回归

**进入**：D的R0通过；保留B失败、Pi成功和C无费未评分原件。先运行PR9已有真实入口/受管工具旅程；只有新base仍失败时才写精确红复现。

**可执行AC**：

- 至少status/diff/log/branch/blame这些对外启用的只读Git工具逐项检查；确有同类缺口时作最小共因修复。所有前置检查与helper均受管，工具级聚合一次完成证明；不按名字、任意GitTool接口或receipt成功铸造证明。写Git/远程工具不自动继承只读路径权限。
- 真实CLI的单`git_status`→第二次模型调用→final通过；并行bash/read/status/read中各action分别有quiescent、非空proof、durable resolution，批次继续到后续模型调用。
- 自定义GitTool、普通注册/替换、未受审wrapper不能继承证明；Git filter子进程、leader退出但后代/pipe仍活、取消/超时/containment未知均保持fail-closed并正确收尾。race与目标CLI测试通过。
- D冻结且基础真实二进制、120轮及受管工具journey实际验收通过后，仅启动**一份Nav D诊断回归**，随后fresh官方评分；不再为旧C补跑付费成绩。B0/Pi1保留，D另行一行；若Pi未重跑，明确Pi为原冻结版本同题历史参照，不能伪写同期随机paired trial。

**退出**：代码合同全过，D真实Nav运行到安全完成并取得官方分数；若仍得0，先判明运行时/解题失败。确有可泛化工程故障才修复并另冻版本；纯解题失败如实记录，经批次诊断后可继续未见验证，不要求反复改产品直到刷过Nav。任务通过不替代取消/负例测试。

## R2：全部核心工具的真实多轮合同

**进入**：R1工程门通过。导出**实际广告给模型的工具清单**及注册来源，建立工具→正例/失败/取消/恢复覆盖矩阵，未测项不能标认证通过。

**可执行AC**：

- 文件链：ls/find/grep/read→write→read→edit→read→最终独立文件hash/内容断言；包含新增、权限失败、路径/symlink边界、输出截断和无匹配。不仅断言函数返回nil。
- Bash链：运行测试→读失败输出→改文件→重跑→final；并行只读与冲突写入分别验证规范顺序。长进程、后台后代、继承pipe、取消和CLI退出后，受管进程/reader/goroutine均收尾，重复执行无残留。
- Git链：状态/差异/日志/分支/blame→本地分支→提交→回读；checkout/reset/revert/merge/stash等启用工具在可抛弃仓库验证。push/fetch/pull只用本地bare remote；不向真实远端发变更，不以关闭工具冒充通过。
- deferred/load、MCP、子Agent、审批与extension中**实际启用的控制工具**也做真实协议多轮：发现→调用→结果进入下轮；拒绝、超时、取消传播、任务所有权/会话路由、重复调用/持久化重开有断言。无可用配置的可选工具标not configured，而不是pass。
- 每个本地工具至少一个完整正向多轮和一个故障分支；共因的取消/后代负例按执行机制分组。每组循环10次无泄漏；这是工程压力重复，不计为10次能力采样。

**退出**：矩阵100%覆盖本轮实际启用核心工具，已知不完整能力单列阻断项；无费真实CLI、race和Reviewer负例通过后再开收费批次。最小先做文件/Bash/Git三链；Git优先复用PR9已有local+bare remote journey，必要时只补独立tree/ref验收。失败停在该链修复，不等所有工具一次铺开。

## R3：压缩、长对话与续接

**进入**：R2通过；预先冻结独立合成fixture、验收清单和中断点，不使用正式题gold或Pi解答造fixture。

**可执行AC**：

- 无费100+turn scripted-model fixture，经真实CLI循环执行；显式缩小测试上下文阈值，确保实际触发至少两次压缩。日志须有触发原因、前后token估计、摘要成功/失败/重试，不把“跑得久”当已测压缩。
- 在前、中、后阶段植入不同事实、待办和文件位置，由独立最终验收检查保留；至少包含摘要失败回退、上下文截断、大工具输出，不能只检查摘要文本存在。
- 在固定checkpoint、工具receipt后和写副作用边界安全终止/重启；恢复后继续完成同一验收，无丢失/重复业务副作用、无孤儿子进程。取消与崩溃是不同场景；无法保证 exactly-once 的工具要明确能力边界。
- session/trace可重建父子调用及调用序号；统计总工作量必须累计退出前后、摘要、重试和子Agent，不能只算恢复后的成功片段。续接与从头重跑各一份对照。
- 性能层另用相同冻结本地fixture重复10次，计启动/dispatch/持久化/工具/收尾耗时与资源。若借AA回放，先`aa-mini-v1`，固定提交和输出策略；只报告固定负载服务/客户端速度，不能据此宣称模型完成任务或Go/Pi原生能力。

**退出**：所有无费机制断言通过、最终产物通过独立验收、无泄漏；脚本100轮不是收费长任务能力分数。真实长项目另到R5验证。

## R4：公共Pro V2扩题，先小批后未见验证

**进入**：R0–R3工程门通过；原Flipt/Vuls和每批新增任务先做empty fail/reference pass黑箱环境控制。参考解答只交隔离runner，Builder/模型不读gold。模型与评分分容器、fresh replay包含新文件/模式，任务没有隐藏tests/solution/fixing refs。

### 本次已冻结的额外10题

见[完整元数据锁](pro-v2-expansion-lock.json)，SHA256：`2e8f4f9409f2ce4d61d5f59651802f3486f446325f2b1646622380c51bbd456b`。官方资产提交：`66f92766bba642462d4bbe5479e83f91f9211862`。复用先前seed `go-cli-pi-pilot-v1|`，同repo按`SHA256(seed+task_id)`升序，排除原三题，取后续Flipt4、Nav3、Vuls3。没有按胜负、题目难易或gold挑选。

本次只读取git树元数据，冻结task tree、instruction与task.toml的Git object ID；**没有读新增指令、gold/测试，也未下载新增镜像或发模型**。该样本仅覆盖三个预先定义Go仓库家族，不代表Pro全榜或所有语言；7个holdout仍是公开数据，不能排除模型训练污染。

| 分区/批次 | Flipt任务后缀 | Nav任务后缀 | Vuls任务后缀 |
|---|---|---|---|
| development（3题） | d966559200183b713cdf3ea5007a7e0ba86a5afb | 3853c3318f67b41a9e4cb768618315ff77846fdb | 2c84be80b65d022c262956cd26fc79d8bb2f7010 |
| holdout batch1（3题） | 9f8127f225a86245fa35dca4885c2daef824ee55 | 9c3b4561652a15846993d477003e111f0df0c585 | abd80417728b16c6502067914d27989ee575f0ee |
| holdout batch2（3题） | c6a7b1fd933e763b1675281b30077e161fa115a1 | 3bc9e75b2843f91f6a1e9b604e321c2bd4fd442a | 2923cbc645fbc7a37d50398eb2ab8febda8c3264 |
| holdout batch3（1题） | f36bd61fb1cee4669de1f00e59da462bfeae8765 | — | — |

**执行顺序与AC**：

1. 先完成原Flipt/Vuls两题资产门，再一题一对D/Pi（或另冻的新版本），fresh评分；原Nav只作回归。
2. 再开新增development三题，每次一对；失败先停修，保留旧版本首次结果。开发集允许定向工程诊断，不能作未调优holdout。
3. 之后逐批解封7题；每批开始前冻结同一Go版本与Pi版本、请求/时限/资源profile、seed20261028的“Go先/Pi先”顺序和attempt数量。遇失败停止下一题，保留全批未完成状态；题目一旦看反馈用于修复，即降为开发题，后续新版本只能在尚未解封题上得到新验证。
4. 首轮每题每Harness一次；先完成固定版本的完整小批，再只对预声明的同题重复实验加两次独立新session，报告三次均值/分布，不取最好一次。若中途改版本，重复分组重开且不合并统计。
5. 环境失败不按分数换题；记录原ID与排除原因，按已冻结hash顺序候补，并在解封前登记。工程排除、题目变开发题、缺失对照均进分母说明，不能只报成功子集。

**退出**：完成实际批准的小批及其失败修复/回归闭环即可形成阶段报告，不要求立即跑满10题。未运行仍明确pending。未见题不足时停止“holdout”宣称，重新预注册新集合后再继续。

## R5：真实长项目、同题重复与效率统计

**进入**：前轮基本可靠；另冻结两个CPU可运行的多阶段长项目及独立验收，先只运行第一个的一对。优先审DeepSWE v1.1或SWE-Marathon v1.1的公开任务环境；只有实际满足本机内存/架构与评分控制才选。不能因名称含long就使用、不能拿短bug耗时等价人类工时；当前尚未冻结具体ID，不称已就绪。

**可执行AC**：

- 任务具有跨文件/组件、多个可独立验证阶段和最终集成验收；至少一项验证压缩/续接或较长依赖链。自建任务需给需求、初态、hidden验收的隔离办法与审查人，不能由被测Agent自己宣布成功。
- 长任务profile在任何响应前单独冻结（建议起点5400秒、120上游请求、主cap16384、有效high，同CPU/RAM）；这与Pro官方3000秒profile分开。预算触顶保留timeout/cap与费用，不临时只给落后一方加时。
- 先一题一对；若两者能稳定闭环，再开第二题，然后每题各补两次。完整上限2任务×2Harness×3attempt=12次，按批逐步执行，不预先整批排队。重复session和工作区全新，环境准备移出agent计时。
- 分别做相同模型下的“首次冷缓存条件”和“自然缓存复用条件”，记录cached-input，不声称能清空远端缓存；能分组的服务条件共同记录，不能只对一方预热。配对采用“Go先/Pi先”两种顺序，串行避免4GiB容器争用。
- 保留工具动作、模型轮数、摘要/重试/子Agent数量及全过程usage；每题报告成功次数、总墙钟、agent/模型/工具/评分分段、input/output/cached/reasoning token（缺失null）、资源与失败原因。模型API时间可重叠时不硬加成墙钟总量。
- AA固定轨迹速度另列性能诊断页：同轨迹/输出策略/缓存profile才比较TTFT、吞吐与p50/p95；闭环轨迹因模型决策不同，不能用token/s或回放通过替代任务成功率。

**退出/统计**：完成两题三次只是诊断样本。报告原始每次结果、每题平均pass@1（不是best-of3）、同题耗时/token分布，成功attempt延迟与失败/删失分别列，并给总费用或总token÷成功次数的摊销量（零成功则不定义）。小样本不给可靠p95或总体显著性结论；扩量后才作配对、按repo/task/trial层级bootstrap或二值配对检验，明确CI与多次调优。不可把版本B/C/D/E、开发/holdout、不同网络/预算/架构或不同benchmark混成一个均分。

## 每轮交付和Reviewer清单

每轮产出一个小目录：`manifest.json`、`acceptance.md`、脱敏原始结果索引、失败分类、版本差异、验证日志与下一批解封名单。必须能回答：本轮承诺什么、实际跑了什么、哪项没过、下轮为何可进入。

- [ ] R0：真实CLI多轮与有效参数，测量/网络负例。
- [ ] R1：复用新main受管工具能力，不绕过核心；B/C历史与D/E新成绩隔离。
- [ ] R2：实际广告工具全覆盖，取消/后代/未知状态不误发完成证明。
- [ ] R3：100+轮压缩确实触发、重启后结果与副作用一致。
- [ ] R4：10题hash与分区可复算，无后验挑选；black-box controls与fresh评分。
- [ ] R5：真实长项目与性能回放分层，同题重复、成本/延迟/成功一并报告。
- [ ] git仅提交本轮授权范围；代码修复先TDD再Reviewer，必要全量校验完成后推送，失败如实披露。

当前失效的Flipt预检已另存`infra-interrupted`，旧暂停队列/控制进程及容器清理，不计empty负例；Nav既有empty0/reference1和真实评分保留。Flipt/Vuls fresh控制待资源窗口，当前不与Git修复/CI并行启动重型工作。
