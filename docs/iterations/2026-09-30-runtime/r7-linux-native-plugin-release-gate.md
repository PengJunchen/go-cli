# R7 Linux native-plugin release gate

Status: proposal only; protected CI workflow edits below require separate user authorization. Source baseline: `4d014fa5c5599968ba19bd2bd3839bb54ae4f02e` (`fix(cli): expose hook observer identity safely`). No workflow, Makefile, verification rule, or ledger was changed to prepare this proposal.

## Proposal

The native-plugin lifecycle test already runs inside both existing Linux race-test jobs: `ci.yml` runs `go test -race -count=1 -coverprofile=coverage.out ./...`, and `pr-check.yml` runs `go test -race -count=1 ./...`. Their `ubuntu-latest` runners therefore already execute `internal/cli/TestConfiguredGoPluginLifecycle`; adding a new job or duplicate targeted test would only build and load the same fixture twice.

Add `CGO_ENABLED: "1"` and `GOCLI_REQUIRE_NATIVE_PLUGIN_TEST: "1"` to the existing `test` job in `ci.yml` and the `compliant` job in `pr-check.yml`. Job-level placement keeps the gate active if the separately proposed `protected-changes-test-gate` addendum later replaces the `go test` command with its wrapper. The lifecycle test consumes `GOCLI_REQUIRE_NATIVE_PLUGIN_TEST`; `CGO_ENABLED=1` applies to every Go command in both jobs. Ubuntu runners are expected to support cgo, and the explicit setting makes the plugin build precondition reviewable.

The existing full-suite commands include the lifecycle test and `-race`. The test detects the race build tag, compiles the fixture plugin with `-race`, loads it through the real configured loader, initializes the extension, observes before/after hooks, and shuts it down. With `GOCLI_REQUIRE_NATIVE_PLUGIN_TEST=1`, an unsupported OS/architecture or `CGO_ENABLED=0` skip becomes a failure. On supported Linux with cgo enabled, fixture build or load failures are already test failures. Thus the proposed jobs require a positive native lifecycle execution without adding a second invocation.

This proposal is independent of `docs/iterations/2026-09-25-runtime/protected-changes-test-gate.patch`: it changes neither the Makefile nor the test command, and its hunks are placed at the two job headers. The existing addendum remains unapplied and unapproved. If it is approved first, this patch should still apply; re-run `git apply --check` against the then-current commit before authorization.

## Proposed patch

The complete patch is saved in [r7-linux-native-plugin-release-gate.patch](r7-linux-native-plugin-release-gate.patch). It contains only the two existing workflow job environment blocks. Applying it would edit protected CI files, so this proposal does not apply it.

## Scope and evidence boundary

The patch targets the existing Ubuntu race-test jobs only. It creates no job, changes no test selection, changes no Makefile target, and does not claim that a Linux runner executed during proposal preparation. The proposal establishes what CI should require; Linux runtime evidence must come from the authorized workflow run after approval.

Validation recorded for this proposal: the patch passes `git apply --check` against the exact `4d014fa5c5599968ba19bd2bd3839bb54ae4f02e` source, and both proposed job environment blocks parse as YAML. The native lifecycle test also passed locally on Darwin with the race and coverage flags used by `ci.yml`; this targeted compatibility check is not a Linux or hosted-CI run. No protected file was changed.

Rollback is limited to removing the two added `env` mappings from those jobs.
