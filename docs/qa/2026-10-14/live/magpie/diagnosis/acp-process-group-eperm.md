# ACP process-group `EPERM` during early-exit close

Captured 2026-10-14 on macOS against source HEAD `508c785dffa06ec25aa36414b51f3afd46d462a7` before the ACP fix. The narrow follow-up was committed as `2c58f995333df9259320d928ab3c295bc776913f` (`2c58f99`). This report records the full-suite failure and fix; it does not claim to identify the operating-system source of `EPERM`.

## Failure and source path

The preserved full `make verify` log, `docs/qa/2026-10-14/live/gates/verify-508c785-race-failed.log`, reports `internal/acp.TestStdioProcessOwnerEarlyExitIsWaitedAndReaped` failing at `stdio_process_owner_test.go:91` with:

```text
Close after early exit: acp: signal stdio process group: operation not permitted
```

The result contained no `ErrStdioProcessCleanupPending`. In the original control flow, `shutdown` adds that error whenever the final group check does not prove absence. Therefore, the reported error is consistent with a later positive group-absence check while the earlier TERM permission error remained joined into `shutdownErr`. The log does not record the group state directly, so that conclusion comes from the source control flow.

`StdioProcessOwner.Start` sets the process-group ID to the child PID and starts `cmd.Wait` independently. The early-exit test waits for child stdout EOF before calling `Close`, so the direct child may already have been reaped. `Close` sends TERM to the numeric group ID; Darwin/Linux preserves any signal error, even if the bounded liveness poll subsequently proves the group absent. The origin of the EPERM itself remains unproven: this evidence cannot distinguish a transient OS/process-group lifecycle race from a reused numeric group ID.

The full verify ran `go test -json -count=1 -race -count=1 ./...`: 8,768 tests passed, 6 skipped, and the one ACP test failed across 28 packages. The preserved summary is `verify-508c785-race-failed.json`; the log SHA-256 is `209e2a1272f3436262179d43cd22914cdf98386c4fa389811e02a01498a19560`.

## Narrow behavior change

`Close` now passes a private per-call process-group operations value. The public path supplies only the platform's real signal and liveness functions; there is no process-wide mutable mock or public success flag. The shutdown helper separates signal errors from group-inspection errors.

A wrapped `syscall.EPERM` signal error is settled only when a subsequent group wait returns `groupGone=true` and there were no group-inspection errors. If the group remains alive or its state is unknown, EPERM and `ErrStdioProcessCleanupPending` remain. Non-EPERM signal errors and every probe error remain visible even if a later probe reports absence. `Join` still requires both confirmed group absence and the direct-child `Wait` channel before closing owned files or running executable cleanup.

## Regression coverage

- A real early-exit child is waited/reaped, and the real process-group probe confirms absence. An injected TERM EPERM no longer makes `Close` fail; `Join` still reports the child's exit status 23 and verifies the child was reaped.
- A real `/bin/sleep` process group is confirmed alive while TERM and KILL return injected EPERM. `Close` and `Join` keep EPERM plus cleanup-pending, and the test kills the child through the real process-group signal path during cleanup.
- An injected group-inspection error plus EPERM keeps both errors and leaves the group result unproved.
- A non-EPERM signal error remains visible even when a probe reports confirmed absence.

The positive test was red before the error-settling change: it observed an actually reaped child and actual group absence, but `Close` still returned the injected EPERM. All four focused regression tests passed after the change.

## Validation

- `go test ./internal/acp -count=1` passed (23.134s).
- `go test -race ./internal/acp -count=1` passed (31.360s).
- `go vet ./internal/acp` and `git diff --check` passed.
- Selected process-heavy `internal/tools` race tests passed with `-count=3` (12.981s). Selected CLI extension/plugin/PTY race tests passed with `-count=1` (6.582s). An exploratory CLI `-count=2` attempt hit the test fixture's Go-plugin-cache behavior (`plugin already loaded`); the same selected tests passed at count 1.
- A full `internal/acp -race -count=20` pressure run was stopped after more than five minutes without a verdict. It is not counted as passing or failing.
- Before this fix, the exact early-exit test passed in isolation once and at `-count=10`, so the prior isolated pass did not rule out the full-suite failure.

No test treats direct-child reaping alone as proof that descendants are gone. No production behavior outside the ACP process-owner files was changed for this diagnosis.
