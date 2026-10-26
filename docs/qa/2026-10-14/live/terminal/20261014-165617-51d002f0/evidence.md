# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6.1-sol`
- Magpie session: `go-cli-live-20261014-25968c950f6d`
- Run nonce: `25968c950f6d`
- Race detector clean: `True`
- Evidence class: real go-cli binary + real PTY + requests transparently forwarded to signed-in Magpie Codex. No model responses are synthesized.
- UI evidence: actual PTY bytes rendered by xterm.js in headless Chromium; screenshots are not native Terminal captures.
- Credential handling: isolated CLI HOME, dummy local API sentinel, no stored Authorization headers, request bodies, or full response bodies.

## Checks

- PASS: actual model response contains requested exact text marker
- PASS: actual upstream assistant response includes exact alpha fixture fact
- PASS: actual upstream assistant response includes exact beta fixture fact
- PASS: actual inference selected ls, literal grep, and read tools
- PASS: both real read tool results were delivered back to the model
- PASS: read calls target both nonce fixture files
- PASS: real tool receipts contain both exact fixture facts
- PASS: default unscoped grep retains zero scope and unknown effect while ls/read retain canonical workspace scope
- PASS: default unscoped grep approval screenshot precedes a successful exact-fact grep receipt and read receipts
- PASS: denied write target was absent before and remains absent after human denial
- PASS: upstream request after denial carries a denied tool receipt
- PASS: human-style write approval produced a file and its matching durable completion proof
- PASS: human-style edit approval applied the exact replacement
- PASS: the model's Edit call has a later receipt and durable quiescent completion proof
- PASS: actual upstream assistant response reports the edited content
- PASS: real sequential Bash created both start and finish markers
- PASS: follow-up mode was entered and submitted while the start marker existed and finish marker was absent
- PASS: live TUI displayed a busy footer while the long Bash command was running
- FAIL: user follow-up marker reached a later Magpie inference request
- FAIL: Bash tool receipt reached a later inference request
- PASS: long Bash call has an individual later receipt and durable quiescent completion proof
- FAIL: interactive CLI exited successfully
- PASS: Go race detector reported no DATA RACE in the real PTY session
- PASS: live PTY produced actual Chat Completions inference requests
- PASS: allow-write: every real inference completed HTTP 200 with its own response ID
- PASS: allow-write: complete response JSON metadata was parsed without changing provider bytes
- PASS: allow-write: every upstream response identifies signed-in Codex and the requested served model
- PASS: allow-write: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: allow-write: each request joins to this session's completed route and nonzero usage
- PASS: allow-write: per-request upstream and Magpie token usage reconciles or is explicitly undefined
- PASS: deny-write: every real inference completed HTTP 200 with its own response ID
- PASS: deny-write: complete response JSON metadata was parsed without changing provider bytes
- PASS: deny-write: every upstream response identifies signed-in Codex and the requested served model
- PASS: deny-write: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: deny-write: each request joins to this session's completed route and nonzero usage
- PASS: deny-write: per-request upstream and Magpie token usage reconciles or is explicitly undefined
- PASS: edit-written-file: every real inference completed HTTP 200 with its own response ID
- PASS: edit-written-file: complete response JSON metadata was parsed without changing provider bytes
- PASS: edit-written-file: every upstream response identifies signed-in Codex and the requested served model
- PASS: edit-written-file: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: edit-written-file: each request joins to this session's completed route and nonzero usage
- PASS: edit-written-file: per-request upstream and Magpie token usage reconciles or is explicitly undefined
- PASS: escape-cancel: every real inference completed HTTP 200 with its own response ID
- PASS: escape-cancel: complete response JSON metadata was parsed without changing provider bytes
- PASS: escape-cancel: every upstream response identifies signed-in Codex and the requested served model
- PASS: escape-cancel: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: escape-cancel: each request joins to this session's completed route and nonzero usage
- PASS: escape-cancel: per-request upstream and Magpie token usage reconciles or is explicitly undefined
- PASS: inspect-fixtures: every real inference completed HTTP 200 with its own response ID
- PASS: inspect-fixtures: complete response JSON metadata was parsed without changing provider bytes
- PASS: inspect-fixtures: every upstream response identifies signed-in Codex and the requested served model
- PASS: inspect-fixtures: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: inspect-fixtures: each request joins to this session's completed route and nonzero usage
- PASS: inspect-fixtures: per-request upstream and Magpie token usage reconciles or is explicitly undefined
- PASS: long-bash-followup: every real inference completed HTTP 200 with its own response ID
- PASS: long-bash-followup: complete response JSON metadata was parsed without changing provider bytes
- PASS: long-bash-followup: every upstream response identifies signed-in Codex and the requested served model
- PASS: long-bash-followup: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: long-bash-followup: each request joins to this session's completed route and nonzero usage
- PASS: long-bash-followup: per-request upstream and Magpie token usage reconciles or is explicitly undefined
- PASS: plain-live-text: every real inference completed HTTP 200 with its own response ID
- PASS: plain-live-text: complete response JSON metadata was parsed without changing provider bytes
- PASS: plain-live-text: every upstream response identifies signed-in Codex and the requested served model
- PASS: plain-live-text: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: plain-live-text: each request joins to this session's completed route and nonzero usage
- PASS: plain-live-text: per-request upstream and Magpie token usage reconciles or is explicitly undefined

## Screenshots

- [`01-connected.png`](01-connected.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`02-live-text-answer.png`](02-live-text-answer.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`inspect-fixtures-1-approval-allow-page-1.png`](inspect-fixtures-1-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`02-live-tools-answer.png`](02-live-tools-answer.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`03-deny-approval-deny-page-1.png`](03-deny-approval-deny-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`04-allow-approval-allow-page-1.png`](04-allow-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`04-edit-approval-allow-page-1.png`](04-edit-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`05-long-bash-approval-allow-page-1.png`](05-long-bash-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`06-long-bash-running.png`](06-long-bash-running.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`06-followup-input-ready.png`](06-followup-input-ready.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`06-followup-input-filled.png`](06-followup-input-filled.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`07-followup-submitted.png`](07-followup-submitted.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`08-long-bash-followup-complete.png`](08-long-bash-followup-complete.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`09-cancel-command-approval-allow-page-1.png`](09-cancel-command-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`09-cancel-command-running.png`](09-cancel-command-running.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`10-escape-sent.png`](10-escape-sent.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`11-escape-cancelled-stable.png`](11-escape-cancelled-stable.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.

## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1831, in run_live
    start_turn(session, bridge, label, f"Fresh prompt after cancellation. Reply with exactly {fresh_answer}; do not use tools.", [fresh_answer])
    ~~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1011, in start_turn
    session.send_line(normalize(prompt.splitlines()))
    ~~~~~~~~~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 618, in send_line
    self.send(data[offset:offset + 2048])
    ~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 610, in send
    os.write(self.master_fd, value)
    ~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^
OSError: [Errno 5] Input/output error

```
