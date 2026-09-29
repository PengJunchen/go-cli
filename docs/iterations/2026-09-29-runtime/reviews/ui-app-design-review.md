# Independent static review: R5/R8 UI app integration design

## Decision

**REJECT as a current builder handoff pending the P2 contract corrections below.** The architecture is otherwise implementable: the canonical assistant key can be derived from the existing per-run transcript index, the tool ordinal remains a separate coordinate, and the lifecycle ordering correctly waits for `turnDone` and a model-apply acknowledgement before quitting the app. This review covers the design and current source only; it does not claim app, benchmark, or PTY evidence.

The design pins source baseline `5db266977b260ba0f1707e766f327bcba92671dd` (`ui-app-integration-design.md:3`). The current checkout reviewed was `fb4d0cc216b3e121aa5afdec8597c7bd94e51a23`. No product source was changed and no tests were run.

## Findings

### P2 — A bound run cancelled before callback dispatch can be published as `Synced`

The state table at `ui-app-integration-design.md:55` allows a bound run to be `Synced` when a terminal sync is “required” and successful. It must also require positive evidence that the bound run actually wrote and synced its terminal transcript, including when cancellation arrives before callback dispatch.

The current owner exposes a concrete false-positive path. `runOwner.run` records a valid binding and sets `terminalSyncSuccess=false` (`internal/core/run_owner.go:799-809`), but sets `terminalSyncRequired` only in the callback-dispatch branch (`:824-832`). If the context is already canceled, `finishUndispatched` marks `callbackFinished=true` and `transcriptCommitted=true` without invoking `CommitTerminal` (`:937-955`). The supervisor then treats `!terminalSyncRequired` as terminal-synced and can reach quiescent after lease/slot release (`:1008-1036`). A publication implementation that uses the listed facts and the “required sync” condition can therefore report `Synced` for a bound, canceled run with no terminal commit. That could falsely label an uncommitted partial transcript as saved.

Require successful owner-observed `CommitTerminal` sync for every bound `Synced` result; a callback that never started must remain `Failed` (or another explicitly non-acknowledged state), not count as “sync not required.” Do not treat `finishUndispatched` as proof of a transcript commit. Add a test that binds a valid sink, cancels before callback dispatch, observes lease/slot release, and asserts the result is not `Synced` and the UI does not label it saved. Also assert that a panicking `RunSlotGuard.Release` is captured as a private release failure and cannot produce `Synced`.

### P2 — `tea.Tick` cannot meet the stated status-task join contract

The design permits sampling through `tea.Tick` (`ui-app-integration-design.md:56`) but also requires sampling tasks to be joined before `<-app.Done()` (`:73`). The pinned dependency is Bubble Tea v1.3.10 (`go.mod:7`): `tea.Tick` creates a timer and its command blocks until that timer fires (`/Users/pengjunchen/go/pkg/mod/github.com/charmbracelet/bubbletea@v1.3.10/commands.go:154-164`). Bubble Tea's command runner explicitly does not cancel or await command goroutines and documents that they remain until the command returns (`/Users/pengjunchen/go/pkg/mod/github.com/charmbracelet/bubbletea@v1.3.10/tea.go:343-366`). Thus a pending tick can outlive `app.Done()`, and the caller has no join handle for it.

Use one explicitly REPL-owned status sampler with a `uiCtx`-aware wait/send path and a `statusDone` acknowledgement; stop and join it before `app.Quit()`. Alternatively, weaken the contract to allow Bubble Tea's bounded timer-command lifetime and do not claim those tasks are joined. Add a shutdown test that leaves a status timer pending, quits, and verifies the chosen lifetime contract.

### P2 — Stream-window truncation can be dropped before the row is rendered

The renderer returns both bounded `Content` and `HasHiddenPrefix`; the latter is sticky until `Reset` and means the returned window must not be presented as the complete message (`internal/tui/streaming_markdown.go:20-24`). The proposed row seam is written as `UpdateRow(key, revision, boundedText, expanded)` (`ui-app-integration-design.md:81`), and the live-chunk test checks row updates/revisions but not the hidden-prefix fact (`:112`). The renderer integration text says to pass the bounded window (`:87`), but does not explicitly require preserving its `HasHiddenPrefix` bit in the app row/view model. If the call site passes only `window.Content`, an evicted 64 KiB preview can look like a complete assistant/tool row.

Carry `HasHiddenPrefix` (or an equivalent per-row `viewTruncated` fact) into the row update and render an explicit truncation marker. Keep it sticky across further chunks and resize; derive/reset it only from the renderer's returned window or a new renderer generation. Add an actual `teaModel.Update`/`View` test that streams past the byte/line bound, verifies the marker and retained visible suffix, and separately proves loaded-canonical search still uses the full source.

### P2 — The accepted local send-gap fact is omitted from layout and refresh invariants

The accepted backpressure contract keeps `LocalSendGapCount` separate from Core `EventGapCount` and does not clear it on status updates (`reviews/backpressure-d717-review.md`, “Contract review”). The source renders the local gap notice as an additional line after the ordinary body/footer (`internal/tui/app.go:945-956`); `RunStatus.EventGapCount` is a separate Core-owned value. The new layout rule measures spinner, interaction, editor, todo, status, pause, and quitting rows (`ui-app-integration-design.md:85`) but omits the local-gap notice. The publication rule discusses unresolved event gaps (`:58`) without explicitly preserving this UI-only gap across `ApplyTranscriptRefresh`.

Keep local queue gaps as a sticky UI diagnostic, distinct from Core event gaps and durable publication; do not clear or upgrade them when a status snapshot or canonical transcript refresh is applied. Include the notice's wrapped row count in `footerRows`. Extend the body-height test with a nonzero local gap at short/narrow geometry, and assert the notice survives a successful model-apply refresh while Core `EventGapCount` remains independently represented.

### P2 — The dependency/status table is stale for the current target

At the current checkout, the accepted queue API is already present: `BubbleteaApp.Send` returns typed errors and `CriticalSendContext` waits on the captured run generation (`internal/tui/send.go:48-120`; integrated as `6ca35e3`). The accepted renderer fix is also integrated (`11d81a1` and `f39d466`); `reviews/renderer-4dc8-review.md` records ACCEPT. Yet the design's source-gap table says `Send` has no return value and queue-full silently drops (`ui-app-integration-design.md:17`), says renderer `4dc8` is still awaiting acceptance (`:18`), and repeats those states in the current-gap summary (`:123`). Those statements are correct only for the older pinned baseline, not the implementation target now being handed to a builder.

Refresh the source baseline/status table against the intended integration SHA, or clearly mark the document as a historical pre-integration snapshot and add a current delta. Show the already-accepted queue/renderer behavior as available, while retaining the still-open app bridge, canonical snapshot, owner publication, and app integration gaps. This avoids rebuilding accepted primitives and makes the builder's release prerequisites actionable.

## Source-grounded checks that passed

- `LoopAgent.RunCanonical` starts its run transcript with the user input, commits/appends steering and follow-up messages before generation, and appends the assistant response afterward (`internal/core/loop.go:379-381`, `:595-651`, `:677-700`). Therefore `uint64(len(transcript))` fixed immediately before `generateWithContinuation` is the planned assistant's next canonical transcript index. `assistantOrdinal := uint64(iter+1)` remains the separate coordinate passed into tool commit/execution; do not interchange them (`internal/core/loop.go:705-710` and tool-dispatch paths).
- `ProjectPreparedTranscript` resolves each published message through its paired `SourceEntryIDs` and exact durable `SessionEntry` before building the canonical row (`internal/cli/transcript_projection.go:151-170`). Adding the optional assistant key from that source entry's `RunID` and `TranscriptIndex` preserves provenance and leaves legacy rows uncorrelated.
- The design correctly distinguishes queue admission from model-apply acknowledgement and orders same-handle refresh after `turnDone`, with app quit only after model application (`ui-app-integration-design.md:63-73`). It also correctly avoids claiming renderer helper benchmarks as actual-app or PTY evidence (`:117-119`).

## Verification boundary

This was a static design/source review only. No source edit, test run, benchmark, app integration, or PTY journey was performed. The findings are design corrections required before using this draft as the current builder handoff; they do not reject the accepted queue or renderer source slices.
