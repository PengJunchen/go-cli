# Independent review: UI EventBus / bridge / viewport stage 1

## Decision

**REJECT the end-to-end run-metadata/publication claim at `d692d144008ce7256fad72218ab4d028046fe26a`; ACCEPT the narrow EventBus/bridge pointer-copy and Fenwick viewport seams as foundation work.** The bridge can preserve values supplied to it, and arbitrary-row viewport updates use an indexed line count. The actual run producers still omit the new occurrence and receipt fields, prepared dispatch discards the source truncation metadata, and the owner exposes no durable-publication acknowledgement. This is a scope/contract gap, not a regression introduced by the queue-copy or viewport changes.

Reviewed commit: `d692d144008ce7256fad72218ab4d028046fe26a` (parent `dbc0cfdf171698477e45ab050e062cf73e9015d2`). The frozen design is `ui-app-integration-design.md`, SHA256 `03c3e3501c8070cd80f48a1251ab5fd5bba95eb04d7b2bce2bcc0b440054e39d`.

## Blocking gaps for the end-to-end claim

### Live events do not carry producer-owned occurrence identity or receipt outcome

`LoopAgent.RunCanonical`'s `sendEvent` closure only appends and forwards the supplied event (`internal/core/loop.go:497-504`); it does not enrich it with `RunID` or transcript coordinates. The complete assistant message event at `loop.go:699-701` omits `RunID` and `CanonicalTranscriptIndex`. The local `assistantOrdinal := uint64(iter + 1)` at `loop.go:707` is used by the durable tool consumer, but is not attached to the pre-tool, tool-call, streaming-output, or tool-result events. In particular, the parallel receipt event at `loop.go:732-738` reads `message.ReceiptOutcome` only to derive `IsError`, and the sequential path at `loop.go:843-845` omits `result.outcome` from its event. Cancellation receipts at `loop.go:566-574` also omit their known `interrupted` outcome.

The new fields are copied through `CoreEventToAgentEvent`, but its test constructs a fully populated `core.AgentEvent` directly (`internal/tui/bridge_test.go:32-67`); it does not exercise a producer. As shipped, the normal core-to-TUI path cannot use these fields to correlate a live assistant/tool occurrence. The loop counter is monotonic locally, but no producer-owned coordinate reaches the consumer, so the event stream cannot prove monotonic occurrence identity or safely reconcile repeated tool-call IDs.

### Source truncation is not copied from the real ToolResult metadata

`dispatchPreparedRunCommitCall` receives the actual `*tools.ToolResult`, then retains only `Output` and `IsError` (`internal/core/run_commit_consumer.go:102-145`). Its `runCommitDispatchResult` has no metadata field (`run_commit_consumer.go:55-61`). Consequently no production event sets `AgentEvent.SourceTruncated` from `ToolResult.Metadata["truncated"]`; `CoreEventToAgentEvent` can only preserve a value supplied by some caller. Keeping the live field nil is safer than inferring from output text, but it does not satisfy the frozen provenance path (`ui-app-integration-design.md:83,89`).

### The owner does not publish a positive durable-sync acknowledgement

`core.Result` has no `DurablePublication` field (`internal/core/types.go:145-159`), and `runOwner.resultSnapshotLocked` copies only run ID, outcome, execution state, status handle, and success (`internal/core/run_owner.go:1191-1198`). Although `runCommitConsumer.commitTerminal` marks the owner after `CommitTerminal` returns nil (`run_commit_consumer.go:281-296`), that positive fact is not returned to the caller as a typed state with binding identity.

The current owner predicates also use `!terminalSyncRequired || terminalSyncSuccess` (`run_owner.go:713-716,1008,1143-1144`), which the frozen contract explicitly prohibits as proof of sync (`ui-app-integration-design.md:54-59`). `EventStream.Send` returns nil for a send after close while recording a gap (`internal/core/eventstream.go:103-124`), and `MemoryEventBus.Publish` is non-blocking and may drop a subscriber event (`eventbus.go:66-88`); neither queue acceptance nor a `done` event can substitute for the owner’s terminal commit fact. The supervisor also calls `guard.Release(claim)` without recovery (`run_owner.go:1015-1020`), so a release panic is not recorded as a failed publication fact as the frozen contract requires.

## Foundation seams accepted

The newly added scalar pointer fields are defensively copied at each relevant boundary: EventStream copies before queueing (`internal/core/eventstream.go:113-115,232-237`), EventBus creates a copy per subscriber (`eventbus.go:75-86`), and the TUI bridge copies the coordinate and truncation pointers (`internal/tui/bridge.go:56-80`). The added mutation-after-send tests passed. This establishes ownership for the new `*uint64` and `*bool` fields; it does not claim that `cloneAgentEvent` deep-copies every pre-existing event payload.

`transcriptViewport.updateRow` reflows only the changed row, updates a Fenwick line-count tree, and preserves its anchor/selection/search keys (`internal/tui/transcript_viewport.go:230-263,266-296`). Prefix queries and row lookup are logarithmic. The 10,000-row test checks line totals, visible cache invalidation, anchor preservation, and stale-revision rejection (`transcript_viewport_test.go:301-334`). Full `SetRows` remains an O(rows) snapshot replacement, as allowed by the design.

## Gates for the next R8 integration slice

This commit does not add a refresh controller or `ApplyTranscriptRefresh` model-apply acknowledgement; `updateRow` is private and is only exercised by its unit test. There is therefore no new refresh goroutine to classify as leaked or bounded. Existing `CriticalSendContext` provides queue admission/backpressure, not model-apply acknowledgement, as the frozen contract states (`ui-app-integration-design.md:72-75`). The current TUI still exits when its event source closes (`internal/tui/app.go:410-412,713-717`), so a later app-lifecycle slice must keep the UI alive long enough to apply the final owner/snapshot result. These are integration gates, not regressions in the accepted foundation seams.

The smallest producer follow-up should populate `RunID` from the run context, pin and reuse the canonical assistant index for incremental and complete assistant events, attach the existing assistant ordinal to every tool lifecycle/output/receipt event, copy receipt outcome from the actual typed receipt, and preserve only a boolean `ToolResult.Metadata["truncated"]` through prepared dispatch to the final tool-result event. Add end-to-end tests for repeated `ToolCallID`s and parallel completions. The owner follow-up should expose `Unbound/Pending/Synced/Failed` from owner-held binding, terminal-commit, lease-close, release, and quiescence facts, with negative tests for undispatched runs, failed terminal writes, callback-forged values, and release panic. Queue sends must remain observations, never saved-state proof.

After those source facts exist, the app slice still needs the bounded control-request/apply-ack path, cancellation/join tests, and the source-close lifecycle change before canonical refresh can be integrated. Actual App benchmarking and PTY journeys were not run here and remain explicit future evidence gates.

## Verification

Tests ran in a temporary detached checkout of the exact target commit; no source or test files were changed.

- `go test -race -count=1 ./internal/core ./internal/tui` — passed; RTK reported 1,448 tests across 2 packages.
- `go test -race -count=20 -run 'TestEventBusCopiesOccurrenceCoordinatePointers|TestEventStreamCopiesOccurrenceCoordinatePointers|TestCoreEventToAgentEvent|TestTranscriptViewportUpdateRowReindexesOnlyChangedRow' ./internal/core ./internal/tui` — passed; RTK reported 80 executions across 2 packages.
- No App benchmark or PTY journey was run.
