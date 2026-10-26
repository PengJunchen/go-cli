# 2026-10-14 Magpie live QA archive

The live CLI run, its failures, repair sequence, measurement limits, and follow-up work are summarized in [`docs/research/2026-10-24-live-qa-digest.md`](../../../research/2026-10-24-live-qa-digest.md). The final run recorded 194/194 checks, 20 user turns, 36 provider requests, and 55 screenshots. The digest distinguishes those historical results from later source changes and preserves failed attempts instead of rewriting them as passes.

On 2026-10-24, raw session transcripts, PTY bytes, screenshots, run-report JSON, generated PPT/PDF files, raw gate logs, sanitized event JSONL, render/profile outputs, and isolated live workspaces were removed after their conclusions were extracted. This folder retains the QA plan, acceptance and issue ledgers, review narratives, installation notes, and failure diagnoses. The final task brief and reusable generated Go input are in [`tests/fixtures/live-eval/magpie-2026-10-14`](../../../../tests/fixtures/live-eval/magpie-2026-10-14/README.md).

- [`rounds.md`](rounds.md) keeps the 29-run status/count index without links to deleted run files.
- [`root-review.md`](root-review.md), [`review/`](review/), and [`magpie/diagnosis/`](magpie/diagnosis/) retain source, product, and harness findings.
- [`issues.json`](issues.json) keeps the issue ledger; [`acceptance.json`](acceptance.json) keeps the acceptance criteria and summary.

The raw evidence is no longer present for direct re-audit. See the preserved reports for their stated scope and limits.
