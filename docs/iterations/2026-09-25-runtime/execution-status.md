# Runtime iteration execution ledger

Last initialized: 2026-09-25. This ledger tracks execution evidence for the 44 tasks copied into [execution-tasks.json](execution-tasks.json). The iteration designer is the sole writer of this shared ledger; builders send lane evidence in their own reports, and the parent forwards approved updates for entry here. Do not mark a task `done` until its isolated commit is cherry-picked to the execution branch and independently reviewed.

## Baseline and branch

| Evidence | Value |
|---|---|
| Audited source baseline | `40692e35e4815f0dff1c9f91b13905008f7221a4`; clean source baseline from 2026-09-24. |
| Execution branch | `codex/runtime-stabilization` |
| Execution start commit | `00149001e2350b18dd69d7fa41ab7fcd6ed0241a`; docs-only checkpoint above the audited source baseline. |
| Audit plan / task metadata | `docs/audits/2026-09-24/10-runtime-iteration-plan.md`, `proposed-tasks.json`, and `findings-to-iterations.csv` (read-only). |
| Status at ledger initialization | 44 total: 41 `pending`, 3 R9 tasks `deferred`. |
| Current task status | 44 total: 4 `done` (R0-DES-01, R0-BLD-01, R0-BLD-05, R1-DES-01), 3 `in_progress` (R0-BLD-02..04), 34 `pending`, 3 R9 tasks `deferred`; machine-readable counts are in `execution-tasks.json`. |

The audit's earlier evidence is historical baseline evidence, not R0 post-fix verification: raw `make verify` failed at the test stage with 37 ANSI assertions under `NO_COLOR=1`/`TERM=dumb`; `make verify` with `NO_COLOR` cleared passed tests but then failed `make scan` with 20 ERRORs and 1,273 WARNs. `make test` with `NO_COLOR=` passed the default race suite, and `go test -race -count=1 ./cmd/cli` passed. The source logs are `docs/audits/2026-09-24/evidence/make-verify.log`, `make-verify-no-color.log`, `make-test-no-color.log`, `go-test-cmd-cli.log`, and `make-scan.log`. Those results do not describe the current code after builders.

Integrated verification on `2d3c333` ran `GOMAXPROCS=2 go test -p 2 -race -count=1 ./...` and exited 1 after 101.69 seconds. Eight CLI MCP fixtures used IDs rejected by the strict parser, and seven `tests/e2e_20260802` color assertions failed under `NO_COLOR=1`; fixes are now integrated as `980712`/`62b8229` and `866dfec`/`3fe4389`, respectively. The failure is recorded in [integrated all-package race log](evidence/r0-integrated-all-race.log); the post-fix full race rerun remains pending, so this is not a passing full gate.

R0 permission/schema slices, provider routing including the small-model credential/origin fallback, and the Bash subprocess/output slice have been integrated, independently reviewed, and package-race checked; the Bash portion of aggregate R0-BLD-03 is accepted while its TUI work remains open. TUI code review is accepted, but E2E color hermeticity is still being repaired. Other active R0 lanes remain open until their own RED/GREEN package-race evidence and review are accepted. Do not claim historical reproducer logs as fixes: a reproducer PASS records the old bug. For each new regression, RED means the test compiles and fails because old behavior violates the expected contract; compilation errors and zero matched tests are invalid RED evidence. GREEN requires the same regression test and the affected package's race suite to pass. Full repository gates are recorded separately.

## Lane handoff and evidence

Each builder records detailed test names, commands, output paths, and its isolated commit in a lane report. The shared ledger is updated only from those reports after parent review.

| Lane / worktree | Source tasks | Isolated base | Builder commit | RED evidence | GREEN / package race evidence | Cherry-pick + independent review |
|---|---|---|---|---|---|---|
| permissions — `.../permissions` | R0-BLD-01, R0-BLD-05 | `00149001e2350b18dd69d7fa41ab7fcd6ed0241a` | `e783d5f`, `12c68de`, `58782ef` (integrated) | [red](evidence/permissions-red.log) | [green/race](evidence/permissions-green.log) | accepted by [R0 code review](reviews/r0-code-review.md), recorded at `764ca0c` |
| bash — `.../bash` | R0-BLD-03, TOOL-002 | `00149001e2350b18dd69d7fa41ab7fcd6ed0241a` | `6235107` (integrated), limit documented at `0d84f12` | [red](bash-red.log) | [green/race](bash-green.log); combined permissions/Bash race in [integrated log](evidence/r0-integrated-permissions-bash-race.log); descendant delay reproduced in [log](bash-descendant-repro.log) | Bash slice accepted by parent review and combo race archive `a00c0db`; aggregate R0-BLD-03 remains open for TUI |
| tui — `.../tui` | R0-BLD-03, UI-01, UI-03, VFY-01 | `00149001e2350b18dd69d7fa41ab7fcd6ed0241a` | `15be450` plus ANSI fix `866dfec`/`3fe4389` (integrated) | reported; see [review report](reviews/tui-provider-code-review.md) | lane race reported; post-fix full race rerun pending | in progress until post-fix integrated verification |
| network — `.../network` | R0-BLD-02, CLI-01, MCP-01, MCP-02 | `00149001e2350b18dd69d7fa41ab7fcd6ed0241a` | test-isolation fix `980712`/`62b8229` integrated; network implementation/review still in progress | reported; independent evidence review pending | pending | pending |
| provider — `.../provider` | R0-BLD-02, LLM-01 | `00149001e2350b18dd69d7fa41ab7fcd6ed0241a` | `c252360` plus fallback fix `87ff770` (integrated); evidence recorded at `2062acb` | [red log](evidence/provider-red.log), [small-model red](evidence/provider-small-red.log); exact tests listed in [lane report](lane-provider.md) | [green log](evidence/provider-green.log), [race log](evidence/provider-race.log), [small-model green](evidence/provider-small-green.log), [small-model race](evidence/provider-small-race.log) | code review accepted in [review report](reviews/tui-provider-code-review.md); R0-BLD-02 remains open for network/privacy lanes |
| privacy — `.../privacy` | R0-BLD-02, TRACE-01 | `00149001e2350b18dd69d7fa41ab7fcd6ed0241a` | in progress | reported; a review found remote error-body leakage in an error field; fix and independent evidence review pending | pending | pending |
| verify — `.../verify` | R0-BLD-04, VFY-01..10 | `00149001e2350b18dd69d7fa41ab7fcd6ed0241a` | in progress | reported; reviewer found SCAN-012 scope must permit valid cross-package production implementations; fix and review pending | pending | pending |

VFY-01's ANSI test hermeticity is owned by the TUI lane; the verify lane handles the scanner and gate-truth work. Protected Makefile, CI workflow, linter, infra rules, and infra task files remain unchanged. Any necessary protected-gate change must first arrive as a reviewable proposed patch and is not applied under this ledger initialization.

## Design and audit status

| Item | Status | Evidence |
|---|---|---|
| R0-DES-01 contract | Complete; parent approved and independent contract review cleared. | [r0-contract.md](r0-contract.md) |
| R0-BLD-01 / R0-BLD-05 permission/schema/Plan Mode slices | Done after branch integration, passing lane race evidence, and independent review. This closes only the R0 stopgap; Plan Mode name/executor identity remains a mandatory R4 fix. | [lane report](lane-permissions.md), [review](reviews/r0-code-review.md) |
| R0-BLD-02..04 | In progress. Provider review is accepted; network, privacy, TUI post-fix integrated verification, and verify scope remain active. A privacy review found remote error-body leakage requiring repair. The Bash portion of R0-BLD-03 passed review and the combined package race check. The integrated all-package race failed on eight strict-parser fixture IDs and seven color assertions before those test fixes were integrated; rerun remains pending. | Lane rows above; [Bash scope/evidence](lane-bash.md), [provider evidence](lane-provider.md), [TUI/provider review](reviews/tui-provider-code-review.md) |
| Source plan accuracy | Prior independent review accepted the 44-task DAG, its 41/3 status split, finding mapping, and evidence limits. R0 contract narrows only lane boundaries and tests; it does not rewrite the audit reports. | `docs/audits/2026-09-24/11-independent-review.md` |
| R1-DES-01 | Complete: parent design accepted and the independent reviewer approved the detailed contract. It incorporates `CompleteResponseGuard`, bounded EventStream owner cancellation, run-span ownership, current-run transcript deltas, and pending-resource/outcome separation. The optional canonical transcript API seam is integrated as `9741137` (worktree cherry-pick `7377fac`); this is interface groundwork, not R1 feature completion. | [r1-contract.md](r1-contract.md), [design review](reviews/r1-design-review.md) |
