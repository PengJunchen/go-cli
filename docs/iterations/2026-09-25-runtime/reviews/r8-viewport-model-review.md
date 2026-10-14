# R8 viewport model review

Reviewed fixed commit `e54688c42f7e722e09e7f3398404b03827b3ea80` in an independent detached worktree. The source delta is limited to `internal/tui/transcript_viewport.go` and `internal/tui/transcript_viewport_test.go`; no app integration or PTY behavior is claimed.

## Decision

**Not accepted yet.** Three viewport behaviors need correction before this primitive slice is accepted.

1. **[P2] Narrow-width wrapping can hide source text.** `newTranscriptViewportRowMetadata` derives `lineCount` from `transcriptViewportSourceLineOffsets` (`internal/tui/transcript_viewport.go:214-228, 609-640`), but `renderRange` renders the result of `transcriptViewportWrap` and caps iteration at that metadata count (`:457-503`). At width 1, `transcriptViewportWrap("✈️x", 1)` is `"\n✈️\nx"` (3 lines) while offsets are `[0 6]` (2 lines). A one-row viewport reports two total lines and returns only `""` and `"✈️"`; `x` is omitted. The same mismatch reproduced for `🇨🇳x`, `👩🏽‍💻x`, and ANSI-styled CJK followed by text. Make line counts and offsets agree with the actual wrapped output, then retain regression coverage for overwide graphemes, variation selectors, flags, ZWJ, ANSI, and narrow widths.

2. **[P2] Selecting an already-visible non-tail row does not pause following.** `Select` calls `ensureRowVisible` (`:316-323`), whose visible-row early return occurs before `followTail` is cleared (`:357-378`). With five rows and height three, selecting visible `entry-00003` while following leaves `FollowTail()==true`; after two appended rows, the window starts at `entry-00004` and the selected row is gone. This conflicts with the handoff rule that selection away from the tail pauses following. Successful search has the same behavior for an already-visible hit: the hit key remains set while appends move it out of the window. The handoff explicitly requires scrolling an offscreen hit into view but does not state the follow-tail rule for a visible hit; define that behavior, with pausing on a successful non-tail search being the predictable choice.

3. **[P2] Removing an anchor near the old tail jumps to the first row after shrink.** In `SetRows` (`:182-194`), `nearest` starts at zero and is replaced with `oldAnchorIndex` only when it is less than the new row count. When a scrolled anchor at old index 7 is removed while shrinking from 10 rows to 5, the condition is false and the viewport jumps to `entry-00000`; the subsequent `nearest >= len(v.rows)` clamp cannot run because `nearest` is already zero. Clamp the old index to the last surviving row when it exceeds the new length, or explicitly define another nearest-anchor policy. This input produces `entry-00004` as the nearest surviving index if index distance is the intended rule.

## Verified behavior

- The loaded source rows are copied, keys and loaded scope are validated atomically, metadata is reused only when key/content/revision/width/expansion match, and changed visible rows are refreshed. Search is restricted to loaded rows, returns keyed hits for duplicate text, and `Window` preserves the loaded-scope label.
- The rendered cache contains only the visible range plus two overscan lines and clones visible fragments rather than retaining the full backing wrapped string. `Window` uses the prebuilt line prefix and starts rendering at the requested row; it does not iterate all loaded history.
- Extreme geometry was probed with `Resize(math.MaxInt, math.MaxInt)` and one short row: it returned one visible line and a one-line cache without attempting a geometry-sized allocation.
- The committed resize test exercises CJK, ANSI, tabs, and a ZWJ emoji and checks the anchor offset at a grapheme boundary. Additional review-only width/offset probes exposed the mismatch in finding 1.

## Independent verification

`rtk proxy go test -list '^TestTranscriptViewport' ./internal/tui` listed the 11 viewport tests present at the reviewed commit. `rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -run '^TestTranscriptViewport' -count=1` passed. `rtk proxy go vet ./internal/tui` passed.

`rtk proxy go test -list '^BenchmarkTranscriptViewportWindow$' ./internal/tui` listed the benchmark. `rtk proxy go test ./internal/tui -run '^$' -bench '^BenchmarkTranscriptViewportWindow$' -benchmem -benchtime=1000x` passed on Darwin arm64 / Apple M5:

| Loaded rows | ns/op | B/op | allocs/op |
|---:|---:|---:|---:|
| 1,000 | 2,798 | 13,759 | 13 |
| 5,000 | 2,920 | 13,759 | 13 |
| 10,000 | 2,673 | 13,759 | 13 |

The flat primitive benchmark supports bounded `Window` work for this fixture; it is not an interactive p95, full TUI, or session-memory bound claim.

Review-only tests were added to the detached review worktree, listed with `rtk proxy go test -list '^TestReviewProbe' ./internal/tui`, and then removed. `rtk proxy go test -v ./internal/tui -run '^TestReviewProbe' -count=1` reproduced the selection, visible-search, and removed-anchor behaviors above. Its offset parity case failed on the four narrow-width examples listed in finding 1; its `math.MaxInt` geometry probe passed. The temporary test file is not part of the submitted commit or review worktree.

This review covers only the independent viewport primitive. App integration, R2/R5 canonical hydration, stream rendering, loaded-scope UI display, and the real terminal resize journey remain separate gates.
