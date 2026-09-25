# R6 CLI MCP wiring and fixture review

**Fixed snapshot:** `e517ad87de49283d06e7cb6d8b0deda9f96ec43f` (`fix(cli): use Streamable HTTP for URL MCP servers`), parent `a9e6b7f706091ce42689032667810977e5ed41f6`. The worktree was clean before review. This review covers the eight-file CLI/fixture slice only; it does not certify the complete R6 adapter lifecycle or R4 trust/registry closure.

**Verdict on `e517ad8`: reject pending one P2 test assertion fix (superseded by the accepted delta below).** The production URL-to-Streamable-HTTP mapping was correct, and the protocol/security fixtures covered the intended narrow profile. However, `TestE2E_Phase47_FullPipeline_Integration` could pass when its newly valid MCP tool call failed, because it only checked the result inside `if err == nil && res != nil`. The follow-up adds required success, result, ID, and output assertions.

## Source review

`registerMCPTools` selects `MCPTransportStreamableHTTP` and `NewHTTPClientAdapter` for URL-backed servers, while command-backed entries still use `MCPTransportStdio` and the official SDK adapter. Doctor's `toMCPConfig` and `newMCPClientForDoctor` use the same URL/command split. The positive CLI test calls the real `registerMCPTools`, gets the normalized tool, executes it, and requires a successful `ok` result. The changes do not route configured stdio servers into the HTTP adapter.

The revised HTTP fixtures validate the 2025-06-18 handshake shape: JSON-RPC 2.0, valid request IDs, initialization protocol version, no version header on initialize, negotiated `MCP-Protocol-Version` on later requests, `notifications/initialized` without an ID and HTTP 202, and the POST `Accept` values for JSON and event streams. The existing MCP HTTP adapter retains the 10 MiB response limit; the updated security fixture still checks an oversized initialize response. OAuth configuration is now explicitly rejected with `ErrUnsupportedOAuth` before any HTTP request; the replacement test makes no claim that OAuth CSRF protection is implemented.

The direct adapter tests in `tests/e2e_20260802/12_mcp_skill_http_e2e_test.go` include a genuine registry `tools/call` journey with required success, returned text, call ID, tool name, arguments, and two-call count assertions. Thus the suite does retain positive HTTP execution coverage; it is not composed only of unsupported-transport tests.

At `e517ad8`, one test-quality caveat was not itself a production blocker: `TestInteractiveCmd_RegisterMCPTools_SSEServer` directly constructed an explicit legacy SSE adapter, verified rejection before network access, then created a fresh empty registry and asserted it was empty. That assertion did not test registration behavior, and the test name suggested it exercised `registerMCPTools` when it did not. The delta below renames this as a direct adapter test and removes the tautological assertion. Do not cite it as evidence that CLI registration rejects legacy SSE.

## Matched tests and validation

The exact lists were checked before execution:

```text
rtk proxy go test -list 'Test(InteractiveCmd_RegisterMCPTools_(StdioServer|StreamableHTTPServer|SSEServer)|ToMCPConfigSelectsStreamableHTTPForURL|NewMCPClientForDoctorSelectsStreamableHTTPAdapter)' ./internal/cli
  exit 0; all five matching CLI tests were listed.

rtk proxy go test -list 'Test(E2E_MCPHTTPAdapter_(LegacySSERejectedBeforeNetwork|StreamableHTTPConnectAndListTools|CallToolThroughRegistry)|E2E_SkillAndMCPCombinedPipeline|E2E_Phase47_(MCPInitializeHandshake_EndToEnd|ParallelMCPConnect|FullPipeline_Integration))' ./tests/e2e_20260802
  exit 0; all seven matching MCP E2E tests were listed.

rtk proxy go test -list 'Test(ET_Phase48_MCP_(OAuthUnsupportedBeforeNetwork|ResponseSizeLimit))' ./tests
  exit 0; both security tests were listed.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/cli -run '^(TestToMCPConfigSelectsStreamableHTTPForURL|TestNewMCPClientForDoctorSelectsStreamableHTTPAdapter|TestInteractiveCmd_RegisterMCPTools_(StdioServer|StreamableHTTPServer|SSEServer))$' -count=1
  exit 0.

rtk env GOMAXPROCS=2 go test -race -p 2 ./tests/e2e_20260802 -run '^(TestE2E_MCPHTTPAdapter_(LegacySSERejectedBeforeNetwork|StreamableHTTPConnectAndListTools|CallToolThroughRegistry)|TestE2E_SkillAndMCPCombinedPipeline|TestE2E_Phase47_(MCPInitializeHandshake_EndToEnd|ParallelMCPConnect|FullPipeline_Integration))$' -count=1
  exit 0; note that the weak conditional assertion in FullPipeline remains a test gap despite the passing exit status.

rtk env GOMAXPROCS=2 go test -race -p 2 ./tests -run '^(TestET_Phase48_MCP_OAuthUnsupportedBeforeNetwork|TestET_Phase48_MCP_ResponseSizeLimit)$' -count=1
  exit 0.
```

No product source was changed and this report was not staged. Acceptance is limited to the transport mapping and verified fixtures after the P2 assertion is fixed; it does not claim full R6 completion.

## Independent delta review: `ceb2407719bbc7ed0028022ff4a7b1f7af023f5a`

**Verdict: accept the test-only follow-up for this slice.** The fixed delta has parent `e517ad87de49283d06e7cb6d8b0deda9f96ec43f` and changes only `internal/cli/interactive_extra_test.go` and `tests/e2e_20260802/22_phase47_production_hardening_e2e_test.go`. The phase47 full-pipeline tools call now requires no error and a nonnil result, checks the returned tool-call ID, and checks the mock's expected `ok` content. The legacy SSE test is now named `TestHTTPMCPAdapterRejectsLegacySSEBeforeNetwork`; it retains the meaningful typed rejection and zero-network assertion, and no longer makes the unrelated fresh-empty-registry assertion. The positive CLI URL registration/execution test and the separate real registry execution coverage remain intact.

Exact test names were listed before the race run:

```text
rtk proxy go test -list 'Test(HTTPMCPAdapterRejectsLegacySSEBeforeNetwork|InteractiveCmd_RegisterMCPTools_StreamableHTTPServer|ToMCPConfigSelectsStreamableHTTPForURL|NewMCPClientForDoctorSelectsStreamableHTTPAdapter)' ./internal/cli
  exit 0; all four tests were present.

rtk proxy go test -list '^TestE2E_Phase47_FullPipeline_Integration$' ./tests/e2e_20260802
  exit 0; the test was present.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/cli -run '^(TestToMCPConfigSelectsStreamableHTTPForURL|TestNewMCPClientForDoctorSelectsStreamableHTTPAdapter|TestInteractiveCmd_RegisterMCPTools_StreamableHTTPServer|TestHTTPMCPAdapterRejectsLegacySSEBeforeNetwork)$' -count=1
  exit 0.

rtk env GOMAXPROCS=2 go test -race -p 2 ./tests/e2e_20260802 -run '^TestE2E_Phase47_FullPipeline_Integration$' -count=1
  exit 0.

rtk git diff --check e517ad87de49283d06e7cb6d8b0deda9f96ec43f..ceb2407719bbc7ed0028022ff4a7b1f7af023f5a
  exit 0.
```

This accepts only the fixed test assertions and legacy fixture clarification at the R6 CLI MCP wiring checkpoint; it does not certify broader R6 completion. This report remains unstaged.
