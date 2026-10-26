# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6.1-sol`
- Run mode: `live-artifact-focus`
- Magpie session: `go-cli-live-20261014-9599a58520d6`
- Run nonce: `9599a58520d6`
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
- PASS: pptx-create: every real inference completed HTTP 200 with its own response ID
- PASS: pptx-create: complete response JSON metadata was parsed without changing provider bytes
- PASS: pptx-create: every upstream response identifies signed-in Codex and the requested served model
- PASS: pptx-create: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: pptx-create: each request joins to this session's completed route and nonzero usage
- PASS: pptx-create: per-request upstream and Magpie token usage reconciles or is explicitly undefined

## Screenshots

- [`01-connected.png`](01-connected.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`02-live-memory-seed.png`](02-live-memory-seed.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`13-pptx-generation-working.png`](13-pptx-generation-working.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-1.png`](pptx-create-1-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-2.png`](pptx-create-1-approval-allow-page-2.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-3.png`](pptx-create-1-approval-allow-page-3.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-4.png`](pptx-create-1-approval-allow-page-4.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-5.png`](pptx-create-1-approval-allow-page-5.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-6.png`](pptx-create-1-approval-allow-page-6.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-7.png`](pptx-create-1-approval-allow-page-7.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-2-approval-allow-page-1.png`](pptx-create-2-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-cli-exit.png`](pptx-create-cli-exit.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.

## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Scope

Selected scenes:

- real provider memory seed
- model-generated PPTX create and separate-turn edit
- rendered OOXML review
- production context compaction and recall

Skipped scenes:

- smoke and approvals
- long Bash with queued follow-up
- Escape cancellation and recovery

Full-suite acceptance is not claimed for this focused run.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1917, in run_live
    report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout, capture_progress="13-pptx-generation-working"))
                           ~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1064, in wait_turn
    raise RuntimeError(f"interactive process exited during {label} with status {process_status}; screen={screen[-1200:]}")
RuntimeError: interactive process exited during pptx-create with status 1; screen=so remember this early fact for a later question after context compaction: LIVE_MEMO [tool call_71cecf4fffdf452eb2d1fd1729c9ab69] RY_9599A58520D6=cobalt. Do not repeat the memory fact yet and do not use tools. [receipt: error] [assistant] LIVE_TEXT_OK_9599A58520D6 [assistant · live] I’ll write and run the Python script, then reopen the saved deck to verify its four slides, editable text, and exact required content. [assistant · live] core: tool completion did not prove local resource quiescence: core: tool completion did not prove local resource quiescence codex/gpt-6.1-sol | turn #2 | interact | chat Tokens: 29486/0 (0%) | Cost: unknown Session ended. error: interactive: operation failed: operation failed: interactive: owner publication did not match the joined durable run (quiescent=true outcome_known=false run_match es=true session_matches=true owner_generation_present=true) Hint: Try again. If the problem persists, run `go-cli doctor` to diagnose common issues or contact support.

```
