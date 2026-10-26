# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6.1-sol`
- Run mode: `live-artifact-focus`
- Magpie session: `go-cli-live-20261014-3a61dca80b0c`
- Run nonce: `3a61dca80b0c`
- Race detector clean: `True`
- Evidence class: real go-cli binary + real PTY + requests transparently forwarded to signed-in Magpie Codex. No model responses are synthesized.
- UI evidence: actual PTY bytes rendered by xterm.js in headless Chromium; screenshots are not native Terminal captures.
- Credential handling: isolated CLI HOME, dummy local API sentinel, no stored Authorization headers, request bodies, or full response bodies.

## Checks

- PASS: actual model response contains requested exact text marker
- PASS: nonce-bound external PPTX fixture exists and target artifact is absent before turn 1
- PASS: turn 1 Write and Bash calls each have a later receipt and durable quiescent completion proof
- PASS: turn 1 tool-generated PPTX exists at the fixture path with immutable pre-edit snapshot
- PASS: turn 1 LLM-issued Write targets the helper that references the exact PPTX and source snapshot is frozen
- PASS: turn 1 LLM-issued Bash runs the same helper with the exact PPTX path
- PASS: turn 1 LLM-issued tool arguments reference the target PPTX output path
- PASS: turn 2 Edit and Bash calls each have a later receipt and durable quiescent completion proof
- PASS: turn 2 Edit targets the same helper and its arguments identify the existing PPTX
- PASS: turn 2 Bash executes that same helper with the existing PPTX path
- PASS: the real helper source changed between separately submitted create and edit turns
- PASS: PPTX bytes changed after the separately submitted turn 2
- PASS: turn 2 response includes the revised status marker
- PASS: frozen after-deck evidence is a byte-identical copy of Dot's workspace artifact
- PASS: independent OOXML verifier confirms both deck versions and the one-field edit
- PASS: interactive CLI exited successfully
- PASS: Go race detector reported no DATA RACE in the real PTY session
- PASS: live PTY produced actual Chat Completions inference requests
- PASS: large-context-read: every real inference completed HTTP 200 with its own response ID
- PASS: large-context-read: complete response JSON metadata was parsed without changing provider bytes
- PASS: large-context-read: every upstream response identifies signed-in Codex and the requested served model
- PASS: large-context-read: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: large-context-read: each request joins to this session's completed route and nonzero usage
- PASS: large-context-read: per-request upstream and Magpie token usage reconciles or is explicitly undefined
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
- PASS: pptx-edit-existing: every real inference completed HTTP 200 with its own response ID
- PASS: pptx-edit-existing: complete response JSON metadata was parsed without changing provider bytes
- PASS: pptx-edit-existing: every upstream response identifies signed-in Codex and the requested served model
- PASS: pptx-edit-existing: each inference completed with a finish reason and streamed responses have a terminal marker
- PASS: pptx-edit-existing: each request joins to this session's completed route and nonzero usage
- PASS: pptx-edit-existing: per-request upstream and Magpie token usage reconciles or is explicitly undefined

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
- [`pptx-create-1-approval-allow-page-8.png`](pptx-create-1-approval-allow-page-8.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-9.png`](pptx-create-1-approval-allow-page-9.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-10.png`](pptx-create-1-approval-allow-page-10.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-11.png`](pptx-create-1-approval-allow-page-11.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-12.png`](pptx-create-1-approval-allow-page-12.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-13.png`](pptx-create-1-approval-allow-page-13.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-1-approval-allow-page-14.png`](pptx-create-1-approval-allow-page-14.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-create-2-approval-allow-page-1.png`](pptx-create-2-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`14-pptx-created.png`](14-pptx-created.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`15-pptx-edit-working.png`](15-pptx-edit-working.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-1-approval-allow-page-1.png`](pptx-edit-existing-1-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-2-approval-allow-page-1.png`](pptx-edit-existing-2-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-2-approval-allow-page-2.png`](pptx-edit-existing-2-approval-allow-page-2.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-2-approval-allow-page-3.png`](pptx-edit-existing-2-approval-allow-page-3.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-2-approval-allow-page-4.png`](pptx-edit-existing-2-approval-allow-page-4.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-2-approval-allow-page-5.png`](pptx-edit-existing-2-approval-allow-page-5.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-2-approval-allow-page-6.png`](pptx-edit-existing-2-approval-allow-page-6.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-2-approval-allow-page-7.png`](pptx-edit-existing-2-approval-allow-page-7.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-2-approval-allow-page-8.png`](pptx-edit-existing-2-approval-allow-page-8.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-3-approval-allow-page-1.png`](pptx-edit-existing-3-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-4-approval-allow-page-1.png`](pptx-edit-existing-4-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-5-approval-allow-page-1.png`](pptx-edit-existing-5-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`pptx-edit-existing-6-approval-allow-page-1.png`](pptx-edit-existing-6-approval-allow-page-1.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`16-pptx-edited.png`](16-pptx-edited.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`17-large-context-working.png`](17-large-context-working.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.
- [`large-context-read-terminal-error.png`](large-context-read-terminal-error.png) — 1440×900 px, xterm 169×50; headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY.

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
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 2124, in run_live
    report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout, capture_progress="17-large-context-working"))
                           ~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1115, in wait_turn
    raise RuntimeError(
    ...<2 lines>...
    )
RuntimeError: CLI persisted run_terminal outcome=error checkpoint=terminal-c3c3596587f5434d during large-context-read; screen= only - not product measurements Edited [live_review_3A61DCA80B0C.pptx](/Users/yinyuepeng/projects/go-cli/docs/qa/202 6-10-14/live/workspaces/20261014-174155-f895248a/workspace/live_review_3A61DCA80B0C. [receipt: success] pptx) in place. [tool: bash] Error: bash: exit status 1 Slide 4 now reads **`Release status: REVISED`**, replacing `Release status: INITIAL` [receipt: error] . [tool: bash] Error: bash: exit status 1 Verified the saved ZIP still contains four slides. All exact titles, nonce text, thr [receipt: error] ee synthetic metric lines, and disclosure remain. Comparison with the original confi [tool: bash] rmed that only the slide-4 status text changed; all other package entry payloads and Verified: helper changed only requested constant; archive changed only INITIAL to RE slide shapes were preserved. The helper changed only at the requested `releaseStatu VISED in slide 4; all other package entry payloads and slide shapes preserved. s` line. [assistant · live] [receipt: success] I’ll read `large-context.txt` and check its first row, last row, and repeated-row co [tool call_b67be8b97c554438a4c10ae3c2a9de5e] unt. read ⠋ working... codex/gpt-6.1-sol | turn #4 | interact | chat

```
