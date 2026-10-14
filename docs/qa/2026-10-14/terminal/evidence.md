# Real terminal E2E evidence

- Revision: `develop@e77e8058c51e5370a2ecc3eacb5fce0419829bcf`
- Working-tree source delta SHA-256: `f9c3c483067a969ba43e275ea910bc4ee5410b3ef2444ba5097f005742e94581` (source paths only; QA evidence is excluded).
- Built binary SHA-256: `c78011df02ac021493d9124fb9d71536926be00c1b0265e88758a78e9a4413cd`
- Screenshot renderer: Chrome headless screenshot of the xterm.js visible viewport fed with raw bytes read from the real CLI PTY; not a text-to-image rendering and not a native Terminal window
- HTTP fixture: the provider API endpoint is the localhost mock HTTP fixture; no live model API key is supplied
- Process isolation: fresh temporary HOME and working directory per scenario; GO_CLI_* inherited values removed; API auth is a fixture value
- Terminal color profile: TERM=xterm-256color and COLORTERM=truecolor; inherited NO_COLOR removed in child process
- Transcript handling: visible-screen assertions read only xterm.js buffer.viewportY through rows; raw PTY byte streams are retained separately per scenario
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
- Raw PTY transcript: [`streaming-answer.pty.ansi`](streaming-answer.pty.ansi) — 1175 bytes, SHA-256 `0bfce81ccf119f60de09df88dd6805b15456dc8c4481c8988e5c05720411532e`.
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
- Raw PTY transcript: [`approval-deny.pty.ansi`](approval-deny.pty.ansi) — 9076 bytes, SHA-256 `33a2246ff078f3f7554cf697a9b4a19a5a0266391d9fd53581b347a17079d2bd`.
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
- Raw PTY transcript: [`approval-allow.pty.ansi`](approval-allow.pty.ansi) — 9669 bytes, SHA-256 `9f2c27962ac8f9bea2cbd77a0a8e5b988167e825d462faf0c302a476217cfdca`.
- Mock counters: 2 request(s), fixture authorization observed: true, denial receipt: false, staged output receipt: true.
- Fixture tool call: `bash` / `call-terminal-e2e`, 41 command lines, response SHA-256 `c0ef6c04da913c42eb794bd1d92bc5c209dbeccbaabfac5f0c5436381dad34a6`.
- Follow-up delivered in provider request: `During the active run, keep xterm colors stable and inspect 日志窗口。`.

The binary is launched with stdin/stdout/stderr attached to a PTY. The capture process forwards the PTY's original byte stream into xterm.js and asks Chromium to screenshot that terminal viewport. The harness does not generate terminal pixels from extracted text.

## Post-run portability follow-up

After this E2E capture, the harness's scratch directory selection was changed
to use Python's `tempfile.gettempdir()` by default, with `--scratch-root` as an
override. The previously captured binary and screenshots were not rebuilt or
rerun for this harness-only setup change. Python and renderer syntax checks,
`run.py --help`, and a startup smoke that created and removed a run directory
under `/private/var/folders/_l/f743070s7yj3qk47q2krwqt00000gn/T` passed. The
source working-tree delta after that change is recorded in `run-report.json`.
The harness is runtime-verified on macOS; Linux is intended but not runtime-
tested in this environment.
