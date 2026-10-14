# Claude native tool-use builder report

## Design clarifications applied before implementation

The implementation follows the reviewed R3 handoff and incorporates both required design clarifications:

- A non-streaming response must explicitly carry top-level `role: "assistant"`. Missing or any other role fails closed before a `tool_use` can be returned to the dispatcher. Decoder errors must not contain the provider response body or tool input.
- Every non-nil `ToolDefinition.Parameters` value must marshal to a JSON object before an HTTP request is built. Scalar, array, and `null` roots are rejected; a nil schema keeps the existing empty-object default, and valid object schemas are preserved without rewriting.

These are protocol checks at the Claude codec boundary. The existing native-provider registration gate remains unchanged, and Gemini is outside this slice.

## Implementation and evidence

Anthropic's Messages API contract was checked against its primary docs: user-defined tools use `name`, optional `description`, and `input_schema`; tool calls are assistant `tool_use` blocks ending in `stop_reason: "tool_use"`; results are user `tool_result` blocks correlated by `tool_use_id`. The docs specify the tool-name pattern as `^[a-zA-Z0-9_-]{1,128}$`, and require result blocks immediately after the call and before user text ([Define tools](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools), [Handle tool calls](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls), [Messages API](https://platform.claude.com/docs/en/api/messages/create), [Streaming messages](https://platform.claude.com/docs/en/build-with-claude/streaming)).

Before implementation, the provider HTTP tests were run against the baseline and failed for the expected missing tool schema/history mapping, dropped `tool_use`, role/schema validation, and Generate/Stream parity. `rtk go test ./internal/llm -run '^(TestClaudeToolUseGenerateAndStreamParity|TestClaudeGenerateClosesToolResultsByCallIDOnSecondRequest|TestClaudeGenerateRejectsInvalidResponseBeforeReturningToolCalls|TestClaudeRejectsInvalidToolSchemasBeforeHTTP|TestClaudeRejectsUnclosedToolHistoryBeforeHTTP)$' -count=1` reported 1 passed and 36 failed against the old provider. The initial real core loop test also showed that a stream which drops the tool call cannot close the loop; that test now explicitly caps its LoopAgent at five iterations and the successful fixture requires exactly two requests. The baseline's default iteration count was not encoded as expected behavior.

`internal/llm/native.go` now sends Claude-native tool definitions with `input_schema`, preserving the existing `{"type":"object","properties":{}}` default for nil schemas and rejecting non-object roots before HTTP. It validates supported input blocks, roles, tool names, and unique tool definitions. Assistant calls serialize as `tool_use`; the immediately following `RoleTool` group must close every call by exact ID, with optional names checked when supplied, and is sent as one user message with results ordered by original calls. Nil call arguments become `{}`. There is no wire `role: "tool"` mapping.

Generate now requires a top-level assistant role and a terminal stop reason. It preserves text-before-tools order and tool IDs/names/object arguments, uses `json.Number` to retain integer precision, rejects unsupported blocks and malformed/partial/non-object inputs, and requires tool-call presence to agree with `stop_reason`. Stream applies the same terminal, ordering, argument, and finish-reason rules; it accumulates tool calls only for the successful Final chunk and rejects unsupported events/deltas/blocks. Request-body updates for Claude's thinking and stream flags use raw JSON messages so large schema numbers survive re-encoding. Error paths for malformed/unsupported responses remain generic and tests use canaries to check that raw content, IDs, schemas, and arguments do not appear in errors or logs. Existing response-body reader and stream close behavior were left in place.

Added real `httptest` coverage for outbound schema JSON, schema root failures before dispatch, invalid tool history before dispatch, two-request Generate closure, Generate/Stream parity, a real Stream→`LoopAgent`→tool side-effect→second-request round trip, and malformed stream terminals with zero tool dispatch and no successful Final. Added decoder and stream negatives for missing/wrong roles, unsupported/thinking/server blocks, interleaved content, ID errors, input errors, stop-reason mismatches, and incomplete streams. Existing Claude fixtures were made protocol-complete. Gemini, provider registration, the native CLI gate, and `internal/core` production code were not changed.

Validation from this worktree:

- `rtk go test ./internal/llm ./internal/core -run Claude -count=1` — 93 test/subtest cases passed across 2 packages.
- `rtk go test ./internal/llm ./internal/core -count=1` — 1,507 test/subtest cases passed across 2 packages.
- `rtk env GOMAXPROCS=2 go test -p 2 -race ./internal/llm ./internal/core -count=1` — both packages passed (`llm` 2.937s, `core` 4.837s).
- `rtk go vet ./internal/llm ./internal/core` — passed.
- `rtk golangci-lint run ./internal/llm/... ./internal/core/...` — passed with no issues on the final run. An earlier run found an ineffectual assignment; that was removed and lint rerun clean.

This verifies the Claude native codec and the local core loopback only. It does not enable or validate the default CLI provider route; that existing gate remains a separate review decision.
