# Verification

Related reviews: [MCP durable contract](mcp-contract-review.md) and [approval behavior](approval-behavior.md).

Date: 2026-10-25. Builds and gates run sequentially with the original Make targets, unchanged scanner/skip rules and timeouts, `GOFLAGS='-trimpath -p=2'`, task-owned caches and temporary directories, and the original HOME. Builds use release flags `-s -w`, without race instrumentation. Race and count settings for tests come from the original Make targets. Live UI evaluation may run concurrently; elapsed times are observations, not a performance comparison.

The new approval-parity candidate's manifest covers 1,253 Go files plus `go.mod` and `go.sum`. Its canonical aggregate SHA-256 is `7886f9740952df3ebff7558eb41e2849b8c37f51f9ed9ddd6eabec3069f42554`. It hashes the compact, key-sorted JSON map from repository-relative file paths to their SHA-256 values. All 1,253 Go hashes were independently matched to the builder's final frozen manifest. Both new release builds preserved this aggregate before and after compilation. A later read-only recheck reused the original snapshot functions and matched the passed gate's complete file map and aggregate exactly.

The earlier candidate covered 1,251 Go files plus the two module files. Its aggregate before and after the initial builds and first verification attempt was `7d134df38e637197c1e09c36561a22cc356bf22ad6965fbd2a901a894bbb74e2`.

The authorized comment-only fixture repair changes that aggregate to `e7bb7ebeb30dbc9dc90abea45f09ac05627828d3a022904f3e4f095145d9bffb`. Exactly one Go file changed; the ten frozen implementation/test files and five live-harness files remained unchanged. Rebuilding both release variants produced exactly the same binary bytes as before the fixture comment.

All results through the ten-function diagnostic below belong to that earlier candidate. The test-helper/deadline corrections and approval-policy parity change form the subsequent candidate with the 1,253-file freeze above. Earlier real-model results must not be attributed to its different production bytes.

| Operation | Status | Exit | Seconds | Test started / passed / failed / skipped | Test packages |
| --- | --- | ---: | ---: | --- | ---: |
| Original `make build` | Passed | 0 | 16.873 | Not a test gate | — |
| `headless,no_plugin` release build | Passed | 0 | 4.062 | Not a test gate | — |
| Original `make verify`, attempt 1 | Failed at lint | 2 | 15.716 | 0 / 0 / 0 / 0; test phase not started | 0 |
| Original `make verify`, attempt 2 | Failed at lint | 2 | 124.709 | 0 / 0 / 0 / 0; test phase not started | 0 |
| Original `make build`, after fixture annotation | Passed; binary unchanged | 0 | 0.720 | Not a test gate | — |
| `headless,no_plugin` rebuild after fixture annotation | Passed; binary unchanged | 0 | 0.301 | Not a test gate | — |
| Original `make verify`, attempt 3 | Failed in full race tests | 2 | 665.123 | 9,073 / 9,057 / 10 / 6 | 32 |
| Exact ten failing functions, corrected child umask | Failed in two fixture helpers | 1 | 8.054 | 10 / 8 / 2 / 0 | 4 |
| New approval-parity full release build | Passed | 0 | 4.918 | Not a test gate | — |
| New approval-parity `headless,no_plugin` release build | Passed | 0 | 2.676 | Not a test gate | — |
| Original `make verify`, attempt 4, new candidate | Passed | 0 | 868.867 | Main gate: 9,102 / 9,096 / 0 / 6 | 32 |
| Original `test-log` phase within verification | Passed | 0 | — | 10 / 10 / 0 / 0 | 5 |
| Original `test-leak` phase within verification | Passed | 0 | — | 6 / 6 / 0 / 0 | 5 |
| Original `make test-e2e`, new candidate | Passed | 0 | 262.559 | 4,937 / 4,931 / 0 / 6 | 9 |

The first verification attempt completed formatting and vet, then failed because `golangci-lint` was absent from PATH. The original formatting target reported its optional `goimports` skip because that executable was also absent. Unit tests, scanner, log tests and leak tests were not started. This is an environment failure; no test failure count is inferred from unexecuted phases.

The environment repair reused the existing `golangci-lint` 2.12.2 binary, built with Go 1.26.1 and matching both CI version pins and the version 2 lint configuration. Only this executable was copied into a task-owned tool directory and added to the verification process's PATH. No optional formatter or new linter version was introduced.

Attempt 2 completed formatting and vet, then the unchanged lint configuration reported one `revive` package-comment issue in an existing live-evaluation fixture. There were zero test-gate starts because lint precedes the tests. This failure is retained separately from the first attempt's missing-executable failure.

The repair adds one accurate package documentation comment to that fixture. Its preexisting manifest already disagreed with the retained repository bytes, so the old values are preserved as unverified historical records; separately recorded pre-annotation and current-copy metadata avoid claiming that the annotated file is the original run's exact bytes. No executable statement or lint rule changed.

Attempt 3 passed formatting, vet, lint (zero issues) and build, then reported ten failing test functions. Seven permission failures were caused by the private log runner incorrectly passing its restrictive artifact umask to Go subprocesses. The runner now preserves the original child umask while keeping its own artifacts private; all seven permission tests passed in the exact follow-up run. A request-count assertion that failed under the full run also passed in isolation, so that timing-sensitive failure remains separately recorded. The remaining two failures consistently expose a fixture helper deriving a filesystem path from a trimpath-rewritten source location. No assertion, race setting, timeout or trimpath flag was weakened to address these findings. Scanner, log-test and leak-test phases were not started because the main test gate failed.

The fixture helper was corrected to locate the real module from the test working directory and validate its module identity. The model-timeout fixture now records both actual model/stream requests and server-observed cancellation, retaining the two-request and deadline assertions with bounds that distinguish the configured idle/total limits from default or outer-guard timeouts. These test corrections and the approval-parity production changes were frozen together before the final builds and gates.

Attempt 4 and the subsequent original E2E target passed on the same frozen new source. Lint reported zero issues. The original scanner inspected 466 files and reported 1,870 findings: zero errors, 1,870 warnings and zero informational findings. The unchanged gate accepts those warnings; no baseline comparison is available to classify all of them as preexisting. Per-phase package/test counts overlap because the original targets intentionally rerun some tests; they are not added into one unique-test total.

The main gate's six skips were: three cases requiring an unavailable `fd` executable; one Linux-specific memory-limit enforcement case; one unsupported-containment branch because containment is supported on this operating system; and one unknown-ownership fallback branch because ownership validation is supported on Darwin/Linux. E2E's six skips were: two `fd` cases, the Linux memory-limit case, and three legacy CodeHighlighter cases already replaced by the glamour MarkdownRenderer. All skip conditions are the original source conditions.

Before building and again before verification, opaque checks found zero content or relevant stat changes across nine personal files and 58 production-authority files. There were zero time-only observations, zero mismatches among ten frozen implementation/test files, and zero mismatches among five live-harness files. These checks inspect only opaque SHA-256 and metadata; private hashes, inodes and file contents are not published.

A subsequent preflight observed one shared trust-store change caused by the authorized live grant. The observation was retained and reviewed before resuming. That shared store is now observed privately while its exact owned-key lifecycle is audited separately; the other eight personal files and original 58 authority files retain hard opaque comparisons. Two new authority files matched the explicitly owned task workspace. This correction concerns external monitoring of authorized writes and does not change a Go gate, assertion, timeout, scanner or skip rule.

Final checks after the required gates and help observations found zero content/relevant-stat changes in those eight personal files and original 58 authority files, with one time-only personal-file observation retained without aborting. Six new authority files matched exact task-owned workspace IDs. Four other new files remain private opaque observations without ownership claims or content reads. A monitoring command-ordering error before E2E was retained in the record; root explicitly authorized continuing E2E after reviewing the unchanged original baselines. Every later metadata/binary check ran independently with its own exit status handled. New files are never deleted merely because they are new.

After all heavy Go gates ended, each release binary ran `--help` ten times from fresh task-owned directories with an allowlisted environment, the original HOME and explicit temporary directories. All 20 calls exited zero. Source and binary hashes matched before and after.

| Help-only observation | Calls / exit zero | Wall median | Wall range | Darwin maximum RSS median | RSS range |
| --- | --- | --- | --- | --- | --- |
| Full release | 10 / 10 | 30.393 ms | 25.077–42.825 ms | 26,075,136 bytes | 25,886,720–26,329,088 bytes |
| Headless/no-plugin release | 10 / 10 | 16.925 ms | 14.673–557.192 ms | 14,786,560 bytes | 14,630,912–15,302,656 bytes |

Wall time includes the `/usr/bin/time` wrapper; the outlier remains in the range. These are help-command observations, not interactive/model latency measurements or a Pi/Rust comparison. No performance ranking is inferred.

Original raw logs, exact commands/environment selections, binary sizes/hashes, source manifests and failed-attempt evidence are retained in private task storage. No failed attempt was overwritten. The required original verification/E2E gates and optional help observations are complete. No Make, lint, scanner, timeout or skip rule was changed. Root owns the separate new-candidate real-model UI/stdio evidence and exact owned-resource cleanup.

An initial read-only cleanup audit checked 8,096 candidate canonical or task-log-derived paths across 44 owned raw logs. Six of the ten new authority files then had exact workspace-ID ownership proofs; four remained unattributed. Temporary-directory path IDs were preserved privately. Twenty-three synthetic offline checks passed, including stop/decision rejection and injected durability failures that preserved sources; those tests did not execute production cleanup or query live processes.

Following independent review and the final scope-bound stop observation, root performed five real CLI revokes and approved the fresh exact cleanup plan. Cleanup exited zero. A final read-only postcheck confirmed five owned journals were archived/removed and five own trust tombstones removed, with zero owned trust keys remaining. The original 58 authority files and eight hard-monitored personal paths remained unchanged, with one time-only observation. Seven known authority files, all stable locks, four unattributed files and two ignored checkout trees were retained. Source, both binaries and all five harness files remained unchanged. The [safe cleanup record](../2026-10-25-sol-live-e2e/cleanup.md) documents these boundaries; it does not substitute for the separate real-model evaluation report.
