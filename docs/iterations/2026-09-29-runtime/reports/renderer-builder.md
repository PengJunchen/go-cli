# R8 streaming renderer builder report

Base: `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`; branch: `codex/repair-20260929-renderer`.

## Changes

- Clone chunk-derived line parts and retained rendered rows so the bounded window does not keep the caller's full chunk or Glamour's full output allocation alive. Evicted rows and parts have their references cleared; rendered line storage is bounded as rows are added.
- Include the pending grapheme in width and byte-budget decisions. A pending cluster wraps intact when it would overflow the row; if the byte budget cannot fit it, the snapshot omits the complete cluster and reports hidden content.
- Move the retained source-tail start to a Unicode grapheme boundary, including ASCII bases followed by combining marks.
- Make `Finish` idempotent after its first canonical render. A later `AppendChunk` still resets the renderer for a new message.
- Correct the 1 MiB benchmark to distribute the exact byte total across chunks, report `input-B/op`, and add 1/16/256/1000-chunk comparisons. Added counter tests use the same fixed 1 MiB input at each chunk count.

Tests for the six review findings were run against the original implementation first; they reproduced all six failures. The updated targeted tests pass.

## Verification

- `go test -race -count=1 ./internal/tui`: passed, 561 tests.
- `go vet ./...`: passed.
- `golangci-lint run ./...`: passed, no issues.
- `git diff --check`: passed.
- `go test ./internal/tui -run '^$' -bench '^BenchmarkStreamingMarkdownAppendChunkScaling$' -benchtime=1x -count=1`: passed. Each case processed exactly 1,048,576 bytes.

One-iteration benchmark observations on Darwin arm64 / Apple M5:

| Chunks | Input bytes/op | Time/op | Allocated bytes/op |
| ---: | ---: | ---: | ---: |
| 1 | 1,048,576 | 3.89 ms | 2,979,808 |
| 16 | 1,048,576 | 4.20 ms | 4,461,552 |
| 256 | 1,048,576 | 7.13 ms | 28,078,704 |
| 1000 | 1,048,576 | 16.20 ms | 101,309,984 |

The measurements show increasing snapshot allocation and time as the same input is divided into more calls. They are diagnostic single-iteration measurements, not a stable performance comparison.

## Limits

`Finish` still sends the complete canonical source through Glamour once to preserve document-wide Markdown semantics. This report makes no claim that this full-document render has bounded latency or peak memory. The app is not wired to this primitive by this change.

## Review follow-up: pending wrapped row

The independent review found that the final pending grapheme could add a virtual wrapped row after the retained-line limit was enforced. The exact width-1 probe returned 257 visible rows while `stats().renderedLines` reported 256. A regression test reproduced 257 rows before the fix.

`Window` now counts the pending row before returning the snapshot and evicts the oldest retained row when needed, keeping `HasHiddenPrefix` set. `stats().renderedLines` now reflects the returned snapshot's actual row count.

Follow-up verification passed: the width-1 regression; CJK width-2, pending-byte, backing-clone, and Finish contract tests; `go test -race -count=1 ./internal/tui -run '^TestStreamingMarkdown'` (22 cases); full `go test -race -count=1 ./internal/tui` (562 tests); `go vet ./...`; `golangci-lint run ./...`; and `git diff --check`.
