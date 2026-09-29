# R3BUILD-1 stream source close/join implementation

## Scope

This slice establishes one source-ownership contract across model middleware and its consumers: a reader that stops accepting a stream cancels the source with a typed local cause, drains the same source channel to close, and only then closes its output. A `(channel, error)` setup result is canceled and joined before the caller retries or returns. A `(nil, nil)` stream setup is rejected.

`llm.LocalStreamAbortCause` is the narrow public cancellation cause. Its error text is fixed (`local stream abort`) so provider response text cannot leak through context logging; `Unwrap` preserves `errors.Is`/`errors.As` classification. Caller cancellation keeps its inherited cause, while a normal joined attempt is released with `context.Canceled` and is not classified as `LocalStreamAbortCause`.

The implementation covers failover and retry attempt ownership, timeout, sanitize, loop detection, validation, overflow recovery, core stream budget consumption, and the CLI output guard. Terminal-validating readers hold a candidate Final until the source closes, reject data after Final and close-without-Final, and publish a single terminal error. The output guard does not cancel a provider context after its source has closed and candidate checking later denies the response.

An assembled CLI loopback test gates the real provider response body's `Close`. It waits until the streaming request is confirmed as `stream: true` and the response body is installed in the gate, then cancels the caller and verifies the assembled event channel stays open until body cleanup returns. This test is a conformance check; no provider implementation source was changed.

The cycler lifecycle correction is deliberately excluded from this commit and is tracked as a separate follow-up commit in this worktree.

## Validation

The BUILD-1-only worktree state was tested with the cycler source and cycler tests restored to the base commit:

- `GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/llm ./internal/core ./internal/cli` — all three packages passed.
- `go vet ./internal/llm ./internal/core ./internal/cli` — passed.
- `golangci-lint run ./internal/llm ./internal/core ./internal/cli` — no issues found.
- `git diff --check` — passed.
- Assembler response-body cleanup barrier test — passed three consecutive focused runs before the final race gate.

No breaker, live detector, assembly policy, config, native/provider source, or protected/shared-ledger file is part of this slice.

## Cycler lifecycle follow-up

The selected-model cycler now joins a channel returned alongside a setup error before running cleanup or deciding whether to fall back. It rejects `(nil, nil)` and suppresses fallback replay during continuation. Every successful selected stream uses one forwarding owner, including models without a cleanup callback, so caller cancellation stops forwarding and drains the same channel; normal completion joins the source, completes optional cleanup, releases the attempt context, then closes the output.

The cycler tests hold source close and cleanup independently to prove source-close → cleanup-complete → output-close order. They also assert caller cancellation remains `context.Canceled`, setup failure reaches the source as `LocalStreamAbortCause` preserving the setup error through `Unwrap`, and fallback begins only after join and cleanup.

The final integrated worktree passed:

- `GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/llm ./internal/core ./internal/cli` — all three packages passed with the cycler follow-up present.
- `go vet ./internal/llm ./internal/core ./internal/cli` — passed.
- `golangci-lint run ./internal/llm ./internal/core ./internal/cli` — no issues found.
- `git diff --check` — passed.

## Independent review follow-up: timeout setup budget and cycler error abort

The two implementation commits above were left intact after an independent review found two additional source defects. Their earlier passing package gates did not cover these cases, so those passes are historical evidence and do not establish acceptance of the reviewed behavior.

The timeout reader previously started its total timer only after `next.Stream` returned. A synchronous provider setup could therefore block past the configured total timeout. The follow-up now creates one deadline context before calling `next.Stream` and keeps that same deadline across setup and chunk forwarding. A configured deadline reaches the source as `LocalStreamAbortCause` wrapping `context.DeadlineExceeded`; an earlier caller cancellation keeps its inherited cause. Setup failures still cancel and drain any returned source channel before returning.

The cycler previously forwarded a successful stream's `Error` chunk and continued reading without canceling the selected attempt. The follow-up forwards that one terminal error, cancels with a typed local cause, drains the same source channel to close, then completes cleanup before closing the output. Existing caller-cancel coverage continues to verify an inherited caller cause stays distinct from a local abort.

Both review probes failed before the fixes for the expected reasons: synchronous timeout setup returned only after external release instead of observing the configured deadline, and the cycler source did not observe cancellation after its error chunk. After the fixes, both review probes passed; additional focused tests verify caller deadlines are not relabeled as local timeouts and caller cancellation remains the first cause when it precedes a source error. The final-target checks passed:

- `GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/llm ./internal/core ./internal/cli` — 2,258 tests passed across all three packages.
- `go vet ./internal/llm ./internal/core ./internal/cli` — passed.
- `golangci-lint run ./internal/llm ./internal/core ./internal/cli` — no issues found.
- `git diff --check` — passed after the follow-up report and source changes.

The follow-up changed only `internal/llm/middleware_timeout.go`, `internal/llm/middleware_timeout_test.go`, `internal/llm/cycler.go`, and `internal/llm/cycler_stream_test.go` in addition to this report. It does not modify native/provider sources or the breaker/live-detector slice.
