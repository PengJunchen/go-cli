# Directory capture review: `c3e5848`

## Verdict

**Reject as submitted.** The NUL-record implementation and the targeted lifecycle/cancellation behavior passed independent checks, but three existing assertions still encode the old LF protocol or old `fd` argument vector. The required `internal/tools` test gate fails on the exact c3 snapshot, so this commit is not independently acceptable until a follow-up updates those tests and passes the gate.

## Reviewed snapshot and boundary

- Commit: `c3e584872c0b4406a4924575bd15c6d045202526` (`fix(tools): preserve directory capture records`), parent `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`.
- `main` at review time: `40692e35e4815f0dff1c9f91b13905008f7221a`, an ancestor of c3.
- Fixed detached worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-directory-review-c3`; it was clean at the reviewed SHA after removing the temporary probe.
- Scope is only c3 relative to its parent. Later directory/backend work was not reviewed.

## Finding

### [P1] The changed record protocol leaves three package tests stale

The capture writer now splits only on NUL and passes `--print0` to `fd`, but three checked-in expectations still describe the former LF protocol/argv:

- `internal/tools/directory_capture_test.go:123` writes `"ok\n"` and expects the captured record `"ok"`. Under the c3 parser, LF is part of the unfinished path; the later overlong record latches the capture error and clears that path, so the result is empty.
- `internal/tools/directory_capture_test.go:147-156` sends `{"z\nb", "\na\n"}` and expects two LF-delimited candidates and truncation. With NUL framing, that is one path, `"z\nb\na\n"`, and no top-K truncation.
- `internal/tools/read_tool_safety_test.go:71,78` expects `fd` argv without the new leading `--print0`; both exact-argv assertions fail.

These are stale test fixtures/expectations, not evidence that the new NUL parser itself mishandles valid NUL records. They nevertheless make the package gate red. The follow-up should use NUL-terminated records (including records split across writes) and update the expected `fd` argv; the production code was not changed during this review.

## Verification

The package test list was enumerated with:

```text
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -list 'Test(FindPathCapture|ScanDirectory|DirectoryToolsRender|Find(FDShim|SystemFind|CaptureLimit|OutputByteLimit|RejectsOutsideWhitelist|PureGo)|ReadDirectory|LS)' ./internal/tools
```

The full required package race gate was run on c3:

```text
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tools
```

It failed on the three stale expectations above: `TestFindPathCaptureWriterDrainsAndReportsCaptureLimit`, `TestFindPathCaptureWriterKeepsSortedTopKAcrossChunks`, and `TestFindSeparatesPatternFromFdOptions`.

They were reproduced directly with:

```text
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -count=1 -run '^(TestFindPathCaptureWriterDrainsAndReportsCaptureLimit|TestFindPathCaptureWriterKeepsSortedTopKAcrossChunks|TestFindSeparatesPatternFromFdOptions)$' -v ./internal/tools
```

That command fails at the same assertions. `go vet` and whitespace validation passed:

```text
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go vet ./internal/tools
rtk git diff --check 17ab7b76ef0b7636abff1e5a055de7acf6f6bb31 c3e584872c0b4406a4924575bd15c6d045202526
```

Both exited successfully.

An independent temporary probe, removed after execution, also passed for:

- NUL records split across writes, empty NUL records, raw LF/whitespace paths, and the raw path-byte cap;
- reversible `strconv.Quote` display and output-budget accounting against encoded bytes;
- cancellation raised inside `ReadDir` when it returns empty `io.EOF`;
- real system `find -print0` round-trip for newline, whitespace-only, tab, and ordinary names, including a second `NewFindTool` execution; managed lifecycle reported `readersJoined`, and `/dev/fd` name count did not grow beyond the probe's one-descriptor tolerance;
- capture saturation: the fake `fd` reached its completion marker, returned the typed cap error with empty output, and did not invoke the fake `find` fallback.

The committed `TestFindFDShimUsesNULRecordsForUnusualNames`, `TestFindSystemFindUsesNULRecordsForUnusualNames`, and `TestFindCaptureLimitDrainsHelperAndStopsFallback` also cover the shim, real-system-find, and drain/no-fallback paths. The unchanged managed command owner still drains stdout/stderr and joins readers before the capture error is returned; lifecycle uncertainty remains terminal through `shouldFallbackAfterManagedHelper`.

## Contract notes

- `directoryDisplayName` is applied before `boundedDirectoryOutput.AppendLine`, so its byte budget charges the actual encoded display record. Metadata uses `output.Records()` and therefore counts only returned whole records.
- The post-`ReadDir` context check catches cancellation even when the read returns empty EOF; cancellation is checked before entries as well.
- Existing Find tests and the earlier handoff explicitly require globally lexical Find top-K output. `ReadDir(1)` discovery order itself is filesystem order, but c3's bounded top-K still sorts retained paths and scans all candidates, preserving the lexical top-K prefix independently of discovery order. The intentional backend-traversal-order caveat applies to the separate Grep design, where the result prefix is retained directly in traversal/event order.
- This review does not treat separate root, exit-status, or glob behavior as fixed by c3, and does not review the later backend WIP.
