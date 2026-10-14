# R3 BUILD-2: streaming circuit-breaker ticket

Status: implementation design, checked against root HEAD `fb4d0cc216b3e121aa5afdec8597c7bd94e51a23` and the fixed BUILD-1 stream-join tree `cc195f9c07b77472734af30c8c2a4b285342d96c`. BUILD-1 is a separate review target at this point; recheck the final integrated SHA before implementing this slice. The earlier R3 design review remains authoritative for the loop-detector work and the close-after-join prerequisite.

## Frozen implementation boundary

The production adapter is `internal/cli/output_guard_adapter.go`; `output_guard_llm.go` does not exist in the inspected tree. `newModelWrapperWithChain` keeps the current order: cost wrapper, middleware chain, optional telemetry, `circuitBreakerModel`, then outer `outputGuardModel`. Assembly wraps the selected model with `ModelCycler` before the chain, so one outer breaker ticket also covers cycler selection/setup fallback, failover, retry, and any chain-owned continuation. Do not move the breaker into the chain or count provider setup attempts as separate tickets.

`production.CircuitBreaker` is the existing four-method mandatory interface. `DefaultCircuitBreaker.Execute` records synchronously when its closure returns, which is too early for a returned stream. `circuitBreakerModel.Stream` currently uses that method only around `inner.Stream`, and its open path manufactures a successful assistant final. Keep `Execute` and its explicit generic fallback option; both CLI model-adapter methods must propagate `production.ErrCircuitOpen` with no synthetic assistant content and no inner call. The current Generate adapter also turns that error into a success message, allowing a refused provider request to appear as a successful run. Removing that conversion is a narrow outcome-correctness repair, not a new fallback provider.

The fixed BUILD-1 tree exports `llm.LocalStreamAbortCause` / `llm.NewLocalStreamAbortCause`. A local abort may unwrap to `context.Canceled` or `context.DeadlineExceeded`, so terminal classification must check `errors.As` for the local-abort type before using `errors.Is` for caller cancellation. Caller cancellation is neutral only when the caller's cause won; a typed local abort while the caller remains live is a failure. If cancellation and a terminal race, use one documented first-observed event rule and test each ordering with explicit gates.

## Optional ticket API and state rules

Add a narrow optional capability alongside `CircuitBreaker`; do not add a method to the mandatory interface. Suggested production API:

```go
import "context"

// StreamOutcome is the terminal classification of one admitted stream ticket.
type StreamOutcome uint8

const (
	// StreamSucceeded records one valid, joined stream success.
	StreamSucceeded StreamOutcome = iota + 1
	// StreamFailed records one joined provider or local stream failure.
	StreamFailed
	// StreamCanceledNeutral releases admission without changing failure counters.
	StreamCanceledNeutral
)

// CircuitBreakerStreamTicket settles one streaming admission at most once.
type CircuitBreakerStreamTicket interface {
	// Complete reports whether this was the first completion in the current generation.
	Complete(StreamOutcome) bool
}

// StreamingCircuitBreaker is an optional streaming capability for a CircuitBreaker.
type StreamingCircuitBreaker interface {
	// BeginStream admits one logical stream call or returns a typed refusal.
	BeginStream(context.Context) (CircuitBreakerStreamTicket, error)
}
```

`*DefaultCircuitBreaker` implements the optional interface using the same state machine and mutex as `Execute`. A caller that only implements `CircuitBreaker` remains source-compatible; the adapter may keep the existing setup-only `Execute` path as its documented compatibility fallback, but it cannot promise terminal-aware accounting or pending HalfOpen reservations for that legacy implementation. The built-in breaker always takes the ticket path. Ticket admission never invokes a configured synthetic fallback.

`BeginStream` performs admission before calling the middleware chain. Open or a full HalfOpen probe budget returns `ErrCircuitOpen` and dispatches nothing. Each HalfOpen ticket reserves one slot until completion; success and failure settle that slot and update the existing thresholds, while neutral caller cancellation releases the reservation without changing success/failure counters. A stream that never closes keeps its ticket and HalfOpen slot pending.

Make completion one-shot. Bind each ticket to the breaker generation at admission; increment the generation on Reset and every state transition. A duplicate completion or a completion from an older generation is ignored and cannot rewrite the current state. This prevents delayed stream cleanup from affecting a later circuit epoch. Do not add a second state machine or alter the required `CircuitBreaker` interface.

## `circuitBreakerModel.Stream` lifecycle

1. If the breaker exposes `StreamingCircuitBreaker`, call `BeginStream(ctx)` before invoking `inner.Stream`. On refusal, return `nil, err` synchronously. Otherwise create a child attempt context with a cancel cause and call the complete wrapped chain once.
2. A synchronous setup failure is one logical failure after any BUILD-1-owned setup channel has joined. Reject nil-channel/nil-error setup as failure. Retry, failover, and cycler setup fallback stay inside this one admission. Once the chain hands out its output channel, never replay a later terminal error.
3. The breaker adapter owns one reader for the returned channel. Forward non-final chunks, hold the single valid Final, and do not publish that Final or complete success until the source channel closes. Require exactly one Final, no error, and no data/error after Final. Missing/malformed terminal, provider error, timeout, cap, or typed local abort is failure.
4. On a local failure or caller cancellation, stop forwarding, cancel the attempt with the preserved typed cause, and drain that same input channel to close. Then emit at most one error terminal and close the adapter output. Complete the ticket only after this join. If a source ignores cancellation, keep the output and ticket pending; do not start another drain goroutine or report success/quiescence.
5. If the actual caller cancellation wins before a terminal, complete neutrally after the source joins. Do not infer neutrality from a child context's `Err()` alone. A `LocalStreamAbortCause` is failure even though it unwraps to a cancellation sentinel.
6. Keep OutputGuard outside the breaker. In buffered-candidate mode it may reject after the provider stream has closed; that later policy decision must not write back to or revise the completed ticket. `LoopOwnsContinuation` continues to rely on the core reader's BUILD-1 join.

## Compile-success RED and acceptance tests

The first adapter regressions should compile against the current constructors and public API, before adding the optional ticket types. Use the real `newModelWrapperWithChain` helper and `DefaultCircuitBreaker`; controlled `BaseChatModel` sources make lifecycle assertions deterministic.

| Test | Current behavior it exposes / required assertion |
|---|---|
| Open stream refusal | Open a threshold-1 breaker with `Execute`, call the wrapped `Stream`, and assert `errors.Is(err, production.ErrCircuitOpen)`, `ch == nil`, zero model calls, and no assistant final. Current code returns a synthetic final, so this is RED. |
| Open Generate refusal | Through the same real model wrapper, call `Generate` with the breaker open and assert the typed open error, a nil message and zero model calls. In a core run, assert an error outcome rather than a successful assistant response. Retain the generic breaker's separately configured fallback tests. |
| Midstream error | Return a channel, then send one error and close. With threshold 1, drain the wrapper output and assert the breaker opens and the source was invoked once. Current `Execute` records setup success before reading the channel, so this is RED. |
| HalfOpen held through join | Use the existing fake clock to expire Open; return a source that sends Final then waits on a release channel. Before release, assert state is still HalfOpen and a second wrapped Stream is refused without another model call. Release/close, drain one final, then assert the ticket settles. Current code closes HalfOpen immediately on setup success, so this is RED. |
| Caller cancellation vs local abort | Cancel the caller while a source is held, then allow it to observe cancellation and close; assert neutral release and no failure count. Separately return/propagate `llm.LocalStreamAbortCause` with the caller live and assert a failure after join. Current code settles both as setup success. |
| Final held until source close | Send a valid Final but keep the source open; assert no successful terminal is published until an explicit close. Then close and assert exactly one Final. Also cover close without Final and post-Final error/data as failures. |
| Guard denial after provider close | Through the real outer output guard, return a valid closed provider stream and deny its candidate. Assert the guard rejects while the breaker remains successful. This is a placement regression guard; current topology should pass it. |

The open, midstream-error, HalfOpen, final-hold, and guard tests use constructors/APIs already present at root HEAD and can be made compile-success RED before adding ticket types. Add the `LocalStreamAbortCause` assertion after integrating the fixed BUILD-1 tree (that exported cause is not present at root HEAD `fb4d0cc`). Add package-level state tests with the optional API: HalfOpen reservation/release, success/failure thresholds, duplicate `Complete`, Reset/epoch invalidation, and stale completion after another ticket changes state. Then add an assembled `httptest` provider case: the first synchronous stream setup fails and a retry or failover succeeds, showing multiple setup dispatches inside one logical call and one ticket outcome. Include the configured ModelCycler in the assembled path or a focused wrapper test to prove selected-model setup fallback is covered by the same admission. Retain a separate post-handoff error case proving no retry/failover replay.

## Scope and verification gate

Files for BUILD-2 remain `internal/production/circuit.go` plus focused tests and the `circuitBreakerModel` section/tests in `internal/cli/output_guard_adapter.go`. Depend on the integrated BUILD-1 close contract before changing breaker accounting. Do not modify retry budgets, middleware order, OutputGuard policy, loop detection, durable run state, or app wiring.

Run the production ticket and CLI adapter tests first, then `GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/production ./internal/llm ./internal/core ./internal/cli`, followed by repository vet/lint and diff checks. This design bounds breaker settlement to joined stream lifecycle; it does not claim that canonical Glamour/model rendering or arbitrary providers are large-input bounded, nor that a non-cooperative stream source can be forcibly stopped.
