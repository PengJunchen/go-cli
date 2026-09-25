# R2 session RunCommitSink review

Reviewed the complete session adapter at `75ceb9c9dd013db414601210ed5c613b3f959c0f` (direct parent `6e34701c1576e32aeb4c6adaaaa48923bc269324`; source history includes the action/receipt schema at `63e9fd0`). The review covered all 1,511 lines of `internal/session/run_commit.go`, its 12 adapter tests, the durable writer and workspace-index contracts, and the approved R2 session/core-consumer handoffs. It does not claim that the Loop/Agent consumer, cleanup supervisor, restart crash matrix, or full R2 phase is closed.

## Findings

**P1 — terminal sync prevents the same run owner from recording late completion and resolution.** `CommitTerminal` sets `s.terminalID` only after the terminal suffix and checkpoint have been synced (`internal/session/run_commit.go:772-786`). Both `RecordExecution` and `ResolveExecution` call `rejectAfterTerminalLocked` before validating or persisting their payloads (`:357-365`, `:533-541`); that helper rejects every subsequent operation once `terminalID` is set (`:819-823`). This blocks the approved late-cleanup path: after `cleanup_pending`, the same run owner must be able to drain a completion signal that closes within its shared cleanup deadline, append the exact proof-bearing execution row, append its resolution checkpoint, and apply that resolution to the workspace index after terminal sync. The owner handoff explicitly allows late proof to clear quarantine without changing the existing receipt or terminal outcome (`r2-core-consumer-handoff.md:56,64-70`); it requires that late proof row and resolution (`:58,66`). The adapter must preserve the terminal fence for transcript, intent, receipt, and compaction writes, while narrowly allowing only the exact receipt-closed action's proof and resolution metadata through the retained run lease.

This was reproduced with an overlay-only regression using a real prepared JSONL session and workspace index. It synced the input, intent and dispatch marker, an identity-only execution row, receipt, and terminal checkpoint, then attempted the late proof and resolution on the same sink under one reused 250 ms context (the existing cleanup bound; no sleep or renewed deadline). The regression compiled and failed behaviorally: both methods returned `session: run already has a durable terminal checkpoint`, and the marker remained unresolved. This exercises the actual adapter under one shared bounded context, not the not-yet-integrated runOwner late-supervisor path. The overlay was not added to the repository.

**P2 — terminal commits and retries do not require the complete committed compaction projection set.** On a first terminal commit, the adapter validates and writes each projection supplied by `terminal.Projections`, but does not compare that slice against compaction rows for the same run that `CommitCompaction` already synced (`internal/session/run_commit.go:724-770`). On an idempotent terminal retry, `verifyTerminalProjectionsLocked` checks that every supplied projection exists, but an omitted projection makes the check vacuously pass (`:947-973`). The approved contract requires the terminal payload to carry all typed projections and verify earlier commits before acknowledging the final result (`r2-contract.md:68,91`; `r2-session-run-commit-adapter-handoff.md:39`).

Two real-session overlay probes both compiled and failed their behavioral assertions: (1) `CommitTerminal` accepted an empty projection list after projection `P` had been synced with `CommitCompaction`; (2) after a terminal commit containing `P`, a retry with the same terminal ID/transcript/outcome but an empty projection list also succeeded. The existing projection row remains durable in both cases; this is a terminal-payload consistency/idempotency defect, not evidence of data loss. Before acknowledging, compare the complete run projection set against the durable rows, including previously committed projections, and reject omissions or changed retries without rewriting those rows.

## Reviewed boundaries

- **Dispatch ordering:** `BeginIntentBatch` appends and syncs the assistant/intent rows before calling `WorkspaceQuarantineIndex.BeginDispatchBatch`; it returns nil only for `WorkspaceDispatchCommittedNow` (`run_commit.go:329-354`). The index contract returns that state only after append and sync under its lock; `AlreadyExists` is explicitly not a replay permit (`workspace_index.go:62-73,229-297`). Session/index errors therefore withhold the dispatch permit.
- **Writer evidence:** the sink uses the `DurableSessionWriter` from the same prepared handle (`run_commit.go:84-94`). The JSONL writer validates and sequences candidate rows, writes, flushes, syncs, then returns canonical rows with their physical IDs and sequence values (`durable_jsonl.go:310-320,405-410,461-501`). Resolution uses the row actually returned by that writer and requires a nonzero sequence before applying the exact checkpoint to the index (`run_commit.go:595-615`).
- **Transcript/receipt order:** input and receipt commits require the next contiguous transcript index; receipt lookup is against the durable assistant occurrence and, for reserved actions, its exact intent. Duplicate indices and physical-order holes fail closed (`run_commit.go:136-168,428-530,1269-1301,1456-1483`). Known denials are validated as unreserved not-dispatched receipts and are not physically appended with the intent batch (`:180-207,243-354`).
- **Compaction source:** the constructor captures a validated run-start `SessionView` with its source IDs. Projection cursors map through that immutable view or the current run's exact canonical transcript rows; boundaries must remain on the active branch and cannot split a tool-call group (`run_commit.go:64-83,846-945`).
- **Lease and handle ownership:** `CloseRunCommit` is idempotent, marks only this sink lease closed, and invokes the supplied release callback once; it does not close `PreparedSession` (`run_commit.go:97-111`). The maintained test confirms the prepared snapshot remains usable after lease release (`run_commit_test.go:52-105`).
- **Failure and replay behavior:** append/sync errors are returned; the JSONL writer poisons itself after write/flush/sync failure (`durable_jsonl.go:473-487`). Marker errors and `AlreadyExists` do not yield a permit. Resolution session sync precedes index apply; index failure returns an error and leaves the local checkpoint available for exact recovery, without replaying the action (`run_commit.go:595-615`). The terminal sync failure test asserts no acknowledgment and a poisoned writer (`run_commit_test.go:561-598`).
- **Typed resource state:** `ResourceState` is enum-validated when recording an observation, but local resolution selects a matching positive `CompletionProof`; it does not infer quiescence from `ResourceState` or `ExecutionIdentity` (`run_commit.go:372-403,1161-1176`; `durable_recovery.go:501-524`). Remote-terminal evidence remains explicitly unsupported.
- **Identity and branch checks:** methods bind calls to this sink, physical session/workspace, RunID, and owner generation (`run_commit.go:789-816`). Intent, execution, receipt, and resolution helpers correlate full action references and reject missing, duplicate, conflicting, wrong-owner, or cross-run matches (`:1012-1129`). Snapshot refresh rejects physical-session or active-branch changes (`:1396-1415`); compaction sources must be on the selected active branch. No additional actionable mismatch was found in these bounded paths.

## Validation

- Exact adapter test discovery was checked first with `rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -list '^TestSessionRunCommitSink' ./internal/session`; all 12 tests were listed.
- Reviewer target race passed at the reviewed commit:

  ```text
  rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 -run '^TestSessionRunCommitSink' ./internal/session
  ok github.com/pengjunchen/go-cli/internal/session 1.523s
  ```

- Both temporary regression sets used `go test -overlay` and exact `go test -list` discovery before execution. They failed on the described runtime assertions, not on build errors or unmatched test names.
- The implementer reported the complete affected session race suite passed in 6.466 seconds; the reviewer did not rerun the full suite. No source changes were made. The detached worktree remained clean at the reviewed hash; this report is intentionally unstaged.

## Follow-up review: bounded post-terminal cleanup and projection equality

Reviewed follow-up source commit `5a6dd764413bb5b1e236ec33e2ca662ef14cc348` (parent `75ceb9c9dd013db414601210ed5c613b3f959c0f`) in detached worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-session-adapter-review-5a6dd764`.

**Both findings above are resolved in this bounded follow-up; no new blocker was found.** The terminal remains a fence for ordinary input, intent, receipt, compaction, and changed-terminal writes. `RecordExecution` permits only a valid positive proof after terminal, and requires it to be dispatched and quiescent, correlated to the exact full intent/action reference and its earlier synced dispatched receipt. It also confirms the matching terminal checkpoint is on this run's durable branch (`internal/session/run_commit.go:360-448,819-830`). An identical late proof is idempotent; a conflicting proof for the same action fails. `ResolveExecution` after terminal permits only local-resource-stop evidence, requires that same synced dispatched receipt and terminal, resolves the proof reference from the durable row, then syncs the resolution checkpoint before applying that exact checkpoint identity/sequence to the index (`:566-674,1118-1205`). The added real-session test observes the marker as still present for wrong-action and cancelled-context calls, then observes `DispatchMarkerResolved` after late proof plus resolution, all under one 250 ms cleanup context. It verifies the same terminal outcome and transcript remain unchanged, the proof/resolution rows append once, and closing the lease rejects subsequent cleanup writes (`internal/session/run_commit_test.go:536-713`).

The new terminal projection validator compares the terminal's complete projection slice to the already durable projections for that run: count, physical order, stable projection ID, summary/content payload, and exact source boundary must match (`internal/session/run_commit.go:719-722,952-1002`). The contract for this slice is that `CommitCompaction` has already synced those projection rows; terminal commit no longer writes projections supplied only in the terminal payload. The regression persists two projections, proves an incomplete first terminal writes no bytes, accepts the exact ordered set, then rejects omitted, reordered, and changed-payload retries without duplicating or changing the terminal row (`internal/session/run_commit_test.go:715-821`).

Context and lease checks run before either late method changes state (`run_commit.go:789-817`). The maintained test rejects a cancelled context and proves the marker remains present. I also ran an overlay-only behavioral probe with a deadline already elapsed: both post-terminal `RecordExecution` and `ResolveExecution` returned `context.DeadlineExceeded`, appended no proof/resolution rows, and left the marker present. The probe source and overlay file were removed after the run. Sync/append failures continue to return without acknowledging success; the resolution path applies the index only after the durable writer returns the canonical synced checkpoint. The existing terminal-sync failure regression still confirms a failed sync poisons the durable writer. I found no path in this delta that replays the action or clears the index marker after a failed append/index operation.

## Follow-up validation

The exact session sink test discovery was checked first:

```text
rtk proxy go test -list '^TestSessionRunCommitSink' ./internal/session
```

It listed all 14 sink tests. Reviewer-run target race passed five repetitions of both new regressions:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=5 -run '^(TestSessionRunCommitSinkAllowsBoundedLateProofAfterTerminalWithoutChangingResult|TestSessionRunCommitSinkTerminalRequiresCompleteOrderedDurableProjections)$' ./internal/session
ok github.com/pengjunchen/go-cli/internal/session 1.730s
```

The overlay-only expired-deadline probe passed under race as well. The implementer reported that the two added regressions failed behaviorally before the fix and that the full affected session race passed in 5.564 seconds; the reviewer did not repeat that full suite. No source files were modified. The detached worktree remained clean at the fixed hash, and this report remains intentionally unstaged.

## Follow-up scope limits

This accepts only the bounded session-adapter repair. It does not validate that the production run owner invokes late proof/resolution callbacks correctly, nor does it close the broader R2 lifecycle, recovery matrix, or R2/R4 phase. The marker-resolution observation is from the real temporary workspace index exercised by the adapter regression, not a production multi-process or OS-sandbox claim.
