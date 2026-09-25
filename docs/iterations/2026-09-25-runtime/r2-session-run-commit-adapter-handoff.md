# R2 session RunCommitSink adapter handoff

Status: source-based design proposal only. No product code is changed here. Baseline is `bf073e8`; implementation waits for parent approval and the completion-proof schema handoff.

## Construction and lifetime

Build one sink per run around the already prepared `session.PreparedSession` and one `*session.WorkspaceQuarantineIndex`. A candidate constructor is:

```go
func NewDurableRunCommitSink(
    prepared session.PreparedSession,
    index *session.WorkspaceQuarantineIndex,
    workspaceID string,
) (core.RunCommitSink, error)
```

The constructor calls `prepared.Snapshot()` once before the run's first `CommitInput`, validates the snapshot's physical session, active branch, active leaf, and `PublishedView`, then keeps the immutable run-start view/revision/source-ID mapping, durable rows, and cursors. It obtains `prepared.DurableWriter()` from that same handle. It never calls `PrepareResume` or opens a second session store. The snapshot is a defensive view of state; the prepared handle's lock and writer remain the authority.

Each sink method reads `core.RunCommitFromContext(ctx)` and `core.RunIDFromContext(ctx)`. It requires that the context names this sink, the same physical `SessionID` and canonical `WorkspaceID`, a non-empty owner-generated `OwnerGeneration`, and the same `RunID` as the payload. The core owner supplies the generation in the callback context; the CLI and sink do not invent it. A sink instance is run-scoped and serializes all writes, including a late `ResolveExecution` from cleanup. The prepared session stays open while its owner may still write a resolution.

Workspace identity is derived and canonicalized by the CLI from the selected workspace root. A fresh workspace is explicitly initialized with `EnsureWorkspace` before any action could be dispatched; the sink does not silently bootstrap a missing index. For resume, session recovery uses the same index as its `RecoveryAuthority` while holding the session lock. The lock order stays session file then workspace index. Session append/sync completes before calling an index method, and the index never calls back into session storage.

For new entries, set `RecordVersion=1`, physical `SessionID`, current `BranchID`, and `ParentID` from the active leaf; chain entries within a batch through their immediate predecessor. Use non-nil pointers for valid zero `TranscriptIndex` and assistant ordinal values. Let `AppendAndSync` assign `Seq` and `PayloadDigest`; after success refresh the active branch/leaf and max sequence with `Snapshot()` on the same prepared handle. Do not update or expose the live tree/history before append and sync succeed.

The adapter keeps entry IDs stable across retries by assigning an opaque ID before its first append and retaining it for an in-process retry. After reopen, it indexes the snapshot by semantic coordinates, compares the complete canonical payload, and reuses the exact existing row; mismatched or duplicate coordinates fail closed. Coordinates are `(RunID, TranscriptIndex)` for transcript rows, `(RunID, AssistantTranscriptOrdinal, ToolCallID)` for call occurrences, `ActionID` for a reserved invocation, and `TerminalCheckpointID` for the terminal checkpoint. Never deduplicate by message text or provider `ToolCallID` alone.

## Method-to-record mapping

| Sink method | Session record(s) and durable order | Workspace-index behavior |
|---|---|---|
| `CommitInput` | Append one `Type=user` row with `RunID`, `InputKind`, `TranscriptIndex`, and the full `AgentMessage` payload (`Content`, blocks, and usage). Repeated identical text at a later index is a new input. | None. Return only after session append, flush, and sync; the core must not start a provider request before this succeeds. |
| `BeginIntentBatch` | Validate the assistant tool-call message against all intents/known receipts. Append the assistant `Type=assistant` row first, then each approved `Type=tool, ActionState=intent` row, and any known pre-dispatch denial receipts. The assistant row is written once per `(RunID, AssistantTranscriptOrdinal)` even when sequential mode reserves later calls in separate one-action batches. Intents store `ActionID`, `ActionBatchID`, owner generation, occurrence coordinates, lowercase-hex `ArgumentDigest`/`SchemaDigest`, `ToolIdentity.Snapshot()`, and a defensive `EffectSet.Values()` copy; never raw args or credentials. | Only after the complete session batch sync, build one `WorkspaceDispatchBatch` containing all reserved actions and call `BeginDispatchBatch`. `WorkspaceDispatchCommittedNow` permits the core to proceed to the same prepared invocation's final fence. `WorkspaceDispatchAlreadyExists` is an error to the caller and never a replay permit. If there are no reserved intents (all calls were denied before reservation), persist the known receipts but do not create an empty dispatch marker. |
| `RecordExecution` | Append a journal-only `Type=tool, ActionState=execution` row with the exact intent's full occurrence reference, `ActionID`, `DispatchState`, optional `ExecutionIdentity`, and the approved action-scoped completion proof. It has no `TranscriptIndex`. This is observation evidence; it does not resolve quarantine. | None. |
| `RecordReceipt` | Append a terminal `Type=tool` row with the complete tool `AgentMessage`, original `ToolCallID`/`ToolName`, `RunID`, assistant ordinal, receipt `TranscriptIndex`, `ReceiptOutcome`, `DispatchState`, and reserved `ActionID` when present. A pre-reservation denial has no `ActionID` and is `not_dispatched`. Preserve provider IDs unchanged; repeated IDs at later assistant ordinals remain distinct occurrences. | None. A receipt never clears a marker. Persist receipts in canonical transcript order before the next provider request; receipt success is not a quiescence proof. |
| `ResolveExecution` | Append a `Type=checkpoint, CheckpointKind=execution_resolution` row with `ExecutionResolution`, plus the exact intent's complete reference fields (`WorkspaceID`, `RunID`, batch/action IDs, ordinal, call ID, owner generation). The `EvidenceRef` must identify the required durable proof row: exact `not_dispatched` receipt for `never_dispatched`, completion-bearing execution row for local quiescence, trusted terminal receipt for remote terminal, or reconciliation checkpoint for operator resolution. | After session checkpoint sync, call `ApplyDurableResolutions` with the exact `PendingActionRef`, checkpoint `EntryID`/`Seq`, and typed proof. On index failure return an error and leave the marker quarantined; reopen recovery retries only this same validated checkpoint, never the action. A no-marker no-op is allowed only for validated `never_dispatched` plus its matching durable receipt. |
| `CommitCompaction` | Validate the projection against the immutable run-start `PublishedView.Revision` and source IDs. Map its cursor to an exact `CompactedThroughEntryID`; reject stale revisions, invalid prefixes, unmappable boundaries, and cuts through an open assistant-call/receipt group. Append one `Type=compaction` row with the summary, source boundary, current active leaf parent, and run provenance. | None. Sync before the next provider request uses the projected history. Keep the original source entries. |
| `CommitTerminal` | Treat the final transcript slice position as its zero-based `TranscriptIndex`. Compare every already persisted row by `(RunID, TranscriptIndex)` and canonical payload; append only missing rows. Verify projections match those already committed (or persist missing valid projections first), then append one `Type=checkpoint, CheckpointKind=run_terminal` row whose `ID` is the caller's stable `TerminalCheckpointID` and whose `RunOutcome` is exact. The missing final rows and terminal checkpoint should be one `AppendAndSync` batch. | None. Return success only after final sync; any conflict or failure withholds the terminal/saved acknowledgment. |

For compaction, the run-start snapshot must be captured before the initial user row is appended. If `TranscriptCount > 0`, `PriorHistoryCount` must equal the original view length and the boundary maps through the run's canonical transcript index map. If `TranscriptCount == 0`, the boundary maps through the original view's `SourceEntryIDs`. The adapter must not compare message text to find the current input or compare a projection against the view revision after this run's own writes.

## Acknowledgment and retry rules

`BeginIntentBatch` is the only adapter operation that reaches the global dispatch index. The sequence is always:

```text
session assistant + intent/denial rows: AppendAndSync
workspace BeginDispatchBatch: append + Sync
core final prepared-invocation fence
executor
session receipt: AppendAndSync
next provider request
session resolution checkpoint: AppendAndSync
workspace ApplyDurableResolutions: append + Sync
```

The index marker is batch-level and may mean every reserved call could have started. If the index call errors after a possibly visible write, the adapter returns an error and the core does not dispatch. A retry that observes `AlreadyExists` also returns an error; recovery will classify the marker as possibly dispatched and will not replay. Calls rejected before reservation keep no `ActionID`; reserved calls fenced by the final prepared-invocation check keep their original `ActionID` in a durable `not_dispatched` receipt.

Session `AppendAndSync` errors poison the handle. Do not proceed to an index marker, executor, provider request, or durable acknowledgment after such an error. A receipt sync failure prevents the next provider call. A resolution index failure leaves the already-synced local resolution checkpoint available for exact recovery replay. Unknown remote outcomes and local transport completion do not clear the workspace marker; only a recognized remote terminal proof, the exact local completion proof, never-dispatched proof, or explicit durable reconciliation can do so.

## Test boundary and fault injection

Adapter tests belong in `internal/session/run_commit_test.go`; the adapter implementation belongs in `internal/session/run_commit.go`. Use a prepared test handle backed by the real durable JSONL store and a temporary `WorkspaceQuarantineIndex` with its existing per-instance write/sync hooks. Keep core/Loop/CLI production edits in their owning consumer lane.

Required adapter regressions:

- Bound context, session, workspace, run, and owner-generation mismatch fails before any session or index write; an absent sink/binding fails before provider entry in the later core integration test.
- Session intent sync failure results in zero index calls; marker success is ordered after session sync. `AlreadyExists` and index write/sync failure produce no dispatch permit and no executor call in the later core integration test.
- Receipt sync failure prevents another provider request. A visible marker after index sync error recovers as possible-dispatch/unknown, with zero action replay.
- Resolution session-sync failure produces zero index calls. If session resolution sync succeeds but index sync fails, the local checkpoint remains, quarantine remains, and reopening applies exactly that checkpoint once before view publication.
- Exact retries are idempotent; conflicting payload at the same transcript/action/terminal coordinate fails closed. Repeated `ToolCallID` at different assistant ordinals does not collide.
- Stale compaction revision, invalid source cursor, and a boundary splitting an assistant-call group write no bytes. Successful restore inserts one untrusted summary at the exact source boundary and retains the uncovered suffix.
- Terminal transcript mismatch, terminal writer/sync failure, or missing stable terminal checkpoint ID never acknowledges a saved run. Retrying the same `TerminalCheckpointID` and payload does not duplicate rows.
- Real cross-session index contention exercises the workspace lock; no test substitutes a process-kill/restart matrix for R2-REV-01.

## Blocking source gaps and ownership

1. There are no production `RunCommitSink` call sites in core at `bf073e8`; the Loop/Agent consumer and CLI binding remain separate work. The current branch has context API definitions, but `runOwner` still invokes its callback with only the claim ID/worker tracker at this baseline. `build_network` reports that the owner lane will attach its generated `OwnerGeneration` and claim `RunID`; the adapter depends on that integration and will consume `RunCommitFromContext` without changing the binding API.
2. The tools lane reports the approved completion-proof API: `PreparedCompletionProof` values `inline_pipeline_joined`, `queued_job_joined`, and `owned_process_group_joined`, exposed by `PreparedInvocation.CompletionSignal()` only after its `Done()` channel closes. This API is not yet present in the `bf073e8` source snapshot. The session row schema and `ActionExecution` currently have no completion-proof field, and `validateExecutionResolutionEvidence` still requires a non-nil `ExecutionIdentity` for local quiescence. Per `r2-local-quiescence-proof.md`, BLD-01 must persist/link the closed proof and validate it; `ExecutionIdentity` remains optional diagnostics and is not stopped evidence. Do not implement `ResolveExecution` until this schema/validator handoff lands.
3. There is a reproducible reopen path where a duplicate retry acknowledges without a new file `Sync()`. Concrete call chain: `durableJSONLStore.AppendAndSync` serializes/writes/flushes a complete LF-terminated row, then its injected `syncFile()` fails and poisons the handle → caller closes that handle → `DurableJSONLSessionPreparer.PrepareResume` locks the same file and calls `readDurableJSONL` → the valid complete-LF branch decodes the row and performs no repair or `file.Sync()` → `PrepareResume` returns the writer → retrying the identical entry finds its existing ID, leaves `newRows` empty, and returns from `AppendAndSync` before `syncFile()`. A temporary focused probe exercised this exact sequence with an injected failing first sync and a counting sync hook on the reopened writer; `PrepareResume` plus the acknowledged exact retry made **zero** calls to the reopened hook. Command: `rtk env GOMAXPROCS=2 go test -p 2 ./internal/session -run '^TestProbeReopenExactRetrySkipsFileSync$' -count=1 -v` (exit 0). The probe file was removed after the run and is not part of this design-only commit. A valid unterminated final row takes a different recovery branch that writes and syncs the delimiter; it does not address the complete-LF repro. BLD-01 must ensure an exact-duplicate retry confirms file durability before success and keep the permanent sync-failure/reopen regression. The adapter cannot safely paper over this with a synthetic row.
4. `RunIntentBatch` carries assistant ordinal but not assistant `TranscriptIndex`; `RunToolReceipt` does carry its index and `RunTerminal.Transcript` uses vector position. The sink can assign the assistant index from its per-run canonical transcript cursor, but this requires the Loop consumer to invoke sink methods in canonical order and to keep sequential reservations for one assistant group idempotent. Add an integration regression for this ordering; do not infer indexes from message text or provider call IDs.
5. Current `WorkspaceQuarantineIndex.BeginDispatchBatch` conservatively blocks all actions while an unresolved marker exists because it has no enforced-scope permit. Preserve that behavior until the reviewed R4 path-scope capability is wired through; this adapter must not trust the `EffectSet` DTO as a read authorization.

Once the completion-proof and duplicate-sync gaps are closed, implement only the session adapter file/tests first. Core's prepared invocation, run owner, actual Loop dispatch, and CLI input/terminal acknowledgment remain consumer-lane responsibilities.
