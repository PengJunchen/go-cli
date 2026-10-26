# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6.1-sol`
- Run mode: `live-artifact-focus`
- Magpie session: `go-cli-live-20261014-e372b2929334`
- Run nonce: `e372b2929334`
- Race detector clean: `True`
- Evidence class: real go-cli binary + real PTY + requests transparently forwarded to signed-in Magpie Codex. No model responses are synthesized.
- UI evidence: actual PTY bytes rendered by xterm.js in headless Chromium; screenshots are not native Terminal captures.
- Credential handling: isolated CLI HOME, dummy local API sentinel, no stored Authorization headers, request bodies, or full response bodies.

## Checks

- PASS: actual model response contains requested exact text marker
- PASS: nonce-bound external PPTX fixture exists and target artifact is absent before turn 1
- FAIL: interactive CLI exited successfully
- PASS: Go race detector reported no DATA RACE in the real PTY session
- PASS: live PTY produced actual Chat Completions inference requests
- PASS: plain-live-text: every real inference completed HTTP 200 with its own response ID
- PASS: plain-live-text: complete response JSON metadata was parsed without changing provider bytes
- PASS: plain-live-text: every upstream response identifies signed-in Codex and the requested served model
- PASS: plain-live-text: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: plain-live-text: each request joins to this session's completed route and nonzero usage
- PASS: plain-live-text: per-request upstream and Magpie token usage reconciles or is explicitly undefined

## Screenshots

- [`01-connected.png`](01-connected.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`02-live-memory-seed.png`](02-live-memory-seed.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`02-live-text-answer.png`](02-live-text-answer.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-cli-exit.png`](pptx-create-cli-exit.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.

## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Scope

This is a focused live artifact/compaction run. It intentionally skips the separately tested smoke, long-task, and Escape scenarios; it does not claim full-suite acceptance.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1855, in run_live
    report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
                       ~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1003, in wait_turn
    working = active_busy_footer(screen_lines)
        ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
RuntimeError: interactive process exited during pptx-create with status -9; screen=Interactive session started. Type 'exit' to quit. > This is a live provider text check. Reply with exactly LIVE_TEXT_OK_E372B2929334. Also remember this early fact for a later question after context compaction: LIVE_MEM ORY_E372B2929334=cobalt. Do not repeat the memory fact yet and do not use tools. Session: s_98ef954e18837c4f240bbbe1b158b99e | Branch: main | Loaded rows: 2 | loaded projection only [user] This is a live provider text check. Reply with exactly LIVE_TEXT_OK_E372B2929334. Also remember this early fact for a later question after context compaction: LIVE_MEMOR Y_E372B2929334=cobalt. Do not repeat the memory fact yet and do not use tools. [assistant] LIVE_TEXT_OK_E372B2929334 codex/gpt-6.1-sol | turn #1 | interact | chat Tokens: 8993/0 (0%) | Cost: unknown Sources: usage=reported/u0 cost=unknown/c1 price=unknown actual=openai/codex/gpt-6.1-sol >

```
