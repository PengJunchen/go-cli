# Provider-local timeout and durable terminal classification

Captured 2026-10-14 for the real Magpie-backed go-cli run. Source state reviewed: `84681a71b08ffd62212a63ef9e0a911e18c20933`.

## Live observation

The isolated run `20261014-152358-63990f30` completed its first text turn against the real Magpie Codex route. On the next model stream, the CLI reported `stream idle exceeded 10s: context deadline exceeded`; the upstream call later finalized as HTTP 499 after 16.05 seconds, with zero reported input and output tokens. The trace had no tool call or receipt for that turn. The terminal UI then reported a transcript refresh identity mismatch. Run report and trace are retained under `docs/qa/2026-10-14/live/terminal/20261014-152358-63990f30/`.

This was a provider-local stream-idle timeout: the model request's nested timeout expired while the enclosing durable run owner remained active. The owner classified the run as an error, while the older terminal classifier treated any `context.DeadlineExceeded` as cancellation. The disagreement prevented the UI's durable refresh acknowledgement from matching the terminal outcome.

## Source change

`internal/core/run_commit_consumer.go:1016` now receives the enclosing run context in `terminalOutcome`. It preserves unknown tool outcomes first and loop-detection errors next, classifies cancellation only when the enclosing context is itself done with the matching cause, and otherwise records an error. `internal/core/loop.go:528` passes the owner-derived `spanCtx` to that classifier. This keeps provider-local deadlines distinct from parent cancellation without weakening the durable owner/TUI checks.

`internal/cli/repl_local_timeout_test.go` adds `TestDurableREPLProviderLocalTimeoutRefreshesAndContinues`. The test uses a local HTTP test server to trigger a short provider-stream timeout in a real durable REPL/Bubble Tea app; it verifies the error is visible, `RunOutcomeError` is persisted and acknowledged before the next input, and a following turn succeeds. This is local test-server coverage, not a real-model acceptance run. The reviewer reported both focused normal and race runs passing.

## Configurable model budgets

`production.model_timeout.total` and `production.model_timeout.stream_idle` accept Go duration strings. The validator requires positive values up to 24 hours and prevents effective stream-idle values from exceeding total. The corresponding environment variables are `GO_CLI_PRODUCTION_MODEL_TIMEOUT_TOTAL` and `GO_CLI_PRODUCTION_MODEL_TIMEOUT_STREAM_IDLE`.

Empty configuration preserves existing behavior: primary model middleware keeps its 30-second total and 10-second stream-idle defaults, while the small background model remains governed by the caller context with no added local timeout. When either timeout is explicitly set, the resolved pair applies to both primary and small-model calls. The Magpie launcher supplies an explicit `10m`/`4m` connection profile so large live streams and small-model summary/title/extraction work share that budget; the framework defaults remain unchanged when no timeout is configured.

A later live PPTX-generation request in run `20261014-181548-4087fce4` exceeded the prior launcher's 90-second stream-idle setting and finalized as HTTP 499 after 122 seconds. The larger 10m/4m launcher profile gives the next full-profile run a longer total and inter-chunk budget; it does not establish that an artifact request will complete within those limits.

Relevant coverage includes `internal/config/loader_test.go` (`TestLoad_ProductionModelTimeoutDefaultsAndEnv`, file loading), `internal/config/validator_test.go` (`TestDefaultValidatorProductionModelTimeout`), and `internal/cli/assembly_model_timeout_test.go` (`TestResolveModelTimeoutsKeepsDefaultsAndAppliesPartialOverrides`, `TestAssembledConfiguredModelTimeoutAppliesToPrimaryAndSmallModel`).

## Validation at the pinned source state

- `make test-e2e` passed on clean source commit `84681a71`: 4,829 test functions started, 4,823 passed, 6 skipped, 0 failed across 7 packages (183.663 seconds). The later commit `508c785` changed only the import grouping in `assembly_model_timeout_test.go`; production source remained identical. The runner recorded an empty source diff before and after the e2e run.
- `make verify` on `84681a71` first stopped at lint on the goimports grouping issue at `internal/cli/assembly_model_timeout_test.go:13`. The raw records were preserved as `docs/qa/2026-10-14/live/gates/verify-84681a7-lint-failed.json` and `verify-84681a7-lint-failed.log` (log SHA-256 `d87a53b93f2641043ceb9595ddc89572a6ca4e0115300be6bf77d2b2426c042e`).
- After the formatting-only commit `508c785`, `make verify` passed lint (`0 issues`) and ran the full race-enabled suite. It reported 8,775 test functions across 28 packages (8,768 passed, 6 skipped, 1 failed); the sole failure was `internal/acp.TestStdioProcessOwnerEarlyExitIsWaitedAndReaped`, which returned `acp: signal stdio process group: operation not permitted` at `stdio_process_owner_test.go:91` on macOS. The full run was otherwise complete and the source diff remained empty. Its records are preserved as `docs/qa/2026-10-14/live/gates/verify-508c785-race-failed.json` and `verify-508c785-race-failed.log` (log SHA-256 `209e2a1272f3436262179d43cd22914cdf98386c4fa389811e02a01498a19560`).
- Follow-up isolated `go test -race ./internal/acp -run '^TestStdioProcessOwnerEarlyExitIsWaitedAndReaped$' -count=1` passed, as did the same command with `-count=10` (1.404 seconds). This points to a failure that was not reproduced by repeated isolated runs; it does not establish that the full-suite failure is harmless. No production-source changes were made during either gate.

No credentials, authorization headers, Codex auth-store contents, prompts, or account identifiers are included in this report.
