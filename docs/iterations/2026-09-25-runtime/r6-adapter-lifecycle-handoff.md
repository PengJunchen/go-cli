# R6 ACP, MCP, and Web adapter lifecycle handoff

**Status:** implementation handoff, source unchanged. Audited at base `779bad1195516fa15f5cbf3896a59a5b622d4e74`. This narrows `r6-entrypoint-design.md` and R6-BLD-02 into source-owned changes and executable acceptance; it does not claim R6 is implemented or that dependent R1/R2/R4 work is complete.

## Contract boundary

| Surface | Supported claim for this lane | Not a supported claim |
|---|---|---|
| ACP HTTP | Versioned, authenticated private `go-cli-acp-json/1` profile over the existing JSON routes. | Official ACP interoperability or gRPC. The existing `grpc_adapter.go` is JSON over HTTP. |
| ACP stdio | The current `StdioAdapter` is a library client over caller-owned `io.Reader`/`io.Writer`; bound frames and make its owned workers/writes safe. | An inbound ACP agent process. There is no `acp stdio` CLI command today. |
| MCP HTTP | MCP `2025-06-18` Streamable HTTP only, with a real local peer proving headers, session and stream behavior. | Implicit legacy HTTP+SSE fallback, arbitrary supported-version negotiation, or replaying an uncertain `tools/call`. |
| Web | Unavailable prototype until a real Agent/run/session owner is connected. Return unavailable instead of successful placeholder chat; reject unauthenticated non-loopback serving. | A working Web agent journey or production Web CLI entrypoint. No Web command is registered in `internal/cli/cli.go`. |

These identities follow the approved [R6 entrypoint design](r6-entrypoint-design.md#L60) and the R6-BLD-02 deliverable and dependency list in [execution-tasks.json](execution-tasks.json#L1436). Keep `sender_id`, tool names, or project trust as labels only; none is an authorization capability.

## Audited source gaps and minimum changes

### ACP HTTP and `serve`

The current server registers `/connect`, `/send`, `/disconnect`, `/stream`, and `/events` with no profile-version middleware or HTTP read/header/idle limits; `Start` starts untracked Serve and cleanup goroutines ([`http_server.go:181-243`](../../../internal/acp/http_server.go#L181)). `/send` launches an untracked goroutine and returns immediately, so the HTTP semaphore is released before the dispatched work finishes ([`http_server.go:393-447`](../../../internal/acp/http_server.go#L393)). `Task.ID` is derived from `sender_id`, so concurrent requests from one sender collide ([`core_handler.go:231-255`](../../../internal/acp/core_handler.go#L231)). The session response has a server ID, but the map is still keyed by the client sender alias ([`http_server.go:355-391`](../../../internal/acp/http_server.go#L355)).

The authenticated routes are internally inconsistent: connect/send route by body `sender_id`, while stream/events replace it with the single auth subject ([`http_server.go:376-383`](../../../internal/acp/http_server.go#L376), [`http_server.go:416-433`](../../../internal/acp/http_server.go#L416), [`http_server.go:495-500`](../../../internal/acp/http_server.go#L495), [`http_server.go:560-565`](../../../internal/acp/http_server.go#L560)). The event endpoint replays a session ring and then subscribes to the process-wide EventBus, whose contract is “all events” for each subscriber ([`http_server.go:584-612`](../../../internal/acp/http_server.go#L584), [`eventbus.go:13-20,42-63`](../../../internal/core/eventbus.go#L13)). This can cross session/task boundaries and has a replay-to-subscribe gap. `Session.pending` is an unbounded slice, and its bounded event ring/live channel do not report eviction as a gap ([`core_handler.go:26-79,105-151`](../../../internal/acp/core_handler.go#L26)).

`serve` already defaults to loopback and authentication, writes generated tokens privately, and rejects `--no-auth` on non-loopback ([`serve_cmd.go:94-149`](../../../internal/cli/serve_cmd.go#L94)). But failed or absent Agent assembly currently falls back to echo mode ([`serve_cmd.go:151-203`](../../../internal/cli/serve_cmd.go#L151)); Stop errors are logged and Cleanup is called without affecting the command result ([`serve_cmd.go:246-267`](../../../internal/cli/serve_cmd.go#L246)). The listening message reports the requested address instead of the actual listener address, which makes `:0` unsuitable for a real subprocess probe ([`serve_cmd.go:216-244`](../../../internal/cli/serve_cmd.go#L216)).

Implement the existing HTTP server as a small transport/session adapter around the existing dispatcher and R1 run owner:

1. Require `X-Go-CLI-ACP-Profile: go-cli-acp-json/1` on every HTTP route and the same exact version in the connect envelope. Reject missing/unknown profile before creating session or task state. Keep JSON HTTP and call it a private go-cli profile.
2. Resolve the authenticated principal before session lookup. Mint an opaque `session_id`; treat `sender_id` only as a display alias. Route every endpoint through the same principal + opaque session binding; a body field may only be checked for consistency. Use a fresh opaque `task_id`/request ID for each accepted dispatch, never `sender_id` as task identity.
3. Reserve one bounded dispatch slot before acknowledging `/send`. The reservation must cover the actual dispatched goroutine through join/terminal accounting, not only the HTTP handler. Queue full returns explicit overload (503); do not start unbounded goroutines. Keep a minimal task record (IDs, authenticated principal, cancel function, completion, bounded result/event projection) and delegate execution/quiescence to R1; do not build another run owner.
4. Bind `/cancel`, `/disconnect`, `/stream`, `/events`, and the approved-design `/approval/respond` route to the exact principal/session/task. A disconnect closes that session's admission and requests cancellation for its owned tasks; it cannot erase still-running records. Apply request/header/idle/body bounds, but do not impose a global write deadline that truncates a live SSE response. Keep long-lived SSE slots separate from dispatch capacity.
5. Feed each task/session from its own R1 run-event projection; do not subscribe the endpoint to `core.EventBus` globally. Preserve a bounded replay ring. Report an explicit transport `event_gap` when the requested cursor predates retained events or producer/subscriber backpressure drops events; distinguish this from R1's run-level `EventGapCount`. Reserve delivery of a task's terminal status until delivered or bounded expiry.
6. Stop accepting dispatch before shutdown. Cancel and join owned handlers/cleanup workers using the shutdown deadline; return an error when serving or cleanup fails. If work remains after the deadline, report pending/quarantined via R1 and return nonzero rather than claiming clean stop. Print the listener's actual `Addr()` for `--addr ...:0`.

The existing `CoreHandler` event bridge only recognizes `*DefaultSubagentDispatcher` and publishes to the global bus ([`core_handler.go:187-207`](../../../internal/acp/core_handler.go#L187)); narrow the source seam so each accepted task has a direct event sink. Do not widen the generic event bus contract into a second routing framework.

Remote approval is only enabled when the exact R4 prepared-invocation capability is available. In that case implement the R6 design's one-use approval ID bound to principal/session/task/run and the exact prepared occurrence; expose only a bounded redacted summary, accept only `allow_once|deny`, and never accept remote `always_allow`. If the prepared seam or local approval owner is absent, fail closed before dispatch (including `serve --approve ask`) rather than returning an echo/ack or claiming an approval round trip. See the approved [approval payload and binding](r6-entrypoint-design.md#L60).

### ACP stdio

`StdioAdapter.Disconnect` returns after a 20 ms delay, with an explicitly leaked async writer possible ([`stdio_adapter.go:90-129`](../../../internal/acp/stdio_adapter.go#L90)). `SendMessage` writes directly, while a separate helper serializes only its own path ([`stdio_adapter.go:131-197`](../../../internal/acp/stdio_adapter.go#L131)). The reader uses unbounded `ReadBytes('\n')` and hands off to an inner goroutine which cannot be joined for a caller-owned unclosable reader ([`stdio_adapter.go:200-255`](../../../internal/acp/stdio_adapter.go#L200)).

Keep the current adapter API; add a 1 MiB frame bound, one writer lock for every frame path, strict malformed/oversized-frame termination, and explicit ownership for pipes/processes supplied by an owner. Cancellation must close only an owned closer/process and join the read/write workers. For a generic unclosable reader, return by the bounded deadline while accurately reporting pending/quarantined; do not claim the blocked read is quiescent. The stdio first connect frame must declare `go-cli-acp-json/1`. This slice does not add a CLI stdio server or claim clean process shutdown for arbitrary caller-owned readers.

### MCP Streamable HTTP and reload

The HTTP adapter documents both Streamable HTTP and old SSE, attempts GET first, and treats most GET failures as success before trying direct POST ([`http_adapter.go:159-227`](../../../internal/mcp/http_adapter.go#L159)). The request path sets `Content-Type` and `Accept` but lacks negotiated `MCP-Protocol-Version` and `Mcp-Session-Id`; response parsing accepts JSON or concatenated SSE `data:` as one JSON value instead of a sequence of JSON-RPC SSE messages ([`http_adapter.go:299-384`](../../../internal/mcp/http_adapter.go#L299)). Its client redirect callback checks origin before invoking a caller callback; a callback can mutate the destination after that check ([`http_adapter.go:117-135`](../../../internal/mcp/http_adapter.go#L117)).

Implement only MCP `2025-06-18` Streamable HTTP as frozen in the approved design and the [versioned transport spec](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports): POST `initialize` first; negotiate exactly the supported profile; include JSON + SSE in POST `Accept`; capture optional `Mcp-Session-Id`; send `MCP-Protocol-Version` and the captured session header on subsequent requests; use GET with `Accept: text/event-stream`; correctly parse individual SSE messages and `Last-Event-ID`; return 202 for accepted notifications; send DELETE on orderly session teardown. Reject bad content type/envelope/ID/version and unsupported OAuth with typed errors. Revalidate origin after any custom redirect callback and never forward credentials cross-origin. Keep the configured HTTP timeout/body cap.

For uncertainty after a `tools/call` may have been dispatched, return the existing unknown-outcome error and never retry/replay. The `2025-06-18` cancellation notification is best-effort only; session DELETE is not proof an action stopped. Retain the existing lost-response/no-retry coverage. `MCPToolAdapter.Execute` currently discards the span context, dereferences nil `result`, and drops `MCPToolResult.IsError` when producing `tools.ToolResult` ([`tool_adapter.go:128-158`](../../../internal/mcp/tool_adapter.go#L128)); derive the client call from `spanCtx`, return a typed error for nil/malformed results, and preserve `IsError` in `tools.ToolResult.IsError` (field exists at [`tools.go:22-34`](../../../internal/tools/tools.go#L22)). Verify server/tool names round-trip through `NormalizeToolName`/`ParseToolName`; preserve existing normalized names for safe inputs and reject/encode collisions without aliasing a different tool.

The current hot reloader `Stop` waits for its polling loop, but reconnect cycles are separately launched and not joined ([`hot_reload.go:176-235,320-364`](../../../internal/mcp/hot_reload.go#L176)). A reload cycle reconnects the existing client and calls a one-way `register(tools)` callback; it does not reload/validate frozen config bytes, remove deleted servers, atomically replace old definitions, or propagate registration errors ([`hot_reload.go:382-424`](../../../internal/mcp/hot_reload.go#L382)). First make cancellation, worker tracking, and client teardown bounded. Frozen config/manifest validation belongs to R6-DES-01. Atomic generation replacement/removal belongs behind the R4 registry seam; until it exists, a failed reload must disable affected stale tools rather than leave them executable. Do not invent another registry.

The exact task-owned CLI wiring is `registerMCPTools`/reloader setup in [`assemble.go:832-968,2128-2249`](../../../internal/cli/assemble.go#L832). Today errors are warned and skipped during assembly, and only the auto-discovered-config path starts the reloader. Preserve source ownership by changing only the MCP setup hunks agreed with the R4/R6 owners.

### Web availability

The current package explicitly describes itself as a placeholder echo with no CLI assembly ([`server.go:28-33`](../../../internal/web/server.go#L28)). Authentication is optional and disabled by default ([`server.go:48-53,78-97`](../../../internal/web/server.go#L48)); `Start` accepts any bind address ([`server.go:112-143`](../../../internal/web/server.go#L112)); `/api/chat` returns `Echo: ...` and `/ws` emits a fake connected event ([`server.go:213-285`](../../../internal/web/server.go#L213)).

In this lane, mark the surface unavailable and make placeholder chat/stream endpoints return 503 (health may remain available); `Start` must reject non-loopback binds without configured authentication. Do not add a CLI Web command or Agent wiring. The planned `TestWebAuthenticatedAgentJourney` remains explicitly deferred until R1 run ownership, R2 session authority, and R4 prepared dispatch are wired; no echo test counts as an Agent journey.

## Work order and ownership

1. **Transport RED fixtures.** Add exact private ACP profile, task isolation/admission, stdio frame/write/cancel, MCP 2025-06-18 local peer, and Web-unavailable tests before changing implementation. Keep each failure record with exact command and exit status. Test the narrow package/CLI scope; do not treat a helper-only unit test as real assembly proof.
2. **ACP transport and serve.** Own `internal/acp/http_server.go`, `core_handler.go`, and the exact `serve_cmd.go` lifecycle hunk. Keep the existing dispatcher/R1 owner as execution authority. Gate remote approval on R4 capability and propagate cleanup errors through serve's command result.
3. **stdio library lifecycle.** Own `internal/acp/stdio_adapter.go` and its tests. Do not introduce an inbound process command in this task.
4. **MCP transport and projection.** Own `internal/mcp/http_adapter.go`, `tool_adapter.go`, and specific `assemble.go`/`hot_reload.go` adapter hunks with their current owners. Keep R4 registry generation replacement and R6-DES-01 frozen config ownership intact.
5. **Web unavailable behavior.** Own `internal/web/server.go` and tests only. Full Web integration is a later approved slice.
6. Run affected package race suites and the real local-peer/process journey. Only after all predecessor APIs land, run the R6 gate; a source-level MCP conformance test or ephemeral CLI journey does not prove durable session semantics.

R1 is the run cancellation, status, worker-join, and quiescence authority. R2 is the physical session/commit/recovery authority: until it is wired, these adapters may report ephemeral completion only and must not claim saved/durable success. R4 owns prepared tool identity/effects/approval, and R6-DES-01 owns frozen input bytes/manifests and auth/trust source selection. Failure of a required seam is a visible fail-closed error, not an echo, silently skipped MCP server, or adapter-invented permission.

## Required regression and acceptance evidence

Keep the planned R6 names from [R6-BLD-02 verification](execution-tasks.json#L1436):

- `TestACPEventsIsolatedByTask`, `TestACPDispatchBounded`, `TestACPRemoteApprovalRoundTrip` (only when R4 prepared approval is present), `TestStdioSilentReadCancelled`, `TestStdioConcurrentWritesSerialized`.
- `TestMCP20250618StreamableHTTPHeaders`, `TestMCP20250618SSEFramesAndResume`, `TestMCPToolCallLostResponseIsUnknownAndNotRetried`, `TestMCPIsErrorAndNilPropagate`, `TestMCPToolNameRoundTrips`, `TestMCPShutdownJoinsReconnects`, and `TestMCPReloadReplacesLoadedToolDefinition` (the last awaits R4 atomic registry replacement).
- `TestWebUnavailable`, `TestWebNonLoopbackUnauthenticatedRejected`. Keep `TestWebAuthenticatedAgentJourney` deferred until a real Web run/session/approval route exists.

Run exact scoped race commands on the implementation snapshot:

```sh
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/acp ./internal/cli ./internal/mcp ./internal/web
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/acp -run 'TestACPEventsIsolatedByTask|TestACPDispatchBounded|TestACPRemoteApprovalRoundTrip|TestStdioSilentReadCancelled|TestStdioConcurrentWritesSerialized'
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/mcp -run 'TestMCP20250618StreamableHTTPHeaders|TestMCP20250618SSEFramesAndResume|TestMCPToolCallLostResponseIsUnknownAndNotRetried|TestMCPIsErrorAndNilPropagate|TestMCPToolNameRoundTrips|TestMCPShutdownJoinsReconnects|TestMCPReloadReplacesLoadedToolDefinition'
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/web -run 'TestWebUnavailable|TestWebNonLoopbackUnauthenticatedRejected'
```

The package suite is necessary because transport fixes touch shared request/cancel state; the named subsets give reproducible failures for each acceptance edge. `RemoteApprovalRoundTrip` and `MCPReloadReplacesLoadedToolDefinition` must remain marked blocked/deferred until their R4 capabilities exist, not skipped and reported as passing.

Acceptance also requires these integration fixtures, captured as red/green evidence with child exit code and bounded shutdown:

1. **Actual `serve` process + local HTTP peer:** build `./cmd/cli`, launch `serve` on `127.0.0.1:0` under isolated HOME/XDG/config with a temporary private token file, read the actual bound listener address and token from their proper channels, then use a real HTTP client. Missing/wrong profile returns 4xx and creates no session/task; valid profile connects, sends, observes per-task events, and cancels exact task. Run a second authenticated principal and concurrent tasks and assert no cross-read or cross-event. Start with absent/invalid Agent config and assert nonzero exit before listening—never an echo response. Send SIGTERM while one dispatch is held; assert bounded process exit, joined work when quiescent, nonzero/pending when not.
2. **Actual stdio pipes:** a controlled peer writes boundary-size, over-limit, malformed, and silent frames; concurrently exercise sends and assert every output line parses as one whole message. Cancel, close only owned pipe endpoints, and wait for all owned workers. For an unclosable test reader, assert bounded disconnect and a pending result, never clean quiescence.
3. **Actual MCP `httptest.Server`:** assert exact initialize/initialized requests, all negotiated headers, optional session capture/propagation, POST/GET Accept, one event per SSE frame, replay cursor gap, notification 202, DELETE on close, same-origin redirect, and rejection after a caller redirect callback mutates the URL. Inject a lost `tools/call` response and assert one dispatch/no replay/unknown; inject `isError`, nil/malformed response and assert they cannot become successful tool output.
4. **Web HTTP:** unauthenticated non-loopback Start fails; enabled loopback prototype reports 503 for chat and stream, never `200 Echo`. No Web Agent journey is counted.

### Official wire references

- [MCP 2025-06-18 transport](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)
- [MCP 2025-06-18 lifecycle](https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle)
- [MCP 2025-06-18 cancellation](https://modelcontextprotocol.io/specification/2025-06-18/basic/utilities/cancellation)
- [MCP 2025-06-18 authorization](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization)

