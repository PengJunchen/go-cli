# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6.1-sol`
- Magpie session: `go-cli-live-20261014-c601f66f879f`
- Run nonce: `c601f66f879f`
- Race detector clean: `True`
- Evidence class: real go-cli binary + real PTY + requests transparently forwarded to signed-in Magpie Codex. No model responses are synthesized.
- UI evidence: actual PTY bytes rendered by xterm.js in headless Chromium; screenshots are not native Terminal captures.
- Credential handling: isolated CLI HOME, dummy local API sentinel, no stored Authorization headers, request bodies, or full response bodies.

## Checks

- PASS: actual model response contains requested exact text marker
- PASS: live PTY produced actual Chat Completions inference requests
- PASS: inspect-fixtures: every real inference completed HTTP 200 with its own response ID
- PASS: inspect-fixtures: every upstream response identifies signed-in Codex and served model
- PASS: inspect-fixtures: each inference stream has a finish reason and terminal marker
- PASS: inspect-fixtures: each request joins to this session's completed route and nonzero usage
- PASS: plain-live-text: every real inference completed HTTP 200 with its own response ID
- PASS: plain-live-text: every upstream response identifies signed-in Codex and served model
- PASS: plain-live-text: each inference stream has a finish reason and terminal marker
- PASS: plain-live-text: each request joins to this session's completed route and nonzero usage

## Screenshots

- [`01-connected.png`](01-connected.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`02-live-text-answer.png`](02-live-text-answer.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`inspect-fixtures-1-approval-allow-page-1.png`](inspect-fixtures-1-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`inspect-fixtures-cli-exit.png`](inspect-fixtures-cli-exit.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.

## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1082, in run_live
    report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
                           ~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 792, in wait_turn
    raise RuntimeError(f"interactive process exited during {label} with status {process_status}; screen={screen[-1200:]}")
RuntimeError: interactive process exited during inspect-fixtures with status 1; screen=so remember this early fact for a later question after context compaction: LIVE_MEMO [receipt: success] RY_C601F66F879F=cobalt. Do not repeat the memory fact yet and do not use tools. [tool call_e951131fc30b4b2e9e3058be80b626ac] [assistant] [source output not truncated] LIVE_TEXT_OK_C601F66F879F [receipt: success] [assistant · live] I’ll list the workspace, search for both markers, and read the matching files. [assistant · live] core: tool completion did not prove local resource quiescence: core: tool completion did not prove local resource quiescence codex/gpt-6.1-sol | turn #2 | interact | chat Tokens: 27223/0 (0%) | Cost: unknown Session ended. error: interactive: operation failed: operation failed: interactive: owner publication did not match the joined durable run (quiescent=true outcome_known=false run_match es=true session_matches=true owner_generation_present=true) Hint: Try again. If the problem persists, run `go-cli doctor` to diagnose common issues or contact support.

```
