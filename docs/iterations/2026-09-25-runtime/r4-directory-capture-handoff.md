# R4 bounded directory capture handoff

**Design snapshot:** main `b98319f368e970a14c5e331cefb2d0aa1c92f38a`.
**Source contract:** `r4-remaining-source-contract.md`, R4-BLD-02 capture paragraph.
**Scope:** Read directory listings, Ls, and Find result capture only. This is a bounded-source-capture design, not an R4 completion claim. It does not change regular-file Read, Grep, RemoteBash, approval, or path-whitelist policy.

## Source findings

| Path | Current behavior | Memory still unbounded before return |
| --- | --- | --- |
| `internal/tools/read.go:158` | Directory names come from `os.ReadDir`, are rendered in filename order, and `Metadata["entries"]` is the number listed. `defaultReadMaxBytes` applies only to regular files. | `os.ReadDir` materializes the entire directory; the output `strings.Builder` then materializes the entire listing. |
| `internal/tools/ls.go:135` | `filepath.WalkDir` filters to immediate children, supports dotfiles, long format, and stable name/time/size sorting. `Metadata["entries"]` is the listed-entry count. | `WalkDir` reads and sorts the whole directory before callbacks; `entries` and the formatted output also grow with the directory. |
| `internal/tools/find.go:159,282` | Find tries `fd`, then system `find`, then the pure-Go walker. `Execute` sorts every path, keeps the first `findMaxResults` (1,000), and reports the retained count in `Metadata["matches"]`. | All Go matches are retained before sort/truncation. `filepath.WalkDir` also materializes each directory's entries. The external route captures all stdout in `bytes.Buffer`, splits every line, and only then sorts/truncates. |
| `internal/tools/managed_search_command.go:14` | The current helper delegates process-group ownership, pipe draining, cancellation, and reader joining to `managedLocalCommand.collect`. It captures stdout into a `bytes.Buffer`; stderr is drained and discarded. | Using it unchanged for Find keeps the unbounded byte buffer even if later slices are limited. |

`ReadTool.resolvePath`, `LSTool.Execute`, and `FindTool.Execute` already resolve and validate the path before listing/searching. Keep those calls and their whitelist checks before any directory open or external helper dispatch. The path whitelist is still an application-level boundary with the existing check/use race, not an OS sandbox.

The current result ordering is observable: Read is filename-ascending; Ls name-sort is ascending, time-sort is newest-first, and size-sort is smallest-first. `filepath.WalkDir` visits lexical names, so stable ties in Ls resolve lexically. Find sorts the complete match set lexically before applying its 1,000-result cap. The cap must select the same sorted prefix; stopping at the first 1,000 traversal hits would change results.

One adjacent source-contract mismatch should stay visible: the R4 contract says to retain Grep's bounded capture behavior, but `grepWithRipgrepManaged` currently goes through the same unbounded stdout `bytes.Buffer` before `truncateMatches`. This slice must not silently change Grep. Keep its process cancellation/join and visible result behavior untouched; any claim that Grep's intermediate process output is source-bounded needs its own follow-up.

## Recommended contract

Use private defaults shared by directory-oriented tools:

```go
const directoryCaptureMaxEntries = 1000
const directoryCaptureMaxOutputBytes = 1 << 20
const directoryCaptureMaxEntryBytes = 4 << 10
const directoryReadBatchSize = 128
```

Keep Find's existing 1,000-item limit. Apply the same item cap to Read and Ls, whose current output has no cap. The 1 MiB output limit matches the existing Read-file and Grep result-size convention. The 4 KiB per-entry key/path guard bounds the retained top-K strings to at most about 4 MiB; an unrepresentable longer Find path is a typed capture-limit error, not a silently shortened path. The 4 KiB path guard is an explicit compatibility tradeoff for very long Windows extended paths; if those paths must be supported, choose a larger per-entry cap and accept the corresponding worst-case `maxEntries × maxEntryBytes` retention budget before implementation.

Use a small private bounded top-K accumulator, parameterized by a strict ordering function. It retains at most 1,000 candidates, replacing the current worst candidate when an earlier candidate arrives, then sorts only the retained slice. Track truncation with a saturating count/flag; do not accumulate a total-results slice. Scan all candidates because the traversal order is not the output order. The algorithm is `O(N log K)` time and `O(K × maxEntryBytes)` candidate memory, plus one 1 MiB formatter and one 128-entry directory batch (`K=1000`).

For Ls, use a comparator that reproduces today's stable behavior explicitly: name ascending; modification time descending with path ascending on ties; size ascending with path ascending on ties. The lexical tie-break preserves the current `WalkDir` stable-sort result without retaining the whole directory. Format only the retained sorted candidates, adding complete lines while the byte budget allows; never cut a filename or long-format record in the middle.

For Find, feed pure-Go and external results into the same lexical top-K accumulator. A dedicated streaming stdout writer should parse helper output one line at a time, retain at most one bounded line plus the top-K paths, and continue draining stdout after the retention cap is reached. Do not route Find through an all-output `bytes.Buffer` and do not terminate the helper just because the capture is full. Preserve current LF line splitting/whitespace-only filtering for external `fd`/`find` output. If one line exceeds the per-entry guard, remember a typed capture error, discard the rest of that line, continue draining, and return the error only after the managed process and readers join; do not fall back to another backend for a capture-limit error.

Replace `filepath.WalkDir` in the Go Find path with a chunked depth-first walker using `os.File.ReadDir(directoryReadBatchSize)`. `WalkDir` itself reads/sorts a full directory, so limiting only the match slice does not solve a very wide single directory. The chunked walker retains only the current batch, the bounded match accumulator, and a depth stack; it must preserve `max_depth`, root-file handling, pattern/type filters, and the current non-following behavior for symlink entries. A depth-first implementation holds one directory handle per active depth; propagate open/read errors and close every opened handle on error or cancellation. Keep the existing unlimited `max_depth=0` behavior; do not silently add a traversal-depth cap. This has an explicit deep-tree file-descriptor tradeoff, noted below.

Keep metadata backward-compatible for complete results: `entries`/`matches` continue to count returned records, and the existing `path`, `pattern`, and `sort` fields stay unchanged. Add an always-present boolean `truncated` to directory results and the corresponding span/log fields. It is true if the entry cap or output-byte cap omitted a complete record; otherwise false. Do not add a text marker to path-list output because it would look like a real filename. Do not add a scan-total field in this slice.

Cancellation remains synchronous for the Go walkers: check `ctx.Err()` between batches and entries and close open directory handles before returning the context error. The external Find path must continue using the existing `startManagedLocalCommand`/`collect` ownership path, including process-group containment, pipe draining, cleanup escalation, and pending completion proof. Collector saturation is not cancellation and must not release ownership early. If context cancellation or unconfirmed process cleanup occurs, preserve the existing no-fallback decision and `PendingExecutionError` semantics.

## Minimal implementation surface

- New `internal/tools/directory_capture.go`: bounded top-K and bounded line/output capture helpers; no exported configuration API.
- `internal/tools/read.go`: replace only the directory branch's `os.ReadDir` plus full builder with chunked reads, top-K selection, and bounded formatting.
- `internal/tools/ls.go`: replace the immediate-child `WalkDir`/full slice with chunked `ReadDir`, the explicit stable-order comparator, and bounded formatting.
- `internal/tools/find.go`: apply the accumulator in the Go walker and helper routes, then build only bounded output.
- `internal/tools/managed_search_command.go`: factor the existing process-owner path into a writer-targeted internal helper; retain `runManagedSearchCommand`'s current `bytes.Buffer` behavior for Grep and other callers. The Find writer must remain synchronous inside `managed.collect` so the existing owner still joins it.
- Direct tests in `read_test.go`, `find_ls_test.go`, `path_security_test.go`, and the existing platform-specific `search_helpers_lifecycle_*_test.go`. Avoid changing prepared registry, assembly, approval, or Grep source.

## TDD acceptance

First add failing tests against the current allocations, then implement the bounded collector and connect each source:

1. Unit-test top-K ordering, stable Ls ties, entry/byte budgets, and whole-line truncation using a synthetic incremental `ReadDir` source and an adversarial line writer. Assert retained item and key-byte counts never exceed the configured bounds; do not use heap-size timing assertions.
2. Create a directory with more than 1,000 names. Read must return the lexical first 1,000; Ls must return the first 1,000 for name/time/size order with lexical ties. Assert exact output prefix, returned `entries`, and `truncated=true`; under-limit fixtures must retain their current output and metadata with `truncated=false`.
3. Force pure-Go Find over a wide/nested tree with more than 1,000 matches. Assert the result is the globally lexical first 1,000—not the first traversal hits—plus `matches=1000` and `truncated=true`. Include `max_depth`, `type`, root-file, and symlink cases to prove those filters do not drift.
4. Use PATH-local `fd` and `find` shims that emit unsorted output beyond the result cap. Verify both routes yield the same sorted prefix, consume large stdout without retaining it, and treat overlong lines as a capture error only after process completion. Ensure the shim records dispatch so an outside/escaping AllowedPaths request proves rejection happens before invoking it.
5. Cancel a helper while it is producing output and while a descendant holds an inherited pipe. Assert the tool returns only after the existing managed-process join, or returns a pending error with a live completion signal that closes only after the process group/readers stop; it must not fall through to another backend while completion is unknown. Preserve the current helper descendant leak tests and add the case against the new streaming collector.
6. Run the relevant file-tool, process-scope, and assembly path tests, then the affected race gates:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tools
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/approval ./internal/core
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/cli -run '^TestAssembleAgentConfiguredPathsConstrainDefaultFileTools$'
```

## Decisions and limits for the reviewer

- The 4 KiB per-entry path bound rejects unusually long paths. Confirm this tradeoff or select a platform-compatible larger bound before implementation; do not truncate the path text itself.
- Chunked depth-first Find holds open directory handles proportional to active traversal depth. It preserves unlimited `max_depth` and bounded width memory, but a very deep tree can hit the OS open-file limit; all handles must close before return. Avoid solving this with a silent depth change, a full sibling list, or an unbounded directory queue. If the project requires constant open descriptors and unlimited depth, that needs a separate portable directory-cursor/spill design.
- Full traversal remains necessary to guarantee the old globally sorted top-1,000 Find results and Ls sort order. Limits bound retention and output, not total entries visited or elapsed time; callers must use context cancellation to stop expensive trees.
- The R4-BLD-02 dependency on `R3-REV-01` remains in the task graph. This directory-capture slice can be implemented and tested locally but cannot close R4 while that dependency or the remaining schema/effect/entrypoint gates are pending.
