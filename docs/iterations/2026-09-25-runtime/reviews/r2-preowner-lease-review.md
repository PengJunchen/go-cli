# R2 pre-owner lease review

## Decision: ACCEPTED

Reviewed commit 31702937305cedb679f6b4ef2a57aef2f49c826e in a separate detached worktree at /Users/pengjunchen/.codex/worktrees/r2-core-preowner-lease-review-3170293. The submitted worktree was clean at the requested commit. The delta changes six files: core Harness and TurnRunner lease capture/transfer, the run-owner lease guard, corresponding core tests, and one CLI test for the real durable manager lease. I read the full production and test diff and the approved R2 CLI production wiring handoff and design follow-up review.

I found no behavioral defect in the submitted lease handoff. Both public entrypoints capture the optional lease before admission/early rejection, keep it with the entrypoint through ordinary pre-owner cleanup, and transfer it synchronously to the owner before execution begins. The guard wraps one close capability in sync.Once; owner fallback adopts from context only when there was no entrypoint-transferred guard. An early cleanup-pending result leaves the lease with the live owner, and the core tests verify release only after the callback joins. Pre-owner close panic is contained and joined with the original rejection without creating a run identity.

### Closed-admission check with the actual durable manager

The specific concern was whether an ordinary lease carried by WithRunCommit could leak when run-slot admission was already closed. I added a temporary reviewer-only test in the detached worktree using newDurableCLISession, bindRun, the resulting real SessionRunCommitSink, and a real DefaultRunSlotGuard. For both Harness.Submit and TurnRunner.RunTurn, the test closed admission first, called the entrypoint with the ordinary bound context, asserted ErrRunAdmissionClosed, then verified manager.requestClose succeeded within one second and the physical session could be reopened with PrepareResume. Both cases passed under race. The temporary test file was removed; the detached worktree is clean.

This confirms the normal context path captures and returns the manager lease after closed-admission rejection. The entrypoint calls RunCommitFromContext before ClaimRun, so it does read the context value before admission; it defers only a panic raised by that lookup until admission succeeds. The comment at internal/core/run_commit.go:172-175 says rejected admission keeps precedence over context reads, which is imprecise. A small wording clarification to say it keeps precedence over panics from context reads would match the behavior. This is a non-blocking P3 comment issue, not a lease leak.

### Verification

The test names were listed before the race commands. Exact commands and results from the independent detached worktree:

~~~text
rtk proxy go test -list '^Test(TurnRunnerNilLoopReturnsLeaseAndPreservesLeasePanic|EntrypointsReturnLeaseAfterClaimFailure|EntrypointsReleaseLeaseAfterWrapperAndClaimCleanup|TurnRunnerTransfersLeaseToOwnerUntilCallbackJoins|TurnRunnerOwnerAdoptsEntryLeaseOnce)$' ./internal/core
  exit 0; all five committed handoff tests listed.

rtk proxy go test -list '^Test(CoreEntrypointReturnsRealDurableManagerLease|DurableCLISessionCreatesPhysicalSessionAndBindsRunLease|DurableCLISessionReleasesLeaseWhenSinkConstructionFails|ReviewClosedAdmissionReturnsActualDurableManagerLease)$' ./internal/cli
  exit 0; all three committed CLI tests and the temporary real-manager probe listed.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/core -run '^(TestTurnRunnerNilLoopReturnsLeaseAndPreservesLeasePanic|TestEntrypointsReturnLeaseAfterClaimFailure|TestEntrypointsReleaseLeaseAfterWrapperAndClaimCleanup|TestTurnRunnerTransfersLeaseToOwnerUntilCallbackJoins|TestTurnRunnerOwnerAdoptsEntryLeaseOnce)$' -count=1
  exit 0; all five targeted core tests passed under race.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/cli -run '^(TestCoreEntrypointReturnsRealDurableManagerLease|TestDurableCLISessionCreatesPhysicalSessionAndBindsRunLease|TestDurableCLISessionReleasesLeaseWhenSinkConstructionFails|TestReviewClosedAdmissionReturnsActualDurableManagerLease)$' -count=1
  exit 0; committed durable-manager lifecycle tests and both closed-admission probe cases passed under race.

rtk git status --short --branch
  exit 0; detached reviewer worktree clean at 31702937305cedb679f6b4ef2a57aef2f49c826e after removing the temporary test.

rtk git diff --check
  exit 0; no whitespace errors.
~~~

This accepts only the pre-owner lease lifecycle slice. It does not certify CLI prompt/REPL wiring or full R2 production closure.
