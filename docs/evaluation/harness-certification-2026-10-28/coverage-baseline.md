# 工具与长任务测试覆盖：新main迁移基线

后续状态：本文保留实施前的源码核查与待办快照；D已冻结并完成一次开发回归，实际门禁、成绩与未完成项见[第2轮](round-02.md)，不将本页待办当作当前结果。

创建：2026-10-28；迁移更新：2026-10-29。**当前基线是PR9合并后的main `5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1`。** 本文件只读核查新main与旧379/C的差异，没有在新工作树运行测试、真实模型或评分。D为main加Thinking修复及本轮评测集成，完整发布commit/diff/binary SHA和实际验收尚待冻结。旧B0/Pi1与C无费未评分见[首轮报告](round-01.md)、[版本迁移说明](version-migration.md)。

PR9已经实现更广的受管工具、state/worker、durable子Agent/MCP、hook、会话管理与PTY修复。旧覆盖文档中“写Git未受管”“只有MCP adapter测试”“post-tool hook不支持”等判断不能沿用为新main事实。现在应先复用已有测试实际验收，只对仍失败或缺断言的环节做TDD增补。

## 测试层级与证明边界

| 层级 | 实际路径 | 证明范围 |
|---|---|---|
| 组件 | 直接工具、adapter、compactor/session方法 | 对象合同，不能证明CLI启用或模型收到结果 |
| 组装CLI入口 | AssembleAgent/newPromptCmd.Run+本地HTTP peer | 实际策略、工具、模型多轮与durable；通常不含独立CLI进程启动 |
| 真实二进制 | build race binary→exec CLI→本地HTTP/SSE | 独立进程配置/工具/持久化；脚本provider不能证明真实模型会解题 |
| 真实模型闭环 | 原生CLI→共同模型→独立产物验收 | 本任务与profile能力；不能代替取消/恢复负例 |

下面的“已有”指源码和关键断言已经只读核查，**不代表D本轮已执行或全绿**。

## 当前可复用覆盖

| 能力 | 实际测试/文件 | 已有断言 | 剩余边界 |
|---|---|---|---|
| 工具库存/Plan | `TestAssemblePreparedToolEffectInventoryAndPlanGate`：[prepared_effect_inventory_test.go](../../../internal/cli/prepared_effect_inventory_test.go) | 特定Assembly配置的39项source/effect/scope与Plan门，同名替换不继承信任 | 多为Prepare/分类，不逐项执行；动态配置仍须导出实际广告清单 |
| read/write/edit/bash真实多轮 | `TestRealBinaryPromptToolApprovalMatrixAndErrorRecovery`：[e2e_binary_tools_test.go](../../../tests/e2e_binary_tools_test.go) | 真实race二进制、auto/deny/默认ask、下一HTTP含receipt、独立文件结果；坏edit→更正edit→final三请求 | 多数工具各自tool→final；find/grep/read→write→edit完整链仍可小补；Bash正例用no-sandbox |
| 二进制两进程resume | `TestRealBinaryPromptResumesDurableSession`：同上 | 两个CLI进程、同physical ID/JSONL、第二请求含前问答 | 两轮简单问答，不含多次压缩或工具副作用中断 |
| 文件/搜索边界 | [read_test.go](../../../internal/tools/read_test.go)、[edit_test.go](../../../internal/tools/edit_test.go)、[write_test.go](../../../internal/tools/write_test.go)、[search_helpers_lifecycle_unix_test.go](../../../internal/tools/search_helpers_lifecycle_unix_test.go)中的`TestPreparedScopedGrepTimeoutWithEscapedWriterKeepsCompletionUnproven`等 | 白名单/symlink、截断、取消、helper/reader收尾与proof负例 | 不等于原子OS沙箱或所有工具均已真实CLI长链验收 |
| Bash进程生命周期 | `TestBashCancellationKillsPipeHoldingDescendant`、`TestStreamingBashNormalExitCleansRedirectedDescendant`：[process_lifecycle_test.go](../../../internal/tools/process_lifecycle_test.go) | 同组后代/pipe/启动取消的真实进程测试 | 任意主动setsid且关闭pipe的逃逸不在完整包含声明内 |
| **新main全Git/local remote旅程** | `TestPromptDurableReviewedToolsLocalAndBareRemoteJourney`：[durable_reviewed_tools_test.go](../../../internal/cli/durable_reviewed_tools_test.go) | 真实prompt组装+本地provider，custom改文件→diff/status/commit/log→local bare push/fetch/pull→分支/stash/reset/merge/revert→state，逐项成功receipt/proof、pending为空、resume不重放custom | 主要逐工具结果/资源合同；可以补独立最终HEAD/tree/bare ref业务断言。不能据此称真实GitHub/SSH/网络分区全覆盖 |
| Git聚合/逃逸负例 | `TestPromptDurableGitAggregateWaitsForLaterProcessAndRetainsUnknownReader`：[durable_reviewed_process_unix_test.go](../../../internal/cli/durable_reviewed_process_unix_test.go) | add/commit后续helper未完不提前proof，escaped reader导致unknown，停止hash probe且不重放 | 部分fake git机制测试，需与真实仓库正例共同报告 |
| Git完整类型/maintenance | `TestReviewedRuntimeAllGitToolsExecuteAgainstLocalRepository`、`TestReviewedRuntimeGitRejectsSubstitutedAndNilDependencies`：[reviewed_runtime_tools_test.go](../../../internal/tools/reviewed_runtime_tools_test.go)；`TestReviewedGitMaintenanceRunsInForegroundWithoutChangingRepositoryConfig`：[git_maintenance_test.go](../../../internal/tools/git_maintenance_test.go) | exact默认实现、替换负例、自动maintenance不detach且不改repo配置；后台hook未知结果 | 不需要重新实现旧R2未受管helper修复；新base仍须实际执行，不假定所有平台/remote协议通过 |
| state工具 | 上述durable journey；`TestReviewedRuntimeStateRejectsCallbacksAndExternalStores`、`TestReviewedRuntimeGoalSnapshotsDoNotShareMutableState`：同上 | task/goal/todo完成、拒绝不受审回调/外部store、snapshot隔离 | task等进程内store重启可为空，这是合同，不能误认成持久化任务内容已恢复 |
| worker跨进程恢复 | `TestPromptDurableOwnedWorkerResultsAndProofSurviveResume`、`TestPromptDurableOwnedWorkerUnknownRetainsPendingWithoutReplay`：[durable_worker_tools_test.go](../../../internal/cli/durable_worker_tools_test.go) | 真实本地worker、两call一次初始化、success/error proof、fresh host resume无重放；null/wrong-id/disconnect/timeout保持unknown | 特定worker协议，不能推断任意extension回调安全 |
| 子Agent独立session/审批/取消 | `TestDurablePromptSubagentOwnsPhysicalSessionAndContext`、`TestDurableSubagentWriteLivesOnlyInChildAndResumesExactly`、`TestManagedSubagentParentCloseCancelsAndJoinsChild`：[durable_subagent_cli_test.go](../../../internal/cli/durable_subagent_cli_test.go) | 父子真实组装多轮、独立JSONL、继承/独立context、子写隔离与resume、父close取消join | 不等于任意嵌套深度/真实长子任务能力或所有模型计费分支均已实测 |
| MCP闭环/恢复/拒绝 | `TestDurableMCPRequestCompletesAcrossOwnedRuns`、`TestDurableMCPUnknownRequestDoesNotReplayAfterResume`：[mcp_durable_execution_test.go](../../../internal/cli/mcp_durable_execution_test.go)；`TestDurableMCPAskDeniedDispatchPerformsNoIO`：[mcp_durable_preflight_test.go](../../../internal/cli/mcp_durable_preflight_test.go) | 实际Assembly+本地MCP HTTP peer，parallel/lazy、结果回模型、多run、未知无重放、拒绝无连接 | 已超过旧adapter层；不是所有stdio/远端副作用可撤销。底层stdio生命周期另见[stdio_process_lifecycle_test.go](../../../internal/mcp/stdio_process_lifecycle_test.go) |
| post-tool hook | `TestPromptDurableConfiguredPostToolHook`、`TestPromptDurablePostToolHooksParallelFailureAndResume`、`TestPromptDurablePostToolCancellationDoesNotReplaySource`：[post_tool_actions_test.go](../../../internal/cli/post_tool_actions_test.go) | source receipt后独立hook action、stdin payload、各自proof、下一请求、失败/取消/恢复无重放 | 新main已支持，不沿用旧unsupported结论；仍需按实际配置测动态信任/审批 |
| 会话管理/导航 | [session_history_review_regression_test.go](../../../internal/cli/session_history_review_regression_test.go)、[durable_cli_wiring_test.go](../../../internal/cli/durable_cli_wiring_test.go) | fork/clone/entry/retry/clear与pending动作证据、确切physical resume | 新main不再普遍拒绝历史管理；导航不等于允许重放未知外部效果 |
| **新main durable截断重开** | `TestPromptConfiguredTruncatingCompactionRestartsWithExactProjection`：[session_history_review_regression_test.go](../../../internal/cli/session_history_review_regression_test.go) | truncating/micro_first/unified三策略，过大历史→固定omitted marker，当前输入保留、3次prompt入口重开投影一致、无辅助摘要请求 | 比旧core单summary更完整；仍不是100+轮独立二进制、多次压缩、业务canary跨进程任务 |
| durable summary组件 | `TestDurableCompactionSessionProjectionSyncsBeforeRequestAndReopensExactly`：[durable_compaction_session_test.go](../../../internal/core/durable_compaction_session_test.go) | 真JSONL、先sync再请求、source ID/untrusted wrapper、重开继续 | 单次组件摘要，不能替代二进制长链 |
| **120轮真实流式二进制** | `TestE2EBinaryLongTask/120 real binary turns compact streamed history`：[e2e_binary_longtask_test.go](../../../tests/e2e_binary_longtask_test.go) | race binary、120次用户输入/SSE请求、reply120、当前输入保留、最早turn移出、truncating日志出现 | 仍只Contains日志一次，未要求≥2次；无事实canary最终产物；不是120步自主解题。fixture显式关闭memory，未配置durable session store |
| PTY取消/follow-up | 同父测试两个PTY子项；`TestLongTaskREPLPromptMarkerExcludesRenderedPrompts`、`TestLongTaskREPLPromptWaitJoinsSplitMarker` | 新main用raw/bracketed-paste真正REPL输入owner marker等待交接，避免把TUI重绘当ready；Bash/审批/follow-up/取消副作用断言 | 不再沿用旧PTY等待机制风险；仍与120轮是不同场景，不能拼成一个压缩后重启旅程 |
| 自动memory/辅助模型 | `TestAutomaticMemoryPersistsInjectsAndHonorsDisabledAndWorkspaceScopeAcrossProcesses`、`TestAutomaticMemorySmallModelHasIndependentTrackedOperation`：[memory_runtime_test.go](../../../internal/cli/memory_runtime_test.go) | 新main默认on、跨进程注入/隔离、禁用、独立tracked辅助调用 | 首批D正式及fixture均显式memory-off/fresh HOME，偏离产品默认；memory-on另profile后测，实际辅助调用仍计账 |

## 当前Git实现与旧R2的关系

新main的`run`、`runReadOnly`都到`runOwned`→[runOwnedCommand](../../../internal/tools/owned_command.go)，只读/写操作保留各自参数环境；[RegisterReviewedTool](../../../internal/tools/reviewed_runtime_tools.go)已对封闭exact默认Git类型提供completion-only，custom/普通注册不借权。自动maintenance仍运行但不daemonize；commit后hash lookup失败返回错误，未知merge不再启动下一probe。

所以旧379/C的“git_commit仍普通注册、g.run仍CommandContext”仅为历史发现。迁移后先运行现成journey/负例，按真实失败决定是否修复；补验方案见[R2 Git新main设计](r2-git-design.md)，不另造同一套已合并机制。

## 哪些make verify会选入

[Makefile](../../../Makefile)的`make verify`包含`check + scan + test-log + test-leak`；默认`test`经test-gate对`./...`执行`-race -count=1`，没有e2e tag。

- 无tag的binary tools/resume、上述普通internal/cli测试会进入默认选择；是否真正执行及skip需以D日志确认。旧C日志的PASS不能迁移成D已通过。
- `tests/e2e_binary_longtask_test.go`带`e2e && (darwin || linux)`，120轮/PTY需单独加tag；目录或文件名含e2e不等于真实二进制。
- `make test-e2e`比最小120子项范围大。先精确运行，避免与fullverify/评分并发。

## D阶段最小实际验收命令

以下为待协调执行命令，**本次文档迁移未运行**。先冻结D源码与实际二进制，再逐个串行；脚本provider无真实模型费用。

```bash
# 1. 已有真实二进制工具矩阵及两进程resume。
go run ./internal/verify/cmd/test-gate \
  -go-arg=-race -go-arg=-count=1 -go-arg=-timeout=10m \
  '-go-arg=-run=^(TestRealBinaryPromptToolApprovalMatrixAndErrorRecovery|TestRealBinaryPromptResumesDurableSession)$' \
  -package=./tests

# 2. PR9已有受管工具local+bare remote旅程、聚合未知资源负例。
go run ./internal/verify/cmd/test-gate \
  -go-arg=-race -go-arg=-count=1 -go-arg=-timeout=10m \
  '-go-arg=-run=^(TestPromptDurableReviewedToolsLocalAndBareRemoteJourney|TestPromptDurableGitAggregateWaitsForLaterProcessAndRetainsUnknownReader)$' \
  -package=./internal/cli

# 3. 只跑已有120轮真实SSE二进制子项，不同时启动PTY其它旅程。
go run ./internal/verify/cmd/test-gate \
  -go-arg=-race -go-arg=-count=1 -go-arg=-tags=e2e -go-arg=-timeout=10m \
  '-go-arg=-run=^TestE2EBinaryLongTask$/^120_real_binary_turns_compact_streamed_history$' \
  -package=./tests

# 4. 已有durable截断投影重开（组装入口），与120轮结果分层记录。
go run ./internal/verify/cmd/test-gate \
  -go-arg=-race -go-arg=-count=1 \
  '-go-arg=-run=^TestPromptConfiguredTruncatingCompactionRestartsWithExactProjection$' \
  -package=./internal/cli
```

这些命令的函数/子项名已静态核对。首批外部profile为`main-5736e88-personal-memory-off-v1`：fresh HOME、个人memory关闭、第三方extensions/skills/MCP未加载，保留核心工具/session/compaction；不是产品全部默认能力的验收。D若移植额外single-status/parallel四工具CLI回归，须单独绑定最终文件/commit后登记命令，不依赖旧C专用tools测试文件。`OFFLINE_FIXTURE_DONE`不能替代每个tool receipt/proof/resolution。

## R2/R3必要增补，先复用已有fixture

- R2文件链：find/grep/read→write→read→edit→read→final，逐tool ID/后续请求和独立最终文件hash。Bash链：确定失败检查→read/edit→重跑成功→final。沿用binary tools本地peer，不另造平台。
- R2 Git：优先现有durable journey；若业务断言不足，在该fixture补独立Git进程验证HEAD parent/tree/commit count与bare ref。是否需要另加小真实binary六请求链，由现有链实际结果决定；旧“先修unmanaged”不适用于main。
- R3：在既有120 fixture补可数压缩事件≥2及前后预算；truncating与summary分开。早/中/晚canary由最终产物验收，不能只验证旧turn删掉。
- 复用durable截断重开与binary两进程resume，组合一个显式session store的真实进程长任务：压缩后退出→同physical ID新进程→完成待办；再补一个副作用边界中断，unknown不擅自重放。不把不同测试分别PASS拼成不存在的一条旅程。
- worker/子Agent/MCP/hook与memory已有更广新main覆盖；逐机制先实际跑代表旅程，再按缺口补长链，不用旧base审计表代替新base事实。

## 已执行状态边界

| 证据 | 状态 |
|---|---|
| 新main源码/关键测试断言与命令名盘点 | 只读完成；未运行tests/模型 |
| 旧B Nav | reward0、runtime failure；Pi历史reward1，永久保留 |
| 旧C | 无费单/并行CLI与目标测试有PASS；没有真实模型任务评分；旧全量日志不等于D通过 |
| D真实二进制/受管journey/120轮/压缩续接 | 待实际执行与结果绑定，不预填PASS |
| D make verify | 由协调者安排；需新工作树新日志，不沿用旧base状态 |
| 额外10题 | 锁字节不变，3development+7holdout，均未解封/未跑模型 |

每个后续PASS需绑定完整commit/diff/binary SHA、平台、命令、test-gate started/pass/fail/skip与日志hash；未匹配、未启用tag、被skip或前置失败都不是通过。本文是行为合同覆盖盘点，不是语句覆盖率或全面认证结果。
