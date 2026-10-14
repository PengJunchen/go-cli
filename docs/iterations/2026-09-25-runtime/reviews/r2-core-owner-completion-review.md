# R2 Core Owner Completion Review

Reviewed fixed commit `26b190c5fb23547aea2e4de53f3b3a38ffdd0e7e` in a detached worktree at `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-core-owner-review-26b190c`. The review follows `r2-core-consumer-handoff.md` and `r2-local-quiescence-proof.md`. The handoff says the seven Loop integration calls are not part of this slice; their absence is not counted as a finding.

## Verdict

**Blocked by two owner lifecycle findings.** The completion-task placeholder correctly keeps worker ownership until the same prepared signal is handed off, and the normal bound-run path gates durable success on terminal sync. However, an early Cancel can cache a cleanup context before callback values exist, and cleanup can return an apparent success while lease/claim release is still blocked.

## Findings

### P1 — Early Cancel can permanently cache a cleanup context without run identity or sink binding

`EinoTurnRunner.RunTurn` publishes the owner in `r.owners` before calling `owner.run` (`internal/core/turnrunner.go:163-166`). `Cancel` looks up that owner, calls `MarkCancelRequested`, and then cancels the turn context (`:268-288`). In the interval between publication and `owner.run` assigning `callbackCtx`, `MarkCancelRequested` calls `startCleanupCommitContextLocked(nil)` (`internal/core/run_owner.go:562-568`). Since `cleanupBaseCtx` is also nil, the helper creates the cached context from `context.Background()` (`:529-546`).

`owner.run` later constructs the callback context with RunID and the sink/binding, stores it at `:642-667`, and attempts to initialize cleanup after noticing the cancellation. That attempt returns the already-cached context unchanged. A durable cleanup write then has neither the callback RunID nor `RunCommitBinding`; this is a reachable startup race, not only a manually invalid owner state.

Temporary probe `TestReviewProbeEarlyCancelKeepsCleanupBinding` reproduced it by applying the reachable order: `MarkCancelRequested`, cancel, then `owner.run`. It failed because `RunCommitFromContext(cleanupCtx)` returned no binding. The probe was removed; no tracked source was changed. Command:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 ./internal/core -run '^TestReviewProbe(EarlyCancelKeepsCleanupBinding|BlockedLeaseCloseDoesNotReturnEarly)$' -count=1
exit code: 1
observed assertion: cleanup context must retain the callback sink binding
```

Fix by preserving the original cleanup deadline while binding the verified callback values once they become available. Do not create a fresh timeout when the callback context is installed; retain the original absolute deadline.

### P1 — Cleanup may return success before lease close and run-slot release complete

`supervise` sets `o.released = true` before calling `CloseRunCommit` and `guard.Release` (`internal/core/run_owner.go:812-826`). It publishes the final execution state and closes `o.done` only after those calls return (`:828-847`). Meanwhile, `cleanupPending` immediately returns the current result and `runErr` when `o.released` **or** `localResourcesJoinedLocked()` is true (`:870-876`). Thus the caller can leave the run while the supervisor is still blocked closing the lease or releasing the guard. With a successful callback and already-synced terminal, the returned snapshot can say `outcome=success`, `state=draining`, `err=nil` before the lease is closed.

Temporary probe `TestReviewProbeBlockedLeaseCloseDoesNotReturnEarly` registered an open resource to start the bounded cleanup wait, joined that resource, then blocked `CloseRunCommit`. It failed with `owner returned before CloseRunCommit completed: outcome="success" state="draining" err=<nil>`. The probe was removed; no tracked source was changed. Command:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 ./internal/core -run '^TestReviewProbeBlockedLeaseCloseDoesNotReturnEarly$' -count=1
exit code: 1
observed assertion: owner returned before CloseRunCommit completed
```

Track release-started separately from release-completed. Until both the lease close and guard release finish, a bounded caller must receive a pending/error result rather than a successful terminal snapshot; the owner must retain one-shot close ownership and finish release through its supervisor.

## Reviewed invariants and validation

- Completion registration increments worker ownership before dispatch. Handoff to an open `Done` increments resource ownership before decrementing the placeholder worker under the same mutex; a closed `Done` is classified ready before worker decrement. This has no zero-resource handoff gap. Repeated handoff for one task ID is ignored after its state changes. Distinct task IDs sharing one channel are conservatively counted independently until that channel closes; the Loop integration must still register one task per action occurrence.
- The owner reads `Proof()` only after the exact channel closes. In the prepared tools implementation, inline failures invalidate before closing and queued panics invalidate before closing, so no proof-cache blocker was found here. Owner-level callback panic becomes unknown and remains quarantined; the Loop worker's deferred handoff is outside this commit and outside this review scope.
- For a normal bound run, terminal sync is required before callback entry; absent sync produces an unknown/quarantined result. `TestRunOwnerWithBoundSinkCannotAcknowledgeUnsyncedTerminal` passed. A pre-callback cancellation is undispatched and reports cancellation rather than success.
- Verified the 13 named owner tests by listing them before execution. The targeted `-race` set passed: completion handoff, nil-vs-empty proof, dynamic signal registration, cancellation boundary recheck, cleanup context sharing, poisoned lease handling, lease panic, terminal-sync gate, callback panic handoff, pre-canceled callback, and final-release race (`ok`, 1.166s).
- The full `internal/core` race suite was not rerun; the two deterministic dynamic probes provide blockers without spending another full-suite run. No source file was modified in the detached review worktree; the temporary probe test was deleted.

## Independent fixed-delta review — `055600ce60eaee361044f7b9034fb1dc761dfb5e`

This follow-up is an independent review by `contract_reviewer` of the fixed delta from `26b190c5fb23547aea2e4de53f3b3a38ffdd0e7e`, in a detached worktree at `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-core-owner-review-055600`. The original findings and author attribution above are preserved.

Both P1 findings are closed in this snapshot:

- Early cancellation now records one absolute cleanup deadline even before the callback context exists. `EinoTurnRunner.RunTurn` configures the timeout before publishing the owner, and `run` installs the validated callback context later without extending that deadline. The owner creates the cancellation-free cleanup context only after it has the callback context, so its `RunID` and validated sink binding are carried into cleanup bookkeeping.
- Invalid sink bindings fail before callback/provider entry and are replaced with an empty run-commit context value. The owner still retains its locally generated `RunID`; it does not manufacture a session/workspace binding from partial caller input. The undispatched path returns `ErrInvalidRunCommitBinding` and releases any borrowed lease once.
- Final release now has distinct `releaseStarted` and `released` states. Once resources join, new resources/workers are rejected and the bounded caller wait is started before the supervisor invokes lease close and guard release. If either release blocks, the caller returns `RunExecutionCleanupPending` with an error rather than success; the claim remains held. Release is attempted once, and the owner becomes quiescent only after the calls return. A panicking lease close is recovered, marks the result unknown/quarantined, and still permits the guard release.

I enumerated the owner-focused tests, then independently ran the targeted race set covering early cancellation, shared deadline/binding, valid versus invalid bindings, blocked lease and guard release, completion-signal cancellation races, lease/callback panic, and resource/worker join. All 14 tests passed (`ok github.com/pengjunchen/go-cli/internal/core 1.269s`):

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -run '^(TestRunOwnerEarlyCancelRebindsCleanupContextWithoutExtendingDeadline|TestRunOwnerEarlyCancelDoesNotInventInvalidBinding|TestRunOwnerCleanupCommitContextSharesDeadlineAndBinding|TestRunOwnerBlockedLeaseCloseRemainsPendingUntilReleaseCompletes|TestCleanupPendingDuringBlockedFinalReleaseRemainsPending|TestCleanupPendingAfterQuiescencePreservesCompletedResult|TestRunOwnerQuarantinesPanickedLeaseReleaseWithoutRetrying|TestRunOwnerPanickedCallbackQuarantinesClaim|TestRunOwnerReturnsPoisonedSinkLeaseOnlyAfterResourceJoin|TestRunOwnerWaitRechecksExactSignalAfterCancellation|TestRunOwnerWaitAcceptsDoneClosedAtCancellationBoundary|TestRunOwnerCancellationKeepsClaimUntilWorkerStops|TestRunOwnerNonCooperativeCallbackReturnsPendingThenQuiesces|TestRunOwnerPendingExecutionResourceHoldsClaimUntilDone)$' ./internal/core -count=1
```

The full `internal/core` race/vet/lint results were supplied by the builder and were not repeated here. I found no blocker in the fixed delta. **Delta verdict: accept `055600ce60eaee361044f7b9034fb1dc761dfb5e` for the owner lifecycle slice.** The Loop integration and other downstream R2 consumers remain separate gates.
