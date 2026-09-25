# R0 Test Gate Wiring Review

**Review status:** Wiring reviewed; one source-level timeout cleanup gap remains open.  
**Review snapshot:** `84324fe7a55ea852583f5082fe5d76ac0dd0608a`  
**Proposal:** `protected-changes-test-gate.patch` (`d249b64cf452cca6467fdcab7253af47fddb0b8fea6672f98942350a3f9d4bf2`).  
**Patch status:** Not applied. No protected Makefile or workflow file was changed during this review.

## Scope and conclusion

I checked the proposed Makefile/CI/PR hunks against the existing `test-gate` and `gatecapture` implementation, the proposal notes, and the current workflow paths. The command wiring is internally consistent: `test`, coverage targets, and the two verification targets pass test flags as repeatable `-go-arg` values and package patterns as explicit `-package` values; CI uploads the file emitted by `make test-cov`; PR checks call the same `make test` target.

The evidence gate handles nonzero Go exit codes, malformed/incomplete event streams, no test files, zero matching tests, and all-skipped test selections as failures. A mixed selection may contain packages with no matching tests or skipped tests, provided at least one selected test/example passes; the wrapper deliberately aggregates evidence across package patterns rather than requiring every package to contain a test. This matches the proposal note and is verified by the focused `test-log` and `test-leak` commands, which emit “no tests to run” from nonmatching packages yet pass with 10 and 6 selected functions respectively.

The wiring itself has no confirmed command-argument or artifact-path defect. However, the gate’s timeout returns while descendant test processes may still be alive (R0GATE-01 below). The proposal must not describe the timeout as process-tree cleanup; this source-level lifecycle issue should be fixed or explicitly accepted before using the gate as a runaway-process containment mechanism.

## Findings

### R0GATE-01 — Timeout kills the direct command but can leave test descendants running

**Severity:** P2 operational lifecycle gap.  
**Evidence:** Confirmed by a local reproduction on the reviewed snapshot.

`test-gate` creates a `CommandContext` for the configured Go executable and returns status 124 when its context deadline expires ([main.go](/Users/pengjunchen/Documents/github/go-cli/internal/verify/cmd/test-gate/main.go:108), [main.go](/Users/pengjunchen/Documents/github/go-cli/internal/verify/cmd/test-gate/main.go:148)). `gatecapture.Run`'s cancellation callback invokes `cmd.Cancel` or `cmd.Process.Kill`, both of which target the direct child only ([run.go](/Users/pengjunchen/Documents/github/go-cli/internal/verify/cmd/gatecapture/run.go:43)). A Go command can have compiler and test-binary descendants; terminating the direct `go test` process does not itself establish those children have stopped.

Reproduction: build the gate binary, pass a temporary executable as `-go-command`; the executable starts `sleep 30`, writes its PID, then waits. Invoke the gate with `-timeout=200ms`. It returned exit 124 after 0.707 seconds, while `kill(pid, 0)` still succeeded (`alive_after_gate_return=True`). The temporary child was then terminated by the probe. The existing `TestRunReturnsWithinWaitDelayWhenDescendantHoldsPipes` also manually kills its pipe-holding descendant after asserting bounded return ([run_test.go](/Users/pengjunchen/Documents/github/go-cli/internal/verify/cmd/gatecapture/run_test.go:74)); it validates bounded waiting, not descendant cleanup.

**Impact:** A timed-out local `make test` may return while a test worker or helper continues consuming CPU, holding resources, or performing test side effects. CI runner teardown limits persistence after a job, but does not make the gate a process-tree supervisor.

**Minimal follow-up:** On supported Darwin/Linux hosts, start the child in an owned process group and have cancellation/output-overflow kill that group, then add a regression asserting the descendant is gone after timeout and overflow. On other operating systems, provide an equivalent process-tree mechanism (for example, a Windows Job Object) or fail closed when the required cleanup guarantee is unavailable. Keep this source fix separate from the protected Makefile/workflow proposal; do not claim broader cross-platform support until tested.

### R0GATE-02 — `test-cov-check` is not declared phony

**Severity:** P3 Makefile reliability note; pre-existing, not introduced by the proposed hunk.

The Makefile's `.PHONY` declaration lists `test` and `test-cov` but omits `test-cov-check` ([Makefile](/Users/pengjunchen/Documents/github/go-cli/Makefile:1), [Makefile](/Users/pengjunchen/Documents/github/go-cli/Makefile:27)). If a filesystem entry named `test-cov-check` exists at the repository root, `make test-cov-check` may consider the target up to date and skip the coverage gate. There is no such collision in the reviewed checkout, so current CI is not affected. Add the target to `.PHONY` when the protected Makefile change is next authorized; no extra test-gate framework is needed.

## Wiring and behavior checks

- `git apply --check docs/iterations/2026-09-25-runtime/protected-changes-test-gate.patch` passed; the proposal remains unapplied.
- `make test-cov` creates `$(COVERAGE_DIR)` and writes `$(COVERAGE_DIR)/coverage.out`; the default is `./coverage`, matching the proposed CI artifact path `coverage/coverage.out`.
- The CI test job switches to `make test-cov`, retaining race detection and atomic coverage. The PR workflow switches to `make test`, retaining race detection and count-one behavior through the same Make target.
- The E2E hunk passes `-tags=e2e` as one `go test` argument and keeps `./tests/...` as an explicit package argument. It is not claimed as executed because this review did not run the Docker-dependent E2E suite.
- `test-cov-check` no longer redirects all `go test` stderr to `/dev/null`, so failures retain the gate's diagnostic output.
- `test-log` and `test-leak` retain their existing filters. These targets prove only that the selected helper tests ran; they do not prove repository-wide goroutine-leak coverage.

## Validation

Commands run on the review snapshot:

```text
rtk git apply --check docs/iterations/2026-09-25-runtime/protected-changes-test-gate.patch
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 ./internal/verify/cmd/test-gate ./internal/verify/cmd/gatecapture
rtk go run ./internal/verify/cmd/test-gate -go-command go -go-arg=-race -go-arg=-count=1 -package=./internal/verify/cmd/test-gate -package=./internal/verify/cmd/gatecapture
rtk go run ./internal/verify/cmd/test-gate -go-command go -go-arg=-race -go-arg=-count=1 -go-arg=-run=TestLogCapturer -package=./internal/verify/...
rtk go run ./internal/verify/cmd/test-gate -go-command go -go-arg=-race -go-arg=-count=1 '-go-arg=-run=TestAssertNoGoroutineLeak|TestGoLeakChecker' -package=./internal/verify/...
```

The package race suite and direct gate run both passed. The gate invocation over its own package and `gatecapture` reported 28 passing functions across two packages. The `test-log` and `test-leak` equivalents reported 10 and 6 passing functions across five packages, respectively; nonmatching packages emitted Go's expected “no tests to run” notices. A disposable module with one skipped test returned 1 with `no test or example passed`; a module with one passing and one skipped test returned 0 and reported both counts. The timeout descendant reproduction returned 124 and confirmed the child remained alive until the probe explicitly stopped it.
