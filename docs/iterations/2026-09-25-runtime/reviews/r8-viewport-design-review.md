# R8 viewport design review

Reviewed the frozen handoff `60b2a00a310a26f493700541ecc244e58d3fe6d3` against its declared source baseline `fa7c9696fe2c522baf21c31a5d0f30f1c7e07661`, current TUI source, the R5 live-projection commits `39041d5` / `34c33b8` / `40178b4`, and the R5 transcript implementation handoff. This is a static design review; no tests or PTY runs were performed.

## Decision

**Approved for the independent viewport model and chunk-renderer slices.** The handoff distinguishes presentation-window clipping from canonical transcript/source capture, limits search to the loaded projection with a visible scope label, avoids claiming session-size-independent memory or full-history search, and separates renderer-only 1 MiB stress data from Runtime/provider limits. It also asks for benchmark and PTY evidence as future acceptance rather than presenting performance or terminal behavior as already proven.

Root has approved the three integration choices in §“Root decisions before app integration”: loaded-projection-only search with its scope shown, the current split layout with per-pane anchors, and process-local display keys only for viewport state. Those keys must never participate in event correlation, reconciliation, persistence, or user-visible durable identity.

## Required integration gate

### R8VIEW-01 — R5 live grouping is not the keyed canonical transcript projection

**P1 for app integration; no blocker to the independent primitives.** The handoff correctly requires durable anchors to use `(session, branch, EntryID)` and says not to infer identity from timestamps, rendered text, position, or `ToolCallID` (§§19–26, 30–36). Its dependency wording should be read as requiring the separate R2/R5 canonical hydration and reconciliation seam as well as acceptance of the live-projection chain; acceptance of `7f66 + 784 + 508` alone is insufficient.

The source explains why: the integrated R5 chain at `40178b4` adds grouped live tool events, late/duplicate event handling, and bounded tool-argument copying. `AccordionEntry` still has rendered `Summary`/`Full`, timestamp, and optional `ToolCallID`, but no durable row key (`internal/tui/accordion.go:50-92`). `core.AgentEvent` has no `RunID`, assistant ordinal, or `EntryID`, and `CoreEventToAgentEvent` forwards only its current event fields (`internal/core/types.go:65-100`, `internal/tui/bridge.go:52-76`). The active tool map is group-local presentation state; it cannot identify a durable row. The current stdout/stderr path still concatenates into `target.Full` for every event (`internal/tui/app.go:1552-1565`), which the handoff appropriately includes in the append-once renderer work.

The separate R5 transcript handoff supplies the needed target: a value-only DTO carrying the selected session/branch, actual `EntryID`, and canonical `AgentMessage`; provisional events remain separate and use exact `RunID + assistant/group ordinal + ToolCallID` only when that seam exists; final same-handle snapshot reconciliation supplies actual durable IDs (`r5-transcript-implementation-handoff.md:17-24, 30-40`). That default CLI/TUI hydration and reconciliation is not implemented by the live grouping commits. Therefore:

- The pure viewport model may proceed with synthetic keyed rows, and the renderer may proceed independently as described in R8 §“Small file ownership and dependency order”.
- Do not integrate the viewport into the default app or call a PTY journey canonical-history evidence until R5/R2 supplies keyed startup rows and a non-positional mapping/replacement path from provisional rows to snapshot rows.
- Any provisional row without an exact source identity uses a private temporary view key; its conversion to the canonical `EntryID` must come from an explicit R5/R2 reconciliation result, never a text, timestamp, event-order, or `ToolCallID` guess. Keep canonical source text available for resize reflow; do not rewrap only the event-time-rendered `AccordionEntry.Full` string.

This is a sequencing clarification, not a request for R8 to implement session storage, paging, or identity generation.

## Source and acceptance checks

The described baseline gaps are real. `AccordionModel.Render` walks all entries; `RenderView` materializes matching rendered lines before tail clipping (`internal/tui/accordion.go:218-306`). `renderViewLocked` scans entries for tool rows, reserves a fixed two lines in split mode, and appends variable-height status, todo, input, and interaction content afterward (`internal/tui/app.go:741-830`). Main-view Up/Down selects an index while PageUp/PageDown belongs to the approval modal; resize currently resets approval-detail review state (`internal/tui/app.go:652-667, 1851-1873`). The current streaming path joins the full `streamingBuf`, splits/scans accumulated Markdown, and retains rendered stable-prefix state (`internal/tui/app.go:1606-1679`, `internal/tui/streaming_markdown.go:19-100`). The handoff identifies these concrete costs and protects modal/editor ownership rather than treating the R5 approval scroller as a general viewport.

The acceptance evidence is appropriately separated: keyed 10k-row model tests for anchor/search behavior; cell/ANSI goldens; renderer-only 1 MiB streaming benchmarks; and a later real CLI PTY journey for resize and interaction behavior. The p95 target is labeled a budget to measure, not an existing result. The R2 full `Snapshot()` limitation is explicit, and the document does not imply that bounded rendered cache bounds total transcript memory.

No additional root decision is needed for this design slice. The one required distinction for handoff is that R5 live-projection-chain acceptance does not satisfy the R2/R5 keyed-canonical-hydration prerequisite for app integration.
