# R0 verification implementation notes

This note records the implementation and evidence in the isolated `codex/runtime-r0-verify` worktree. It is not a release result for the main branch; R1 runtime implementation and integration validation remain open.

## SCAN-012 evidence model

The rule source says to inspect interfaces declared under `internal/core` and check for a corresponding default implementation. The scanner now loads the internal production package tree once per scan through `go/packages` (`Tests: false`, `NeedSyntax` and `NeedTypes`) and uses the shared `go/types` package graph plus `types.Implements` to find concrete named types in any loaded internal production package. The existing `internal/verify` exclusion is respected. This recognizes implementations supplied by callers such as `internal/cli/cliHITLEmitter` without treating test fakes as production code. Import aliases, named-type identity, pointer receiver sets, aliases in method signatures, and promoted methods are resolved by the compiler's type checker.

The SCAN-012 load has a 90-second context timeout. It is one `packages.Load("./...")` rooted at the internal production tree, not one load per interface. A package load/type-check error, missing requested package, incomplete type information, or timeout yields an ERROR finding with a diagnostic location when available; it does not silently skip the affected package. `packages.Error.Pos` is parsed into its file and line components.

Only non-basic interfaces with type terms are excluded as generic constraints. A generic method-set interface such as `Reader[T]` remains a runtime interface and produces an unresolved ERROR until it can be checked as an instantiation. Generic concrete types are not instantiated by this scanner; a declaration that may satisfy an interface only after instantiation can therefore remain an ERROR. The finding says that no implementation is proven among non-generic types. This conservative limitation is covered by a test and is not a severity change or suppression.

SCAN-012 is structural method-set evidence. An embedded interface wrapper counts if Go's method set says it implements the target; this does not prove a zero-valued wrapper can be invoked, that a factory constructs it, or that runtime behavior is correct. `RunVerify` records scanner and test-declaration results as static evidence. Its `TESTCOV` message explicitly says declarations are not executed by `RunVerify`; VQ/VG marker checks are `HINT` with `static_heuristic` evidence. The separate `test-gate` CLI runs `go test -json` and checks actual `run`/`pass` events. It is proposed for Makefile/CI/PR wiring in `protected-changes-test-gate.patch`; until that protected addendum is approved and integrated, this worktree command is not the default project gate.

## Adversarial coverage

The tests cover a real cross-package production implementation, a test-only implementation that must not satisfy a production interface, a generic runtime interface, an uninstantiated generic concrete candidate, type-set constraints, unresolved external embeddings, absent concrete values in compile assertions, different imported package identities under identical aliases, aliases in method signatures, and concrete/interface method promotion. A `RunVerify` fixture also shows that an embedded interface wrapper can satisfy SCAN-012 structurally while the report labels both scanner and test inventory as static evidence; it does not claim a behavioral pass.

The current worktree scan produced 313 production Go files, 1,273 WARN findings, and 0 ERROR findings after the cross-package type check. The earlier audit's 20 SCAN-012 errors remain preserved in `docs/audits/2026-09-24/09-verification.md` and its evidence log. The current scanner's 0 SCAN-012 errors means the in-scope core interfaces had concrete internal production method-set implementations in this worktree. It does not assert that those implementations are default-constructed or behaviorally safe. The scan also uses the authoritative rule's `internal/core` scope; old AST hits in other packages are outside that rule's stated scope.

At the intermediate one-package stage, the only SCAN-012 error was `internal/core/hitl.go:43 HITLQuestionEmitter`: the adapter in core implements the tools-side contract, while the actual core emitter is supplied by `internal/cli/cliHITLEmitter`. The updated internal-tree search recognizes the latter by type identity. No empty adapter, inline suppression, severity change, or rule-file change was used.

## Test execution gate

`internal/verify/cmd/test-gate` accepts package patterns through repeated `-package` flags and Go test options through repeated `-go-arg` flags. It invokes the configured Go command as `go test -json`, parses the event stream, and fails if no test function started or none passed. It includes Go's original no-test output and a count/command diagnostic, fails on malformed event JSON, preserves the child `go test` exit status, and uses a 15-minute default timeout (configurable). A separate addendum proposes using it for full tests, coverage checks, helper-focused test targets, and E2E tests.

`test-log` and `test-leak` continue to select only the existing log-capture and goroutine-checker helper tests under `internal/verify/...`. The gate ensures selected helpers really run; these targets do not establish full Runtime lifecycle leak coverage. TUI `NO_COLOR` test isolation remains assigned to the TUI lane.

## Dependency and proposed protected changes

`golang.org/x/tools v0.38.0` is pinned for `go/packages`; upstream's tagged `go.mod` requires Go 1.24.0, while this project remains on Go 1.24.2. `go.mod`'s minimum version was not raised. No scanner rules, CI files, Makefile, or task ledger files were modified in this worktree.

`protected-changes.patch` is the already-submitted scanner/coverage proposal and must remain byte-for-byte unchanged while its approval is pending. `protected-changes-test-gate.patch` is an independent sequential addendum generated against the first proposal's output. Both proposals were applied in order only to a temporary copy for patch validation; the protected originals were not touched.
