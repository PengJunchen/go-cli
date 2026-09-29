# Directory capture and Find backend repair

## Scope

The worktree starts at `17ab7b7` on `codex/repair-20260929-directory`. Commit `227f2b8` independently fixes the two directory-capture P2s. This report accompanies the scoped Find backend follow-up commit.

The capture writer now parses helper output as NUL-delimited records, drops only empty records, and keeps LF, whitespace-only and non-UTF-8 names as raw bytes through sorting and budget accounting. Read, Ls and Find quote exceptional names reversibly when displaying them. Directory scanning checks cancellation again as soon as `ReadDir` returns, including EOF and empty-batch cases.

Find now explicitly requests glob, case-sensitive matching, hidden paths, and ignored paths from fd, matching the tool's glob description and the Go/system-find behavior. The fd command keeps its existing `--` separator before the pattern and validated root. The system-find backend omits a directory root with `-mindepth 1`; file roots bypass directory-oriented helpers and use the Go matcher so they remain searchable. An fd exit status of 1 is treated as helper failure and falls through to system find/Go.

The path whitelist check still runs in `Execute` before helper dispatch. No grep, managed-owner, symlink or type-filter contract was changed.

## TDD evidence

For the P2s, the new cancellation barrier test first reproduced EOF swallowing a cancellation, while NUL/raw-name tests exposed record splitting and whitespace trimming. The fixes made those tests pass. Existing capture-writer tests were updated to supply NUL-framed helper output; their record-budget and sorting assertions remain intact.

For the backend follow-up, tests run against the P2-only commit first exposed the fd regex/glob argument mismatch, system find returning a matching directory root, and fd exit 1 being misclassified as a valid empty result. The exact-argv tests also preserve the baseline `--` injection guard. After implementation, the focused backend checks and the complete tools race suite passed.

The local machine has no `fd` executable (`rtk which fd` returned no path). PATH shims therefore assert the complete argv and route the public-API fixture through the real system `find`; the separate optional real-fd no-side-effect test skips when fd is unavailable. Real system-find and forced-Go fixtures compare case sensitivity, hidden names, `.gitignore` matches, directory roots and file roots.

## fd contract evidence

The official [fd man page](https://github.com/sharkdp/fd/blob/master/doc/fd.1) says regex is the default, `--glob` selects glob matching, `--case-sensitive` disables smart-case, `--hidden` includes hidden entries, `--no-ignore` disables ignore filtering, and `--print0` separates results with NUL. Its [changelog](https://github.com/sharkdp/fd/blob/master/CHANGELOG.md) records `--glob` as introduced in v7.4.0; older binaries that reject this option use the existing backend fallback.

The [fd exit-code source](https://github.com/sharkdp/fd/blob/master/src/exit_codes.rs) maps `HasResults(false)` to 1, while ordinary successful walking returns success when there are no errors. The man page documents the conditional 0/1 result status under `--quiet`/`--has-results`; this invocation does not use those flags, so exit 1 is not interpreted as “no matches.” The [walk implementation](https://github.com/sharkdp/fd/blob/master/src/walk.rs) explicitly skips the root directory entry.

## Commands and results

RED before the backend implementation:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -p 2 -count=1 -run '^(TestFindGlobPatternMatchesSameEntriesAcrossBackends|TestFindSystemFindExcludesDirectoryRootAndFileRootIsIncluded|TestFindFDZeroExitWithNoResultsReturnsEmptyResult|TestFindFDExitOneFallsBackAsHelperError|TestFindFDArgvKeepsDashPatternsAfterOptionSeparator)$' ./internal/tools
```

The new assertions failed on the P2-only commit: fd did not receive `--glob`, system find emitted the directory root, and exit 1 prevented fallback. Those failures were corrected in the backend implementation.

GREEN focused verification:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -p 2 -count=1 -run '^(TestFindGlobPatternMatchesSameEntriesAcrossBackends|TestFindSeparatesPatternFromFdOptions)$' ./internal/tools
ok github.com/pengjunchen/go-cli/internal/tools 0.029s
```

Final package race verification:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tools
ok github.com/pengjunchen/go-cli/internal/tools 44.968s
```

Other final checks:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go vet ./internal/tools
# exit 0

rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb make lint
golangci-lint run ./...
0 issues.

rtk git diff --check
# exit 0
```

An earlier full race attempt failed because legacy tests still supplied LF-delimited capture chunks and asserted the pre-`--print0` fd argv. The fixtures now use NUL framing and the argv assertions include the active options; the complete package race run above is the final result.
