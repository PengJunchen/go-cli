# Independent review: R5/R8 UI app integration design revision

## Decision

**REJECT as a builder handoff pending two P2 contract clarifications.** The revision corrects the five findings in `ui-app-design-review.md`: a bound-but-undispatched run cannot become `Synced`; owned pumps replace `tea.Tick`/`waitForMsg` while excluding Bubble Tea's unjoinable internal commands; renderer hidden-prefix state is carried to each row; local send gaps remain sticky and separate from Core event gaps; and the source/status table now reflects the accepted queue and renderer slices. The rest of the proposal is statically implementable within the stated R5/R8 ownership order, subject to the two issues below.

Reviewed draft: `docs/iterations/2026-09-29-runtime/ui-app-integration-design.md`. Source baseline: `182e1bf9271b92e6cbca518e9f0d41408d82bfce`; current checkout: `1144e1ebc9f339a04d502f0bdca04069c9d30ff5`. The only change between those commits is ACP documentation; `internal/core`, `internal/cli`, and `internal/tui` are unchanged. This was a static review only. No product source was changed and no tests, benchmarks, or PTY runs were performed.

## Findings

### P2 — `SourceTruncated` provenance is not available at the proposed app seam

The app contract requires an independent per-row `SourceTruncated` fact and says to populate it from existing capture metadata (`ui-app-integration-design.md:83,89,91`). That structured fact is not carried to either live events or the durable transcript today:

- `StreamingBashTool` returns `ToolResult.Metadata["truncated"]` (`internal/tools/streaming_bash.go:216-220`), but the prepared dispatch path retains only `Output` and `IsError` (`internal/core/run_commit_consumer.go:102-145`); the receipt/canonical row is therefore content plus receipt fields, not the tool metadata (`internal/session/types.go:57-145`).
- The stream path emits a textual `[output truncated]` marker (`internal/tools/streaming_bash.go:21,121-124,366-369`). `eventStreamSink.Send` turns that marker into ordinary `Content` with `ToolCallID` and `Stream`, and `CoreEventToAgentEvent` does not add a truncation fact (`internal/core/loop.go:1421-1430`, `internal/tui/bridge.go:52-75`). A source can itself print the same marker, so parsing row text cannot reliably distinguish capture truncation from literal output.
- The proposed `AgentEvent` increment lists occurrence coordinates and `ReceiptOutcome`, but no source-truncation metadata (`ui-app-integration-design.md:38-41`); the R8 boundary also excludes changes to managed capture and durable session schema (`:128`).

As written, a builder cannot truthfully set or preserve the promised provenance flag from the canonical snapshot. Either narrow the contract to preserve the source marker as ordinary text without claiming a structured `SourceTruncated` fact, or name the upstream owner/API that carries an unambiguous flag and state how that fact survives canonical refresh/reopen under the no-schema-change constraint. The renderer's `ViewTruncated` flag is otherwise correctly independent.

### P2 — The apply-ack gate has no pre-mutation linearization point

The reliable-control contract says `teaModel.Update` validates and applies the refresh, then acknowledges through the shared gate; cancellation, Quit, or cleanup that wins the gate must return an error and prevent model application (`ui-app-integration-design.md:75`). If cancellation completes the gate after `Update` has mutated the model but before it acknowledges, the caller can receive an error for a request that actually changed the view. The gate as described serializes completion, but does not establish who wins before mutation.

Specify a linearization step before applying: the model must claim `Pending -> Applying` (or equivalent) before mutation; cancellation/quit may claim `Pending -> Cancelled/Closed`; a model that did not claim the request discards it. Once the model claims it, it completes application and posts the buffered ACK even if the caller context is canceled meanwhile. Add a deterministic test that pauses `Update` between identity validation and mutation while cancellation races, and checks both returned outcome and whether rows/status changed. This is needed for the stated rule that a request whose cancellation wins is never applied, including a delayed `Program.Send` delivery.

## Checks that passed

- The durable assistant key `(RunID, CanonicalTranscriptIndex)` is supported by the existing `SessionEntry.TranscriptIndex` contract; `RunCanonical` appends steering/follow-up inputs before generating each assistant response, while `AssistantTranscriptOrdinal` remains the separate tool-call coordinate (`internal/core/loop.go:595-710`; `internal/session/types.go:146-160`).
- The proposed positive `Synced` predicate closes the prior false-positive path: `runOwner.run` currently sets `terminalSyncRequired` only at callback dispatch, while `finishUndispatched` marks the transcript committed without a terminal sink call (`internal/core/run_owner.go:807-832,937-955`). Requiring positive `terminalSyncRequired && terminalSyncSuccess`, owner identity, lease/release success, and quiescence prevents that path from becoming a saved acknowledgement.
- The R8 row-update contract recognizes that the accepted viewport uses a flat prefix index and explicitly permits the narrow indexed line-count seam needed for arbitrary interleaved row updates; full `SetRows` remains snapshot-only (`internal/tui/transcript_viewport.go:80-90,112-140`; design `:79,83`). This is implementable without replacing the viewport's presentation-window role.
- The revision distinguishes actual app `Update`/`View` measurement, renderer-only probes, and CLI PTY evidence, and keeps the zero Finish threshold until real app measurements exist. These are acceptance criteria, not evidence already obtained.

## Verification boundary

No source edits, tests, benchmarks, or PTY journey were performed. The two findings concern missing design contracts, not failures of the accepted queue, renderer, or viewport primitives.
