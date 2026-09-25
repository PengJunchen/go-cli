# R2 streaming consumer 独立源码复审

复审对象为固定快照 `a7b1919ebdbbdbfbfb9786fccb15efcb0375b890`，父提交为 `07dddc1`。该提交只改动 `internal/core/run_commit_consumer.go`、consumer 测试和真实 session 测试。结论：流式 capability 检查、attested sink 绑定、真实 session 持久化和并行混合调用顺序均符合本 slice 的约定；本次未发现阻断项，**接受该 bounded slice**。

## 复审结果

- `prepareInvocation` 只用 `registry.Get` 识别当前 `StreamingBashTool` capability。确认流式调用后，它先校验 core event stream 是否为受信任的同步 sink，再调用一次 `StreamingToolCallPreparer.PrepareStreamingToolCall`；实际 registration identity、授权、schema 和 stages 由这次 Prepare 冻结。sink 不可 attestate 或 preparer capability 缺失时，流式调用在策略和 executor 前以 `ErrRunCommitStreamingUnsupported` 失败。普通工具仍通过既有 `PrepareToolCall` 路径；没有 event stream 时也保留普通 `Dispatch` 路径。
- 同一个已 attestate sink 随 prepared invocation 传到 `DispatchStreaming`。tools 层的 prepared invocation 固定该 sink identity，并在最终 dispatch fence 前拒绝替换；默认 `EventStreamImpl` 只有搭配 nil 或非 nil `MemoryEventBus` 才被 core adapter attestate，未知 bus/wrapper fail closed。
- Sequential 与 parallel consumer 共用该准备/dispatch helper。parallel action 自己保留对应的 sink；worker 不写 durable sink，Loop 在 worker join 后按原 assistant call slot 写 execution 与 receipt。真实 Session 混合测试覆盖 Streaming Bash 与普通 Read 同组，断言 receipt 物理顺序为 `bash-parallel-1`、`read-parallel-1`，关闭并重开后仍能通过 session sink 验证。
- 真实 Session 单调用测试执行有 stdout/stderr 的 Streaming Bash，验证两个 output event、完成证明、execution row、成功 receipt、terminal checkpoint 和 dispatch marker resolution；关闭并重开后 sink 验证成功。
- 未知 event bus 的对抗测试使用真实 session sink，断言在 prepared policy、event publish 和命令执行之前返回 typed unsupported error；不存在命令副作用或 action intent，receipt 明确为 `not_dispatched`，terminal outcome 为 error。

## 验证

执行前先列出测试。定向 consumer 测试为：

```text
TestRunCommitLoopStreamsPreparedBashAndReopensWithSessionSink
TestRunCommitLoopStreamsBashAlongsideNonStreamingToolInCanonicalOrder
TestRunCommitLoopRejectsUnattestedStreamingSinkBeforePolicyOrDispatch
```

以上三项通过。tools sink/prepared-streaming 定向列表包括 normal/error completion、descendant join、sink replacement pre-commit、dispatch admission、wrapper unwind、unsupported pipeline、custom sandbox、nil/typed-nil attestation、exact forwarding 和 concurrent send serialization；全部通过。

以下检查也通过：

```text
rtk proxy git diff --check a7b1919ebdbbdbfbfb9786fccb15efcb0375b890^ a7b1919ebdbbdbfbfb9786fccb15efcb0375b890
rtk proxy go test -race -count=1 ./internal/core ./internal/tools
rtk proxy golangci-lint run ./internal/core ./internal/tools
```

Race 结果：core `ok`（4.656s），tools `ok`（42.288s）；lint 为 `0 issues`。

本报告只接受 `a7b1919` 的 core/tools 流式 consumer slice。它不证明 CLI Assembly/REPL wiring、session manager shutdown/lease close、完整全仓验证或 UAT 已完成。
