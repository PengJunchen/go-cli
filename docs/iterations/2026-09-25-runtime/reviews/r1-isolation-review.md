# R1 interceptor isolation review

Review date: 2026-09-25  
Review target: `62b8229` (lane source `980712`)  
Review type: independent source review and focused race tests; no implementation changes.

## Decision

**Needs one behavior fix before acceptance.** Per-loop isolation itself is sound, and the production registry wrapper still covers real tool execution. Removing the global Plan Mode registration changes the production event semantics from a pre-execution `tool_canceled` to a tool execution error. The reviewer and root agreed that the existing `tool_canceled` behavior must remain. The root has asked lane 02 to combine per-loop Plan Mode and Hook interceptors in that order, retain the registry wrapper as a fallback, and add actual A-block/B-runs coverage.

## Findings

1. **[P1, pending fix] Preserve pre-execution Plan Mode cancellation.** Before this change, Assembly registered `NewPlanModeToolInterceptor` globally. `LoopAgent.Run` checked `PreToolCallEvent.IsCancelled()` before execution, which emitted `tool_canceled` and skipped the executor. Commit `62b8229` removes that registration and relies on `NewPlanModeToolWrapper` only. The wrapper returns `ErrPlanModeBlocked` from `Execute`; the Loop's normal tool-error path then emits an error `tool_result` and continues. No side effect occurs, but callers observe a different event/history contract. Keep a Plan Mode interceptor on the Assembly's LoopAgent, composed before its Hook interceptor, while retaining the registry wrapper as defense in depth.

2. **[P2, test gap addressed by pending fix] Exercise both isolation and enforcement through real Assemblies.** `assembleTools` wraps `s.tr` with `MiddlewareToolRegistry(NewPlanModeToolWrapper(s.planCtrl))`; later extension and extra-tool registrations delegate to the underlying registry, while `Get` still returns wrapped executors. `assembleLoopAgent` passes this same `s.tr` to `WithTools`, and both sequential and parallel Loop execution resolve calls using `Get`, so the wrapper remains on the production execution path after removing the global interceptor. However, `TestAssemblyInterceptorsAreIsolated` enters Plan Mode only on assembly A, then proves a mutation on B runs. It does not execute A's mutation, so it would still pass if A's production wrapper were accidentally omitted. Add an Assembly A blocked-mutation assertion with zero underlying calls and retain B's successful mutation assertion under parallel/race execution.

## Confirmed isolation properties

- `LoopAgent.toolInterceptors` is copied from options at construction. `SetToolInterceptor` mutates only the receiver's slice under `LoopAgent.mu`; `newPreToolCallEvent` snapshots that slice under the read lock. Updating an interceptor cannot mutate an event already in flight.
- Both Loop execution branches construct pre-tool events through `newPreToolCallEvent` before dispatch. Local interceptors run first and short-circuit on error; the legacy global list is then snapshotted under its existing read lock.
- `RegisterToolInterceptor` and `ClearToolInterceptors` retain their global behavior. `SetToolInterceptor` now follows its documented LoopAgent-scoped behavior: it replaces that LoopAgent's local interceptor list, and `nil` clears only that list.
- Assembly now attaches its own Hook callback to its `LoopAgent`; it no longer leaks Hook policy across separate Assemblies through process-global registration.

I verified the targeted test names with `go test -list` before running them. The focused `-race` selection for `internal/core` and `internal/cli`, including the real-Assembly isolation test, Plan Mode wrapper registry test, global API tests, and scoped interceptor tests, passed. That result does not cover the still-missing Assembly A enforcement assertion or preserve the old `tool_canceled` behavior.
