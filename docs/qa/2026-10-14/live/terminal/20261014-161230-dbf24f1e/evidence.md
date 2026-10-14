# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6.1-sol`
- Magpie session: `go-cli-live-20261014-a8fb820d308a`
- Run nonce: `a8fb820d308a`
- Race detector clean: `True`
- Evidence class: real go-cli binary + real PTY + requests transparently forwarded to signed-in Magpie Codex. No model responses are synthesized.
- UI evidence: actual PTY bytes rendered by xterm.js in headless Chromium; screenshots are not native Terminal captures.
- Credential handling: isolated CLI HOME, dummy local API sentinel, no stored Authorization headers, request bodies, or full response bodies.

## Checks

- PASS: actual model response contains requested exact text marker
- FAIL: interactive CLI exited successfully
- PASS: Go race detector reported no DATA RACE in the real PTY session
- PASS: live PTY produced actual Chat Completions inference requests
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

## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1519, in run_live
    report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
                           ~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 967, in wait_turn
    raise TimeoutError(f"turn {label} did not become idle in {timeout}s; screen={session.screen()[-1200:]}")
TimeoutError: turn inspect-fixtures did not become idle in 300.0s; screen=LPHA_A8FB820D308A=blue and LIVE_FACT_BETA_A8FB820D308A=green. [approval] grep local interaction #1 scope: unknown; once only; reuse unavailable details 1-3/3 args: path: /Users/yinyuepeng/projects/go-cli/docs/qa/2026-10-14/live/workspaces/20261014-161230-dbf24f1e/workspace pattern: LIVE_FACT_ALPHA_A8FB820D308A [y] once [n] reject [Esc] cancel

```
