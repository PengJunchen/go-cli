# R0 privacy code review

Review date: 2026-09-25
Review target: privacy lane commits `47705d2a6bb397a1cc515b902f493165d9c82161` and `2fdd650527c25ed504794c3c8987adb49baa1d1b`
Review type: independent source review and focused race tests; no product files changed.

## Decision

**Accepted for the R0 privacy slice.** The earlier error-field and span-status P1 findings are closed. This is not a whole-runtime release sign-off: retry/visibility policy remains an R3 decision, and other R0/R4 findings are tracked separately.

## Evidence

The actual default streamed provider-failure path is covered: `cmd/cli.run` → `AssembleAgent` → `LoopAgent` → `HTTPChatModel.Stream` → `roundTrip` → `newProviderError`, followed by Loop error status/log/event handling and CLI error output. The real CLI test uses a local HTTP provider that returns 400 and echoes a prompt canary. It asserts the invocation remains failed, only one provider request occurred, and the canary is absent from both production tracers' JSONL files, the CLI log, stdout, and stderr. It also verifies the exported trace retains error status, `error_type`, HTTP status, and span/operation identity.

`ProviderError.Error()` now reports only the safe category, provider, and HTTP status; the response body remains available on `ProviderError.Message` for in-memory classification. The reviewed production path does not serialize or log `Message`. `LoopAgent` and both model Generate spans add bounded `error_type` and `http_status_code` attributes. The default `RedactingExporter` masks every non-empty `StatusMessage` while preserving `SpanStatusError`; explicit `full` still passes the message through and `off` clears it. The default slog redactor masks `err`, `error`, `cause`, and `original_error`, including nested/grouped attributes, while preserving safe diagnostic fields.

The follow-up test `TestFailureTurnSynthesizer_ProviderErrorBodyKeywordsAreNotRecoverySignals` constructs a typed 503 error whose private response body contains `temporary` and `timeout`. It verifies that the safe typed summary still includes category and status, the body does not appear in the synthesized content, and `IsRecoverable` returns false. Since that predicate is the gate used by `FailureSynthesisMiddleware` before issuing its one continuation run, remote body keywords no longer trigger synthesis retry.

I ran this focused race selection from the privacy worktree; all four packages passed:

```text
GOMAXPROCS=2 go test -race -p 2 ./internal/tracing ./internal/llm ./internal/core ./cmd/cli \
  -run '^(TestDefaultTraceAndLogsRedactProviderErrorBody|TestRedactingExporterRedactsStatusMessageAndKeepsSafeMetadata|TestRedactingSlogHandlerMasksErrorFieldsAndKeepsDiagnostics|TestProviderError_Error|TestFailureTurnSynthesizer_ProviderErrorBodyKeywordsAreNotRecoverySignals)$' \
  -count=1 -timeout 90s

ok   github.com/pengjunchen/go-cli/internal/tracing
ok   github.com/pengjunchen/go-cli/internal/llm
ok   github.com/pengjunchen/go-cli/internal/core
ok   github.com/pengjunchen/go-cli/cmd/cli
```

The privacy builder separately recorded the complete four-package race suite as passing in `evidence/privacy-green.log`.

## Retry boundary retained for R3

This review accepts that provider response-body words such as `temporary` or `timeout` no longer drive **failure-synthesis** retry. It does not claim that all provider retries are closed: `internal/llm/middleware_retry.go` still classifies typed `ErrTypeServer` as transient, while dispatch visibility and ambiguous side-effect outcomes need a unified R3 matrix. Keep that behavior out of this privacy patch; do not reintroduce the response body into `Error()` or classify every HTTP 5xx as newly safe to retry. The existing `context.Canceled` handling in `DefaultFailureTurnSynthesizer.IsRecoverable` is also deferred to R3, as directed by the parent review.

## Scope notes

The explicit `RedactionLevelFull` configuration remains an intentional opt-out. Directly constructed exporters or custom log handlers outside the default CLI wrappers are outside this R0 slice. Redacted byte lengths and fixed operation names remain visible by policy. This report closes only the provider-error privacy path and does not override the separate R4 executor-identity finding or other iteration gates.
