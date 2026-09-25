# R7 Async tracing shutdown handoff

Status: implementation-ready narrow follow-up to the fixed e5 review, pending its incremental confirmation. This handoff authorizes only the first `AsyncExporter` owner slice and the `cmd/cli` root finalizer needed to keep that owner alive. It does not close R7 lifecycle, Assembly, R1 admission, prompt/REPL/serve, or exporter protocol gates.

## Fixed source and review basis

- Design baseline: `e5e8d608114042269363cc6f50138976c5a6c164`.
- Source locations: `internal/tracing/async.go` currently starts a fresh five-second context per span, waits on `wg` without observing a caller deadline, and calls `inner.Shutdown` for every invocation. `cmd/cli/main.go` creates a `signal.NotifyContext`, ends the root span, calls `exporter.Shutdown(ctx)`, and returns to `main`, which immediately calls `os.Exit(run(...))`.
- The e5 review accepted the single close owner, waiter-only context, final outcome accounting, and the split from Assembly/R1 work. This handoff closes only its two remaining findings: one shared drain deadline and the current CLI caller's bounded wait.

## Shutdown timing contract

The first `Shutdown` call that changes the exporter from open to closing fixes one immutable drain deadline at `now + 10s`. Later `Shutdown` calls join that same owner and cannot extend or replace its deadline. `ExportSpan` calls after the state change fail with the existing typed closed error and increment `rejected_after_close`; the queue and state transition must be synchronized so a concurrent send cannot succeed after close admission.

Each active `inner.ExportSpan` callback receives a context whose deadline is the earlier of its five-second per-callback limit and the remaining shared drain deadline. A callback that started before the first `Shutdown` keeps its already-created five-second context; it is not restarted or assigned a new deadline. Since that callback's remaining cooperative lifetime is at most five seconds and the shared drain budget is ten seconds, it will reach its callback deadline no later than the shared drain deadline. If it ignores cancellation, it remains pending/quarantined instead of being treated as stopped. A span accepted into the bounded queue is never granted a new total drain period merely because it is later in the batch. The worker must stop handing off queued spans at the shared deadline. At that boundary, accepted spans not yet handed to `inner.ExportSpan` become `abandoned_on_shutdown`; a callback already handed to the sink but not yet returned remains `pending`/quarantined. The owner retains the sink and all state required for late completion. It must not start `inner.Shutdown` while an `ExportSpan` callback is still running.

Only after the worker and any active `ExportSpan` callback actually return may the close owner invoke `inner.Shutdown`, exactly once, with its own `context.WithTimeout(context.Background(), 10s)` callback context. This callback budget is independent of the drain deadline. A cooperative callback returning an error records that actual error; a callback that ignores its deadline remains pending until it returns. No timeout is evidence that the callback stopped.

The complete cooperative close window is therefore at most 20 seconds: one shared 10-second queue-drain phase followed, after worker join, by one 10-second inner-shutdown phase. Pending spans and close errors remain distinguishable from successful closure. `delivered` means only that `inner.ExportSpan` returned nil; it does not claim durable storage. For accepted spans, the one final outcome partitions them as `delivered + sink_failure + abandoned_on_shutdown + pending`; queue-full drops and post-close rejections were never accepted. The inner shutdown error is reported separately from span outcomes. Concurrent/repeated shutdown callers observe the same final owner result, while an individual waiter's context can stop only that wait.

## Root CLI finalizer

In `cmd/cli/main.go`, preserve the existing order of ending the root span before exporter shutdown, but do not pass the canceled signal context directly to the waiter. Derive one finalizer context from `context.WithoutCancel(ctx)` with a 20-second timeout matching the two default owner phases. Keep it alive until `Shutdown` returns or that bound expires; only then may `run` return to `main`/`os.Exit`. On callback failure or pending-at-finalizer-deadline, write a bounded diagnostic to stderr and return a nonzero code. The stderr message must use a stable safe classification and must not interpolate arbitrary callback error text or span payload; the returned error chain still preserves the actual callback error for programmatic callers. The finalizer must not turn a signal-canceled invocation into an immediate waiter timeout followed by `os.Exit` while the owner is still draining.

This is only the root exporter finalizer. It does not add an Assembly cleanup framework, alter command signal behavior, or claim that an in-process callback ignoring context can be forcibly stopped. The bound lets the command observe a truthful pending result before exit; if the finalizer reaches its own deadline, it returns failure rather than reporting clean shutdown.

## TDD and acceptance

Write failing tests before implementation. Keep timeout values injectable only through unexported test construction; production callers cannot change them through environment or config.

1. In `internal/tracing`, preserve the four e5 focused gates: post-close rejection; queue-full and sink-failure accounting; exactly-once `inner.Shutdown` across concurrent callers; and waiter timeout while an uncooperative callback retains the owner until actual return.
2. Add `TestAsyncExporterSharedDrainDeadlineDoesNotResetPerSpan`: use a gated fake and short injected budgets to prove later callbacks receive only the remaining shared time, not a renewed drain budget. Also assert that spans still queued at expiry become abandoned, an in-flight non-cooperative callback remains pending, and `inner.Shutdown` does not overlap it; after release, the same owner joins the worker and invokes inner shutdown once.
3. In `cmd/cli`, exercise the exact finalizer helper called by `run` with an already-canceled signal context and a gated exporter. Prove it waits on a cancel-independent bounded context until the owner completes, and prove the bounded pending/error path returns nonzero before the process exit boundary. Use short test-only bounds; production remains the documented 20 seconds.
4. Run the named tracing tests and CLI finalizer tests with `GOMAXPROCS=2 go test -p 2 -race -count=1` on their exact package paths. Preserve both RED and GREEN commands and exit codes. Do not run full repository `make verify` in this slice; root owns the integrated gate.

The final report must distinguish a returned sink callback from durable delivery and a pending callback from a stopped callback. Passing these unit/race gates is not proof of Assembly cleanup order, R1 quiescence, prompt/REPL/serve propagation, or full R7 acceptance.
