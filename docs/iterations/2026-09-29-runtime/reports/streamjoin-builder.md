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
