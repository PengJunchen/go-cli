# Independent review: renderer row-bound fix `4dc8bdb`

Reviewed commit `4dc8bdb5e39cb0b5c7720acc296b0c0fbde7141f` (`fix(tui): count pending wrapped row in window bounds`), parent `eb261ea8916e72eb4e4749fcdf1974ea06b24593`, in fresh detached worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-renderer-review-4dc8`.

## Decision

**ACCEPT for this renderer source fix.** The prior width-1 row-cap defect is fixed, and the relevant earlier bounded-window, pending-grapheme, cloning, and Finish/Resize probes pass. This review does not establish app integration or bound full-document Glamour latency or peak memory.

## Row-cap regression

The fix counts committed lines, the current line, and the virtual row introduced by a pending wrapped grapheme. It evicts retained leading rows until the snapshot fits; if the pending row itself cannot fit, it omits that complete grapheme and marks hidden content. `stats().renderedLines` now counts rows in the returned snapshot.

The exact prior reproduction used 266 ASCII bytes at width 1. It returned 256 visible rows, `stats().renderedLines == 256`, 511 content bytes, and `HasHiddenPrefix == true`.

## Additional probes

- Pending width-2 CJK graphemes still wrap as `界\n界`; width-1 CJK and combining text keep the following visible character on its own row.
- At width `1<<20`, a pending CJK grapheme that would exceed the 64 KiB snapshot budget is omitted whole; the returned UTF-8 remains valid and the reported byte and row counts match the snapshot. A 128 KiB ASCII chunk at the same width retains bounded visible content and reports hidden content.
- Chunk-derived rows/parts and rendered rows do not retain the large caller/render input backing strings. Eviction leaves no stale string references in unused live slice capacity.
- `Finish`→`Resize`→repeat `Finish` keeps the resized result without repeating the canonical render; `Reset` permits a new canonical message. The committed source-tail, repeat-Finish, split-UTF-8, combining, and resize tests also passed in the focused run.
- Temporary review probes were removed. No benchmark was run.

## Independent verification

- `go test -race -count=1 ./internal/tui -run '^TestStreamingMarkdown'`: passed, 22 matched cases/subtests per RTK.
- `go test -race -count=1 ./internal/tui`: passed, all 562 tests.
- `git diff --check HEAD^ HEAD`: passed.

The builder report for this commit records `go vet ./...` and `golangci-lint run ./...` as passing; those two gates were not rerun independently in this review.
