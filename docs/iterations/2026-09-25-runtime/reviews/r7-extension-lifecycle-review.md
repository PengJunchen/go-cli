# R7 Extension Lifecycle Review

## Decision

Reviewed snapshot: `ef3c7040142819bd2bc0d5a53ac5fcad4b69d7f6`. The callback deadline defect found in `78be59c` is fixed: the caller's context reaches extension Shutdown callbacks while the manager owner waits independently. The publication race reported against `4666f35` is also fixed: staged Init publication and manager Shutdown now share a lifecycle gate.

Scope is limited to `internal/extension/registry.go`, `manager.go`, `plugin_manager.go`, and lifecycle tests. This review does not assess Assembly, R1 admission, skills, hooks, EventBridge, exporter ownership, or default CLI wiring; it makes no support claim for those paths.

## Source-established behavior

- `DefaultExtensionRegistry` stores ordered names for all five registration categories. `commitFrom` seals private staging, checks every category for conflicts under the registry lock, then publishes entries and order slices only after all checks pass.
- The coordinator reserves a name before calling `Init`. Calls for the same comparable instance wait on one result; a different instance with the same name gets `ErrExtensionNameConflict`. A canceled waiter leaves the owner running. Failed `Init` seals and discards staging without normal `Shutdown`; successful Init followed by cancellation or commit conflict invokes cleanup Shutdown once and preserves joined errors.
- PluginLoader, extension Init, and Shutdown callbacks run outside the PluginManager mutex. Load rejects a late result after closing. Manager Shutdown retains one `shutdownOp` owner while it waits for load/Init work and then shuts down extensions in reverse load order. A caller timeout returns its own typed `CleanupPendingError`; it does not clear the owner or mark manager shutdown complete.
- The 4666 delta separates the manager owner's uncancelled wait context from the first Shutdown caller's callback context. Normal Shutdown and late-Init cleanup callbacks receive the first caller's deadline. A timed-out waiter does not cancel the manager owner; an uncooperative callback keeps earlier extensions open until it actually returns.
- A callback that itself returns `CleanupPendingError` has returned. The manager records that as a completed callback error (`stopped_with_error`), joins and returns it, and does not run that callback again. This is distinct from a waiter timing out while the callback is still in flight.

## Publication gate closure at `ef3c704`

`extensionCoordinator` owns one `extensionLifecycleGate`. The Init owner now locks staging registry → parent registry → lifecycle gate → coordinator before its final abort check and commit. It checks both the owner cancellation state and `gate.closing` while holding that shared gate; the merge runs under the same critical section. `PluginManager.Shutdown` takes the gate before setting `pm.closing`, records the first shutdown callback context, sets `gate.closing`, and releases the gate before canceling manager operations or starting callbacks. Thus either Init completes publication while holding the gate before shutdown closes it, or Shutdown closes the gate first and Init discards staging and cleans up. There is no check-then-commit window after the close linearization point.

The new regression, `TestPluginManagerShutdownClosesCommitGateBeforePublication`, deterministically holds the parent registry lock so the successful Init callback stages a tool but cannot publish. It starts Shutdown, observes `pm.closing`, releases the registry, then asserts Init fails, the staged tool is absent, and cleanup Shutdown ran exactly once. This reproduces the previously failing ordering without relying on a timing-only cancellation check.

The lock order is explicit and has no reverse acquisition in this slice: Shutdown holds the lifecycle gate while briefly taking the manager and coordinator locks, but never takes either registry lock; manager Load/Init release `pm.mu` before invoking loader/coordinator work; the Init owner never holds the coordinator lock while waiting for registry locks. User Init/Shutdown callbacks still run without manager, coordinator, registry, or lifecycle-gate locks held.

An Init commit that obtains the gate before Shutdown is allowed to publish; Shutdown then observes the committed running extension and tears it down through the normal reverse-order owner. If Shutdown obtains the gate first, no later staged registration is published. This matches the lifecycle contract's linearization at the shared gate.

## Validation

The source inventory command `go test ./internal/extension -list .` listed 110 test names. The reviewer did not run the full extension package.

At the original builder snapshot `e45578eb9074ffb5accbde4073195db939a9fea6`, 13 focused official tests passed under `-race`: `TestExtensionOrderStable`, `TestExtensionInitFailurePropagates`, `TestPluginManagerLoadRejectsDuplicateExtensionNamesAtomically`, `TestExtensionConcurrentInitAndShutdownSafe`, `TestExtensionStagingCommitConflictShutsDownAndJoinsErrors`, `TestExtensionWaiterCancellationDoesNotCancelOwner`, `TestUncooperativeExtensionInitReturnsCleanupPending`, `TestUncooperativeExtensionShutdownReturnsCleanupPending`, `TestPluginManagerShutdownDuringInitQuarantinesUntilLateInitCleansUp`, `TestPluginManagerShutdownErrorsPropagateWithoutRepeatingCallbacks`, `TestPluginManagerShutdownReverseOrder`, `TestRegistryConcurrentConcurrentRW`, and `TestCoordinatorConcurrentLifecycle`.

At `78be59c`, nine focused official tests passed under `-race`, including the first manager-level late-Init continuation regression. Its callback-deadline probe exposed that callbacks could not see the caller deadline.

At `4666f35`, six focused official tests passed under `-race`: `TestPluginManagerShutdownContinuesAfterLateInitCleanup`, `TestPluginManagerShutdownCallbackGetsCallerDeadlineAndContinues`, `TestPluginManagerShutdownWaitsForUncooperativeCallbackBeforeEarlierExtension`, `TestPluginManagerCallbackCleanupPendingErrorIsCompleted`, `TestPluginManagerShutdownDuringInitQuarantinesUntilLateInitCleansUp`, and `TestPluginManagerShutdownErrorsPropagateWithoutRepeatingCallbacks`. These verify caller deadline delivery, independent owner waiting, reverse callback order, single execution, and separation of callback-returned errors from waiter-pending state.

At `ef3c7040142819bd2bc0d5a53ac5fcad4b69d7f6`, the builder reports the full `internal/extension` race suite passed and five focused lifecycle tests passed. The reviewer independently ran `rtk go test -race ./internal/extension -run '^TestPluginManagerShutdownClosesCommitGateBeforePublication$' -count=20`; all 20 repetitions passed. The previous blocker probe established that the old ordering could publish after `pm.closing`; the new deterministic regression asserts the opposite outcome under that same schedule.

Other temporary probes at the original snapshot passed under `-race`: external registration conflict stayed atomic; eight concurrent manager Shutdown callers invoked callbacks once; and Shutdown racing a blocked Load rejected its late result. The temporary callback-context and publication probes were removed from the detached review worktrees.

## Limits

These checks establish lifecycle behavior in the requested extension package only. They do not establish Assembly cleanup ordering, exporter/sink lifetime, R1 run ownership, extension feature wiring, or full CLI readiness. This review did not rerun the full package suite; it independently repeated only the new race regression. No product source was modified by the reviewer.
