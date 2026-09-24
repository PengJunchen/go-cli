# R0 TUI lane evidence

Branch: `codex/runtime-r0-tui`  
Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/tui`  
Scope: UI001/UI003 ask_user delivery, approval/HITL request queue and cancellation, and hermetic ANSI tests.

## Result

The production `teaModel.Update` now renders `ask_user` requests and returns answers through the existing CLI emitter protocol. Approval and HITL prompts share a FIFO modal queue; internal interaction tokens bind cancellation to one waiter even when external approval IDs collide. Resolving or canceling an interaction closes its completion signal so its context watcher exits. Closing the approval source denies pending approvals and stops that reader while leaving the main TUI event loop active. A canceled approval is denied even if its cancellation message has not yet reached `Update`; the callback also discards an `AlwaysAllow` response if its context is already canceled.

The CLI message names remain available as aliases to TUI-owned message types, avoiding an import cycle. `TeaApprovalCallback` forwards its request context into the TUI. No approval cache behavior was changed.

ANSI contract tests now clear inherited `NO_COLOR` only inside the test process and pin a deterministic color profile. `TestNoColorSuppressesANSI` still sets `NO_COLOR=1` explicitly and verifies plain output.

## Regression evidence

Baseline failures are captured in `evidence/tui-red.log`: both production Bubble Tea ask_user journeys timed out because the real model dropped the message, and the concurrent approval regression exposed the old single-slot behavior. `evidence/tui-cancel-race-red.log` captures an `AlwaysAllow` decision slipping through when the canceled-request check is removed from `Update`; `evidence/approval-context-race-red.log` captures the callback mapping that same stale `AlwaysAllow` response to approval when its context check is removed. `evidence/tui-no-color-before.log` records ANSI assertions failing under inherited `NO_COLOR=1`.

After the fix, the same production ask_user journeys, concurrent approval/modal ordering, per-request queued cancellation, approval-channel closure, answered-watcher exit, and keyboard response paths pass. Reverse-order approval requests receive their own responses; duplicate external request IDs do not affect cancellation identity.

## Test evidence

All commands ran from the worktree. Test-list commands and targeted commands exited 0; the full affected-package race command exited 0.

| Evidence | Exact command | Exit |
|---|---|---:|
| TUI test list | `rtk proxy go test ./internal/tui -list '^(TestTeaModel_ApprovalEntryDisplayed|TestTeaModel_ApprovalKeyY|TestTeaModel_ApprovalKeyN|TestTeaModel_ApprovalKeyA|TestTeaModel_ApprovalDiffPreview|TestTeaModel_ApprovalDiffToggle|TestApprovalRequestMsg_UpdateFlow|TestTUIConcurrentApprovalRequestsRemainResolvable|TestTUIAskUserMessageReachesHandler|TestTUIAskUserFreeTextReachesHandler|TestAskUserCancelUnblocksWait|TestTUIApprovalAndHITLModalsFollowArrivalOrder|TestTUICancelResolvesEveryQueuedInteraction|TestTUIClosedApprovalChannelResolvesQueuedRequests|TestTUIQueuedApprovalCancellationKeepsOtherRequestResolvable|TestCanceledApprovalCannotBeApprovedBeforeCancelMessage|TestAnsweredInteractionsStopContextWatchers)$'` | 0 |
| CLI test list | `rtk proxy go test ./internal/cli -list '^(TestHITLQuestionReachesRealBubbleteaModel|TestHITLQuestionCancellationRemovesRealTUIWait)$'` | 0 |
| Approval test list | `rtk proxy go test ./internal/approval -list '^(TestTeaApprovalCallback_SendsRequest|TestTeaApprovalCallback_DeniesAlwaysAllowResponseAfterCancellation)$'` | 0 |
| TUI targeted | `rtk env NO_COLOR=1 rtk proxy go test ./internal/tui -run '^(TestTeaModel_ApprovalEntryDisplayed|TestTeaModel_ApprovalKeyY|TestTeaModel_ApprovalKeyN|TestTeaModel_ApprovalKeyA|TestTeaModel_ApprovalDiffPreview|TestTeaModel_ApprovalDiffToggle|TestApprovalRequestMsg_UpdateFlow|TestTUIConcurrentApprovalRequestsRemainResolvable|TestTUIAskUserMessageReachesHandler|TestTUIAskUserFreeTextReachesHandler|TestAskUserCancelUnblocksWait|TestTUIApprovalAndHITLModalsFollowArrivalOrder|TestTUICancelResolvesEveryQueuedInteraction|TestTUIClosedApprovalChannelResolvesQueuedRequests|TestTUIQueuedApprovalCancellationKeepsOtherRequestResolvable|TestCanceledApprovalCannotBeApprovedBeforeCancelMessage|TestAnsweredInteractionsStopContextWatchers)$' -count=1` | 0 |
| CLI production model integration | `rtk env NO_COLOR=1 rtk proxy go test ./internal/cli -run '^(TestHITLQuestionReachesRealBubbleteaModel|TestHITLQuestionCancellationRemovesRealTUIWait)$' -count=1` | 0 |
| Approval callback | `rtk env NO_COLOR=1 rtk proxy go test ./internal/approval -run '^(TestTeaApprovalCallback_SendsRequest|TestTeaApprovalCallback_DeniesAlwaysAllowResponseAfterCancellation)$' -count=1` | 0 |
| Full affected packages with race detector | `rtk proxy env GOMAXPROCS=2 NO_COLOR=1 go test -race -p=2 ./internal/tui ./internal/cli ./internal/approval -count=1` | 0 |
| Full TUI under `NO_COLOR=1` | `rtk env NO_COLOR=1 rtk proxy go test ./internal/tui -count=1` | 0 |
| Product no-color behavior | `rtk env NO_COLOR=1 rtk proxy go test ./internal/tui -run '^TestNoColorSuppressesANSI$' -count=1 -v` | 0 |
| Full TUI with ambient environment | `rtk proxy go test ./internal/tui -count=1` | 0 |
| Whitespace check | `rtk git diff --check` | 0 |

Corresponding outputs are in `evidence/tui-test-list.log`, `evidence/cli-test-list.log`, `evidence/approval-test-list.log`, `evidence/tui-targeted.log`, `evidence/cli-targeted.log`, `evidence/approval-targeted.log`, `evidence/tui-race.log`, `evidence/tui-no-color-after.log`, `evidence/tui-no-color-contract.log`, and `evidence/tui-ambient-after.log`. The late cancellation regressions' baseline failures are in `evidence/tui-cancel-race-red.log` and `evidence/approval-context-race-red.log`.

Environment comparison: the host shell exported `NO_COLOR=1` during final verification. The baseline `NO_COLOR=1` suite failed ANSI assertions; after the fix both the explicitly isolated and ambient runs pass the full TUI package, while the explicit product test still verifies that application rendering suppresses ANSI when requested. The test harness changes process-local test settings only; it does not change the user's shell or runtime environment.

Commit SHA is reported with the lane delivery after commit creation.
