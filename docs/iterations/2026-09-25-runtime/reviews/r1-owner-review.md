# R1 core run-owner review

Review date: 2026-09-25  
Review target: `daf46309eb7473d98424985bf5f2d978d0f2f7aa` with fixes through `e47919f4266785ddc3ccf0b22ec0a20f0b6e34f1` and test-fixture correction `e274c4ee45dccf7a569a59f48341a1a6e2fa339d`.  
Integration source snapshot checked separately: `aec0bd915bc107b6c0fcbf385511a2a05174e5ed` (includes `73ac4ddf4504625c112f820130a8da7d25dbf3a5`).  
SubAgent fixture follow-up: test-only commit `d7dda9e378a2ef42179de0fb8eb8726e34c79a63`, integrated snapshot `96b9f9d76975b063e49746dacb6dafecdfed0972`.  
Review type: independent source review and focused race runs; no product code changes.

## Decision

**Accept the reviewed core owner slice and its assembled shared-guard integration boundary at `aec0bd9`.** The owner retains the shared run claim until the callback, registered workers/resources, and canonical transcript are quiescent; reports bounded-wait and live status separately; and preserves unknown action outcomes without confusing them with active execution. The required real-assembly shared-guard regression and canonical-history metadata preservation now pass focused race tests. This review does not claim completion of the broader repository gate being run separately by the root reviewer.

## Findings

### R1OWN-01 — Transient backpressure could cancel an unrelated long run — fixed in `e47919f`

In the initial owner implementation, any full `BlockUntilConsumed` event buffer started a one-shot cleanup deadline. If a consumer resumed before that deadline, the timer still remained armed and could cancel the callback later, even while it was making normal progress. This made a temporary slow reader capable of aborting a long provider or tool operation.

The final implementation scopes the timer to a backpressure lease: `EventStreamImpl.send` acquires the lease when the channel first proves full and releases it as soon as that send resumes or returns (`internal/core/eventstream.go:188-189,219-229`). Expiration is actionable only while the lease is still active (`internal/core/run_owner.go:202-238,352-357`). A continuously blocked send still reaches the bounded cleanup path.

The compiled regressions `TestTransientBackpressureDoesNotCancelLongRun` and `TestContinuousBackpressureBoundsBlockedSendAndPublishesResult` cover recovery and sustained blocking (`internal/core/run_owner_test.go:70,124`). Both passed repeated race runs on the final owner snapshot.

### R1OWN-02 — A canceled caller could still acquire the run slot — fixed in `e47919f`

When the semaphore was available at the same time `ctx.Done()` was ready, Go could choose the semaphore send case and return a claim for an already canceled context. The no-op guard also accepted a canceled context. This could briefly admit work after cancellation and unnecessarily retain the shared slot.

`DefaultRunSlotGuard.ClaimRun` now checks cancellation before selection and again after acquiring the semaphore, returning the token if cancellation won the race; the no-op guard rejects an already canceled context (`internal/core/run_slot.go:55-72,126-132`). `TestRunSlotGuard_AlreadyCanceledContextNeverGetsClaim` and `TestRunSlotGuard_ClaimRunCancellationAfterSemaphoreAcquireReturnsToken` exercise both boundaries (`internal/core/run_slot_test.go:119,153`).

### R1OWN-03 — Cancellation outcome depended on watcher scheduling — fixed in `e47919f`

The cancellation watcher and callback completion can race for the owner mutex. If the callback returned a cancellation error first, classification could observe that the watcher had not yet set `cancelRequested` and publish `error` instead of `cancelled`. Callback completion now classifies against the authoritative owner context (`o.ctx.Err()`) and error chain (`internal/core/run_owner.go:249-295`). `TestRunOwnerCancellationOutcomeDoesNotDependOnWatcherScheduling` repeats this race directly (`internal/core/run_owner_test.go:208`).

### R1OWN-04 — Default compaction adapter erased canonical receipt/input authority on every run — fixed in `aec0bd9`

`AssembleAgent` always installs the compaction hook (`internal/cli/assemble.go:359`), and `AgentImpl.Run` invokes it after appending the validated canonical transcript (`internal/core/agent.go:199-229`). Before `aec0bd9`, the default `AgentMessage -> TurnItem -> AgentMessage` conversion omitted `InputKind`, `ReceiptOutcome`, and provider `Usage`; therefore a below-budget/no-op compaction still replaced canonical history with a lossy projection. A submitted user turn could lose its input provenance and a dispatched tool receipt could lose its unknown/error/success outcome, corrupting the authority history even though no compaction was performed.

The fix adds the missing fields to `TurnItem` with additive `omitempty` JSON tags (`internal/compaction/compactor.go:60-72`), maps them in both directions, and uses `core.CloneAgentMessages` to preserve independent copies of nested tool arguments, image URLs, and usage (`internal/cli/interactive.go:79-129`, `internal/core/agent_history.go:12-48`). The registry adapter now reuses the same conversion (`internal/cli/registry_adapters.go:121-130`) rather than maintaining a second lossy field list. `TestCompactionHook_PreservesCanonicalMetadataWithoutCompacting` asserts exact equality for follow-up provenance, unknown receipt outcome, usage, tool-call arguments and content blocks even below threshold, then mutates the returned graph to verify deep-copy isolation (`internal/cli/compaction_adapter_test.go:86-126`). The assembled run test also asserts the real `Agent.Messages()` history retains user input kind and unknown receipts while the run is pending (`internal/cli/run_ownership_integration_test.go:252-328`).

The distinct mid-turn provider-history bridge in `internal/core/loop.go:933-1008` also now preserves `Usage` and `FinishReason`, and deep-copies content blocks and nested tool arguments; `TestMidTurnCompactionMappingPreservesAndClonesMessageFields` checks its round-trip (`internal/core/loop_canonical_test.go:16-54`). `FinishReason` is not a field on canonical `AgentMessage`, so this is provider-context preservation, not an expansion of canonical transcript metadata.

### R1OWN-06 — SubAgent fixtures did not verify live follow-up admission — corrected in `d7dda9e`

The prior Phase 4 fixture drained the event channel to closure before calling `Send`, so the sub-agent had already left its live admission window; it then asserted the follow-up succeeded without observing a corresponding user event. The boundary fixture also encoded the old behavior that accepted messages before `Run` and after completion. These were stale tests, not a newly found product defect: the current contract accepts `Send` only in `Running`/`Waiting` and records a message only after the inbox accepts it (`internal/core/subagent.go:334-354`).

The test-only correction uses an explicit `NewSubAgentFactory`/`DefaultSubAgent`, sets `MaxTurns` above the 32-slot event buffer, and waits until the unconsumed channel is full while state remains `Running`. This bounded barrier proves the producer is live and blocked before `Send`; the test then asserts `Received`, drains events and observes the follow-up user event, waits for the deterministic final result, and verifies completion (`tests/phase4_e2e_test.go:259-309`). The boundary case now verifies idle and terminal sends return `ErrSubAgentNotRunning` and do not enqueue (`tests/e2e_20260802/10_boundary_edge_e2e_test.go:208-241`). No production code changed in this follow-up.

### R1OWN-GATE-01 — Required assembled shared-guard behavior test — closed on `aec0bd9`

The source wiring creates one `s.runSlotGuard` and passes it to both `Harness` and `TurnRunner` (`internal/cli/assemble.go:365,373`; creation at `1281`). `TestAssemblySharesRunSlotGuardAcrossHarnessAndTurnRunner` now builds a real `AssembleAgent` and tests both submission directions under sequential and parallel tool execution (`internal/cli/run_ownership_integration_test.go:252-371`). A pending mutating tool holds the first claim; the opposite entry point times out before provider request/tool dispatch; after release the live handle reaches quiescent while its outcome remains unknown; and the next run is admitted. The assembled model path uses `model.Stream`, so this covers the streaming execution path. The test also verifies canonical metadata preservation described in R1OWN-04.

## Lifecycle review

The owner registers workers before dispatch through the shared single/parallel tool helpers and registers pending resources before releasing the worker marker. It releases the claim only when the callback has returned, the canonical transcript is committed, and worker/resource counts reach zero (`internal/core/run_owner.go:119-167,370-409`). A dispatched context error or a dispatched call without a receipt is marked unknown; a registry lookup failure before dispatch remains a known error (`internal/core/parallel_tools.go:152-205`). Unknown outcome by itself does not prevent release once execution and transcript are quiescent.

`cleanup_pending` is returned as a caller snapshot while the owner retains its live handle. An unresolved callback/worker is marked unknown and quarantined; later positive completion may move the live handle to `quiescent`, without mutating the already returned result (`internal/core/run_owner.go:370-443,458-472`). The tests exercise a non-cooperative callback, pending execution resources with both known and unknown outcomes, panic quarantine, a single terminal event attempt, event-gap accounting, and immutable returned execution state (`internal/core/run_owner_test.go:403-589,670-710`).

Harness finalization sets `run_id`, `outcome`, and `execution_state` span attributes and publishes the result/terminal event before its deferred stream close (`internal/core/harness.go:192-221`). The timeout callback no longer closes the stream ahead of span/result publication. TurnRunner also uses the same owner and preserves live status through `Get`; completed quiescent turns prune their owner entry together with the turn (`internal/core/turnrunner.go:137-171,440-452,480-512`).

The final test-only commit `e274c4e` corrected a contradictory fixture: `TestDispatchedKnownInterruptedContextErrorIsNotUnknown` now cancels inside the owner callback, then dispatches once and asserts the dispatch count. The earlier pre-canceled setup was incompatible with the owner’s correct “do not start callback after canceled claim” rule. This was a test-fixture issue, not an additional production defect.

## Validation

On owner worktree `codex/runtime-r1-owner` at `e274c4ee45dccf7a569a59f48341a1a6e2fa339d`, the exact selected test names were confirmed with `go test -list`. The 22 focused owner, EventStream, Harness, and RunSlot tests passed under `-race -count=2`. Seven high-risk cancellation/backpressure/release-boundary tests passed under `-count=20`.

On integration snapshot `c847496a1b5acabdd20e1ee14370e4cee285d792`, eight focused real-loop and slot-guard tests passed under `-race -count=2`: canonical sequential unknown-outcome stop, parallel receipt closure and cancellation snapshot, sequential/parallel mode, concurrent Harness submission, slot release, and TurnRunner concurrent-slot rejection.

On the fixed integration snapshot `aec0bd915bc107b6c0fcbf385511a2a05174e5ed`, the real assembled shared-guard test plus four CLI field-mapping/compaction adapter tests passed under `GOMAXPROCS=2 go test -race -p 2 -count=2 ./internal/cli -run '^(TestAssemblySharesRunSlotGuardAcrossHarnessAndTurnRunner|TestCompactionHook_PreservesCanonicalMetadataWithoutCompacting|TestMessagesToTurnItems_PreservesToolCalls|TestTurnItemsToMessages_RoundTrip|TestCompactorAdapter_PreservesToolCalls)$' -timeout 90s`. The core mid-turn field/deep-copy regression passed under `GOMAXPROCS=2 go test -race -p 2 -count=2 ./internal/core -run '^TestMidTurnCompactionMappingPreservesAndClonesMessageFields$' -timeout 90s`. The earlier pre-fix assembled test exposed missing canonical fields; the fixed snapshot retains those assertions and passes. `git diff --check aec0bd9^ aec0bd9` also passed.

On the SubAgent test-only follow-up at integrated snapshot `96b9f9d76975b063e49746dacb6dafecdfed0972`, the full parent Phase 4 test (including trace-tree assertions) passed twice under race with `GOMAXPROCS=2 rtk proxy go test -race -p 2 -count=2 ./tests -run '^(TestPhase4EndToEnd|TestBoundary_SubAgentSendBeforeRun)$' -timeout 90s` (`ok .../tests 1.125s`). That command's `./tests` package contains `TestPhase4EndToEnd` but not `TestBoundary_SubAgentSendBeforeRun`; the boundary case was not matched there. I confirmed the exact test is listed in `./tests/e2e_20260802` using `GOMAXPROCS=2 rtk proxy go test -list '^TestBoundary_SubAgentSendBeforeRun$' ./tests/e2e_20260802`, then independently passed it once under race with `GOMAXPROCS=2 rtk proxy go test -race -p 2 -count=1 ./tests/e2e_20260802 -run '^TestBoundary_SubAgentSendBeforeRun$' -timeout 90s` (`ok .../tests/e2e_20260802 1.075s`). I reviewed the fixture diff and confirmed it preserves the intended live admission behavior rather than weakening the assertion or depending on sleeps.

No full `internal/core` or `internal/cli` package suite is claimed by this review. The broader integrated race suites are tracked separately from this owner-specific acceptance. A prior package-suite run on the pre-integration candidate reported one known older CLI fixture failure (`TestOutputGuardRunsBeforeVisibleDelta`); it is separate from the focused passing tests above and is not reported as a current failure on `aec0bd9`.
