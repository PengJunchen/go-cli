# R7 MCP hot-reload lifecycle implementation review

**Reviewed source:** `f67a1c1a532dd68b5290f3f1fb96a2d7f80c5ead` (`codex/runtime-r7-mcp-hotreload`). Review used a separate detached worktree; only `internal/mcp/hot_reload.go` and `internal/mcp/hot_reload_test.go` were treated as implementation scope.

**Verdict:** The shutdown owner and reconnect lifecycle are sound for the tested default path, but this slice needs one small input-boundary fix before acceptance: a non-positive poll interval can panic in the poll goroutine and crash the process.

## Finding

### R7MCP-01 — A zero or negative configured poll interval panics outside lifecycle recovery

**Severity:** P2; programmatic option misuse, not a currently observed CLI configuration path.

`WithPollInterval` accepts any `time.Duration` ([`hot_reload.go`](../../../../internal/mcp/hot_reload.go:147)); `NewDefaultHotReloader` copies the value without validation (lines 229–252). `Watch` returns nil after publishing the poll goroutine (lines 258–289). The goroutine calls `time.NewTicker(h.pollInterval)` before installing any defer ([`hot_reload.go`](../../../../internal/mcp/hot_reload.go:393)). A zero or negative duration panics there. The reconnect owner’s recovery at lines 475–485 cannot catch a panic in the separate poll goroutine, so the panic is process-wide and `Done` cannot report a sanitized terminal result.

The preserved subprocess probe calls `Watch` with `WithPollInterval(0)`: `Watch` accepts it and the child exits with `panic: non-positive interval for NewTicker`. See [`r7-mcp-hotreload-probes.go.txt`](../evidence/r7-mcp-hotreload-probes.go.txt) and [`r7-mcp-hotreload-invalid-poll-panic.txt`](../evidence/r7-mcp-hotreload-invalid-poll-panic.txt). A repository search found no current production/configuration call site passing this option; this is an exported constructor-option boundary, not a default CLI failure.

Reject non-positive intervals before starting the poller (preferably with a `Watch` error, since the constructor returns no error), or normalize them to the documented default. Add zero and negative interval tests proving no goroutine is published and no process panic occurs. Do not treat a recovered panic after poll startup as equivalent to successful completion; any recovery path must also close the lifecycle channel only after the poll goroutine has actually unwound.

## Lifecycle checks that passed source review

- `HotReloadLifecycle` is an additive optional capability; the existing `HotReloader` interface is unchanged ([`hot_reload.go`](../../../../internal/mcp/hot_reload.go:21)). `Done` is allocated once and returns the same channel; `RequestStop` fences future admissions, cancels the watcher context, closes the stop signal, and does not wait for a client or callback. `Wait(ctx)` only bounds that caller’s wait and does not request stop. `Stop` is the blocking convenience operation.
- Completion is not published until `stopped` is true, no reconnect owner is active, and the started poller has exited ([`hot_reload.go`](../../../../internal/mcp/hot_reload.go:358)). `runCycle` defers span end, returns before the reconnect owner clears `connecting`, and the poller sets `pollExited` only in its exit defer. Thus a completed `Done` includes the actual poll/reconnect/register callback and span-end work for the normal, stop, and recovered worker-panic paths.
- Reconnect admission is protected by `mu`: the first request sets `connecting` before launching its goroutine; overlapping changes set one `pending` bit. The owner consumes pending work without releasing `connecting`, or clears it under the same lock used by `RequestStop` and new admissions. There is no release-then-launch gap in that state transition.
- Each stage checks the stop fence before admission, and checks again after each potentially uncooperative MCP call and registration callback before advancing to the next stage. `RequestStop` cancels the shared poll/reconnect context and does not wait for an active callback; `Done` remains open until it returns.
- Callback panic is converted to the fixed `HotReloadCallbackPanicError`, with the panic payload omitted. The reconnect owner separately converts unexpected reconnect-worker panics to `HotReloadWorkerPanicError`. A temporary client-panic probe confirmed the fixed error, no payload disclosure, and `Done` after worker unwind. The existing registration-panic test also checks that the reconnect span is exported before `Done` closes.
- The `Watch`/`RequestStop` race publishes and starts the poll owner under the same mutex, and the 25-iteration test exercises that fence. `RequestStop` before `Watch` closes the stable completion channel and prevents a later poller from being published.

These checks establish the local hot-reload owner lifecycle only. They do not prove full default assembly ownership or MCP transport reconnection against a live external server.

## Validation

`go test -list` confirmed the 14 existing `TestHotReloader*` tests plus the two temporary review probes. The exact probe list is saved in [`r7-mcp-hotreload-test-list.txt`](../evidence/r7-mcp-hotreload-test-list.txt).

Targeted lifecycle and probe race tests passed twice:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/mcp -run '^(TestHotReloader.*|TestReview.*)$' -count=2
```

The full MCP package race suite also passed:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/mcp -count=1
```

Outputs are preserved in [`r7-mcp-hotreload-target-race.txt`](../evidence/r7-mcp-hotreload-target-race.txt) and [`r7-mcp-hotreload-full-mcp-race.txt`](../evidence/r7-mcp-hotreload-full-mcp-race.txt). The invalid-interval subprocess probe passed by confirming the current process panic; it is not a green product behavior. No full-repository suite or assembly-level MCP hot-reload test was run.

## Fixed follow-up

**Reviewed delta:** `c195d8c6da028de45bdcf9abb159d718ecf49739` (parent `f67a1c1a532dd68b5290f3f1fb96a2d7f80c5ead`). It changes only `internal/mcp/hot_reload.go` and `internal/mcp/hot_reload_test.go`.

**Verdict:** R7MCP-01 is closed; the fixed delta is accepted for the bounded hot-reload slice.

`Watch` now checks `pollInterval <= 0` as its first operation and returns `ErrHotReloadInvalidPollInterval` before validating or reading the config path, mutating watch state, or starting an owner ([`hot_reload.go`](../../../../internal/mcp/hot_reload.go:263)). The zero/negative interval subprocess test verifies no poll owner or watch state is published, the instance remains unstopped, and `RequestStop`/`Wait` still complete `Done`. A positive interval remains usable, and the default interval constant and constructor defaults are unchanged. This closes the process-panic path without widening lifecycle semantics.

On this fixed snapshot, `go test -list` found the invalid-interval regression plus positive-interval and no-watch-stop tests. Those three tests passed under `-race -count=5`; the full `./internal/mcp` race suite also passed. Evidence is preserved in [`r7-mcp-hotreload-fixed-c195-test-list.txt`](../evidence/r7-mcp-hotreload-fixed-c195-test-list.txt), [`r7-mcp-hotreload-fixed-c195-target-race.txt`](../evidence/r7-mcp-hotreload-fixed-c195-target-race.txt), and [`r7-mcp-hotreload-fixed-c195-full-mcp-race.txt`](../evidence/r7-mcp-hotreload-fixed-c195-full-mcp-race.txt). This follow-up still does not claim assembly-level acceptance.
