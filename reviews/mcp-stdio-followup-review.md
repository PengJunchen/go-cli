# MCP stdio follow-up review

**Verdict: ACCEPT at HEAD `51ac5d25e691a091d14ada948f13dc1576d6fbce`.** This review covers the isolated worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-mcp-stdio`, including source commits `4c73ad49f070327a48d46619a3efacf2c85dd691`, `81a3eed1848249b14ad8a3d2457b2551ccd3ca04`, and `a4432ee4286ff9b5eb896d01bd7f98e9d70dfbe7`. HEAD adds only the builder report update; the worktree was clean before this review.

The two P2 findings from `mcp-stdio-final-review.md` are fixed. `Connect` checks `procOwner.isStopping()` before taking the connected fast path, joins the exact old owner, and clears the adapter's owner/connection only when the pointer still names that generation. Concurrent callers wait for the in-flight `connectDone`; owner cleanup is elected once and all waiters join the same `done` receipt. The committed real-child regression cancels a silent call, waits for its process receipt, reconnects onto a new owner, and confirms the uncertain call was written exactly once.

Request cancellation exits now use `contextCause(ctx)`. The committed pre-write and post-dispatch tests use a sentinel `WithCancelCause` and assert `errors.Is` for both the sentinel and `context.Canceled`; the post-dispatch case also checks the outcome-unknown marker and exact process receipt. These checks exercise the helper child process for the owned stdio lifecycle.

The response reader charges all consumed frames in a request against the 10 MiB cap, including notifications and unrelated response IDs. The helper tests reject an oversized line, an oversized unterminated fragment, and aggregate ignored frames beyond the budget; a complete response exactly at the limit is accepted. The `CallTool` path does not use the read-only reconnect retry loop, and the cancellation, protocol-limit, recovery, and teardown tests each verify a dispatched tool call is not replayed. Oversize failures retain their typed protocol error and unknown outcome through the unwrap chain.

Borrowed connection ownership remains intact: `WithConnection` marks the connection external, so adapter `Disconnect` does not close it. The full package run includes `TestAdapterDisconnectPreservesExternalConnection`. A blocked read from a caller-owned reader still requires that reader's owner to release it; the API documentation states this constraint, and the byte cap still bounds data accumulated by the adapter.

## Verification

- `rtk env GOMAXPROCS=2 go test -race ./internal/mcp -run '^TestOwnedStdio(RequestPreserves|ConnectRecovers|ResponseBudget)' -count=1 -timeout=180s` — passed.
- `rtk env GOMAXPROCS=2 go test -race ./internal/mcp -count=1 -timeout=180s` — passed.
- `rtk env GOMAXPROCS=2 go test -race ./internal/mcp -run '^TestOwnedStdio' -count=5 -timeout=240s` — passed, including repeated cancellation/disconnect join races and stopped-generation recovery.
- `rtk git diff --check 4c73ad49..HEAD` — passed.

The review did not run CLI production E2E, process-group/orphan behavior, or `make verify`. There is no dedicated test racing two simultaneous `Connect` calls; generation identity and single-join behavior were reviewed in source, and the committed recovery/teardown tests passed repeatedly under the race detector. No source or test files were changed for this review.
