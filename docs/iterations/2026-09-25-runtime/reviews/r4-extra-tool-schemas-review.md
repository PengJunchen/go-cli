# R4 shipped-extra tool schemas implementation review

**Target:** `732ba672d289e85d7ea7c4348f828314c2937a04`
**Implementation parent:** `468aa6cc3cc598bd1253219cff723e5693cd106f`
**Design authority:** `3539d09` and the accepted [R4 extra-tool schema design review](r4-extra-tool-schemas-design-review.md)
**Verdict:** Accepted for the bounded schema slice. The native Anthropic tool request/history/non-stream response gap remains a separate provider-contract P1; this result does not close R4 provider readiness.

## Review result

The target adds `Parameters()` to the 15 fixed shipped tools missing schemas at the source census and leaves the existing `git_pr_create` schema unchanged. The tool schemas are closed root objects (`additionalProperties: false`), with fields and required keys matching the existing executors. The implementation does not add runtime defaults, unsupported schema vocabulary, task-status/priority enums, string-length rules, or URL-format claims.

The intentional model-boundary differences match the accepted design. `goal_update.status` lists only the six canonical lower-case values while its raw executor retains trim/case aliases; tests exercise both directions. `tool_search.category` includes the explicit empty-string no-filter sentinel plus the four source categories. `ask_user.options` is `array<string>`. `todo_write` keeps action-dependent field checks in its executor because the prepared validator has no conditional-schema support. Blank required strings likewise pass presence/type validation and fail in the existing executor after policy; unknown keys and wrong field types are rejected by the prepared schema boundary before policy.

The tests exercise the prepared protected registry and verify invalid calls do not advance the policy witness or reach side-effect witnesses. The assembly test obtains the fixed tools from the real assembled registry, confirms `Parameterized` survives, verifies `tool_search` retains `TopToolsProvider`, captures Loop model definitions, and sends those definitions through the Eino OpenAI-compatible HTTP request encoder. The mixed-provider test exercises OpenAI-compatible `function.arguments` JSON-string decoding and Anthropic native streamed `tool_use` `partial_json` decoding, then sends valid and invalid `ask_user.options` arrays through the same prepared proxy. It separately checks OpenAI request-schema serialization. These tests do not invoke a real external provider.

## Residual provider-contract P1

The Claude inbound streaming decoder coverage must not be read as Anthropic tool support being complete. In this target, `claudeRequest` (`internal/llm/native.go:1328`) has no tools field, `encodeClaudeRequest` (`:1404`) does not encode `GenerationOptions.Tools` or tool-call history, and `decodeClaudeResponse` (`:1453-1460`) only accumulates text blocks rather than native non-stream `tool_use`. Anthropic outbound schemas, assistant tool-call / `tool_result` history, and non-streaming tool-use response handling remain open and require a separate provider-contract slice and review.

## Validation evidence

The four added test functions were enumerated from the committed files:

- `TestFixedExtraToolsExposeAccurateParameterSchemas`
- `TestFixedExtraPreparedSchemasReachExistingExecutors`
- `TestExtraToolSchemaMixedProviderDecodersReachPreparedExecutor`
- `TestAssembleAgentExtraToolSchemasReachLoopModel`

On the target worktree, `rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tools ./internal/llm ./internal/cli` passed all three packages, and `rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go vet ./internal/tools ./internal/llm ./internal/cli` passed.

For a behavior-RED witness, I created a temporary detached worktree at the implementation parent (`468aa6c`), copied only the three new test files into it, and ran the four named tests. All packages compiled; the tests then failed at runtime because `todo_write` and `ask_user` did not expose `Parameterized` through the raw/protected definitions. This is a behavioral failure, not a compile failure. The temporary worktree was used only for this comparison; the fixed target worktree remained unchanged.

One small non-blocking coverage omission remains: the generic invalid-type loop skips array properties, so it does not directly assert that a scalar value such as `options: "yes"` is rejected as a non-array. The provider test does reject a non-string array member, and the production schema declares `array` with string items; the prepared validator also checks the array container type. Adding one scalar-container case would make the test matrix more complete, but review found no corresponding implementation defect.

This is a scoped implementation review, not a full R4 gate. The target contains only the schema source and test changes plus the updated handoff; no protected files or runtime executors were modified.
