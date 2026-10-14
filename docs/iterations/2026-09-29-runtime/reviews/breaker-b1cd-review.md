# R3 BUILD-2 breaker stream ownership review

Reviewed source commit `b1cd5ea352b9d7c9dcb980eb9988fd999aa05084` in worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-breaker`, based on `258c8f6de9d7e03360aa84536acdaa0c1684ca9e`.

**ACCEPT for this narrow BUILD-2 slice.** I found no blocking correctness issue in the optional stream-ticket state machine or the CLI adapter lifecycle. `CircuitBreaker` remains unchanged; default tickets reserve HalfOpen probes, settle once, and ignore stale generations. The adapter refuses an open circuit without invoking the provider, holds Final until source close, validates terminal shape, joins failed/canceled sources before settling, preserves typed local-abort failures against later caller cancellation, and leaves post-close OutputGuard decisions outside breaker accounting. Retry setup and configured ModelCycler fallback remain inside one admission.

## Independent evidence

- On a disposable worktree at base `258c8f6de9d7e03360aa84536acdaa0c1684ca9e`, I extracted the baseline-compatible adapter tests from the candidate and removed imports used only by later tests. The test package compiled. The focused RED run failed the expected behavior assertions: typed Open refusals (Generate and Stream), midstream failure accounting, Final hold through HalfOpen source close, and the four malformed/missing-terminal cases. The post-close OutputGuard placement test passed on the base. The disposable worktree was removed afterward.
- Candidate GREEN: `GOMAXPROCS=2 rtk go test -p 2 -race -count=5 ./internal/production ./internal/llm ./internal/core ./internal/cli` — exit 0, RTK reported 8,560 passing test events across four packages.
- `rtk go vet ./internal/production ./internal/llm ./internal/core ./internal/cli` — exit 0.
- `rtk gofmt -d` on all changed Go files — no output. `rtk git diff --check 258c8f6..b1cd5ea` — exit 0. The candidate worktree is clean at report-only HEAD `44682918c5cf124c6c2b6b6e4fbbaaa8229ca162`; implementation source remains `b1cd5ea352b9d7c9dcb980eb9988fd999aa05084`.

The implementation report correctly limits validation to affected packages. This review does not claim `make verify`, a full repository run, live provider behavior, or production deployment validation.

## Root integration boundary

The shared root is `49d568a7f9eee992fedfb97a1fcfe2d53494de35`, with merge base `258c8f6de9d7e03360aa84536acdaa0c1684ca9e`. Relative to that base, root-only source changes touch `internal/cli/assemble.go`; the candidate changes `internal/production/circuit.go`, adds `internal/production/circuit_breaker.go` and `internal/cli/circuit_breaker_adapter.go`, and removes the old adapter block from `internal/cli/output_guard_adapter.go`. The root's current version of `output_guard_adapter.go` still has that old block, so the deletion applies to the expected code. No candidate source change overlaps the root-only `assemble.go` diff. No integration was performed in this review.
