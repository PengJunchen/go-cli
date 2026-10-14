# Standards review

**Comparison:** base `e77e8058c51e5370a2ecc3eacb5fce0419829bcf`; upstream `1cd1951`, `4d1497b`; integrated HEAD `6aa6f9318f3d3c40822c368e489c9284cd932072`. Commit 6aa changes only test files; production source matches the UI capture at `89df724c491083a81239acc04bd42269c58ea6e0`.

**Sources:** repository `AGENTS.md`/`CLAUDE.md`; `go-cli-infra/AGENTS.md`, `CLAUDE.md`, `rules/go-validation-rules.md`, `rules/verify-rules.md`; Fowler smell baseline. `golangci-lint` v2.12.2 reported 0 issues.

## Hard checks

- **Unresolved documented violations: 0.** On commit 6aa, `make verify` passed (8,713 test functions: 8,707 passed, 6 skipped; lint 0 issues, scan 0 errors/1,803 warnings). Serial `make test-e2e` passed (4,787 test functions: 4,781 passed, 6 skipped).
- The gate fixture retains the shell-parent-exits/pipe-holding-descendant case. Its 2s timeout, 3s `WaitDelay`, 500ms cleanup, and 2.5s scheduling margin yield an 8s bound. Focused race evidence confirms marker creation, timeout 124, and descendant stop.
- Portability fix uses `tempfile.gettempdir()` and validated `--scratch-root`; syntax/help/scratch smoke passed. Runtime verified on macOS; Linux runtime untested; Windows unsupported.

## Integration and evidence

- `1cd1951` publishes completion before results only for joined executors; pending resources still publish results before joining. Tests cover both, and blocked-Bash longtask uses the pending-resource path.
- `4d1497b` narrows typo-ignore patterns. `.gitattributes` keeps PTY transcripts and raw JSONL/JSON logs byte-preserving. Hash provenance was checked against raw Git output.
- Terminal evidence is 36 checks/12 screenshots from source commit 89df. The report records an empty source-delta hash and binary SHA256 `852776240ba41f069183201a07bb5b89a2a3e454bef88e837b1581afe7b278f5`; commit 6aa changes tests only, so app runtime matches.

## Judgement

- No actionable Fowler smell found. Escape cancellation now classifies legacy cancellation as expected while joining the turn and event bridge; unsafe generic timeout was removed. Focused race and tagged E2E checks pass.
- PTY tests use shell PID/start markers, not approval-preview text, and token/PGID checks limit cleanup to the owned live group. A foreground sleep may persist up to its 5s fixture bound after its Bash leader exits; Linux runtime and zombie-group behavior are unverified.
- PTY children run with `--no-sandbox`; evidence covers approval, tool effects/cancellation, and local HTTP requests, not OS sandbox enforcement or real-provider behavior.

## Disposition

Standards pass: no confirmed hard violation; remaining limits are platform/runtime scope stated above.
