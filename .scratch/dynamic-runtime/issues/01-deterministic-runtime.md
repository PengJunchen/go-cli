# 01: 确定性代际进入真实运行

**Publication:** unpublished-to-tfs

**Status:** verified-local

**User story:** 作为长会话用户，我需要每次运行固定配置与资源 owner，以便刷新不会混合新旧资源；同一代际内的首次 MCP 发现可以增加下一模型请求的能力可见性。

**Goal:** 将配置编译、run准入、模型工具/prompt消费、调用与关闭贯通为一个可审计代际。

**What to build:** 将配置编译、run准入、模型工具/prompt消费、调用与关闭贯通为一个可审计代际。

**Blocked by:** None (can start immediately)

## Acceptance criteria

- [x] AC-01-1: 相同非秘密内容与不同输入遍历顺序得到相同digest和拓扑；循环/缺依赖/重复provider在Init前拒绝，若握手已获得资源则完成关闭。秘密配置只用进程内opaque revision标识，不承诺跨重启复放。
- [x] AC-01-2: 真实Harness和TurnRunner run pin覆盖prompt/schema/dispatch/hooks；旧run合法完成，撤权后新调用和迟到完成失败关闭。
- [x] AC-01-3: 候选失败保留旧代；单飞与current+candidate或current+retiring上限；清理逆序幂等且panic不跳过其余owner。

## Executable verification

以下为复跑入口；本轮实际已完成完整 `make verify`，不声称额外count=20命令已经执行。

- `go test -race -count=1 ./internal/capability ./internal/core ./internal/cli`
- `go test -race -count=20 ./internal/capability -run "Generation|Reload|Lease|Revoke|Scope"`

## Verified evidence

完整 `make verify` exit 0：`logs/make-verify-final3.log` 与 `.exit`，8962 started / 8956 passed / 6 skipped / 0 failed。该日志包含 `TestRunPinsGenerationUntilWorkerQuiesces`、`TestCallResultFencedAfterRevoke`、`TestPinnedRegistrySchemaCacheUsesGeneration`、`TestGenerationLeaseFailureAndInvocationDrain`、`TestGenerationPublishRejectsStaleRevision`、`TestGenerationCoalescesUntilRetiringDrains`、`TestScopePanicReentryAndCloseWaiting` 与实际CLI hook/prompt回归通过。

AC-01-1 的内容hash、defensive copy、缺失/循环、重复provider、Init前拒绝及cleanup已有通过记录（`TestSnapshotDetachesAndRejectsInvalidGraphs`、manifest/Assembly DAG用例）。补证 `TestSnapshotIdentityIgnoresInputTraversalOrder` 已实际通过default/headless race，capability分别1.250s/1.474s（`runtime/final-entry-default-race-3.log`、`final-entry-headless-race.log`），不再仅靠静态排序检查。秘密identity的进程内版本边界已写入规格。

SDK真实入口引出的production成本归属窄修已进入最后源完整make verify final3并exit 0；完整E2E在保留一次并发fixture超时后，以原Make target、GOFLAGS=-p=1串行package方式重跑final4并exit 0，过程详见review-integration。正式性能/交付仍由票07/08跟踪。

## Rollback

关闭dynamic配置回到旧Assembly路径，等待所有lease排空后清理新manager。

## Rule mapping

Interface-First; Trace Logging; existing Mock contracts; go-cli-infra Go validation and verify rules. No protected-file changes.
