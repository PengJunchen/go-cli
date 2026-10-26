# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6.1-sol`
- Run mode: `live-artifact-focus`
- Magpie session: `go-cli-live-20261014-96aeb82fcb5e`
- Run nonce: `96aeb82fcb5e`
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
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1893, in run_live
    report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout, capture_progress="13-pptx-generation-working"))
                           ~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1010, in wait_turn
    result = drive_approval(session, evidence, f"{label}-{len(approvals_for_turn)+1}", "allow", screenshots, timeout=min(300, timeout), screen=screen_lines)
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 989, in drive_approval
    raise AssertionError(f"approval was not unlocked after reviewing all detail pages: {normalize(screen)[-1000:]}")
AssertionError: approval was not unlocked after reviewing all detail pages:  and exact required content. [approval] write local interaction #1 scope: unknown; once only; reuse unavailable view truncated · details 82-101/101 · ↑/↓ PgUp/PgDn + paragraph.font.bold = bold + paragraph.font.color.rgb = RGBColor(*color) + return shape + + +prs = Presentation() +prs.slide_width = Inches(13.333) +prs.slide_height = Inches(7.5) +for title, lines in SLIDES: + slide = prs.slides.add_slide(prs.slide_layouts[6]) + slide.background.fill.solid() + slide.background.fill.fore_color.rgb = RGBColor(246, 248, 251) + text_box(slide, title, 0.7, 0.65, 12, 1.1, 32, (24, 41, 65), True) + for index, line in enumerate(lines): + text_box(slide, line, 0.7, 2.25 + indeouts[6]) + slide.background.fill.solid() + slide.background.fill.fore_color.rgb = RGBColor(246, 248, 251) + text_box(slide, title, 0.7, 0.65, 12, 1.1, 32, (24, 41, 65), True) + for index, line in enumerate(lines): + text_box(slide, line, 0.7, 2.25 + index * 1.15, 12, 1, 28, (24, 41, 65)) [y] review details [n] reject [d] diff

```
