# R2 core run-commit binding review

## Verdict

Approved for the assigned core binding slice at `ec7e6f50f79d1653b4c7739e946fbc08f1a32088`. The fixed worktree was clean and the commit changes only `internal/core/run_commit.go`, `internal/core/run_commit_test.go`, and `internal/core/run_owner.go`. I found no blocker in the reviewed scope. This does not approve Loop/CLI integration or establish that durable checkpoints are wired end to end.

## Review evidence

- [`run_commit.go`](../../../../internal/core/run_commit.go:32) uses one private context key and one value containing both sink and `RunCommitBinding`. `WithRunCommit` replaces both together; a nil sink clears the binding, while lookup treats the resulting entry as masked. The legacy `WithRunCommitSink` delegates to this same key with an empty binding, so replacing a sink cannot retain an ancestor descriptor and nil still masks it. A child `WithRunCommit` value likewise replaces both parent values; the test checks the child sink and incomplete child descriptor without inheriting parent fields.
- [`run_owner.go`](../../../../internal/core/run_owner.go:60) creates one owner generation per `runOwner`, separately from the claim ID. Immediately before callback dispatch, the owner overwrites any caller-supplied generation, validates the complete physical session/workspace/generation descriptor, and attaches that pair to the callback context. An invalid descriptor takes the undispatched error path before `invokeRunCallback`; the Harness regression confirms the agent callback is never entered and the stream returns `ErrInvalidRunCommitBinding` with an error outcome. Core has no `internal/session` import.
- The same `runOwner` path is used by Harness and TurnRunner. Their focused tests each run twice, assert that callback `RunID` comes from the owner claim rather than the parent context, preserve the attached sink and physical fields, replace a caller-supplied generation, and observe distinct owner generations across the two runs. The context replacement test proves the binding behavior at the seam; it does not claim to validate the deferred subagent/Loop integration.
- Cancellation behavior remains distinct from descriptor failure: `finishUndispatched` classifies only `context.Canceled` and `context.DeadlineExceeded` as cancellation; other pre-dispatch errors remain errors. The targeted pre-cancel, watcher-scheduling, and claim-retention cancellation tests pass.
- `newClaimID` contains a `UnixNano` fallback if `crypto/rand.Read` returns an error. The module requires Go 1.24.2 or later; Go 1.24 release notes guarantee `crypto/rand.Read` does not return an error and state that an underlying Reader error irrecoverably terminates the program. Thus that timestamp fallback is unreachable under the declared toolchain contract; no fallback collision was reproduced, and I do not treat it as a blocker. See the [Go 1.24 release notes](https://go.dev/doc/go1.24), “crypto/rand”.

## Validation

On the fixed snapshot, with local Go `go1.26.4 darwin/arm64`:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/core -run '^(TestRunCommitSinkContextLookupAndMasking|TestRunCommitBindingValidation|TestRunCommitContextReplacementDoesNotInheritBinding|TestHarnessRunContextUsesOwnedRunID|TestTurnRunnerRunContextUsesOwnedRunID|TestHarnessRunContextCarriesSinkAndOwnerBinding|TestTurnRunnerRunContextCarriesSinkAndOwnerBinding|TestHarnessRejectsInvalidRunCommitDescriptorBeforeCallback|TestOwnerDoesNotStartCallbackAfterClaimContextCanceled|TestRunOwnerCancellationOutcomeDoesNotDependOnWatcherScheduling|TestRunOwnerCancellationKeepsClaimUntilWorkerStops)$'
ok   github.com/pengjunchen/go-cli/internal/core  1.063s
```

This was a targeted core race run only. It does not replace the separately owned full-core/session validation or later Loop/CLI integration gates.
