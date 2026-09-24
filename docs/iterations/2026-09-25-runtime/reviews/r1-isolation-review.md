# R1 interceptor isolation review

Review date: 2026-09-25
Review targets: `62b8229` (lane source `980712`) and follow-up `dfbba64` (lane source `d6ec923`).
Review type: independent source review and focused race tests; no implementation changes.

## Decision

**Accepted after follow-up.** `dfbba64` attaches Plan Mode and Hook interceptors to each Assembly's LoopAgent, runs Plan Mode first, and retains the registry wrapper as a final guard. The revised real-Assembly test proves A emits the existing `tool_canceled` event without creating its target file while concurrently B's independent write succeeds.

## Findings

1. **[P1, resolved by `dfbba64`] Preserve pre-execution Plan Mode cancellation.** The Assembly now combines its Plan Mode and Hook interceptors per LoopAgent, Plan Mode first. The registry wrapper remains a final guard for execution outside the Loop path. The blocked call again emits `tool_canceled` before execution.

2. **[P2, resolved by `dfbba64`] Exercise both isolation and enforcement through real Assemblies.** `assembleTools` installs `NewPlanModeToolWrapper` in the outer registry chain before later extension/extra-tool registration. The outer `Get` still wraps definitions registered through the delegated `Register`; `assembleLoopAgent` passes that same registry to `WithTools`, and sequential/parallel execution both call `Get`. `TestAssemblyInterceptorsAreIsolated` now runs actual built-in `write` calls in A and B concurrently: A's Plan Mode blocks before file creation and emits the matching `tool_canceled`; B has an independent inactive controller and creates its file.

## Confirmed isolation properties

- `LoopAgent.toolInterceptors` is copied from options at construction. `SetToolInterceptor` mutates only the receiver's slice under `LoopAgent.mu`; `newPreToolCallEvent` snapshots that slice under the read lock. Updating an interceptor cannot mutate an event already in flight.
- Both Loop execution branches construct pre-tool events through `newPreToolCallEvent` before dispatch. Local interceptors run first and short-circuit on error; the legacy global list is then snapshotted under its existing read lock.
- `RegisterToolInterceptor` and `ClearToolInterceptors` retain their global behavior. `SetToolInterceptor` now follows its documented LoopAgent-scoped behavior: it replaces that LoopAgent's local interceptor list, and `nil` clears only that list.
- Assembly now attaches its own Hook callback to its `LoopAgent`; it no longer leaks Hook policy across separate Assemblies through process-global registration.

I verified the targeted test names with `go test -list` before running them. The focused `-race` selection for `internal/core` and `internal/cli`, including the concurrent real-Assembly A/B test, Plan Mode wrapper registry test, legacy global API tests, and scoped interceptor tests, passed. No source-level blocker remains in this isolation slice.
