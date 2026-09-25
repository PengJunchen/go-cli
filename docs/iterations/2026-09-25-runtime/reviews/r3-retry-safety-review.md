# R3 retry-safety implementation review

**Status:** The initial review of `a86de5be5d821eb76a31017f358b009b6fd096b3` found one P1, retained below with its RED evidence. Independent follow-up review accepts the fix at `73411b378731377ce63645de859afde285691e23`; that specific typed-provider classification finding is closed. The fixed worktree was clean. I did not modify reviewed source.

This review covers the bounded local retry-count, cancellation-gate, and typed-classification slice in `r3-retry-safety-handoff.md`. It does not claim an operation-wide retry budget, stream-terminal circuit-breaker outcome, or cost-accounting fix.

## Findings

### P1 — Generic harness normalization hides typed auth and overflow errors from the production retry classifier

`DefaultRetryPolicy.Classify` checks for `*core.AgentHarnessError` and immediately returns `classifyHarnessError` at `internal/production/retry.go:136-140`; only afterward does it search the unwrap chain for `*llm.ProviderError` at `:141-151`. `core.NormalizeError` wraps an unrecognized provider error in an `AgentHarnessError` at `internal/core/harness_error.go:54-77`. For a wrapped `ProviderError` whose text contains “temporary connection reset”, `NormalizeError` assigns the generic `ErrCodeModelError` at `:120-121`; `classifyHarnessError` maps that code to `ErrorTransient` at `retry.go:173-174`. The nested auth/overflow type is therefore never reached, and `ShouldRetry` allows another dispatch at attempt zero when the total is three (`retry.go:183-195`).

A disposable table probe called the real `core.NormalizeError` with wrapped `llm.ProviderError` values for `ErrTypeAuth` and `ErrTypeOverflow`. The command `rtk env GOMAXPROCS=2 go test -p 2 -count=1 ./internal/production -run '^TestReviewProbeNormalizeErrorPreservesProviderFatality$'` exited 1: both normalized errors classified as transient and `ShouldRetry` returned true. The same probe confirmed that explicit normalized busy and hook-rejection codes remain fatal when their causes are transient provider errors, and that categorized cancellation/deadline causes remain non-retryable. The disposable test file was removed after the probe.

The committed `TestClassifyWrappedProviderError` and `TestClassifyWrappedTypedFatalBeforeTransientText` at `internal/production/retry_test.go` cover `fmt.Errorf` wrappers, but not the actual `NormalizeError` wrapper; they therefore miss this precedence case. A narrow correction should keep cancellation/deadline and explicit busy/hook fatal precedence, inspect the provider error beneath generic model/tool harness errors before applying their transient fallback, and add permanent auth/overflow regressions using `core.NormalizeError` plus the transient-sounding cause.

## Confirmed behavior

- Total-attempt semantics are exercised through wrapped model dispatches: `ProductionModelWrapper.Generate` makes exactly 1 or 3 calls for configured totals 1 or 3 (`internal/production/llm_integration_test.go`); the real `RetryModelMiddleware` verifies those exact totals for both `Generate` and immediate `Stream` initialization failures (`internal/llm/middleware_retry_test.go`).
- Retry middleware inside the production wrapper rejects dispatch after cancellation races with an already-ready backoff timer. Production-wrapper tests exercise both Generate and Stream initialization; the middleware also checks `ctx.Err()` before the initial and every subsequent dispatch.
- Negative attempt indices are rejected. Wrapped `ProviderError` values passed directly to the production classifier map rate limit, server/network, auth, and overflow to the expected categories; typed auth/overflow beat transient-sounding error text in that direct-wrapper case.
- `context.Canceled` and `context.DeadlineExceeded` remain non-retryable through both context state and error chains. The disposable probe also confirmed these errors stay non-retryable when wrapped in `CategorizedError`.

## Verification

The target list was checked before test execution with:

```text
rtk env GOMAXPROCS=2 go test -p 2 -list '^(TestRetryModelMiddleware.*|TestDefaultRetryPolicy.*|TestClassify.*|TestShouldRetry.*|TestRetryShouldRetry.*|TestProductionModelWrapper.*)$' ./internal/production ./internal/llm
```

The focused list included the max-attempt dispatch, cancellation-boundary, wrapped-provider, and retry-policy tests.

On the fixed snapshot:

- `rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/production ./internal/llm` — passed (`production` 1.710s; `llm` 2.721s).
- `rtk env GOMAXPROCS=2 go vet ./internal/production ./internal/llm` — passed.
- `rtk golangci-lint run --timeout=2m ./internal/production ./internal/llm` — no issues found.
- `rtk git diff --check 7daf21a..a86de5be5d821eb76a31017f358b009b6fd096b3` — passed.

These package checks pass, but the P1 normalized-provider probe means the typed-fatal guarantee is not yet closed for errors that arrive wrapped by the core harness normalizer.

## Follow-up review: `73411b378731377ce63645de859afde285691e23`

**Result: accepted for this bounded retry-safety slice.** The delta changes only `internal/production/retry.go` and `retry_test.go`. `Classify` now preserves cancellation/deadline and categorized-error handling, keeps explicit `busy`/`hook_rejected` harness codes fatal, and inspects a nested `ProviderError` before falling back for generic `model_error`/`tool_error` harness codes. Rate-limit, server/network, and auth/overflow retain their typed categories; unknown provider error types fall back to the generic harness classification. This closes the reproduced auth/overflow replay path without making an ordinary generic model/tool error fatal.

The new `TestClassifyNormalizedProviderErrorBeforeGenericHarnessCode` exercises the actual `core.NormalizeError` chain for auth, overflow, rate limit, server, and tool-auth cases, asserting both category and retry decision. `TestClassifyNormalizedHarnessTerminalCodeOverridesProviderCause` confirms a transient server cause cannot override normalized busy or hook-rejection codes. The previous exact-total-dispatch and cancellation-gate tests remained green; the patch does not alter those implementations.

Verification on the fixed commit:

- Target discovery: `rtk env GOMAXPROCS=2 go test -p 2 -list '^(TestRetryModelMiddleware.*|TestDefaultRetryPolicy.*|TestClassify.*|TestShouldRetry.*|TestRetryShouldRetry.*|TestProductionModelWrapper.*)$' ./internal/production ./internal/llm` — listed the new regressions, total-attempt tests, and cancellation tests.
- Focused race repetition: production classifier, max-attempt, and cancellation regressions at `-race -count=10`; retry middleware max-attempt/cancellation tests at `-race -count=10` — passed.
- `rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 ./internal/production ./internal/llm` — passed (`production` 1.676s; `llm` 2.702s).
- `rtk env GOMAXPROCS=2 go vet ./internal/production ./internal/llm` — passed.
- `rtk golangci-lint run --timeout=2m ./internal/production ./internal/llm` — no issues found.
- `rtk git diff --check a86de5be5d821eb76a31017f358b009b6fd096b3..73411b378731377ce63645de859afde285691e23` — passed.

This accepts the repaired local classification and its total-attempt/cancellation regressions only. It does not establish one operation-wide attempt budget across middleware, overflow recovery, and failover; the separate R3 production wiring gate remains open.
