# Protected verification-gate changes

Status: proposed only. No protected source file was edited. The complete patch is in [`protected-changes.patch`](protected-changes.patch), generated in memory with `difflib` from the untouched originals at worktree base `00149001e2350b18dd69d7fa41ab7fcd6ed0241a`.

Before applying the patch, compare the target files with these original Git blob IDs:

| Protected file | Original blob ID |
|---|---|
| `Makefile` | `32a78c9bde2af05f105bd32d771a178bfd9d78f5` |
| `.github/workflows/ci.yml` | `2e886a904d6447928c3c6a1cb9ff9c999cd407df` |
| `.github/workflows/pr-check.yml` | `8e529516c1d436f48f0961c67d2e9badf1c29fb2` |

The proposal makes local test package coverage match CI: `make test` and `make test-cov` target `./...`, including `cmd/cli`. The `scan` target calls the new scanner gate, which preserves scanner process status, rejects malformed or inconsistent JSON, prints findings, and exits non-zero for every ERROR. CI removes both `|| true` paths and the blanket SCAN-012 / test-file SCAN-003 filters, then calls the same `make scan` entry point. The PR workflow gains that scanner gate after its all-package race suite.

This intentionally preserves the audit baseline: raw `make verify` failed under the recorded `NO_COLOR=1` / `TERM=dumb` environment; clearing `NO_COLOR` let the race suite pass but the scanner then reported 20 SCAN-012 ERRORs. The proposal does not claim those are post-fix results and does not suppress them. TUI ANSI test isolation remains with the TUI lane. The scanner gate will still surface any remaining SCAN-012 and SCAN-003 ERRORs for separate evidence-based disposition.

No change is proposed to `.golangci.yml`, `go-cli-infra/rules/`, `go-cli-infra/infra/`, tasks, or milestone status. Rule definitions and severity remain unchanged. The scope is limited to fixing command coverage and making scanner failures observable and consistent across local, CI, and PR checks.

Impact: local `make test` and `make test-cov`, CI scan, and PR checks will cover or gate more packages. Existing packages skipped by the old Makefile may now fail tests. Any scanner ERROR, scanner process error, invalid JSON, or inconsistent report will block `make scan` and all workflows that invoke it. This is the intended effect; a failure requires correcting the source or separately reviewing the rule trigger, not restoring a blanket filter.

Rollback: revert the three protected-file hunks from `protected-changes.patch` as one change. No task-state or rule-file rollback is needed because the proposal makes no such edits.

The proposed gate wrapper uses `golang.org/x/tools/go/packages` only inside SCAN-012's single `internal/core` load. Version `v0.38.0` is pinned; its upstream tagged `go.mod` declares `go 1.24.0`, compatible with this project’s `go 1.24.2` directive ([upstream tagged go.mod](https://go.googlesource.com/tools/+/refs/tags/v0.38.0/go.mod)). Loading is limited to one package per scan invocation and has a 30-second context timeout. Package/type errors and timeouts remain SCAN-012 ERROR findings with their diagnostic location where available; they do not silently pass.
