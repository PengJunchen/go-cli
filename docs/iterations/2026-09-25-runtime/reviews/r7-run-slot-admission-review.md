# R7 run-slot admission review

**Verdict: accept the guard-only slice at the fixed hash.** I found no blocker in the admission fence, claim/release linearization, wakeup, or stable idle signal. This review covers only `DefaultRunSlotGuard`; it does not accept Assembly close, R3 model switching, or their integration.

## Fixed source and review

Reviewed `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/run-slot-admission` at `c05b012969194ba0dd339915d05c207152d26807` (`fix(core): close run-slot admission atomically`). The commit changes only `internal/core/run_slot.go` and `internal/core/run_slot_test.go`; the worktree was clean. I compared it with the accepted guard contract in `r7-assembly-close-handoff.md` at `255736649f39005b88a1cf5443db0835dc3cf12e`.

`ClaimRun`, `CloseAdmission`, and `Release` use the same mutex. A claim either publishes `current` before the close fence, keeping idle open until its matching release, or observes `closed` and returns `ErrRunAdmissionClosed`. There is no semaphore reservation interval between admission and `current` publication. Release only notifies availability and closes idle after matching the current claim; stale releases do not mutate notifications. Closing `closedCh` wakes blocked claimers, which recheck state under the mutex. Repeated close calls return the same idle channel. `ClaimRun` checks `ctx.Err()` before admission and again when observing a closed fence, so an already-canceled/observably canceled claimant gets the context cause.

`RunSlotGuard` itself is unchanged. `RunAdmissionCloser` is an optional interface implemented by the default guard, so existing custom guards remain source-compatible. `ExecuteClaimedRun` retains validation and deferred `Release`, including release while a callback panic unwinds. No registry or second ownership mechanism was introduced.

## Verification

The exact test names were listed before execution. The new close tests and the existing stale-release, cancellation-before-publication, and panic-release tests were present.

```text
rtk proxy go test -list '^(TestDefaultRunSlotGuardCloseWakesBlockedClaim|TestDefaultRunSlotGuardCloseIsStableWhenIdle|TestDefaultRunSlotGuardCloseRacesClaimAndRelease|TestRunSlotGuard_ExecuteClaimedRun_ReleasesOnPanic|TestRunSlotGuard_Release_InvalidClaim_NoOp|TestRunSlotGuard_ClaimRunCancellationBeforeClaimPublicationLeavesSlotAvailable)$' ./internal/core
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/core -run '^TestDefaultRunSlotGuardClose(WakesBlockedClaim|IsStableWhenIdle|RacesClaimAndRelease)$'
rtk env GOMAXPROCS=2 go test -p 2 -race -count=20 ./internal/core -run '^TestDefaultRunSlotGuardCloseRacesClaimAndRelease$'
rtk env GOMAXPROCS=2 go test -p 2 -race ./internal/core -count=1
rtk git -C /Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/run-slot-admission diff c05b012^ c05b012 --check
```

All commands passed; the full `internal/core` race run completed in 3.761s. I did not run the full repository suite.

## Limits

There is no dedicated deterministic test where context cancellation and the close fence become ready at precisely the same instant; the priority behavior is established by the entry check and closed-branch recheck. Assembly’s use of the optional capability and shared Harness/TurnRunner shutdown behavior remain separate integration work and are not covered by this acceptance.
