# R0 code review

Review date: 2026-09-25  
Review target: permission commits `e783d5f`, `12c68de`, and `58782ef`; evidence ledger `lane-permissions.md`.  
Review type: source review of the R0 permission, file-argument, and Plan Mode slice. I did not modify product code or rerun the builder's tests.

## Review result

The permission/file/schema changes are acceptable for the R0 hotfix slice, subject to the explicitly tracked Plan Mode executor-identity residual below. This is not a security-release approval: R4 must close that residual before the product is treated as safe under Plan Mode.

`ApprovalMiddleware` now distinguishes transient callback decisions from reusable decisions. Callback `ApprovalAllow`, callback denial, and callback errors bypass the middleware's session and store writes; `ApprovalAlwaysAllow` remains reusable. The regression invokes identical calls twice and checks that the callback is asked twice and the store receives no decision writes. This matches the current classifier-Ask path and leaves static classifier decisions intact.

`WriteTool` and `EditFileTool` now implement `Parameterized` schemas with the required file arguments, and their executors reject missing or non-string `content`/`new_string` before file access or mutation. Explicit empty strings remain valid. The mutation queue wrapper checks the required mutation payload before calling `Enqueue`; tests cover missing and wrong-typed values through direct execution and the queue path, preservation of original bytes, and explicit empty values.

Plan Mode now blocks names outside its exact built-in read/control allowlist, including unknown and external mutation names. The allowlist matches the reviewed current registrations: `read`, `ls`, `grep`, `find`, `web_search`, `web_fetch`, read-only Git tools, `lsp_query`, `tool_search`, `goal_get/list`, and `task_get/list`. Control-plane exceptions are `ask_user`, `enter_plan_mode`, and `exit_plan_mode`; R0 preserves the current exit behavior, while long-term D-01 remains deferred. This review found no further R0 implementation blocker in these three commits. Builder race evidence is recorded in the lane ledger; it was not rerun here.

## R4 mandatory follow-up: bind Plan Mode to the resolved executor

The current name-only Plan Mode guard has a reachable same-name executor bypass. `DefaultToolRegistry.Register` documents and implements last-registration-wins behavior (`internal/tools/registry.go:20-22,39-58`). `assembleExtensions` registers plugin-provided tools after built-ins without checking for a collision (`internal/cli/assemble.go:921-929`). An extension can therefore register an arbitrary side-effect tool named `read`, replacing the read-only builtin. `ShouldBlockWrite` checks only the call's name against the allowlist (`internal/core/plan_mode.go:85-99`), so the replacement passes while Plan Mode is active.

R4 must prevent this by binding effect/permission decisions to the resolved tool identity or by rejecting unsafe builtin replacement at registration. The R0 change only tightens unknown-name behavior and preserves current read/control behavior; it does not close this executor-identity path and must not be described as safe-release closure.
