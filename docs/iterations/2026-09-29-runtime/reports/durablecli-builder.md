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

### Real CLI binary subprocess evidence

Added `cmd/cli/durable_cli_subprocess_e2e_test.go`. It builds `./cmd/cli` as a real executable, starts separate child processes with isolated `HOME`, config, XDG paths, session store, and work directory, and directs the configured OpenAI provider to a deterministic loopback HTTP peer. The provider asks the production builtin `write` tool to create `durable-effect.txt`; no external LLM or in-memory runtime substitutes are used.

The final verbose command passed with no skips:

```text
rtk proxy go test -v -count=1 ./cmd/cli -run '^(TestDurableCLIBinaryPersistsWriteReceiptAndDoesNotReplayAfterRestart)$'
PASS
ok github.com/pengjunchen/go-cli/cmd/cli 31.804s
```

The test's actual binary build exited 0 with empty build output. In the normal process-exit case, the first child exited 0 with empty stderr and stdout:

```text
Session ID: s_6f54c8bc916c648580e3eed538705c2d
write
wrote 11 bytes to /private/var/folders/md/b0sr21q97jsg2gfn9lymdw3h0000gn/T/TestDurableCLIBinaryPersistsWriteReceiptAndDoesNotReplayAfterRes1493676179/002/work/durable-effect.txt
provider completed call
```

The resumed child exited 0 with empty stderr and stdout:

```text
Session ID: s_6f54c8bc916c648580e3eed538705c2d
provider completed call
```

Its request included both accepted prompts and the original `call-durable-write-once`. The physical JSONL contained 11 version-1 rows with unique source IDs, one action (`action-f436a098fc9e6091`), one successful write receipt, the matching workspace `dispatch_ready` and `execution_resolution` rows, and two distinct successful terminal checkpoints: `terminal-dd6c44b936b47ffd` / run `9dfaf1678ed0e5f2` / seq 8, and `terminal-73612a3120f7bf5b` / run `b6aaf343733e1275` / seq 11. The actual managed effect remained `effect-once`, with one `write.done` across both processes.

A second scenario held the loopback peer's second response behind a barrier after the child had synced the successful receipt and workspace resolution. At the barrier the physical session had 6 JSONL rows and no run terminal. The child stdout was `Session ID: s_731353d309adac83a0389292bd17922f`, `write`, and `wrote 11 bytes to /private/var/folders/md/b0sr21q97jsg2gfn9lymdw3h0000gn/T/TestDurableCLIBinaryPersistsWriteReceiptAndDoesNotReplayAfterRes3237648970/002/work/durable-effect.txt`; stderr was empty. It was then killed. The observed process state was `signal: killed`, exit code `-1` (`*exec.ExitError`). Restarting with that exact ID exited 0, printed `Session ID: s_731353d309adac83a0389292bd17922f` and `provider completed call` with empty stderr, did not issue a second write, and added one successful terminal checkpoint `terminal-84263af32241c1fb` / run `9b0f49bb5badb9a2` / seq 9. The final physical file had 9 version-1 rows with unique source IDs, retaining the same action ID `action-008289fa090c9dc7`; the workspace index still had exactly one dispatch and one matching resolution.

Captured structured log records include the actual isolated config/session selection and side effect, for example:

```text
time=2026-09-29T15:45:02.413Z level=INFO msg=cli_prompt_durable_session physical_session_id=s_6f54c8bc916c648580e3eed538705c2d
time=2026-09-29T15:45:02.419Z level=INFO msg=llm_stream_start op=llm.stream provider=openai model=durable-cli-fixture messages=2
time=2026-09-29T15:45:02.433Z level=INFO msg=write.done path=/private/var/folders/md/b0sr21q97jsg2gfn9lymdw3h0000gn/T/TestDurableCLIBinaryPersistsWriteReceiptAndDoesNotReplayAfterRes1493676179/002/work/durable-effect.txt bytes=11 duration_ms=3 trace_id="" span_id=""
time=2026-09-29T15:45:02.449Z level=INFO msg=llm_stream_start op=llm.stream provider=openai model=durable-cli-fixture messages=4
time=2026-09-29T15:45:02.453Z level=INFO msg=core.run_slot.released id=9dfaf1678ed0e5f2
time=2026-09-29T15:45:02.453Z level=INFO msg=cli_prompt_complete op=cli.prompt.complete result_len=23
time=2026-09-29T15:45:02.453Z level=INFO msg=command_end command="***REDACTED*** (bytes=6)" success=true duration_ms=86
```

For the interrupted run, the actual log contains `msg=write.done ... bytes=11`, then `msg=llm_stream_start ... messages=4` at the held response barrier; after restart it contains the same physical ID, `messages=5`, `core.loop.finish iterations=1 messages=6`, run-slot release, prompt completion, and `command_end ... success=true`. The test validates every physical row's source ID and schema version, unique run-terminal IDs, transcript ordering, and workspace-index source IDs/sequences. `go test -list '^(TestDurableCLIBinaryPersistsWriteReceiptAndDoesNotReplayAfterRestart)$' ./cmd/cli` listed the test; both normal and `-race` targeted runs passed with no skips.

An additional full-package `rtk go test -count=1 ./cmd/cli` run reported 54 passed and one pre-existing fixture incompatibility: `TestDefaultInteractiveTraceContainsNoPrompt` configures `session.store_path` as `.../sessions.jsonl`, so the durable CLI rejects it as a legacy file path. Running that test alone reproduces the same error; this package test was not changed in the subprocess-test-only slice. The focused subprocess test and its race run both pass.

This process test establishes exact restart and no replay after a synced successful receipt, including a kill while the next provider request is outstanding. It does **not** exercise an OS child process/tool that survives parent exit, quarantine of such a live orphan, or a kill before receipt durability; those crash windows remain unclaimed. It also does not close R2 as a whole.

## Remaining boundary

Durable session fork, ACP serve sessions, REPL session navigation mutations, and history-mutating `/compact`, `/clear`, and `/retry` remain explicitly unsupported. Durable compaction/fork semantics are not implemented. This CLI slice does not close the full R2 runtime plan; it wires the supported default prompt/REPL path to the existing prepared-session and run-owner seams.
