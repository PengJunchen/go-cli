# R0-BLD02 network lane

Branch: `codex/runtime-r0-network`  
Base: `00149001e2350b18dd69d7fa41ab7fcd6ed0241a`

## Changes

- `serve --no-auth` rejects empty-host, IPv4 wildcard, and IPv6 wildcard binds. `IsLoopbackAddr` now recognizes loopback IPs and localhost case-insensitively; a missing host is treated as all interfaces.
- The HTTP MCP adapter validates the configured URL and every SSE POST endpoint against the configured HTTP origin before attaching configured headers, bearer tokens, or an OAuth access token. Cross-origin redirects stop before the redirected request is sent. Same-origin requests and redirects remain available.
- SDK and HTTP `tools/call` transport failures now state that the outcome is unknown and the tool may have executed. The SDK no longer reconnects and replays a dispatched `tools/call`; `tools/list` keeps its existing reconnect behavior.

## Evidence

The red command ran against the unmodified base implementation, with the new behavioral tests in place:

```text
rtk env GOMAXPROCS=2 rtk go test -count=1 -p 2 ./internal/acp ./internal/cli ./internal/mcp -run "TestIsLoopbackAddr|TestServeCmdNonLoopbackNoAuthRejected|TestHTTPAdapterRejectsCrossOrigin(SSEEndpoint|SSEEndpointWithOAuthToken|Redirect)|TestHTTPAdapterCallToolLostResponseHasUnknownOutcome|TestMCPCallToolLostResponseIsUnknownAndNotRetried"
exit 1: 13 passed, 11 failed
```

Failures exercised wildcard classification and serve exposure, foreign-origin SSE and redirect requests, and lost-response tool-call outcomes. The RTK test summary is in [network-red.log](evidence/network-red.log).

The same exact test list passed after implementation:

```text
exit 0: 24 passed across internal/acp, internal/cli, and internal/mcp
```

The affected package race suite also passed:

```text
rtk env GOMAXPROCS=2 rtk go test -race -count=1 -p 2 ./internal/acp ./internal/cli ./internal/mcp
exit 0: 752 passed across 3 packages
```

Both green outputs are in [network-green.log](evidence/network-green.log). `git diff --check` passed.

## Typed dispatch-boundary follow-up

The follow-up replaces error-text matching with a typed request-boundary result. SDK write/read transport failures remain retryable for safe read-only calls, so `tools/list` reconnect behavior is preserved. Once a request may have been dispatched, a `tools/call` error is reported as unknown and is never replayed. Pre-dispatch validation failures and a valid JSON-RPC error response remain definite errors.

An HTTP response is trusted only when the status is successful and the JSON-RPC 2.0 envelope contains this request's numeric ID plus exactly one valid result object or well-formed error object. A missing/wrong ID, malformed envelope/body, non-success HTTP status, or transport error after entering `Do` is treated as an unknown tool outcome.

The new red tests were run before the implementation and failed on lost/custom-round-tripper responses, malformed JSON, a wrong response ID, an HTTP 503 response, and a matching SDK response with the wrong JSON-RPC version:

```text
rtk env GOMAXPROCS=2 go test -count=1 -p 2 ./internal/mcp -run "TestHTTPAdapterCallToolUntrustedResponsesHaveUnknownOutcome|TestHTTPAdapterCallToolCustomRoundTripperFailureIsUnknownAfterDispatch|TestMCPCallToolMalformedMatchingResponseIsUnknown"
exit 1
```

The complete focused green list, including known RPC errors, pre-dispatch classification, SDK lost responses, reconnect behavior, and transport deadline tests, passed:

```text
rtk env GOMAXPROCS=2 go test -count=1 -p 2 ./internal/mcp -run "TestHTTPAdapterCallTool(UntrustedResponsesHaveUnknownOutcome|MatchingRPCErrorIsDefinite|CustomRoundTripperFailureIsUnknownAfterDispatch)|TestMCPCallTool(MalformedMatchingResponseIsUnknown|LostResponseIsUnknownAndNotRetried)|TestMCPReconnect|TestIsConnectionError|TestToolCallOutcomeErrorDoesNotMarkPreDispatchFailuresUnknown|TestTransportRequest"
```

The focused output is in [network-boundary-targeted-green.log](evidence/network-boundary-targeted-green.log). The full MCP race suite passed with `rtk env GOMAXPROCS=2 go test -race -count=1 -p 2 ./internal/mcp`; output is in [network-boundary-green.log](evidence/network-boundary-green.log). The HTTP fixture's dropped-response/custom-round-tripper tests assert the side-effect count remains exactly one.

Unknown tool outcomes expose `OutcomeUnknown() bool` through a stable error interface, and the wrapper preserves the underlying error chain with `Unwrap` for the R1 core to consume. SDK `ReadString` remains a blocking read: context cancellation is checked between lines and cannot interrupt an in-flight read. That existing lifecycle limit remains an R6 follow-up and is not claimed as closed here.

## Independent review follow-up

The review found eight CLI MCP tests whose mock servers always returned ID 0. The shared `writeMockMCPResult` fixture helper now reads and echoes the request's JSON-RPC ID, so initialize, initialized, and tools/list responses follow the live request. The exact eight-test selection first failed with request-ID mismatch and then passed; red and green logs are [cli-mcp-fixtures-red.log](evidence/cli-mcp-fixtures-red.log) and [cli-mcp-fixtures-green.log](evidence/cli-mcp-fixtures-green.log).

`parseJSONRPCResponse` rejects duplicate or non-canonical spellings of reserved top-level protocol fields before decoding the envelope, and rejects duplicate `code`/`message` members in RPC error objects. Unknown extension fields remain accepted. Side-effect fixtures cover conflicting duplicate IDs whose last value matches the request, case aliases for `ID`, `JSONRPC`, `Result`, and `Error`, duplicate `result`/`error`, and duplicate error `code`/`message`; each ambiguity is an `OutcomeUnknown` failure and the server-side effect counter remains exactly one. A same-origin redirect positive control also verifies that redirected GET, initialize, and initialized requests arrive with the configured header and bearer credentials. The initial red and targeted race green output is in [network-review-red.log](evidence/network-review-red.log) and [network-review-targeted-green.log](evidence/network-review-targeted-green.log).

After the fixes, the full CLI race suite passed (`GOMAXPROCS=2`, `-p 2`); its output is [cli-mcp-fixtures-race.log](evidence/cli-mcp-fixtures-race.log). The full MCP race suite passed with the same limits; its output is [network-review-race.log](evidence/network-review-race.log). ACP and repository-wide verification were not repeated because these review follow-ups touch only CLI test fixtures and MCP response parsing/tests.

The final case-alias follow-up has its own red/green evidence: [network-casefold-red.log](evidence/network-casefold-red.log), [network-casefold-test-list.log](evidence/network-casefold-test-list.log), [network-casefold-green.log](evidence/network-casefold-green.log), [network-casefold-extension-list.log](evidence/network-casefold-extension-list.log), and [network-casefold-race.log](evidence/network-casefold-race.log). The new exact red test exposed Go's case-insensitive struct-field matching for `ID` and `JSONRPC`, plus last-key-wins error `code`/`message`; all the cases now fail closed as unknown after dispatch.

## Commits

- `bc50965c87b08e3ca6ad323327ec92674e536888` — reject unauthenticated wildcard binds.
- `cf877fc6b77c22efc4ec3f0a901e5bbc1d5e7f89` — report ambiguous tool-call outcomes and keep replay disabled for side effects.
- `48db2c1526119eab44183da020524f7d747911f8` — constrain MCP HTTP endpoints and redirects to the configured origin.

## Limits

Validation covered the exact regression list and race tests for the three affected packages. Full `make verify` and whole-repository checks were not run. No CLI MCP authentication fields or assembly changes were added, and ACP client/session-flow work remains outside this lane.
