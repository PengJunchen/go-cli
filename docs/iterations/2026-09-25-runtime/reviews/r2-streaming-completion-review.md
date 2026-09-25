# R2 streaming completion 独立源码复审

复审固定源码快照 `9d1a8c24e9f597edc32ebff54b5c81da1c4d74db`，独立 worktree 为 `r2-streaming-completion-review-9d1a8c2`。范围是 tools prepared streaming sink/completion、Streaming Bash 本地进程生命周期和 core 的 EventStream sink adapter；不修改产品源码。结论：proof 来源与 sink 身份检查总体符合已批准的窄边界，但公开 attestation wrapper 没有处理两个 reader 并发调用的问题，当前 seam 仍有一项 P2 blocker。另有一个计时型回归测试存在非确定性风险。此结论不代表 durable Loop 已接线。

## 发现

### R2SC-01 — 同步 sink attestation 允许同一 sink 并发执行 Send（P2，确认）

`StreamingBashToolImpl.ExecuteStreaming` 由 `managedLocalCommand.collect` 启动两个 reader goroutine 分别读取 stdout 和 stderr（`internal/tools/process_scope.go:93-116`）；`readPipe` 在 reader goroutine 内直接调用 sink（`internal/tools/streaming_bash.go:320-337`）。因此两个输出流可同时进入同一个 sink。

`AttestSynchronousStreamSink` 的合同只要求每次 `Send` 同步返回且不脱离 invocation-owned work（`internal/tools/prepared_stream_sink.go:16-23,40-48`）。包装器的 `Send` 直接调用底层方法，没有串行化（`:33-38`）。这意味着一个同步但非并发安全的 sink（例如共享 `bytes.Buffer` 或状态对象）仍可在已 attested 的执行中发生数据竞争；“同步”本身不代表多个调用互斥。`StreamSink` 接口也没有定义并发调用语义（`internal/tools/streaming_bash.go:25-33`）。默认 `EventStreamImpl` 有自己的并发保护，但不能据此认为公开 attestation 工厂接纳的其他 sink 都安全。

独立临时 probe 使用 `RegisterDefaults` 注册的 `StreamingBash`、stdout/stderr 两个输出流和一个每次 `Send` 都阻塞到测试释放的 sink；在第一次回调仍阻塞时，第二个回调也进入了 sink。probe 用 atomic 计数确认重叠，不含自身数据竞争。命令：

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -run '^TestReviewAttestedSinkCanReceiveConcurrentReaderCallbacks$' -count=5 -v ./internal/tools
```

固定快照 5/5 通过，证明并发可达。临时 probe 文件 `internal/tools/review_stream_sink_concurrency_probe_test.go` 已从 review worktree 删除。最小修复是在私有 `synchronousStreamSink.Send` 中用同一个 mutex 包住底层 `sink.Send`，并加一个双流 gate 回归：第一个 callback 持锁等待时第二个不得进入，释放后两者均完成，最终 proof 仍须等两条 reader 和完整 executor pipeline 结束。也可选择把并发安全列为 attestation 的明确前置条件，但这会把关键 correctness 义务留给每个宿主调用方；当前批准的默认 sink 不受此问题影响。

### R2SC-T01 — descendant join 回归依赖 `sleep 1` 和固定延迟（非功能 blocker）

`TestPreparedStreamingBashCompletionWaitsForSinkAndDescendantJoin` 用 `sleep 1 & ...; wait`，sink 回调开始后睡眠 100ms 再释放，随后只观察 100ms（`internal/tools/prepared_completion_test.go:629-681`）。若测试进程在 sink 开始后被调度暂停约 800ms 以上，descendant 可能在 sink 释放前已经结束；释放后 completion 可以合法立即关闭，测试却会以“descendant still running”失败。当前 `-race -count=3` 通过，但固定延时不能保证慢机器或压力环境不触发误报。

建议改为可控 FIFO/文件 gate 或小型 helper child：测试先收到 child 已启动并阻塞于 gate 的确认；释放 sink 后验证 completion 仍开，再显式放行 child 并等待 proof。这样直接证明资源存续，而不把 wall-clock 延时当作存活证据。

## 已确认的边界

- attestation interface 由包内未导出方法封闭；外部只能经 `AttestSynchronousStreamSink` 得到私有 `*synchronousStreamSink`。Prepare 接收并保存该精确包装器；Dispatch 通过类型断言和包装器指针身份比较，不对任意 sink interface 做 `==`。不同 wrapper、raw sink 和 non-comparable sink 在执行前拒绝；对应测试确认不提交 dispatch fence、不运行命令，拒绝尝试仍消耗 one-use permit（`prepared_registry.go:27-43`、`prepared.go:793-825`、`prepared_completion_test.go:684-724`）。
- 普通 `PrepareToolCall` 后调用 `DispatchStreaming` 时，dispatch 会清除 process completion proof；它不会借用普通 no-sink Dispatch 的注册 proof（`prepared.go:836-840`）。该路径可用于非 durable 兼容调用，但 durable consumer 必须在 dispatch 前选择 prepared streaming attestation seam，并对 nil proof fail closed。
- 只有受信任的 `*StreamingBashToolImpl` 注册能获得该本地进程 proof，且 sandbox 必须是审阅过的 `AllowAllSandbox` 或 `DefaultBashSandbox` 类型；普通 custom streaming executor 或其他 sandbox 不继承 proof（`trusted_registry.go:242-264`、`prepared_completion_test.go:797-860`）。执行包装器存在且未被 assembly 明确标为 synchronous 时 proof 会清空（`prepared_registry.go:79-81`）。CLI assembly 只对固定 wrapper 链及同步内建 sidecar 设置此标志（`internal/cli/assemble.go:1230-1249`）。
- streaming process proof 要求本地 executor pipeline 返回，并且 process leader、owned process group 与两个 reader 都已 join；`forcedPipeClose` 不生成 containment proof（`process_scope.go:28-30,141-164`、`prepared_completion.go:138-157,213-224,266-279`）。sink callback 位于 reader 内，因此 reader join 也覆盖 sink 返回。成功退出和非零退出都能得到 resource-join proof；proof 表示资源已收敛，不代表工具成功。
- `TestPreparedCompletionForcedPipeCloseClosesLocalSignalWithoutProof` 是直接构造 `managedLocalResult` 的状态机单元测试，不能作为 OS forced-close 行为证据。`process_scope_unix_test.go` 有实际进程组清理覆盖，但本次没有证明通用进程树或主动脱离进程组的子进程可被捕获；源码也明确将保证限定为 owned process group。
- `attestedEventStreamSink` 只接受具体 `*EventStreamImpl` 和 nil/默认 `*MemoryEventBus`，拒绝 typed-nil bus、包装 stream 和 custom bus（`internal/core/prepared_streaming_sink.go:5-27`）。不过固定快照中该 helper 及 `PrepareStreamingToolCall` 尚无产品调用点；本复审不把 default durable streaming 的真实 Loop 接线、RunCommitConsumer 路径或 CLI E2E 标为完成。
- 取消错误沿 `localLifecycleError` 保留 `context.Canceled`，现有 `TestStreamingBashCancellationWaitsForReaders` 用 `errors.Is` 验证，并在返回后确认 reader 不再继续发事件。本次只对现有工具路径做定向测试。

## 验证

先用 `go test -list` 确认了 tools 和 core 两包的目标测试。固定 snapshot 的定向 race 测试通过：

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=3 ./internal/tools -run '^(TestPreparedStreamingBashAttestedSinkProvesNormalAndErrorCompletion|TestPreparedStreamingBashCompletionWaitsForSinkAndDescendantJoin|TestPreparedStreamingBashRejectsSinkReplacementBeforeCommit|TestPreparedStreamingBashDispatchAndDispatchStreamingShareAdmission|TestPreparedStreamingBashCompletionWaitsForAttestedWrapperUnwind|TestPreparedStreamingBashDoesNotAttestUnsupportedPipeline|TestPreparedStreamingBashCustomSandboxDoesNotProvideProcessProof|TestPreparedStreamingCustomExecutorDoesNotInheritCompletionProof|TestPreparedCompletionStreamingBashHasNoProofWithoutTrustedSink|TestPreparedCompletionRegisteredStreamingBashDirectDispatchJoinsProcessGroup|TestAttestSynchronousStreamSinkRejectsNilAndTypedNil|TestAttestedSinkIdentityCheckDoesNotCompareArbitraryInterfaces|TestStreamingBashCancellationWaitsForReaders|TestStreamingBashReportsPreparedProcessCompletionAfterNormalAndError)$'
```

结果：`ok github.com/pengjunchen/go-cli/internal/tools 4.412s`。core helper 的目标 race 测试也通过 10 次：

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=10 ./internal/core -run '^TestAttestedEventStreamSink'
```

结果：`ok github.com/pengjunchen/go-cli/internal/core 1.034s`。没有运行全量 tools/CLI 测试；没有改动产品代码。review worktree 在清理临时 probe 后保持干净。

## Follow-up: serialized attested sink

复审固定修复快照 `4702be7edffd372b1165b48fe11439f5de413cb3`，parent 为本报告首轮审查的 `9d1a8c24e9f597edc32ebff54b5c81da1c4d74db`。修复只改 `internal/tools/prepared_stream_sink.go` 和其测试；R2SC-01 已关闭。

私有 `synchronousStreamSink` 新增互斥锁，并在底层 `sink.Send` 整次调用期间持锁，因而 `StreamingBash` 的 stdout/stderr readers 不会并发进入用户 sink。注释明确说明该保证及禁止底层 callback 递归调用同一个 wrapper 的限制（`prepared_stream_sink.go:16-19,25-27,33-43`）。这不改变 exact-wrapper 身份校验、one-use permit、completion proof 或 sink error 传播；底层 Send 返回后才释放锁，reader join 仍是整个 callback 的 join 屏障。

新增的 `TestAttestedSynchronousStreamSinkSerializesConcurrentSend` 用一个可控 gate 阻塞首个底层 Send，确认第二个调用不能重叠进入；释放首个调用后确认两个调用都返回。该断言与旧快照上的真实 `StreamingBash` 双 reader probe 对应，关闭了首轮确认的并发缺口。未发现此两文件 delta 的其它问题，接受 R2SC-01 修复。

固定快照定向验证：先用 `rtk env GOMAXPROCS=2 go test -list '^TestAttestedSynchronousStreamSinkSerializesConcurrentSend$' ./internal/tools` 确认命中，再运行：

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=20 ./internal/tools -run '^TestAttestedSynchronousStreamSinkSerializesConcurrentSend$'
```

结果：`ok github.com/pengjunchen/go-cli/internal/tools 1.631s`。另以 `-race -count=2` 重跑了 prepared Streaming Bash 的 normal/nonzero proof、sink barrier/descendant、sink replacement、wrapper unwind、unsupported pipeline/custom sandbox/custom executor、unattested sink 和 cancellation reader 等目标测试，结果：`ok github.com/pengjunchen/go-cli/internal/tools 3.293s`。按任务范围没有重跑全量 tools/CLI race。

R2SC-T01 的 `sleep 1`/固定延时测试脆弱性仍作为非功能 blocker 记录；此 follow-up 没有更改该测试。当前可接受已审查的 streaming sink/completion 源码 slice，但 `attestedEventStreamSink` 与 `PrepareStreamingToolCall` 在本快照仍无生产调用点，durable Loop/RunCommitConsumer/CLI 接线必须另行验证，不能据此宣称端到端完成。
