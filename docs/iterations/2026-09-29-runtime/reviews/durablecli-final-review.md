# Durable CLI final review

**Decision: ACCEPT** the bounded durable prompt/REPL wiring slice. No P1 or P2 findings remain in the reviewed slice.

## Exact candidate review

Reviewed clean worktree `codex/repair-20260929-durablecli` at `84f88f9d88b58f7f73f0e6390b783e7e7b406189`, based on `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`, including the full chain `9a4667282aedd887489e8047b4a9b5b9b10f9e5e`, `4de967cf3459a99eeed38f8ae69c231141f8dfd2`, `aeb96045f6c1963a9146f46e5593830740294809`, `bc75e4db7f2b547ab9caea62ebc653a77087749c`, `e393464cf4a3b12ef8ee48ee8961b893878772aa`, and `84f88f9d88b58f7f73f0e6390b783e7e7b406189`.

The manager pins one exact physical session and branch; `bindRun` lends its prepared handle and attaches the real sink/identity to the run context (`internal/cli/durable_cli_session.go:64-117,140-203`). The actual Harness/TurnRunner entrypoints capture and transfer that lease to the core run owner; the Assembly close owner waits for admission to become idle before closing the manager-owned session (`internal/core/harness.go:148-246`, `internal/core/turnrunner.go:86-223`, `internal/cli/assembly_lifecycle.go:502-513,600-606`). Prompt and REPL seed/refresh history from `PublishedView.Messages`; exact resume does not select “latest” (`internal/cli/assemble.go:386-393,429-433`, `internal/cli/repl_session.go:370-380,751-829`).

Durable assembly skips `JSONLSessionStore`; the REPL starts its pending writer and flush ticker only when that legacy store exists, and its durable turn path does not call `persistSession` (`internal/cli/assemble.go:1663-1675`, `internal/cli/repl_session.go:457-463,792-796`). Prompt output errors continue draining events and then read the run result before returning joined errors; the Assembly close result is joined by the deferred command close (`internal/cli/prompt.go:265-288`, `internal/cli/assembly_lifecycle.go:600-615`). A REPL sink-binding failure is typed, returned after the turn goroutine joins, and stops further input (`internal/cli/repl_session.go:731-749,887-905`; regression at `internal/cli/repl_durable_binding_test.go:19-58`).

The terminal gate is truthful: the run owner withholds durable success when terminal sync is unacknowledged, and the REPL accepts only a quiescent status with a known outcome (`internal/core/run_owner.go:832-857,1144-1145`, `internal/cli/repl_session.go:751-810`). `/clear`, `/compact`, and `/retry` are rejected before dispatch; durable session resume/fork/clone/switch is rejected before tree mutation, while read-only views remain available (`internal/cli/slash.go:239-250`, `internal/session/slash_commands.go:95-102`).

The independent selected `-race` run passed the durable prompt/REPL/serve, exact-resume, output-drain, typed binding, lease handoff, marker recovery, and workspace-index cases across `internal/cli`, `internal/session`, and `internal/core`. Additional verbose race checks passed `TestRunOwnerWithBoundSinkCannotAcknowledgeUnsyncedTerminal`, `TestRunOwnerMarksDurableTerminalSyncOnlyAfterExplicitSuccess`, Assembly pending/close-error propagation, prompt drain/join, and REPL binding failure. `TestDefaultInteractiveTraceContainsNoPrompt` passed after the fixture’s store path changed to a directory; the fixture diff is only that path change (`cmd/cli/main_test.go:835`). `rtk git diff --check 17ab7b7..HEAD` passed.

The actual-binary test passed with no skips:

```text
rtk proxy go test -v -count=1 ./cmd/cli -run '^TestDurableCLIBinaryPersistsWriteReceiptAndDoesNotReplayAfterRestart$'
```

It built `./cmd/cli`, then verified normal process exit plus exact physical-ID resume, with one managed write and two unique successful terminal checkpoints. Its second scenario killed the CLI process after the synced write receipt/resolution and while the next provider request was blocked; the killed run had no terminal, exact-ID restart did not replay the write, and the resumed run added one successful terminal.

## Integration addendum at Green SHA

Reviewed final root integration `25b88ba890f59be488e2ea5defbf09186baf873d` and the wiring commit `429c946d713bd51cad27fe1f10712214396b4c53`. The entrypoint guard remains first after flag parsing: default/explicit `--approve ask` returns a typed unsupported error before assembly, token write, or listener start (`internal/cli/serve_cmd.go:102-106`; tests at `internal/cli/serve_cmd_test.go:66-106`). Missing runtime remains a fail-start error, and assembly errors still close any returned Assembly without an echo fallback (`internal/cli/serve_cmd.go:140-178`). The durable-session guard follows the non-nil runtime check and precedes model resolution, Assembly, token generation/write, and listener start (`:147-155`).

Because the intentional `--approve ask` rejection precedes the durable guard, the durable serve regression now specifies `--approve deny`; only that argument changed, and the typed durable-unsupported and no-provider assertions remain (`internal/cli/durable_cli_wiring_test.go:417-431`; fixture-only commit `25b88ba`). Independent focused `-race` tests passed for durable serve rejection, all Ask-rejection subcases, missing runtime, assembly fail-start, and Assembly close propagation:

```text
rtk proxy go test -v -race -p 2 -count=1 ./internal/cli -run '^TestServe(ApproveAskRejectedBeforeTokenWriteOrListener|MissingRuntimeFailsBeforeTokenOrListener|AssemblyFailureDoesNotWriteTokenOrStartListener|AssemblyErrorClosesReturnedAssembly|RejectsConfiguredDurableSessionBeforeListenerStart)$'
```

No finding on this integration delta.

## Remaining boundaries

This accepts the CLI wiring slice only and does not close all R2 work. The subprocess test does not cover a separate OS child/tool that survives CLI-parent death or a crash before receipt durability. Quarantined safe-read admission also remains separate R2 work. The bound post-terminal compaction path remains assigned to the later R3 owner and is not a finding against this slice.
