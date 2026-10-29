# 第3轮：长链续接通过，执行窗口与回收仍分阶段验收

日期：2026-10-29。本轮新增的是**工程机制证据，没有新增真实模型任务成绩**。R1b在两个真实CLI进程间完成了120次输入，并通过文件检查点、一次副作用和最终产物验收；宿主Bash执行窗口与Linux孤儿回收也有局部结果。当前Darwin Go组合的全量与单独tagged验收已通过；不同归档源码、平台和profile仍分别报告，不能扩称全部平台或全部工具已认证。

[第2轮](round-02.md)的Go D结果保持：Nav补丁官方reward1、CLI exit1、完整交付0。D使用的amd64二进制未改写。当前组合没有在更多标准题上取得新模型分数。Go改动已分为ACP fixture [df625c3](https://github.com/PengJunchen/go-cli/commit/df625c3f32679e1a2ca69007745b95e8baaa89ab)、宿主Bash窗口 [ea224bd](https://github.com/PengJunchen/go-cli/commit/ea224bd74139ee3e889d2d9ec6a14813337114c2)、显式outer [b88de71](https://github.com/PengJunchen/go-cli/commit/b88de71c8dc87a5f63e46167e731018c7034849a)、R1b测试 [081289b](https://github.com/PengJunchen/go-cli/commit/081289b29bfdeed997e7eef8f5574c1dd35c54db)并推送；当前Go内容已有整合校验，平台边界如下。

## 长任务怎样确认没有“接着聊，却忘了做过什么”

R1b v2使用本地确定性HTTP/SSE peer，驱动真实CLI，不调用付费模型。进程A提交60次用户输入后退出；新进程B通过公开 `interactive --resume-session` 接口恢复同一个物理session，再提交60次。测试不是直接调用内部Session API来代替公开恢复。

初始任务给出随机业务标识和canary；peer必须从真实工具结果取回状态。先写文件检查点并执行一次副作用，恢复后读真实检查点，再生成最终文件。独立grader从初始请求取得预期值，核对原始业务文件，而不是从最终答案反推预期。

| 验收项 | v2实际结果 |
|---|---:|
| 真实CLI进程 | 2，均exit0 |
| 公开用户输入 | 60+60=120 |
| 实际本地HTTP请求 | 125 |
| 不同持久压缩记录/边界 | 117/117，A57、B60 |
| 成功且有资源证明/resolution的工具动作 | 5 |
| 恢复前/结束后pending action | 0/0 |
| 业务副作用次数 | 1 |
| 测试墙钟 | 23.32秒 |
| 真实模型请求 | 0 |

测试profile明确使用4096上下文压力、截断压缩、memory-off。它证明文件检查点和恢复机制能在这些条件下完成这条工程链，不证明默认128k配置自然会发生117次压缩，也不证明LLM摘要质量或模型自主规划120步。早期临时聊天信息在恢复后不可见，业务canary依赖文件检查点保留，不把两者混作“永不遗忘”。125次请求没有真实usage，成本与模型能力分均不能补成0。

独立grader的缺检查点、重复副作用、错误新session等负对照通过；这验证grader区分力，不是三次完整120轮CLI故障注入都已运行。第一次正向旅程已通过，但缺少业务原件导出；第二次仅修复取证导出后在新OUT重跑，原运行和原件没有回填，不是best-of2。

上表是独立测试基线 `d282b5c` 上的v2历史结果（23.32秒）。三份测试原件随后按已审SHA和权限精确集成，没有搬生产代码或旧分支历史。当前Darwin组合又独立运行了一次R1b，得到23.14秒；125 HTTP、120次公开输入、117个不同压缩边界（A57/B60）、两PID同session各60、五动作正向proof/typed resolution、恢复前后pending0及effect1均再次通过，三份业务原件由Reviewer独立核对。两个运行分别留存，不取最好一次，也不比较这0.18秒差异的效率。

## 一条同步命令是否有足够时间跑完

D的普通Bash固定30秒，模型schema只有command，现有CLI当时没有延长入口；这不等于D的Nav故障由超时引发。本轮候选增加宿主配置 `tools.bash.timeout`，例如300s，默认仍30秒。上层取消与runtime收紧继续优先，TERM/KILL和关停预算没有放宽。

用本地peer驱动真实Darwin ARM64 CLI，supervisor关闭，执行同一个 `exec sleep 31`：

| 条件 | receipt工具duration | 实际结果 |
|---|---:|---|
| 默认30秒 | 30,006ms | 明确执行deadline，receipt unknown、exit1；资源proof/resolution已闭合，无shutdown pending |
| 宿主300秒 | 31,020ms | receipt success、exit0，继续到最终回复；资源proof/resolution闭合 |
| 显式无效空配置 | 未派发工具 | exit1，模型HTTP请求0 |

原始脚本wall分别30.242/31.679秒，包含本地peer关闭等测量开销，不是纯工具耗时、CLI吞吐或模型性能指标；不与后面四臂CLI wall混池。

这说明候选的配置确实接入了真实CLI，不只是解析函数接受300s。它不是Linux评分环境验收，不是模型速度比较，也没有改变D的历史profile。工具实际描述会随有效宿主窗口变化，schema仍只有command；driver不能伪造timeout参数或默认worker工具。[时限核查](bash-timeout-audit.md)、[配置设计](bash-timeout-config-design.md)记录来源与边界。

## Linux后台进程：资源收尾与业务成功分别算

专用outer-CLI reaper候选只在standalone Linux显式启用、非TTY时生效。outer不装配模型/插件，先拒绝exec继承的未知child；唯一业务child是实际CLI，其余被接管孤儿按真实父子关系和pidfd精确回收。内层group ESRCH、reader和Unknown规则不变。

实际CLI的pidfd由 `SysProcAttr.PidFD` 在fork/clone时原子返回，不再用可复用数字PID重开信号目标。Go标准库可能先同步产生并join能力探针，因此不能宣称整个生命周期只有一次fork；在Start返回、私有release前检查唯一actualCLI，reaper此后才启动。parent-death单元合同检查至少一次可观察取消，普通信号转发仍检查恰好一次，不臆断重复parent-death信号的具体来源。

Linux ARM64、no-init真实CLI矩阵，经独立核对得到：

| 合成条件 | wall秒 | 工具/CLI及资源结果 |
|---|---:|---|
| 关闭outer，未等待后台命令 | 41.237 | child成为PPID1/Z；无completion/resolution，CLI exit1/pending |
| 开启outer，同一后台命令 | 0.340 | child消失、proof/resolution闭合；仍unknown、exit1 |
| 开启outer，前台命令 | 10.388 | success、exit0、child消失及proof/resolution闭合 |
| 开启outer，后台后显式wait | 10.346 | success、exit0、child消失及proof/resolution闭合 |

后台任务被终止后，即使回收干净也没有变成成功。合法的脚本内并行加wait仍可成功，不靠正则封禁所有 `&`。这四臂来自原生ARM64候选二进制，不是D的amd64通过记录。

取消测量保留两条早期无效记录：v3的amd64/Rosetta Python pidfd发送器ENOSYS、v4的进程发现器漏认actualCLI，均没有确认发出目标信号。这些是measurement invalid，不算两次产品取消失败，也不能删除后只报成功。

后续独立v5取消补臂已通过独审：使用同一原生ARM64归档二进制f4e56f…/源清单7006626f…，稳定识别outer→actualCLI→Bash leader→child后只发一次TERM。child从原leader的S状态转为outer接管的Z（exit15），随后连续观测absent；唯一action有quiescent、非空正向proof和typed resolution，CLI0.546秒、exit1/context canceled及Unknown，无shutdown pending。外部容器删除晚于Wait和取证，没有靠外杀造proof。发送器最后身份检查到发信号约6.5微秒，不是原子身份检查；不能扩大成无竞态的全局进程树保证。

此结果仅对该Linux ARM归档与本地peer取消场景成立。Linux amd64 opt-in CI尚未绑定终态，本机Mac Docker的amd64/Rosetta仍未通过；正式amd64工程profile继续supervisor-off。内层reader joined布尔没有直接导出，持久正向proof和/proc观察分别报告。所有自有容器/网络已清除，旧无效126与两条取消测量记录均保留。[设计及限制](linux-owned-orphan-reaping-design.md)

## 验证账本与下一步

- ACP三个测试夹具修正EOF/Disconnect事件顺序：目标race重复5次通过；生产适配器及原2秒deadline不变。它是fixture-only，不是新的ACP产品能力修复。
- 此前归档导出runner合同为52项通过，覆盖进程快照等测量行为；这是执行器合同，不是52道任务或新模型成绩。
- 当前Darwin Go组合 `make verify` **exit0，通过**：9708 started、9699 passed、0 failed、9 skipped、31包，472.44秒；fmt/vet/lint/build、scan、log10、leak6均通过。goimports缺工具按原规则skip，不声称已执行。验证时HEAD为098c350加冻结候选；1362份Go源文件SHA及mode前后、当前提交内容均一致，才将此证据绑定四组提交，未改保护规则或时限。
- 另跑tagged验收门 **5/5通过、0失败/跳过**（包含父测试，下面列4个叶子场景）：已有120-turn5.79秒、PTY后续保持Bash副作用1.28秒、PTY Escape取消6.26秒、当前整合R1b23.14秒。总tagged门44.82秒，不能把父子测试耗时重复相加；它是实际Darwin race CLI加本地peer，不是Linux opt-in或模型能力成绩。
- 新Python执行器最终包已通过safe audit及独审：25份public源与 `tool-metadata-final-export-20261029T001458Z-41eb78` 全部hash一致，pinned Python/Harbor的64合同全部通过，14.444秒，源manifest为f14539b5…，已提交0962aa7。旧f93的24源/61合同、此前52合同及旧64/14.948秒导出原样保留，不倒填为最终包证据。实际新profile双CLI有效配置/wire、最终二进制/payload与模型派发仍待完成；执行器合同不等于模型任务成绩。
- 原Flipt题资产门通过：固定源码/镜像、初态无评分与答案目录、指定fix不可见、工作树干净。官方镜像保留2442个历史提交/1个ref，不能称全部历史剥离。新fresh empty得到reward0/exit1、681.57秒；reference得到reward1/apply0/verifier0、803.00秒，四份前后资产侧车完全一致、两个容器清除。两控制串行完成，无暂停/自动重试，已通过独立标量/hash审核。补充fresh初态配置探针确认15个准确Go/Pi项目及全局配置候选路径全缺席，cwd/canonical/gitroot均为/app；配置presence侧车也已独审；这仅排除预置配置存在，不能代替实际fixture/wire的5m窗口、memory、small-model、extensions有效态核验。模型仍未放行。
- 新增10题锁未变、未解封；后续Flipt真实pair须另冻明确profile。默认profile不能悄悄改成Bash300或outer-on。真实模型长项目、LLM摘要、多次采样、全部工具覆盖仍未完成。

## 可追溯证据

下表为相邻本地执行区的脱敏汇总工件名及SHA256；不发布raw轨迹、业务原件、完整补丁、gold或隐藏断言。哈希绑定对应归档版本，不能拿局部归档代替最终组合冻结。

| 本地工件 | SHA256 |
|---|---|
| `main-integrated-verify-20261028T235917Z-719edc.log` | `75eefff2faffa595340c2114a479a7e9694fbf100780144115f4f1eb00eb3ac2` |
| `main-integrated-tagged-longtask-20261029T000807Z-653ced.log` | `c3496e170f10e77acfe0aa1674ee8ef6a940960f0783f5f08017c648648b4821` |
| `r1b-integrated-main-integrated-tagged-longtask-20261029T000807Z-653ced/evidence.json` | `34a2fbbd11ae998962b7ab689cb96ad4cb97a11d11612a655d6d9383683b6674` |
| `outer-supervisor-v5/run-20261029T000954Z-b978a1/validated-summary.json` | `a516f31d1f0fe54a4e84814e966824bc373e5bcfe7dcb3536b03bc5bfd789777` |
| `outer-v5-cancel-attempt2-independent-review.md` | `a5bb12742405473086f4a3faeaec77179e1302b2df408f4ed7cdd4c63ba794b8` |
| `r1b-public-resume-20261029-v2-validated.json` | `1b158b4fbe5c5436ca237513441a2a51cf6865721f7f7fd1fb1412c1b971ca30` |
| `r1b-v2-independent-review.json` | `beb3859dd643bdde13697c3b9c9796ab8e950ba5108afbf1532beeaba762a1d0` |
| `r1b-v2-integration-20261028T232809Z-6f85fd/source-target-manifest.json` | `ac3e2029169dcae67fe36df860bc75f77741a36c6ea3d750ab5df0a949a309ac` |
| `bash-timeout-independent-review.json` | `826e420cd8c5c309e4721a217f9915e7f5b284814c759587121c8aebc1a62b6d` |
| `bash-host-timeout-v1/run-20261028T233623Z-ed1210/timing-scope.json` | `273ee6ff18f468ea0718d276e87516dc5b57c236fa96c04441c794a5cc8601da` |
| `outer-v3-real-cli-independent-review.json` | `690519e552f1839f9c010a285e1ffa193b7b2e2ce51608831e953764c437a6ff` |
| `acp-disconnect-fixture-validation.json` | `92e7c0050b69559f5a00f1836a0221fde55f15d12cc2974325296ec58f3f444f` |
| `process-snapshot-export-52-tests.txt` | `a886a6da1db361f0fb61e6202f85be88afcb55056da929b479bc377021fd5f0d` |
| 最终public `runner-source-manifest.json`（25源） | `f14539b53aa0f4e28924fb28b84de95623b012f384eb4e7b5b6fde32f8414613` |
| `runner-tool-metadata-final-full64.txt`（14.444秒） | `c3c89054200d3a5f086bae5b3f29d3beb994078c3c129d76ed700430286df9ae` |
| `tool-metadata-final-frozen.json` | `281badca7faf4846ac7a8dba84fb3e742afeb2099b178b600a01e200b5ca3038` |
| `tool-metadata-final-independent-review.md` | `942566b01c4fb04381ac21c26cec9493dc1b6578f17043d90546c966ea44b03b` |
| `flipt-recalibration-20261028T233035Z-5a3842/calibration-validated.json` | `d2db51c31209a4f619e847508eca177f94bc257c6f36b9e3a561626ee9bdcf5e` |
| `flipt-asset-gate-20261028T191344Z-044c1e/project-config-probe-20261028T235902Z-0e979e.json` | `409b7f62157243776407b0abfc9006bd705c73ab597289e41c4ea92310ee8da8` |
| `flipt-calibration-independent-review.md` | `60f495893ca67ff4c4280eb98dd00f9ffacb961e0a8c380280c758684962ef3a` |

timeout源清单为 `0980ad05906350985389dd419ee1acd28b4b29276da6c329ece60e38a738fd35`、Darwin二进制为 `a79fa567c6ea883cd6e21e9fbed811e3cc654911e6fba85e786c6b1c7038f06c`；outer v3源清单为 `7006626f63a9342de9744db95d6dcc9a9f526cf70e8229ed82edf3ec955f9979`、ARM64二进制为 `f4e56f341f1483ac408a99cf28be88ce8294682a60629e8d5a2e2bc750ab822d`。两份快照的验证范围不同，均不是新的标准题平均分。
