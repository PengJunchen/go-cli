# R3 model-selector lifecycle review

## Verdict

**Accept the bounded selector lifecycle slice after follow-up `6ea4b5809510a3674003c91f3adf6f5a66a33adc`.** The original `6ccb3c3a4231e6458e12e7ab1091b0b43dcfd095` snapshot was rejected on nil model publication, failed-disposer ownership loss, and invalid admission results. The follow-up closes those findings and passed the focused and full `internal/llm` race suites. This does not complete R3: Assembly has not yet wired the selector to its shared `RunSlotGuard`, and no production close sequence is exercised here.

The review used detached worktrees at both fixed hashes. No product source was changed by the reviewer; temporary adversarial probes were removed after execution.

## Findings and delta

### R3-SEL-01 — Successful nil builder result was published (fixed)

At `6ccb3c3`, `SwitchModelSpec` accepted `(nil, nil, nil)`, returned success, replaced `PrimaryModel()` with nil, and logged `llm_model_switched`. This could leave the selector and its consumer without a usable model. A temporary `TestReviewProbeNilCandidateMustNotPublish` failed against that snapshot. A returned cleanup handle for a rejected candidate also needs to run; if that cleanup panics, the candidate generation must remain owned and the selector must fail closed.

At `6ea4b58`, `isNilChatModel` rejects nil interfaces and typed-nil models before admission/publication with `ErrModelBuilderReturnedNil`; a supplied candidate cleanup runs exactly once. Cleanup panic is sanitized, retained for later disposal without retry, and triggers the failure handler. Permanent coverage is in `model_selector_lifecycle_test.go`.

### R3-SEL-02 — Final disposer discarded failed cleanup generations (fixed)

At `6ccb3c3`, `modelGenerationDisposer.run` set `d.generations=nil` before invoking cleanup callbacks. A cleanup panic therefore left only the stored error; the failed model and cleanup handle were no longer retained by the close owner. Temporary `TestReviewProbeDisposerRetainsFailedGenerationWithoutRetry` reproduced the empty generation list after a panicking cleanup.

At `6ea4b58`, successful generations release their model and cleanup references, while generations whose cleanup panicked remain in `d.generations` with `cleanupAttempted` set. Repeated disposal returns the stored error and does not retry failed cleanup. `TestModelGenerationDisposerRetainsOnlyFailedGenerationsWithoutRetry` covers mixed success/failure generations and repeated calls (`model_selector_lifecycle_test.go`).

### R3-SEL-03 — Configured admission accepted missing proof and leaked error-path release (fixed)

At `6ccb3c3`, a configured admission returning `(nil, nil)` was treated as successful and allowed publication without a release/claim handle. A callback returning `(release, err)` cleaned the candidate but dropped the returned release closure. Temporary `TestReviewProbeConfiguredAdmissionWithoutReleaseMustNotPublish` and `TestReviewProbeAdmissionReleaseWithErrorIsNotLost` reproduced both behaviors.

At `6ea4b58`, configured `(nil, nil)` fails with the fixed `ErrModelSwitchAdmissionMissingRelease` and rolls back the candidate without poisoning a healthy selector. When admission returns both a release handle and an error, the selector cleans the candidate first and releases exactly once afterward; a cleanup panic runs the failure handler before release. Tests are in `model_selector_lifecycle_test.go`.

### Existing lifecycle contract behavior

The mutation gate is acquired before the builder, so concurrent and nested switches fail busy before invoking another builder. Selector query methods remain usable from the switch callback because the state mutex is released around callbacks. The callback and retired-model cleanup run while the admission release is still held; callback, cleanup, failure-handler, and release panics are converted to typed errors without exposing panic payloads. `SealAndDetach` seals before waiting, preserves ownership after a timed-out caller, and returns an idempotent disposer once it acquires the same mutation gate.

Temporary race probes also confirmed that concurrent `SealAndDetach` waiters share the same disposer after a timeout/retry sequence, and that a blocked failure handler keeps admission held until it finishes. These probes were removed after passing.

## Verification

The selector test names were listed before execution. On the original `6ccb3c3` snapshot, the seven existing lifecycle tests passed under race at 20 repetitions, and the full `internal/llm` race suite passed. The temporary probes then reproduced the three findings above; the returned `(candidate, cleanup, buildErr)` path independently passed, cleaning the candidate once and preserving the current model.

On the fixed `6ea4b58` snapshot:

- `rtk env GOMAXPROCS=2 go test -p 2 ./internal/llm -list 'TestModelSelector|TestModelGenerationDisposer|TestDefaultModelSelector'` — discovered the permanent lifecycle tests.
- `rtk env GOMAXPROCS=2 go test -p 2 -race -count=20 ./internal/llm -run '^(TestModelSelectorRejectsNilCandidatesAndCleansReturnedHandle|TestModelSelectorNilCandidateCleanupPanicIsRetainedAndPoisons|TestModelGenerationDisposerRetainsOnlyFailedGenerationsWithoutRetry|TestModelSelectorConfiguredAdmissionWithoutReleaseRejectsCandidate|TestModelSelectorAdmissionReleaseWithErrorRollsBackBeforeRelease|TestModelSelectorAdmissionFailureDisposesCandidateAndKeepsCurrent|TestModelSelectorCleanupPanicIsRetainedAndNotReplayed|TestModelSelectorFailureHandlerPanicIsSanitizedAndRetainsCleanup)$'` — passed.
- `rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/llm` — passed.
- `rtk git diff --check 6ccb3c3a4231e6458e12e7ab1091b0b43dcfd095..6ea4b5809510a3674003c91f3adf6f5a66a33adc` — passed.

No full repository suite or actual CLI Assembly/RunSlot integration was run as part of this selector-only review.
