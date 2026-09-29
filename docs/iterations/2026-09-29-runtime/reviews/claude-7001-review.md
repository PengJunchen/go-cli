# Claude native tool-use independent review

## Decision

**ACCEPT for the Claude native protocol slice at `7001fd47b1c52e9081ea4b2b663f15e91b70928f`. No actionable P1/P2 findings.** This does not approve or enable the default CLI provider route; `internal/llm/model_resolver.go` still marks Claude/Anthropic native identities and native provider implementations disabled.

## Reviewed contract and source

Reviewed the fixed commit and its [`claude-builder.md`](../reports/claude-builder.md), then compared the implementation with [`r3-native-claude-tools-design-review.md`](../../2026-09-25-runtime/reviews/r3-native-claude-tools-design-review.md) and [`r3-native-claude-tools-handoff.md`](../../2026-09-25-runtime/r3-native-claude-tools-handoff.md).

- Request construction sends Claude `tools` as `{name, description?, input_schema}`; non-nil schemas and call arguments are marshaled as JSON objects before HTTP. The nil schema default remains an empty object. Tool calls and adjacent results are grouped by exact unique call ID, with results serialized in original call order; malformed/incomplete history fails before dispatch.
- Generate requires the top-level assistant role and terminal stop reason, preserves ordered text/tool-use blocks and object arguments, and rejects unsupported blocks, duplicate IDs, invalid input roots, text after tool-use, and stop-reason/tool-call mismatches. Integer arguments decode as `json.Number` and survive the follow-up request.
- Stream checks event/data type agreement, contiguous block indexes, block lifecycle, tool ID uniqueness, canonical text-before-tool order, bounded tool accumulation, terminal consistency, and post-`message_stop` data. It publishes tool calls only in the successful Final chunk.
- The direct Generate follow-up test checks a real HTTP request's assistant `tool_use` and user `tool_result` history. The core loopback uses the real Claude stream adapter and tool registry, asserts two requests and one execution per tool, and validates result ID/order on the second request. A malformed stream fails before dispatch.
- Malformed response and stream paths use generic errors; canaries are asserted absent from returned errors/logs. Existing bounded provider-body tests remain in the full `llm` race run.

The two design-review clarifications are present: missing/non-assistant response role is rejected before tool calls are returned, and non-nil tool schemas with scalar, array, null, or unmarshalable roots fail before HTTP. The result content field's `omitempty` is not a finding: Anthropic's [tool-call guide](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls) marks `tool_result.content` optional.

## Independent verification

- `rtk run go test ./internal/llm -list Claude` and `rtk run go test ./internal/core -list Claude` listed the relevant fixed-commit Claude tests.
- `rtk env GOMAXPROCS=2 go test -p 2 -race ./internal/llm ./internal/core -count=1` passed both packages.
- `rtk go vet ./internal/llm ./internal/core` passed.
- Temporary review-only probes passed: a custom transport returning one byte per `Read` preserved multibyte text, tool IDs, and tool arguments across SSE framing; missing block index, duplicate tool ID, and post-`message_stop` data each failed without Final; a real core loop with a tool-use/`end_turn` mismatch returned an error, executed zero tools, and exposed neither the call ID nor tool input in logs/errors. Those probe files were removed after the runs.

This review approves only the local Claude protocol implementation and loopback evidence. It does not claim default CLI routing, PTY behavior, remote Anthropic validation, or the separate Gemini protocol is complete.
