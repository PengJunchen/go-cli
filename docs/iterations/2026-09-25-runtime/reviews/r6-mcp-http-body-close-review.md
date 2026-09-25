# R6 MCP HTTP response body close review

**Reviewed snapshot:** `6fb31f873c9255e7b8d73788fa17ea0660fc078d` (`fix(mcp): close response body during panic unwind`), based on `6481dfd`. This review is limited to the final response-body-close fix and its interaction with the existing HTTP operation lifecycle; it does not accept the preceding eight HTTP commits as an integrated R6 gate.

**Verdict: accepted; no blocker found.** `HTTPClientAdapter.request` registers one `sync.Once`-guarded `Body.Close` immediately after `Do` returns a response. The normal path invokes it directly after `readRequestResponse` and before any SSE resume GET; the defer invokes the same closure on panic/unwind and is a no-op after normal close. Thus the POST body is closed before resumption, and neither path closes it twice.

The request’s `endHTTPActivityLocked` defer is registered before the body-close defer. Go’s LIFO unwind therefore completes `Body.Close` before decrementing the generation’s active-operation count or closing its quiescent channel. `performClose` waits on that channel before issuing DELETE. A blocking close keeps the operation owned; a caller’s `Disconnect` waiter can return pending at its deadline without falsely completing the close or starting DELETE.

The new regression `TestHTTPAdapterJoinsResponseBodyCloseAfterReadPanic` uses a body whose `Read` panics and whose `Close` blocks on a test gate. While Close is blocked, Disconnect returns both `context.DeadlineExceeded` and `ErrHTTPOperationPending`, DELETE count stays zero, and Close count stays one. Releasing Close lets the original synchronous caller observe its panic; a later Disconnect completes the single DELETE, with the response still closed exactly once.

I also ran a temporary same-package probe against the ordinary SSE-resume path and removed it afterward. It checked the initial POST body’s close count inside the GET RoundTripper (already exactly one before resume), and verified both initial and resumed response bodies closed exactly once. This confirms the explicit-close/deferred-close pair remains ordered and idempotent when resumption occurs.

The listed targeted suite and full package race both passed:

```text
rtk env GOMAXPROCS=2 go test -list '^TestHTTPAdapterJoinsResponseBodyCloseAfterReadPanic$|^TestHTTPAdapterDisconnectWaitIsCancelableAndCloseIsJoined$|^TestHTTPAdapterLifecycleOwnerPanicIsContained$|^TestMCP20250618SSEFramesAndResume$' ./internal/mcp
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/mcp -run '^(TestHTTPAdapterJoinsResponseBodyCloseAfterReadPanic|TestHTTPAdapterDisconnectWaitIsCancelableAndCloseIsJoined|TestHTTPAdapterLifecycleOwnerPanicIsContained|TestMCP20250618SSEFramesAndResume)$' -count=1  # ok, 5.168s
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/mcp -count=1  # ok, 7.894s
```

No product code was changed in the review worktree; the temporary SSE probe was removed. These results validate the body-close delta and local HTTP lifecycle only, not full MCP/ACP interoperability or the preceding unintegrated commits.
