# R8 canonical viewport memory evidence

This report measures the shared-line canonical transcript projection and in-row anchor fix at source commit `587be07e6d0961e72bf7261fb6148b0caae8f963` (parent `edc5719b176eb7e33eb4031ae4fe13fcb4e6c9fd`). The comparison baseline is `36292cc7ce3f797fd7915838a3456fc754db32e7`, before the representation change.

## Fixed workload and method

Environment: macOS Darwin/arm64, Apple M5, Go 1.26.4. Each benchmark used `-benchtime=1x -count=1`; latency percentiles are one-run samples, not a statistical performance claim.

`BenchmarkBubbleTeaTranscriptUpdateView` initializes the real Bubble Tea app with 1,000, 5,000, or 10,000 loaded rows. For each history size, it sends exactly 1,000 assistant events of 1 KiB each (1 MiB total) through `teaModel.Update`, calls `BubbleteaApp.View()` after every event, and reports p50/p95 Update+View latency and allocation delta per chunk. The benchmark also records heap after loading the history and after the stream, following GC.

`BenchmarkBubbleTeaLongTranscriptProjectionRetainedHeap` builds 10,000 rows with distinct 8 KiB source bodies (81,920,000 source bytes), constructs the app at width 80, applies an 80×24 window size, renders `View()`, and runs GC. `projection-retained` is the `HeapAlloc` delta from before app construction to after app initialization and render; `total-heap` is the post-GC `HeapAlloc`. The input snapshot is present before the baseline sample, so its canonical source bytes are included in total heap but excluded from the projection delta.

Command for both fixed workloads:

```sh
go test ./internal/tui -run '^$' -bench 'BenchmarkBubbleTeaTranscriptUpdateView|BenchmarkBubbleTeaLongTranscriptProjectionRetainedHeap' -benchtime=1x -count=1
```

## Results

The fixed 10,000×8 KiB projection retains 12,530,848 B after the change, down from 106,365,104 B at the baseline (93,834,256 B, or about 88%, less). Total post-GC heap fell from 195,202,128 B to 101,362,776 B. The input transcript remains 81,920,000 source bytes in both measurements.

| Loaded history | Baseline p50 / p95 | New p50 / p95 | Baseline alloc/chunk | New alloc/chunk | New retained history heap |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1,000 rows | 14 / 20 µs | 15 / 22 µs | 21,927 B | 21,927 B | 4,277,112 B |
| 5,000 rows | 14 / 18 µs | 14 / 21 µs | 22,016 B | 22,016 B | 9,488,856 B |
| 10,000 rows | 14 / 16 µs | 15 / 18 µs | 22,114 B | 22,114 B | 16,011,016 B |

Long-source heap measurements:

| Source | Projection-retained heap | Total post-GC heap | Allocated bytes/op |
| --- | ---: | ---: | ---: |
| Baseline `36292cc7ce3f797fd7915838a3456fc754db32e7` | 106,365,104 B | 195,202,128 B | 753,823,168 B |
| Optimized `587be07e6d0961e72bf7261fb6148b0caae8f963` | 12,530,848 B | 101,362,776 B | 730,571,344 B |

The change keeps canonical `searchText` complete, stores safe plain-text display lines as slices into the already-loaded source, and renders wrapped canonical fragments only when they enter the viewport. Rows containing ANSI/control sequences keep the sanitized body together so style state remains correct across source newlines. Lazy wrapped-line offsets are derived from display source lines when scrolling or restoring an anchor, so a long canonical row remains correctly anchored after resize. The semantic tests cover full-source search, bounded-preview marker and tail label, final source line visibility, shared display/search source, ANSI state across lines, and in-row anchor preservation. `go test -race ./internal/tui -count=1` passed all 608 tests at the optimized source snapshot.

## Native terminal status

Native PTY interaction is captured separately at [`r8-native-pty-587be07`](../r8-native-pty-587be07/README.md). The valid active-run journey scrolled with PageUp and resized at 60/80/120/200 columns while preserving the visible body anchor; End was sent before stream completion, and the final committed frame then showed the newest answer. This is separate from the synthetic app benchmark above. R8-BLD-02 capability/help/doctor/restart promises were not exercised or claimed.
