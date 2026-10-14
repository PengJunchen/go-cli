# R7 Assembly-close review

Reviewed frozen commit `93d7fc591ce325d38e2c24f6cabd2a0db67e924f` in worktree `r7-assembly-owner` (parent `d9dd6f3d4977cbf260ac9ef1445873828ab194e8`). This review covers its eight CLI lifecycle files and the production seams they call. The admission fence, run-slot idle contract, model-selector sealing, plugin and hot-reload join APIs, memory-worker wait, and pending-client retention are directionally sound. The commit is not accepted because the following shutdown paths either retain resources after known completion or can wait without a bound.

## Findings

### P1 — Successful built-in HTTP ACP shutdown is always classified as unknown

`internal/cli/assemble.go` constructs the production `grpc` ACP transport with `acp.NewGRPCAdapter`. That adapter is the repository's private HTTP ACP client. On a successful Connect, its Disconnect fences the generation, waits for its in-flight operations, sends `/disconnect`, and returns the stable terminal result. However, `internal/cli/assembly_lifecycle.go:607-630` only trusts ordinary Disconnect completion for `*acp.StdioAdapter`; every other ACP implementation is returned as `ErrAssemblyClientCompletionUnknown`, even when Disconnect returned nil. `closeResources` then retains downstream Assembly resources permanently.

The adjacent `waitACPConnect` helper at lines 585-604 has the same concrete-type restriction for ordinary Connect errors. A rejected HTTP handshake can be known to have completed, but the Assembly cannot join the built-in HTTP client's generation through this code path. Use an explicit, reviewed completion capability for the built-in adapter (including failed Connect cleanup) rather than inferring completion from an arbitrary implementation's nil error.

Evidence: a temporary real-peer probe created an `httptest` ACP endpoint, completed Connect, held the `/stream` request until Disconnect cancelled it, returned success from `/disconnect`, then called `assembleState.closeResources`. The call returned `disconnect ACP client: cli: client shutdown completion is unknown`; the EventBus subscription remained open. Probe `TestReviewProbeHealthyACPDisconnectLetsAssemblyClose`, command:

```text
rtk env GOMAXPROCS=2 go test -p 2 -run '^TestReviewProbeHealthyACPDisconnectLetsAssemblyClose$' ./internal/cli
```

The temporary probe was removed after execution. Add a maintained success-path regression alongside the existing real pending-cleanup test.

### P1 — Serve does not fence and join all server producers, and can block before Assembly close starts

`internal/cli/serve_cmd.go:250-256` calls `server.Stop(context.Background())` before invoking `closeAssemblyForCommand`. `HTTPServer.Stop` marks the server stopped, clears its listener references, closes sessions, cancels the server-level context, then calls `http.Server.Shutdown(ctx)` (`internal/acp/http_server.go:320-350`). A handler that does not leave after cancellation keeps Shutdown blocked indefinitely because its context has no deadline. The Assembly's fixed close budget is never entered.

Even when HTTP Shutdown returns, it is not a complete producer join: `handleSend` starts `s.handler.ProcessMessage` in an untracked goroutine (`internal/acp/http_server.go:564-580`), and `Start` launches `Serve` and `sessionCleanupLoop` without retaining completion channels (`:305-312`, `:774+`). Stop does not fence and account for those goroutines. A detached message handler can still use sessions or Assembly-owned dependencies after the HTTP handler has returned; a second Stop then returns nil because `running` was already cleared.

Give the server lifecycle one owned close operation: atomically fence and register dispatch work, retain/join the Serve, session-cleanup, and dispatch producers, and use a fixed shutdown budget. Keep Assembly dependencies alive while any producer remains unconfirmed. Merely bounding `http.Server.Shutdown` does not address detached dispatch workers. Add barrier tests for a non-cooperative HTTP handler and for a detached `ProcessMessage` worker; prove Stop is bounded, its completion remains pending until the real workers exit, and Assembly resources are retained in that interval.

### P2 — REPL flush ticker is stopped but not joined before session teardown

`internal/cli/repl_session.go:486-510` starts a producer goroutine. `stopFlushTicker` at lines 515-523 stops the ticker and closes the stop channel, but has no producer-exited signal and does not wait. If the producer has selected a tick, it may still be inside `PendingSessionWrites.Flush` or its logger callback while the close owner proceeds to final flush, `SessionStore.Save`, and `SessionStore.Close`. Flush calls serialize against one another and the JSONL store has a mutex, but that serialization is not proof that the producer exited before the backing store is closed; a buffered tick can also be selectable after the stop signal closes.

Add an actual goroutine-exited signal and make Assembly close join it before final flush and store teardown. Preserve the never-started/setup-failure path so it does not wait on a channel that was never owned.

Evidence: temporary probe `TestReviewProbeStopFlushTickerDoesNotJoinActiveFlush` used a controlled slog handler to block the ticker after a flush and observed `stopFlushTicker` return while that callback was still active. Command:

```text
rtk env GOMAXPROCS=2 go test -p 2 -count=1 -run '^TestReviewProbeStopFlushTickerDoesNotJoinActiveFlush$' ./internal/cli
```

The probe passed by demonstrating the missing join and was removed after execution.

## Validation

The maintained targeted CLI lifecycle/prompt/serve race selection passed:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^Test(AssemblyClose|AssemblyDoesNotTrust|CloseAssemblyForCommand|PromptCmd|Serve)' ./internal/cli
```

Result: `ok github.com/pengjunchen/go-cli/internal/cli 2.509s`.

This selection does not cover the missing healthy built-in ACP path or adversarial serve producers. The temporary ACP and ticker probes establish their stated observations; the serve finding is based on the source lifecycle seams cited above. No claim is made about Linux runtime validation by this review.

## Server-generation delta review

Reviewed the fixed HTTP server lifecycle chain through `889209ac38f3c0a3def8ec3474cc9a2d593fca6b` (`5677fcb`, `5145005`, `d88b9a3`, and `889209a`) in an isolated detached worktree. The server-local portion now fences admission under the same mutex used to register request and dispatch work, joins `Serve`, session cleanup, registered request handlers, and detached `ProcessMessage` workers through one close owner, and only permits restart after that generation's `closeDone`. `Stop` waiters can return their own context error while the owner retains the pending cleanup; the maintained logger-barrier regression covers the earlier issue where a blocking stop log delayed that waiter. Worker panics are contained as sanitized local failures. The full ACP package race suite passed on the fixed head:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 ./internal/acp
```

Result: `ok github.com/pengjunchen/go-cli/internal/acp 28.000s`.

### P2 — HTTP-client local-join helper is unsafe for typed nil and does not fence reconnect

`internal/acp/grpc_adapter.go:IsBuiltInHTTPClientLocallyJoined` accepts an `ACPClient`, type-asserts it to `*gRPCAdapter`, and dereferences it without checking whether the asserted pointer is nil. A typed-nil `*gRPCAdapter` therefore panics instead of returning false. The helper also snapshots `current` and `cleanupDone` under the adapter mutex, unlocks, and then checks the old channel. If cleanup closes that channel and another goroutine calls `Connect` before the helper returns, it can report true while a new generation is live. Treat this as an instantaneous local observation only: callers must fence future Connect calls for the observation to support resource teardown. It says nothing about remote DELETE success.

The helper is not wired into production CLI assembly at this commit, so it does not close the earlier healthy built-in HTTP ACP finding: the Assembly still needs a reviewed caller-side fence and completion join before it can release resources. Add a typed-nil regression and either keep the channel check within the lock with an explicit instantaneous-observation contract, or provide an atomic close-generation proof if callers can reconnect concurrently.

This delta review accepts the HTTPServer-local producer join and Stop-waiter behavior only. It does not change the overall R7 rejection: the healthy built-in ACP Assembly path, REPL ticker join, and serve-command wiring findings above remain open. No Linux runtime claim is made.
