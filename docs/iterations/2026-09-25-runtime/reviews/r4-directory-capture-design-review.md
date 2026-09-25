# R4 directory capture design review

## Verdict

Accept the bounded-capture direction with a few implementation-contract clarifications. The proposed 1,000-record, 1 MiB output, and 4 KiB candidate limits are consistent with the reviewed R4 capture requirement and the explicit tradeoffs approved for this slice. Grep's unbounded intermediate output remains a separate gap and is not closed here.

## Reviewed snapshot and evidence

- Design: `r4-directory-capture-handoff.md` at `64f8e673e4798d033b9c25bdf7e502f2e184acfa`.
- Source contract: R4-BLD-02 capture paragraph in `r4-remaining-source-contract.md`.
- Source inspected: `internal/tools/read.go`, `ls.go`, `find.go`, `managed_search_command.go`, `write.go`, and existing `read_test.go` / `find_ls_test.go`.
- Review type: static source/design review only. No product files or tests were changed or run.

## Findings

### R4-DC-1 [P2] The DFS batch-memory bound needs to account for active depth

The handoff says the walker retains “one 128-entry directory batch” plus a depth stack (`r4-directory-capture-handoff.md:35,41`). A depth-first walker that reads a batch and descends before processing the remaining entries must retain the unvisited suffix of that batch at each ancestor. In that implementation, pending directory entries can be `O(depth × batchSize)`, not one batch total. This does not invalidate bounded width retention or the accepted `O(depth)` descriptor tradeoff, but the stated memory bound would be inaccurate.

Before implementation, either make the walker consume one entry at a time (`ReadDir(1)`) or otherwise avoid retaining a batch per active frame, or update the memory accounting to include the pending batches and their name bytes. Keep the top-K candidate and output budgets independently explicit.

### R4-DC-2 [P2] Preserve or deliberately change the current unreadable-entry behavior

Current `LSTool.Execute` skips `WalkDir` callback errors and `DirEntry.Info()` errors (`internal/tools/ls.go:141-143,168-171`). `findPureGo` likewise skips walk errors and returns partial results except on context cancellation (`internal/tools/find.go:303-305,337-342`). The handoff proposes propagating open/read errors from the replacement walkers (`r4-directory-capture-handoff.md:41`), which changes that best-effort behavior.

This can be a sensible fail-closed choice, but specify whether a per-entry metadata/open failure aborts the whole operation or skips that entry. Keep root-open failures distinct from failures encountered below the root, and test the chosen behavior. Do not describe it as compatibility-preserving if it is changed.

### R4-DC-3 [P2] Find output shape is backend-dependent today

The Go walker deliberately omits the searched directory itself and converts matches relative to the root where possible (`internal/tools/find.go:313-335`). The external paths are accepted directly from `fd` or system `find` output after line splitting (`find.go:214-279`); there is no normalization step. For example, system `find` invoked with `.` can emit `./...` and the root itself, while the Go walker cleans relative paths and skips the directory root. The existing Find tests force the Go implementation, so they do not settle this difference.

The handoff's shared lexical top-K accumulator is sound, but the cross-backend acceptance test should first state whether it compares ordering over identical synthetic path values or requires normalized user-visible paths. If normalization is intended, specify root inclusion and relative/absolute rendering before implementing it; otherwise retain and test the existing backend-specific path semantics rather than implying all routes produce the same output.

### R4-DC-4 [P2] Capture-limit errors must stop backend fallback after full drain/join

`FindTool.find` allows fallback after ordinary helper errors (`find.go:172-202`), and `shouldFallbackAfterManagedHelper` only blocks cancellation, pending completion, and unconfirmed containment (`managed_search_command.go:82-95`). A streaming writer that reports an overlong-line capture error after the helper has joined would otherwise be treated as an ordinary helper failure and could silently run the next backend. The handoff correctly says not to fall back for capture-limit errors (`r4-directory-capture-handoff.md:39`); make that error classification explicit in the implementation path.

The writer must latch the capture error, discard the rest of the oversized line, and continue returning successful byte counts while draining. Only after the process and readers join should Find return the typed capture error. Lifecycle uncertainty must continue to take precedence and prohibit fallback.

## Confirmed design strengths and limits

- Read currently materializes the directory with `os.ReadDir` and builds the full listing (`read.go` directory branch); Ls retains all immediate children and the full formatted string; Find retains all matches and external stdout before sorting/capping. The proposed bounded collector addresses the actual accumulation points.
- The explicit Ls lexical tie-break matches the current `WalkDir` lexical visitation followed by stable time/size sorts.
- Full traversal remains necessary for globally sorted top-K results. The design correctly avoids equating result limits with scan-time or traversal-work limits.
- The accepted 4 KiB path/key guard and unlimited-depth `O(depth)` directory-handle tradeoff are documented. Preserve typed failure for an unrepresentable path; do not emit a shortened or fabricated path.
- The separate Grep gap is real: it uses the same unbounded `bytes.Buffer` helper before result truncation. This review does not accept any claim that Grep's source output is bounded.

## Review boundary

No product source was changed. The implementation should retain these clarifications in its tests or follow-up notes, then run the handoff's listed race gates on the fixed implementation snapshot.
