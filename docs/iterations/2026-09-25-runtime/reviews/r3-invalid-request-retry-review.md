# R3 invalid-request retry review

Reviewed frozen commit `6473a7a75c78b4c58bef221ba7fc179893469d23` (parent `c3de6b7ec0f4506e68bbaf35d68bb28e989c964c`) in `/Users/pengjunchen/.codex/worktrees/r2-provider-http-retry`. The worktree was clean. The change adds `ErrTypeInvalidRequest`, maps ordinary HTTP 400 and 404 to it while retaining 400 overflow detection ahead of that mapping, and treats the new category as fatal in both LLM and production retry classification.

## Review result

Accepted. The change is narrowly scoped and preserves the requested precedence:

- 400 overflow remains `ErrTypeOverflow`; an overflow marker outside the bounded diagnostic prefix now correctly falls through to ordinary invalid-request classification.
- 401/403 auth and 429 rate-limit cases retain their existing types and retry categories. 5xx and network errors remain transient.
- LLM middleware does not retry ordinary 400 or 404 errors for Generate or Stream. Existing context-cancellation checks remain ahead of retry attempts.
- Production classification recognizes invalid-request provider errors inside normalized generic model/tool harness errors. Explicit categorized and harness timeouts retain their retryable timeout precedence; busy and hook-rejected harness codes remain terminal.
- The CLI regression still makes one provider request and verifies a runtime-generated response canary is absent from traces, logs, stdout, and stderr. Its trace assertion now expects the safe `invalid_request` type.

## Test discovery and validation

The affected tests were enumerated from these files:

- `internal/llm/provider_error_test.go`: `TestNewProviderError_StatusMapping`, `TestClassifyError`.
- `internal/llm/provider_body_test.go`: `TestProviderErrorOverflowClassificationUsesBoundedPrefix`.
- `internal/llm/middleware_retry_test.go`: `TestRetryModelMiddlewareDoesNotRetryInvalidRequestProviderErrors`.
- `internal/production/retry_test.go`: `TestClassifyWrappedProviderError`, `TestClassifyNormalizedProviderErrorBeforeGenericHarnessCode`, `TestOversizedProviderResponsePreservesExplicitRetryPrecedence`, `TestClassifyNormalizedHarnessTerminalCodeOverridesProviderCause`, `TestShouldRetryRespectsMaxAttempts`, `TestShouldRetryRefusesCanceledContextAndErrors`.
- `cmd/cli/main_test.go`: `TestDefaultTraceAndLogsRedactProviderErrorBody`.

All selected tests passed under the race detector:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^(TestNewProviderError_StatusMapping|TestClassifyError|TestProviderErrorOverflowClassificationUsesBoundedPrefix|TestRetryModelMiddlewareDoesNotRetryInvalidRequestProviderErrors)$' ./internal/llm
ok github.com/pengjunchen/go-cli/internal/llm 1.025s

rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^(TestClassifyWrappedProviderError|TestClassifyNormalizedProviderErrorBeforeGenericHarnessCode|TestOversizedProviderResponsePreservesExplicitRetryPrecedence|TestClassifyNormalizedHarnessTerminalCodeOverridesProviderCause|TestShouldRetryRespectsMaxAttempts|TestShouldRetryRefusesCanceledContextAndErrors)$' ./internal/production
ok github.com/pengjunchen/go-cli/internal/production 1.025s

rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^TestDefaultTraceAndLogsRedactProviderErrorBody$' ./cmd/cli
ok github.com/pengjunchen/go-cli/cmd/cli 1.094s
```

No source changes were made during review.
