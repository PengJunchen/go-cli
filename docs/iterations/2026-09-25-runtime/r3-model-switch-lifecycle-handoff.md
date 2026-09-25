# R3 model switch and Loop lifetime handoff

Status: design-only follow-up for the approved R3 provider/catalog contracts. Source baseline is integration tip `463354450a3015ecc516734de4a3fbb52b654379` (`codex/runtime-stabilization`). This document does not authorize implementation, complete R3, or enable a protocol. R3 implementation remains gated by R2 recovery/owner integration and `R2-REV-01`.

## Source findings

| Source | Observed behavior | Consequence |
|---|---|---|
| `internal/llm/model_selector.go:150-186,189-211` | `SwitchModel*` builds outside the selector lock. `replacePrimary` publishes selector identity/model, unlocks, runs old cleanup callbacks, then calls `onModelSwitched`. | Two successful switches can overlap: A can block in old cleanup; B publishes and updates the Loop; then A's late callback calls `LoopAgent.SetModel(A)`. Selector reports B while the Loop runs A. |
| `internal/llm/model_selector.go:262-272` | `CloseReleasesCleanups` only snapshots and invokes the current cleanup slice; there is no closed state or switch/close serialization. | A concurrent or later switch can install a model after close has taken its snapshot, leaving a live model/cleanup that close never sees. |
| `internal/cli/assemble.go:721-729,801-820` | Assembly registers the initial primary cleanup directly in `cleanupList`; the selector separately owns cleanup callbacks returned by later builders. The switch callback only calls `s.loopAgent.SetModel`. | Switched-model cleanup ownership is split. `CloseReleasesCleanups` has no production call site, so the final switched model is not closed by the current assembly cleanup path. |
| `internal/core/loop.go:351-358,372-390,607` | `SetModel` replaces a pointer under `LoopAgent.mu`. `RunCanonical` snapshots that pointer once and uses it through the run/continuation loop. | Cleaning up a replaced model immediately can close a client/transport still used by the active run. The existing Loop snapshot is stable for that run, but there is no model borrow/lifetime reference. |
| `internal/cli/assemble.go:402,405-410,1372-1379`; `internal/core/harness.go:163-187`; `internal/core/turnrunner.go:134-166`; `internal/core/run_owner.go:386-425` | Assembly gives Harness and TurnRunner the same `DefaultRunSlotGuard`. Each run owner releases its claim only after callback/transcript completion and zero registered workers/resources. | Claiming this same slot is the smallest production quiescence fence for assembled CLI model replacement. A selector-only mutex does not fence active model use. |
| `docs/iterations/2026-09-25-runtime/r2-core-consumer-handoff.md:68-70` | The approved R2 handoff requires `RunCommitRunLease.CloseRunCommit()` to return the physical-session lease only after actual run resources join; the run slot is released after this owner work. This API is a handoff requirement, not currently implemented in `internal/core/run_commit.go`. | Once integrated, acquiring the shared run slot also waits for the R2 per-run lease to be returned. Do not treat the durable-session lease as a model lease or release it to make a model switch proceed. |

The approved authority remains [`r3-provider-design.md`](r3-provider-design.md), its design-only review at `e64f4f4`, and the approved selector/catalog slice [`r3-model-catalog-handoff.md`](r3-model-catalog-handoff.md). The latter already requires full provider-scoped switch identity and failed-switch state retention. This follow-up only defines publication and model resource lifetime at the Loop boundary.

## Minimal contract recommended for the CLI path

Use one serialized model-mutation operation in the existing selector/assembly path, plus the already-shared `RunSlotGuard`; do not hold the selector state mutex across builders, cleanup callbacks, or notifications, and do not add a general model/runtime registry.

1. Resolve and build the candidate from the existing immutable resolver and full `ModelSpec` before entering the publication section. Keep its cleanup locally owned until commit succeeds. Preserve the current provider/protocol/origin and credential checks.
2. Serialize mutation/close operations with a separate operation gate. Claim the same `RunSlotGuard` used by Harness and TurnRunner using the caller context. If claim fails or the context is canceled before publication, clean up the uncommitted candidate outside state locks and leave the active selector/Loop unchanged. The run owner continues to hold the slot while R1 workers/resources remain; after R2 integration it also retains the slot until the run-commit lease is returned. Current Harness/TurnRunner admission uses a 200 ms claim timeout, so a run arriving during the short switch transaction may fail as busy; it must make no provider call against a half-published model.
3. Under the selector state mutex, perform one short publication of the current `ModelSpec`, model, generation, and cleanup ownership. Release that mutex before updating the Loop or running any cleanup. While the operation gate and run-slot claim remain held, update the Loop's model through the existing `SetModel` seam, then clean up the retired model. Only then release the run-slot claim and operation gate. Thus no assembled CLI run can snapshot or call the retired model during the selector/Loop publication gap, and a slow old cleanup cannot let another switch callback overtake it. Read-only selector queries from a callback remain safe because the state mutex is not held.
4. Give the selector an idempotent closed state. Close participates in the same serialized operation and run-slot fence, marks the selector closed before invoking external code, clears the Loop model, and closes each model-owned cleanup exactly once. A switch that finishes building after close must dispose of its candidate and return a typed closed error without publishing. Register one selector close with `assembleState.runCleanup` before the underlying primary cleanup would run; preferably transfer primary cleanup ownership into the selector so a model generation has one cleanup owner. Keep the independently configured small-model cleanup separate.
5. Limit this guarantee to models acquired through the assembled CLI's Harness/TurnRunner path and its shared run guard. Direct external `LoopAgent.Run` calls or retained naked `BaseChatModel` pointers bypass that fence; their callers must provide their own quiescence/lifetime coordination. Do not add a global refcount to make arbitrary uncontracted model references appear safe.

The mutation gate orders publication, callback, and cleanup as a single transaction, but it does not by itself define which request wins if two builders finish in the opposite order from their invocation. The smallest semantics is **last successful operation to acquire the mutation gate and run slot commits last**. If product instead requires last-invoked-wins, assign an invocation generation before building and discard an older candidate when a newer request has superseded it; a newer failed/canceled build would then also suppress the older candidate. This policy needs an explicit owner decision.

The current callback is the assembly's in-memory `LoopAgent.SetModel` update. While the operation gate is held, callback read-only queries are allowed; a callback must not synchronously call `SwitchModel*` or close the selector. Decide before implementation whether nested mutations should be rejected with a typed error or queued for a later safe point. Do not permit an undocumented reentrant call to wait forever on the same run-slot/operation gate.

## Close and bounded shutdown boundary

`AgentAssembly.Cleanup` currently has type `func()` and `assembleState.runCleanup` invokes cleanup closures synchronously. The R1 owner deliberately keeps a run claim until true quiescence, even if a caller already observed `cleanup_pending`; therefore close must not bypass a still-held claim and close a model in use. Root must choose one shutdown contract before builder handoff:

- wait for the shared run slot to become quiescent before model cleanup, accepting that a noncooperative owner can hold synchronous cleanup; or
- expose a bounded/context-aware close result through the existing CLI shutdown path. On timeout, report pending and retain the model cleanup for the owner/quiescence path; never call cleanup just because the caller stopped waiting.

The second option aligns better with R1 truthful `cleanup_pending` and the R2 rule that durable leases return only after actual resource join, but should reuse the existing run owner/CLI shutdown path rather than create another detached cleanup goroutine. This handoff does not assert that R2's lease API or a bounded Assembly close currently exists.

## RED-first acceptance

Add focused tests before implementation. Use fake models with gated `Stream`/cleanup callbacks and the production shared run-slot wiring; do not test only selector helper state.

- `TestModelSwitchCallbackCannotPublishOutOfOrder`: first switch blocks in a retired-model cleanup; start a second switch. Assert the second cannot publish/callback until the first operation finishes. Each callback performs `CurrentModelSpec`/`PrimaryModelName` reads to prove no state mutex is held; after both return, selector identity and Loop model are the same generation.
- `TestModelSwitchWaitsForRunOwnerQuiescence`: a real `LoopAgent.RunCanonical` starts with model A and blocks inside its stream; a switch to B must not clean A, update the Loop, or admit another run until the current RunOwner releases the shared slot. Then assert the first run used A, the next run uses B, and A's cleanup ran exactly once. Repeat with a registered worker/resource that holds the R1 owner after the Loop callback returns; the switch must remain fenced until that resource joins. After R2 integration, add a sink `RunCommitRunLease` fixture and prove switch admission also waits for its release.
- `TestSwitchCanceledAtAdmissionPreservesCurrentModel`: occupy the run slot, build a candidate with an observable cleanup, cancel the switch context while it waits, and assert candidate cleanup exactly once with no selector/Loop publication.
- `TestSelectorCloseClosesAdmissionAndOwnsAllCleanups`: close an idle assembly after zero or multiple switches; assert initial/current/intermediate cleanup counts are exactly once, the Loop cannot call a model after close, repeated close is harmless, and subsequent switch fails with the typed closed error. Race close against a blocked builder; once it returns, the uncommitted candidate is disposed and never published.
- `TestModelSelectorReadQueryDuringPublish`: callback queries the selector while a switch is publishing and completes under a bounded timeout; this prevents moving the state lock over callbacks as a false fix. Add an explicit test or contract assertion for the chosen nested-mutation behavior.
- `TestAssemblyCleanupIncludesFinalSwitchedModel`: assemble with a local HTTP model fixture, switch once, then invoke the real `AgentAssembly.Cleanup`; verify the active switched client/cleanup is released once and no switch can reopen the selector.

Run exact tests in their owning packages, then race the concurrency/lifetime cases:

```bash
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/llm -run '^(TestModelSwitchCallbackCannotPublishOutOfOrder|TestSwitchCanceledAtAdmissionPreservesCurrentModel|TestSelectorCloseClosesAdmissionAndOwnsAllCleanups|TestModelSelectorReadQueryDuringPublish)$'
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/core -run '^TestModelSwitchWaitsForRunOwnerQuiescence$'
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/cli -run '^TestAssemblyCleanupIncludesFinalSwitchedModel$'
```

For the A/B ordering and close-versus-builder tests, repeat the named tests with `-count=20`. Verify the test names with `rtk go test -list` before recording them. Do not run full R3/R2 acceptance from this design slice. The first RED should reproduce selector B + Loop A after A's cleanup is released, a switch completing while a run still calls A, and a post-close switch retaining a newly built cleanup. A passing selector-only test is not evidence of safe production admission or Assembly cleanup.

## Required root decisions

1. Concurrent switch policy: gate-acquisition/commit order (recommended minimum) or last-invoked-wins with an invocation generation and stale-build disposal.
2. Callback reentry: reject nested mutation explicitly or queue it; read-only query reentry remains required. The current internal Loop setter is safe and synchronous.
3. Assembly close behavior when an R1 owner retains the run slot: synchronous wait, or bounded pending result with cleanup retained. The current `func()` cleanup signature cannot surface a timeout/error.
4. Confirm the compatibility boundary for direct `LoopAgent`/model references: this slice guarantees only Assembly's shared Harness/TurnRunner path; callers bypassing it own their borrow/quiescence discipline.

R2 run-lease release, R1 owner quiescence, and R3 resolver/provider identity remain independent gates. This design does not authorize native protocol enablement, retry changes, cost accounting, cycler/subagent resolver work, or a new global model-runtime framework.
