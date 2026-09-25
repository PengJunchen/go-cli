# R2 completion-proof schema review

Reviewed commit: `bc11df9b3b377af5c1532035a1146b55407e964d` (`feat(session): require typed completion proof for resolution`). The review used a detached worktree at that commit; the builder worktree was not changed. The commit changes five files: `internal/core/run_commit.go`, `internal/session/durable_contract.go`, `internal/session/durable_recovery.go`, `internal/session/durable_recovery_test.go`, and `internal/session/types.go`.

## Result

The new schema boundary is directionally correct: version-one execution rows accept only the three approved completion values (or the empty value for an observation without proof), non-execution rows reject a populated proof, and recovery no longer treats `ExecutionIdentity` as completion evidence. Unknown values, identity-only local evidence, and future-row evidence fail closed in the recovery path.

I cannot approve this as satisfying the approved R2 proof-validation contract yet. Two gaps remain: local resolution does not require a dispatched receipt, and the direct `SessionViewFromEntries` path accepts semantically mismatched resolution checkpoints without invoking the durable-action analyzer.

## Findings

**P1 — local resolution can clear a not-dispatched action.** In `internal/session/durable_recovery.go:518-521`, the local branch checks that a referenced execution row exists, has a nonempty proof, precedes the resolution, and matches the action reference. It does not require the occurrence's receipt to be `DispatchStateDispatched`; it also does not validate the execution row's `DispatchState`. A detached-worktree probe with an exact positive `inline_pipeline_joined` proof row and an exact terminal receipt both marked `not_dispatched` was accepted by `PrepareResume`, which invoked `ApplyDurableResolutions` once. This contradicts the approved requirement for “a dispatched action” in `r2-local-quiescence-proof.md:31`. At minimum, require the exact receipt to be dispatched; also validate the execution row's state if `ActionExecution.DispatchState` is intended to be authoritative. Add regressions for both contradictory rows and assert the recovery authority is not called.

**P1 — direct view accepts a cross-action resolution.** `SessionViewFromEntries` in `internal/session/derive.go:35-56` runs row-coordinate and open-call-tail checks, then projects entries; it does not call `analyzeDurableActionJournal`. After closing the tool-call group, a checkpoint whose `ActionID` differed from the matching intent, receipt, and referenced proof row passed direct view construction. The recovery analyzer would reject this checkpoint because it has no exact matching intent, but the direct view path silently skips checkpoint semantics. The approved adapter handoff explicitly requires the shared analyzer to validate relationships for direct `SessionViewFromEntries` calls (`r2-session-run-commit-adapter-handoff.md:49-51`). Wire the same semantic validation into that path and retain a regression proving mismatched action, future evidence, and wrong-row-kind evidence are rejected before projection.

## Schema and boundary checks

- The source pins exactly `inline_pipeline_joined`, `queued_job_joined`, and `owned_process_group_joined` from `tools.PreparedCompletionProof`. `validateVersion1EntryCoordinates` rejects any other nonempty enum on an execution row and rejects any populated proof outside a version-one tool execution row (`internal/session/durable_contract.go:231-240,272-278`). A transient probe confirmed the wrong-row-kind cases fail.
- `analyzeDurableActionJournal` rejects unknown proof strings before it offers resolutions to recovery authority (`internal/session/durable_recovery.go:112-121,536-543`). The permanent `TestPrepareResumeRejectsUnsupportedCompletionProofBeforeAuthority` covers an unknown string.
- Local proof lookup requires an existing execution row, positive nonempty proof, `Seq` before the resolution, and full `PendingActionRef` equality (`internal/session/durable_recovery.go:518-521`; the ref includes physical session, workspace, run, batch, ordinal, tool-call, and action IDs at `:483-499`). The transient future-row probe confirmed recovery rejects a proof whose row follows its checkpoint.
- Identity-only execution remains diagnostic: `TestPrepareResumeRejectsLocalResolutionWithoutCompletionProof` supplies a valid `ExecutionIdentity` but no completion enum and verifies rejection before authority.
- Legacy `RecordVersion==0` rows bypass the version-one coordinate validator, as expected for legacy decoding, but `analyzeDurableActionJournal` ignores them as version-one evidence. A legacy completion field therefore cannot enter the recovery proof map; a version-one resolution cannot use it. Version `>1` is explicitly rejected by the version-one validator and the session tree/projection loaders. This is fail-closed for resolution, though an explicit permanent mixed-version regression would make the boundary clearer.
- Remote-terminal resolution remains rejected until typed protocol evidence exists; generic successful receipts do not become proof.

## Validation

Passed on the detached review worktree at `bc11df9b3b377af5c1532035a1146b55407e964d`:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/session -run '^(TestSessionEntryV1JSONRoundTrip|TestVersion1AssistantAndFirstReceiptRequirePresentZeroCoordinates|TestPrepareResumeAppliesValidatedResolutionBeforeInspectingMarkers|TestPrepareResumeRejectsRemoteTerminalWithoutTypedEvidenceBeforeAuthority|TestPrepareResumeRejectsInvalidLocalResolutionBeforeAuthority|TestPrepareResumeRejectsLocalResolutionWithoutCompletionProof|TestPrepareResumeRejectsUnsupportedCompletionProofBeforeAuthority)$'
ok github.com/pengjunchen/go-cli/internal/session 1.473s

rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/core -run '^(TestRunCommitSinkContextLookupAndMasking|TestRunCommitBindingValidation|TestRunCommitContextReplacementDoesNotInheritBinding|TestHarnessRunContextCarriesSinkAndOwnerBinding|TestTurnRunnerRunContextCarriesSinkAndOwnerBinding|TestHarnessRejectsInvalidRunCommitDescriptorBeforeCallback|TestRunOwnerPendingExecutionResourceHoldsClaimUntilDone)$'
ok github.com/pengjunchen/go-cli/internal/core 1.070s

rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/tools -run '^TestPreparedCompletionProofValuesAreStable$'
ok github.com/pengjunchen/go-cli/internal/tools 1.071s
```

Additional temporary probes in the detached review worktree confirmed: future-row recovery evidence is rejected; version-one proof on non-execution rows is rejected; legacy rows do not produce recovery resolutions; direct view accepts a cross-action checkpoint; and recovery accepts a local resolution paired with a `not_dispatched` receipt. The temporary test file was removed after the probes. The detached review worktree is clean. `git diff --check` on the reviewed commit passed.

## Follow-up review

Follow-up commits reviewed independently from detached worktree `r2-proof-followup-review`:

- `77cdba205d837e0090ec5fb4aa5907dee0e6bf69` — `fix(session): validate late action metadata in open groups`.
- `311bec9a3087c611d2c6605594c24cc675159ab4` — `fix(session): require dispatched proof for local resolution`.

Both original findings are closed. The local-resolution validator now requires a dispatched terminal receipt and a dispatched execution row, requires the proof row to be strictly after the matching intent and before the resolution, and retains full `PendingActionRef` equality (`internal/session/durable_recovery.go:517-523`). The version-one entry validator also rejects a populated positive proof on a non-dispatched execution row (`internal/session/durable_contract.go:272-278`). `TestPrepareResumeRejectsLocalResolutionForNotDispatchedReceiptBeforeAuthority` verifies that this contradiction fails before either recovery-authority method and leaves the workspace marker present.

`SessionViewFromEntries` now invokes `analyzeDurableActionJournal` before projection (`internal/session/derive.go:79-82`). The open-group tail then permits only the current group's exact intents, execution observations, receipts, and receipt-backed resolutions; execution after a receipt remains valid for the two-call sequential case. The new tests cover early and late completion proof, late resolution while a sibling call is open, rejection of a mismatched action reference and future proof, canonical receipt order, and withholding incomplete history. The durable writer itself rebuilds its view through `SessionViewFromEntries` (`internal/session/durable_jsonl.go:814`), so the same semantic check is applied on append and recovery paths.

No remaining blocker found in these commits for the reviewed proof-schema and direct-view boundary. The branch-local full race command passed on the exact `311bec9a3087c611d2c6605594c24cc675159ab4` tree:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 ./internal/session
ok github.com/pengjunchen/go-cli/internal/session 4.603s
```

`git diff --check bc11df9b3b377af5c1532035a1146b55407e964d 311bec9a3087c611d2c6605594c24cc675159ab4` passed. The detached follow-up review worktree is clean. This review does not claim the broader core/CLI durable-session integration is complete.
