# Independent review: first live PTY smoke

Run `20261014-150122-7ef3b912` is a **failed smoke**, not a passing end-to-end result. The runner reached a genuine model response and an approval prompt, then the next turn terminated on a go-cli run-owner/quiescence error. The harness did not persist its in-memory request ledger after the exception, so this run cannot establish per-turn Magpie IDs, served model, or usage independently.

## Provenance and inspected evidence

- Run report: [`run-report.json`](../terminal/20261014-150122-7ef3b912/run-report.json)
- Evidence summary: [`evidence.md`](../terminal/20261014-150122-7ef3b912/evidence.md)
- Live text screen: [`02-live-text-answer.png`](../terminal/20261014-150122-7ef3b912/02-live-text-answer.png), SHA-256 `84ec05fe30286c2258a3e8881bd1f2b6e89917c55e1a939d193f4259f9c96564`
- Approval screen: [`inspect-fixtures-1-approval-allow-page-1.png`](../terminal/20261014-150122-7ef3b912/inspect-fixtures-1-approval-allow-page-1.png), SHA-256 `81286bba5c727e7b573e392df9ed9c20e7a15ee8d209a478760b086e411dd58e`
- Fixture: [`presentation-fixture.json`](../terminal/20261014-150122-7ef3b912/presentation-fixture.json), SHA-256 `82fe3e7de921a3591f4b26fd18efd7be75c4096fbd12c33e41bc9f5c1ab34f38`
- Source revision: `e0493e5fe92f98ee460a0e25740a97852272ddf2`
- Race-build path recorded by the run: `docs/qa/2026-10-14/live/workspaces/20261014-150122-7ef3b912/bin/go-cli-race`
- Race-build SHA-256 recorded by the run: `ac6fbf5d546d1776ffb1acd7aab23278248a3cf718c88673c0968c5b3077b864`

## Findings

The xterm.js screenshots are readable and show the actual interactive CLI. The text-answer screenshot displays `LIVE_TEXT_OK_247ABD3D8F47`, and the approval screenshot shows a live `grep` approval for the two nonce-bound fixture facts. This is useful evidence that the runner launched the real CLI and presented an approval surface.

The saved report's failure traceback says `turn inspect-fixtures did not become idle in 300.0s`; the visible terminal failure is `tool completion did not prove local resource quiescence` and `owner publication did not match the joined durable run (quiescent=true outcome_known=false ...)`. The run ended without a retained bridge ledger, provider audit rows, a completed fixture-read result, or a PPTX. Do not count the approval card alone as proof that the tool completed or had a filesystem effect.

| Acceptance row | Review status | Basis |
| --- | --- | --- |
| LIVE-01 real binary provenance | Inconclusive | Report has a source revision and race-binary hash, but the exception path omitted per-run launch/config details. |
| LIVE-02 completed Magpie inference per turn | Inconclusive | The visible text marker supports a response; no saved response ID, route row, served model, or usage row survives in the report. |
| LIVE-03 tool dispatch and completion | Inconclusive | The genuine approval UI is visible, but the tool receipt/execution record was not persisted and the CLI failed during owner/quiescence handling. |
| LIVE-04 / LIVE-05 file effects | Not run | No allowed-write or denied-write case completed in this smoke. |
| LIVE-06 natural context compaction | Not run | No compaction or recall case was reached. |
| LIVE-07 / LIVE-08 PPT creation and second-turn edit | Not run | No presentation was produced. |
| LIVE-09 screenshot provenance | Pass for these captures | The harness source renders the bytes read from the real PTY through xterm.js; both inspected images match that screen state. |

Before rerunning, preserve the bridge snapshot, Magpie route/usage audit, turn records, and tool receipts on the `run_live()` exception path as well as the success path. The production quiescence failure must also be resolved or separately classified before the remaining cases can pass.
