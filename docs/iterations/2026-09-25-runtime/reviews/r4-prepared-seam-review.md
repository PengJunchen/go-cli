# R4 prepared dispatch seam review

Reviewed snapshot: `67c5c4897f736107ba191a2e7fea4d696c4c3906`, focused bounded-JSON fix `dfb5433f67d282bbcaf43b480d9766b0c96cf899`, and bounded approval-grant fence follow-up `0ebd841ec4550cf8a17343f6e0e548a2d1495f04`, based on R4 contract base `4f8d1a0`. Review scope is only `internal/tools/prepared.go` and `internal/tools/prepared_test.go` in the `r4-prepared` worktree.

Decision: **accept this prepared-API seam**. The checked invariants below hold in the reviewed snapshot. This is not R4 integration acceptance: registry replacement, protected `List`, policy/approval cache, mutation queue handoff, and R2 journal/dispatch wiring are not implemented in this seam and remain separate gates.

## Findings

- **Generation fence covers the complete tuple.** `DispatchFence.Publish` and the private `commit` compare the entire `DispatchFenceSnapshot` (registry, approval, and Plan generations) under the same short lock. Publication increments its selected domain after its callback returns; the callback is documented as a short in-memory publication only. Dispatch marks commitment under the fence and invokes the captured executor after releasing it. The dispatch path itself performs no tool execution, wait, or I/O while holding the fence; registry/policy publishers must honor the callback contract (`internal/tools/prepared.go:84,117`).
- **Call, schema, identity, and effect snapshots are defensive.** Call and schema values are cloned before digesting; accessors return copies, and trusted identity/effect fields do not expose mutable authority. The schema test verifies a native `[]string` remains `[]string`; the real Find/Git test demonstrates a provider-style `float64` reaches both actual tools unchanged. `GitBlameTool` reads line bounds only as `float64`; the test checks that `start_line=2,end_line=2` returns exactly the second line, so conversion to `int` would fail the assertion (`prepared.go:302,345`; `prepared_test.go:78,276`).
- **Recursive JSON validation is bounded.** The initial checkpoint recursively cloned containers without a depth bound, so a cyclic map/slice could recurse indefinitely before the later `json.Marshal` check. `dfb5433f` adds a 128-container-depth limit and returns `ErrPreparedValueNotJSON` beyond it. A subprocess regression checks both cyclic map and cyclic slice; a boundary regression accepts depth 128 and rejects 129 (`prepared.go:397-410`; `prepared_test.go:324,347`). This closes the only blocker found in the initial snapshot.
- **Admission is one-use on the direct and streaming paths.** A valid receiver atomically consumes admission before executor, context, or fence validation, so any failed first attempt cannot be retried with a fresh context. A streaming invocation embeds the same private state; `Dispatch` and `DispatchStreaming` therefore compete on the same CAS and cannot both execute. Tests cover stale-fence and canceled first attempts, successful reuse rejection, and stream-then-direct reuse (`prepared.go:589-608,618-642,661-675`; `prepared_test.go:134,157,190,213,237`).
- **The concrete commit marker is private and unambiguous.** The concrete prepared object has no exported marker setter; the sole `committed.Store(true)` is inside `commit`'s callback, which is reached only through the private fence comparison. The regression where the executor itself returns `ErrDispatchFenceChanged` still reports `DispatchCommitted()==true`, distinguishing an executor error from a stale pre-dispatch fence (`prepared.go:117,582-585,625-642`; `prepared_test.go:260`). At the initial `67c5c489` checkpoint the exported interface was structurally implementable in Go; the `0ebd841` follow-up adds unexported capability methods and closes that concern for external packages. The registry must still issue only invocations minted through the controlled path.

## Approval-grant fence follow-up (`0ebd841`)

The bounded follow-up is accepted. `PublishApprovalGrant` serializes under the same fence lock, compares the complete registry/approval/Plan tuple before invoking the update, and deliberately leaves the tuple unchanged for an additive positive grant. This allows parallel `AlwaysAllow` preparations based on the same policy snapshot to publish both grants without making each other's permits stale. Stale tuples from any of the three domains reject before the callback. The two new tests cover both concurrent grants and stale rejection across Registry, Approval, and Plan (`prepared.go:129-151`; `prepared_test.go:37,64`).

The API's add-only property is a caller contract, not something the generic callback can enforce. The subsequent approval implementation must use `PublishApprovalGrant` only for a positive monotonic memoization addition. Approval mode changes, clear/delete/revoke, deny/classifier changes, and definition replacement/reload must use `Publish(DispatchFenceApproval, ...)` or the relevant domain publication so old permits become stale. The approval caller is not part of this commit and remains unreviewed.

`Schema()` and `ToolCall()` return `nil` if defensive cloning reports an error. I found no reachable invalid-snapshot path in this seam: freeze validates and clones inputs before storing them, snapshot fields are private, and getters do not expose their backing trees. Keep that immutability invariant in the registry integration; this is a defensive-hardening note, not a blocker for the reviewed seam.

## Validation

Ran the 16-case prepared target selection with race detection in the frozen `dfb5433f` worktree:

```text
rtk go test -race ./internal/tools -run '^(TestDispatchFencePublishRejectsStaleFullSnapshot|TestDispatchFenceCommitChecksSnapshotBeforeMark|TestFrozenToolCallDefensivelyCopiesNestedJSONAndDigest|TestToolSchemaSnapshotDefensivelyCopiesJSON|TestTrustedEffectSetReturnsDefensiveCopyAndUnknownDefault|TestTrustedEffectValuesAreStableAndIdentitySnapshotIsJSONSafe|TestPreparedInvocationConsumesDirectPermitOnce|TestPreparedStreamingAndDirectDispatchShareOneUsePermit|TestPreparedInvocationStaleFenceDoesNotInvokeExecutor|TestCanceledFirstDispatchConsumesPermitAndCannotRetryWithFreshContext|TestCanceledStreamingFirstAttemptConsumesSharedPermit|TestPreparedInvocationReportsCommitEvenWhenExecutorReturnsFenceSentinel|TestPreparedDispatchPreservesFindAndGitNumericArguments|TestPreparedValueRejectsNonJSONObjects|TestPreparedValueRejectsCyclicContainersInSubprocess|TestPreparedValueEnforcesMaximumJSONDepth)$' -count=1
Go test: 16 passed in 1 packages
```

This targeted package run also compiled the implementation and test code. No full-repository suite was run for this bounded seam review.

For the `0ebd841` follow-up, I created a clean detached worktree at that exact commit so unrelated uncommitted registry work in the builder worktree could not affect the result, then ran only the two new grant tests and the stale-publication/commit/invocation fence regressions:

```text
rtk go test -race ./internal/tools -run '^(TestDispatchFencePublishApprovalGrantAllowsParallelMonotonicGrants|TestDispatchFencePublishApprovalGrantRejectsStaleAnyDomain|TestDispatchFencePublishRejectsStaleFullSnapshot|TestDispatchFenceCommitChecksSnapshotBeforeMark|TestPreparedInvocationStaleFenceDoesNotInvokeExecutor)$' -count=1
Go test: 8 passed in 1 packages
```

The temporary detached worktree was removed after the run. The approval caller classification and R4 registry/queue/approval integration remain separate pending gates.
