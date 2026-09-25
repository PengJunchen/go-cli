# R7 tracing lifecycle design review

## Verdict

**Audit accepted; implementation handoff needs a narrower first slice and two explicit lifecycle contracts.** The audit is grounded in the fixed source and correctly keeps this proposed follow-up outside the currently authorized R7-BLD-02 telemetry scope. It does not authorize implementation or claim lifecycle closure.

## Fixed snapshot and scope

- Reviewed design commit: `6cb173d9ce8e7e9b9f86f9c451cbd5f6565792de`, in a clean detached worktree.
- Audited source baseline named by the document: `a245a37c3cbcde6c49c246f16bfd69e245ad1524`.
- Compared against `r7-lifecycle-design.md` and `execution-tasks.json`; no product source or execution ledger was changed, and no tests were run for this docs-only review.

## Source-grounding result

The six findings are supported by the named source: `AsyncExporter.ExportSpan` returns nil on a full queue and has no post-close admission gate; `Shutdown` closes `done` once but waits without observing `ctx` and calls the inner `Shutdown` for every caller; the worker gives each `ExportSpan` a fresh background timeout; and the channel remains writable after the worker exits. The interface comment still promises at-least-once delivery. The CLI passes its signal-cancelled context to exporter shutdown, while `Tracer.Flush` is empty. Assembly exposes a bare `Cleanup func()`, discards extension shutdown errors, and registers final cleanup so it closes event/session/memory resources before earlier extension disposers. Prompt/REPL spans are finalized as OK on error paths, and the current R1 run-slot source has no shared close-admission fence.

The document also correctly records that root and assembly now reuse one tracer, excludes the already-fixed stdout preflight, and distinguishes the partial telemetry authorization from the proposed tracing/CLI work. I found no stale double-tracer or unsupported-sink claim in this audit.

## Required design tightening before an implementation handoff

1. **Stage TDD around the single exporter owner first.** Keep the first executable slice to `AsyncExporter` admission, one shutdown owner/future, queue/delivery accounting, and its four focused gated-fake regressions: post-close submit, queue-full/failure report, exactly-once close, and uncooperative callback with retained owner. The current table then expands into the R1 admission fence, `AgentAssembly.Close`, prompt/REPL/serve wiring, root CLI exit propagation, and a subprocess smoke in the same follow-up. Those are dependent integration work and should be a later, separately authorized stage after BLD-01/R1 contracts are accepted. This avoids creating parallel close owners or a second cleanup framework while the first exporter lifecycle is still being specified.

2. **Specify callback context separately from waiter context.** State which context the single `inner.Shutdown` callback receives, what happens when its cooperative deadline expires, and that each caller may stop waiting without cancelling or replacing the shared owner. The owner must keep the sink reachable and finish late cleanup exactly once; a caller's `cleanup_pending` wait result must remain distinguishable from an actual callback error that happens to have the same typed error. The current phrase “according to the remaining deadline” is not enough to implement or test this boundary.

3. **Define report counters as process-level outcomes, not durability.** In this implementation, “delivered” can mean only that `inner.ExportSpan` returned nil. It cannot imply an `fsync` or durable JSONL commit. Define accepted, sink-returned-success, sink-failure, abandoned, rejected-after-close, and pending as disjoint or otherwise explicitly related outcomes, and ensure one final result is shared by concurrent/repeated shutdown callers. Do not add a second polling/stats surface; the audit correctly recommends one finalizer report.

4. **Keep failure injection private and bounded.** The proposed CLI subprocess test needs an exporter failure seam that production callers cannot toggle through environment/configuration. A narrow unexported factory used by package tests is adequate if restoring it is race-safe; otherwise retain the subprocess smoke for real JSONL success and test shutdown error propagation at the narrow `run` seam. Do not introduce a production test-bypass setting.

## Test-plan assessment

The gated fake exporter and mandatory gate release/owner wait are appropriate. The focused exporter tests exercise current RED conditions. The rest of the named regressions are valid acceptance targets but are not a minimal first TDD slice: `TestAssemblyCloseClosesSharedAdmissionAndRetainsR1Owner`, command call-site error propagation, and `TestCLITraceShutdownFailureExitsNonzero` each require separate cross-layer dependencies and should be sequenced after the exporter owner contract. If later kept, the signal test must prove use of a fresh bounded cleanup context derived without inheriting signal cancellation; it must also distinguish callback deadline from a caller merely timing out while waiting.

The review did not execute implementation tests because this fixed commit only adds an audit/design document. The document correctly says its proposed tests have not run and that the existing gates remain open.

## Acceptance boundary

Accept the source audit and its explicit authorization boundary. Do not treat the current design as permission to change `internal/tracing`, `assemble.go`, command call sites, or `run_slot.go`. Before a future implementation task starts, split the exporter-owner tests from dependent assembly/CLI integration, and freeze the callback-context and outcome-counter definitions above. R7 lifecycle, assembly/CLI propagation, shared close admission, and command-level trace evidence remain open.

## Fixed follow-up review at `e5e8d608114042269363cc6f50138976c5a6c164`

**The new Shutdown and delivery definitions are accepted, but the first exporter slice still needs two narrow corrections before approval.** Reviewed this docs-only commit against parent `6cb173d9ce8e7e9b9f86f9c451cbd5f6565792de` in a clean detached worktree. No implementation tests were run.

### Accepted changes

- `Shutdown(waitCtx)` now clearly makes the caller context a waiter deadline only. The sole close owner uses a separate callback context for its one `inner.Shutdown`; a waiter returning `ShutdownPendingError` does not cancel or replace that owner. A callback's actual returned error preserves its error chain and is distinct from a waiter timing out; a callback that ignores its context remains pending until it really returns.
- `delivered` now means only that the corresponding `inner.ExportSpan` returned nil. The counter equation partitions every accepted span into delivered, failed, abandoned, or snapshot-pending; the inner `Shutdown` error is separate from per-span delivery results.
- The first TDD slice is separated from the R1 admission, Assembly.Close, prompt/REPL/serve, and CLI integration gates. The four focused fake-exporter tests and the explicit “do not claim full CLI lifecycle from exporter tests” boundary are sound.

### Required corrections

1. **R7-TRACE-DES-1 [P1] Define one shared drain deadline.** The design says queued spans drain “within owner deadline” and that spans not handed to the sink at that deadline become abandoned, but it defines only `shutdownTimeout`, created after the worker has joined and used for `inner.Shutdown`. It does not define when the drain deadline starts or how long it lasts. The per-span `inner.ExportSpan` context is described as independently bounded without a duration or a relationship to the total drain budget. If the existing per-span timeout is restarted for every span, a full queue can extend the drain by many multiples of that timeout; if an exporter ignores cancellation, the owner has no point at which it can classify never-started spans as abandoned.

   Create one immutable drain deadline when the first `Shutdown` transitions to closing. Do not reset it per span. Bound each active export callback by the lesser of its per-callback budget and the remaining shared drain budget. On drain-deadline expiry, classify accepted spans not yet handed to the inner exporter as abandoned; an in-flight callback remains pending/quarantined, the sink stays retained, and `inner.Shutdown` must not run concurrently with it. Once the worker actually joins, run the single `inner.Shutdown` callback with its separately defined one-shot 10-second cooperative budget. Keep the two phases and their counters/status distinct. Add a gated regression proving a later span does not receive a fresh total drain budget.

2. **R7-TRACE-DES-2 [P1] Keep the existing SIGINT caller alive long enough to observe the owner.** The only production caller of the default async exporter is `cmd/cli/main.go`: `run` passes its `signal.NotifyContext` context to `exporter.Shutdown(ctx)`, and `main` immediately calls `os.Exit(run(...))`. When SIGINT/SIGTERM has canceled that context, the new waiter-only contract can return pending immediately while the close owner continues in a goroutine; `os.Exit` then terminates the process and that owner before it can finish draining or close the sink. This is a behavior regression even though full CLI/assembly lifecycle integration belongs later.

   Include the minimum finalizer change in the first exporter slice: give the current CLI call a bounded wait context that does not inherit signal cancellation, then propagate an actual final error or pending-at-deadline result to stderr and a nonzero exit. The wait budget must cover the configured drain and callback phases, or, if it expires sooner by explicit policy, the CLI must report pending/failure rather than a clean close. Add a canceled-signal-context subprocess or `run`-seam regression that proves the finalizer waits for a gated owner or exits with the pending failure after its bounded budget. This does not pull `AgentAssembly.Close`, R1 admission, prompt/REPL/serve wiring, or the later cross-layer gates into the first slice.

### 10-second budget and disposition

The `inner.Shutdown` 10-second callback budget does not conflict with an existing caller deadline by itself: the current production caller has no fixed timeout on normal completion, but its signal context is already canceled on SIGINT/SIGTERM. The conflict is that the proposed waiter semantics expose that cancellation immediately to `os.Exit`. Once the finalizer uses a cancel-independent wait, document whether the intended worst-case is one drain budget plus the separate 10-second callback budget; otherwise define how the finalizer deadline cuts the owner short and reports pending. The current 10-second callback timeout alone cannot stand in for a bounded drain deadline.

The e5e delta therefore improves the callback/error and delivery contracts and correctly narrows the TDD stages, but I do **not** approve the first exporter slice until the shared drain budget and minimum current-caller finalizer behavior are specified. Full Assembly/R1/serve integration remains a later gate.
