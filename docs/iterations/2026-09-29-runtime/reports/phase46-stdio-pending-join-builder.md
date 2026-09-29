# Phase 46 stdio pending-join fixture repair

Date: 2026-09-30
Worktree: `go-cli-repair-20260930-tagged-e2e-fixtures`
Branch: `codex/fix-tagged-e2e-phase21-phase22-fixtures-20260930`
Base commit: `01126cee63ef9a6178ca041ce069feb5e7b94597`
Input failure log: `/tmp/go-cli-tagged-e2e-bda13f2.jsonl`

## Finding and change

The old test expected `Disconnect` to return nil while its caller-owned `io.PipeWriter` was blocked because the peer had stopped reading. The ACP contract tracks that real write and reports pending work until the caller releases the pipe. The input failure log shows the test timing out at its five-second success expectation and then detecting the still-running writer and cleanup goroutine; its `require.NoError` also ran before its pipe cleanup.

The E2E fixture now observes the real second write (the disconnect frame) through a thin writer wrapper around the same `io.PipeWriter`. After that call begins, it cancels the disconnect wait and asserts an `acp.StdioPendingError`, `context.Canceled`, unknown delivery, and an open `Done` channel. It then closes the caller-owned peer reader and remaining pipe endpoints and waits for `Done` before the goroutine-leak verifier runs. A deferred cleanup closes every endpoint on all assertion paths and performs a bounded disconnect/join if the normal pending path was not reached. No ACP runtime behavior was changed.

## Verification

The focused lifecycle test passes three consecutive race-enabled runs:

```text
rtk proxy go test -race -count=3 -tags=e2e ./tests -run '^TestE2E_Phase46_StdioAdapterNoGoroutineLeak$' -v
```

Each run observes the actual blocked disconnect-frame write, reports pending delivery, releases the caller-owned pipe, and joins the tracked workers. This verifies the fixture lifecycle rather than weakening the transport ownership or join contract.
