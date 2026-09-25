# R0 verification lane independent review

Artifact: `/Users/pengjunchen/Documents/github/go-cli/docs/iterations/2026-09-25-runtime/reviews/r0-verification-review.md`

Status: **pass for R0 implementation evidence** at commit `0692b2ef146d8c6b6fa0e5d6a704d111689ab70a` (`fix(verify): harden gate event evidence`), relative to baseline `00149001e2350b18dd69d7fa41ab7fcd6ed0241a`. The verify worktree was clean at review. This approves the lane's source and evidence boundaries; it does not connect the gates to the default build/CI flow.

## Findings

SCAN-012 now loads production packages below `internal/` with `go/packages` and uses shared `go/types` identities and `types.Implements` to find actual cross-package implementations. It recognizes production `internal/cli`'s `cliHITLEmitter`, uses the interface type identity rather than matching a name, excludes `_test.go` implementations, and fails closed when loading or type information is incomplete. Cross-package positive and test-only negative cases are covered. The wrapper that embeds an interface is structural method-set evidence only; it does not prove a default factory, a useful delegate, or runtime behavior.

Static source heuristics remain separate from behavioral evidence. VQ/VG source markers produce `HINT` with `EvidenceStaticHeuristic` and explicitly require behavioral verification. TESTCOV reports static test inventory and fails when no behavioral test function exists; rules with no executed evidence remain `SKIP`/`EvidenceNotRun`. The runner does not promote a static marker into a behavior pass.

The scanner gate rejects child-process failure, malformed or incomplete JSON, missing report fields, mismatched summary counts, unknown severity, output-limit overflow, and reports containing ERROR findings. Tests exercise the process and report failure paths as well as a valid report. The actual worktree scan exited 0 and reported 313 production Go files, 1,273 WARN findings, and 0 ERROR findings. This is a static scan result; the WARNs remain visible and it is not evidence for R1 runtime behavior.

The test gate parses real `go test -json` events, adds `-count=1` by default so a cached package result cannot stand in for a fresh test run, and requires at least one passing test or example. It rejects zero matches, test failures even when a child exits zero, unfinished tests, malformed event shapes, invalid package/test event ordering, and package streams without a terminal event. It accepts a benchmark terminal alongside passing tests, rejects benchmark-only evidence, and its real parallel-test fixture also passes with explicit `-count=2`.

Both subprocess gates use bounded captures and `exec.Cmd.WaitDelay`. The gatecapture fixtures exercise stdout/stderr overflow, timeout, and a descendant that inherits the output pipes; each gate returns failure within its bound when evidence is incomplete. On timeout or overflow the helper cancels the direct child. `WaitDelay` bounds the gate's wait when a descendant still holds a pipe; it does not terminate that descendant. The pipe-holder fixtures explicitly clean up their descendant after checking the bounded return. This is acceptable for this R0 evidence gate because those failures cannot produce a passing result; process-tree cleanup remains a separate operational concern.

## Independent verification

I independently ran the gate package race suite at the reviewed commit:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 ./internal/verify/cmd/gatecapture ./internal/verify/cmd/scanner-gate ./internal/verify/cmd/test-gate
PASS: all three packages
```

Exact discovery commands confirmed the cross-package SCAN-012 production positive, test-only negative, wrapper/static-evidence boundary, the failure/nonterminal/package-order/benchmark/cache test-gate cases, and scanner-gate subprocess/report failure cases. The independent pre-commit run of the full scanner gate also returned 0 with the same scanner source and reported 313/1,273/0; the builder's final-hash execution record includes the final full-scan run.

I also reran the SCAN-012 cross-package positive, test-only negative, and wrapper/static-evidence cases with `rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestScanner_SCAN012RecognizesCrossPackageProductionImplementation|TestScanner_SCAN012DoesNotCountTestOnlyCrossPackageImplementation|TestRunVerifyTreatsEmbeddedInterfacePromotionAsStaticEvidence)$' ./internal/verify`; all three passed.

The builder's final-hash full race run passed:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/verify/...
PASS: internal/verify (97.840s), gatecapture, scanner, scanner-gate, and test-gate
```

The execution log is [verify-execution.md](../verify-execution.md). Protected proposal hashes remained `dce747ce55616e4a5faa8b3bc223853da5a318309be4c779bcb651a9479bbc07` and `7538750d4179f2ec31c6c3f17336fda7f1e3985979bdc9e14a79883971a5b359`.

## Integration boundary

`Makefile`, `.github/workflows`, and `.golangci.yml` have no diff from the baseline, and no `scanner-gate` or `test-gate` reference is wired into `Makefile` or GitHub workflows. The protected gate changes remain proposal material and were not applied. Therefore, the lane implementation and direct gate commands pass, while the repository's default Makefile/CI path is **not yet connected** to those gates; do not report default-gate parity or a full `make verify` result from this lane.

## Legacy E2E fixture follow-up

Reviewed test-only source commit `e53cfc4e6ec2f44a65397cf70f8861be7dc1302c`, integrated as `25de39d78a02c5b97b1dce4eb09bb5aa40de4641`. The integration has no diff from that source for [`09_verify_e2e_test.go`](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r1-integration/tests/e2e_20260802/09_verify_e2e_test.go). The fixture now asserts each of the six VQ/VG heuristic results appears once with `Status=HINT` and `EvidenceStaticHeuristic`, matching the runner's actual evidence contract. Existing marker-detection tests in `internal/verify/vqvg_rules_test.go` remain intact; this patch changes no product code, rule, or threshold and does not hide a marker-detection failure.

The summary fixture counts each result by `PASS`, `FAIL`, `SKIP`, or `HINT`, checks unrun rules remain `EvidenceNotRun`, checks heuristic hints remain `EvidenceStaticHeuristic`, rejects unclassified statuses, and asserts both the summary fields and total conserve every result.

Independent targeted race verification on integration commit `25de39d78a02c5b97b1dce4eb09bb5aa40de4641`:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestVerify_RunVerifyAgainstGoProject|TestVerify_ReportSummaryComputation)$' ./tests/e2e_20260802
exit 0
tests/e2e_20260802: 1.078s
```
