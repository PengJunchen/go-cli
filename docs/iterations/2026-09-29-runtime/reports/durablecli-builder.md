# Durable CLI wiring builder report

Date: 2026-09-29  
Worktree: `repair-20260929-durablecli`  
Branch: `codex/repair-20260929-durablecli`  
Base: `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`

## Delivered

- A configured `session.store_path` now selects the prepared durable session path for ordinary prompt and interactive invocations. The path must be a directory; legacy `.jsonl` file paths fail closed before workspace authority or provider setup.
- Prompt and REPL accept `--resume-session <physical-id>` and resume that exact physical session. The ambiguous boolean `interactive --resume` is rejected when durable storage is configured.
- Prompt and REPL bind one real `SessionRunCommitSink` to each accepted core run. Prompt output failures cancel the run, drain the event stream, read the terminal result, and then let the Assembly close owner join the run before releasing the manager-owned session.
- Durable startup and REPL refresh restore agent history only from `PreparedSessionSnapshot.PublishedView.Messages`. REPL navigation uses `BuildFromSnapshot`; its tree and `/branches` view are read-only, while `/resume`, `/fork`, `/clone`, and `/switch` return a typed unsupported error before mutation. Durable REPL skips `PendingSessionWrites` and its flush ticker.
- The existing Assembly close owner closes the durable manager after active run admission becomes idle. Serve rejects durable ACP configuration before token-file writes or listener startup. A durable prompt fork is typed unsupported before creating session or workspace state.
- Added real-file integration coverage for two prompt invocations with exact restart, REPL multi-turn and exact-ID restart, legacy JSONL path rejection without rewrite, prompt-fork early rejection, writer-error drain/join, typed session/history mutation rejection, and serve rejection. The REPL fixture also verifies `/cost` and `/session tree` remain available, no rejected `/retry` reaches the provider, and final messages are restored from the physical JSONL/index after reopening. Updated existing persistence fixtures to use directory-based durable storage.

## Core lease baseline

The optional pre-owner lease capture and accepted-run transfer source is already present in the base (`9258330`). This change does not duplicate it. The new Harness regression verifies an accepted pending run keeps the lease until its callback joins, then releases it exactly once; it is GREEN against the inherited implementation. The existing lease suite continues to cover TurnRunner adoption and rejection behavior.

## Validation

Initial CLI wiring tests were run RED against the baseline and exposed missing default prompt/REPL durable activation, exact physical resume, JSONL fail-closed behavior, pre-listener serve rejection, and prompt drain/join behavior. The history-mutation fixture also fails if the durable dispatcher guard is removed: `/retry` reaches the provider for an extra turn. After the guard is restored, the integration verifies two user turns only and canonical history across restart.

Validation passed:

- `rtk go test -p 2 -count=1 ./internal/cli ./internal/session ./internal/core` — 2,017 tests passed.
- `rtk go test -race -p 2 -count=1 ./internal/cli ./internal/session ./internal/core` — 2,017 tests passed.
- `rtk go vet ./internal/cli ./internal/session ./internal/core` — passed.
- `rtk golangci-lint run ./internal/cli ./internal/session ./internal/core` — no issues.
- `rtk make scan` — exit 0; 371 files scanned, zero errors and 1,547 warnings. It reports `SCAN-006` for `ErrDurableCLIUnsupported` as “only referenced by _test.go files” even though the exported sentinel is the production `Error`/`Unwrap` identity; it stays exported to preserve `errors.Is`. It also reports `SCAN-008` for the pure durable-session helper file, which has no logging side effect to record. Other warnings include existing repository findings.

## Remaining boundary

Durable session fork, ACP serve sessions, REPL session navigation mutations, and history-mutating `/compact`, `/clear`, and `/retry` remain explicitly unsupported. Durable compaction/fork semantics are not implemented. This CLI slice does not close the full R2 runtime plan; it wires the supported default prompt/REPL path to the existing prepared-session and run-owner seams.
