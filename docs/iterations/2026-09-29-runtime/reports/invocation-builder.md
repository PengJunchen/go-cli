# Invocation owner builder report

- Baseline: `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`
- Branch: `codex/repair-20260929-invocation`
- Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-invocation`

## Change

`internal/cli` now returns a private `assemblyClosePendingError` from the existing pending path. Its `Done()` is the actual close owner's `o.done`, and its unwrap chain retains `ErrAssemblyClosePending` and the prior cause. `AssemblyCloseCompletions` walks wrapped and joined errors, accepts only that private type, deduplicates channels, returns every actual pending Assembly channel, and fails closed when any such channel is missing. It ignores unrelated `Done()` interfaces.

`cmd/cli.runCLI` now returns the exit code and original error chain. `run` extracts all actual Assembly channels and waits on their aggregate barrier for a fixed 20-second producer budget. If any channel remains open or completion is unknown, it reports pending and returns nonzero without ending the root span, shutting down the exporter, closing the invocation log, or restoring the logger. A timer expiry never marks producer completion.

After every producer joins, the invocation owner records the command status, ends the root span, calls the existing no-op `Tracer.Flush`, and makes the existing bounded `ShutdownWithReport` attempt. `initialDone` reflects that first attempt; full owner `Done()` includes logging cleanup. If the exporter report is pending, the same owner calls `ShutdownWithReport(context.Background())` on the same reporter and retains the invocation logger/file until `OwnerFinished` is true. Logger restore is guarded by `slog.Default() == invocationLogger`. No tracing source or exporter API changed.

## Regression evidence

RED tests first failed to compile because the typed Assembly extractor and invocation owner did not yet exist. After implementation, the focused and complete package runs passed.

- `internal/cli/assembly_close_test.go` verifies an earlier closed fake `Done()` cannot replace the actual Assembly close channel, multiple actual pending channels are returned, duplicate channels are deduplicated, and a missing actual channel fails closed.
- `cmd/cli/invocation_owner_test.go` verifies the producer barrier defers root/exporter/log cleanup, all producer channels must join, a real `AsyncExporter` delivers the late child and root before sink shutdown with zero `RejectedAfterClose`, and an exporter callback can log after the first bounded report while the invocation log remains open. It also checks the logger compare-and-restore guard.
- The actual executable calls `os.Exit(run(...))`; this implementation does not claim the retained owner survives that process exit. A still-pending owner returns nonzero with application-level trace/log finalization deferred. The tests exercise the main-package wait boundary in process; they do not launch a subprocess to observe the operating system's `os.Exit` behavior.

## Validation

- RED: `rtk go test -p 2 -count=1 ./internal/cli -run '^TestAssemblyCloseCompletions'` failed before the extractor was implemented, as expected.
- RED: `rtk go test -p 2 -count=1 ./cmd/cli -run '^TestInvocationCloseOwner'` failed before invocation-owner types were implemented, as expected.
- `rtk go test -p 2 -count=1 ./cmd/cli ./internal/cli` — passed, 785 tests across 2 packages.
- `rtk go test -race -p 2 -count=1 ./cmd/cli ./internal/cli` — passed, 785 tests across 2 packages.
- `rtk go test -race -p 2 -count=1 ./internal/core` — passed, 864 tests.
- `rtk go vet ./cmd/cli ./internal/cli` — passed.
- `rtk golangci-lint run ./cmd/cli ./internal/cli` — passed, no issues.
- `rtk git diff --check` — passed.

These checks cover the edited packages and their race-sensitive owner paths. They are not a repository-wide `make verify` run or a claim of full R7 lifecycle acceptance.
