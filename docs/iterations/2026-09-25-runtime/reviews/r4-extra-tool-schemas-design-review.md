# R4 remaining tool schemas design review

**Target:** `b0036553f4f1d4645fd20fe1c7fd505861c9cde4`  
**Source snapshot:** `575fc5f746f702256a9b5d46150b955951a71066`  
**Verdict:** **Accepted with one explicit schema refinement.** The inventory and implementation plan match the source. The `tool_search.category` enum must include the empty-string no-filter sentinel in addition to its four canonical categories; the root approved this refinement during review.

## Source and registration review

The fixed extra-tool census is accurate. `assembleExtraTools` registers six ordinary tools (`todo_write`, `task_create`, `task_update`, `goal_create`, `goal_update`, `git_pr_create`), eight trusted controls (`task_get`, `task_list`, `goal_list`, `goal_get`, `ask_user`, `enter_plan_mode`, `exit_plan_mode`, `tool_search`), and two trusted network reads (`web_fetch`, `web_search`). `git_pr_create` is already `Parameterized`; the other 15 fixed extras do not yet expose schemas. Optional custom, extension, MCP, LSP, remote Bash, and skill registrations do not replace schemas for these concrete shipped definitions.

The handoff's execution path is also accurate. `DefaultToolRegistry` snapshots a registered `Parameterized` schema. `MiddlewareToolRegistry` exposes protected definitions which retain `Parameterized`; the `tool_search` proxy also retains `TopToolsProvider`. Loop forwards parameters both when it builds the normal model tool list and when it rebuilds the filtered list. Prepared dispatch validates the captured schema before asking policy, so malformed arguments can be tested for rejection before policy or execution. No new schema channel or registry redesign is needed for this slice.

## Field contracts and intentional boundaries

The proposed fields, requiredness, and types follow the concrete executors:

| Tool | Model schema contract |
| --- | --- |
| `web_search` | Required `query: string` |
| `web_fetch` | Required `url: string` |
| `todo_write` | Required `action: string` enum `add`, `update`, `list`, `remove`; optional string `id`, `content`, `status`, `priority` |
| `task_create` | Required `title: string`; optional `description: string` |
| `task_update` | Required `id: string`; optional `status`, `title`, `description` strings |
| `task_get` | Required `id: string` |
| `task_list` | No properties |
| `goal_create` | Required `title: string`; optional `description`, `success_criteria` strings |
| `goal_update` | Required `id: string`; optional `status`, `title`, `description`, `priority`, `success_criteria` strings |
| `goal_get` | Required `id: string` |
| `goal_list` | No properties |
| `enter_plan_mode` | Optional `reason: string` |
| `exit_plan_mode` | Optional `summary: string` |
| `ask_user` | Required `question: string`; optional `options: array<string>` |
| `tool_search` | Optional `query: string` and `category: string` |

All schemas should be root objects with `additionalProperties: false`. No JSON Schema defaults should be added: the executor/store owns defaults, and the prepared validator does not apply a schema `default`. In particular, `todo_write.add` defaults empty-or-omitted `priority` and `status` to `medium` and `pending`; `task_create` creates with `pending`; `goal_create` creates with `draft` and `medium`. Task status and priority remain unconstrained strings because their stores accept arbitrary values. Do not infer a task status enum from the constants or comments.

The enum sources are correct. `todo_write` dispatches only exact lowercase `add`, `update`, `list`, and `remove`. `goal_update` parses six canonical statuses (`draft`, `active`, `in_progress`, `completed`, `blocked`, `abandoned`) after trimming and lowercasing; the model schema may deliberately accept only those canonical spellings. `tool_search` normalizes category case and surrounding whitespace and recognizes `file`, `network`, `shell`, and `other`.

For `tool_search.category`, retain the currently accepted explicit empty-string sentinel: the schema enum must be `""`, `"file"`, `"network"`, `"shell"`, and `"other"`, with the property still optional. Both omission and `""` mean no category filter in the executor. Uppercase and whitespace-padded category/status aliases may be rejected on the prepared model path while remaining accepted by direct execution; parity tests should state that direction explicitly rather than expecting aliases through a canonical enum.

The schema is intentionally a stricter model-call boundary than direct `Execute` in documented cases. Closed schemas reject unknown keys that raw executors may ignore, and typed optional fields reject wrong types that some executors treat as absent. `ask_user` converts JSON arrays to strings by dropping non-string members, while `array<string>` rejects such input before policy. These do not require changing executor permissiveness. Required strings remain structurally required but may be blank because `minLength` is unsupported; blankness stays an executor check. Likewise, `todo_write` action-dependent fields (`content` for `add`, `id` and `status` for `update`, `id` for `remove`) remain runtime checks; do not expand the validator with conditional-schema machinery in this slice.

`web_search`/`web_fetch` provider configuration, URL safety, timeout, and response-size limits remain runtime concerns rather than schema arguments. `ask_user`'s empty-string check is exact, so whitespace-only questions currently pass the executor. The plan-mode schemas must leave `exit_plan_mode` execution and approval behavior unchanged; D-01 remains deferred.

## Validation plan review

The proposed acceptance sequence is appropriately bounded for a schema-only change:

1. Table-driven schema contract tests for all 15 missing schemas, including required keys, types, exact enum values, empty category sentinel, closed properties, and schema snapshot isolation. Assert there are no unsupported keywords or JSON Schema defaults.
2. Prepared-registry positive and negative cases with policy/executor witnesses. Invalid structure, wrong types, invalid enums, and extra properties must stop before policy. Cover blank required strings and action-dependent todo omissions as runtime failures after structural validation. Test direct-executor behavior separately where it intentionally differs from the prepared model boundary. For canonical enums, assert raw case/space aliases remain accepted while prepared calls reject them before policy.
3. Use the proposed injected/local fixtures (fake `WebSearchProvider`, `httptest.Server`, fresh stores, recording plan controller, auto-answer HITL emitter, and real registry-backed `ToolSearchTool`) so tests do not contact external services or create GitHub state.
4. Decode equivalent JSON tool arguments through the actual OpenAI-compatible and Anthropic argument adapters, including `ask_user.options` as a JSON array, then send them through prepared validation. This is a focused adapter parity check, not a provider or network integration test.
5. Assert the real assembled protected registry exposes schemas for the fixed extras and that the Loop's model tool definitions preserve them; additionally verify `tool_search` still provides ranked filtering through its protected capability.

The broader R4 gates remain separate. A green schema suite alone does not establish provider readiness, broader effect classification, or full R4 closure.

## Review method and limits

I reviewed the handoff at the target commit and compared its registry, executor, schema-freeze/validation, and Loop references against the source snapshot. This is a static design review; no product code or tests were changed or run. Final implementation review should confirm the category sentinel and directional alias tests in the resulting schema diff.
