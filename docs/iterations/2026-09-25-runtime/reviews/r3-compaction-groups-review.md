# R3 tool-call group compaction review

Status: **ACCEPT**

Reviewed frozen snapshot `b600fa99a80bac4a1962b19fcce55d3365d85950` in `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r3-compaction-groups` (`4586f65` implementation plus `b600fa9` legacy fixture pairing). The checkout was clean before review. Scope was the in-memory tool-group algorithm and paired legacy fixtures; durable session projection/preflight wiring was not assessed.

## Findings

No blocking findings in the reviewed scope.

`groupTurnItems` validates each assistant call group locally: call IDs and names must be non-empty, IDs unique within the group, and each call must have exactly one immediately following `RoleTool` receipt. Receipts can arrive in a different order because they are matched by `ToolCallID`. Orphans, duplicate/missing receipts, unknown IDs, empty IDs, and conflicting names fail with `errMalformedToolGroup`. The ID set is discarded after closure, so the same ID can be reused by a later group. Missing receipt names are filled only after a unique ID match, on the copied `TurnItem` slice; the input and nested call arguments remain unchanged.

`TruncatingCompactor` now budgets complete groups. It preserves all system entries, scans non-system groups newest-first, includes a group when its whole estimate is `<=` the remaining budget, skips a group that does not fit, and restores chronological order. The tests exercise the exact fit boundary and the one-token-short case: a two-call group is kept whole at budget 3 and dropped whole at budget 2 while the newer user item remains. This retains the former budget-driven selection behavior at group granularity; this package has no `minKeepCount` option.

`SummaryCompactor` validates the full transcript before calling the summarizer and chooses cut indexes only at group boundaries. Its tests check that a malformed orphan does not reach the summarizer and that the output retains either all calls and receipts or none. `UnifiedCompactor` returns a malformed-group error from the summary attempt directly instead of treating it as a reason to run truncation; `TestUnifiedCompactorPropagatesMalformedToolGroupWithoutFallback` verifies that neither the summarizer nor truncating fallback proceeds after the malformed error.

The `b600fa9` fixture updates pair legacy `RoleTool` rows with assistant `ToolCalls` and `ToolCallID` values across the modified compaction, interactive, integration, and phase fixtures. The complex-conversation fixture now compacts only at generated-turn boundaries. I left the existing `RoleSystem` summary placeholder behavior unchanged; it is explicitly outside this slice.

## Validation

The test names were enumerated with `go test -list .` before execution. The targeted unified malformed-propagation test passed. Full `internal/compaction` race tests passed, as did the affected compaction E2E tests under race, including micro, truncating, unified routing, long-conversation stability, Phase 19, and Phase 47 fixtures. Lint of both affected packages reported zero issues.

Commands (all from the frozen worktree):

- `rtk proxy go test ./internal/compaction -run '^TestUnifiedCompactorPropagatesMalformedToolGroupWithoutFallback$' -count=1 -v` — pass.
- `rtk proxy go test -race -count=1 ./internal/compaction` — pass.
- `rtk proxy go test -race -count=1 ./tests/e2e_20260802 -run '^(TestCompaction_(MicroCompaction|TruncatingCompaction|UnifiedStrategyRouting|TokenEstimation|QualityEvaluation|ComplexLongConversation|EdgeCases)|TestE2EInteractive_AutoCompaction|TestUnifiedCompactorRouteOrder|TestWT_Compaction_UnifiedRouterDegrades|TestWT_Compaction_LongConversationStability|TestE2E_Phase19_CompactionSinglePath|TestE2E_Phase19_ConfigFieldConsumption|TestE2E_Phase47_CompactionPreservesContext)$'` — pass.
- `rtk proxy golangci-lint run ./internal/compaction ./tests/e2e_20260802` — pass, 0 issues.

`RoleSystem` summary-role semantics and provider preflight/session durability remain separate implementation gates; this review does not claim them complete.
