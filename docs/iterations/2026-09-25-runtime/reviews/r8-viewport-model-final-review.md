# R8 viewport model final review

Reviewed fixed commit `b02a48c088bd580076c8dc317e5032549c0579cd`, whose parent is `e54688c42f7e722e09e7f3398404b03827b3ea80`, in a clean detached review worktree at `/Users/pengjunchen/.codex/worktrees/r8-viewport-model-final-review`. The complete product delta is limited to `internal/tui/transcript_viewport.go` and `internal/tui/transcript_viewport_test.go`.

## Decision

**Accepted for the viewport primitive scope.** All three findings in the prior rejection are fixed in the reviewed commit.

1. **Narrow-width wrapping and source offsets now agree.** Metadata line counts use the actual sanitized `Hardwrap` output; the same helper returns the wrapped string and offsets used by rendering. Offsets advance through preserved source bytes and only consume a source newline when the wrapped newline matches it. The width-1 regression cases cover a variation-selector grapheme (`✈️x`), a flag (`🇨🇳x`), a ZWJ/skin-tone grapheme (`👩🏽‍💻x`), and ANSI-styled CJK followed by `x`. They assert that offset count equals rendered-line count, each offset is a grapheme boundary, and trailing text remains visible.
2. **Successful selection and search pause follow-tail even for visible rows.** `Select` and a successful `Search` clear `followTail` before `ensureRowVisible` can take its already-visible early return. Regression tests append rows after selecting/searching a visible non-tail row and confirm the viewport stays at the chosen anchor and the target stays in the window.
3. **A removed anchor clamps to the nearest surviving old ordinal.** `SetRows` now retains the old anchor index and clamps it to the final surviving row when shrinking below that index. The regression case scrolls to old row 7, removes it while shrinking ten rows to five, and confirms the new anchor is row 4.

## Independent verification

`rtk proxy go test -list '^TestTranscriptViewport' ./internal/tui` listed 15 non-zero viewport tests. The targeted race run for the three fixes and adjacent resize, cache, metadata, and error-state cases passed. The full TUI race suite and vet passed:

- `rtk proxy go test -race ./internal/tui -run '^TestTranscriptViewport(NarrowWidthOffsetsMatchActualWrappedGraphemes|SelectingVisibleRowPausesFollowTail|VisibleSearchHitPausesFollowTail|RemovedTailAnchorClampsToLastSurvivor|ResizeKeepsLogicalAnchor|WindowAndCacheBounds|ReusesUnchangedRenderedRows|SetRowsReusesMetadataAndRejectsBadKeys|EmptyAndCellSafeRows)$' -count=1`
- `rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tui`
- `rtk proxy go vet ./internal/tui`
- `rtk git diff --check e54688c42f7e722e09e7f3398404b03827b3ea80 b02a48c088bd580076c8dc317e5032549c0579cd`

The checked-in tests also cover duplicate/empty keys and missing loaded scope failing atomically; repeated-key search; scroll and End; logical-anchor preservation across resize; empty and negative geometry; loaded-scope preservation; visible-plus-overscan cache bounds; and unchanged-fragment reuse.

## Scope and evidence limits

This accepts only the viewport model primitive. It does not establish app wiring, loaded-scope UI display, streamed rendering, a PTY resize journey, or full-history storage/search behavior. The model retains per-row metadata and only bounded visible-plus-overscan rendered fragments; rendering may temporarily wrap a full row before cloning retained fragments. This final review did not rerun the earlier `BenchmarkTranscriptViewportWindow`; the previous benchmark figures at `e54688c` are not fresh performance evidence for `b02a48c`.
