# R3 continuation no-replay review

**Status: accepted for the bounded continuation no-replay change** at `1023c22ccd2c29182d45d28944a36c50ed0f4b6f`. The reviewed worktree was clean. I did not modify product source.

This review covers only preventing a failed continuation request from being retried, failed over to another model, or recursively overflow-recovered. It does not establish a shared operation-wide dispatch budget, `AttemptID`, usage/cost accounting, or completion of the broader R3 gate.

## Findings

The implementation uses a private context key distinct from `LoopOwnsContinuation` (`internal/llm/stream_limits.go:16-42`). `LoopAgent.generateWithContinuation` adds it to each continuation request immediately before `streamGenerateWithBudget` (`internal/core/loop.go:1314-1319`); the initial request keeps its original context. The separate marker matters because Loop ownership suppresses middleware-owned length continuation, while this marker suppresses replay after a continuation dispatch error.

Retry and failover return the first error for a marked Generate or Stream setup call (`internal/llm/middleware_retry.go:251-268,295-312`; `internal/llm/middleware_failover.go:89-104,118-132`). Overflow recovery does not start another local continuation on a marked successful length response and returns a marked overflow error without trimming/retrying (`internal/llm/middleware_overflow.go:97-115,247-256`). Its legacy middleware-owned continuation also marks the nested provider call (`:363-396`). These checks preserve ordinary initial-request behavior. Context derivation uses the marked parent for the core stream and overflow stream (`loop.go:1316-1317`; `middleware_overflow.go:154-156`), so cancellation/deadline and other parent values are retained while the marker propagates.

The real `AssembleAgent` HTTP fixture starts with a truncated candidate, then returns 500, 429, or context overflow for the continuation. Each case requires an error, no incremental or final partial content, and exactly two provider requests (`internal/cli/r1_assembly_stream_test.go:305-335`). A separate real HTTP case confirms an unmarked initial 500 still follows the configured retry and succeeds on the second request (`:338-352`). Component tests cover both Generate and Stream retry/failover suppression, overflow retry suppression, suppression of a second middleware-owned continuation, and marker propagation on the legacy Generate and Stream continuation paths (`internal/llm/middleware_retry_test.go:112-132`; `middleware_failover_test.go:232-255`; `middleware_overflow_test.go:71-180`).

No blocking defect was found in this bounded slice. Existing retries and overflow recovery remain available to unmarked initial requests; this change does not claim to unify those local policies under one dispatch budget.

## Verification

The test names were listed before the targeted race runs:

- `rtk env GOMAXPROCS=2 go test -list 'Test(RetryModelMiddlewareDoesNotReplayContinuationRequest|OverflowRecoveryMiddlewareDoesNotRetryContinuationRequest|OverflowRecoveryMiddlewareDoesNotContinueMarkedCandidate|OverflowRecoveryMiddlewareMarksItsOwnContinuationNoReplay|FailoverModelMiddlewareDoesNotFallbackForContinuationRequest)$' ./internal/llm` — listed all five tests.
- `rtk env GOMAXPROCS=2 go test -list 'Test(ContinuationErrorReturnsFailure|ContinuationFailureClassesDoNotReplayIncompleteCandidate|InitialRequestStillUsesConfiguredRetry)$' ./internal/cli` — listed all three tests.
- Core continuation targets were identified from `internal/core/loop_stoplength_test.go` and `loop_guard_stream_test.go` before execution.

Targeted race checks passed:

- `rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestRetryModelMiddlewareDoesNotReplayContinuationRequest|TestOverflowRecoveryMiddlewareDoesNotRetryContinuationRequest|TestOverflowRecoveryMiddlewareDoesNotContinueMarkedCandidate|TestOverflowRecoveryMiddlewareMarksItsOwnContinuationNoReplay|TestFailoverModelMiddlewareDoesNotFallbackForContinuationRequest|TestContinuationErrorReturnsFailure|TestContinuationFailureClassesDoNotReplayIncompleteCandidate|TestInitialRequestStillUsesConfiguredRetry)$' ./internal/llm ./internal/cli` — passed (`llm` 1.023s; `cli` 2.215s).
- `rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestLengthReasonTriggersContinuation|TestContinuationMergesResponses|TestContinuationAppendsPartialContent|TestMaxContinuationAttemptsEnforced|TestContinuationDropsPartialToolCalls|TestMultipleContinuationsAccumulate|TestCompleteResponseGuardContinuationFailurePublishesNoCandidate|TestGuardedStreamBudgetIncludesLengthSegmentToolPayload)$' ./internal/core` — passed (`core` 1.030s).

The full affected package race suites also passed on the same fixed worktree:

- `rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/llm ./internal/core ./internal/cli` — passed (`llm` 2.763s; `core` 4.795s; `cli` 21.340s).
