# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6-luna`
- Magpie session: `go-cli-live-20261014-040d1c634402`
- Run nonce: `040d1c634402`
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
- PASS: human-style write approval allowed real file creation
- PASS: interactive CLI exited successfully
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

## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1463, in run_live
    report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
                           ~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 837, in wait_turn
    result = drive_approval(session, evidence, f"{label}-{len(approvals_for_turn)+1}", "allow", screenshots, timeout=min(300, timeout))
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 795, in drive_approval
    screen = session.wait_for("[approval]", timeout)
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 646, in wait_for
    raise TimeoutError(f"timed out waiting for {needle!r}; last screen: {last[-1000:]}")
TimeoutError: timed out waiting for '[approval]'; last screen: 40d1c634402.txt) w replaced 20-byte block with 19-byte block in /Users/yinyuepeng/projects/go-cli/docs/ ith exactly `ALLOWED_040d1c634402`. qa/2026-10-14/live/workspaces/20261014-155705-a891a2fe/workspace/approved_040d1c6344 [user] 02.txt Use the edit tool to replace the exact text ALLOWED_040d1c634402 with EDITED_040d1c6 [receipt: success] 34402 in approved_040d1c634402.txt. Preserve the rest of the file and report the fin [tool: read] al contents. EDITED_040d1c634402 [assistant] [receipt: success] I’ll make the exact text replacement, then read the file to confirm its final conten ts. [assistant] [assistant] The final contents of [approved_040d1c634402.txt](/Users/yinyuepeng/projects/go-cli/ docs/qa/2026-10-14/live/workspaces/20261014-155705-a891a2fe/workspace/approved_040d1 c634402.txt) are: `EDITED_040d1c634402 codex/gpt-6-luna | turn #5 | interact | chat Tokens: 114740/0 (0%) | Cost: unknown Sources: usage=reported/u0 cost=unknown/c13 price=unknown actual=openai/codex/gpt-6-luna >

```
