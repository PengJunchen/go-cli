# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6.1-sol`
- Magpie session: `go-cli-live-20261014-539983dffcb6`
- Run nonce: `539983dffcb6`
- Race detector clean: `True`
- Evidence class: real go-cli binary + real PTY + requests transparently forwarded to signed-in Magpie Codex. No model responses are synthesized.
- UI evidence: actual PTY bytes rendered by xterm.js in headless Chromium; screenshots are not native Terminal captures.
- Credential handling: isolated CLI HOME, dummy local API sentinel, no stored Authorization headers, request bodies, or full response bodies.

## Checks

- PASS: actual model response contains requested exact text marker
- FAIL: interactive CLI exited successfully
- PASS: Go race detector reported no DATA RACE in the real PTY session
- PASS: live PTY produced actual Chat Completions inference requests
- FAIL: inspect-fixtures: every real inference completed HTTP 200 with its own response ID
- PASS: inspect-fixtures: complete response JSON metadata was parsed without changing provider bytes
- FAIL: inspect-fixtures: every upstream response identifies signed-in Codex and served model
- FAIL: inspect-fixtures: each inference completed with a finish reason and streamed responses have a terminal marker
- FAIL: inspect-fixtures: each request joins to this session's completed route and nonzero usage
- PASS: inspect-fixtures: per-request upstream and Magpie token usage reconciles or is explicitly undefined
- PASS: plain-live-text: every real inference completed HTTP 200 with its own response ID
- PASS: plain-live-text: complete response JSON metadata was parsed without changing provider bytes
- PASS: plain-live-text: every upstream response identifies signed-in Codex and served model
- PASS: plain-live-text: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: plain-live-text: each request joins to this session's completed route and nonzero usage
- PASS: plain-live-text: per-request upstream and Magpie token usage reconciles or is explicitly undefined

## Screenshots

- [`01-connected.png`](01-connected.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`02-live-text-answer.png`](02-live-text-answer.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`inspect-fixtures-cli-exit.png`](inspect-fixtures-cli-exit.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.

## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1243, in run_live
    report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
                           ~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 861, in wait_turn
    raise RuntimeError(f"interactive process exited during {label} with status {process_status}; screen={screen[-1200:]}")
RuntimeError: interactive process exited during inspect-fixtures with status 1; screen=r: (1) list the current workspace with ls, (2) search with grep for the single literal pattern LIVE_FACT_ALPHA_539983DFFC B6 under '.', (3) read scan-a.txt, (4) read nested/scan-b.txt. Do not use Bash, combine patterns, or write files. After all four tool results, report exactly LIVE_FACT_A LPHA_539983DFFCB6=blue and LIVE_FACT_BETA_539983DFFCB6=green. Session: s_61671dcaaf4893f300e43e8d45432c51 | Branch: main | Loaded rows: 2 | loaded projection only [user] This is a live provider text check. Reply with exactly LIVE_TEXT_OK_539983DFFCB6. Also remember this early fact for a later question after context compaction: LIVE_MEMOR Y_539983DFFCB6=cobalt. Do not repeat the memory fact yet and do not use tools. [assistant] LIVE_TEXT_OK_539983DFFCB6 timeout: stream idle exceeded 10s: context deadline exceeded codex/gpt-6.1-sol | turn #2 | interact | chat Known tokens: 8996/0 | Cost: unknown Session ended. error: interactive: operation failed: operation failed: interactive: wait for canonical transcript apply acknowledgement: tui: transcript refresh identity does not match the active app Hint: Try again. If the problem persists, run `go-cli doctor` to diagnose common issues or contact support.

```

## Settled gateway audit

After the CLI exit, a follow-up loopback audit matched the interrupted second request by its exact response ID and route ID. Magpie finalized it as HTTP 499 (`the agent canceled the request`) with zero input/output tokens; this confirms cancellation, not completed inference. Account and credential fields were discarded. See [`magpie-final-audit-after-cli-exit.json`](magpie-final-audit-after-cli-exit.json).
