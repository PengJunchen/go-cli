# R2 managed Grep/Find lifecycle delta review

## Verdict

**Conditionally rejected at `2e003985fa5a37206fccec4ffa80b07bac38855a`.** The two-commit delta closes the earlier forced-pipe-close containment/fallback/proof defect: a forced close now yields unknown, stops fallback, and withholds positive process-group completion proof. One P2 remains: on the forced-close path, a caller deadline/cancellation cause is omitted from the returned error chain. Preserve `ctx.Err()` with `%w`/`errors.Join` while retaining the unknown outcome and all fail-closed behavior. A deadline cause must not be presented as proof that the escaped process or remote work completed.

## Snapshot and review boundary

- Reviewed commits `2bf4c0971847fa2782b738a1a51033477ce3ec37` and `2e003985fa5a37206fccec4ffa80b07bac38855a`; the latter is the fixed head, based on `84324fe7a55ea852583f5082fe5d76ac0dd0608a` plus the first delta.
- Source was checked in the independent detached worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-managed-search-delta-review-2e00398` on Darwin arm64. No product source was changed.
- This review covers managed Grep/Find helper lifecycle, local completion reporting, fallback, and cancellation behavior only. Default Grep/Find prepared-registration/proof wiring remains a separate deferred slice; this does not establish full R2 closure.

## Verified behavior

- `managedLocalResult` distinguishes local `quiescent` from `containmentProven`; the latter is false after forced pipe closure (`internal/tools/process_scope.go:151`). A reader being joined after its read descriptor is forcibly closed no longer proves that a writer outside the owned process group stopped.
- The real `setsid` integration cases `TestGrepForcedPipeCloseStopsFallbackAndWithholdsProof` and `TestFindForcedPipeCloseStopsFallbackAndWithholdsProof` observe the escaped child alive when the tool returns. They assert no fallback output, `OutcomeUnknown`, no false `PendingExecutionError` for already-joined local resources, and no positive process-completion report; cleanup then kills and joins the child.
- `shouldFallbackAfterManagedHelper` rejects `errProcessContainmentUnconfirmed`. The ordinary failed-helper cases `TestGrepJoinsFailedHelperDescendantBeforeFallback` and `TestFindJoinsFDFailedHelperDescendantBeforeSystemFindFallback` still stop/join the owned group before running the documented fallback and publishing a completed group proof.
- Pending completion remains tied to an actual observable local resource's `Done` channel. The escaped `setsid` case, whose local readers have joined but whose escaped writer cannot be observed through the group, returns unknown without inventing a never-closing channel.

## Remaining P2: forced close drops the caller's cancellation cause

In `managedSearchLifecycleError` (`internal/tools/managed_search_command.go:46-56`), the `forcedPipeClose` branch returns before the `interrupted` branch. Its cause joins the containment sentinel and process/reader errors, but never includes `ctx.Err()`. The general `localLifecycleError` forced-close branch (`internal/tools/process_scope.go:396-403`) has no context parameter either. This makes timeout/cancellation indistinguishable from a non-cancelled forced close to callers using `errors.Is`, even though the tool correctly remains fail-closed.

An independent temporary real-process probe reproduced the loss. It starts a fake `rg` helper which launches a `setsid` child retaining inherited output pipes, runs Grep under a 300 ms deadline, verifies at return that the child is still alive, output is empty, outcome is unknown, no positive proof was sent, and then kills and joins the child. The only failing assertion is that the returned error should preserve `context.DeadlineExceeded`:

```sh
rtk env GOMAXPROCS=2 go test -race -p 2 -run '^TestReviewManagedGrepTimeoutWithEscapedPipeWriterFailsClosed$' ./internal/tools -count=1 -v
```

The probe fails at `errors.Is(err, context.DeadlineExceeded)`: the returned chain contains the containment-unconfirmed sentinel, `signal: terminated`, and closed-reader errors, but not the deadline. The review-only probe is preserved, untracked and outside the builder worktree, at `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-managed-search-delta-review-2e00398/internal/tools/zz_review_managed_search_cancel_test.go` for the narrow follow-up. It is not part of either reviewed commit.

Required fix: include the non-nil caller cancellation/deadline cause in the forced-close unknown error chain. Keep `OutcomeUnknown`, do not enable fallback, do not publish positive completion, and do not equate `DeadlineExceeded` with process/resource completion.

## Independent validation

The exact relevant test names were enumerated before execution. The focused lifecycle/cancellation and fallback race runs passed on Darwin arm64:

```sh
rtk env GOMAXPROCS=2 go test -race -p 2 -run '^(TestManagedSearchFallbackStopsForPendingOrCanceledHelper|TestManagedLocalCommandClosesReadDescriptorsAfterEOF|TestManagedLocalCommandTracksDelayedReaderJoin|TestManagedLocalCommandForcedCloseJoinsReaderWithoutProvingContainment|TestGrepForcedPipeCloseStopsFallbackAndWithholdsProof|TestFindForcedPipeCloseStopsFallbackAndWithholdsProof|TestGrepJoinsFailedHelperDescendantBeforeFallback|TestFindJoinsFDFailedHelperDescendantBeforeSystemFindFallback)$' ./internal/tools -count=1
```

Result: pass (`2.071s`). A separate focused cancellation set also passed (`1.832s`):

```sh
rtk env GOMAXPROCS=2 go test -race -p 2 -run '^(TestBashCancellationKillsPipeHoldingDescendant|TestGrepPureGoPreCanceledContextIsNotSuccessfulPartialResult|TestGrepPureGoMidWalkCancellationDoesNotReturnPartialSuccess|TestFindPureGoCancelledContext|TestManagedSearchFallbackStopsForPendingOrCanceledHelper)$' ./internal/tools -count=1
```

A temporary real Grep timeout probe without an escaped pipe writer also passed (`1.385s`): it preserved `errors.Is(err, context.DeadlineExceeded)`, stopped fallback, and observed the owned group join. That control case isolates the error-chain defect to forced pipe closure. The builder reported the full tools race suite passed; this reviewer did not repeat the full suite.

No files were staged or committed by this review. Accept the reviewed behavior after the caller-cause error-chain delta is fixed and independently rechecked; keep default Grep/Find proof registration and broader R2 status explicitly deferred.

## Root follow-up acceptance at 1099c68

The parent reviewer independently inspected `1099c68ab2036b597d0464972ff1c16baff4368c`: its production delta only joins the caller context cause into the forced-pipe-close error. The unknown outcome, fallback prohibition, pending-resource identity, and completion-proof rules are unchanged. The new real-process regression verifies the escaped writer remains alive at return, no fallback/proof is published, and `errors.Is(err, context.DeadlineExceeded)` holds.

At that fixed snapshot, root independently ran the Grep timeout regression and both Grep/Find forced-pipe-close regressions under race with `-count=3` (4.017s), followed by `TestManagedSearchFallbackStopsForPendingOrCanceledHelper` under race with `-count=3` (1.074s). Test discovery confirmed these four names; the first selection also contained an unmatched extra pattern, which is not counted as a test. Output is recorded in [root race evidence](../evidence/r2-managed-search-1099-root-race.log). This accepts the bounded helper slice and fixes the P2 error-chain finding; default prepared registration and full R2 remain open.

Integrated mappings: `2bf4c097 → d39dc37`, `2e003985 → ca09393`, and `1099c68 → da7779a`.
