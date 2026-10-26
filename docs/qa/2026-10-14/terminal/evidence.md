# Real terminal E2E evidence

> Retention note (2026-10-26): the 12 historical review screenshots and `run-report.json` were restored byte-for-byte from `origin/main@8e65b284ad55299b80f46202d903384a71a85d3c`; see [the restoration manifest](../../../iterations/2026-09-25-runtime/evidence/restoration-2026-10-26-manifest.json). This report and its images describe the 2026-10-14 capture and are not current verification. Raw PTY/ANSI transcripts remain deleted.


- Revision: `develop@89df724c491083a81239acc04bd42269c58ea6e0`
- Working-tree source delta SHA-256: `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` (source paths only; QA evidence is excluded).
- Built binary SHA-256: `852776240ba41f069183201a07bb5b89a2a3e454bef88e837b1581afe7b278f5`
- Screenshot renderer: Chrome headless screenshot of the xterm.js visible viewport fed with raw bytes read from the real CLI PTY; not a text-to-image rendering and not a native Terminal window
- HTTP fixture: the provider API endpoint is the localhost mock HTTP fixture; no live model API key is supplied
- Process isolation: fresh temporary HOME and working directory per scenario; GO_CLI_* inherited values removed; API auth is a fixture value
- Terminal color profile: TERM=xterm-256color and COLORTERM=truecolor; inherited NO_COLOR removed in child process
- Transcript handling: visible-screen assertions read only xterm.js buffer.viewportY through rows; raw PTY byte streams were captured separately per scenario, then removed during the 2026-10-24 retention cleanup; this report retains only their recorded sizes and hashes.
- Evidence class: real binary PTY end-to-end; UI pixels are from xterm.js in headless Chrome.

## Scenarios

### streaming answer

- PASS: startup banner rendered in TTY
- PASS: narrow xterm viewport and real PTY report identical columns and rows
- PASS: normal color profile is not suppressed by inherited NO_COLOR
- PASS: UTF-8 Chinese answer arrived in multiple SSE content chunks
- PASS: one local mock request used fixture authorization
- PASS: interactive REPL exited cleanly
- Screenshot: [`01-startup.png`](01-startup.png) — 560×360 requested px; PNG 560×360 px; PTY/xterm 64×20 cells; 0 ANSI-colored cells; assertions: {"Interactive session started": true}.
- Screenshot: [`02-chinese-stream-midstream.png`](02-chinese-stream-midstream.png) — 560×360 requested px; PNG 560×360 px; PTY/xterm 64×20 cells; 36 ANSI-colored cells; assertions: {"这是本地模拟": true}.
- Screenshot: [`03-chinese-stream-complete.png`](03-chinese-stream-complete.png) — 560×360 requested px; PNG 560×360 px; PTY/xterm 64×20 cells; 36 ANSI-colored cells; assertions: {"这是本地模拟模型逐段返回的中文答案。": true}.
- Raw PTY transcript (removed 2026-10-24; metadata only): 1175 bytes, SHA-256 `0bfce81ccf119f60de09df88dd6805b15456dc8c4481c8988e5c05720411532e`.
- Mock counters: 1 request(s), fixture authorization observed: true, denial receipt: false, staged output receipt: false.

### approval deny

- PASS: mock proposes the complete long bash command
- PASS: approval screen appears before any shell side effect or tool receipt
- PASS: approval command's visible output token is not mistaken for a started task
- PASS: wide xterm viewport and real PTY report matching dimensions with at least 120 columns
- PASS: normal color profile is not suppressed by inherited NO_COLOR
- PASS: Page Down scrolled the long approval details
- PASS: resize keeps xterm.js and PTY column/row counts identical
- PASS: narrow resize resets review position and increases wrapped detail rows
- PASS: all approval pages were reachable after resize
- PASS: denial tool receipt returned to the mock model
- PASS: denied shell tool receipt contains no staged output
- PASS: denied shell produced no stage files
- PASS: denial answer rendered to terminal
- PASS: interactive REPL exited cleanly
- Screenshot: [`04-approval-deny-wide-page-one.png`](04-approval-deny-wide-page-one.png) — 1440×900 requested px; PNG 1440×900 px; PTY/xterm 169×50 cells; 14 ANSI-colored cells; assertions: {"[approval]": true, "bash": true, "scope: unknown": true, "view truncated": true, "PgUp/PgDn": true}.
- Screenshot: [`05-approval-deny-wide-page-two.png`](05-approval-deny-wide-page-two.png) — 1440×900 requested px; PNG 1440×900 px; PTY/xterm 169×50 cells; 14 ANSI-colored cells; assertions: {"details 20-39/42": true}.
- Screenshot: [`06-approval-deny-narrow-after-resize.png`](06-approval-deny-narrow-after-resize.png) — 560×400 requested px; PNG 560×400 px; PTY/xterm 64×22 cells; 14 ANSI-colored cells; assertions: {"details 1-17/77": true}.
- Screenshot: [`07-approval-denied-complete.png`](07-approval-denied-complete.png) — 560×400 requested px; PNG 560×400 px; PTY/xterm 64×22 cells; 38 ANSI-colored cells; assertions: {"本地模拟器收到拒绝凭据": true}.
- Raw PTY transcript (removed 2026-10-24; metadata only): 8800 bytes, SHA-256 `a9ef98b3fa4f6ad9267cd7968fd3f21d6d24c080f3124a8426bc1edcf3a671db`.
- Mock counters: 2 request(s), fixture authorization observed: true, denial receipt: true, staged output receipt: false.
- Fixture tool call: `bash` / `call-terminal-e2e`, 41 command lines, response SHA-256 `c0ef6c04da913c42eb794bd1d92bc5c209dbeccbaabfac5f0c5436381dad34a6`.

### approval allow

- PASS: mock proposes the complete long bash command
- PASS: approval screen appears before any shell side effect or tool receipt
- PASS: approval command's visible output token is not mistaken for a started task
- PASS: wide xterm viewport and real PTY report matching dimensions with at least 120 columns
- PASS: normal color profile is not suppressed by inherited NO_COLOR
- PASS: Page Down scrolled the long approval details
- PASS: all wide approval pages were reviewed before allow
- PASS: wide running view visibly renders the side-by-side conversation and tool panes
- PASS: running task follow-up editor preserves English spaces and Chinese text
- PASS: allow key caused the first filesystem side effect before task completion
- PASS: visible running tool view arrived before the tool receipt
- PASS: tool receipt returned all three stage outputs under the fixture call ID
- PASS: submitted follow-up reached the mock model unchanged in the provider request
- PASS: all three stage files contain their expected side effects
- PASS: long task completed and final answer rendered
- PASS: interactive REPL exited cleanly
- Screenshot: [`08-approval-allow-wide-page-one.png`](08-approval-allow-wide-page-one.png) — 1440×900 requested px; PNG 1440×900 px; PTY/xterm 169×50 cells; 14 ANSI-colored cells; assertions: {"[approval]": true, "bash": true, "scope: unknown": true, "view truncated": true, "PgUp/PgDn": true}.
- Screenshot: [`09-approval-allow-wide-page-two.png`](09-approval-allow-wide-page-two.png) — 1440×900 requested px; PNG 1440×900 px; PTY/xterm 169×50 cells; 14 ANSI-colored cells; assertions: {"details 20-39/42": true}.
- Screenshot: [`10-approval-allow-wide-last-page.png`](10-approval-allow-wide-last-page.png) — 1440×900 requested px; PNG 1440×900 px; PTY/xterm 169×50 cells; 14 ANSI-colored cells; assertions: {"details 23-42/42": true}.
- Screenshot: [`11-long-bash-running-followup-edited.png`](11-long-bash-running-followup-edited.png) — 1440×900 requested px; PNG 1440×900 px; PTY/xterm 169×50 cells; 0 ANSI-colored cells; assertions: {"working...": true, "[tool] bash": true, "│": true, "followup>": true, "During the active run, keep xterm colors stable and inspect 日志窗口。": true}.
- Screenshot: [`12-long-bash-complete.png`](12-long-bash-complete.png) — 1440×900 requested px; PNG 1440×900 px; PTY/xterm 169×50 cells; 64 ANSI-colored cells; assertions: {"本地模拟器确认三阶段 shell 命令全部完成，并收到 follow-up 指令。": true}.
- Raw PTY transcript (removed 2026-10-24; metadata only): 9478 bytes, SHA-256 `84c95f2dfb23e93fa785f1363588fada7ebed251bd68517b48c7a6a32da1fde3`.
- Mock counters: 2 request(s), fixture authorization observed: true, denial receipt: false, staged output receipt: true.
- Fixture tool call: `bash` / `call-terminal-e2e`, 41 command lines, response SHA-256 `c0ef6c04da913c42eb794bd1d92bc5c209dbeccbaabfac5f0c5436381dad34a6`.
- Follow-up delivered in provider request: `During the active run, keep xterm colors stable and inspect 日志窗口。`.

The binary is launched with stdin/stdout/stderr attached to a PTY. The capture process forwards the PTY's original byte stream into xterm.js and asks Chromium to screenshot that terminal viewport. The harness does not generate terminal pixels from extracted text.

## Capture lineage

This report and its 12 PNGs are the historical capture from `89df724c491083a81239acc04bd42269c58ea6e0`; the 3 raw PTY transcripts from that capture were removed during the 2026-10-24 retention cleanup and remain unavailable. The images replace the same-named terminal evidence from the earlier `224462776911986f0c9afba6b895acce1007458e` run. The older e77 gate reports at `docs/qa/2026-10-14/gates/test-e2e-e77.json` and `verify-e77.json` remain separate historical evidence and were not changed by this capture.
