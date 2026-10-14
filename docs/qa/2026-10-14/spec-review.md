# Spec review

Review target: the request to validate `develop` at `e77e8058c51e5370a2ecc3eacb5fce0419829bcf` with a deterministic mock LLM, using the real terminal UI, tool execution, and long-task workflows; fix discovered defects; and report evidence without claiming unverified results.

## Coverage

- **Terminal UI:** [`terminal/evidence.md`](terminal/evidence.md) and [`terminal/run-report.json`](terminal/run-report.json) record 36/36 checks across startup, streamed Chinese output, approval denial and approval, long-running Bash, follow-up input, and completion. The evidence includes 12 PNG screenshots and three raw PTY ANSI transcripts. I inspected screenshots 11 and 12 and the renderer: real CLI PTY bytes are fed to xterm.js, and captures/assertions use the current `buffer.viewportY` viewport.
- **Tools and persistence:** [`tools/approval-binary-e2e.md`](tools/approval-binary-e2e.md) records real-binary read/write/edit/bash auto and deny behavior, default allowed-path read and non-TTY write denial, recoverable edit error followed by correction, and session resume in a second process. Assertions check provider receipts and filesystem effects.
- **Long tasks:** [`longtask/evidence.md`](longtask/evidence.md) and [`longtask/results.json`](longtask/results.json) record a real race-instrumented CLI completing 120 local HTTP turns with compaction, a PTY follow-up verified in the actual second provider request after production queue acknowledgment, and Escape cancellation with the delayed side effect absent. The focused long-task run passed.
- **Discovered defect:** the follow-up and steering editors dropped spaces. The narrow `tea.KeySpace` fix in `internal/tui/app.go` has red/green regression evidence and the terminal follow-up scenario verifies spaces and Chinese text reach the provider unchanged.
- **Regression gate:** [`gates/verify.json`](gates/verify.json) records `make verify` exit 0: 8,700 passed, 0 failed, 6 skipped across 28 packages; lint reported 0 issues; the scanner reported 0 errors and 1,803 warnings; log and leak gates passed 10/10 and 6/6.

## Scope and limits

The implementation and test harness map to the requested terminal, tools, persistence, and long-task acceptance scope. The only production behavior change addresses the reproduced input defect; the other code changes support requested E2E coverage and existing test-fixture reliability. No unrelated product feature was added.

The provider is deterministic and local, so this is protocol and CLI acceptance, not live-model quality testing. Screenshots are genuine xterm.js viewport pixels in headless Chrome, not native Terminal captures. Runtime evidence is from Darwin; Linux was cross-compiled but not run. The long-task Bash invocations use `--no-sandbox`, so the evidence does not validate OS sandbox enforcement.

## Current verdict

Spec coverage is sufficient, with no identified missing requested scenario or unrelated feature work. **Final acceptance remains pending the current full tagged E2E suite.** `longtask/results.json` retains an earlier tagged-suite failure caused by releasing Bash before the production follow-up queue acknowledgment; the harness was corrected and the focused rerun passed. Do not mark the overall task passed until the current full tagged run is recorded.
