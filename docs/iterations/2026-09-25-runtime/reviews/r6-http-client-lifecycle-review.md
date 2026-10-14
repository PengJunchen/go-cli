# R6 private ACP HTTP client lifecycle review

**Reviewed snapshot:** `4f3e29944ba234098a186481cd8656cb5c7244a3` (`codex/runtime-r6-http-route-fix`), including `56e36ee` and based on server integration `6359d85`.
**Worktree:** `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r6-http-route-fix`.
**Review scope:** source and lifecycle tests for the `gRPCAdapter` private HTTP client; no source changes or staging. A temporary package-local probe was added, run, and removed.

## Verdict

**Needs follow-up before acceptance.** The client uses the intended private `go-cli-acp-json/1` HTTP profile, explicit bearer configuration, server-returned opaque session IDs, bounded response parsing, redirect blocking, and no send replay or explicit reconnect. Its operation accounting normally keeps connect, sends, stream reads, and response-body closes owned until they return. Two failure paths can nevertheless erase session identity or permit an unsafe retry, and idempotent `Connect` changed behavior for an already-active generation.

## Findings

1. **P1 — A panic during connect can clear an ambiguous remote create and admit a second create.** `runGeneration`’s recovery at [`grpc_adapter.go:282-299`](../../../../internal/acp/grpc_adapter.go#L282) records `errACPWorkerPanic` while connecting. If a `RoundTripper` panics after `/connect` may have reached the peer, no `ErrHTTPConnectOutcomeUnknown` is attached. After local worker join, `cleanupGeneration` sees an empty session ID and a non-unknown error, reaches the nil cleanup result, and `finishCleanup` clears the generation ([`grpc_adapter.go:393-420`](../../../../internal/acp/grpc_adapter.go#L393)). A later `Connect` therefore sends another `/connect`.

   A disposable package-local probe simulated a peer that accepted the first create and then panicked in `RoundTrip`. It observed `Connect` return only `errACPWorkerPanic`, `Disconnect` return nil, and the next `Connect` issue a second create. Treat a panic after the dispatch boundary as an unknown create and retain the fence; only a proven pre-dispatch panic may be classified as a local failure that permits retry. Do not guess a session ID for cleanup.

2. **P1 — Candidate session salvage bypasses the opaque-ID validator for the display alias.** `connectSession` keeps `candidateID` when it is nonempty, within length, and trim-stable ([`grpc_adapter.go:565-581`](../../../../internal/acp/grpc_adapter.go#L565)). The authoritative validator also rejects `sessionID == displayAlias` ([`grpc_adapter.go:635-637`](../../../../internal/acp/grpc_adapter.go#L635)), but the salvage path does not apply that same predicate. With a wrong-profile response containing `session_id` equal to the configured `WithName` alias, the client returns a known protocol error, stores that alias as the generation’s session ID, and sends it to `/disconnect`; successful cleanup clears the fence and admits another create.

   The disposable probe confirmed the `/disconnect` envelope carried `display-alias`. Apply the same `validOpaqueSessionID` predicate before retaining any candidate. An invalid candidate, including an alias, must remain unknown and fenced without issuing `/disconnect` to that value. Add regressions for wrong-profile and malformed JSON responses with alias and other invalid candidates, plus the valid opaque candidate cleanup case.

3. **P2 — Already-active `Connect` now fails when its caller context is canceled.** The new context check precedes the generation-state check ([`grpc_adapter.go:195-213`](../../../../internal/acp/grpc_adapter.go#L195)). The previous implementation returned success when already connected before considering the context; the new implementation returns `ctx.Err()` even though it needs no new dispatch. Keep the canceled-new-generation admission rule, but let an existing active generation’s completed/idempotent result win. Add a regression that first connects, then calls `Connect` with an already-canceled context and expects the existing active result, while retaining the current zero-request/no-generation test for a canceled first connect.

## Cleanup deadline behavior to preserve and document

The first `Disconnect` caller is the cleanup owner: its context is passed into `cleanupGeneration`; later callers only bound their wait. If the first caller’s deadline expires while `operations.Wait()` is waiting for a stream-body close or active send, cleanup joins the local resources, observes the expired owner context, skips the remote `/disconnect`, stores the deadline error, and keeps the generation failed/fenced. It does not start another generation or resend cleanup. A gated-body probe verified that the remote-delete count remains zero after the body is released and joined.

This is a single absolute cleanup-owner budget, not waiter-only cancellation. It is fail-closed locally but can leave the remote session allocated until server-side expiry. Keep this behavior in this slice; state it explicitly in the client lifecycle contract and test both the owner-deadline case and the existing later-waiter-only timeout case. Do not silently replace it with an unbounded or newly invented cleanup budget.

## Verified behavior

- The client sends `X-Go-CLI-ACP-Profile: go-cli-acp-json/1` on its profile requests; `/connect` also carries a versioned JSON envelope. Streaming responses must be `application/x-ndjson`, bounded to 4 MiB total and 1 MiB per frame.
- `WithHTTPBearerToken` is explicit. Redirects are blocked at `http.Client.CheckRedirect`; endpoint userinfo, query, and fragment are rejected. Trace endpoint attributes contain only origin. Error, body-read, body-close, and stream-read panic messages are fixed/sanitized while wrapped causes remain discoverable with `errors.Is`.
- A valid response’s server-issued session ID is used for send, stream, and disconnect, while the client name remains a display alias. Authenticated loopback tests verify the principal comes from the bearer token and the alias stays separate.
- `SendMessage` sends one request and has no pending queue or automatic replay. `Reconnect` returns `ErrReconnectUnsupported`; bounded stream polling retries only the same session. Known 4xx connect denial permits an explicit cleanup and caller retry. Uncertain status, lost response, malformed response with no usable ID, and body-read failure retain the generation fence.
- The generation operation count covers connect and reader work. Sends increment under the same mutex that changes the generation to closing; cleanup waits those operations, including response-body close. Repeated `Disconnect` callers observe one stable cleanup result, and failed cleanup fences new connects.

This is a review of a private profile only; it does not claim official ACP or gRPC interoperability. The stream poll retry policy does not provide replay or exactly-once inbound delivery.

## Verification

Test listing was run before test execution:

```text
rtk env GOMAXPROCS=2 go test -p 2 ./internal/acp -list .
```

The targeted client and response-close race selection passed:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/acp -run '^(TestGRPCAdapter|TestHTTPACP|TestHTTPResponseClose)'
ok github.com/pengjunchen/go-cli/internal/acp 8.488s
```

The full ACP package race suite passed:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/acp
ok github.com/pengjunchen/go-cli/internal/acp 17.177s
```

Vet and lint passed on the reviewed snapshot:

```text
rtk env GOMAXPROCS=2 go vet -p 2 ./internal/acp
rtk golangci-lint run ./internal/acp/...
# golangci-lint: No issues found
```

A temporary review-only `TestReviewProbe*` file exercised the connect-panic replay, first-owner deadline, and malformed-alias cleanup paths under `-race`; it asserted the observed behavior described above and was removed afterward. The worktree contains only this review document as an untracked file; no source was changed or staged.

## Independent delta review — `4e495792ab8a57c6da9c1ded712e447ab2173248`

**Verdict: accepted for the bounded private HTTP lifecycle slice.** I reviewed the fixed source and tests against all three findings above and the cleanup-owner limitation. The earlier rejection remains as historical evidence for `4f3e299`; this section records the delta disposition only.

- A panic after the `/connect` dispatch boundary is now classified as an unknown remote create. The generation remains fenced, no guessed session ID is sent to `/disconnect`, and a subsequent `Connect` does not dispatch a second create. The regression covers the post-dispatch panic path.
- Session-ID salvage now uses the opaque-ID validator. Invalid/display-alias candidates remain unknown and fenced without a DELETE; a valid opaque candidate returned with a wrong profile can still be cleaned up. The corresponding lifecycle cases pass.
- `Connect` checks an existing generation before applying a new-generation caller-context check: an already-active generation is idempotent success with a canceled caller context, while a canceled first connect makes no HTTP request and does not publish a generation.
- Cleanup waiters share the stable operation result and Done channel. A pending result preserves `errors.Is(err, callerContextError)`, and Done is published only after request operations, response-body close, and cleanup owner work have actually joined.
- The first `Disconnect` caller owns the cleanup budget. If that context expires while local operations are still unwinding, cleanup skips remote DELETE, stores the error, and leaves the generation fenced; a remote session may remain allocated until server-side expiry. The lifecycle contract and regression state this limitation explicitly. Later waiter cancellation does not claim cleanup completion.

The seven focused lifecycle regressions passed five race-enabled repetitions, and the full ACP package race suite passed:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=5 ./internal/acp -run '^(TestGRPCAdapterDisconnectWaitsForStreamBodyClose|TestGRPCAdapterDisconnectOwnerDeadlineSkipsRemoteDeleteAndKeepsFence|TestGRPCAdapterCanceledConnectAdmissionDoesNotPublishGeneration|TestGRPCAdapterConnectActiveGenerationWinsCanceledContext|TestGRPCAdapterConnectPanicAfterDispatchIsUnknownAndFenced|TestGRPCAdapterDisconnectCleanupResultIsStable|TestGRPCAdapterWrongProfileCleansOnlyValidatedOpaqueCandidate)$'
ok github.com/pengjunchen/go-cli/internal/acp 1.561s

rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/acp
ok github.com/pengjunchen/go-cli/internal/acp 23.535s
```

No product source was changed or staged during this delta review. Acceptance is limited to this private ACP JSON/HTTP profile and the reviewed host test run; it does not claim official ACP/gRPC interoperability or prove remote teardown after an owner deadline.
