# R5 keyed canonical transcript projection design

Status: source-grounded design proposal for independent review. This document adds no runtime behavior and does not claim R5 or R8 implementation acceptance. Source inspection is pinned to main `6501d1f97984551018a84084d13e1cb3843a26d4` and the reviewed R5/R8 handoffs listed below.

## Decision and boundary

Build one value-only canonical transcript DTO in `internal/tui` and one pure CLI adapter in `internal/cli`. The adapter consumes a coherent `session.PreparedSessionSnapshot`; it does not read files, mutate `Agent` history, construct IDs, or infer transcript state from events. `PreparedSession.Snapshot()` on the retained handle is the only refresh input. A successful projection is submitted as a **whole snapshot replacement** to the TUI canonical lane. Provisional events remain a separate per-invocation overlay and are never merged into durable rows by text, timestamp, event order, tool name, or `ToolCallID` alone.

The DTO carries only the selected physical session, active branch, actual durable entry IDs, canonical `core.AgentMessage` copies, and the narrow typed facts needed to render compaction and action state. It is not a second session format, journal writer, model-history reconstruction path, or tool-dispatch authority. R8 may preserve viewport anchors using the durable key, but this proposal adds no paging, search, or viewport behavior.

## Source facts

- [`internal/session/durable_contract.go`](../../../../internal/session/durable_contract.go) defines `PreparedSessionSnapshot` as a coherent copy of the complete durable state on one prepared handle. `DurableEntries` preserve physical append order and include journal/checkpoint/hidden/branch rows. `PublishedView` is the complete model-history view and may lag the journal while a tool-call group is open. `SessionView.Validate()` requires nonempty physical session and branch IDs, equal `Messages` / `SourceEntryIDs` lengths, a revision for nonempty views, and unique nonempty source IDs. `SessionView.Clone()` deep-copies canonical messages and source IDs.
- [`internal/session/derive.go`](../../../../internal/session/derive.go) creates `PublishedView` by projecting the already selected branch and appending each visible message with its actual source `EntryID`. `EntryTypeCompaction` projects as a `Role="user"` message with an untrusted-summary prefix, so UI row type must come from the exact durable source row, not from `AgentMessage.Role`.
- [`internal/session/types.go`](../../../../internal/session/types.go) provides the durable source type and exact facts: `ID`, `Type`, `Seq`, `SessionID`, `BranchID`, `RunID`, `TranscriptIndex`, `AssistantTranscriptOrdinal`, `ToolCallID`, `ToolName`, `ActionBatchID`, `ActionID`, action/dispatch/resource/completion fields, receipt outcome, and compaction boundary/summary. Journal and checkpoint entries are separate durable records from transcript messages.
- [`internal/core/types.go`](../../../../internal/core/types.go) shows that `AgentEvent` has no `RunID`, assistant ordinal, or `EntryID`. A tool-output event has only a standalone `ToolCallID`; canonical `core.AgentMessage` contains nested content blocks, usage, tool calls, and JSON args and must be defensively copied.
- [`internal/core/eventstream.go`](../../../../internal/core/eventstream.go) implements `RunStatusSource` on `EventStreamImpl`, exposes the live status handle and event gap count. [`internal/core/turnrunner.go`](../../../../internal/core/turnrunner.go) attaches the exact run owner to that stream before entering the run callback that emits events. The REPL creates a fresh stream for each `executeTurn` and passes that same stream to `RunTurn` and the TUI bridge. The TUI may bind that stream's whole overlay to the exact `RunID` from its status handle or returned `Result`; it must not copy that identity into individual events or invent missing occurrence coordinates.
- [`internal/core/run_status.go`](../../../../internal/core/run_status.go) separates logical `Outcome` from resource `ExecutionState`. A `cleanup_pending` or `quarantined` observation is not a completed run. EventStream closure and `RunTurn` return alone are not quiescence.

## Frozen-base source anchors and binding race

These line references are for the source snapshot named in the header:

- internal/session/durable_contract.go:18-61,64-99 — positional view validation/deep clone and coherent snapshot contract; DurableEntries may be newer than PublishedView.
- internal/session/derive.go:32-70,354-384 — selected-branch projection records actual source IDs; typed compaction is translated to a user-role model summary.
- internal/session/types.go:55-159 — immutable durable entry identity, occurrence coordinates, action facts, compaction fields, and run-terminal checkpoint.
- internal/core/types.go:65-99,119-133 — event payload has no run/ordinal/entry ID, while Result carries RunID and a live status handle.
- internal/core/eventstream.go:41-61,271-283 — default stream implements the optional RunStatusSource and stores the handle.
- internal/core/turnrunner.go:203-225 — RunTurn installs the status handle before entering the event-producing callback. The REPL separately launches RunTurn and starts the bridge (internal/cli/repl_session.go:693-743), so the bridge may start before the handle is visible. Resolve this race by checking the same stream's status source when handling each event and again at run return; events emitted by an accepted RunTurn occur after handle installation. If no handle/result identity exists because the entrypoint failed before owner creation, keep the generation unbound.
- internal/core/run_status.go:3-43 — outcome, execution state, gap count, and the retained status handle are distinct values.
- The remote ACP stream remains outside this local prepared-session binding unless it receives the same reviewed snapshot, owner-status, and terminal-sync contracts; do not imply its events are covered here.

## Proposed value API

Keep the DTO in `internal/tui` so TUI remains independent of `internal/session`. It may import `internal/core` for the canonical message and run-state value types. Prefer closed local enums for row kind and journal fact state; never serialize these view values as session entries.

```go
// internal/tui/transcript_projection.go

type DurableTranscriptKey struct {
    PhysicalSessionID string
    BranchID          string // the selected view branch, not necessarily the source row's owning branch
    EntryID           string // exact PublishedView.SourceEntryIDs[i]
}

type TranscriptRowKind uint8 // user, assistant, tool, system, compaction

const (
    TranscriptRowUser TranscriptRowKind = iota + 1
    TranscriptRowAssistant
    TranscriptRowTool
    TranscriptRowSystem
    TranscriptRowCompaction
)

type CanonicalTranscriptRow struct {
    Key        DurableTranscriptKey
    Kind       TranscriptRowKind  // mapped from the exact durable source EntryType
    Message    core.AgentMessage  // defensive copy of PublishedView.Messages[i]
    SourceSeq  uint64             // exact durable source sequence; zero is allowed for legacy rows
    ActionID   string             // copied only from this exact durable source row
    Compaction *CompactionFact    // non-nil only for a source EntryTypeCompaction
    Occurrence *ToolOccurrenceKey // set for a tool receipt only when its source has the full exact key
    Calls      []ToolOccurrence   // assistant calls with exact source occurrence coordinates
}

type CompactionFact struct {
    CompactedThroughEntryID string // actual durable boundary; empty only for supported legacy rows
    Summary                 string // exact durable summary/content fact, separate from Message.Role
}

type ToolOccurrenceKey struct {
    RunID                      string
    AssistantTranscriptOrdinal uint64
    ToolCallID                 string
}

type ActionJournalStage uint8

const (
    ActionJournalIntent ActionJournalStage = iota + 1
    ActionJournalExecution
)

type ActionJournalFact struct {
    EntryID         string // actual DurableEntries[i].ID
    ActionBatchID   string
    ActionID        string // empty where the durable record has no reservation
    Stage           ActionJournalStage
    DispatchState   core.DispatchState
    ResourceState   core.RunExecutionState
    CompletionProof string // exact stored label; UI display only, never an execution authority
}

type ToolOccurrence struct {
    Key          ToolOccurrenceKey
    ToolName     string
    JournalFacts []ActionJournalFact // each fact retains its actual EntryID
}

type CanonicalTranscriptSnapshot struct {
    PhysicalSessionID string
    BranchID          string
    Revision          string
    MaxSeq            uint64
    ActiveLeafEntryID string
    Rows              []CanonicalTranscriptRow // exactly PublishedView order
}

type RunObservation struct {
    RunID          string // copied from the exact status handle / Result
    Outcome        core.RunOutcome
    ExecutionState core.RunExecutionState
    EventGapCount  uint64
    // TerminalEntryID and TerminalOutcome report only a matching durable row
    // observed in this snapshot and selected branch. They do not represent
    // owner acknowledgement or a saved-success decision.
    TerminalEntryID string
    TerminalOutcome core.RunOutcome
}
```

`CanonicalTranscriptSnapshot.Clone()` must clone `Rows`, `Calls`, `JournalFacts`, compaction values, and nested `Message` payloads using `core.CloneAgentMessages`; callers must not retain aliases to prepared session data. `Validate()` checks nonempty physical session/branch identity, unique nonempty durable keys, valid enum combinations, and kind/role consistency (`user→user`, `assistant→assistant`, `tool→tool`, `system→system`, `compaction→user`). A compaction row must carry compaction facts; non-compaction rows must not. It does not mint IDs or bless malformed session input.

The CLI adapter is a pure transformation with a narrow signature:

```go
// internal/cli/transcript_projection.go
func ProjectPreparedTranscript(ctx context.Context, snapshot session.PreparedSessionSnapshot) (tui.CanonicalTranscriptSnapshot, error)
func ProjectRunObservation(ctx context.Context, snapshot session.PreparedSessionSnapshot, handle core.RunStatusHandle, resultRunID string) (tui.RunObservation, error)
```

`ProjectRunObservation` receives the same snapshot used for the transcript projection, the real owner status handle, and the returned Result.RunID when available. It uses BuildFromSnapshot and the active-leaf path only to verify that the matching terminal checkpoint belongs to the selected branch. It requires a nonempty handle RunID; a supplied result ID must match it. The adapter copies outcome/state/gap count exactly. It finds a terminal fact only by a durable EntryTypeCheckpoint whose CheckpointKind is CheckpointRunTerminal and whose RunID exactly matches the handle. A matching durable checkpoint on the selected branch's active-leaf parent chain means only that this snapshot observed that row; it is not an owner acknowledgement and cannot override the status handle. No timestamp, sibling-branch, or latest-row fallback is allowed. No terminal row means terminal persistence is not established. The value API has no `Saved` or acknowledgement field and must not infer one.

Because the projection functions accept a snapshot value, they cannot establish that an arbitrary supplied value is fresh. The refresh caller must obtain it with `Snapshot()` from the retained prepared handle immediately before projection. If that call fails (including after the writer is poisoned), return the error and do not invoke the projector with a cached earlier snapshot as a substitute. The prior displayed rows may remain visible as last-known UI state, but they are not a new projection or terminal evidence.

Keep R2/session types out of TUI. The adapter may use the snapshot's validated active branch and exact source rows; it must not reopen a session file or use `BuildFromStore`. If the caller needs the navigation tree, it separately calls `BuildFromSnapshot` with that same snapshot.

## Projection algorithm and fail-closed rules

1. Clone the supplied snapshot before projection. Require nonempty snapshot.SessionID and snapshot.ActiveBranchID. Require PublishedView.Validate() and exact equality of PublishedView.SessionID == snapshot.SessionID and PublishedView.BranchID == snapshot.ActiveBranchID.
2. Call BuildFromSnapshot(ctx, snapshot) and obtain the ordered active-leaf path with GetBranch(snapshot.ActiveLeafEntryID). For an empty snapshot, require an empty published view and use an empty path. Use this path only to validate branch membership for source/journal/checkpoint rows; never derive message content/order from the tree. Build indexes by actual SessionEntry.ID and reject empty/duplicate IDs in both the complete snapshot and selected path. Preserve durable append order; do not sort it to create transcript rows.
3. Iterate `PublishedView.Messages` once in its given order. At index `i`, obtain the only source from `PublishedView.SourceEntryIDs[i]`, then resolve that exact ID in the durable index and require it to be a member of the validated active-leaf path. A missing/duplicate/mismatched source is a projection error; do not fall back to parent traversal, `TranscriptIndex`, timestamp, message text, or physical row order. Before emitting a row, validate that the exact source `EntryType`, projected row kind, and canonical message role agree: user/user, assistant/assistant, tool/tool, system/system, and compaction/user. For a tool receipt, the message's `ToolCallID`, `ToolName`, and `ReceiptOutcome` must exactly match the source row; a mismatch rejects the entire projection. Do not synthesize or repair any of these facts.
4. Construct `DurableTranscriptKey{PhysicalSessionID: snapshot.SessionID, BranchID: snapshot.ActiveBranchID, EntryID: sourceID}`. The branch component identifies the selected projection. A shared immutable source row can retain its original owning `SessionEntry.BranchID`; copy that only as optional provenance if needed, never substitute it for the selected view branch key.
5. Deep-copy the canonical message at the same index. Determine row kind from the exact source `EntryType`; for compaction keep `KindCompaction` even though the provider projection's message role is `user`. Carry the durable compaction boundary and summary as typed facts, and do not render a compaction marker as a newly submitted user turn. When an assistant row carries tool calls, require the canonical call IDs and names to agree with the exact source assistant entry before using either to form occurrence metadata.
6. For an assistant row, derive a ToolOccurrenceKey only when the exact source entry supplies nonempty RunID, a non-nil AssistantTranscriptOrdinal, and the corresponding canonical tool call has a nonempty ID/name. Associate journal rows only when their actual durable facts validate the exact (physical SessionID, RunID, AssistantTranscriptOrdinal, ToolCallID) tuple and matching tool name, and the journal EntryID lies on the active branch path produced by BuildFromSnapshot. SessionEntry.BranchID alone is not branch membership because shared-prefix source entries can retain their source-branch ID. Keep each joined journal row's actual EntryID, ActionBatchID, and persisted ActionID. If the key is incomplete or ambiguous, preserve the canonical message but omit occurrence metadata. A duplicate ToolCallID in another assistant group is valid and remains a different occurrence; ToolCallID by itself never joins anything.
7. Journal intent/execution/checkpoint rows never become transcript rows. A journal occurrence whose assistant source is not in `PublishedView` remains unpublished metadata and is not used to synthesize an open assistant/tool row. Thus a newer durable open group can coexist with the previous complete `PublishedView` without exposing a half-group as canonical.
8. Any identity contradiction, duplicate source, or attempted ambiguous join returns an error and leaves the currently displayed projection intact. No partially projected replacement is published.

## Provisional overlay and termination

Each `executeTurn` owns one EventStream and one provisional overlay generation. Events are appended only to that generation. Before the stream has a status handle, the UI may use a private process-local overlay key for rendering; it is an anchor only, not a RunID or durable identity. Once `RunStatusSource.RunStatusHandle()` or the corresponding `Result.RunID` is available, bind the **whole overlay generation** to that exact run. If both sources exist and disagree, keep it uncorrelated/incomplete and reject canonical reconciliation for that generation. Never add this RunID to event payloads or create per-event ordinal, EntryID, or ActionID.

The current `AgentEvent` shape cannot distinguish repeated `ToolCallID` values across assistant groups, so a provisional `tool_output` remains an uncorrelated event inside its owning run overlay. It is not attached to a specific canonical tool occurrence until a future exact mapping API exists. This design deliberately avoids an R5 per-action event-identity protocol.

A whole-run overlay has these states: `live`, `owner_pending`, `closed_unreconciled`, `reconciled`, or `unbound_incomplete`.

- The event stream closing or `RunTurn` returning does not end ownership. While the exact status handle says `cleanup_pending`, `draining`, or `quarantined`, keep the run status and overlay visibly pending; do not claim saved success or clear the run overlay. Continue observing the same handle under R1's bounded lifecycle contract.
- When the owner reports `quiescent`, stop accepting live events for that run and take one new `Snapshot()` from the same retained `PreparedSession` handle. Project the entire new canonical view before sending one replacement update. The replacement swaps the full canonical row slice; it never appends a “final” message alongside prior provisional text. If `Snapshot()` fails (including a poisoned writer), return the error and do not use a cached earlier snapshot as the new projection or as terminal evidence; retain the current display as last-known and keep the ended event generation unsaved/unknown.
- If a valid fresh snapshot contains a `run_terminal` row with the exact run ID on the selected branch, replace the canonical lane. The row establishes only that this snapshot observed the durable fact. Whether the whole provisional generation can retire or display saved status remains bound to the exact owner observation below; a row cannot override an owner sync/ack failure.
- Preserve the run's `Outcome`, `ExecutionState`, and `EventGapCount` as separate status facts. An error/cancelled/unknown run may still refresh canonical facts, but it cannot show “complete · saved”.
- Show “complete · saved” only when the same exact run has `Outcome=success`, `ExecutionState=quiescent`, `EventGapCount=0`, and the same-handle fresh snapshot contains its actual terminal checkpoint with `RunOutcome=success` on the selected active-leaf path. The current core owner sets the result unknown when terminal sync is required but the consumer did not acknowledge `CommitTerminal`; therefore a visible row never substitutes for owner acknowledgement. A checkpoint for another run, a stale snapshot, a successful provider response, a clean event close, or nonzero event gap count is insufficient.
- Overlay retirement and the saved badge are separate predicates. A later UI reducer may retire a whole-run overlay for a known success, error, or cancelled outcome only when execution is quiescent and a matching terminal outcome is present in the same fresh selected-path snapshot; it must retain the error/cancelled status and never clear a nonzero event-gap warning. Unknown outcome or any owner/checkpoint mismatch prevents retirement. The saved badge has the stricter success plus zero-gap predicate above. This DTO slice only copies those facts and implements neither retirement nor acknowledgement reduction.
- If a fresh snapshot succeeds but the exact run checkpoint is absent, refresh any valid durable canonical projection from that same snapshot, but retain the ended event generation as `closed_unreconciled` with its original run outcome/error and an explicit unsaved/unknown label. If snapshot acquisition itself fails, do not reuse stale snapshot data. Do not delete an overlay by comparing text. If there is no exact RunID, keep it `unbound_incomplete`; a session refresh cannot claim to have reconciled it.
- Never reset a nonzero `EventGapCount` to zero because a snapshot succeeded. Canonical transcript replacement can repair the durable message view, but this EventStream count also covers lost noncanonical progress/output events and has no per-event reconciliation watermark. Keep the gap warning with the run observation until a future explicit source contract can prove the gap's full scope is reconciled. Unknown outcome remains unknown even if the snapshot is readable.

The app applies each validated `CanonicalTranscriptSnapshot` as an atomic whole-value replacement. A replacement from a different physical session or branch must first reset the old canonical lane and its run overlays; it cannot merge rows across identities. For refreshes on the same handle, the REPL owner serializes fresh `Snapshot()` acquisition, projection, and update so an older asynchronous result cannot overwrite a newer one. A failed/poisoned `Snapshot()` returns before projection and cannot be replaced with a cached snapshot; tests for this retained-handle error path belong to the later app/lifecycle integration slice. Preserve a viewport anchor only by the durable key if it still exists; viewport policy belongs to R8.

## TDD and file ownership

**This design slice is independent and can be implemented without changing CLI lifecycle or Bubble Tea app state.** It produces a projection value and tests; R2/R5 default wiring remains a later gate.

- `internal/tui/transcript_projection.go` — value DTO, closed row-kind enum, clone/validate methods only. No `session` imports and no storage access.
- `internal/cli/transcript_projection.go` — pure snapshot-to-DTO adapter and exact run-observation projection. It does not access `REPLSession`, Assembly, `AgentImpl`, EventStream mutation, or durable writers.
- `internal/cli/transcript_projection_test.go` — projection tests using a real `PreparedSession`/snapshot plus deliberately malformed value snapshots for fail-closed paths.
- internal/tui/transcript_projection_test.go — DTO deep-copy and validation only; no live Bridge/app wiring or replacement reducer in this independent slice.
- R5 `repl_session.go`, `assemble.go`, `bridge.go`, `app.go`, and `accordion.go` integration is owned by the later R5 lifecycle/app builder after R2 durable CLI wiring and this DTO are reviewed. `internal/session` and `internal/core` stay unchanged in this slice. R8 owns viewport behavior.

### Independent DTO/adapter TDD slice

These tests belong to the first implementation checkpoint and do not require changing BubbleteaApp, BridgeEvents, the REPL, Assembly, or the run lifecycle:

1. TestTranscriptProjectionUsesPublishedSourceIDs — exact positional mapping to physical session/selected branch/actual EntryID; duplicate/missing source IDs and mismatched view identity reject the entire projection.
2. TestTranscriptProjectionKeepsCompactionTyped — compaction source remains a typed row although the canonical provider message role is user; marker is not exposed as a new prompt.
3. TestTranscriptProjectionRejectsSourceMessageMismatch — reject a user source paired with an assistant role and a tool receipt whose canonical `ToolCallID`, `ToolName`, or `ReceiptOutcome` differs from the exact durable source; accept a compaction row with user role and compaction kind.
4. TestTranscriptProjectionJoinsActionFactsByExactOccurrenceAndEntryID — join exact journal facts to the source assistant call and receipt; repeated call_1 in a later run/ordinal remains separate; sibling-branch, incomplete-key, and nonmatching journal facts never join; the DTO exposes actual ActionID/EntryID only when stored.
5. TestTranscriptOpenJournalDoesNotReplacePublishedView — real prepared snapshot with a newer open intent/execution group renders exactly the older PublishedView rows and source IDs, with no synthetic assistant/tool transcript row.
6. TestTranscriptProjectionDefensiveCopyNestedToolArgs — mutate DTO ToolCalls args/content blocks/usage after projection and prove the snapshot/view source is unchanged; mutate a clone and prove the DTO stays unchanged.
7. TestTranscriptRunObservationRequiresExactTerminalCheckpoint — status handle RunID/outcome/state/gap are copied; exact same-snapshot terminal checkpoint is reported only as an observed fact; wrong-run or missing terminal rows remain absent.
8. TestTranscriptRunObservationKeepsOwnerFailureDespiteTerminalRow — with a real prepared-session terminal row and owner status `Outcome=unknown` (or nonzero gap), the projection reports those exact independent facts; it does not synthesize acknowledgement or saved state from the row.
9. TestTranscriptProjectionDoesNotCachePriorSnapshot — after a valid projection, a malformed next snapshot returns an error rather than reusing previously projected rows. The caller's fresh-snapshot path separately must short-circuit on `PreparedSession.Snapshot()` error rather than reusing cached input.

Before running named tests, use go test -list to verify they exist. These tests validate only the standalone DTO and projection adapter using real prepared snapshots plus malformed fixtures. They do not claim that they exercise the Bubble Tea update path, default REPL, subprocess/PTY, startup hydration, or R2 production wiring.

### Follow-up app/lifecycle integration acceptance

After R2 durable CLI wiring and DTO review, the separate R5 app owner should add:

- TestTranscriptWholeReplacementIsAtomicAndIdentityScoped — a complete validated replacement never appends duplicates or exposes partial rows; physical session/branch change resets rather than combines the old identity.
- TestTranscriptProvisionalLifecyclePreservesPendingAndGap — exact stream/status association binds the whole run overlay, repeated ToolCallID events stay uncorrelated per call, cleanup_pending does not retire it, quiescent plus exact terminal snapshot retires the entire run overlay, no-terminal keeps a closed unsaved overlay, unknown/error never yields saved success, and nonzero gap stays visible.
- TestTurnDoneDoesNotDuplicateAssistantText and the real Bridge→App.Update/View tests from the R5 handoff, followed later by isolated default-CLI PTY/restart acceptance.

The app/lifecycle follow-up owns repl_session.go, bridge.go, app.go, and accordion.go; none are changed by the independent DTO/adapter implementation. A test-only overlay reducer in the first slice would create a second lifecycle implementation, so do not add one.
## Reviewed sources

- `docs/iterations/2026-09-25-runtime/r5-transcript-implementation-handoff.md`
- `docs/iterations/2026-09-25-runtime/r8-viewport-handoff.md`
- `docs/iterations/2026-09-25-runtime/reviews/r8-viewport-design-review.md`
- `internal/session/durable_contract.go`, `types.go`, `derive.go`, `tree_builder.go`, `prepared_snapshot_test.go`
- `internal/core/types.go`, `run_status.go`, `eventstream.go`, `turnrunner.go`
- `internal/cli/repl_session.go`
