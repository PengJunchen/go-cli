# Independent review: stream ownership chain at `f7780a7`

Reviewed exact detached target `f7780a732504b09cd7897ff1fb59b3ae85ab6343` in a clean worktree. The chain is `17ab7b7` → `86c89fc` → `cc195f9` → `812c01f` → `f7780a7`.

## Verdict

**ACCEPT for this narrow stream-ownership slice.** The three previously reported P2 defects are corrected: the total timeout covers synchronous setup, cycler error chunks terminate and join the selected source, and setup completion preserves the first cancellation cause. I found no remaining P1/P2 in those contracts. This does not mark the wider R3 iteration complete or validate native-provider, breaker, or live-system behavior.

## Review evidence

In [`middleware_timeout.go`](../../../../internal/llm/middleware_timeout.go#L113), the budget context is created before `next.Stream`. On setup abort, `attemptCause` snapshots the immutable stream-context cause before draining the source. The returned error therefore keeps an earlier local timeout recognizable while `errors.Join` preserves a later setup error for `errors.Is`/`errors.As`; a setup error that itself triggers cancellation remains the primary returned error. The added channel-plus-error, nil-channel-plus-error, and nil-nil cases cover this ordering, while the caller-first deadline test confirms it remains caller cancellation.

The cycler in [`cycler.go`](../../../../internal/llm/cycler.go#L313) forwards one source error, cancels and drains the same channel, runs provider cleanup, and only then closes its output. Its caller-first test covers cancellation before a later source error, and the setup-error test holds fallback until source close and cleanup finish. The core reader in [`loop.go`](../../../../internal/core/loop.go#L1147) drains on abort before returning; the CLI output guard closes its output only after its reader path has joined the inner channel. The loopback assembly test uses a custom `RoundTripper` gate around response-body `Close()` ([fixture](../../../../internal/cli/r1_assembly_stream_test.go#L35)); it is evidence for wrapper cleanup ordering, not evidence of a defect in the HTTP provider.

## Validation

At the exact target SHA, this focused race command passed: `rtk go test -race -p 2 -count=10 ./internal/llm -run '^(TestTimeoutModelMiddleware_StreamTotalTimeoutIncludesSynchronousSetup|TestTimeoutModelMiddleware_StreamPreservesCallerDeadlineCause|TestReview812cProbeLateCallerDeadlineDoesNotReplaceEarlierBudgetCause|TestTimeoutModelMiddleware_LateCallerDeadlineDoesNotReplaceLocalCauseForNilSetup|TestStreamErrorChunkCancelsAndJoinsSelectedSource|TestStreamCallerCancelBeforeErrorKeepsCallerCause|TestStreamSetupErrorWithChannelJoinsCleanupBeforeFallback)$'` — 90 test executions passed.

`rtk git diff --check 17ab7b76ef0b7636abff1e5a055de7acf6f6bb31..HEAD` passed, and the detached review worktree was clean at `f7780a732504b09cd7897ff1fb59b3ae85ab6343`. The target's builder report also records a full `internal/llm` race run plus `go vet` and `golangci-lint`; I did not repeat those broader checks for this bounded review.
