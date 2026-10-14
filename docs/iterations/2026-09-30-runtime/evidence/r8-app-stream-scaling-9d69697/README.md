# R8 app streaming hot-path evidence

Candidate source commit: `10fa50d0dcfdf7c63009d3d30eff3b7c9cce6792`, on top of `9d696978850fca7acda1c7af81a72bdaf028d685`. The implementation bounds legacy `teaModel.handleIncremental` work to an incremental parser and visible terminal tail while retaining the complete source for final Markdown rendering. It keeps the truncation marker on its own line and limits post-`done` duplicate suppression to the immediately following complete message. Any intervening event clears the pending digest. The canonical transcript viewport is unchanged.

## App-level benchmark

The benchmark drives 256 KiB as 8,192 32-byte assistant events through real `teaModel.Update` and `teaModel.View` calls at width 80. The old path copied and rendered the growing full response on each update. The bounded path appends each chunk once, serializes at most the visible body tail, and performs a full Markdown render on completion.

`benchmark-before.txt` records the baseline source `1354874f413cda7caa902c43121df13e94b922a5`, measured once with `-benchtime=1x`. `benchmark-after.txt` records three one-operation samples from candidate `10fa50d`, using the same benchmark, `-benchtime=1x -count=3`, and `-benchmem`. Both runs used Go 1.26.4 on Darwin/arm64 (Apple M5).

| Measurement | Baseline | Candidate, median | Approximate change |
|---|---:|---:|---:|
| One 256 KiB stream | 17.355 s | 10.232 ms | 1,696x faster |
| Allocated bytes | 25,127,936,328 B | 56,441,936 B | 445x lower |
| Allocations | 41,026,361 | 132,944 | 309x fewer |

These one-operation samples show the per-chunk improvement; they do not imply completed legacy history has session-size-independent memory use. Completed messages still retain their full rendered content in the accordion.

## Correctness and race checks

The four focused streaming regressions passed: bounded preview and full final Markdown, `done` finalization, preservation of distinct final content, and an identical later assistant after an intervening tool result. The truncation marker is asserted to have its own line before and after resize.

`go test -race ./internal/tui -count=1` passed all 625 tests. The focused command was:

```text
go test ./internal/tui -run '^(TestIncrementalAssistantPreviewIsBoundedAndFinalMessageKeepsFullMarkdown|TestDoneFinalizesStreamingAssistantWithoutLosingFullText|TestFinalAssistantMessageDifferentFromStreamIsNotDropped|TestDoneDigestDoesNotSuppressLaterIdenticalAssistantAfterInterveningEvent)$' -count=1
```

## Native PTY regression journey

The CLI binary built from candidate `10fa50d` had SHA-256 `d2a242182ff45c808748e2e1d0a811092b291cf748f71f1ef75de130c3e48149`. The native Darwin `openpty` journey passed with 26 authenticated streaming requests and one Read receipt. PageUp preserved the prior-row anchor across 60, 80, 120, and 200-column resizes; wide layouts showed the tool pane; `/exit` returned 0 and restored ICANON, ECHO, and ISIG.

While the provider still held the active stream, pressing End did not immediately reveal the latest appended answer. The answer became visible after the stream completed. This records the existing canonical viewport behavior; the bounded legacy-preview change does not modify that path. Fixed-width PTY frames and the raw transcript are local review artifacts and are excluded from this docs commit because their intentional terminal padding fails `git diff --check`.
