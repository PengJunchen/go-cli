# R3 provider response body bounds review

**Verdict: reject `e8c83f7e5c1a14b711a4118027b7bb697b96ea0c` pending the two findings below.** The byte caps, bounded successful-response reads, sanitized I/O errors, synchronous close, and existing successful SSE path passed review and tests. Error-status/read-failure handling still drops structured classification, and production retry classification loses the typed oversize decision after normalization.

## Fixed source and review method

Reviewed commit `e8c83f7e5c1a14b711a4118027b7bb697b96ea0c` with parent `0d6a9b8cf0269d64002fc68a7024c75955945959` in a clean detached worktree at `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r3-provider-body-bounds-review-e8c83f7`. The full delta contains exactly seven `internal/llm` files: `middleware_retry.go`, `native.go`, `provider.go`, `provider_body.go`, `provider_body_test.go`, `provider_error.go`, and `provider_error_test.go`.

I read the full diff and traced both HTTP implementations through generate and native streaming error paths, the bounded reader/closer, `llm` retry classification, and production classification. The temporary adversarial tests were untracked and removed after the probes; the detached worktree is clean. No submitted file or commit was modified.

The accepted behavior observed in the implementation includes reading at most limit+1 bytes, differentiating exact-limit from oversize bodies, preserving causes through sanitized read/close wrappers, closing acquired bodies synchronously once on read errors and panics, bounding HTTP error prefixes, and leaving successful native SSE bodies on their previous streaming path. An additional temporary real-HTTP test passed for both Eino and native models: exactly 10 MiB of valid JSON with trailing whitespace was accepted; the same complete JSON plus a second trailing JSON value beyond the cap returned `ErrProviderResponseTooLarge` before decode.

## Findings

### [P2] Preserve HTTP status and bounded overflow prefix when reading an error body fails

[`HTTPChatModel.roundTrip`](../../../../internal/llm/provider.go:519) and both non-2xx branches in [`native.go`](../../../../internal/llm/native.go:195) return only the read/close errors when `readBoundedProviderBody` reports a read error. Meanwhile [`readProviderBodyBytes`](../../../../internal/llm/provider_body.go:65) discards any bytes returned alongside that read error. The response status is already known, and a reader may have returned a useful bounded prefix before failing, but neither reaches `newProviderError`.

An untracked adversarial reader/round-tripper probe reproduced this for Eino Generate, native Generate, and native Stream with both HTTP 401 and HTTP 400 whose partial prefix contained `context_length_exceeded`. In all six cases `errors.As(err, *ProviderError)` failed; the 401/400 classification was lost, and `llm` retry classification returned transient instead of fatal. `errors.Is` still found both read and close causes, and the returned `Error()` omitted the body/URL canary. This breaks the stated requirement to retain HTTP status and overflow classification from the bounded prefix, and can retry authentication or overflow errors.

The bounded partial payload and known HTTP status should remain available for constructing a structured `ProviderError`, while sanitized read and close failures remain in the unwrap chain.

### [P2] Keep oversized responses fatal after production error normalization

`llm.classifyError` explicitly uses `errors.Is(err, ErrProviderResponseTooLarge)`, so direct and joined oversize errors are fatal. `production.DefaultRetryPolicy.Classify` does not recognize that sentinel. When the same typed error is wrapped by `core.NormalizeError`, its `model_error` branch only calls `classifyProviderError`, which recognizes `*llm.ProviderError`; the oversize sentinel is not one. It then falls back to the generic `model_error` category and becomes transient.

The temporary production probe passed for the direct/joined error, then failed after normalization: `Classify(normalized)` returned `transient` and `ShouldRetry` returned true. Normalized provider failures are an existing supported classifier path (`TestClassifyNormalizedProviderErrorBeforeGenericHarnessCode`), so the new typed fatal condition should survive it as well.

## Verification

The test lists were checked before the targeted `-run` commands. Exact commands and results from the detached review worktree:

```text
rtk proxy go test -list 'Test(ReadBoundedProviderBody|HTTPChatModel|NativeChatModel|ProviderErrorOverflow|ClassifyError|NewProviderError)' ./internal/llm
  exit 0; listed the relevant bounded-reader, HTTP/native, overflow, and retry tests.

rtk proxy go test -list 'Test(RetryPolicy|Classify|DefaultRetryPolicy|ProductionRetry)' ./internal/production
  exit 0; listed the relevant production classifier tests.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/llm -run '^(TestReadBoundedProviderBody.*|TestHTTPChatModelJoinsSanitizedReadAndCloseFailures|TestHTTPChatModelSanitizesReadAndClosePanics|TestHTTPChatModelRejectsOversizedJSONResponse|TestHTTPChatModelBoundsAndRedactsOversizedErrorBody|TestNativeChatModelRejectsOversizedJSONResponse|TestNativeChatModelBoundsErrorBodiesForGenerateAndStream|TestProviderErrorOverflowClassificationUsesBoundedPrefix|TestClassifyError|TestHTTPChatModelGenerate_ResponseReadError|TestHTTPChatModel_roundTripErrorBody|TestNativeChatModelGenerate_Non2xx)$' -count=1
  exit 0; relevant committed LLM tests passed under race.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/production -run '^(TestClassifyWrappedProviderError|TestClassifyWrappedTypedFatalBeforeTransientText|TestClassifyNormalizedProviderErrorBeforeGenericHarnessCode|TestClassifyNormalizedHarnessTerminalCodeOverridesProviderCause)$' -count=1
  exit 0; relevant committed production classifier tests passed under race.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/llm ./internal/production -count=1
  exit 0; full LLM and production package race suites passed after removing temporary probes.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/llm -run '^TestReviewProviderFullBodyLimitIncludesJSONTrailingData$' -count=1
  exit 0; exact-limit valid JSON and oversized valid-JSON-plus-trailing-data probe passed for Eino and native models.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/llm -run '^TestReviewHTTPStatusSurvivesErrorBodyReadFailure$' -count=1
  exit 1; all six 401/400 × Eino/native Generate/native Stream cases lost ProviderError; retry classification was transient. Read/close causes remained unwrap-able and their canaries were absent from Error().

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/production -run '^TestReviewOversizedResponseRemainsFatalWhenNormalized$' -count=1
  exit 1; direct/joined oversize classification was fatal, but normalized classification returned transient.
```

## Independent follow-up review: `0620473eb559a529317ca4a863df26550f765017`

**Verdict: reject pending one P2 precedence fix.** This follow-up fixes both original findings: non-2xx read-error branches now retain the known status and bounded partial prefix in a `ProviderError`, and normalized generic model errors carrying `ErrProviderResponseTooLarge` are fatal. The earlier rejection remains historical evidence for `e8c83f7`; this delta review does not erase it.

I reviewed the fixed commit in a clean detached worktree at `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r3-provider-body-bounds-review-0620473`, based on `e8c83f7e5c1a14b711a4118027b7bb697b96ea0c`. The six-file delta is limited to the two HTTP implementations, bounded body helper/tests, and production retry classifier/tests. The original builder worktree was left untouched and remains clean at the reviewed commit.

### Fixed original findings

[`readProviderBodyBytes`](../../../../internal/llm/provider_body.go:62) now retains bytes returned alongside a read error; [`readBoundedProviderBody`](../../../../internal/llm/provider_body.go:42) still caps the result at the configured limit and marks a limit+1 prefix as truncated. Each non-2xx error-read path now constructs a `ProviderError` from the already-known status and bounded prefix, sets `BodyTruncated`, and joins that typed error with sanitized read and close failures ([`provider.go`](../../../../internal/llm/provider.go:519), [`native.go`](../../../../internal/llm/native.go:196), [`native.go`](../../../../internal/llm/native.go:274)).

An independent real-model probe using an adversarial `http.RoundTripper` passed for status 401 and 400 overflow responses across compatible Generate, native Generate, and native Stream. It forced a read error after the limit+1 byte, then verified the 4 KiB message cap and prefix, `BodyTruncated`, retained status and auth/overflow type, `errors.Is` for both read and close causes, one synchronous close, fatal LLM classification, and no canary in `Error()` or captured slog output.

The production follow-up recognizes the typed oversized sentinel after `core.NormalizeError` so the generic `model_error` wrapper is fatal as required. The committed cancellation and harness-terminal tests passed, and cancellation remains checked before the sentinel.

### [P2] Preserve explicit retry and harness categories ahead of the oversized sentinel

[`DefaultRetryPolicy.Classify`](../../../../internal/production/retry.go:132) checks `ErrProviderResponseTooLarge` before the existing `categorized` and `AgentHarnessError` precedence. That changes established behavior for explicit structured wrappers: a `core.AgentHarnessError{Code: core.ErrCodeTimeout, Cause: ErrProviderResponseTooLarge}` now classifies as fatal and `ShouldRetry` becomes false instead of returning `ErrorTimeout`; `NewError(ErrorTimeout, ErrProviderResponseTooLarge)` is also changed to fatal. The classifier previously honored those explicit categories before generic error fallback. Keep cancellation fatal, keep naked and normalized generic model/tool oversize errors fatal, and perform the sentinel check only after explicit categorized/terminal code decisions have been preserved.

The independent precedence probe confirmed all of these results in one run: naked and normalized generic oversize errors were fatal, cancellation stayed fatal, but the explicit harness timeout category and retryability were lost, as was the explicit categorized timeout. The probe was removed afterward.

### Follow-up verification

Target names were listed before their `-run` commands. Exact independent commands and results:

```text
rtk proxy go test -list '^Test(HTTPStatusSurvivesErrorBodyReadFailure|ReadBoundedProviderBodySanitizesAndPreservesReadCloseCauses|HTTPChatModelBoundsAndRedactsOversizedErrorBody|NativeChatModelBoundsErrorBodiesForGenerateAndStream)$' ./internal/llm
  exit 0; all four target tests were present.

rtk proxy go test -list '^Test(ClassifyNormalizedProviderErrorBeforeGenericHarnessCode|ClassifyNormalizedOversizedProviderResponseIsFatal|ClassifyNormalizedHarnessTerminalCodeOverridesProviderCause|ShouldRetryRefusesCanceledContextAndErrors)$' ./internal/production
  exit 0; all four target tests were present.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/llm -run '^(TestReadBoundedProviderBodySanitizesAndPreservesReadCloseCauses|TestHTTPStatusSurvivesErrorBodyReadFailure|TestHTTPChatModelBoundsAndRedactsOversizedErrorBody|TestNativeChatModelBoundsErrorBodiesForGenerateAndStream)$' -count=1
  exit 0; targeted HTTP/body LLM tests passed under race.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/production -run '^(TestClassifyNormalizedProviderErrorBeforeGenericHarnessCode|TestClassifyNormalizedOversizedProviderResponseIsFatal|TestClassifyNormalizedHarnessTerminalCodeOverridesProviderCause|TestShouldRetryRefusesCanceledContextAndErrors)$' -count=1
  exit 0; normalized classification, harness-terminal, and cancellation tests passed under race.

rtk proxy go test -list '^TestReviewPartialErrorBodyReadRetainsBoundedPrefixAndStatus$' ./internal/llm
  exit 0; independent temporary probe listed.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/llm -run '^TestReviewPartialErrorBodyReadRetainsBoundedPrefixAndStatus$' -count=1
  exit 0; six 401/400 × compatible/native Generate/native Stream subcases passed, including truncation, typed classification, joined causes, sanitization, and close-once checks.

rtk proxy go test -list '^TestReviewOversizeClassificationRetainsExplicitPrecedence$' ./internal/production
  exit 0; independent temporary probe listed.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/production -run '^TestReviewOversizeClassificationRetainsExplicitPrecedence$' -count=1
  exit 1; exact failures: explicit harness timeout got `fatal`, expected `timeout`; its `ShouldRetry` was false; explicit categorized timeout got `fatal`, expected `timeout`. Naked/normalized oversize and cancellation assertions passed.

rtk git status --short --branch && rtk git rev-parse HEAD && rtk git diff --check
  exit 0; independent detached worktree is clean at `0620473eb559a529317ca4a863df26550f765017` after removing temporary probes.
```

This review only covers the provider body-bounds delta; it does not make a broader runtime or retry-policy closure claim.

## Independent final follow-up review: 75525593716a64b9f4666709fbbeb1933fd4a087

**Verdict for this follow-up: ACCEPTED.** The two-file delta restores the explicit timeout precedence that was rejected in 0620473 while keeping typed oversized responses fatal for bare and generic normalized provider/model/tool errors. This accepts the final retry-classification follow-up only; it does not erase the historical rejection evidence above or expand the provider-body-bounds review into a broader runtime closure claim.

I verified the frozen builder worktree at /Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r3-provider-body-bounds-final was clean at the requested commit, then created a separate detached review worktree at /Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r3-provider-body-bounds-review-7552559. The reviewed commit is based on 0620473 and changes exactly internal/production/retry.go and internal/production/retry_test.go.

The classifier now checks cancellation and deadline errors first, preserves explicit categorized errors before inspecting generic normalized harness errors, keeps busy and hook-rejected terminal codes fatal, and checks ErrProviderResponseTooLarge from classifyProviderError only on bare errors or generic model/tool harness errors. The added table verifies both explicit timeout forms (categorized timeout and AgentHarnessError timeout) remain retryable; bare, normalized model, and generic tool oversize errors remain fatal; busy/hook-rejected remain fatal; and a joined cancellation plus oversize cause stays fatal. Existing tests also verify cancellation and deadline causes independently.

Target names were listed before the race command. Exact commands and results from the independent detached worktree:

- rtk proxy go test -list '^Test(OversizedProviderResponsePreservesExplicitRetryPrecedence|ClassifyNormalizedOversizedProviderResponseIsFatal|ClassifyNormalizedHarnessTerminalCodeOverridesProviderCause|ClassifyNormalizedProviderErrorBeforeGenericHarnessCode|ShouldRetryRefusesCanceledContextAndErrors|ShouldRetryRespectsMaxAttempts)$' ./internal/production
  - Exit 0; all six tests were listed.
- rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/production -run '^(TestOversizedProviderResponsePreservesExplicitRetryPrecedence|TestClassifyNormalizedOversizedProviderResponseIsFatal|TestClassifyNormalizedHarnessTerminalCodeOverridesProviderCause|TestClassifyNormalizedProviderErrorBeforeGenericHarnessCode|TestShouldRetryRefusesCanceledContextAndErrors|TestShouldRetryRespectsMaxAttempts)$' -count=1
  - Exit 0; targeted classifier and retry tests passed under race.
- rtk git diff --check 0620473eb559a529317ca4a863df26550f765017 75525593716a64b9f4666709fbbeb1933fd4a087
  - Exit 0; no whitespace errors.
- rtk git status --short --branch
  - Exit 0; detached review worktree clean at 75525593716a64b9f4666709fbbeb1933fd4a087.

No source or test file was modified in the review worktree; no changes were staged or committed.
