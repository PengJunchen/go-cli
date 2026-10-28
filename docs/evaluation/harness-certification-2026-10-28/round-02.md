# 第2轮：补丁通过官方验收，运行关停仍失败

运行及核查日期：2026-10-29（北京时间）。Go D 在固定 Navidrome 开发题上的 **官方 reward 为1、verifier退出码为0**；Go CLI 自身退出码为1，最后一次后台 Bash 操作的资源收尾没有得到证明。两件事分别保留：补丁正确性通过，完整运行交付失败。

这是一份 Go-only 开发回归。Nav 已参与故障分析，不能重新标成未见题。旧 Go B 的0分、旧 Pi1.4.0的1分仍是[历史记录](round-01.md)，此次没有同期重跑 Pi，也不能据此宣布胜过 Pi。未解封任务和重复试验仍待后续冻结。

## 本次固定了什么

被测源码为干净提交 **`5aa063ee647f753f7f0fc7282a3baf5b6e691932`**，基于main5736e88，保留其原生Git、工具、子Agent和会话实现，仅带本轮通用Thinking编码修复及回归。构建为Linux amd64、Go1.26.1、CGO关闭、trimpath、buildvcs=false。源码差异为空；构建来源和二进制另立清单，不回填此前候选或旧job。

官方任务资产提交 `66f92766bba642462d4bbe5479e83f91f9211862`；任务为 `instance_navidrome__navidrome-1e96b858a91c640fe64e84c5e5ad8cc0954ea38d`。任务容器Linux amd64，在Mac ARM宿主仿真执行，1CPU/4GiB、pids512、cap-drop ALL、no-new-privileges。10GiB存储只声明，没有硬限。采用自有薄驱动接官方Harbor任务资产，不是完整官方榜单成绩。

模型请求固定 `codex/gpt-6.1-sol`、high、主调用输出上限16384；全部请求计入80次共同上限，Agent总时限3000秒。没有整题token/美元硬限。四个采样字段实际省略，不宣称temperature=0或远端确定性。Agent只能访问固定模型relay，公网、包仓库和宿主直连探针均失败；评分另起fresh同镜像，按官方public网络执行原patch replay与verifier。

profile为 `main-5736e88-personal-memory-off-v1`：新HOME、自动个人记忆关闭，原生工具、session及压缩保留。Go内部压缩预算128000、micro_first，Pi历史配置容量500000；并未对齐两者内部策略。**本profile的small_model为空，Go没有LLM摘要器，需要时采用原生截断降级**，详见[辅助策略审计](../../../scripts/harness_eval/reports/auxiliary-policy-audit.md)和[容量审计](../../../scripts/harness_eval/context-budget-audit.md)。当前D不因发现这项差异而临时改配置。

## 验收链先校准，再运行一次

新黑箱校准使用与clean grader相同的资源、安全和超时参数，empty与reference分别在不同fresh容器串行执行：empty reward0/exit1/266.29秒；reference reward1/应用退出0/verifier退出0/263.32秒。前后四份资产哈希侧车完全一致，控制容器均已清除。旧control未记录的哈希没有补填。

随后只运行一次Go CLI，没有补提示、重启或挑最好一次。候选冻结后，在fresh同image ID中仅重放候选补丁，调用固定官方PatchReplayAgent和原test.sh。独立审核逐项验证16个候选文件、11个评分文件的冻结哈希；镜像、replay和5份verifier哈希与新控制侧车一致。未读取gold、Pi答案或隐藏断言；隐藏输出仅核字节哈希。

## 结果与消耗

| 指标 | Go D 本次结果 |
|---|---:|
| 官方fresh补丁reward | **1** |
| 官方verifier退出码 | 0 |
| CLI退出码 | **1** |
| 自定义完整交付分 | **0** |
| Agent墙钟秒 | 355.761 |
| 独立评分墙钟秒 | 281.209 |
| 上游请求/HTTP200 | 37/37 |
| 输入token累计 | 609,514 |
| 其中缓存输入token | 525,184 |
| 输出token累计 | 10,588 |
| 输入加输出总量 | 620,102 |
| reasoning token | 1,942 |
| 缺失usage/未结束请求/契约拒绝 | 0/0/0 |
| 单请求最大prompt token | 30,695 |
| 美元费用 | unknown |

缓存输入属于输入子集，reasoning属于输出统计内的细分，均不再加到总量。37次请求包含原生子Agent实际调用，不只计算主Agent；费用未取得可靠账单，不以token猜美元。累计输入609,514是多轮重复上下文，不是单次窗口。最大单请求30,695，不能把本次故障归因于超过128k上下文。

“完整交付分”是自定义运行指标：valid官方评分存在，但CLI异常退出，因此记0；它不替换或抹掉官方reward1。纯解题诊断子集不收该运行时失败，本次仍完整保留在全部attempt和主交付分母中。一次尝试没有置信区间或总体排名，较短耗时也不能直接当速度胜出。

## 运行时发生了什么

实际创建了2个原生子Agent、3个durable session。一个子session的10个action均完成资源resolution但run以error结束；另一个19个action均完成并成功结束。主session有23个intent、22个资源resolution，最后一次Bash为cleanup_pending、receipt unknown，没有正向completion proof或resolution。

对应的是可见项目中的普通编辑、格式化及**后台运行测试**。独立审查了本次普通业务源码和测试补丁，没有发现跳过测试、修改裁判或硬写reward的行为；未公开完整补丁或日志。错误指向本地执行资源，不是模型API拒绝：37次请求均200且high/输出/usage合同满足。CLI终端报告invocation shutdown remains pending，日志/trace最终收尾被推迟。

Agent结束后，collector另停止并杀掉了两个本容器残留PID，最终任务与评分容器均已清除。这项外部清理不能冒充Go内部已经完成的proof。原attempt没有保留足够/proc状态，尚不能仅凭PID1是sleep认定孤儿僵尸是根因。

## 后续因果诊断边界

已另做无费三臂本地协议实验：固定D二进制和镜像，A/B后台命令逐字相同、仅Docker init不同，C只去掉后台标记。子进程自身原子发布ready；三臂均sleep10秒，避免混淆原生30秒Bash超时。独立观察者采实际PID/PPID/PGID/state，三臂各串行一次，没有真实模型请求。

| 合成诊断臂 | 实际进程与持久证据 | CLI结果 |
|---|---|---|
| A：无init、后台 | 子进程被终止后，实际观测PPID1/state Z约40.99秒；唯一action无proof/resolution | exit1，41.583秒，shutdown pending |
| B：仅增加init、相同后台命令 | 子进程消失；唯一action有owned_process_group_joined及local_resources_quiescent resolution | exit1，0.648秒，工具仍unknown，关停不再pending |
| C：无init、前台 | 子进程正常结束并消失；同一资源proof/resolution闭合 | exit0，10.562秒，工具success并进入最终回复 |

独立审核38个冻结文件哈希并按唯一action核对，未把递归日志重复计数当成多个证明。B没有变成业务成功：后台工作被owner终止，资源收尾与命令完成是两条指标。reader join内部布尔未直接导出，正向封闭proof和/proc观察分别记录。合成A直接复现了僵尸进程导致资源pending的机制；**原NavD没有/proc快照，不能把这项复现倒写成D每个残留PID的直接证明**。原NavD官方1/exit1及全部消耗保持不变。

Harbor0.25.0主服务模板没有默认init，只有egress sidecar设init；Nav也没有额外reaper入口。因此不能把问题说成薄驱动漏掉官方init。init是独立移植性对照，不能为了使Go通过而悄悄改变正式profile。后续修复必须保持真实进程组、leader和reader收尾证明，不把仍存在的僵尸进程组伪报为消失，不吞错或放宽超时凑绿。

## 已验证与尚未覆盖

核心生产修复及test-only夹具已提交于42ce224；D的Go内容一致。第二轮完整make verify通过：9666 started / 9657 passed / 0 failed / 9 skipped，scan/log/leak门通过；goimports因工具缺失按Makefile跳过。单独tag120为2/2通过，仅证明120次真实本地SSE用户提交及至少一次压缩日志等机制。当前评测源包导出49个合同通过；D提交5aa063在GitHub上13项检查均成功。

这些证据不等于R1b跨进程、多次压缩任务已通过，也不等于全部原生工具已认证。R1b只在独立工作树进行候选测试开发和源审，尚无对应真实二进制旅程结果；长任务LLM摘要、更多未见任务、多个模型与重复试验均未完成。

## 来源绑定

| 工件 | SHA256 |
|---|---|
| 空源码diff | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |
| Linux二进制 | `099f9b687a96737722ec7e0eec9264e00fe8f38488bc4f0e72ad39d76ba71894` |
| 最终payload清单 | `6179c19f9e322358786bc57879fc0f9112c7d9ad90abe47b8dd24c1ba79dbc13` |
| runner22文件清单 | `1fa85cfcaa5ca42b2c7edee94f176849db72176dc7ba7d8c06592b703bac24d1` |
| 新校准validated侧车 | `9f3d2e56ddbc1267841db2168925c16692e079ae17092467102117039a3d82a2` |
| 候选补丁 | `9976ee333168bfc74836cff68652fd156b47324dab73f26f78bd8174056c3d35` |
| Agent原result | `ceb07b71b76fba0840f97592badbd4d48fc5fd7c3404d411b4cb8a5c53d7da55` |
| wire记录 | `4d213110d5c5285f610b5751c16ff1acd89cb19de05c4797cafa1ae4de58f51e` |
| fresh grade | `37c5e70cc506e7a281143c30887dc5f2d47cd972f8455e51c4c88b61765d290e` |
| 三臂诊断validated侧车 | `cf8375698bf197f64391bb5588674a8f9ad64a38ae15499f6ae4db72a760552e` |

哈希用于绑定受控本地工件，不能证明服务端底模权重或隐藏路由恒定。公开包不包含raw trace、完整patch、gold或隐藏验收。下一步按[分轮计划](plan.md)与[版本账本](version-migration.md)另冻实验，保持原10题锁不变。
