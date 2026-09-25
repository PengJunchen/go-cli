# R0 Permissions Lane

Branch: `codex/runtime-r0-permissions`

Baseline: `00149001e2350b18dd69d7fa41ab7fcd6ed0241a`

## Delivered

- Approval callback `ApprovalAllow` permits its current call only. It does not populate the middleware session map or `ApprovalStore`; an identical later call consults the classifier and prompts again unless an earlier explicit `ApprovalAlwaysAllow` is already cached. Classifier-authored decisions, automatic policy decisions, and explicit `ApprovalAlwaysAllow` retain their reusable behavior.
- Write and Edit expose required JSON Schema fields. Their executors reject missing or non-string `content`/`new_string` before mutation. The mutation queue validates payload types before enqueueing so missing values cannot be transformed into empty strings. Explicit empty strings continue to work through the queue.
- Active Plan Mode now uses a temporary read-only allowlist drawn from tools actually registered by the CLI: `read`, `ls`, `grep`, `find`, `lsp_query`, `tool_search`, `ask_user`, `git_diff`, `git_status`, `git_log`, `git_branch`, `git_blame`, `git_remote`, `web_fetch`, `web_search`, `goal_get`, `goal_list`, `task_get`, `task_list`, `enter_plan_mode`, and `exit_plan_mode`. Git state mutations, remote Bash, todo/task/goal mutations, subagent dispatch, and unknown MCP/custom/extension names fail closed. `exit_plan_mode` preserves its existing behavior while D-01 remains deferred.

## Evidence

- Baseline red run: [permissions-red.log](evidence/permissions-red.log)
- Passing targeted and race runs: [permissions-green.log](evidence/permissions-green.log)

## Commits

- Approval: `adf0fdbc433a5cdba33124ff515388515b184354`
- File schema and mutation queue: `8833fa56bfb0a3281605a66e9b0d05979572b0fb`
- Plan Mode: `c3cb66a1f39b86394acd275e09d3c41739c711d4`

## Scope and limitations

Only `internal/approval/`, `internal/tools/{write,edit,middleware}.go` and their tests, and `internal/core/plan_mode*.go` and tests are changed. The general tool effect/capability model remains in R4. Plan Mode uses tool names as a stopgap, so a dynamically registered tool that reuses an allowlisted built-in name is not distinguished by executor identity. The model's current ability to call `exit_plan_mode` is preserved while the D-01 policy remains deferred.

`make verify` and the full repository suite were not run in this lane; the three affected package race suites passed.
