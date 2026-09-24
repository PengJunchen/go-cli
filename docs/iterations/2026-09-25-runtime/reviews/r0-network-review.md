# R0 network review

Review date: 2026-09-25  
Review target: integrated network commits `636eee1`, `7ad171d`, `9ce9ed8`, and `e837769`, at main worktree HEAD `2d3c333`; network lane source commit `10395d4`.  
Review type: independent source review and targeted/race verification. I did not modify product code.

## Review result

No P1 finding. The wildcard bind check rejects empty-host, IPv4 wildcard, and IPv6 wildcard addresses when `--no-auth` is set; positive loopback classification and loopback `--no-auth` startup are covered. The HTTP adapter checks configured and SSE-derived endpoints before applying configured credentials, blocks cross-origin redirects in `CheckRedirect` before the redirected request is sent, and preserves same-origin direct endpoints. Regression tests verify that foreign SSE endpoints and redirect targets receive zero requests, including configured headers, bearer credentials, and OAuth-token cases.

SDK and HTTP `tools/call` bypass automatic replay. The lost-response tests assert a single remote side effect and an error that exposes `OutcomeUnknown`. The shared response parser requires JSON-RPC 2.0, the exact numeric request ID, and exactly one valid result or error; a matching well-formed RPC error remains definite. SDK `ReadString` still cannot interrupt an in-flight read on context cancellation; that lifecycle limitation remains the R6 follow-up and is not considered fixed here.

Two P2 items remain:

1. **The affected CLI race suite is red because MCP test servers return a fixed response ID.** The full `./internal/acp ./internal/cli ./internal/mcp` race run failed 8 `internal/cli` MCP registration/E2E tests. For example, `TestIntegration_MCPAutoDiscovery_ArrayFormat` reports `mcp: response ID does not match request ID 2`: the test fixture at `internal/cli/ime_integration_test.go:198-205` returns `id: 0` for every POST even though the adapter sends initialize as ID 0, initialized as ID 1, and tools/list as ID 2. The neighboring map/config fixtures and `ime_e2e_config_test.go` / `interactive_extra_test.go` use the same fixed-ID pattern. Update those test responders to echo the incoming request ID, then rerun the affected-package race suite. This is a fixture failure rather than evidence of a runtime MCP regression, but it prevents the required suite from passing.

2. **Duplicate JSON object keys can be interpreted as a trusted response.** `parseJSONRPCResponse` unmarshals the envelope into a Go struct (`internal/mcp/request_boundary.go:88-103`), and Go's decoder keeps the last value for duplicate keys. Thus a response with conflicting `id` members can be accepted when its last `id` matches the request, even though the envelope has no unambiguous trusted RPC response ID. Reject duplicate envelope keys (at least `jsonrpc`, `id`, `result`, and `error`) and add a regression showing that conflicting duplicate IDs produce `OutcomeUnknown`.

The code permits same-origin redirects by construction, but I did not find a positive same-origin redirect test; the current direct streamable/SSE endpoint tests do not exercise the redirect branch. Add that positive control when repairing the network regression suite so the contract's same-origin redirect guarantee is tested explicitly.

## Verification

- Exact wildcard, cross-origin, and lost-response regression selection: `rtk env GOMAXPROCS=2 rtk go test -count=1 -p 2 ./internal/acp ./internal/cli ./internal/mcp -run 'TestIsLoopbackAddr|TestServeCmdNonLoopbackNoAuthRejected|TestHTTPAdapterRejectsCrossOrigin(SSEEndpoint|SSEEndpointWithOAuthToken|Redirect)|TestHTTPAdapterCallToolLostResponseHasUnknownOutcome|TestMCPCallToolLostResponseIsUnknownAndNotRetried'` — passed, 24 tests across 3 packages.
- Typed request-boundary selection: `rtk env GOMAXPROCS=2 rtk go test -count=1 -p 2 ./internal/mcp -run 'TestHTTPAdapterCallTool(UntrustedResponsesHaveUnknownOutcome|MatchingRPCErrorIsDefinite|CustomRoundTripperFailureIsUnknownAfterDispatch)|TestMCPCallTool(MalformedMatchingResponseIsUnknown|LostResponseIsUnknownAndNotRetried)|TestMCPReconnect|TestIsConnectionError|TestToolCallOutcomeErrorDoesNotMarkPreDispatchFailuresUnknown|TestTransportRequest'` — passed, 19 tests.
- Affected-package race run: `rtk env GOMAXPROCS=2 rtk go test -race -count=1 -p 2 ./internal/acp ./internal/cli ./internal/mcp` — failed: 776 passed, 8 failed in `internal/cli`; the eight failures are the fixed-ID fixtures described above.
- Isolated ACP and MCP race run: `rtk env GOMAXPROCS=2 rtk go test -race -count=1 -p 2 ./internal/acp ./internal/mcp` — passed, 235 tests across 2 packages.
- Representative fixture failure reproduced without race: `rtk proxy env GOMAXPROCS=2 go test -count=1 ./internal/cli -run '^TestIntegration_MCPAutoDiscovery_ArrayFormat$'` — failed with response ID mismatch (`request ID 2`).

I did not run `make verify` or whole-repository checks. The focused review covers the network lane only.
