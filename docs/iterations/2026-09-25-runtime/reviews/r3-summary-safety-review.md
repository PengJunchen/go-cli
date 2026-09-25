# R3 summary safety review

**Target:** `42aa1cc8ec7281c6c8878dbb0152ef42f00dcd33`  
**Worktree:** detached `r3-summary-review-42aa1`  
**Verdict:** **Blocked** by one system-prompt preservation defect. The summary/user-role projection itself is aligned with the untrusted-summary contract.

## Blocking finding

### P1 — Compaction can remove the mandatory system prompt

`SummaryCompactor.Compact` computes the cut across all item groups, treats `normalized[:cut]` as the region to replace, and emits only a summary item plus the kept suffix (`internal/compaction/summary.go:170-205`). `renderSummaryItem` skips original `RoleSystem && !IsCompaction` items (`summary.go:259-263`). When the initial system prompt falls inside the old prefix, it is therefore neither included in the derived summary nor retained. Existing derived compaction summaries are handled before this role check and remain demoted to user-role untrusted context; the fix must preserve original system authority without promoting a derived summary.

This is reachable in the normal Loop path: it appends the system prompt before history and the current user (`internal/core/loop.go:516-551`); after mid-turn compaction it replaces the complete message slice with `turnItemsToMessages(compacted)` (`internal/core/loop.go:905-927`). That conversion only emits the summary and retained items; it does not reattach the system prompt (`internal/core/loop.go:1088-1100`). Subsequent model requests can run without the configured system/tool instructions.

A temporary behavior probe used a first system item containing `MANDATORY-SYSTEM-CANARY`, an old user item, a recent user item, a fake summarizer, and a 50-token limit. `Compact` chose `cut_point=2` and returned only the derived summary plus `RECENT-CANARY`; the summarizer prompt and output both lacked the system canary. The probe failed its preservation assertion as expected. This also exposes a gap in `TestSummaryCompactorProducesPlaceholderWithinBudget`: its fixture has a system item but assertions only check the summary and recent entries.

**Required before acceptance:** keep original mandatory system instructions outside the summarizable prefix and preserve them in their original provider role/order, counting them against the same budget. Continue to project every derived summary as user-role untrusted context. If the mandatory prompt plus retained context cannot fit, return an explicit budget error instead of dropping the system item.

## Non-blocking resource observation

The summarizer input passed to the model is capped by the configured estimator, and the per-item prompt blocks are individually capped. Temporary construction is not bounded by the total prompt cap: `buildSummaryPrompt` retains all bounded blocks and joins them before truncating (`summary.go:230-249`). A temporary probe with 500 long user items observed a 1,024,772-byte estimator input, then a final prompt of 1,200 bytes/tokens. Thus the final request budget is enforced, but intermediate memory scales with transcript item count. This is not a final-prompt budget bypass; avoid describing it as bounded temporary memory. A narrow bounded head/tail accumulator could address it if this slice requires a memory ceiling.

## Projection and authority review

The changed projection uses a fixed `UntrustedSummaryContext` wrapper and emits compaction summaries as user-role messages in both the Loop and session restore paths. Session restore clears tool calls, IDs, usage, input kind, and receipt metadata for compaction entries (`internal/session/derive.go:376-389`). The summary prompt labels the transcript as untrusted source material, omits raw tool arguments, and includes tool names/outcomes. I found no authority escalation in these paths.

## Validation

- `rtk proxy go test -list '^TestReviewProbe' ./internal/compaction`: exit 0; both temporary probes were enumerated before execution.
- Temporary preservation probe `rtk proxy go test -p 2 ./internal/compaction -run '^TestReviewProbeSystemPromptPreservation$' -count=1 -v`: exit 1 as expected; exact output showed `cut_point=2`, only the summary plus `RECENT-CANARY`, and `summarizer_prompt_contains_system=false`.
- Temporary materialization probe `rtk proxy go test -p 2 ./internal/compaction -run '^TestReviewProbeSummaryPromptIntermediateMaterialization$' -count=1 -v`: exit 0; logged 1,024,772-byte maximum estimator input and a 1,200-byte/estimated-token final prompt.
- Existing compaction targets `TestSummaryCompactor*`, `TestSummaryPrompt*`, `TestTruncatingCompactorKeepsSystem`, core `TestTurnItemsToMessagesProjectsCompactionAsUntrustedUserContext`, and session `TestCompactionRecoveryUsesFixedUntrustedUserProjection` were confirmed by `-list`, then passed targeted normal and `-race` runs.
- Full `rtk env GOMAXPROCS=2 rtk proxy go test -race -p 2 -count=1 ./internal/compaction ./internal/core ./internal/session`: compaction and core passed; session exited 1 in multiple `TestSessionRunCommitSink*` cases because `run_commit_test.go:60` expected `core: run commit terminal was not synced` but received nil. The failing tests/files are outside this commit’s diff. The reviewed 42aa1 worktree also predates the separate aa53407 session-helper test dependency; treat this as an integration-base failure rather than a summary regression. The targeted changed session projection test passes.
- `rtk env GOMAXPROCS=2 rtk proxy golangci-lint run ./internal/compaction ./internal/core ./internal/session`: passed, 0 issues.
- `rtk git diff --check 42aa1cc8^ 42aa1cc8`: passed.
- Temporary review probes were removed; the detached worktree is clean.

## Independent review of 4402b9fab934959d4a91b64be9e090691a6cb2e7

**Base/dependencies:** `4402b9f` is reviewed on top of `42aa1cc`; `aa53407` is a prerequisite session-test helper cherry-pick, not new production scope in this delta.  
**Verdict:** The original P1 system-prompt drop is fixed in the Summary/Truncating/Unified paths and the real Loop path. One bounded **P2 remains**: the factory-exposed standalone `micro` strategy bypasses the new mandatory-system preflight and legacy summary-role normalization. Keep the delta pending that alignment.

### P2 — Standalone MicroCompactor bypasses the mandatory-system contract

`DefaultCompactorFactory.Create("micro")` returns `MicroCompactor` directly (`internal/compaction/factory.go:54-64`). `MicroCompactor.Compact` shallow-copies the items and only replaces tool results (`internal/compaction/micro.go:32-95`); unlike Summary, Truncating, and Unified, it does not call `groupTurnItems` or `checkMandatorySystemPromptBudget`.

Two temporary direct behavior probes reproduced the inconsistency:

- A legacy item with `RoleSystem && IsCompaction` is returned as system-role unchanged. The accepted contract requires a defensive copy projected as user-role untrusted summary context, without mutating caller input.
- With a mandatory system prompt estimated at 100 tokens and `maxTokens=10`, Micro returns `ErrRequiresTruncating`; callers cannot identify the typed mandatory-system-over-budget condition used by the other strategies. The standalone strategy should run the same preflight before attempting replacement and return `ErrMandatorySystemPromptOverBudget` when mandatory systems alone exceed the budget.

This is limited to the standalone micro strategy. Unified normalizes and preflights before invoking its Micro child, and the Loop’s message conversion also demotes compaction entries. I found no reappearance of the original system-prompt removal in Summary, Truncating, Unified, session restore, or the tested Loop path. The fix should add direct Micro contract tests, including unchanged input assertions; no broader tool-argument or metadata changes are implicated by this review.

### Checks and evidence

- `rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 ./internal/compaction ./internal/core ./internal/session`: passed on the reviewed worktree.
- Targeted race runs passed for Summary/summary-prompt, Truncating, Unified, compaction token estimation and role projection, the real Loop mid-turn mandatory-system failure case, and session compaction recovery projection.
- Temporary Micro probes were listed and run as behavior probes; the two assertions above failed against the implementation as expected. The temporary test file was then removed. A separate stale-cache probe confirmed that the final Summary budget check recounts mandatory system content and the untrusted wrapper rather than accepting the old token cache.
- The reviewed worktree is clean at `4402b9fab934959d4a91b64be9e090691a6cb2e7`; `git diff --check` passed. No production source or ledger was modified by this review.

## Independent review of 90446f53c0ae787e161d8ae255f6e8bdf168de45

**Target:** `90446f53c0ae787e161d8ae255f6e8bdf168de45`, based on the reviewed `4402b9f` delta in `codex/runtime-r3-summary-safety`. This commit changes only `internal/compaction/micro.go` and `internal/compaction/micro_test.go`.  
**Verdict:** **ACCEPT for this delta.** It closes the standalone Micro P2 recorded above. Across the reviewed product chain, the original 42aa summary system-preservation P1 is fixed by 4402, and the remaining Micro normalization/preflight P2 is fixed here; I found no remaining blocker in this scoped chain.

### Review findings

`MicroCompactor.Compact` now calls `groupTurnItems` before estimating or replacing tool results, so legacy `RoleSystem && IsCompaction` entries are demoted on a defensive item-slice copy and malformed tool-call groups fail closed. It then calls `checkMandatorySystemPromptBudget` before any replacement, returning `ErrMandatorySystemPromptOverBudget` while preserving the original items on preflight failure. Successful compaction changes only fields in copied `TurnItem` values; the added regression compares the caller input before and after.

The normalization regression obtains the compactor through `NewDefaultCompactorFactory().Create("micro")` and exercises that concrete factory path. Additional regressions verify over-budget preflight before replacement and malformed-group rejection without input mutation. The legacy summary test confirms role demotion while preserving the historical item metadata expected by this compaction projection.

### Validation

- Listed `TestMicroCompactor*` and `TestDefaultCompactorFactory_Create` before execution; the list included the new factory-path, over-budget, and malformed-group cases.
- `rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^Test(MicroCompactor.*|DefaultCompactorFactory_Create)$' ./internal/compaction`: passed.
- `rtk git diff --check 4402b9fab934959d4a91b64be9e090691a6cb2e7 HEAD`: passed. The review worktree is clean at `90446f53c0ae787e161d8ae255f6e8bdf168de45`.
- This was a bounded review of the two-file Micro delta; it does not claim a new full-suite run.
