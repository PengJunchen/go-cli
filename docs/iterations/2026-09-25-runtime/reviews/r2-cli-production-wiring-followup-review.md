# R2 CLI production wiring design follow-up review

**Reviewed fixed design:** `ea6d1a7e9d00443a9d66e542c09cf54128d93c99`, parent `e58bb2f2e81d9adebdf85ef238ad8b0c12238331`, following handoff `2b8101ed5cf9105bc639dfcc41f2474b394c1a59`. The worktree was clean. This is a docs-only feasibility review against the R2 contract, approved core/session handoffs, current core entrypoints, and the earlier CLI session handoff.

**Verdict: approve this design follow-up.** It resolves the prior P1 pre-owner lease leak and P2 fork-scope ambiguity with implementable boundaries. No product tests were run because this review assesses design/source feasibility only. This approval does not certify implementation, R7 Assembly-close integration, or full R2 closure.

## Lease return and transfer

The revised design makes both `Harness.Submit` and `EinoTurnRunner.RunTurn` capture an optional `RunCommitRunLease` from the input context before early rejection paths. Until a run owner accepts it, the entrypoint retains the release obligation; its pre-owner cleanup releases after claim, wrapper, stream, and context cleanup. At the synchronous accepted-run boundary, the capability transfers once to the owner. `Harness.Submit` can perform this after wrapper status succeeds and before setting `transferred`/starting `h.run`; `RunTurn` can do so after wrapper status succeeds and before `owner.run` begins.

This is consistent with the current source shape: both entrypoints and `runOwner` live in `internal/core`, and `RunCommitRunLease` is already an optional sink capability in `run_commit.go`. `runOwner.run` currently discovers the capability from context at owner start; the design explicitly requires it to reuse an entrypoint-transferred token instead of creating a second wrapper/return path, while retaining the existing context lookup for direct `runOwner.run` use. That is a narrow internal seam and avoids changing the exported sink interface.

The cleanup-pending boundary is correctly owned: once accepted, a caller return cannot reclaim or close the lease. The owner retains it through callback/resource joins, terminal bookkeeping, and late-proof policy. Pre-owner close panic handling preserves the original rejection while adding `ErrRunCommitLeaseClosePanicked`; because no owner accepted a run, no new `RunID` or unknown run is fabricated. The design also correctly leaves a manager borrow pending if a panicking release callback did not complete.

Required implementation checks remain in the handoff: exact-once pre-owner release for nil-loop, claim, wrapper registration/status, and stream setup rejection; accepted Harness and TurnRunner transfer; held callback/resource cleanup with early `cleanup_pending`; and direct owner context adoption without a transferred token. These checks are acceptance work, not evidence supplied by this design review.

## First-slice fork deferral

The handoff now explicitly supersedes the earlier first-slice prompt-fork requirement. Durable prompt `--fork` returns a typed unsupported error before durable session preparation/index creation, lease acquisition, history mutation, or provider entry. Durable REPL `/resume`, `/switch`, `/fork`, and `/clone` reject before tree/history/branch/session mutation; `/tree` and `/branches` remain read-only canonical-snapshot views. The non-durable compatibility path may retain its existing behavior. The document keeps complete durable fork/branch semantics open for a separate design and implementation, so it does not present rejection as fork support or as full R2 completion.

This boundary is feasible against the current prompt and slash-command entrypoints: durable mode can reject before `newDurableCLISession` and before the existing prompt assembly path, and the slash handlers can fence mutators before invoking `SessionTree` or `BranchStore`. Exact physical `--resume-session` remains the only durable session selector in this slice.

The Assembly-close prerequisite remains explicit and uses the existing `AgentAssembly.Close(ctx)` owner. Serve/ACP remains unsupported for durable mode; no shared session or echo fallback is implied.

No design blocker remains for this slice. All R2 production behavior, failure-path tests, and restart evidence remain unimplemented/unverified until the separate code gates pass.
