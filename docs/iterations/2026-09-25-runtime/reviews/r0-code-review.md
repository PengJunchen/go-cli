# R0 code review

Review date: 2026-09-25  
Review target: permission commits `e783d5f`, `12c68de`, and `58782ef`; Streaming Bash output capture and reader-error coverage through `2178cd5`; evidence ledgers `lane-permissions.md` and `bash-green.log`.  
Review type: source review of the R0 permission, file-argument, Plan Mode, and Streaming Bash slices. I did not modify product code. I reran the two new reader-error tests.

## Review result

The permission/file/schema changes are acceptable for the R0 hotfix slice, subject to the explicitly tracked Plan Mode executor-identity residual below. This is not a security-release approval: R4 must close that residual before the product is treated as safe under Plan Mode.

`ApprovalMiddleware` now distinguishes transient callback decisions from reusable decisions. Callback `ApprovalAllow`, callback denial, and callback errors bypass the middleware's session and store writes; `ApprovalAlwaysAllow` remains reusable. The regression invokes identical calls twice and checks that the callback is asked twice and the store receives no decision writes. This matches the current classifier-Ask path and leaves static classifier decisions intact.

`WriteTool` and `EditFileTool` now implement `Parameterized` schemas with the required file arguments, and their executors reject missing or non-string `content`/`new_string` before file access or mutation. Explicit empty strings remain valid. The mutation queue wrapper checks the required mutation payload before calling `Enqueue`; tests cover missing and wrong-typed values through direct execution and the queue path, preservation of original bytes, and explicit empty values.

Plan Mode now blocks names outside its exact built-in read/control allowlist, including unknown and external mutation names. The allowlist matches the reviewed current registrations: `read`, `ls`, `grep`, `find`, `web_search`, `web_fetch`, read-only Git tools, `lsp_query`, `tool_search`, `goal_get/list`, and `task_get/list`. Control-plane exceptions are `ask_user`, `enter_plan_mode`, and `exit_plan_mode`; R0 preserves the current exit behavior, while long-term D-01 remains deferred. This review found no further R0 implementation blocker in these three commits. Builder race evidence is recorded in the lane ledger; it was not rerun here.

## Streaming Bash output and reader-error review

The R0 Streaming Bash capture change is acceptable for this slice. The shared capture applies `MaxOutput` to retained stdout and stderr source bytes together, accounts for the separator, and adds only the fixed truncation marker after the cap. Both scanner-error and cap-hit paths drain the remaining pipe bytes before the reader returns, allowing finite child commands to finish instead of blocking on full pipes. A scanner/read error is retained in the returned `ToolResult` metadata and also returned as an execution error; if the subsequent drain itself fails, the result preserves both errors and reports that the pipe was not drained.

Commit `2178cd5` closes the previously missing non-EOF reader-error test gap. `TestReadPipeReportsReaderErrorAfterDraining` injects a reader error after data, verifies the error survives a successful EOF drain, and checks bounded truncated output. `TestReadPipeReportsDrainFailure` injects a second error during drain, verifies both causes remain discoverable, and checks the same output bound. I reran both tests with `rtk env GOMAXPROCS=2 go test -p 2 ./internal/tools -run '^(TestReadPipeReportsReaderErrorAfterDraining|TestReadPipeReportsDrainFailure)$' -count=1 -timeout 30s -v`; both passed. The recorded full `internal/tools` race suite also passes in `bash-green.log`. This accepts the R0 capture/read-error slice; the separately reproduced inherited-pipe cancellation delay remains an R1 process-lifecycle issue, not evidence that R0 cancellation is closed.

## R4 mandatory follow-up: bind Plan Mode to the resolved executor

The current name-only Plan Mode guard has a reachable same-name executor bypass. `DefaultToolRegistry.Register` documents and implements last-registration-wins behavior (`internal/tools/registry.go:20-22,39-58`). `assembleExtensions` registers plugin-provided tools after built-ins without checking for a collision (`internal/cli/assemble.go:921-929`). An extension can therefore register an arbitrary side-effect tool named `read`, replacing the read-only builtin. `ShouldBlockWrite` checks only the call's name against the allowlist (`internal/core/plan_mode.go:85-99`), so the replacement passes while Plan Mode is active.

R4 must prevent this by binding effect/permission decisions to the resolved tool identity or by rejecting unsafe builtin replacement at registration. The R0 change only tightens unknown-name behavior and preserves current read/control behavior; it does not close this executor-identity path and must not be described as safe-release closure.
