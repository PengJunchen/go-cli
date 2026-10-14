# R6 MCP HTTP operation completion review

**Reviewed snapshot:** `b2187a258c78a23d3f340327c3847fd92c5f0cad`.
**Worktree:** `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r6-mcp-operation-review-b2187` (detached, clean).
**Scope:** The three changed MCP files in this commit: the HTTP adapter and its body-panic and lifecycle tests. No source changes were made.

## Verdict

**Accepted for this narrow completion-signal slice.** `httpOperationPendingError.Done()` exposes the channel for the specific connect/close operation that the waiter left pending. The channel is observational: it signals local operation completion, not whether a remote tool action succeeded or failed.

## Review notes

- The pending-error constructors pass the existing operation's `done` channel. A pending `Disconnect` waiter observes its generation's single `closeOp`; concurrent waiters receive that same channel. A pending `Connect` waiter observes either its own `connectOp` or, when waiting for an old generation to close before reconnecting, that exact `closeOp`.
- `runConnect` stores the result and closes `op.done` only after `performConnect` returns. `runClose` likewise closes `op.done` after `performClose` returns; `performClose` waits for the generation's active work to quiesce before attempting session deletion. The close operations are not recreated to serve `Done()`.
- The channel belongs to its operation/generation. Reconnecting after a completed close does not replace the old channel or make an old pending error observe the new generation.
- `Error()` remains sanitized and fixed, while `Unwrap()` preserves the waiter cancellation/deadline cause and `Is()` preserves `ErrHTTPOperationPending` matching.
- No finding in the reviewed diff. This review does not claim a remote side-effect outcome or a full MCP/Assembly lifecycle closure.

## Verification

The exact target names were listed before execution:

```text
rtk proxy go test -list '^Test(HTTPAdapterJoinsResponseBodyCloseAfterReadPanic|HTTPAdapterDisconnectWaitIsCancelableAndCloseIsJoined|HTTPAdapterCanceledConnectRemainsOwnedUntilActualReturn|HTTPPendingConnectCompletionBelongsToItsGeneration)$' ./internal/mcp
```

All four listed tests passed under the race detector for 10 repetitions:

```text
rtk env GOMAXPROCS=2 rtk proxy go test -race -p 2 ./internal/mcp -run '^(TestHTTPAdapterJoinsResponseBodyCloseAfterReadPanic|TestHTTPAdapterDisconnectWaitIsCancelableAndCloseIsJoined|TestHTTPAdapterCanceledConnectRemainsOwnedUntilActualReturn|TestHTTPPendingConnectCompletionBelongsToItsGeneration)$' -count=10
ok   github.com/pengjunchen/go-cli/internal/mcp 2.239s
```

`git diff --check` passed for the reviewed commit. The full MCP package race suite was not rerun during this independent review; the builder reported it had already passed.
