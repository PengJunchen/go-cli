# Runtime repair checkpoint — 2026-09-30

This checkpoint preserves successive historical snapshots from `codex/runtime-repair-20260929`; that branch is historical, and the active local merge target is `develop`. The current integrated source snapshot recorded here is `4d014fa5c5599968ba19bd2bd3839bb54ae4f02e`. The full-repository gate evidence below is from its pre-R7-Hook predecessor, `8cb287a5201ce52816d7fc67873b4d60b2b495cd`; focused affected-package gates also passed at `4d014fa`. Documentation-only commits through `29f60564daa2863ebe5d65e7ec5dd071d3b1233e` add reviewed handoffs. This is not a completion claim for the 47-task [execution plan](../2026-09-25-runtime/execution-tasks.json). The older [shared execution ledger](../2026-09-25-runtime/execution-status.md) remains the historical task ledger; this note records newer evidence without changing protected task metadata or that ledger's sole-writer rule.

The historical ledger still lists 14 `done`, 16 `in_progress`, 14 `pending`, and 3 intentionally `deferred` tasks. A read-only comparison of the unfinished builder tasks' planned test names against top-level Go test functions found 17 matches among 128 distinct names. The other 111 names require either an explicit mapping to equivalent tests or new executable acceptance cases; this count alone does not prove the behavior is untested.

## Most recent full-gate baseline: `8cb287a` (before R7 Hook integration)

| Check | Result | Limit and evidence |
|---|---|---|
| `GOMAXPROCS=2 NO_COLOR=1 make verify` | Exit 0: formatting, vet, lint (0 issues), build, all-package `go test -race -count=1 ./...`, scanner, test-log and goroutine-leak gates passed. Scanner: 414 files, 0 ERROR, 1,723 WARN. | `goimports` was unavailable and its optional step was skipped. The scanner WARN findings remain triage items. Logs: `/tmp/go-cli-verify-wave2b-20260930.log` and `/tmp/go-cli-verify-wave2b-full-output-20260930.log` (ends with `Verification complete`). |
| `GOMAXPROCS=2 go test -race -count=1 -tags=e2e ./tests/...` | Exit 0; `tests` completed in 28.175s and `tests/e2e_20260802` in 41.088s. | The earlier `./tests/e2e` attempt failed only during package setup because that directory does not exist; this corrected `./tests/...` run supersedes it. Log: `/tmp/go-cli-e2e-wave2b-20260930.log`. |
| `GOMAXPROCS=2 go test -race -tags=mock ./internal/mock/...` | Exit 0 in 1.029s. | `-count=1` was not passed. Mock-only validation. Log: `/tmp/go-cli-mock-wave2-20260930.log`. |
| `GOMAXPROCS=2 make test-cov-check` | Exit 0; 82.1% total statement coverage against the 65% threshold. | Aggregate coverage is not a per-critical-path guarantee. Log: `/tmp/go-cli-coverage-wave2-20260930.log`. |

The source at `8cb287a` includes these bounded slices, carried into `4d014fa`; none closes its full phase:

- **R3:** Active loop detection warns by default; termination is opt-in, and unknown priority does not itself force termination. Durable current input has a typed identity at index 0 and retains `RunID`/`PriorSourceCount` through preflight and overflow handling. Clone boundaries prevent the compactor from aliasing mutable caller-owned message data.
- **R4:** Native `rg` wrapper and adversarial helper tests cover prepared-path behavior; real SubAgent tests cover prepared-policy behavior. The native `fd` path was unavailable/skipped and is not claimed as exercised.
- **R7:** Operational shutdown reports all seven async-exporter counters; meaningful anomalies exit 1 and quiet zero-loss exits 0. Missing current context occupancy remains unknown and is kept separate from cumulative token usage.

The linked [R3](r3-acceptance-audit.md), [R4](r4-acceptance-audit.md), and [R7](r7-acceptance-audit.md) acceptance audits are frozen maps from older source snapshots. The gate evidence above is newer for source `8cb287a`; it does not replace the audits' phase-level findings or close those phases.

## R7 Hook integration at `4d014fa`

The reviewed Hook-observer implementation is integrated at `4d014fa5c5599968ba19bd2bd3839bb54ae4f02e`. It carries the validated extension owner into Hook events, supplies extension-owned snapshots of run data, and treats remote plugin descriptors as unavailable. The full `make verify` gate has not been rerun at this source; these affected-package gates passed:

| Check | Result | Evidence and limit |
|---|---|---|
| `GOMAXPROCS=2 GOCLI_REQUIRE_NATIVE_PLUGIN_TEST=1 go test -race -count=1 ./internal/cli ./internal/extension` | Exit 0; CLI 41.524s, extension 1.347s. | The required native Go plugin lifecycle gate ran and passed on Darwin. The Linux CI gate remains unwired; Linux execution was unavailable because no Docker daemon was available, and the protected gate wiring is pending approval. Log: `/tmp/go-cli-r7-hook-integrated-race-20261001.log`. |
| `GOMAXPROCS=2 go test -race -count=1 -tags=no_plugin ./internal/cli ./internal/extension` | Exit 0; CLI 37.539s, extension 1.192s. | The native-plugin lifecycle fixture skipped as expected in the disabled-plugin build; the `no_plugin` package tests passed. Log: `/tmp/go-cli-r7-hook-integrated-noplugin-20261001.log`. |
| `GOMAXPROCS=2 go vet ./internal/cli ./internal/extension` | Exit 0. | Scoped to the affected packages. |
| `GOMAXPROCS=2 golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./internal/cli ./internal/extension` | Exit 0; 0 issues. | Scoped to the affected packages. Log: `/tmp/go-cli-r7-hook-integrated-lint-20261001.log`. |

The full-gate result at `8cb287a` remains useful source-wide evidence, but it does not establish a full verify at `4d014fa`. The next full repository gate is planned after the remaining R6, R7 Skill-schema, and R3 envelope integrations.

## Earlier historical evidence

| Check | Historical result at `551e260` | Limit |
|---|---|---|
| `PATH=/Users/pengjunchen/go/bin:$PATH GOMAXPROCS=2 NO_COLOR=1 make verify` | Exit 0. Format, vet, lint (0 issues), build, full repository race, scanner, log and leak gates passed. Scanner: 403 files, 0 ERROR, 1,699 WARN. | The scanner warnings remain findings for triage; zero ERROR is the current gate result, not proof that every original task is complete. |
| `GOMAXPROCS=2 NO_COLOR=1 make test-cov-check` | Exit 0; total statement coverage 82.0% against the 65% threshold. | Coverage is the repository aggregate, not a per-critical-path guarantee. |
| `go test -race -count=1 -tags=e2e ./tests/...` | Exit 0; 790 tests across two packages. | Tagged local E2E does not establish production or every external integration. |
| `go test -race -count=1 -tags=mock ./internal/mock/...` | Exit 0; 153 tests. | Mock validation only. |

The later historical source snapshot at `34e7a7d` had these checks after the R2/R3/R4/R7/R8 repairs and test-fixture follow-ups; it is not the current source head:

| Check | Result at `34e7a7d` | Limit |
|---|---|---|
| `PATH=/Users/pengjunchen/go/bin:$PATH GOMAXPROCS=2 NO_COLOR=1 make verify` | Exit 0; format, vet, lint (0 issues), build, full repository race, scanner-gate, log, and goroutine-leak checks passed. Scanner: 412 files, 0 ERROR, 1,717 WARN. | All default-build packages passed; scanner WARN findings remain triage items. |
| `GOMAXPROCS=2 NO_COLOR=1 make test-cov-check` | Exit 0; total statement coverage 82.0% against the 65% threshold. | Aggregate coverage does not prove each critical path. |
| `GOMAXPROCS=2 NO_COLOR=1 go test -race -count=1 -tags=e2e ./tests/...` | Exit 0 across both tagged test packages. | Local E2E is not production or external-service validation. |
| `GOMAXPROCS=2 NO_COLOR=1 go test -race -count=1 -tags=mock ./internal/mock/...` | Exit 0. | Mock-only validation. |

R8 long-transcript source and its anchor correction received independent review, then passed 609 TUI race tests on the integrated branch. The fixed 10,000-row × 8 KiB plain-text workload retained approximately 12.5 MB of extra viewport data versus approximately 106 MB before the change; the exact workload and limits are in the [benchmark report](evidence/r8-canonical-viewport-memory-587be07/README.md). A [native PTY journey](evidence/r8-native-pty-587be07/README.md) exercised live PageUp, 60/80/120/200-column resize, End, final answer, a real Read tool cycle, `/exit`, and terminal restoration. Its final viewport contains a successful receipt, but no saved-publication badge; the evidence says so explicitly.

R3 usage/cost provenance and R4 prepared workspace-read preflight were independently reviewed and merged. The R3/R8 integration snapshot passed 2,721 TUI/CLI/Core/Production race tests; the later R4 integration snapshot passed 2,137 CLI/Tools race tests. The full `make verify` result above is the exact `551e260` source-wide check. R4 validates the same frozen path arguments before policy classification and executor dispatch. Its existing symlink check is still check-then-use application validation, not an OS sandbox.

After the historical snapshot, reviewed R2 start-gate, process identity, Linux PID-namespace inspector and emission changes were integrated through `1354874`; the root `core`/`tools`/`session` race suite passed 2,636 tests before the inspector and namespace additions. Reviewed R3 stream sanitizer repairs are integrated through `1ba61a4`: Claude/Gemini markers are handled across chunks, unclosed sensitive blocks fail closed, and ordinary CJK bracket text is preserved pending typed reasoning metadata. The independently reviewed R4 actual-Assembly effect inventory gate is integrated at `7e487d9`. Reviewed R2 process recovery diagnostics and its mixed-row fail-closed repair are integrated through `eb3760f`. The separate Linux real-CLI SIGKILL/escaped-child test passed on the first candidate; after the mixed-row repair, the full session race suite passed 430 tests on the isolated candidate. Reviewed R8 bounded assistant stream preview and exact duplicate handling are integrated through `31c03b8`, with [benchmark and PTY summary](evidence/r8-app-stream-scaling-9d69697/README.md). The final R8 candidate passed 625 TUI race tests in isolation; its native PTY End frame was taken before rendered text was awaited, so immediate active-stream follow-tail behavior remains unverified. Reviewed R7 Skill loader bounds, cancellation, and rooted traversal are integrated through `7e44faf`; its deterministic ancestor-swap regression passed 10 repetitions under race on Darwin, and the Linux amd64 test binary compiled. Later test-only follow-ups aligned Skill E2E descriptions and removed scanner false positives without changing production behavior. Earlier `make verify` failures at `299f27c` (race), `7e44faf` (goimports), `316b62e` (old Skill E2E wording), and `93025fc` (nine scanner ERRORs in fixtures) remain historical failures; the final `34e7a7d` gate above supersedes them for the current source.

Linux Docker exposed two baseline test fixtures that failed even at `551e260`: an orphaned fake-Git child remained a zombie under a non-reaping PID 1, and a fake-Git log separator relied on a non-POSIX `printf \\x1f` escape. The reviewed fixture repairs are integrated at `c6da3b4` and `299f27c`; the isolated Linux Tools race suite with both repairs passed 1,296 tests, skipped 4, and failed 0. This is Linux test evidence for that source composition, not a claim that every platform or the later integrated head was rerun.

## Historical open work recorded at `34e7a7d`

- **R0 protected test gate:** Only the originally approved `protected-changes.patch` has been applied. The separate [test-gate addendum](../2026-09-25-runtime/protected-changes-test-gate.md) is still unapplied pending its own authorization and release review. `git apply --check` succeeds at this source; that is not authorization or a gate pass.
- **R2 restart recovery:** The reviewed trusted Bash gate now rejects a PATH-shadowed interpreter before start, and sequential/parallel managed Bash records exact action/process identity before releasing user code. Linux records a PID-namespace token and has a conservative read-only group inspector; Darwin inspection remains unknown. The reviewed session diagnostic stays `Unknown` for missing, mismatched, or conflicting dispatched identities, and **does not** resolve a restarted action. `internal/tools/process_scope.go` documents that descendants can escape a POSIX group with `setsid`/`setpgid`, so even an absent recorded PGID does not prove every local side effect stopped. The real-CLI SIGKILL/escaped-child evidence shows a late effect after the original group disappeared; unknown receipt and quarantine remain. Recovery must retain both until stronger durable containment proof or explicit reconciliation.
- **R3/R7 source audit findings:** The stream sanitizer, Skill adapter argument contract, Skill/Memory XML encoding, and Skill loader resource/cancellation/rooted traversal defects are reviewed and integrated. A later review's mixed legacy/attempt cost and TUI provenance findings were checked against the current source; `CombinedKnownCostSubtotal`, CLI display and TUI bridge/status tests already cover them. These bounded repairs do not close R3 or R7 phase acceptance.
- **Remaining R3–R8 acceptance:** This wave closes bounded defects, not every task in the original plan. Source-level acceptance, UI journeys, service entrypoints, capability claims and deferred product features still require task-by-task review. R9 remains intentionally deferred.

## Current remaining boundaries and reviewed design handoffs

These are design candidates or unresolved acceptance boundaries, not integrated implementations:

- **R3 provider envelope:** The reviewed [provider-envelope preflight handoff](r3-provider-envelope-preflight-handoff.md) describes a bounded estimate/admission contract. Its implementation remains unmerged; it does not close R3 or enable a native provider.
- **R6 trust dispatch:** The [live-epoch revocation design](r6-trust-dispatch-revocation-design.md) and [trust snapshot API handoff](r6-trust-snapshot-api-handoff.md) remain implementation candidates, not shipped behavior.
- **R7 Hook observer:** The [Hook observer owner-boundary contract](r7-hook-observer-contract.md) is the reviewed design basis for the implementation integrated at `4d014fa`; the remaining phase acceptance is open.
- **R7 Skill schema:** The [Skill schema handoff](r7-skill-schema-handoff.md) is reviewed design only; its implementation remains open.
- **R8 active preview:** The [active-preview guard handoff](r8-active-preview-guard-handoff.md) is a reviewed, doc-only boundary proposal. A release owner must choose between preserving the complete-response guard with content-free progress only, or explicitly permitting a visibly provisional speculative preview that may already expose text later blocked or redacted. No preview builder is authorized until that disclosure choice is made; R8 active partial-text acceptance remains open.
- **Other pending decisions:** R2 restart recovery and reconciliation remain pending. Native Claude/Gemini enablement, the R4 Plan/model-transition decision, and the R3 PRD-04 hard-cost-budget policy remain unresolved. R9 remains intentionally deferred.

No remote push or PR is part of this checkpoint. Recheck the active local `develop` merge target immediately before merging accepted work.
