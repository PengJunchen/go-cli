# Independent review: live run 20261014-182412-40ef8373

## Result

This **full live run failed** at the first context-pressure prompt. It did reach the successful large-context file read and the real PPTX creation and separate-turn edit. The test did not submit a context-pressure prompt, so production compaction, post-compaction canary recall, Escape cancellation, and the new-physical-session recovery were not exercised. The PPTX acceptance subcase also fails because LibreOffice cannot load either frozen deck; see [the independent PPTX review](182412-pptx-independent-review.md).

At `context-pressure-1`, the harness timed out while writing a 30,000-byte prompt to the PTY after only 3,356 bytes. No pressure inference was sent. The CLI had already returned from the large-context read; the successful receipt proves the model requested the read, the 273,673-byte fixture was read, and its SHA-256 and both boundary markers match the durable receipt. This does not establish that a summary was produced or recalled.

## Run identity and provider joins

- Dot run/session: `20261014-182412-40ef8373` / `go-cli-live-20261014-e6a0daeaba5f`.
- Provider/model: signed-in Codex through Magpie, requested and upstream-served as `codex/gpt-6.1-sol`.
- Source revision: `49dbf9755d860b877edd45ed9a73161ab5ebbb8f`; source diff at build was empty.
- Race binary SHA-256: `4ac09868598015fccc50a16ef90e6c4284d91707240314ad3a0329dece355218`.
- Final run-report SHA-256: `234b2170c5d21769e0c54413f805a49b72edfbbf8244c9f6dd859ecace182b54`.
- PTY ANSI stream SHA-256: `df7a761d7ac488beadff688febff897c7aab3a0e8a8e0604a0ecf559f6ff12ee`.
- Fixture SHA-256: `0ce937cc231139de98adb4810d29dfd9c59d04bf7a8755be21937a1b552dbe90`.

I independently examined all 26 completed bridge inference rows. All 26 response IDs were unique. Each response ID matched its per-request Magpie route snapshot and usage row; route IDs matched exactly. Every route was done/status 200; every request identified the same requested and upstream-served Codex model, contained a finish reason and terminal stream marker, and had nonzero usage. Provider prompt/output counts reconciled exactly to Magpie uncached plus cached input/output for all 26. The aggregate routes endpoint records only the latest route, so the stronger exact joins are the per-request snapshots plus usage `response_id`/`route_id` rows.

## Scene results

| Scene | Independent result | Evidence and caveats |
| --- | --- | --- |
| Plain text and fixture inspection | PASS for completed scene | Real text answer, `ls`/literal `grep`/`read` calls, both exact fixture facts and their receipts, default unscoped grep’s zero scope/unknown effect, and approval screenshot are present in the run report and PTY evidence. |
| Denied write | PASS for observed no-effect behavior | Actual human denial; target absent before and after; following provider request carries the denied receipt. |
| Default Python policy rejection | PASS on raw evidence; report boolean is a false negative | Durable error/quiescent no-process proof, exact marker absent before and after, then an ordinary no-tool recovery turn. The assistant explained that `python3` was blacklisted and the marker was not written. The harness phrase detector still misses “is blacklisted.” |
| Allowed write and edit | PASS for observed effects | Nonce-bound file created then edited to the exact new text, with matching tool-call receipts and durable quiescent completion proofs. |
| 20-second Bash and queued follow-up | PASS on raw evidence; two report checks are false negatives | Busy footer shown while `.started` existed and `.finished` was absent; follow-up entered and submitted in that interval. The bridge’s later request contains uppercase `FOLLOWUP_E6A0DAEABA5F_QUEUE_ACK` and the Bash receipt, followed by a read receipt for the exact finish-file value. The runner compares against lowercase `nonce`, so its marker and receipt booleans are false. |
| PPTX creation and separate-turn edit | FAIL for package interoperability | Dot’s first turn created v1, and another real turn changed only the helper’s `releaseStatus` constant and wrote v2. OOXML content/text/shape preservation looked correct to the old verifier, but both files have a noncanonical main-presentation MIME and LibreOffice cannot load them. Visual review is not possible. |
| Large-context read | PASS for the read operation only | The model requested the 273,673-byte fixture. Its durable receipt is successful/quiescent, `inline_pipeline_joined`, and matches the fixture hash plus first/last markers. |
| Production compaction, retained canary and latest prompt | NOT RUN | The first pressure prompt never completed PTY submission; there is no compaction summary boundary or post-compaction provider recall request. |
| Escape cancellation and fresh physical session | NOT RUN | The run ended before cancellation and session-recovery steps. |

## Acceptance decision

The run demonstrates live provider attribution and completed tool effects, but it does not satisfy full scenario acceptance. Keep the PPTX MIME/render failure separate from the PTY write failure. A future run needs a loadable model-created PPTX and edit, a pressure-turn input path that submits successfully, production durable summary evidence that retains the early canary and covers the large-read receipt, a real post-summary provider request containing both canary and latest prompt, and the complete unknown-cancel/fence/new-session flow. Keep the two detected harness false negatives in the report and fix the nonce case comparison and explanation phrase set before counting their checks.
