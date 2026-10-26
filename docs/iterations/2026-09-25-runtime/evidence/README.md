# Runtime iteration evidence retention

On 2026-10-24, raw command logs, race-test JSONL event streams, test-list output, temporary stderr/raw captures, and other raw run artifacts were removed. On 2026-10-26, only the historical review set was restored byte-for-byte from `origin/main@8e65b284ad55299b80f46202d903384a71a85d3c`: 12 terminal screenshots, the terminal `run-report.json`, seven structured scanner/integration JSON reports, and four probe source excerpts. The exact paths, source Git blob IDs, byte sizes, and reasons are recorded in [the restoration manifest](restoration-2026-10-26-manifest.json).

The restored screenshots, structured evidence, and probe source are retained review materials for their original iterations; they are not fresh verification. Raw command logs, PTY/ANSI transcripts, JSONL event streams, temporary fixtures, stderr captures, and unlisted scanner outputs remain deleted. The iteration's tested snapshots, pass/fail counts, root causes, corrections, remaining work, and lane handoffs remain in `../execution-status.md`, `../verify-execution.md`, `../verify-implementation-notes.md`, and the lane/review Markdown files.

The 2026-10-24 branch cleanup report records the fresh verification attempted against `dev-2026-10-24`.
