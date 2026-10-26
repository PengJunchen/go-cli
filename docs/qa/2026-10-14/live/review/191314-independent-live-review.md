# Independent live-run review: 20261014-191314-3ac94c9e

**Result: PASS for the requested live case.** The runner reports 194/194 checks passed, CLI exit 0 and a clean race build. I independently joined all provider rows, opened the durable session records, checked the compaction/read boundary and cancellation recovery, and verified the generated PPTX subcase. This is evidence for this run and environment only.

- Source revision: `ec267df999405a03a09ed5485256aee6cc133aae`.
- Race binary SHA-256: `09650e64b03c1341f516265e7d1f086945247a594b2e8259ec0acfbf71ec662c`.
- Frozen run report SHA-256: `36d453483795e6fdd0dcaea03eb4265a139c51b1a2a6a902d7518967c326177e`.
- The report records 194 checks, all `passed: true`, CLI exit 0, race detector clean, and 54 terminal/UI screenshots. Screenshots are headless Chromium renderings of real PTY bytes, not native Terminal captures.

## Provider and tool lineage

There are 36 completed upstream responses in the run's provider ledger: 35 streamed interactive turns and one internal nonstream summary completion. All have unique response IDs, route IDs and local bridge capture sequences; all completed HTTP 200. Each provider row joins to the run's Magpie session and a session-scoped route snapshot. The response ID, route ID and session match the corresponding usage row. All streamed replies have a terminal `[DONE]` and finish reason; the nonstream summary reply is parsed as complete JSON and has finish reason `stop`. Usage reconciles per request: provider prompt tokens equal Magpie uncached plus cached input, and provider output tokens equal Magpie output. The usage audit is complete (36 of 36 rows). The upstream did not expose a separate request ID, so this review relies on response/route/session identities and does not claim request-ID correlation.

The session requested `codex/gpt-6.1-sol`; upstream rows identify provider `codex` and the selected model, and Magpie route/usage records agree. Tool call IDs correlate to Dot dispatches and later receipts. The real inspection approval shows Grep's `scope: unknown` and the exact fixture marker before approval. A write-deny approval was explicitly rejected and `blocked_e08ccc4bb0e4.txt` is absent. The approved write/edit path produced `approved_e08ccc4bb0e4.txt` containing `EDITED_e08ccc4bb0e4`. The default policy-rejection turn attempted the exact Python-backed Bash command, received a durable policy-block error with `inline_pipeline_joined` proof, left its target absent before and after, and then completed a separate real text response.

The long Bash request was approved and visibly ran with the `working...` footer. The report records a 20-second requested sleep; a follow-up was submitted while `.finished` was still absent. Both start and finish markers were eventually written, the tool receipt was successful/quiescent, and a later real request read the finished marker.

## PPTX subcase

The deck was created and edited in two separate user turns. Turn 1 has completed model responses, Write/Bash calls and successful later receipts; it saved v1 before turn 2. In turn 2, a distinct Edit/Bash sequence changed the same model-authored helper's status constant and produced v2. Helper snapshots are byte-identical to the corresponding workspace file at capture time and differ only from `INITIAL` to `REVISED`. The output file's SHA-256 matches the frozen v2.

Both versions are canonical OOXML packages with four editable slides, 13 text shapes and no pictures. The only changed package part is `ppt/slides/slide4.xml`; all exact fixture text remains. I independently rendered all four v2 slides at 1601×900 and visually inspected each; text is readable, there is no clipping or overlap, and the synthetic-data disclosure appears on every slide. Independent render hashes match the harness renders. Details and hashes are in [the independent PPTX review](191314-pptx-independent-review.md) and [the independent verifier report](191314-independent/pptx-review.json).

## Context compaction and recall

The ordinary real turns and a successful 273,673-byte `large-context.txt` read led to production compaction. The durable summary row is `runcommit_6009d50f59b0b58ed7f046c9395ae5009cf34a9922e570d74e0e41d8b2a97ef2`, sequence 120. Its `compacted_through_entry_id` points to the successful read receipt `runcommit_7a18643b5a662e766493055049901091a56c955ee27ba3a1b8da55ea613b4d6`, sequence 106. The source and receipt match byte-for-byte at 273,673 bytes (SHA-256 `3dde7ddc5dcd3345b4de0cea2f59af7aea13a59fd340f2d7acaabb422e34861b`). The persisted summary retains both boundary markers and the early canary `LIVE_MEMORY_E08CCC4BB0E4=cobalt`.

Production logs show the summary strategy reduced 61 items to 10 at 108,034 estimated tokens against a 102,400-token maximum, producing a 33,375-token compacted context. The later provider request contains the latest prompt and retained canary; the actual response recalls the exact canary value. The full-size boundary and recall screenshots are in the run evidence. This was ordinary turn growth and production compaction, not manual transcript trimming.

## Cancellation and fresh-session recovery

The approved Escape test created its `.started` marker; `.finished` remained absent. The same durable tool call ended with `receipt_outcome: unknown`, quiescent resource state, and `owned_process_group_joined` completion evidence. The TUI showed the interrupted notice and explicit unknown-outcome warning. An ordinary prompt in the old session was fenced: it caused no provider request and added no durable user row.

`/session new` returned a different physical session ID and its JSONL file was present after acknowledgement. A fresh prompt then completed a real provider response and durable success in that new physical session with no tool calls or workspace mutation. The old session's unknown terminal record remained unchanged after both the new-session action and fresh turn. The new session file initially had no rows, so the successful first turn and terminal evidence—not file creation alone—establish recovery.

The run used default configuration without `sandbox.allowed_paths`; Grep showed unknown scope. The evidence establishes application-level approval and tool outcomes, not an OS filesystem/process sandbox. Global configuration was not changed.

The machine-readable cross-check is [the independent evidence ledger](191314-independent/live-evidence-review.json). It independently reports PASS after validating the complete response/route/session/usage joins, typed summary/source boundary, recall, and recovery fields.
