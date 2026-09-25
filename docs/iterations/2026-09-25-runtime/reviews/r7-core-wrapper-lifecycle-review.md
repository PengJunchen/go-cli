# R7 core wrapper lifecycle implementation review

**Reviewed source:** `5122e9264dc850a4506eb86aa181e1a2bd89837d` (`codex/runtime-r7-run-wrapper`). Review used a clean detached worktree. The implementation diff is limited to `internal/core/harness.go`, `run_slot.go`, `turnrunner.go`, and `run_slot_wrapper_test.go`; `run_owner.go` was not modified.

**Verdict:** The default run-slot guard correctly keeps admission closed until both owner release and wrapper completion, but the TurnRunner returns from a pending owner while still invoking post-run hooks and agent snapshots. This causes a reproducible data race and reverses lifecycle callback order. A second, lower-severity panic-safety gap affects custom wrapper registrars.

## Blocking finding

### R7CORE-01 — TurnRunner runs post-run callbacks while the run callback is still pending

**Severity:** P1.

`RunTurn` now invokes `OnTurnStart` and captures the initial message count inside the function passed to `owner.run` ([`turnrunner.go`](../../../../internal/core/turnrunner.go#L196)). The owner may return a cleanup-pending snapshot after its bounded wait while that callback continues. `RunTurn` then unconditionally invokes `OnError`, calls `safeMessageCount`, and invokes `OnTurnEnd` ([`turnrunner.go`](../../../../internal/core/turnrunner.go#L260)). It does not establish that the callback actually joined before using the same agent and hook chain again.

A temporary deterministic probe blocked the first `Messages()` call, canceled the runner lifetime, and used a short cleanup timeout so the post-run snapshot entered while the original callback remained blocked. The second snapshot overlapped the first call, and `go test -race` reported a data race between the callback write to `beforeMsgCount` at `turnrunner.go:203` and the post-run read at `turnrunner.go:273`. The probe was removed after review. A second temporary probe held `OnTurnStart` past cancellation and observed `OnTurnEnd` executing before `OnTurnStart` returned. This is also an ordering violation for the same `HookChain`, even without a race report.

The caller can still receive the bounded cleanup-pending result, but post-run message inspection, hooks, and span completion must wait for the actual callback join. The existing wrapper lease is the natural place to retain this work: transfer the one post-run finalizer to a single continuation when `owner.done` is still open, then run post-hooks/snapshot/span end and call `wrapperDone` last. Keep panic containment around that continuation so a hook or span panic cannot crash the process or strand the lease. Do not use `RunExecutionState` alone as proof that the callback joined; check the owner's completion signal.

## Additional finding

### R7CORE-02 — A panicking custom wrapper registrar leaks the acquired claim

**Severity:** P2; limited to custom `RunSlotWrapperRegistrar` implementations.

Both public entrypoints acquire the claim and call `registerRunWrapper` before installing their setup-cleanup defer ([`harness.go`](../../../../internal/core/harness.go#L154), [`turnrunner.go`](../../../../internal/core/turnrunner.go#L128)). A temporary guard implementing the public optional registrar interface and panicking from `RegisterRunWrapper` caused both `HarnessImpl.Submit` and `EinoTurnRunner.RunTurn` to panic with the guard's `Release` count still zero. The temporary probe was removed. `DefaultRunSlotGuard.RegisterRunWrapper` does not panic, and ordinary registration errors are handled by releasing the claim; this finding concerns the extension point accepted by `WithRunSlotGuard` / `SetRunSlotGuard`.

Install claim cleanup immediately after `ClaimRun` and before invoking the optional registrar, while preserving the existing default guard's two-party owner/wrapper release semantics. If a custom registrar can partially publish a wrapper before panicking, its contract also needs to state how that partial registration is retired; otherwise fail closed rather than leaving a permanent active claim.

## Lifecycle checks that passed

- `DefaultRunSlotGuard` serializes claim, close, and release under its mutex. `Release` and `wrapperDone` finalize only when both owner release and wrapper completion have occurred; an ordinary claim without a wrapper still releases. Stale and duplicate wrapper registration are rejected.
- `CloseAdmission` closes the stable idle signal only when no claim is active or after the current claim finalizes. A close racing with a claim is linearized under the same mutex. The idle signal, not a status snapshot, is the close-completion authority.
- `ActiveRunStatus` captures the handle under the guard lock and invokes `Snapshot` after unlocking; the re-entry probe confirms it does not deadlock. Its `ExecutionState` is the owner snapshot while the separate active boolean reports the claim. A quiescent owner snapshot must not be interpreted as proof that wrapper post-hooks, span export, or stream close have finished.
- Both entrypoints claim and register the wrapper before borrowing tracing producers or running hooks/message providers. The default Harness defer order ends the span and closes the stream before completing the wrapper. TurnRunner's synchronous path similarly keeps the wrapper through its post-run hooks and end span; the pending-owner case is the blocker above.
- Existing owner tests prove that cancellation and bounded caller return do not release the claim while a non-cooperative callback, worker, or pending resource remains active. Existing lifetime tests cover cooperative cancellation. No real Assembly close path is present in this implementation slice, so this review does not establish end-to-end Assembly teardown.

## Validation

`rtk proxy go test -p 2 -list . ./internal/core` listed the package tests, including the wrapper lifecycle cases. The targeted wrapper/owner tests below passed under race detection for 20 iterations:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/core -run '^(TestHarnessRunSlotCoversSpanEndAndStreamClose|TestTurnRunnerRunSlotCoversTurnStartHook|TestTurnRunnerRunSlotCoversPostRunHooks|TestRunSlotWrapperAndOwnerReleaseBothGateClaim|TestRunSlotPureClaimReleasesWithoutWrapper|TestRunSlotWrapperRegistrationRejectsDuplicateAndStaleClaims|TestRunSlotWrapperStatusStartsActiveAndAttachesOnce|TestRunSlotActiveStatusCallsHandleOutsideGuardLock|TestHarnessAndTurnRunnerRejectAfterAdmissionFenceBeforeCallbacks|TestEntrySpanSetupPanicReleasesClaimAndWrapper|TestTurnStartHookPanicIsContainedByOwnerAndReleasesWrapper|TestMessageSnapshotPanicIsContainedByOwnerAndReleasesWrapper|TestRunLifetimeCancellationStopsRunInput|TestRunOwnerCancellationKeepsClaimUntilWorkerStops|TestRunOwnerNonCooperativeCallbackReturnsPendingThenQuiesces|TestRunOwnerPendingExecutionResourceHoldsClaimUntilDone)$' -count=20
```

The full core race suite passed, and `go vet ./internal/core` passed. The temporary message-snapshot probe intentionally failed under `-race` with the write/read locations above; the temporary hook-order and registrar-panic probes reproduced their respective behaviors. All temporary probes were removed, and the detached review worktree is clean.

## Follow-up review: `d45a0931` and final `4b49597d`

**Verdict:** The two blocking behaviors and the registrar-panic claim leak are fixed in the follow-up. The final `4b49597deba59fd601030e5fd3fe737af0f3f33b` delta from `d45a0931cac94d974f9441cfb1441cbb729a153d` only restores normal completed-turn logging to `Info` and clarifies that `ActiveRunStatus` describes the owner snapshot and active claim, not wrapper completion. No additional source finding was identified.

- `TestTurnRunnerDoesNotReadMessagesWhileCallbackPending` confirms the pending caller receives an immutable cleanup-pending result, the second message snapshot does not start until the original callback joins, and the agent is not started after cancellation during its initial snapshot.
- `TestTurnRunnerPostHooksWaitForPendingTurnStart` confirms `OnTurnEnd` waits for the blocked `OnTurnStart`, that late hooks retain the canceled input context, and cancellation before the hook returns prevents later agent/message work.
- `TestTurnRunnerTransfersOriginalContextUntilPendingOwnerJoins` confirms late hooks run only after the pending resource joins, receive the original bounded result/error snapshot, and still see the live status handle progress to quiescent. The continuation waits for `owner.done`, runs hooks and span completion, stops the linked context, and completes the wrapper last. Panic recovery encloses the continuation body and its `finally` cleanup is guarded by `sync.Once`.
- `TestEntrypointsReleaseClaimWhenWrapperRegistrarPanics` confirms both Harness and TurnRunner release their acquired claim exactly once when the optional registrar panics. The default guard's wrapper/owner dual gate remains unchanged.
- `ActiveRunStatus` now explicitly documents that teardown must wait for the `CloseAdmission` idle channel; this resolves the status-snapshot ambiguity without changing the completion authority.

The exact four follow-up tests were listed before execution and passed with `-race -count=20`. The full `internal/core` race suite passed with `-count=1`; `go vet ./internal/core` and `git diff --check 5122e9264dc850a4506eb86aa181e1a2bd89837d..HEAD` passed. `golangci-lint run --new-from-rev=5122e9264dc850a4506eb86aa181e1a2bd89837d ./internal/core` reported no new issues. The full package lint still reports the pre-existing `goimports` formatting issue in `internal/core/run_slot_wrapper_test.go:10`, which reproduces on the 5122 baseline. The detached worktree at 4b is clean. This remains a core wrapper slice; no real Assembly wiring or end-to-end Assembly shutdown was reviewed here.
