# Invocation owner independent review

- **Decision:** ACCEPT the bounded invocation-owner slice at `4eeeb7e6737b93404e3a72d3f785b3c160cc05d6`.
- **P1/P2:** None found.
- **Reviewed checkout:** detached worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-invocation-review-4eee`, exact target SHA, clean before review.
- **Contract sources:** `reports/invocation-builder.md` and the primary `invocation-owner-design.md`.

## Findings

`assemblyClosePendingError` is private, and `AssemblyCloseCompletions` matches only `*assemblyClosePendingError`; it does not use `errors.As` for arbitrary `Done` interfaces. It walks both single and joined unwrap trees, deduplicates by channel identity, and returns invalid when any actual pending owner lacks a channel. `pending()` carries the existing close owner's `o.done`. The adversarial tests cover an earlier closed unrelated `Done`, multiple owners, duplicate channels, and a nil actual channel.

`runCLI` returns the original `error` chain with the mapped exit code. On the pending path it avoids formatting that chain. `runWithInvocationWaitBudget` extracts all actual channels and passes their aggregate barrier to the owner. Invalid completion remains a nil barrier; the wait returns nonzero and the owner does not advance teardown. The production budget is fixed at 20 seconds and applies only to the caller's wait for producer completion. A timer expiry rechecks the barrier and does not close it or cancel Assembly work.

The owner waits on the real producer barrier before ending the root span or beginning exporter shutdown. The first exporter report is bounded and treats `!OwnerFinished` as pending. In that case the same owner calls `ShutdownWithReport(context.Background())` on the same reporter; only a positive `OwnerFinished` permits log close and logger restoration. The restore is guarded by `slog.Default() == invocationLogger`. The gated `AsyncExporter` tests verify late child/root delivery before sink shutdown, a callback logging after the first bounded report while the invocation log is open, and retention of a newer logger.

Pending diagnostics use stable messages and numeric exporter counts; the original Assembly cause is retained for `errors.Is`/`errors.Unwrap` but is not included in `assemblyClosePendingError.Error()`. `runCLI` suppresses formatting for errors matching `ErrAssemblyClosePending`, so pending panic/callback causes are not printed by this boundary. The actual `main` entry point is `os.Exit(run(...))`: after `run` returns pending, process exit terminates the retained goroutine. The builder report states this limit accurately and does not claim background work survives process exit.

The tests are owner/barrier tests inside the main package and an extractor test in `internal/cli`; they are not a subprocess test of the shipped `go-cli` executable. There is no existing seam that can deterministically inject a still-pending Assembly into the real executable. A test-binary helper that calls `os.Exit` would demonstrate Go process semantics, not the production CLI path, so I did not add one or treat its absence as a blocker for this slice.

## Verification

- Test inventory: `rtk run go test ./cmd/cli ./internal/cli -list TestInvocation` listed all five invocation-owner/barrier tests; `rtk run go test ./internal/cli -list TestAssemblyCloseCompletions` listed both adversarial extractor tests.
- `rtk go test -race -p 2 -count=1 ./cmd/cli ./internal/cli` — passed, 785 tests across 2 packages.
- `rtk go vet ./cmd/cli ./internal/cli` — passed.
- The builder's `reports/invocation-builder.md` additionally records package lint and `internal/core` race runs. This review does not claim repository-wide `make verify`, real executable subprocess coverage, or full R7 lifecycle acceptance.
