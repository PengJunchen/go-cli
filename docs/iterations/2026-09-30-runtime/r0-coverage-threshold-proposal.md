# R0 `test-cov-check` coverage-threshold proposal

Status: proposal only; the protected repository `Makefile` was not changed. Source baseline: `1a37eaac74a08a649df00c1627deda7347515c12`. The proposal patch is [r0-coverage-threshold-proposal.patch](r0-coverage-threshold-proposal.patch), and the executable fixture validator is [r0-coverage-threshold-proposal-test.sh](r0-coverage-threshold-proposal-test.sh).

## Protected-file boundary

The applicable rule in `../go-cli-infra/CLAUDE.md` (lines 9–13) states:

> **禁止自行修改以下文件**，需用户三次确认方可变更：
>
> [rules/] 全部文件 · `../go-cli/.golangci.yml` · `../go-cli/Makefile` · `../go-cli/.github/workflows/ci.yml` · `../go-cli/.github/workflows/pr-check.yml` · [infra/tasks.json] · [infra/milestone.md]
>
> 三次确认流程：AI 提出建议 → 用户确认 → 展示 diff → 用户确认 → 说明影响 → 用户确认 → 执行。

The `Makefile` is protected by that rule. This change prepares a reviewable diff only; it does not apply it to the repository file or claim the required confirmations have happened.

## Problem and proposed behavior

The current `test-cov-check` extracts coverage with `grep total`. That matches both the final `total: (statements) 81.5%` row and any function row whose source path contains `total`, such as `total_counter.go`. The resulting multi-line value is fed to `bc` and then to the shell test. The shell can report `too many arguments`, take the `else` branch, print a pass message, and return success. The independent, anchored result for the supplied root baseline is 81.5%; at the existing default threshold of 65%, that value should pass.

The proposed hunk changes only parsing and threshold comparison after the existing `go test -coverprofile=... ./...` command. It captures all `go tool cover -func` output together with its status, prints the captured report, and returns the tool's nonzero status. It accepts exactly one row whose first field is `total:`, whose shape is `total: (statements) N%`, and whose percentage is a number from 0 through 100. Missing, duplicate, malformed, or out-of-range total rows fail closed. It validates the configured threshold as a number in the same range and compares numerically with `awk`; `COVERAGE_THRESHOLD ?= 65` remains unchanged, and the proposal does not lower the threshold or change `test-cov`.

## Proposed diff

The patch contains a `Makefile` hunk for review. It carries the existing `mkdir` and `go test` recipe lines verbatim as stable patch anchors; applying it leaves both commands unchanged. It has not been applied to the protected file. It is based on the exact source baseline above; re-run `git apply --check` against the then-current source before considering the patch after any intervening changes.

## Validation and evidence boundary

`r0-coverage-threshold-proposal-test.sh` first checks patch applicability, then copies the baseline `Makefile` into disposable scratch directories. It applies the proposal only to the scratch copy and runs the target with a fake `go` command. For the baseline case, a `bc` wrapper records its input and invokes the host's real `bc`; the validator requires `bc` so this reproducer uses the actual calculator behavior. It does not run the repository test suite or modify `Makefile`.

The baseline fixture includes `/module/total_counter.go:17: fakeFunction 100.0%` and `total: (statements) 81.5%` at threshold 65. It asserts that real `bc` receives exactly `100.0` followed by `81.5 < 65`, then reproduces the shell-argument error followed by a success result. The validator prints both the captured calculator input and baseline `make` output. The proposed recipe passes only the anchored 81.5% value at the default threshold, fails 81.5% against 90 and 64.9% against the default 65, and rejects missing, duplicate, malformed, and over-100% total rows. It also rejects a `go tool cover` failure with exit status 7 even when stdout contains a valid-looking total, and verifies the recipe reports that exact status; a nonnumeric threshold is rejected as well.

Validation run: `sh -n docs/iterations/2026-09-30-runtime/r0-coverage-threshold-proposal-test.sh` passed, `git apply --check docs/iterations/2026-09-30-runtime/r0-coverage-threshold-proposal.patch` passed against the exact baseline, and the fixture validator passed every case. These are proposal-fixture checks, not a full Go test run or an authorization to edit the protected `Makefile`.
