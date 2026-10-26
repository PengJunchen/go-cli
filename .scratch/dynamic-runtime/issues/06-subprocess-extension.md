# 06: 可选子进程扩展的版本预算与退出

**Publication:** unpublished-to-tfs

**Status:** verified-local

**User story:** 作为第三方扩展用户，我需要版本化子进程调用与有界退出，以便扩展故障不会拖垮主会话。

**Goal:** 外部示例通过host握手/注册/调用/取消/关闭，工具与provider沿公开SDK消费。

**What to build:** 外部示例通过host握手/注册/调用/取消/关闭，工具与provider沿公开SDK消费。

**Blocked by:** 01: 确定性代际进入真实运行; 05: 公开SDK定制能力被真实CLI消费

## Acceptance criteria

- [x] AC-06-1: 请求ID/握手版本/最大帧/queue/inflight/deadline有界；无效响应与崩溃分类。
- [x] AC-06-2: 默认不继承任意宿主env；cwd与允许env明确；撤权和关闭有界terminate并wait。
- [x] AC-06-3: 候选失败不发布，无盲目重放有副作用调用；状态unknown/partial真实；不称OS沙箱。

## Executable verification

以下为复跑入口；本轮实际执行的完整 `make verify` 已覆盖对应race测试。

- `go test -race -count=1 ./internal/extension ./pkg/extensionapi/v1 -run "RPC|Subprocess|Protocol"`

## Verified evidence

`logs/make-verify-final3.log` exit 0，包含 `TestExtensionProcessDoesNotInheritHostSecrets`、`TestRPCTimeoutFrameAndDisconnectCleanProcess` 的hang/oversize/disconnect/bad-shape/panic、`TestExtensionHandshakeAndChecksumRejectFailedCandidate`、`TestRPCArtifactRejectsFIFOWithoutWaitingForWriter`、`TestProjectRPCMaterializationPreservesLoaderSuffixAndCapturedCwd` 通过。RPC结果形状/transport失败保留outcome unknown；从未增加自动重放。Unix进程组与其他平台直接进程的限制如规格所述，不声称OS沙箱、Windows本机运行或实时token streaming已验证。

## Rollback

保持opt-in关闭；终止并等待已拥有子进程，不改native默认路径。

## Rule mapping

Interface-First; Trace Logging; existing Mock contracts; go-cli-infra Go validation and verify rules. No protected-file changes.
