# Independent review: renderer fix `eb261ea`

Reviewed commit `eb261ea8916e72eb4e4749fcdf1974ea06b24593` (`fix(tui): bound streaming markdown window retention`) in a fresh detached worktree at `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-renderer-review-eb26`.

## Decision

**REJECT.** One P2 remains: the returned presentation can exceed the 256-row window limit by one row when the final pending grapheme wraps. No P1 was found.

### P2: pending wrapped row bypasses the visible row limit

`Window` computes and emits `pendingWrap` after the retained line parts (`internal/tui/streaming_markdown.go:168-206`), but that virtual row is not included in the row eviction performed by `enforceWindowBounds` (`:663-684`) or in the row count returned by `stats` (`:765-780`).

Minimal reproduction:

```go
renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
window := renderer.AppendChunk(ctx,
    strings.Repeat("x", maxStreamingRenderLines+10),
    RenderOpts{Width: 1},
)
visibleRows := strings.Count(window.Content, "\n") + 1
```

Observed: `visibleRows == 257`, `renderer.stats().renderedLines == 256`, and the content is 513 bytes. The byte budget holds, but the actual visible-row cap does not. The pending row must be included in the line-bound decision; if it is omitted to preserve the cap, the window must keep `HasHiddenPrefix` true.

## Verified behavior

- The committed focused renderer race run passed. `go test -list '^TestStreamingMarkdown' ./internal/tui` lists 17 focused test functions; `go test -race -count=1 ./internal/tui -run '^TestStreamingMarkdown'` passed (RTK reported 21 test cases/subtests).
- `go test -race -count=1 ./internal/tui` passed all 561 package tests.
- `go vet ./...` exited successfully.
- Temporary review probes were removed after execution. Every byte split of CJK, emoji, flag, and combining examples preserved valid UTF-8 and following visible text. Width 1 and width `1<<20` chunk probes stayed within the byte limit and retained visible CJK text. Probes for combining, ZWJ, and regional-indicator cutoffs confirmed `setSourceTail` advances past a partial grapheme; the committed `Finish` cutoff test also passes.
- Backing-storage probes found no caller-chunk or rendered-input string retained by cloned visible rows/parts, and no stale string references in unused live slice capacity after eviction. The source ring stores bytes and numeric unit lengths, so ring eviction does not retain source strings.
- A giant zero-width combining input stayed within the 1 KiB pending-grapheme cap and preserved following visible text. One local diagnostic measured 16,384 combining marks in one chunk at about 85 ms and 4,096 one-mark chunks at about 29 ms; these are observations, not performance guarantees.
- `Finish` rendered once when repeated, returned the current finished window after resize without another canonical render, and `Reset` allowed a new canonical message. The exact 1 MiB append benchmark distributes `1<<20` bytes across each chunk count and reports `input-B/op`; it is diagnostic for append behavior. No Glamour latency or peak-memory claim is made.

The only blocking finding is the 257-row snapshot. This review made no product-code changes and ran no Glamour performance benchmark.
