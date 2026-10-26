# go-cli cleanup and integration — 2026-10-24

## Branch and merge result

The checkout is on `dev-2026-10-24`, created from `dev` at `38239888f6275f9a329825a9bde791b449280227`. The `dev` branch remains unchanged. The integrated source tip before this cleanup was `9e124a2fdf44fbe5df8d339460cf69df49b6801a`; this report and artifact-removal commit are on top of that tip. The final branch SHA is the commit containing this report and is recorded in `/tmp/cleanup-go-cli-result.json` for the parent summary. No push or history rewrite was performed.

Three genuine merges preserve the source history:

- `origin/main` was merged at `8c2b69a`; its useful unique change `a5d067e test(tui): wait for complete event stream before quitting` is retained.
- Dynamic capability runtime commits `cd0693b feat(runtime): add progressive capabilities and portable extensions` and `d813dba fix(runtime): unify dynamic reload and approval lifecycles` were integrated at `9613cb9`.
- Guangzhou evaluation commits `de6ea76 fix(runtime): resolve live evaluation regressions` and `f33ac65 docs(qa): fix preserved fixture checksum target` were integrated at `9e124a2`.

No `ours` merge was used. The TUI E2E rename conflict was resolved by carrying the complete-event-stream close and error assertions into the renamed full test. The approval API conflict was resolved around the runtime's UI-neutral `InteractiveRequest`/`InteractiveResponse` contract, with decision provenance kept explicit and TUI aliases preserved. The merged changes passed focused verification described below. All source commits are ancestors of the integrated branch.

## Preserved work and evidence

The runtime branch retains progressive capability discovery, portable extensions, hot reload lifecycle changes, and their design/review documents. The Guangzhou branch retains approval provenance, synchronous tool-search completion proof, adjacent Chinese tool-name matching, terminal renderer padding repair, live evaluation scripts, and its source/research commits.

Reusable inputs were moved under `tests/fixtures/live-eval/`: the 2026-10-15 prompt/oracle sets and checksum manifests remain intact; the 2026-10-14 Magpie fixture retains the original presentation brief, the exact 273,673-byte synthetic compaction input, and the final generated Go source used to create/edit the QA deck. Its manifest records SHA-256 values and the source run ID. The final generated deck and other run outputs were removed.

The research digest [`2026-10-24-live-qa-digest.md`](../research/2026-10-24-live-qa-digest.md) keeps the 10/14 real CLI results, failures and repairs, 10/15 A/B evaluation boundaries, measurement caveats, and next steps. Existing runtime iteration ledgers, long-task regression evidence summaries, code review notes, and detailed 10/22 live-evaluation findings remain. Retention notes mark raw evidence paths that no longer exist.

## Removed worktrees and generated data

After confirming all source heads and commits were ancestors of the merged branch, these three clean worktrees were removed and stale Git worktree records pruned:

- `/Users/yinyuepeng/.codex/worktrees/dynamic-capability-runtime/go-cli` (`d813dba3dd3c9ac4aed8acab6344a27a2700e626`)
- `/Users/yinyuepeng/.codex/worktrees/guangzhou-develop-eval/go-cli` (`f33ac653036a455ee2315ed6a48c6847e1d4ef66`)
- `/Users/yinyuepeng/.codex/worktrees/guangzhou-main-baseline/go-cli` (detached `8e65b284ad55299b80f46202d903384a71a85d3c`)

The main checkout's raw QA evidence removal staged 1,113 tracked files (219,494,938 bytes, about 209 MiB): PTY transcripts, per-run reports, screenshots, render/PPT outputs, gate logs and JSONL traces from `docs/qa`, `docs/iterations/2026-09-25-runtime/evidence`, `docs/iterations/2026-10-22-live-e2e`, and `docs/audits/2026-09-24/evidence`. Summaries, source scripts, formal fixtures, issue ledgers, and review narratives were retained.

Removed the 29 isolated `docs/qa/2026-10-14/live/workspaces` runs and their generated race binaries, Python environments, traces, sessions, audit/history state, and LibreOffice profiles. The workspaces occupied about 1.93 GiB. Also removed the root build output (`bin`, about 24 MiB), `scripts/e2e_terminal/node_modules` (about 21 MiB), Python `__pycache__` directories, ignored QA gate logs, and `.DS_Store` files outside local CLI state.

The recovered working-tree space is approximately 2.7 GiB across the isolated QA runs, duplicate worktree checkouts, raw QA evidence, build/dependency output, and caches. Git history and old branches were preserved, so deleted tracked blobs remain reachable in history. The global Go cache was not cleared. A schema-only inspection of the repository-root `.go-cli` found one 42-line operational log and eight trace JSONL files containing 19 OpenTelemetry span records (`trace_id`, `span_id`, parent/span kind, name, timestamps, status and attributes). These were exporter telemetry, not session-tree records; no session tree, draft, or configuration file was present. The exact log and trace files were removed, and their empty directories were removed. No useful feature finding was encoded in those telemetry records; QA conclusions are retained in the research digest and iteration reports.

The cleanup also removed the complete ignored parent directories `cmd/cli/.go-cli` and `internal/cli/.go-cli`. The complete pre-removal enumeration contained exactly five log/audit JSONL files and two zero-byte audit lock files, and no session/draft files. The untracked log/audit files cannot be recovered from Git; their JSONL bodies were not read. The report therefore records that the parent directories were removed and the inventory contained only those seven files, but does not claim the JSONL records contained no user detail. The exact relative paths and byte sizes are included in `/tmp/cleanup-go-cli-result.json`. Equivalent operational state inside isolated QA workspaces was removed with those runs.

## Verification

- `make build`: passed; generated `bin/go-cli` was removed afterward.
- `gofmt -s -w .` and `go vet ./...`: passed as the first steps of `make verify`. `goimports` was unavailable and the Makefile skipped it. `make verify` then stopped at `lint` because `golangci-lint` is not installed; no linter result is claimed.
- `make test`: 9,052 passed, 6 skipped, 1 failed across 32 packages. The single failure was `internal/acp.TestStdioConcurrentFramesAreSerialized`. A focused race rerun of that test passed; `internal/acp` has no changes between the original `dev` base and the integrated source tip, so this is recorded as an unresolved one-off concurrency/test-load failure, not a full-suite pass.
- `make scan`: passed for 466 files with 0 errors and 1,868 warnings.
- `make test-log`: passed, 10 test functions. `make test-leak`: passed, 6 test functions.
- `go test -race -tags=e2e ./tests/e2e_20260802 -run '^TestE2EInteractive_FullPipeline$' -count=1`: passed during merge-conflict verification. The source worktree validation also passed the Python live-evaluation tests (20/20), renderer Node syntax check, selected approval/tools/TUI race tests, and `git diff --check`.

Remaining verification gaps: install `golangci-lint` and `goimports` in the development environment, then rerun `make verify`; investigate the single ACP concurrency failure if it recurs under normal load. No unresolved merge conflict or unmerged source worktree remains.
