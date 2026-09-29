# Durable CLI live-orphan crash matrix stage-1 review

**Decision: REJECT** acceptance of test-only crash matrix commit `a7a5e03396244964052d9c00651230db09f58d3e`, based on `7c078ffde1b2acfd9cc88e984f99d5fff8db0e76`. One P1 finding leaves the no-replay acceptance criterion unproven.

## Evidence reviewed

The focused actual-binary test passed on Darwin:

```text
rtk proxy go test -v -count=1 ./cmd/cli -run '^TestDurableCLIBinaryQuarantinesLiveOrphanedBashAndNeverReplays$'
```

It built `./cmd/cli` successfully and completed in 32.506s. `rtk go vet ./cmd/cli` passed. The test starts that built binary from an isolated working directory, configures its real builtin `bash`, and waits until the physical session JSONL contains the Bash intent and the workspace index contains its `dispatch_ready` record. The run-commit path syncs the intent rows before `BeginDispatchBatch`, and the workspace index append syncs the dispatch marker before granting dispatch.

The Bash command writes a fixture nonce to its start marker, waits behind an effect gate, then writes the visible effect and updates a nonce-tagged heartbeat while waiting for release. Before killing the parent, the test sees one dispatch, no resolution, no receipt, no terminal, and no effect before the gate. It calls `Kill` on the owned CLI `exec.Cmd.Process`; the observed exit is `signal: killed` / `-1`. The heartbeat advances after that exit, and the physical session still has no receipt or terminal. This establishes that the managed Bash command remains alive after the CLI parent dies without a PID lookup or process-group kill.

Exact-ID resume reopens the same physical session, creates one `unknown`/`dispatched` receipt, completes one terminal, and leaves the dispatch/resolution counts unchanged. A distinct new physical session in the same captured workspace attempts a write; the CLI exits 1 with the quarantine error, no effect file, no receipt, no terminal, and no dispatch marker for that action. Reopening that exact second session creates one `interrupted`/`not_dispatched` receipt. The fixture releases the live Bash with its nonce-bound file and observes the matching exit proof; cleanup also releases and waits for the owned child without signaling a guessed PID.

## Findings

### P1 — The effect assertion cannot detect a replay

`cmd/cli/durable_cli_subprocess_e2e_test.go:319` writes the same nonce to `effectPath` with `>` (truncate/overwrite). The post-kill, post-resume, and final assertions at lines 208, 216, 249, and 275 only compare that file to the same nonce. A second execution of the Bash command would overwrite identical bytes and pass every one of those checks. `startedPath` and `exitPath` are also overwritten with the same nonce, while the single shared heartbeat does not identify how many command instances wrote it. The test therefore proves the original child is alive and that the marker is unchanged, but it does not prove the external effect ran exactly once or that no replay occurred.

Minimal repair: make the fixture append one unique invocation record to an append-only start ledger and one unique effect record to an append-only effect ledger. Sync each ledger write and reopen/read back the counts before the parent kill and after each exact reopen; assert exactly one start and one effect throughout. Keep the nonce in each record so the evidence remains bound to this fixture execution. Then rerun the focused test and vet.

### P2 — The recorded diff-check result is not reproducible

`rtk proxy git diff --check 7c078ffde1b2acfd9cc88e984f99d5fff8db0e76 a7a5e03396244964052d9c00651230db09f58d3e` reports trailing whitespace on metadata lines 3–5 of `docs/iterations/2026-09-29-runtime/reports/durable-crash-matrix-builder.md`. The builder report says `git diff --check` passed. Remove the trailing spaces or correct the reported validation result.

## Boundary

This SHA adds a crash/mutation test and its builder report; it does not integrate the separate safe-read proof/admission design. This review does not claim safe-read behavior is implemented or validated.
