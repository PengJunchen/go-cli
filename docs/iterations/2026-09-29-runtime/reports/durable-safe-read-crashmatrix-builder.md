# Durable CLI safe-read crash matrix builder report

Date: 2026-09-30
Worktree: `go-cli-repair-20260930-saferead-crashmatrix`
Branch: `codex/repair-20260930-saferead-crashmatrix`
Base: `8e2fa266c6e86d31bf7d33cb8e0941ca41daa4f4`

## Scope

This test-only stage verifies the accepted scoped Read/LS quarantine admission through real built `./cmd/cli` processes. It adds no product source changes and does not modify the root index. The test uses the accepted SafeRead implementation already integrated at the source base, alongside the live-orphan fixture accepted in the preceding crash-matrix stage.

The test is `TestDurableCLIBinaryAllowsScopedReadersDuringLiveOrphanAndRetainsMutationQuarantine` in `cmd/cli/durable_cli_subprocess_e2e_test.go`.

## Process and durable-file evidence

The test builds the actual CLI binary from this worktree and runs it with isolated `HOME`, XDG config/cache, temp directory, durable session store, and one shared workspace directory. The builtin allowlist is `bash`, `read`, `ls`, and `write`; a loopback OpenAI-compatible peer supplies the calls in a fixed sequence. The workspace contains a preexisting file whose unique content is asserted in the follow-up provider request after the actual Read executes.

The first physical session starts a real managed Bash process. Before releasing its effect gate, the test reads back the synced session intent and workspace `dispatch_ready` row. Bash appends nonce-bound start and visible-effect records to append-only ledgers and syncs each write. The CLI parent is then killed only through its owned `exec.Cmd.Process.Kill`. The original session still has no receipt or terminal, its workspace action has one dispatch and zero resolutions, the two ledgers each contain one record, and the original invocation's heartbeat continues advancing after parent exit.

Two new physical CLI sessions then exercise the real builtin `read` and `ls` tools against the same captured workspace while the Bash mutation marker remains unresolved. Read returns the unique file content to the model; LS returns the existing filename. Each session has a real successful `dispatched` receipt, executor evidence, and terminal. The authoritative workspace JSONL records one dispatch and one resolution for each reader, while the old Bash action remains at one dispatch and zero resolutions. The index contains three dispatched actions and two resolutions after both readers.

A third new physical session calls the real builtin `write`. The CLI exits with the observed quarantine rejection; its intent is persisted without a receipt or terminal, the target file is absent, and its action has zero dispatches and zero resolutions. The index remains at three dispatched actions and two resolutions. The original Bash start/effect ledgers remain at one record each and its heartbeat continues to advance.

The test then reopens only the exact old physical session ID. Real recovery appends the existing Bash action's `unknown`/`dispatched` receipt and the run commits one successful terminal without rerunning Bash. The ledgers remain at one invocation and one effect; the old mutation stays unresolved, and both safe-read actions retain their own completed resolutions. Finally, the nonce-bound release file lets the actual orphan exit; the test checks its invocation-specific exit proof and confirms child exit alone does not resolve the old action.

All session and workspace-index assertions read back the actual newline-delimited JSONL files, checking source IDs, action IDs, tool-call IDs, workspace identity, receipts, terminal rows, index sequence/record uniqueness, dispatch totals, and exact per-action resolutions. Cleanup always opens the fixture gates and release file, then waits with a deadline for exit proofs for recorded invocations; it sends no guessed PID or process-group signal.

## Verification

- `rtk proxy go test -v -count=1 ./cmd/cli -run '^TestDurableCLIBinaryAllowsScopedReadersDuringLiveOrphanAndRetainsMutationQuarantine$'` — passed on Darwin; it built the actual CLI, ran Read, LS, rejected Write, and exact-resumed the old session; no skips, package time 32.452s.
- `rtk go test -race -count=1 ./cmd/cli` — passed; 64 tests in one package.
- `rtk go vet ./cmd/cli` — passed.
- `rtk golangci-lint run ./cmd/cli` — no issues found.
- `rtk git diff --check` — passed before commit.

These checks cover the CLI package and named Darwin actual-binary scenario; they do not establish full `make verify` or other-OS process behavior. The pre-SafeRead RED control details follow.

## Pre-SafeRead RED control

The unchanged test was also run on source commit `25b88ba890f59be488e2ea5defbf09186baf873d`, before SafeRead. A disposable detached worktree received only the accepted prior test-fixture diff from `ba6af18c12de9bbe317d91f8e65f818471476e62` and this new test diff; no production source was copied or changed. The exact same `rtk proxy go test -v -count=1 ./cmd/cli -run '^TestDurableCLIBinaryAllowsScopedReadersDuringLiveOrphanAndRetainsMutationQuarantine$'` command compiled and built the real CLI.

The control created the old Bash intent and synced dispatch marker, observed the append-only effect ledger at one record, killed only the owned CLI parent, and observed the old child heartbeat advance. The fresh real Read session then exited 1 when persisting its workspace dispatch marker: `session: workspace is quarantined by unresolved actions`. The CLI also logged terminal/transcript validation errors after rejecting the unreceipted tool call. This is the expected pre-SafeRead failure at the reader admission boundary; the control does not claim later matrix assertions or JSONL/index readback ran. The test reported only the expected exit-code assertion failure and no cleanup failure. Its registered cleanup opens the nonce-bound effect gate and release file, waits for the fixture-specific exit ledger, and requires the heartbeat to stabilize; a cleanup failure would have been reported as an additional test failure.

The temporary pre-SafeRead worktree is separate from this stage and is removed after the control. The accepted-source GREEN run remains the evidence for the complete Read/LS/Write/resume matrix.
