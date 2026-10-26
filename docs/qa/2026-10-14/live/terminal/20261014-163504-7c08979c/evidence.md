# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6.1-sol`
- Magpie session: `go-cli-live-20261014-5eea0d10a457`
- Run nonce: `5eea0d10a457`
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

## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1705, in run_live
    report["screenshots"].append(session.capture(evidence, "07-followup-queued", [followup_marker]))
                                 ~~~~~~~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 722, in capture
    raise AssertionError(f"screenshot {name} missing visible assertions {missing}: {screen[-800:]}")
AssertionError: screenshot 07-followup-queued missing visible assertions ['FOLLOWUP_5eea0d10a457_QUEUE_ACK']: ing exactly ALLOWE 57.txt D_5eea0d10a457. Request the approval as usual. [receipt: success] [assistant] [tool call_e09d0a7aeeeb4116b035165a902810fb] I’ll request the write action with the exact filename and content you specified. bash [assistant] Created `approved_5eea0d10a457.txt` containing exactly `ALLOWED_5eea0d10a457`. [user] Use the edit tool to replace the exact text ALLOWED_5eea0d10a457 with EDITED_5eea0d1 0a457 in approved_5eea0d10a457.txt. Preserve the rest of the file and report the fin al contents. [assistant] I’ll replace the exact text using the edit tool and preserve the rest of the file. [assistant] EDITED_5eea0d10a457 [assistant · live] I’ll run the command and report completion after the finished file is created. > followup> █ codex/gpt-6.1-sol | turn #6 | interact | chat

```
