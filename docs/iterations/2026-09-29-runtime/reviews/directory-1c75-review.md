# Directory combined patch review: `1c75ec2`

## Verdict

**Accept this scoped directory-capture and Find backend combination.** The combined patch passes the listed `internal/tools` test, race, vet, and diff gates, and fixes the earlier C3 rejection reason. One independent `type: f` symlink parity gap remains as a follow-up P2; it predates this slice and is outside the NUL-capture/backend changes reviewed here. R4 is therefore not fully closed.

## Snapshot and scope

- Reviewed target: `1c75ec2f9dfb1c1611746ea3bca05f1039a46384`.
- Base: `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`.
- Target's actual parent: `227f2b84bf314b15b0129b1728e408995e7ac84d`; the review request stated `227f2b85`.
- Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-directory-review-1c75`, detached at the target SHA. The worktree was clean after removing temporary review probes; no product files were changed.
- Reviewed the root `directory-c3-review.md`, target `directory-builder.md`, and the prior R4 design/implementation reviews. This review covers the full base-to-target combination, not the previously rejected C3 snapshot.

## Finding

### [P2 follow-up, outside this slice] Go `type: f` results include symlinks that helper backends exclude

`findKindOK` treats every non-directory as a file (`internal/tools/find.go:451-455`). In the Go walker, `DirEntry.Info().IsDir()` is false for a symlink to a file, so a directory search with `type: "f"` can return that symlink. The system helper's `-type f` filter excludes it. The official `fd` man page likewise defines `--type f` as regular files and treats symlinks as a separate `l` type ([fd manual](https://github.com/sharkdp/fd/blob/master/doc/fd.1#L1767-L1815)). This is an existing kind-policy discrepancy, not a regression introduced by the reviewed NUL capture patch; the target keeps the existing policy for this slice.

A temporary review-only test created `alias.txt` as a symlink to a regular file. With `pattern: "alias.txt", type: "f"`, forced Go returned `alias.txt`; forcing the system-find route by making the fd shim exit 1 returned no matches. The probe file was removed. This behavior predates the target commit; the new non-directory-root shortcut at `find.go:181` uses the same existing predicate when routing non-directory roots to Go.

Keep the separate follow-up narrow: distinguish regular-file entries from other non-directory entries in `type: "f"` filtering and cover both child symlinks and the single-path branch. There is no need to redesign general symlink traversal for this finding. This parity gap means R4 is not fully closed, but it does not block acceptance of this scoped capture/backend slice.

## Verified behavior

- The capture writer now parses NUL records, accepts a final unterminated record, ignores only empty records, and preserves raw LF, whitespace-only, and invalid-UTF-8 bytes for sorting and the candidate-byte limit. Tests cover records split across writes and the former C3 LF/argv expectations.
- Read, Ls, and Find quote newline/control, invalid UTF-8, and whitespace-only names for display. Ordering and candidate limits use raw paths; display encoding occurs before whole-record output budgeting. A temporary fake-fd probe with 1,000 records of 3,000 control bytes each produced 87 complete encoded records and `truncated: true`, confirming the byte cap charges the quoted output. Probe artifacts were removed.
- The shared `ReadDir` scanner rechecks context immediately after `ReadDir`, so cancellation returned with empty EOF is surfaced. The deterministic EOF cancellation test passes.
- Managed helper output is synchronously drained by the shared process owner; capture `Finish` and fallback occur only after its readers are joined. The saturation marker test and descendant/process-group lifecycle test pass. A temporary fake-fd probe confirmed an ordinary exit 1 falls through to Go when `find` is absent.
- The fd shim assertions verify `--print0 --glob --case-sensitive --hidden --no-ignore`, retain the `--` separator before the model pattern, and verify pattern/type placement. The system-find fixtures verify `-print0` and `-mindepth 1` for directory roots. A regular single-file root returns the same absolute path via the new Go branch and forced Go; a matching directory root is omitted by system find and Go.
- Directory-root output prefixes remain backend-specific: system find emits absolute paths while the Go walker emits paths relative to its root. The tests compare basenames where they compare backend membership and separately assert each route's current rendering; the target does not normalize those strings.
- The target report says no type-filter or symlink contract changed. The P2 follow-up above is the one concrete remaining type/symlink discrepancy found in the reviewed boundary; it is not attributed to this slice. No Grep implementation or CLI gate was reviewed or changed here.

## Validation

Test discovery enumerated the relevant `TestFind*`, `TestReadDirectory*`, `TestLS*`, bounded-output, capture-writer, and cancellation tests:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -list '^(Test(BoundedTopK.*|BoundedDirectoryOutput.*|ScanDirectory.*|FindPathCaptureWriter.*|DirectoryToolsRender.*|Find.*|ReadDirectory.*|LS.*))$' ./internal/tools
```

The three assertions called out by the prior C3 rejection now pass:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -p 2 -count=1 -run '^(TestFindPathCaptureWriterDrainsAndReportsCaptureLimit|TestFindPathCaptureWriterKeepsSortedTopKAcrossChunks|TestFindSeparatesPatternFromFdOptions)$' -v ./internal/tools
PASS; all three tests passed.
```

Full package race gate:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tools
ok github.com/pengjunchen/go-cli/internal/tools 44.564s
```

Other checks:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go vet ./internal/tools
# exit 0

rtk git diff --check 17ab7b76ef0b7636abff1e5a055de7acf6f6bb31 1c75ec2f9dfb1c1611746ea3bca05f1039a46384
# exit 0
```

`fd` is not installed (`rtk which fd` exited 1). `TestFindAvailableFDDoesNotExecuteDashPattern` was explicitly run and skipped with `fd is not installed`; the fd behavior evidence is from argv shims and the official manual, not a real fd binary. Real system `find` tests ran in the full package race gate.
