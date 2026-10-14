# R3 BUILD-2 stream breaker implementation

## Source and base

- Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-breaker`
- Branch: `codex/repair-20260929-breaker`
- Exact base: `258c8f6de9d7e03360aa84536acdaa0c1684ca9e` (integrated BUILD-1 stream-join tree)
- Source commit: `b1cd5ea352b9d7c9dcb980eb9988fd999aa05084`

## Behavior

The default breaker now offers optional no-fallback Generate admission and a streaming ticket API without changing the mandatory `CircuitBreaker` interface. Stream tickets are bound to state generations, complete once, hold HalfOpen probe slots through source join, and ignore stale or duplicate completion. Generic `Execute` keeps its configured fallback behavior.

The CLI adapter refuses open Generate/Stream calls with `ErrCircuitOpen` and no synthetic assistant response. One ticket wraps the selected middleware/cycler chain; the adapter joins setup-error and terminal sources, validates one Final at EOF, records source errors and typed in-chain local aborts as failures, and treats ordinary caller-first cancellation as neutral. It settles the ticket after source join but before closing its output, so an outer OutputGuard cannot make a post-close policy decision before provider accounting. Legacy custom breakers remain supported through `Execute`; that compatibility path cannot provide terminal-aware accounting or held HalfOpen reservations.

The two additional contract probes first failed against the implementation: a caller context carrying `LocalStreamAbortCause` left the breaker Closed, and the output channel closed while ticket completion was blocked. Both pass with the fix. The earlier BUILD-2 compile-success RED probes and stream-join source-close proof are recorded in their respective iteration evidence.

## Validation

On the final source commit:

- `GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/production ./internal/llm ./internal/core ./internal/cli` — all four packages passed.
- `go vet ./internal/production ./internal/llm ./internal/core ./internal/cli` — passed.
- `golangci-lint run ./internal/production/... ./internal/cli/...` — no issues found.
- `git diff --cached --check` — passed before the source commit.
- Focused production and CLI circuit-breaker tests — passed.

## Limits

This was scoped verification for the affected packages, not `make verify` or a full repository test run. No live provider or production deployment was exercised. A non-cooperative source that never closes after cancellation intentionally keeps the ticket and adapter output pending. Legacy custom breakers retain their interface compatibility but are not covered by the default breaker's terminal-aware accounting guarantees. No attempt ledger, usage-permit framework, provider implementation, protected file, push, or shared-root checkout was changed.
