# R3 durable compaction design review

**Reviewed target:** the working-tree revision of `docs/iterations/2026-09-29-runtime/durable-compaction-design.md` at product-source HEAD `182e1bf9271b92e6cbca518e9f0d41408d82bfce`. The target design is modified in the worktree; the inspected core, llm, session, compaction, and CLI source seams have no local edits. Static design/source review only; no tests were run and no product files were changed.

**Verdict: REJECT pending one scoped correction.** The revision resolves both blockers in `remaining-runtime-design-r2-review.md`: a fresh below-budget operation can send a complete prior unknown receipt unchanged, while same-operation retry remains governed by the operation/replay authority; and `PendingRunInput` now carries the immutable count captured from the sink snapshot. The trusted scoped-read exception is correctly described as an R2/R4 prerequisite that is not implemented, so the draft does not claim that quarantine currently admits such reads. The source tags, exact per-call cut/result, sync-before-request ordering, suffix validation, raw-summary persistence/user-role projection, and ordered terminal projection list are otherwise consistent with the cited R2/R3 contracts and available sink seams.

## Blocking finding

### P1 — The existing post-run compaction hook remains an unprojected durable path

The draft makes Loop preflight and its typed projection result the durable compaction path, but does not say to remove or bypass the existing `AgentImpl` post-run hook for a bound durable run. The default CLI assembly still installs that hook unconditionally (`internal/cli/assemble.go:421`). After `CanonicalLoop.RunCanonical` returns, `AgentImpl.Run` appends the canonical transcript delta to `a.history` and then invokes `a.compactionHook`, replacing history with the hook output (`internal/core/agent.go:162,214-230`). The CLI hook calls `Compactor.Compact` directly and converts the result back to messages; it has no `NewProjection`, `CommitCompaction`, or terminal projection-list path (`internal/cli/compaction_adapter.go:21-44`). The Loop's deferred terminal commit has already run by this point.

That leaves a second, post-terminal compaction path in the assembled durable CLI. It can produce an in-memory history different from the exact projection that was synced, can invoke the summarizer outside the per-request operation/accounting sequence, and has no source cursor to persist. It also bypasses the required ordinary-request/compaction eligibility ordering. The original R3 design explicitly makes request preflight authoritative and removes the old turn-end trigger; this target cites that design but does not carry the removal into its owner table or required REDs.

The manual `/compact` command has the same unsynced mutation route: `CompactHandler.Handle` calls `Agent.Compact` (`internal/cli/slash_handlers.go:106-118`), which runs the hook and replaces agent history without a run projection or sink commit. The durable mode needs an explicit rule for this existing entry point as well.

**Required correction:** state that a durable `AgentImpl.Run` must not apply the legacy post-run `CompactionHook`; keep that hook for non-durable runs only. In durable mode, `/compact` must either use a separately defined typed projection/commit path with the same source and terminal guarantees, or fail explicitly without mutating history. Add RED coverage for the bound post-run path and the manual command so neither can run a strategy/summarizer or alter durable agent history outside the request-preflight projection path. This is limited to fencing existing compaction entry points; it does not require another persistence mechanism.

## Contract checks retained

- The new ordinary request gate and over-budget-only eligibility gate directly address the prior R2 unknown-receipt rejection. The draft preserves the historical group and receipt outcome, rejects unknowns before all strategies/summarizers on the compaction path, and keeps current-operation retry authority separate.
- `SourcePendingRunInput` and `SourceRunTranscript` now share the source count captured from `len(snapshot.SourceEntryIDs)`. Prior summaries are rehydrated as real `PriorPublished` marker sources, while same-run summaries alone carry `Through`.
- The typed result carries `Items`, the invocation's winning `Strategy`, and a raw summary plus the exact cut cursor. The draft prevents strategy selection from relying on `LastStrategy`, validates an unchanged source-keyed suffix, rejects the same-run synthetic-summary suffix case, and commits each accepted projection before the provider request that consumes it.
- The draft preserves the R2 one-sink/one-snapshot mapping and explicitly leaves trusted scoped reads blocked until their R2/R4 proof and permit are implemented.

Review was static. No test, build, or runtime validation was performed.
