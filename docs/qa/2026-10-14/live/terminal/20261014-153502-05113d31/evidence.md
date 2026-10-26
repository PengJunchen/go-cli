# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6-luna`
- Magpie session: `go-cli-live-20261014-de91948f10d6`
- Run nonce: `de91948f10d6`
- Race detector clean: `True`
- Evidence class: real go-cli binary + real PTY + requests transparently forwarded to signed-in Magpie Codex. No model responses are synthesized.
- UI evidence: actual PTY bytes rendered by xterm.js in headless Chromium; screenshots are not native Terminal captures.
- Credential handling: isolated CLI HOME, dummy local API sentinel, no stored Authorization headers, request bodies, or full response bodies.

## Checks

- PASS: actual model response contains requested exact text marker
- FAIL: actual upstream assistant response includes exact alpha fixture fact
- FAIL: actual upstream assistant response includes exact beta fixture fact
- PASS: actual inference selected ls, literal grep, and read tools
- FAIL: both real read tool results were delivered back to the model
- PASS: read calls target both nonce fixture files
- PASS: interactive CLI exited successfully
- PASS: Go race detector reported no DATA RACE in the real PTY session
- PASS: live PTY produced actual Chat Completions inference requests
- PASS: deny-write: every real inference completed HTTP 200 with its own response ID
- PASS: deny-write: complete response JSON metadata was parsed without changing provider bytes
- PASS: deny-write: every upstream response identifies signed-in Codex and the requested served model
- PASS: deny-write: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: deny-write: each request joins to this session's completed route and nonzero usage
- PASS: deny-write: per-request upstream and Magpie token usage reconciles or is explicitly undefined
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
- [`02-live-tools-answer.png`](02-live-tools-answer.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.

## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1337, in run_live
    denial = drive_approval(session, evidence, "03-deny", "deny", report["screenshots"], timeout)
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 791, in drive_approval
    screen = session.wait_for("[approval]", timeout)
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 642, in wait_for
    raise TimeoutError(f"timed out waiting for {needle!r}; last screen: {last[-1000:]}")
TimeoutError: timed out waiting for '[approval]'; last screen:  search with grep for the single literal pattern LIVE_FACT_ALPHA_DE91948F10D review fixture 6 under '.', (3) read scan-a.txt, (4) read nested/scan-b.txt. Do not use Bash, combi LIVE_FACT_ALPHA_DE91948F10D6=blue ne patterns, or write files. After all four tool results, report exactly LIVE_FACT_A LPHA_DE91948F10D6=blue and LIVE_FACT_BETA_DE91948F10D6=green. [receipt: success] [assistant] I’ll run the four requested workspace operations in order. [assistant] [assistant] [user] Use the write tool to create blocked_de91948f10d6.txt containing exactly BLOCKED_de9 1948f10d6. This is a deliberate approval test; request the tool action normally. loopdetection: detected repeated response (count=4) codex/gpt-6-luna | turn #3 | interact | chat Tokens: 51265/0 (0%) | Cost: unknown Sources: usage=reported/u0 cost=unknown/c6 price=unknown actual=openai/codex/gpt-6-luna Error: operation failed Hint: Try again. If the problem persists, run `go-cli doctor` to diagnose common issues or contact support. >

```
