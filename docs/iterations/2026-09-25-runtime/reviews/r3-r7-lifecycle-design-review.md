# R3/R7 Model and Assembly Lifecycle Design Review

Review date: 2026-09-25  
Review type: independent source-grounded design review; no product code changed and no tests run.

## Reviewed snapshots

| Item | Snapshot |
|---|---|
| R3 model-switch handoff | `10a49b2b618fed4b7e3e80c8d0ac4a88488df74c` |
| R7 assembly-close handoff | `fb1253ae2863a387eea9a93ff46519e6fadc2eda` |
| R7 handoff amendment | `881d6b0920873fb3fcef9b4958c8115bc513054b` |
| Source reviewed | `a8717d0` on `codex/runtime-stabilization` (includes the R1 owner and tracing changes) |

The review compared the handoffs with `internal/core/run_slot.go`, `run_owner.go`, `run_commit.go`, `internal/llm/model_selector.go`, `internal/core/loop.go`, `internal/cli/assemble.go`, `internal/mcp/hot_reload.go`, `internal/extension/plugin_manager.go`, and `internal/tracing/async.go`. The earlier `r7-contract-review.md` is design approval, not evidence that Assembly close has been implemented or accepted.

## Decision

**Do not accept the complete Assembly-close lifecycle yet.** The R7 design has a sound bounded first slice: add an admission fence and stable idle signal to the existing `DefaultRunSlotGuard`, without creating another run owner or editing Assembly wiring. That slice is implementable once its race tests cover the reservation and release windows below.

**Return the R3 handoff for one P1 correction before model-switch implementation.** Its mutation gate is acquired after the potentially blocking model builder. As written, Assembly close cannot know that such a builder is still active. Move the non-queueing gate acquisition before the builder, or provide an equivalent selector-owned in-flight operation lease. The smallest fit is the same try-gate held across build, run-slot claim, publication, callback, and retired-model cleanup; keep the selector state mutex out of callbacks and builders.

These findings do not invalidate the R1 owner. At `internal/core/run_owner.go:820-882`, it waits for callback/workers/resources, closes the optional `RunCommitRunLease`, and only then releases the shared guard. Assembly can use that release as the active-run fence once R2's production session wiring is actually integrated; the current handoff correctly says that wiring is still pending.

## P1 — Model construction is outside the close-visible mutation operation

`internal/llm/model_selector.go:150-187` invokes `modelSpecBuilder` (or `modelBuilder`) before entering `replacePrimary`; `replacePrimary` only takes `switchMu` at line 190. The handoff likewise orders candidate build before acquisition of the mutation gate. In contrast, the close handoff waits for shared run admission to become idle and then acquires the selector mutation gate before closing the selector.

That leaves a lifecycle gap: a switch can be blocked in its builder without holding either the run claim or the mutation gate. Close can pass the R1 idle wait, close the selector and its dependencies, and return. The builder can then resume and run candidate cleanup after close has reported completion. This violates the promised single close owner and can use Assembly-captured resolver/configuration state after teardown. It also means a test that only races close with a builder and checks eventual candidate cleanup does not prove close owns that builder's lifetime.

Required correction: attempt the selector's non-blocking mutation gate **before** invoking the builder and hold it until the operation either disposes the uncommitted candidate or commits publication and retires the prior model. Do not hold the selector state mutex across the builder, `LoopAgent.SetModel`, or cleanup callbacks. Close first fences the shared run guard, then waits for this same operation gate; a caller deadline may return pending, while the close owner retains dependencies until the switch operation exits. A concurrent switch should receive the typed busy result without starting another build.

Add `TestSelectorCloseWaitsForInflightBuilderAndDisposesCandidate`: block a real selector builder after gate acquisition, start Assembly close, and verify the close waiter becomes pending while selector/resolver-dependent resources remain owned. Release the builder; because admission is closed, the switch must not publish, its candidate cleanup must run once, and the same close owner may then close the selector. Also test a switch that claimed the run slot before the close fence: close must wait for publication/retirement and subsequent lease/owner release before disposing the active model.

## P1 — Define the guard's idle signal across claim reservation and release

The current guard does not yet have close state or an idle signal (`internal/core/run_slot.go:38-116`). `ClaimRun` sends to the semaphore before publishing `current` under `mu` (`:55-81`); `Release` clears `current`, unlocks, and only then drains the semaphore (`:101-116`). A closed-bit check around `current` alone would therefore miss an acquisition that has reserved the semaphore but not yet published, and a close racing release could publish idle before the semaphore transition is complete. A claimant already blocked on the semaphore also cannot observe closure unless closure is part of its wait set.

The handoff correctly requires ClaimRun and CloseAdmission to linearize together and says a waiter racing close must wake and fail. Make that condition executable in the guard implementation: wake blocked claimers on a stable admission-closed signal; count or otherwise account for in-flight claim reservations; check closure while publishing the claim under the same mutex; and close the stable idle channel only after there is no current claim, no pre-close reservation still unwinding, and the semaphore token has been returned. Repeated `CloseAdmission` calls must return the same close/idle state. Calls after the fence must return a stable typed closed-admission error.

Use deterministic package-level barriers rather than relying only on stress timing. Cover (1) close after semaphore reservation but before `current` publication, where idle must remain open until the claim fails and returns its token; (2) a second ClaimRun blocked behind an active claim, where the close signal wakes it before that claim releases; (3) close racing Release, where idle does not close between `current = nil` and token return; and (4) an already idle guard whose returned signal is closed immediately and remains identical on repeated close. Keep admission closed permanently after the first fence.

The same concrete guard is already passed to Harness and TurnRunner (`internal/cli/assemble.go:402-410,1393-1396`), so a narrow `RunAdmissionCloser` capability on `DefaultRunSlotGuard` is appropriate. Do not implement a second Assembly run registry or change R1 worker accounting. A custom guard that cannot close admission must fail closed if one is ever admitted to this Assembly path.

## R7 lifecycle facts and limits

- The current Assembly exposes only `Cleanup func()` and synchronously invokes raw closures in reverse order (`internal/cli/assemble.go:252-256,682-708`). Its final cleanup currently calls `StopHotReloaders`, closes event/session/memory resources, and has no single-flight result or error aggregation. The handoff correctly proposes a separate Assembly close owner and immediate admission fence before dependent teardown.
- `DefaultHotReloader.Stop` joins `pollDone` but not an active reconnect loop (`internal/mcp/hot_reload.go:216-237`). `scheduleReconnect` starts `reconnectLoop` separately, and `runCycle` invokes the registration callback (`:313-363,385-423`). Close must stop/cancel new scheduling immediately, retain the actual reloader handles, and later join the exact active cycle and callback before closing registry/session/event/tracing dependencies. Clearing the package-global `activeHotReloaders` list cannot stand in for joining those operations (`internal/cli/assemble.go:2136-2170`).
- Plugin callbacks and exporters are cooperative. `PluginManager.Shutdown` has its own operation and callback can return the same `ErrCleanupPending` sentinel a waiter uses (`internal/extension/plugin_manager.go:200-283`). Assembly needs an operation-specific wait/result seam and must not infer callback completion from an error sentinel. A contextless callback cannot be killed: if it outlives the caller, retain its stack and every later dependency, return pending to that waiter, and continue with the same owner only after the callback actually returns.
- The one 20-second Assembly deadline is a cooperative owner budget, not a forced-stop guarantee. Start it once at close initiation, independent of each caller's wait context, and do not reset it per phase. Budget expiry can make the result pending/quarantined; it cannot authorize resource closure. The root invocation exporter remains owned by `cmd/cli/main.go` and is finalized after Assembly close (`cmd/cli/main.go:114-155`).
- The async exporter has its own 10-second drain plus 10-second sink-shutdown defaults (`internal/tracing/async.go:12-20,269-337`). Its underlying callback can still outlive those cooperative limits; its owner waits for the worker and does not establish that context cancellation forcibly stopped it. Assembly shutdown must retain the exporter/dependent tracing state on pending rather than convert the 20-second budget into a claim of completion.
- Assembly currently registers the initial primary and small-model cleanups in the raw cleanup list while switched primary generations are held by the selector (`internal/cli/assemble.go:715-820`). The R3 handoff's single selector owner and separate small-model owner are necessary. Keep the guarantee limited to primary model use through the Assembly's shared Harness/TurnRunner guard; direct Loop calls and naked model pointers remain outside it.

## Acceptance boundary

The next implementation may be only the `run_slot.go` admission/idle slice and its exact race tests. It must not be described as Assembly close, hot-reload completion, selector lifetime closure, R2 durable-run wiring, or full R7 acceptance. Once the R3 builder gate order is corrected, selector implementation should test real Loop model use with a blocked stream plus an R1 registered worker/resource; verify old model cleanup is delayed until the run owner releases the shared guard and exactly one model generation owns each cleanup.

No product tests were run for this review. The report records design/source evidence only; it does not claim the proposed lifecycle behavior is implemented or verified.

## Follow-up design delta at `b4f528e` (source handoff `255736649f39005b88a1cf5443db0835dc3cf12e`)

The updated R3/R7 handoffs close both P1 design findings above. R3 now acquires the same non-queueing selector mutation gate before resolver/model construction, holds it through candidate cleanup or publication/retirement, and requires Assembly close to wait on that gate while retaining resolver/provider dependencies. Its new blocked-builder acceptance case asserts a post-fence claim fails, candidate cleanup occurs once, and close remains pending until the builder exits. R7 now specifies a stable admission-closed wake signal for blocked claimers, one stable idle channel, a common linearization for claim publication and release/availability, and prefers removing the semaphore reservation window rather than adding a reservation counter framework. Its guard-only tests explicitly cover blocked claim wakeup, idle idempotence, and claim/close/release races.

**Delta decision: accept these documentation corrections for the narrow `run_slot.go` guard-only implementation slice.** The previous R3 builder-lifetime P1 and R7 guard-signal P1 are closed at design level. Full Assembly close remains unaccepted: no Assembly owner, hot-reload join, extension wait/result seam, model cleanup, or R2 production wiring was implemented or tested by this review. No product tests were run for this documentation delta.
