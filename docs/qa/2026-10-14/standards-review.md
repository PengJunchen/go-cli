# Standards review

**Fixed point:** `develop` at `e77e8058c51e5370a2ecc3eacb5fce0419829bcf`

**Sources:** repository `AGENTS.md` and `CLAUDE.md`; `go-cli-infra/AGENTS.md`, `CLAUDE.md`, `rules/go-validation-rules.md`, and `rules/verify-rules.md`; Fowler smell baseline. Repo-wide golangci-lint v2.12.2 was run by the main agent and passed with 0 issues; lint-covered rules are omitted here.

## Hard violations

- **Unresolved count: 0.** The runner portability finding from the initial review is resolved: [`scripts/e2e_terminal/run.py`](../../../scripts/e2e_terminal/run.py) now defaults to `tempfile.gettempdir()`, accepts a validated `--scratch-root`, and records platform support accurately. Python/Node syntax, help output, and a scratch-directory create/remove smoke check passed after the E2E capture; the full screenshot run was not repeated. Linux runtime remains unverified and Windows is documented as unsupported.
- No other confirmed violation of the documented Go or verify rules. The ACP fixture cleanup is registered immediately after successful assembly; the test-gate fixture still models a parent that exits while a descendant keeps the pipes open. The terminal report records both the baseline commit and a working-tree source hash.

## Judgement calls

- No actionable Fowler smell found in the reviewed Go changes. The TUI `KeySpace` handling mirrors the existing cursor insertion path for runes; lifecycle and identity fixtures gate real side effects on durable evidence.
- The longtask PTY uses `--no-sandbox` in [`tests/e2e_binary_longtask_test.go`](../../../tests/e2e_binary_longtask_test.go). This documentation gap is resolved: its README, evidence, and result scope now clarify that approval, Bash side effects/cancellation, and HTTP requests are tested, while OS sandbox isolation is not. The tools E2E report states the same limit.

## Disposition

The reported terminal run contains 36 passing checks and 12 real-PTY screenshots; evidence records the working-tree source hash and built-binary hash. The ACP baseline failure and fixture repair are recorded separately in [`tools/acp-trusted-executable-fixture.md`](tools/acp-trusted-executable-fixture.md); repeated targeted and tagged-package reruns passed, so the original stage remains unresolved rather than attributed to load. The additional tagged `internal/core` rerun passed all 1,020 tests; its raw JSON is [`tools/rawlogs/core-e2e.json`](tools/rawlogs/core-e2e.json). The core failure from the initial aggregate run was not reproduced. The terminal runner portability defect and the longtask sandbox-limit documentation gap are both resolved; Linux runtime remains unverified as documented.
