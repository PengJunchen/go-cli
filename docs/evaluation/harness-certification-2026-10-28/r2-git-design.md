# R2 Git写链：复用新main，补实际验收

创建：2026-10-28；迁移更新：2026-10-29。当前发布基于PR9后的main `5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1`；D为该base加Thinking修复及评测集成，发布commit/diff/binary SHA尚待冻结。本文只读设计，不改Go、不运行测试/模型/评分。旧379/C“git_commit未受管”的设计依据已经过时，不能对新main重复实施同一修复。

历史：旧B的Nav0/Pi1保留，旧C只有无费证据、没有真实模型任务评分。详见[版本迁移](version-migration.md)、[首轮报告](round-01.md)。

## 新main已经具备什么

| 路径 | 新main实际实现 | 应先复用的测试 |
|---|---|---|
| Git run/read | `run`、`runReadOnly`均到`runOwned`→`runOwnedCommand`，每个helper join leader/所拥有进程组/reader；未知时停止后续动作 | `TestReviewedRuntimeAllGitToolsExecuteAgainstLocalRepository`：[reviewed_runtime_tools_test.go](../../../internal/tools/reviewed_runtime_tools_test.go) |
| 注册与权限 | `RegisterReviewedTool`封闭exact第一方Git实现，普通Register/custom依赖不获证明；completion与effect/审批分开 | `TestReviewedRuntimeGitRejectsSubstitutedAndNilDependencies`及[reviewed_runtime_tools.go](../../../internal/tools/reviewed_runtime_tools.go) |
| commit/merge异常 | commit后hash lookup失败显式报错；unknown merge不再启动conflict probe；失败网络操作可为结果unknown | `TestReviewedRuntimeGitUnknownMergeStartsNoProbe`：同上；[git.go](../../../internal/tools/git.go) |
| 自动维护 | `maintenance.autoDetach=false`与`gc.autoDetach=false`使自动维护留在受管调用，不关闭任务或改repo配置 | `TestReviewedGitMaintenanceRunsInForegroundWithoutChangingRepositoryConfig`、`TestReviewedGitBackgroundHookStillHasUnknownOutcome`：[git_maintenance_test.go](../../../internal/tools/git_maintenance_test.go) |
| 组装CLI多轮 | custom改文件→diff/status→commit/log→local bare push/fetch/pull→分支/stash/reset/merge/revert→state，逐tool receipt/proof、pending空；resume不重放custom | `TestPromptDurableReviewedToolsLocalAndBareRemoteJourney`：[durable_reviewed_tools_test.go](../../../internal/cli/durable_reviewed_tools_test.go) |
| 多helper未知资源 | commit helper未完时不提前proof；escaped reader保持unknown，后续hash probe不执行，不重放模型动作 | `TestPromptDurableGitAggregateWaitsForLaterProcessAndRetainsUnknownReader`：[durable_reviewed_process_unix_test.go](../../../internal/cli/durable_reviewed_process_unix_test.go) |

上述是源码覆盖，D本轮尚未实际运行，不能直接勾选PASS。先执行[coverage baseline的现成命令](coverage-baseline.md)，实际失败再定位；不根据旧base风险给新main加第二套聚合或注册cap。

## 最小下一步：先已有journey，再补独立业务断言

已有长journey验证每项成功receipt、完成proof、无pending与custom不重放；它不等于每一步最终Git业务状态都经外部oracle核对。优先在同一fixture补测试进程独立Git断言：

1. 记录初始HEAD及commit count；修改已跟踪文件，commit后核对新HEAD parent、tree内容、message、count只增1。
2. push后在临时bare repo独立读ref，与本地期望commit一致；fetch/pull另核tracking ref与工作tree，而不是只检查工具输出success。
3. stash/pop/reset/revert等本地变更逐阶段验内容与ref，避免最终revert把前序错误掩盖。可先补commit和bare ref两个关键检查，逐批增加，不一次重写全部测试。
4. approval/Plan/custom替换负例沿用已有路径；completion证明不能改变现有允许/拒绝规则，副作用未知不能自动重试。

所有Git配置只改临时repo，远端仅`t.TempDir()`的local bare。不得调用真实GitHub、SSH凭据或向用户仓库push。

## 必要时的真实二进制六请求链

如果已有组装入口绿但需要覆盖独立CLI启动/配置/进程边界，复用[现有binary tools fixture](../../../tests/e2e_binary_tools_test.go)，添加一个小链而不是新造平台：

- 独立HOME/XDG、临时`git.workdir`和session store、本地HTTP peer；真实模型key为空。
- 初始提交包含`fixture.txt=before\n`与忽略`.go-cli/`的`.gitignore`，repo-local synthetic name/email、禁签名；普通正例禁hook，故障hook放独立fixture。
- 修改已跟踪文件使git_diff确有内容，避免未跟踪新文件导致diff空；忽略运行审计文件，避免add_all把它们混进业务提交。

| 主请求 | 动作 | 下一请求/外部验收 |
|---|---|---|
| 1 | write fixture.txt=after | 后续请求有该tool-call ID的receipt，文件实际变更 |
| 2 | git_status | 下一请求显示目标被修改 |
| 3 | git_diff path=fixture.txt | 下一请求含before→after |
| 4 | git_commit message=synthetic/add_all=true | 下一请求有对应结果，资源已完成，不只leader返回 |
| 5 | git_log max_count=1/path=fixture.txt | 下一请求的hash/message与独立HEAD一致 |
| 6 | final | 独立Git核对parent/tree/count；重开JSONL各action有resolution，无pending；不是只看final文本 |

脚本预设动作，只认证工具/运行时，不证明真实模型自主解题。若要最小红复现，应预先stage一个修改、只请求commit→final两轮，先确认新main是否真的失败；**不能预判它一定红**，更不能为制造红而退回旧实现。新增函数名由Builder实施时登记，当前不提供尚不存在、会零匹配的测试命令。

## 若新base仍失败，通用修复必须守住的门

- 以D实际失败建立精确红测试，保留版本/日志；修复后新版本另冻，不覆盖B/C/D原attempt。
- 只对确切第一方实现授予completion，效果保持unknown或原合同；不能把写操作套只读cap、按名字发proof、给任意GitTool接口借权或放宽core。
- 仓库检查/add/commit/hash lookup与hook/transport等资源必须按实际helper序列join；前一helper或最后helper不能替全调用证明。未知/pending/强关reader保持保守，不用inline-success兜底。
- 资源结束与副作用确定分开；commit后查询失败、push丢响应等不能因本地process结束就安全重放。新main已有错误语义必须保留，不回退旧`commit created`假成功。
- 单个leader退出不证明任意主动setsid且关闭pipe的逃逸后代被包含；不扩大OS沙箱声明。后台Git维护保持受管，不关掉原生功能以凑绿。
- 沿用原审批/Plan/custom替换/worker/memory合同，必要目标race与独立Reviewer通过，再按协调安排完整make verify；不得与模型/评分抢资源。

## 本轮可执行AC

| AC | 现有/增补路径 | 当前状态 |
|---|---|---|
| AC-1 原生多轮可达 | main现成local+bare remote journey | 待D实际运行 |
| AC-2 未知资源不早proof/不重放 | main现成aggregate escaped-reader负例 | 待D实际运行 |
| AC-3 默认类型/审批边界 | main exact类型、普通/custom替换、deny/Plan负例 | 待D实际运行 |
| AC-4 独立业务验收 | 在现有fixture优先补commit parent/tree/count与bare ref | 先核现成断言，再最小增补 |
| AC-5 二进制边界 | 必要时复用binary fixture六请求链 | 尚未新增；不冒称已通过 |
| AC-6 身份与回归 | D完整commit/diff/binary SHA、目标测试/race/完整verify | 待协调冻结和执行 |

R2先完成无费工具有效性，再依计划运行Nav开发回归及未见Pro小批。额外10题锁未变、未解封；不靠重复收费任务发现已有工具协议缺口。
