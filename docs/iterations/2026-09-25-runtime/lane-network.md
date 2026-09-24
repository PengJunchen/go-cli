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

## Commits

- `bc50965c87b08e3ca6ad323327ec92674e536888` — reject unauthenticated wildcard binds.
- `cf877fc6b77c22efc4ec3f0a901e5bbc1d5e7f89` — report ambiguous tool-call outcomes and keep replay disabled for side effects.
- `48db2c1526119eab44183da020524f7d747911f8` — constrain MCP HTTP endpoints and redirects to the configured origin.

## Limits

Validation covered the exact regression list and race tests for the three affected packages. Full `make verify` and whole-repository checks were not run. No CLI MCP authentication fields or assembly changes were added, and ACP client/session-flow work remains outside this lane.
