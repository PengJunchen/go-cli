# R6 MCP Streamable HTTP adapter review

Reviewed frozen source commit `912a53380e663b7bfbab94a5975fd882c034c2d4` in a detached worktree. Scope was `internal/mcp/http_adapter.go`, `internal/mcp/http_adapter_test.go`, and `internal/mcp/initialize_test.go`; no product files were changed. The slice needs fixes before acceptance because the probes below found lifecycle, response-boundary, protocol-truncation, and credential-redaction gaps.

## Findings

### [P1] Disconnect deadline cannot interrupt a request holding the lifecycle read lock

`request` acquires `a.lifecycle.RLock()` and holds it through network request and response-body parsing (`internal/mcp/http_adapter.go:317-320`). `Disconnect` takes `a.lifecycle.Lock()` before consulting its context or cancelling anything (`:236-239`). A blocked response body therefore prevents Disconnect from reaching its context-bound DELETE; the caller's deadline is ignored while waiting for the mutex. `Connect` uses the same non-context-aware exclusive lock (`:194-197`).

Probe `TestReviewProbeDisconnectWaitsForLifecycleRequest` blocked a `tools/list` response body after headers, then invoked `Disconnect` with a 25 ms deadline. Disconnect remained blocked more than 100 ms; it returned only after the response body was released. The HTTP DELETE then observed the expired context. The fix needs a lifecycle cancellation/wait design whose waiting path honors the caller context; merely adding a timeout to DELETE does not address the lock wait.

### [P1] Resumed SSE response can exceed the response-size limit in aggregate; CRLF bytes are undercounted

`readSSEResponse` initializes `totalBytes` on every invocation and counts `len(scanner.Text()) + 1` (`internal/mcp/http_adapter.go:498-531`). The resume path independently calls it on the GET response (`:615-637`), so the initial POST stream and resumed GET each receive a fresh `maxHTTPResponseSize` budget. In addition, `bufio.Scanner` strips CR from CRLF lines; adding only one delimiter byte undercounts each such source line. The counter also assumes an LF for an unterminated final line.

`TestReviewProbeResumedSSEExceedsAggregateResponseLimit` returned a 6 MiB progress event in the POST stream and another 6 MiB progress event plus the matching response in the resumed GET. Both bodies were individually below the 10 MiB cap, but `ListTools` succeeded although the combined response exceeded it. `TestReviewProbeSSECRLFUnderCountsRawSourceBytes` supplied a valid SSE event whose raw source was `maxHTTPResponseSize+3` bytes using CRLF framing; the parser accepted it. Enforce one call-scoped source-byte budget across POST and resume and count actual framing bytes, including CRLF and an EOF tail.

### [P1] EOF dispatches an unterminated SSE event

After `scanner.Scan` reaches EOF, `readSSEResponse` calls `dispatch()` whenever data or an event ID remains (`internal/mcp/http_adapter.go:550-562`). This accepts a matching response without the blank line that terminates an SSE event. The 2025-06-18 transport uses the SSE event stream format; the HTML SSE event-stream algorithm discards pending data at EOF instead of dispatching it. See [WHATWG HTML, event stream interpretation](https://html.spec.whatwg.org/multipage/server-sent-events.html#event-stream-interpretation).

`TestReviewProbeSSEDispatchesUnterminatedEOFEvent` supplied a matching `id` and `data` event with EOF and no final blank line. The adapter returned the response successfully. Treat this as a truncated/missing response (and preserve resumability only when the cursor rules permit it), rather than accepting a partial event.

### [P1] Cross-origin redirect errors can expose URL userinfo

The redirect rejection formats both URLs using `%s` (`internal/mcp/http_adapter.go:149,162`). A URL's string form includes userinfo, so credentials embedded in the configured endpoint can leak into the returned error and any logs that record it. Existing tests cover custom auth headers and bearer headers not being sent to a foreign origin, but do not cover credentials in URL userinfo.

`TestReviewProbeCrossOriginErrorLeaksURLUserinfo` configured `https://probe-user:probe-password-canary@...`, triggered a cross-origin redirect, and found `probe-password-canary` in the resulting error. Redact userinfo (and avoid including credential-bearing full URLs in redirect diagnostics); keep the current pre- and post-custom-callback origin checks.

### [P2] JSON body read errors discard cancellation/error identity

For JSON responses, `readRPCResponse` converts a body read failure to `httpProtocolError("read JSON-RPC response: %v", readErr)` (`internal/mcp/http_adapter.go:370-376`). This loses the original error chain and makes `errors.Is(err, context.Canceled/DeadlineExceeded)` false; an I/O truncation is also mislabeled as protocol syntax failure. SSE read errors preserve their original error instead.

`TestReviewProbeJSONReadCancellationLosesCause` cancelled the request while its JSON response body was blocked. The tool call correctly remained outcome-unknown, but the returned error no longer matched `context.Canceled`. Wrap the read error with `%w` or preserve a typed transport/read error while retaining unknown-outcome classification.

## Positive coverage confirmed

- `Connect` rejects OAuth before making a request (`TestHTTPOAuthConfigRejectedBeforeNetwork`, `TestMCP20250618RejectsOAuthBeforeNetwork`) and rejects the legacy HTTP+SSE transport before network access.
- Initialize and `notifications/initialized` use the Streamable HTTP POST flow; the notification has no JSON-RPC ID and accepts HTTP 202 with an empty body. Subsequent requests carry the negotiated protocol version and optional session ID.
- `TestMCP20250618SSEFramesAndResume` exercises separate SSE frames, progress notifications, cursor propagation through `Last-Event-ID`, and one GET resume without replaying the `tools/call` POST.
- Redirect callbacks are checked again after callback mutation, and custom/bearer credentials are not forwarded to a foreign origin. The userinfo error disclosure above is a separate diagnostic leak.
- Disconnect's DELETE handling treats 405 as permitted and explicitly does not claim that a remote action stopped. The adapter does not pretend to consume an independent server-initiated GET notification stream; the approved handoff leaves that unsupported.

The implementation aligns with the relevant [2025-06-18 Streamable HTTP transport specification](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports): POST initialize/request flow, optional session and protocol headers, 202 notifications, and GET resume using the last event ID. The spec does not turn transport disconnect or DELETE into proof that a dispatched action stopped.

## Verification

The temporary probes were kept out of the frozen source commit and run from the detached `912a53380e663b7bfbab94a5975fd882c034c2d4` worktree. Exact temporary test names:

- `TestReviewProbeDisconnectWaitsForLifecycleRequest`
- `TestReviewProbeResumedSSEExceedsAggregateResponseLimit`
- `TestReviewProbeSSECRLFUnderCountsRawSourceBytes`
- `TestReviewProbeSSEDispatchesUnterminatedEOFEvent`
- `TestReviewProbeJSONReadCancellationLosesCause`
- `TestReviewProbeCrossOriginErrorLeaksURLUserinfo`

All commands exited 0:

```text
rtk env GOMAXPROCS=2 go test -p 2 ./internal/mcp -run '^$' -list '^TestReviewProbe'
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/mcp -run '^TestReviewProbe'
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/mcp
```

The first command listed all six probes. The targeted race run passed in 4.461s and the complete `internal/mcp` race suite passed in 5.937s. These are review probes against the frozen implementation, so a passing test run means the cases reproduced deterministically; it is not a passing acceptance result.

## 294ba3b lifecycle follow-up

Reviewed frozen commit `294ba3b576446ba0f5dbaf4bd97c0f9438ec7a2b` in a clean detached worktree. Relative to the original six findings above, the boundary is now materially better: Disconnect fences new requests and cancels active local request contexts before waiting; caller deadlines return a pending error while the single close owner remains alive; DELETE waits for actual active request completion and is attempted once; failed DELETE is stored and prevents reconnect. The original response-limit, raw CRLF accounting, unterminated EOF, JSON cancellation error-chain, and URL-userinfo probes now pass their corrected assertions.

### [P1] Lifecycle owner goroutines do not contain callback panics

`runConnect` and `runClose` execute untrusted/custom `http.RoundTripper` and response-body methods from adapter-owned goroutines without a top-level panic boundary. A panic in `RoundTrip` escapes `runConnect`; a panic in the DELETE response body's `Close` escapes `runClose`. The former also leaves the generation's initial `active` count unreleased, so simply recovering without a deliberate terminal/quarantine path would still strand close waiters. A panic is not proof of request, local transport, or remote session completion; the affected generation must not be reused or reported as successfully closed.

Temporary subprocess probe `TestReview294PanicEscapesLifecycleOwnerGoroutine` exercised both paths. The parent test required nonzero child exit and the exact panic marker for (a) a custom RoundTripper panic during initialize and (b) a DELETE body `Close` panic. The probe passed, meaning both panics currently terminate the child process rather than becoming a sanitized adapter error. The adapter needs recovery at the outermost owner boundary, one terminal publication, and ownership accounting that reaches Done only after all actual work and defers have exited; a recovered panic must remain fail-closed/unknown.

### [P1] Connect publishes Done before its owner goroutine exits

`runConnect` calls `endHTTPActivityLocked` and closes `op.done`, unlocks, and then invokes `slog.Info("mcp.http.connect", ...)`; its `stopLife` and `op.cancel` defers also execute only after that logger call returns. A global/custom slog handler is a callback and can block. The close owner is released by the already-closed `quiescent` channel and can issue DELETE and publish its own Done while the connect owner is still blocked in the handler. Thus the published operation completion does not yet mean the owner goroutine has exited or released its callback/context-cleanup obligations.

Temporary probe `TestReview294CloseDoneCanPrecedeConnectOwnerExit` installed a handler that blocks on `mcp.http.connect`, waited until the handler was blocked after Connect returned, then called Disconnect. Disconnect and DELETE completed within 300 ms while the handler's release channel was still open. Move every potentially blocking callback/log/defer ahead of terminal publication, or otherwise join the actual owner before the close completion is observable. `runClose` does close its done only after DELETE response-body Close and local cancellation, and normal requests end tracked activity after body read/close; this finding is specifically the post-publication connect logger/defer order.

### 294ba3b verification

The frozen source test list included these direct regressions: `TestHTTPAdapterDisconnectWaitIsCancelableAndCloseIsJoined`, `TestHTTPAdapterCanceledConnectRemainsOwnedUntilActualReturn`, `TestHTTPAdapterFailedDeleteIsStoredAndBlocksReconnect`, `TestMCP20250618ResumeEnforcesAggregateResponseBudget`, `TestReadSSEResponseCountsRawCRLFBytes`, `TestReadSSEResponseDiscardsUnterminatedFinalEvent`, `TestMCPJSONBodyCancellationPreservesCause`, and `TestHTTPAdapterCrossOriginErrorRedactsURLUserinfo`. The two temporary lifecycle probes were listed before execution as well. Commands and exit codes:

```text
rtk env GOMAXPROCS=2 go test -p 2 ./internal/mcp -run '^$' -list '^(TestHTTPAdapter|TestMCP20250618|TestMCPJSONBody|ReadSSEResponse|ReviewProbe)'
  exit 0
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/mcp -run '^(TestHTTPAdapterDisconnectWaitIsCancelableAndCloseIsJoined|TestMCP20250618ResumeEnforcesAggregateResponseBudget|TestReadSSEResponseCountsRawCRLFBytes|TestReadSSEResponseDiscardsUnterminatedFinalEvent|TestMCPJSONBodyCancellationPreservesCause|TestHTTPAdapterCrossOriginErrorRedactsURLUserinfo)$'
  exit 0
rtk env GOMAXPROCS=2 go test -p 2 ./internal/mcp -run '^$' -list '^TestReview294'
  exit 0
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/mcp -run '^TestReview294'
  exit 0 (subprocess probes confirmed the two panic failures)
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/mcp
  exit 0
rtk env GOMAXPROCS=2 go vet -p 2 ./internal/mcp
  exit 0
```

## 4536339 error-redaction and aad7fc2 lifecycle follow-up

Reviewed `4536339f6604c8ac5005463142780ad5c752006c` (parent `75d621ed9fc8646c5000da72acf54f4008e92498`) and `aad7fc218bb84e9906b55d3d444c58e050f09141` (parent `4536339f6604c8ac5005463142780ad5c752006c`) from a clean detached worktree.

The reviewed fixes close the two 294 lifecycle findings. Connect now performs both success log callbacks before publishing `connected`, protocol version, active-count quiescence, and operation Done. Cancellation callback cleanup is synchronized by `linkedCancellation`; Disconnect remains pending until the blocked Connect owner exits, then uses the session ID it learned before cancellation to make one DELETE. Lifecycle worker panics from RoundTripper, initialize response-body Close, connect logging, and DELETE response-body Close are converted to sanitized errors; a known session learned before panic is deleted, while a failed/ambiguous close remains stored and blocks reconnect. Tests also verify no URL/userinfo/query/callback payload appears in HTTP request errors while `errors.Is` still reaches the wrapped callback or RoundTripper cause.

### [P1] A pre-canceled first Connect poisons the adapter generation

`Connect` normalizes a nil context but does not reject an already-canceled caller context before installing a new generation. Its initialize request then fails before reaching the server, but the failed generation and completed `connectOp` remain current. A subsequent valid Connect joins that stored failure instead of starting a healthy generation.

Temporary real-`httptest` probe `TestReviewAAD7CanceledConnectDoesNotPoisonNextConnect` first called Connect with a canceled context and confirmed `errors.Is(err, context.Canceled)` and zero server requests. It then immediately called Connect with `context.Background()` against a valid server and expected successful initialize/notification requests. The frozen source failed the second Connect with `mcp: initialize handshake: mcp: initialize request: mcp: HTTP POST request failed`; request count remained zero. Reject an already-canceled context before admitting a new generation. Preserve existing done-priority behavior when a generation already exists, and do not infer safe retry for failures after dispatch.

### [P2] Normal Connect cleanup can race with the waiter as pending

In `runConnect`, the success path calls `op.cancel()` before closing `op.done`. The owning `waitForConnect` simultaneously selects on `op.done` and `operationCtx.Done`; normal cleanup can therefore wake the waiter through cancellation before the completion channel is closed, and the waiter may observe the still-open `op.done` and report `ErrHTTPOperationPending` for a successful, completing Connect.

The first complete package race run on this snapshot failed `TestHTTPInitializeHandshake` (`internal/mcp/initialize_test.go:159`) with `mcp: connect remains pending while the HTTP operation is still in flight`, despite using `context.Background()`. A second full package race run passed. A 30-count isolated run and a temporary 1,000-adapter immediate-RoundTripper stress probe did not reproduce it, so record this as an intermittent race observed once under the full suite, not a deterministic test failure. Review the normal-completion cancellation ordering so it cannot masquerade as caller/Disconnect cancellation while retaining pending behavior for genuinely active operations.

### 4536339/aad7fc2 verification

The exact targeted names were listed before execution. The race-targeted run passed lifecycle ownership/panic cases, the earlier Disconnect/failed-DELETE regressions, redaction and `errors.Is` cases, JSON cancellation, and SSE budget/framing cases. The full MCP race suite passed on retry; vet and package lint passed. The first full MCP race run had the intermittent failure above. Full-repository `make lint` remains red on the unchanged out-of-scope finding `internal/tools/mutation.go:424:31` (`errcheck`, unchecked error); `golangci-lint run ./internal/mcp` reports no issues. The intentional pre-canceled-Connect probe is review-only and was removed afterward.

```text
rtk env GOMAXPROCS=2 go test -p 2 ./internal/mcp -run '^$' -list '^(TestHTTPAdapterConnectCompletionWaitsForOwnedLogging|TestHTTPAdapterLifecycleOwnerPanicIsContained|TestHTTPAdapterLifecyclePanicChild|TestHTTPAdapterDisconnectWaitIsCancelableAndCloseIsJoined|TestHTTPAdapterCanceledConnectRemainsOwnedUntilActualReturn|TestHTTPAdapterFailedDeleteIsStoredAndBlocksReconnect|TestHTTPAdapterCrossOriginErrorRedactsURLUserinfo|TestHTTPAdapterRedirectCallbackErrorRedactsURLSecrets|TestHTTPAdapterRoundTripErrorRedactsURLSecrets|TestMCPJSONBodyCancellationPreservesCause)$'
  exit 0
rtk env GOMAXPROCS=2 go test -p 2 ./internal/mcp -run '^$' -list '^Test'
  exit 0
rtk env GOMAXPROCS=2 go test -p 2 ./internal/mcp -run '^$' -list '^TestReviewAAD7CanceledConnectDoesNotPoisonNextConnect'
  exit 0
rtk env GOMAXPROCS=2 go test -p 2 -count=1 ./internal/mcp -run '^TestReviewAAD7CanceledConnectDoesNotPoisonNextConnect$'
  exit 1 (healthy retry failed; zero requests reached httptest server)
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/mcp -run '^(TestHTTPAdapterConnectCompletionWaitsForOwnedLogging|TestHTTPAdapterLifecycleOwnerPanicIsContained|TestHTTPAdapterLifecyclePanicChild|TestHTTPAdapterDisconnectWaitIsCancelableAndCloseIsJoined|TestHTTPAdapterCanceledConnectRemainsOwnedUntilActualReturn|TestHTTPAdapterFailedDeleteIsStoredAndBlocksReconnect|TestHTTPAdapterCrossOriginErrorRedactsURLUserinfo|TestHTTPAdapterRedirectCallbackErrorRedactsURLSecrets|TestHTTPAdapterRoundTripErrorRedactsURLSecrets|TestMCPJSONBodyCancellationPreservesCause|TestMCP20250618ResumeEnforcesAggregateResponseBudget|TestReadSSEResponseCountsRawCRLFBytes|TestReadSSEResponseDiscardsUnterminatedFinalEvent|TestReadSSEResponseSupportsCRAndLeadingBOM|TestValidLastEventIDRespectsHTTPFieldValueSafety|TestMCP20250618ResumeAcceptsUTF8EventID)$'
  exit 0
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/mcp
  first run exit 1 at TestHTTPInitializeHandshake:159 with connect pending; second run exit 0
rtk env GOMAXPROCS=2 go vet -p 2 ./internal/mcp
  exit 0
rtk golangci-lint run ./internal/mcp
  exit 0
rtk make lint
  exit 2 (unchanged internal/tools/mutation.go:424:31 errcheck finding)
```

The pre-canceled and normal-completion stress probes were temporary and removed from the frozen worktree. No product files were changed. The earlier 912a/294 findings remain in the report as historical evidence; this review does not accept the HTTP adapter slice until the pre-canceled-generation and normal-completion pending race are addressed and rechecked.

The temporary `review_294_probe_test.go` was review-only and is not part of the frozen commit. This follow-up does not accept the MCP HTTP slice while the owner panic and premature Done findings remain open. It also does not claim acceptance of the whole R6 iteration.

## 75d621e SSE parser delta

Reviewed `75d621ed9fc8646c5000da72acf54f4008e92498`, whose parent is the reviewed 294 commit. This delta replaces Scanner's LF-only line splitting with a bounded reader that recognizes LF, CR, and CRLF, strips one leading UTF-8 BOM, retains raw-byte charging through the shared request budget, discards an unterminated final line, and permits valid UTF-8 event IDs in `Last-Event-ID` while rejecting unsafe HTTP field values. The resume regression exercises an actual POST interruption followed by a GET carrying a non-ASCII cursor. The earlier aggregate-budget, raw-CRLF-budget, and unterminated-EOF regressions remain in place and passed.

I found no new blocker in this parser-only delta. `doHTTP` redirect diagnostics remain origin-only/sentinel-based in this commit; no same-origin diagnostic change is included here. The previously reported lifecycle panic and premature Connect Done findings remain open because this commit does not touch those paths.

The named test list was checked before running. `TestReadSSEResponseSupportsCRAndLeadingBOM`, `TestReadSSEResponseCountsRawCRLFBytes`, `TestReadSSEResponseDiscardsUnterminatedFinalEvent`, `TestValidLastEventIDRespectsHTTPFieldValueSafety`, `TestMCP20250618ResumeAcceptsUTF8EventID`, and `TestMCP20250618ResumeEnforcesAggregateResponseBudget` passed under race; complete `internal/mcp` race suite and vet also passed, all exit 0:

```text
rtk env GOMAXPROCS=2 go test -p 2 ./internal/mcp -run '^$' -list '^(TestReadSSEResponse|TestValidLastEventID|TestMCP20250618Resume|TestHTTPAdapterDisconnectWait|TestMCPJSONBody|TestHTTPAdapterCrossOrigin)'
  exit 0
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/mcp -run '^(TestReadSSEResponseSupportsCRAndLeadingBOM|TestReadSSEResponseCountsRawCRLFBytes|TestReadSSEResponseDiscardsUnterminatedFinalEvent|TestValidLastEventIDRespectsHTTPFieldValueSafety|TestMCP20250618ResumeAcceptsUTF8EventID|TestMCP20250618ResumeEnforcesAggregateResponseBudget)$'
  exit 0
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/mcp
  exit 0
rtk env GOMAXPROCS=2 go vet -p 2 ./internal/mcp
  exit 0
```
