# R0 MCP foreign SSE endpoint reachability

Baseline: `8073a17e045c2bb0dd500805613fc711eeddf10c`.

The R0 contract at `r0-contract.md:12,20` requires a foreign absolute SSE endpoint and a foreign-origin redirect to receive no MCP request or credentials. The historical planned-test mapping (`reviews/r0-r1-planned-testname-mapping.md`, baseline `5dcf538`) records that `TestHTTPAdapterRejectsCrossOriginRedirectBeforeCredentials` covers only the redirect branch.

Production wiring assigns URL-only MCP configurations to `MCPTransportStreamableHTTP` and constructs `NewHTTPClientAdapter` (`internal/cli/assemble.go:2248-2266`); Doctor uses the same adapter selection (`internal/cli/doctor.go:620-638`). The foreign absolute SSE endpoint branch is not reachable through the current HTTP adapter. `internal/mcp/http_adapter.go:25-27` declares `HTTPClientAdapter` as the 2025-06-18 Streamable HTTP adapter, and `Connect` rejects every transport except `MCPTransportStreamableHTTP` before network access (`:305-315`). The existing `TestHTTPAdapterRejectsLegacySSETransportBeforeNetwork` verifies that the legacy `MCPTransportSSE` configuration sends no request (`internal/mcp/http_adapter_test.go:569-585`). The configured HTTP endpoint is the endpoint used for initialization and subsequent requests; SSE response bodies are parsed as JSON-RPC frames, and resume GETs reuse that same endpoint (`http_adapter.go:799-823,1041-1065`). `newHTTPMessageRequest` checks the endpoint against the configured origin before applying configured headers or the bearer token (`http_adapter.go:758-786`). There is no legacy `event: endpoint` handler in the current path.

Adding a test that feeds an `event: endpoint` frame to the current adapter would test a flow it does not implement. The applicable cross-origin acceptance for the supported transport is the existing `TestHTTPAdapterRejectsCrossOriginRedirectBeforeCredentials` (`http_adapter_test.go:587-620`): it configures a custom header and bearer token, redirects initialization to a second origin, requires `errCrossOriginRedirect`, and asserts that the second origin receives zero requests. `TestHTTPAdapterAllowsSameOriginRedirectWithCredentials` remains the positive same-origin control.

Conclusion: `TestMCPRejectsCrossOriginEndpointWithCredentials` is inapplicable to the current adapter until legacy SSE endpoint discovery is supported. Keep the existing redirect test as the supported transport's cross-origin boundary; do not manufacture a response-derived endpoint flow.


## Verification

- `rtk go test -list "^TestMCPRejectsCrossOriginEndpointWithCredentials$" ./internal/mcp` returned `Go test: No tests found`; this confirms the planned test is absent and is not counted as RED evidence.
- `rtk proxy go test -list "^TestHTTPAdapterRejectsCrossOriginRedirectBeforeCredentials$" ./internal/mcp` listed the supported redirect regression.
- `rtk env GOMAXPROCS=2 rtk go test -count=1 -p 2 ./internal/mcp -run "^TestHTTPAdapterRejectsCrossOriginRedirectBeforeCredentials$"` passed (1 test).
- `rtk env GOMAXPROCS=2 rtk go test -race -count=1 -p 2 ./internal/mcp` passed (209 tests).
- `rtk go vet ./internal/mcp` passed.
