# Independent review: stream ownership joins at `cc195f9`

Reviewed the fixed detached target at `cc195f9c07b77472734af30c8c2a4b285342d96c`, with `86c89fc` as its parent and `17ab7b7` as the review base. This review covers BUILD-1 (`86c89fc`) and the separate cycler extension (`cc195f9`) from `reports/streamjoin-builder.md` and the 2026-09-25 R3 breaker-loop handoff.

## Verdicts

- **BUILD-1 `86c89fc`: REJECT, P2.** The stream total timeout no longer bounds the synchronous `next.Stream` setup call.
- **Cycler extension `cc195f9`: REJECT, P2.** A source `MessageChunk.Error` is forwarded but is not treated as terminal, so the selected source is neither canceled nor joined on that path.
- **No P1 found.** The findings are independent; fixing one does not resolve the other. This slice review does not mark R3 complete or establish the native CLI gate.

## Findings

### P2 — total timeout starts after synchronous stream setup (`86c89fc`)

In [`middleware_timeout.go`](../../../../internal/llm/middleware_timeout.go#L105), `timeoutModel.Stream` creates only a cancel-cause context and immediately calls `t.next.Stream` at lines 107–109. The configured `totalTimeout` timer is not created until line 146, after that call has returned. A provider whose `Stream` waits synchronously for response headers therefore receives no total-timeout deadline during setup. The HTTP provider does this synchronous wait in [`provider.go`](../../../../internal/llm/provider.go#L513), at `m.client.Do(req)`.

This regresses the base behavior: at `17ab7b7`, the timeout middleware wrapped `ctx` with `context.WithTimeout(ctx, t.totalTimeout)` before calling `t.next.Stream`. A deterministic temporary probe blocked inside `next.Stream`, configured a 15 ms total timeout, and allowed setup to return only after 80 ms. The target did not return at the timeout; after releasing setup it returned `setup released`, not `context.DeadlineExceeded`. The probe file was removed after the run.

Restore total-timeout coverage across setup while retaining the new same-owner cancellation and source-join behavior. Add a regression test where setup waits on `ctx.Done()` and assert the call returns on the configured deadline without an external release.

### P2 — cycler does not join a source after forwarding an error chunk (`cc195f9`)

The `MessageChunk.Error` contract says the caller should stop consuming and return the error ([`llm.go`](../../../../internal/llm/llm.go#L207)). In [`cycler.go`](../../../../internal/llm/cycler.go#L323), the forwarding loop checks caller cancellation and source closure, then forwards each received chunk at lines 352–363. It has no `chunk.Error` terminal branch. If a selected model sends an error chunk and then waits for its attempt context to be canceled before closing its channel, the cycler leaves its forwarding goroutine, provider cleanup, and output close pending indefinitely when the caller correctly stops at the error chunk.

A deterministic temporary probe used a selected model that emitted one buffered error chunk and then waited on `ctx.Done()`. Reading the error did not cancel the selected attempt within 60 ms, so cleanup and output close did not complete until the probe's emergency release. The probe file was removed after the run.

Handle the error chunk as a local terminal: preserve/forward the error, cancel the selected attempt with the typed local-abort cause, drain and join its source, then run cleanup before closing the cycler output. Do not turn a stream error into fallback after output has begun.

## Reviewed behavior that passed source review

The seven LLM middleware readers cancel and drain the same source for consumer cancellation, middleware-detected error, output caps, and post-final data; setup errors that return a channel are joined before the setup error is returned or fallback/retry begins. They reject `(nil, nil)` streams and hold a successful final chunk until the source is closed. The core guarded reader follows the same join rule, rejects nil streams, joins setup-error channels, and rejects a closed stream without a final chunk.

The CLI output guard joins error, cap, cancellation, setup-error, and malformed-after-final paths. Its deliberate behavior after the inner source has already closed is covered: a later output-guard denial does not cancel the provider context. The assembled HTTP test is a real loopback request through the assembled CLI; its `RoundTripper` wraps the actual response body and blocks `Close()` return, and the test verifies the event output remains open until that body-close barrier is released. This is a valid cleanup barrier, not a provider-only synthetic red test.

For the cycler, ordinary source close runs provider cleanup before closing output; caller-cancel paths propagate a typed local-abort cause and join the source first; setup-error channels are joined before fallback; nil streams are rejected; and `ContinuationNoReplay` prevents fallback on both model-build and stream-setup failures. The missing terminal handling for an error chunk is the remaining finding for this extension.

The wrapper composition puts the middleware chain inside telemetry, circuit breaker, and output guard. The circuit breaker executes only the synchronous `inner.Stream` setup call and returns once it receives the stream channel; later local cancellation of a middleware-owned child context does not reach the already-returned breaker call as a new error. I found no successful-child-cancellation misclassification in this composition.

## Validation

At the exact target source SHA, the following passed:

- Test inventory: `rtk run go test -list '^Test' ./internal/llm ./internal/core ./internal/cli` (exit 0).
- Full race pass: `rtk run env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/llm ./internal/core ./internal/cli` (completed before adding temporary review probes; product source was unchanged afterward).
- Focused race passes: LLM `TestOwningMiddlewareReaders` (`-count=5`) and `TestStream` (`-count=3`); core `TestStreamGenerate` (`-count=5`); CLI `TestOutputGuard` (`-count=3`) and `TestAssembledStreamJoinsProviderBodyCleanupBeforeEventClose` (`-count=3`).
- `rtk go vet ./internal/llm ./internal/core ./internal/cli`.

Both temporary RED probes failed against the reviewed target for the reasons above and were deleted. The review worktree returned to a clean state at `cc195f9c07b77472734af30c8c2a4b285342d96c`.
