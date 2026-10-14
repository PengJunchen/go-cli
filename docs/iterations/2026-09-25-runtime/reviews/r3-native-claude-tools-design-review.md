# R3 Native Claude tool-use design review

Reviewed fixed handoff `d04c3d6a7fcf524712bc81b01d8defc857255d45` at `docs/iterations/2026-09-25-runtime/r3-native-claude-tools-handoff.md`. This was a read-only design review against the provider source in that checkout. No product code, design, or ledger was changed, and no tests were run.

## Decision

**Needs two narrow contract clarifications before design approval.** The core direction is implementable and preserves the existing native-provider CLI gate. The handoff correctly treats text followed by client `tool_use` blocks as the supported `llm.Message` projection, rejects unsupported/interleaved blocks instead of reordering them, and keeps the independent Gemini gap out of this slice.

Anthropic’s current documentation describes client calls as assistant `tool_use` blocks followed immediately by one user message with the corresponding `tool_result` blocks; each result is correlated by `tool_use_id`, and result blocks precede any user text. The proposed single result group ordered by the original calls satisfies that contract. The ordinary text-then-tool-use response shown in Anthropic’s example remains supported; failing closed only when the current message type cannot represent a different block order is an appropriate compatibility boundary. [Handle tool calls](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls), [Parallel tool use](https://platform.claude.com/docs/en/agents-and-tools/tool-use/parallel-tool-use)

The source confirms `llm.Message` holds text and `ToolCalls` in separate fields, while `ContentBlocks` currently represents text/images; it cannot losslessly persist arbitrary text/tool interleaving. The existing `decodeClaudeResponse` also currently drops tool blocks, and `encodeClaudeRequest` omits `GenerationOptions.Tools` and maps `RoleTool` to a wire role. The proposed native codec plus real HTTP/Loop regression is therefore the right implementation seam. Anthropic’s API defines `input_schema` as the schema for an object-valued `tool_use.input`, and the streaming API sends partial JSON deltas whose completed input is an object. [Messages API](https://platform.claude.com/docs/en/api/messages/create), [Streaming messages](https://platform.claude.com/docs/en/build-with-claude/streaming)

The handoff correctly leaves default CLI enablement out of this change: `isDisabledCLIProvider` still rejects the native Claude/Anthropic identities before resolver credential lookup/build. Native adapter protocol tests must not be described as default CLI route acceptance; that remains a separate registration gate.

## Required clarifications

### P1 — Validate the response role before promoting tool calls

The handoff calls for a complete assistant terminal but does not explicitly require the decoder to reject a missing or non-`assistant` top-level `role`. The current `claudeResponse` shape omits `role`, and `decodeClaudeResponse` constructs `RoleAssistant` unconditionally (`internal/llm/native.go`, `decodeClaudeResponse`). Once `tool_use` is decoded into `Message.ToolCalls`, a malformed response with another role would be reinterpreted as an assistant request and could reach the tool dispatcher.

Add `role` to the decoded response contract and fail closed unless it is exactly `assistant`, before returning any tool calls. Include missing-role and non-assistant-role cases in decoder negatives; ensure the error omits response body/content. This is a small protocol validation check, not a wider adapter abstraction.

### P2 — Require an object-root tool schema before HTTP dispatch

`ToolDefinition.Parameters` is `any` (`internal/llm/llm.go`). The handoff says to preserve the schema and fail before the request for invalid/unmarshalable schemas, but its request tests only explicitly call out the unmarshalable case. Anthropic’s client tool `input_schema` describes the object emitted as `tool_use.input`; R4’s prepared tool contract also requires object-root schemas. A scalar, array, or JSON `null` can currently be represented through the public option type unless the handoff requires validation.

Clarify that a non-nil schema must marshal to a JSON object before the HTTP request is constructed; keep the existing nil-schema default as the empty object, and preserve valid schema fields without rewriting them. Add scalar, array, and null-root negatives that assert zero server requests. This asks only for root-shape validation, not a new JSON Schema validator.

## Deferred scope

The proposed text/image/client-tool subset, body and stream limits, privacy canaries, final-only tool-call publication, and unsupported thinking/server/unknown block behavior are appropriate for this slice. `tool_result.is_error` remains an acknowledged limitation because `llm.Message` has no typed execution-error field; the handoff correctly forbids inferring it from result text. Gemini protocol support, Claude CLI route enablement, and the R1/R4 execution guarantees remain separate gates. No implementation or runtime acceptance is claimed here.
