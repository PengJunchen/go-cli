# R4 directory-capture implementation review

## Verdict

Reject frozen implementation `ebfc9900cb1d5fd7313944553220f96994e910fc` for two P2 correctness issues: path-record/display corruption and cancellation swallowed during directory reads. The bounded top-K, whole-record byte budgets, pure-Go depth-first cleanup, whitelist-before-helper ordering, and managed-process drain/join behavior passed the source review and affected race gates below. Grep's separate unbounded intermediate capture remains open and is not claimed fixed.

## Snapshot and scope

- Reviewed target: `ebfc9900cb1d5fd7313944553220f96994e910fc` in detached worktree `r4-directory-capture-review-ebfc990`.
- Design snapshot: `84dd36e`'s `r4-directory-capture-handoff.md`; preceding design review: `reviews/r4-directory-capture-design-review.md`.
- Product diff: `84dd36e..ebfc990`, nine files limited to `internal/tools` directory capture and direct tests. No product files were changed in this review; temporary probe tests were removed.
- Review does not claim R4 phase completion, schema/effect completeness, R2 production wiring, Grep capture bounds, or Linux runtime validation.

## Findings

### P2 — Valid filenames containing line breaks or only whitespace are lost or split into false Find results

`findPathCaptureWriter.Write` treats LF as a record boundary (`internal/tools/directory_capture.go:219-245`), and `captureLineLocked` drops every line for which `strings.TrimSpace(line) == ""` (`:247-255`). `findWithFdManaged` and `findWithFindManaged` do not request NUL-delimited output (`internal/tools/find.go:221-239,264-285`). Since filesystem names may contain LF or consist entirely of whitespace, external Find can turn one real path into multiple fake candidates or silently omit it. Read and Ls likewise append raw names to newline-delimited output (`read.go:188-212`, `ls.go:180-222`), so a name containing LF renders as multiple apparent entries; whitespace-only and non-UTF-8 names are not reversibly distinguishable in the display.

Temporary real-tool probes in the detached review worktree reproduced both cases, then were removed:

- A file named `odd\nname`, returned by a local `fd` shim, produced external Find `matches=2` and output `"name\nodd"`; forced pure-Go Find produced `matches=1` and output `"odd\nname"`.
- A file named three spaces produced external Find `matches=0` and empty output; forced pure-Go Find produced `matches=1` and output containing the three spaces.

Probe command and result:

```text
rtk env GOMAXPROCS=2 go test -p 2 -count=1 -run '^TestReviewProbeFindExternal(NewlineFilenameSplitsRecord|WhitespaceFilenameIsDropped)$' -v ./internal/tools
PASS; newline case logged external matches=2 vs Go matches=1; whitespace case logged external matches=0 vs Go matches=1.
```

Use NUL framing for helper capture (`fd --print0`, system `find -print0`) and parse NUL records, ignoring only an actually empty record. For visible Read/Ls/Find text, keep ordinary names unchanged but reversibly Go-quote names containing control characters/LF, invalid UTF-8, or only whitespace. Sort and count raw path values; encode only for display and charge the encoded whole record against the output-byte budget. Add helper and public-tool tests proving no split/loss, accurate metadata, and identical rendering across backends. Approval to use this narrow format correction was given, but no fix was present at this review freeze; record this as unresolved.

### P2 — Read/Ls can report success when cancellation occurs during the final ReadDir call

`scanDirectoryBatches` checks `ctx.Err()` before `ReadDir`, then processes returned entries and returns immediately when `ReadDir` reports EOF (`internal/tools/directory_capture.go:175-192`). It does not recheck the context after the call when the result is empty. A deterministic probe using the existing `directoryBatchReader` seam canceled the context inside `ReadDir` and returned `nil, io.EOF`; the scanner returned `nil` instead of an error. Read and Ls share this scanner, so the cancellation can be swallowed on an empty/final batch.

Probe command and result:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -count=1 -run '^TestReviewProbeScanDirectoryDoesNotSwallowCancellationDuringRead$' ./internal/tools
FAIL as expected: expected context canceled in the error chain, received nil.
```

Recheck `ctx.Err()` immediately after each `ReadDir` call and before interpreting its entries, EOF, or error. This does not make `os.File.ReadDir` interruptible; it ensures a cancellation that occurs during the call cannot be mistaken for a successful empty listing. The probe was temporary and removed after detection; this remains unresolved at the reviewed snapshot.

## Confirmed behavior at this snapshot

- The top-K accumulator retains no more than 1,000 keys, enforces the 4 KiB per-key byte limit, and sorts the retained candidates. Read/Ls share a 1 MiB output writer that counts each record plus its delimiter and never emits a partial record. The retained prefix and truncation metadata are covered by the direct tests.
- Read and Ls call `os.File.ReadDir(128)` through `scanDirectoryBatches`. Find uses `ReadDir(1)` in its DFS, so there is no pending 128-entry suffix retained at each active depth. The walker keeps one open directory per active depth, closes popped directories, and defers closing every remaining handle on error/cancellation. This preserves bounded directory width but keeps the documented depth-proportional descriptor tradeoff.
- Root open/read failures in Find return an error; unreadable descendants and entry metadata failures are skipped while setting `truncated=true`. Read/Ls propagate root listing and close errors. Context is checked before reads and between entries; the Find walker also checks after each read and before/after opening a child. However, the shared Read/Ls scanner can swallow cancellation on an empty/final batch as described above.
- Path resolution and whitelist validation happen before `FindTool.find` performs `LookPath` or dispatches a helper. `TestFindRejectsOutsideWhitelistBeforeHelperDispatch` uses local `fd`/`find` markers to verify the rejection.
- The Find capture writer latches an overlong-record error, discards the rest while returning the full write count, and continues draining. The Find path calls `Finish` and exposes candidates only after the managed lifecycle reports joined readers and known cleanup. Capture-limit errors, pending execution, or unconfirmed containment return empty output and stop fallback. Existing tests cover a helper completion marker, descendant/escaped-writer containment, and no fallback on an unconfirmed owner.
- Go path strings retain their bytes for sorting and candidate limits; the missing reversible display encoding in the finding above is the remaining path/UTF-8 issue. Entry symlinks are not followed by `DirEntry.Info().IsDir()`; path resolution continues to reject a symlink as the final requested path. Grep still goes through the legacy `bytes.Buffer` route and retains its separate unbounded intermediate-capture gap.
- An additional temporary nested-tree probe passed: after DFS visited `a/z...` before the lexically earlier root sibling `a.txt`, the global top-K output still placed `a.txt` before `a/...` and retained the expected first 1,000 records. The probe was removed.

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 -run '^TestReviewProbeFindNestedTraversalStillReturnsGlobalLexicalPrefix$' ./internal/tools
ok github.com/pengjunchen/go-cli/internal/tools 1.120s
```

## Test discovery and validation

The targeted test names were listed before execution with:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -list '^(TestBoundedTopK|TestBoundedDirectoryOutput|TestScanDirectoryBatches|TestFindPathCaptureWriter|TestReadDirectory|TestFindGoReturns|TestFindManagedBackends|TestFindCaptureLimit|TestFindOutputByteLimit|TestFindRejectsOutsideWhitelist|TestFindPureGo|TestLS)' ./internal/tools
```

The discovery output included the top-K, batch-cancellation, Read directory-prefix, Find Go/backend/capture/whitelist/depth/symlink, and Ls sorting/truncation tests. The CLI configured-path target was listed before running as well.

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tools
```

Result: `ok github.com/pengjunchen/go-cli/internal/tools 43.219s`.

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/approval ./internal/core
```

Result: approval passed in 1.990s; core passed in 5.003s.

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/cli -run '^TestAssembleAgentConfiguredPathsConstrainDefaultFileTools$'
```

Result: `ok github.com/pengjunchen/go-cli/internal/cli 1.095s`.

The temporary nested global-top-K probe passed under race. `git diff --check 84dd36e ebfc990` passed. These were Darwin runs only.
