# R2 Core Loop consumer 独立源码复审

复审对象为固定源码快照 `3e555959543b8cdc827120c6a8a33b9d8b3e3aa4`，其 Loop consumer 基线为 `14e7b2f5e4cdbdfe4b9037e20498ff147720a301`。`3e55595` 中并行取消结果快照修复已单独验证通过；本报告不把它之前已知的 `action.result` 并发读写列为新问题。结论：核心提交顺序与未知结果处理大体符合已批准合同，但该快照仍有一项可触发进程崩溃的 late-drain panic blocker，以及一项重复登记同一 Bash 资源的合同偏差，暂不批准此 slice 完成。

## 发现

### R2CC-01 — late completion sink panic 会逃出 supervisor 并终止进程（P1，确认）

`runOwner.supervise` 在 callback、worker 和资源都已 join 后直接调用 `drainLateCompletionTasks`，见 `internal/core/run_owner.go:911-953`。该函数会在 supervisor goroutine 中调用 `RunCommitSink.RecordExecution` 和 `ResolveExecution`，见 `:990-1045`。这条晚写路径没有 `recover`；普通 run callback 则由 `invokeRunCallback` 恢复并转成未知/quarantined 状态（`:899-909`），lease close 也有专门的 panic containment（`:1048-1055`）。

触发条件是某个已持久化 receipt 的 completion task 在 Loop 安全点之后变为 ready，且 sink 在 late `RecordExecution` 或 `ResolveExecution` 中 panic。一个临时、仅用于复现的子进程测试构造了 `receiptDurable=true`、ready 且 proof 有效、terminal 已同步的 task，并让扩展 sink 的 `RecordExecution` panic。命令：

```text
rtk env GOMAXPROCS=2 go test -p 2 -run '^TestReviewLateSinkPanicProbe$' -count=1 -v ./internal/core
```

父测试捕获到子测试进程以 `panic: review late execution sink panic` 退出；堆栈落在 `run_owner.go:1031`，由 `run_owner.go:947` 的 supervisor 调用。临时 probe 文件已从独立 review worktree 删除。`RunCommitSink` 是由调用方绑定的扩展接口，没有“不 panic”约束，因此这条路径可达；panic 会跳过后续 per-run lease close、run-slot release 和状态发布。

最小修复是给 late drain 提供与 callback 等价的 panic containment，把 panic 作为持久化结果未知的错误处理，不重试、不假装 resolution 成功，并让 supervisor 继续执行已 join 资源的 lease/slot cleanup，最终状态保持 unknown/quarantined。应分别覆盖 `RecordExecution` 与 `ResolveExecution` panic；后者即使底层已经 append，也必须按结果不确定处理。

### R2CC-02 — prepared Bash 路径把同一进程资源计数两次（P2，源码链确认）

`dispatchPreparedRunCommitCall` 对每个 `PendingExecutionError` 无条件调用旧的 `registerRunPendingResource`（`internal/core/run_commit_consumer.go:96-105`），随后 defer 又把预注册的 completion task 交给 `runOwner`（`:78-90`）。已批准的 R2 handoff 规定 prepared completion task 负责这个 action 的资源计数；prepared 路径不再为同一 completion 再走旧 pending hook。

这不只是抽象风险：默认 CLI 将 `*BashTool` 与 `DefaultBashSandbox` 注册时会启用 `CompletionOwnedProcessGroupJoined`（`internal/tools/trusted_registry.go:233-243,270-282`；CLI wiring 在 `internal/cli/assemble.go:857,912`）。`BashTool.Execute` 调用 `reportPreparedLocalProcessCompletion(ctx, lifecycle)`（`internal/tools/bash.go:207-211`）；当未能证明 quiescence 时，又把同一个 `lifecycle.executionDone` 放进 `PendingExecutionError`（`:253-257`）。reporter 将该 channel 接入 prepared signal（`internal/tools/prepared_completion.go:263-275`），completion state 另外观察它并关闭自己的 action-scoped channel（`:164-195,210-220`）。因此旧 hook 和 prepared task 建立两个 owner resource 计数和两个观察者，底层指向同一次本地进程/reader join。

目前没有证据表明该双计数会过早释放 lease；它偏向安全侧，可能短暂重复报告 pending 并多等一次调度。但它偏离单一所有权合同，并使 status 的 pending 计数不能对应独立资源。建议当已注册的 prepared signal 提供最终 `Done` 时只让该 signal 负责资源跟踪；对 `Done()==nil` 的自定义/遗留 prepared invocation，仍保留 `PendingExecutionError.ExecutionDone` fallback，不能一刀删除旧 hook。回归应覆盖默认 Bash pending 生命周期只计一次，以及 nil prepared signal 的 legacy pending fallback 仍能在 channel 关闭前保留 owner、关闭后释放。

## 已验证的实现边界

- `RunCanonical` 在首次 provider 调用前提交初始 input；绑定、owner、RunID 不匹配时在 provider/tool 入口前失败。steer/follow-up 作为后续输入在下一次模型调用前提交。run 终止时提交 canonical transcript terminal checkpoint。对应 `internal/core/loop.go:372-450` 及输入插入路径 `:590-625`。
- Sequential 路径对单个调用 prepare、同步 intent、注册 completion task、dispatch 同一 invocation、同步 execution/receipt，再等待 proof 并 resolution；没有 proof 或存储错误时阻断后续 provider/调用，见 `run_commit_consumer.go:361-494`。
- Parallel 路径先 prepare 全组并提交一批 intents；结果与 known denials 按原始 call slot 写 receipt，worker 不直接写 sink，见 `:528-750`。3e 修复用每 action 的 `done` close 建立结果同步，仅复制已完成的 `action.result`；未完成调用生成 synthetic unknown，而不是读取活 worker 的字段（`run_commit_consumer.go:607-659`）。
- owner 的 completion handoff 在其锁内由 worker placeholder 转为 resource observer，避免 owner worker/resource 计数同时归零；late proof 不由 watcher 写入，而由 owner supervisor 在 terminal sync 后统一 drain。无 proof 时不会自动重试或清除 marker。
- worker helper 的 defer 会先完成 prepared task handoff，再由外层 goroutine 写本地 result、close `action.done` 并 `workers.Done`（`run_commit_consumer.go:592-600`）。取消时未完成的 action 已被持久化为 unknown，Loop 不读取之后才写入的结果；若 prepared `Done` 仍开，handoff 会将它转成 owner resource，若已闭合则执行器资源已 join。外层 handoff 后只剩本地结果/同步 bookkeeping，没有发现仍需 session lease 的工具或 sink 工作；这项先后次序在本快照中不是独立 blocker。
- Durable compaction 在阈值触发时以 `ErrRunCommitCompactionProjectionUnavailable` fail closed，不调用 summarizer，也不向下一轮 provider 请求；子 agent 在继承 durable binding 的情况下于 child provider 前返回 typed unsupported error。

## 验证与证据边界

先用 `go test -list` 确认 consumer 与 owner cleanup 测试命中。独立定向 race 命令如下，固定快照通过 20 次：

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=20 ./internal/core -run '^TestRunCommitConsumer|^TestRunOwnerCancellationKeepsClaimUntilWorkerStops$|^TestRunOwnerNonCooperativeCallbackReturnsPendingThenQuiesces$|^TestRunOwnerPendingExecutionResourceHoldsClaimUntilDone$|^TestCleanupPendingAfterQuiescencePreservesCompletedResult$'
```

覆盖项包括顺序调用的 input/intent/execution/receipt/resolution/terminal 顺序；实际 `AgentImpl + LoopAgent` 下的单组 parallel receipt 顺序；proof 缺失与 compaction fail-closed；取消时共享 cleanup deadline 与 input binding；child fail-before-provider；dispatch panic unknown；新的 gated parallel-cancellation live-result 回归；以及 owner 取消、quarantine、pending resource 释放。结果为 `ok github.com/pengjunchen/go-cli/internal/core 6.587s`。

该组测试使用内存 `consumerTestSink`，不能证明真实 session `AppendAndSync`、workspace index fence、lease manager 或重启恢复。当前 parallel 测试也没有用可控慢/快 executor 证明反序完成、deny 与多个 receipt 混排仍按 canonical index 写入。真实 session/restore 集成测试固定并通过前，不应把 R2 durable consumer/recovery 标为完成；该 gate 与本次 core-only review 分开跟踪。

## 固定快照 `14934f3ffc3075a68184383f3bc74512a1dec2ed` 增量复审（2026-09-25）

该快照关闭了上述 R2CC-01 与 R2CC-02，但仍不批准 core consumer slice：发现新的 sequential proof/resolve 排序竞态 R2CC-03。报告只审到此固定 commit；后续修复需另行复审。

### 先前两项关闭

- **R2CC-01 late sink panic containment：已关闭。** `drainLateCompletionTasks` 对 late `RecordExecution` 与 `ResolveExecution` 分别经 `callLateCompletionSink` 包装（`internal/core/run_owner.go:1076-1090`），panic 只转成不带 panic payload 的 `ErrRunCommitSinkPanicked`。supervisor 将其记为 unknown/quarantined，仍在资源 join 后关闭 run lease、释放 run slot；只有 sink 调用正常返回才设置对应的 durable 标记。`TestRunOwnerLateCompletionSinkPanicsAreContained` 与 `TestRunOwnerQuarantinesLateCompletionSinkPanicsAndReleasesAfterJoin` 分别覆盖两种 panic，断言没有虚假的 execution/resolution durable 状态、不会泄露敏感 panic 文本且清理只做一次。
- **R2CC-02 prepared 路径双计数：已关闭。** `dispatchPreparedRunCommitCall` 只有在 completion task 不接受 `PendingExecutionError.ExecutionDone` 时才走旧的 pending-resource fallback；prepared task 接受时，由单个 action completion group 接管。`TestDispatchPendingExecutionErrorDoesNotDoubleTrackPreparedSignal` 覆盖同一 channel、两个不同 channel、nil handle 和无 prepared task 的 legacy fallback；owner 对同一 action 最多跟踪 prepared signal 与一个 distinct underlying Done，而状态仍按一个 action resource 计数。

### R2CC-03 — completion 在 receipt 持久化期间变为 Ready 时，sequential 路径会先尝试 Resolve（阻断）

`executeSequentialCall` 在写第一条 execution row 前读取一次 completion details（`internal/core/run_commit_consumer.go:425-444`），随后调用可能阻塞的 `recordActionReceipt`（`:447-451`）。`resolvePreparedTask` 再次读取状态；当进入函数时 task 已是 Ready 且 proof 有效，它会跳过整个 `if` 分支（`:475-480`），但补写缺失 proof row 的逻辑只在分支内、仅在 `waitForCompletionTasks` 之后执行（`:484-499`），接着直接调用 `ResolveExecution`（`:501-505`）。因此 sink 在 receipt 持久化期间等待时，completion observer 可先将 task 设为 Ready；receipt 返回后这条路径就会尝试在 proof-bearing execution row 之前 resolution。

我在独立 detached review worktree 临时加入 `TestReviewSequentialResolveRechecksReadyAfterReceiptReturns`：测试使用真实 `DefaultFileMutationQueue` 和 prepared `write` invocation，以同步 wrapper 暂停 queued executor；receipt sink hook 放行 executor，并等待 owner 对同一 task 观察到 Ready 与有效 proof 后才返回。sink 随后拒绝任何缺少 proof-bearing execution row 的 resolution。复现命令：

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -run '^TestReviewSequentialResolveRechecksReadyAfterReceiptReturns$' -count=1 ./internal/core
```

命令通过是因为 probe 将当前错误顺序作为断言目标：receipt hook 已观察到有效 proof，而 Resolve 调用时 proof execution 尚未 durable，`earlyResolveCount` 精确为 1。该 probe 连续运行 10 次均重现，之后已从 review worktree 删除，没有改动固定源码。真实 `SessionRunCommitSink` 的 `ResolutionLocalResourcesStopped` 只接受完整 action 引用匹配、且晚于 intent 的正 completion proof execution row；resolver 没有此 row 时 fail closed（`internal/session/run_commit.go:1173-1188`，recovery validation `internal/session/durable_recovery.go:513-519`）。所以真实 sink 会拒绝这次提前 resolution；不能依赖任意 sink 都实现同样防线。

并行路径未发现对应缺口：receipts 全部处理后，它统一重新读取 Ready task；缺少 `proofExecutionDurable` 时先写 late execution row，再写 resolution（`internal/core/run_commit_consumer.go:733-780`）。未完成 dispatch 的 action 则交由 owner 在 terminal sync 后 drain。最小修复是把 sequential 路径整理为统一的“receipt 返回后重新确认 Ready 和有效 proof；缺失 proof row 时先写入并确认 durable；最后才 resolution”步骤，不能只在等待分支补写。

### 固定快照验证

先以 `go test -list` 枚举并确认命中 consumer、completion-group、late-panic、真实 session/reopen 与 marker-preservation 测试。定向 race 回归命令在固定源码加临时 probe 的 review worktree 上连续通过 10 次（`internal/core` 约 5.3 秒）：

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=10 ./internal/core -run '^(TestReviewSequentialResolveRechecksReadyAfterReceiptReturns|TestRunCommitConsumer.*|TestRunOwnerCompletion.*|TestRunOwner(LateCompletionSinkPanicsAreContained|QuarantinesLateCompletionSinkPanicsAndReleasesAfterJoin|PanicStillHandsOffTaskAndReleasesAfterJoin|PendingExecutionResourceHoldsClaimUntilDone|WaitRechecksExactSignalAfterCancellation|WaitAcceptsDoneClosedAtCancellationBoundary|CancellationKeepsClaimUntilWorkerStops|NonCooperativeCallbackReturnsPendingThenQuiesces)|TestRunCommitLoop(PersistsAndReopensWithSessionSink|KeepsMarkerAfterClosedCompletionWithoutProofAndReopen))$'
```

固定快照全包验证与静态检查通过：

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/core -count=1   # ok, 4.453s
rtk go vet ./internal/core                                       # exit 0
```

两条 session 集成测试使用真实临时 JSONL session、workspace quarantine index 和 `SessionRunCommitSink`：成功路径检查 durable rows、resolved marker、关闭后 `PrepareResume` 及重新构造 sink；proofless 路径检查 marker 在 close/reopen 后仍 unresolved。它们证明当前真实存储闭环和 fail-closed 行为，但未覆盖上述 receipt/blocking 时序，因此不关闭 R2CC-03。

## 固定快照 `3dc1c8ff6aa24ff65cdb29a674595dd7901e420e` 增量复审（2026-09-25）

**结论：R2CC-03 已关闭；该 core consumer delta 通过独立复审。** `resolvePreparedTask` 在 receipt 持久化返回后总会重新检查 owner readiness；若当前 proof 已有效而正向 execution row 尚未持久化，则先写完整 occurrence/proof row，成功后才 `ResolveExecution`。因此 completion 在阻塞 receipt 写入期间从 open 变 ready、或函数进入前已经 ready，两条路径都遵守 `receipt → positive proof execution → resolution`。unknown receipt 不会被改写为 success/error，单 action proof 与原 completion resource owner 不变。

固定快照上的定向回归：

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/core -run '^(TestRunCommitSequentialRechecksReceiptTimeProofBeforeResolve|TestRunCommitLoopPersistsLateProofDuringReceiptBeforeResolution)$' -count=5
```

通过（`ok github.com/pengjunchen/go-cli/internal/core 1.475s`）。第二个回归用真实 `SessionRunCommitSink`、durable JSONL session、workspace quarantine index 与 receipt 写入 gate，断言先有 unknown receipt，再有唯一正向 proof execution，最后才有 resolution；marker 得到 resolution，但原 unknown receipt 仍保持 unknown。core 全包 race 与 vet 也通过：

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/core -count=1  # ok, 4.171s
rtk go vet ./internal/core                                       # exit 0
```

### Panic 终态探针：未发现真实 session 中的伪成功

另以临时 `core_test` 探针构造真实 `PreparedSession`、`SessionRunCommitSink` 和已绑定的 `Harness + AgentImpl + LoopAgent`，让 model `Stream` 在初始 `CommitInput` 后 panic。unwind 时 `RunCanonical` 的命名返回值确实仍是零值；其终态 defer 尝试以空 transcript 提交 success。真实 sink 在 `internal/session/run_commit.go` 的 `CommitTerminal` 校验处拒绝 `terminal checkpoint requires the canonical run transcript`，因此 durable snapshot 只有已同步的用户 input，没有 terminal checkpoint；`RunStatusHandle` 的 owner 结果是 `RunOutcomeUnknown`。临时测试以 race 通过后删除，没有改动固定源码。该结果符合 fail closed：没有伪造 terminal success，run 保持 incomplete/unknown。自定义 `RunCommitSink` 必须同样拒绝不满足 canonical terminal 契约的空 transcript；本次真实 sink 路径没有额外 blocker。

### session 包九个旧 sink fixture 失败的归因

命令 `rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/session -run '^TestSessionRunCommitSink' -count=1` 在以下九个测试中失败：

```text
TestSessionRunCommitSinkCommitInputPersistsBeforeReturning (run_commit_test.go:93)
TestSessionRunCommitSinkPersistsIntentBeforeGlobalMarker (run_commit_test.go:262)
TestSessionRunCommitSinkRecordsReceiptBeforeResolvingExactProof (run_commit_test.go:372)
TestSessionRunCommitSinkBuffersCanonicalReceiptOrderAcrossKnownDenial (run_commit_test.go:466)
TestSessionRunCommitSinkCommitTerminalAppendsOnlySuffixAndIsIdempotent (run_commit_test.go:518)
TestSessionRunCommitSinkAllowsBoundedLateProofAfterTerminalWithoutChangingResult (run_commit_test.go:671)
TestSessionRunCommitSinkTerminalRequiresCompleteOrderedDurableProjections (run_commit_test.go:795)
TestSessionRunCommitSinkCompactionUsesRunStartRevisionAndCanonicalSource (run_commit_test.go:922)
TestSessionRunCommitSinkCompactionCannotCutAnOpenToolCallGroup (run_commit_test.go:966)
```

九项都得到相同的 `core: run commit terminal was not synced`。这是 sink 单元测试 helper 的旧生命周期夹具问题，不是本 consumer delta 回归：`runSessionCommitAgentWithContext` 在 `Harness.Submit` 前绑定真实 sink，`runOwner` 因而正确要求 canonical consumer 同步 terminal；fixture 的 `sessionCommitTestAgent.Run` 却只直接调用 sink API，绕过 `runCommitConsumer.commitTerminal`，该 consumer 是设置 owner terminal-sync 成功位的唯一生产路径。保持生产要求不变，最小修复应把 sink/binding 从 Submit 父 context 移入 test agent 的 callback context（在 owner 已建立 RunID、且 owner 未观察到 durable binding 后再绑定），或让这些用例通过真正的 canonical consumer 路径；继续用“已绑定 sink 的 Harness + 直接 sink callback”会违反当前测试前置条件。真实生命周期与 terminal ack 已由 core session integration 测试单独覆盖。

## 固定夹具/并行测试修复 `6b34074ad237277e86f85bb491a4e751c70d59ef` + `4a00a6ba94dd3b2be3d7b4221161e4d1348c4fac` 增量复审（2026-09-25）

**结论：通过，无新增 blocker。** session 的 `runSessionCommitAgentWithContext` 现在将 sink 与 binding 存在测试 agent 上，Submit 父 context 不带 durable sink；agent 进入 callback 后才用已有 owner RunID 绑定，故这批测试明确只审 `SessionRunCommitSink` 的低层持久化 API，不冒充 RunOwner/canonical terminal 协议。helper defer 调 `CloseRunCommit`；sink 原有幂等释放语义不变。首个 sink 测试仍在 helper 返回后重复调用 `CloseRunCommit` 并断言 release callback 仅一次，同时确认 prepared session 仍可 Snapshot，因此没有通过削弱 sink binding 或底层句柄生命周期来修绿。

core 并行取消测试现在等待两个真实 tool handler 都进入 `<-ctx.Done()` 阻塞点后才 cancel，取消结果两项均断言为 unknown，且 provider call index 仍为 1。新 `TestCanonicalLoopPublishesReturnedParallelResults` 单独验证完整返回组两个 tool receipt 均为 success、随后才进行下一次 provider call。原本覆盖 mixed result 的 `TestCanonicalLoopClosesParallelReceiptsOnUnknownToolOutcome` 仍保留，继续断言已 dispatch 的 unknown 和 completed 两项分别形成 unknown/success receipt；未删减此覆盖。

历史 RED 证据也与此解释一致：完整 race JSONL `evidence/integration-default-race-7daf21a.jsonl:16597-16613` 记录旧取消夹具对 `completed-call` 预期 success、实际为 unknown。旧 fixture 的 `completed` channel 在 handler 返回前发出，测试收到信号就取消，不能证明 executor 已返回并被 loop 采纳；实际 unknown 是正确的取消边界结果。新 fixture 不再要求这项竞态成功，而由独立的完整返回组与仍保留的 mixed-outcome 用例验证其余两种行为。

定向测试和完整包 race 均通过：

```text
rtk env GOMAXPROCS=2 go test -list '^(TestSessionRunCommitSink|TestCanonicalLoop(MarksParallelCancellationSnapshotUnknown|PublishesReturnedParallelResults|ClosesParallelReceiptsOnUnknownToolOutcome))' ./internal/session ./internal/core
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/core -run '^(TestCanonicalLoopMarksParallelCancellationSnapshotUnknown|TestCanonicalLoopPublishesReturnedParallelResults|TestCanonicalLoopClosesParallelReceiptsOnUnknownToolOutcome)$' -count=20  # ok, 1.051s
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/session -count=1  # ok, 5.612s
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/core -count=1     # ok, 4.778s
```
