# Independent review: stream ownership chain at `812c01f`

Reviewed exact SHA `812c01fc6a0cca59d278f3518bbe144256e3bf84` in a clean detached worktree. The reviewed chain is `812c01f` → `cc195f9` → `86c89fc` → `17ab7b7`. Scope was the two previously rejected stream-ownership findings and the timeout cause behavior added by `812c01f`.

## Verdict

**REJECT, P2: the timeout wrapper can replace an earlier local total-timeout cause with a later caller cancellation cause during synchronous setup.**

The two earlier findings are otherwise corrected in this chain: the total budget starts before `next.Stream`, and the cycler treats `MessageChunk.Error` as terminal, cancels and drains the selected source, then cleans up before closing output. No P1 was found. This narrow review does not mark all of R3 complete.

## Finding

### P2 — a later caller deadline erases the first local timeout cause

[`timeoutModel.Stream`](../../../../internal/llm/middleware_timeout.go#L105) creates `budgetCtx` with a typed local total-timeout cause before calling the wrapped model. That child context retains the first cancellation cause. However, `abortSetup` checks `parentCtx.Err()` first at lines 119–129, and returns `context.Cause(parentCtx)` without consulting an already-canceled `budgetCtx`.

A deterministic temporary probe used a 20 ms middleware total timeout and a caller context with a 100 ms deadline. The wrapped model began synchronous setup, observed the local `LocalStreamAbortCause` when the 20 ms budget expired, then deliberately ignored cancellation. After the caller deadline also expired, it returned a channel plus a setup error; its source closed only after the probe released it. The timeout wrapper joined that channel, but returned only the later caller `context.DeadlineExceeded`, with no `LocalStreamAbortCause`, despite the source having observed the earlier local timeout. The focused probe failed at the returned-error `ErrorAs` assertion:

```text
rtk run go test -race -p 2 -count=1 ./internal/llm -run TestReview812cProbeLateCallerDeadlineDoesNotReplaceEarlierBudgetCause
```

This loses cancellation provenance on the setup `(channel, error)` path and can misclassify a locally timed-out provider attempt as caller cancellation. Treat the immutable `budgetCtx` cause as the authoritative first cause when aborting setup, while preserving a caller cause when the caller canceled first. Keep joining any channel returned with the setup error. The existing [`TestTimeoutModelMiddleware_StreamPreservesCallerDeadlineCause`](../../../../internal/llm/middleware_timeout_test.go#L213) covers the caller-first case but not this reverse ordering.

## Confirmed corrected behavior

At `812c01f`, the timeout context is established before synchronous setup, and the source sees a typed local cause when the configured total timeout fires. The new test covers that setup timeout; its companion test confirms that a caller deadline which happens first remains unwrapped as a local abort.

The cycler now checks for an error chunk after confirming the caller has not already canceled, applies a local abort cause, forwards the error, drains the same source channel, and only then runs cleanup and closes output. Its new barrier tests hold source close and cleanup separately, assert one terminal error, and verify caller cancellation that precedes a later source error keeps the caller cause. `ContinuationNoReplay` still prevents fallback on setup failure.

## Validation

Fresh focused race runs at the exact target SHA passed five consecutive runs each:

- `rtk run go test -race -p 2 -count=5 ./internal/llm -run TestTimeoutModelMiddleware_Stream`
- `rtk run go test -race -p 2 -count=5 ./internal/llm -run TestStreamErrorChunk`
- `rtk run go test -race -p 2 -count=5 ./internal/llm -run TestStreamCallerCancelBeforeError`
- `rtk run go test -race -p 2 -count=5 ./internal/llm -run TestStreamSetupErrorWithChannel`

The failing probe was temporary and removed; the detached review worktree is clean at `812c01fc6a0cca59d278f3518bbe144256e3bf84`. The builder report at this exact SHA records the full `llm`/`core`/`cli` race run (2,258 tests), `go vet`, `golangci-lint`, and `git diff --check` as passing. I did not rerun the full package gate for this review.
