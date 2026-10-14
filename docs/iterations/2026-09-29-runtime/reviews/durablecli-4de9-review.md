# Durable CLI source review

Reviewed detached snapshot `4de967cf3459a99eeed38f8ae69c231141f8dfd2` (parent `9a4667282aedd887489e8047b4a9b5b9b10f9e5e`, base `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`). Read `reports/durablecli-builder.md`, the R2 CLI handoff, and the R2 contract. No product source was changed.

## Findings

### P2 — Durable REPL can turn a failed run bind into successful command completion

At `internal/cli/repl_session.go:865-871`, `bindRun` failure is represented as `OutcomeError` plus `ExecutionQuiescent`. `durableTurnIsJoinable` accepts that pair (`:777-783`); after refresh, `executeTurn` prints `turnErr` and returns `nil` (`:740-750`). `readInput` therefore continues (`:667-672`), and the REPL can eventually exit successfully after a durable binding/persistence setup failure. The report's other failure boundaries are mostly fail-closed, but this path does not return the error to the command caller as R2 requires.

Minimal RED: inject a durable `bindRun`/sink-construction error while `snapshotSelected` still succeeds; assert `REPLSession.Run` returns that error and does not accept another user turn or call the provider. Preserve the existing recoverable handling for ordinary provider errors if desired.

### P2 — “Restart” fixtures do not prove a process restart or crash recovery

`TestPromptDurableTwoInvocationsResumeExactPhysicalSession` calls `promptCmd.Run` twice in the test process (`internal/cli/durable_cli_wiring_test.go:107-140`). `TestInteractiveDurableResumeUsesExactPhysicalSessionAfterRestart` likewise calls `REPLSession.Run` twice in-process (`:308-353`). The durable wiring fixtures contain no built-binary subprocess. `cmd/cli/auth_persistence_e2e_test.go` does build and run the real CLI (`:38-56,111-139`), but exercises credential persistence and does not configure durable sessions. Thus exact resume after helper/handle reopen is covered; process exit, SIGKILL, and live-child quarantine are not.

Minimal real-process proof: extend the built-binary pattern in `cmd/cli/auth_persistence_e2e_test.go`; isolate `HOME`, `XDG_CONFIG_HOME`, and the session directory; configure a loopback fake provider; run child A to create and finish a durable prompt, then child B with the exact emitted physical ID. Assert B's provider request includes A's canonical rows and verify JSONL/index after both children exit. Add a separate deterministic crash barrier after input sync and before provider dispatch; kill the actual CLI child there, resume in a second CLI child, and assert the input is restored with no replay. A test that only calls `PrepareResume` or invokes `Run` twice in one PID must remain unit/in-process evidence.

## Source boundaries checked

- Prompt and interactive configured-session paths prepare the workspace authority and selected physical session before `AssembleAgent` (`prompt.go:199-209`, `repl_session.go:292-310`, `durable_cli_session.go:72-117`). `--resume-session` selects an exact physical ID; ambiguous boolean resume and legacy `.jsonl` paths reject, and durable prompt fork returns typed unsupported before workspace initialization.
- `assembleSession` exits before opening the legacy JSONL writer when durable mode is active (`assemble.go:1663-1675`). REPL startup and post-turn refresh derive history/tree from the selected prepared-session snapshot (`repl_session.go:350-361,786-802`); the durable path does not start `PendingSessionWrites`/its flush ticker. The run adapter captures its writer and identity from the same locked prepared handle (`session/run_commit.go:45-95`).
- Prompt output errors cancel, keep draining events, collect the terminal result, and close through the assembly owner (`prompt.go:250-282,306-325`; existing `TestPromptDurableOutputFailureDrainsRunAndReturnsLease`). Core Harness/TurnRunner lease-handoff tests cover pending-return versus callback join (`core/run_commit_lease_handoff_test.go:196-235,247-312`). Assembly close errors are joined into command results (`assembly_lifecycle.go:600-606`; prompt/REPL deferred close paths).
- `/compact`, `/clear`, and `/retry` are rejected before handler dispatch (`slash.go:244-249`); durable session navigation uses a read-only handler that rejects mutation before changing the tree (`session/slash_commands.go:87-101`). Exact-session branch mismatch is rejected by the manager rather than reopening the same file (`durable_session_manager.go:172-205`). Durable sub-agent dispatch is typed unsupported before creating/running a child, with a second guard at the real child runner (`core/subagent_tool.go:129-148`, `core/real_subagent_runner.go:49-56`).
- Durable ACP `serve` is a typed unsupported feature before token generation/file write and listener startup (`serve_cmd.go:86-92`, token path begins at `:127`). This is an explicit current feature boundary, not a regression in serve startup ordering. Close and unsupported sentinels retain `errors.Is` identity through `Unwrap`/`%w` paths.

The storage/index path also explicitly blocks **all** new actions during unresolved quarantine because the adapter has no safe-read permit (`session/workspace_index.go:274-278`). This is a conservative unsupported capability, not unsafe admission; the R2 contract's scoped read exception is not implemented. Existing in-process coverage maps marker recovery to `TestPrepareResumeReconcilesAbsentReservedMarkerAsInterrupted` and `...PresentMarkerAsUnknownWithoutReplay` (`session/durable_recovery_test.go:148-241`), cross-session mutation exclusion to `TestWorkspaceIndexBlocksConcurrentMutationAcrossSessions` (`session/workspace_index_test.go:518-566`), and unscoped-read denial to `TestWorkspaceIndexUnresolvedMarkerBlocksAllNewActionsWithoutScopeProof` (`:787-803`). These do not replace the contract's real orphan-child process test (`TestRecoveryBlocksWorkspaceMutationWhileOrphanChildLives`); that named test is absent from this source snapshot.

## Validation

- `rtk go test -race -p 2 -count=1 ./internal/cli ./internal/session ./internal/core` — exit 0; 2,017 tests passed across the three packages.
- Focused registrations executed successfully: one CLI durable prompt resume test, seven session recovery/index/sink cases, and four core lease/child/reopen cases. (The full race run also includes the current registered tests.)
- No real built CLI durable-session child, external provider, PTY, or orphan-child crash/quarantine test was run. The subprocess acceptance gap above remains open; this review does not declare R2 closed.
