# R3 Retry Safety Handoff

Status: approved bounded component slice. This handoff covers only local retry-count semantics, cancellation gates, and typed error classification. It does not claim the R3 retry/accounting contract is implemented.

## Authority and source baseline

The R3 contract is in `r3-provider-design.md` and its design closure is recorded in `reviews/r3-contract-review.md`. The review approves the design clarification, not implementation; R3-BLD-02 remains gated on R2 implementation and restart-safety acceptance. This slice is intentionally limited to corrections that can be proven inside the existing retry policy and middleware boundary without R2 attempt facts.

Source was checked at `7daf21ab9b24644246a06950f5820a9e3b01c200`:

- `internal/production/retry.go` defines `MaxAttempts` as a total and receives a zero-based attempt index, but currently permits a retry at every index below `MaxAttempts`.
- `internal/llm/middleware_retry.go` uses the same zero-based index and loops around `Generate` and the initial `Stream` call. Its default policy also treats `MaxAttempts` as a total while allowing the last extra retry.
- `internal/production/retry.go` classifies an internal `categorized` interface by a direct assertion, then handles `AgentHarnessError` and free-text matches. It does not classify `llm.ProviderError` structurally. In the CLI standard chain, `assemble.go` adapts this production policy into `RetryModelMiddleware`, so the local LLM classifier's `ProviderError` cases do not protect the configured path.
- Both retry loops check context while waiting for backoff, but have no final `ctx.Err()` check before the next model call. A zero-delay timer and a concurrently canceled context can both be ready, allowing the timer branch to dispatch again.

## Bounded changes

**Total-attempt count.** The initial provider call is attempt index 0. With a configured total of N calls, a retry after index `i` is allowed only if `i < N-1`. Reject negative indices conservatively and use the comparison `attempt >= MaxAttempts-1`; do not calculate `attempt+1`, which can overflow. Apply the same contract to the production and LLM default policies, and update existing policy fixtures to assert the total-call meaning. `MaxAttempts=1` must permit exactly the initial dispatch.

**Cancellation fence.** A retry is never allowed when `ctx.Err()` is set or the error chain contains `context.Canceled` or `context.DeadlineExceeded`, even if the error is otherwise tagged transient. Both policies must apply this precedence before classifying the error. The Generate and Stream retry loops must also check `ctx.Err()` immediately before every model dispatch, including the initial dispatch, so a cancellation racing with an already-ready backoff timer cannot authorize another call.

**Typed classification.** Production classification must use `errors.As` to inspect wrapped typed errors before free-text fallback. Preserve the existing `CategorizedError` and `AgentHarnessError` behavior; map `llm.ProviderError` types explicitly: rate limit to `ErrorRateLimit`, server/network to `ErrorTransient`, and auth/overflow to `ErrorFatal`. A typed fatal category remains fatal even when a wrapper or cause contains words such as “temporary” or “connection reset”. Keep current fallback behavior for ordinary untyped errors; this slice does not broaden retryability or add provider error categories.

## Test-first acceptance

Write and enumerate these behavior tests before changing implementation:

| Test | Expected red on baseline | Required green behavior |
|---|---|---|
| Production wrapper exact dispatch count for `MaxAttempts=1` and `3` | Current policy can make 2 and 4 calls | Observed calls are exactly 1 and 3 |
| LLM default policy dispatch-count table for totals 1 and 3 | Current default loop can make one extra call | The same exact totals as the production policy |
| Wrapped `ProviderError` classification table | Production classifies 5xx/network from its text as fatal instead of typed transient | Known provider types map as specified; wrapped auth/overflow stay fatal |
| Wrapped typed-fatal error with transient-sounding cause | Direct assertion / string fallback can miss a wrapped category | Typed fatal wins before string matching |
| Canceled context and wrapped cancellation error policy cases | Current policies can still return true for retry | Both policies return false before category/backoff work |
| Production wrapper Generate and Stream-initialization cancellation boundary | Existing retry loop has no per-dispatch cancellation fence | A cancellation after retry admission cannot cause a second provider call |

The call-count tests must exercise actual wrapped model invocations, not only `ShouldRetry` return values. Cancellation tests must cover both Generate and the immediate error returned by Stream initialization. Existing assertions are to be corrected to the documented total-attempt contract, not removed or weakened.

## Explicitly deferred behind R1/R2

This slice does not add an operation-wide budget, assign `AttemptID`, authorize provider fallback, retry after stream consumption begins, or change overflow/continuation policy. The current middleware, failover, cycler, and overflow layers own separate behavior; unifying actual dispatches requires the approved per-run Loop decision authority and typed attempt facts. Core currently has the RunCommit sink and lease contracts, but production `CommitInput`/`CommitTerminal` call sites are not present in this source baseline. R2-synced input may be reused only by a still-owned run; recovered runs must not replay a provider request.

Streaming circuit-breaker outcomes and cost accounting remain deferred as well. `circuitBreakerModel.Stream` currently calls synchronous `CircuitBreaker.Execute` around stream creation, which records success before channel consumption. The production cost wrapper records only successful Generate responses with non-nil Usage, uses configured `modelName`, and passes Stream through. Core already consumes `MessageChunk.Error`, `Final`, and `Usage`, and the chunk type carries Usage; none of those facts currently form an idempotent per-dispatch accounting record. Fixing these requires the terminal/ownership seam and actual selected model identity, not a local retry counter patch. Missing usage after dispatch must eventually be represented as unknown, not zero.

No hard/soft cost budget is introduced. No CLI assembly, Core Loop, ModelCycler, resolver, or session/action-ledger changes are part of this handoff.
