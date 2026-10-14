# Independent review: R5/R8 UI app integration design revision 3

## Decision

**ACCEPT narrow handoff.** The reviewed draft SHA256 is `03c3e3501c8070cd80f48a1251ab5fd5bba95eb04d7b2bce2bcc0b440054e39d`. The two P2 findings from `ui-app-design-r2-review.md` are resolved with source-aligned contracts and deterministic RED coverage. This accepts the design handoff only; it is not implementation, test, benchmark, or PTY evidence.

Source baseline remains `182e1bf9271b92e6cbca518e9f0d41408d82bfce`; current checkout is `1144e1ebc9f339a04d502f0bdca04069c9d30ff5`. The intervening commit adds ACP documentation only; the reviewed core, CLI, and TUI source is unchanged.

## Re-review of the two P2 findings

### `SourceTruncated` provenance

The new contract treats `SourceTruncated` as tri-state: nil means unknown/absent, and true or false is set only from structured metadata. It identifies the real `ToolResult.Metadata["truncated"]` bool, requires R5 to preserve it through prepared dispatch, the final typed core event, the TUI bridge, and the exact tool occurrence row, and keeps it unknown for restored canonical rows when the existing payload has no such metadata. It explicitly treats `[output truncated]` as ordinary text and forbids adding a durable schema field (`ui-app-integration-design.md:35,83,89,116,119,127`).

That is implementable within the assigned R5 seams. The current source confirms that `StreamingBashTool` already returns a structured bool (`internal/tools/streaming_bash.go:216-220`), while prepared dispatch currently discards metadata (`internal/core/run_commit_consumer.go:102-145`) and core/TUI events currently have no truncation field (`internal/core/types.go:65-100`, `internal/tui/bridge.go:52-75`). The draft names those exact transfer points and limits extraction to the bool, so it does not require changing managed capture or the durable session schema. Keeping reopened rows unknown when no durable payload fact exists is accurate.

One defensive-copy detail belongs in implementation: `SourceTruncated` is a `*bool`, so each DTO/row boundary that takes ownership should copy the pointed-to bool and preserve nil, rather than retain an alias. The draft does not promise immutable event/DTO pointers, so this is not a handoff blocker; it should be honored alongside the existing bridge's defensive copying of pointer-backed values.

### Apply-ack cancellation race

The control gate now has an explicit linearization point: validate request binding IDs, claim `Pending -> Claimed` before any model mutation, and only then apply. Cancellation, Quit, or cleanup can transition a still-pending request to `Canceled`/`Closed`; a late Update then fails its claim and performs no mutation. After claim, cancellation cannot complete the gate or return early: the model actor owns the application and must publish the actual `Applied` acknowledgement. Shutdown resolves each unclaimed request once and waits for claimed application ACKs before `Program.Run`/`app.Done()` completes (`ui-app-integration-design.md:75`).

`TestRefreshApplyClaimLinearizesCancellation` now exercises both orderings with barriers, including no mutation when cancellation wins and a still-pending caller until actual ACK when the model wins (`:108`). This closes the prior “mutated but returned error” race without adding an ACK sender goroutine. The single synchronous Bubble Tea `Update` actor can complete a claimed mutation and buffered ACK before returning to the program loop; unclaimed requests are resolved by the shared gate during shutdown.

## Other frozen-contract checks

The five earlier findings remain addressed: bound-but-undispatched runs require positive terminal-sync evidence for `Synced`; explicitly owned pumps replace `tea.Tick`/`waitForMsg` for joinable work while Bubble Tea's internal commands remain outside the join claim; `HasHiddenPrefix` becomes per-row `ViewTruncated`; `LocalSendGapCount` remains sticky and separate from Core event gaps; and the source table reflects the accepted send and renderer slices. Occurrence identity, same-session refresh ordering, bounded viewport updates, full-loaded-source search, and separate app/PTY/performance evidence remain consistent with the frozen source boundary.

## Verification boundary

This was a static, bounded design review at the requested draft SHA. No source files or tests were changed or run, and no benchmark or PTY journey was performed.
