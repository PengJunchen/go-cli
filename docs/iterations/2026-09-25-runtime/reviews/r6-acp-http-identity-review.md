# R6 ACP HTTP identity implementation review

**Reviewed source:** `2279ea2f7f56533926c84578c457a79bcc204a7a` (`codex/runtime-r6-acp-http-identity`).

**Verdict:** reject this slice pending two fixes. The authenticated HTTP session lookup is session-ID and principal scoped, but the live event bridge reintroduces the display alias as a dispatch key. A second confirmed issue accepts ambiguous repeated profile headers.

## Blocking findings

### R6HTTP-01 — Same display alias cross-routes concurrent dispatch events

**Severity:** P1, cross-session event disclosure and event loss.

`/send` validates the opaque session ID and principal, then replaces the message sender with the stored display alias in [`http_server.go`](../../../../internal/acp/http_server.go:551). [`CoreHandler.handleMessage`](../../../../internal/acp/core_handler.go:435) constructs `task.ID` as `acp-<sender alias>`, stores the session under that key, and unconditionally deletes the key when that dispatch returns (lines 443–447). The event bridge uses only this `taskID` lookup to choose the destination session (lines 391–394). Meanwhile, the server intentionally permits two distinct sessions to use the same alias; the existing test asserts this at [`http_profile_identity_test.go`](../../../../internal/acp/http_profile_identity_test.go:269).

The temporary probe used two real HTTP `/connect` sessions for the same principal and alias, then held two `/send` dispatches open through a real `DefaultSubagentDispatcher`. The first dispatch’s event appeared in the second session’s event history. Completing the first dispatch deleted the shared map key while the second was still running, so the second dispatch’s next event disappeared. This reproduced in five consecutive race-test runs. Probe source: [`r6-http-identity-probes.go.txt`](../evidence/r6-http-identity-probes.go.txt); combined race output: [`r6-http-identity-target-race.txt`](../evidence/r6-http-identity-target-race.txt).

Use a unique dispatch/task key and bind that exact invocation to its originating session. Cleanup must remove only the binding installed by that invocation. Keep same-alias sessions valid; rejecting a second alias would mask the routing bug and break the documented display-label semantics. Acceptance must send overlapping work through two same-alias sessions, verify each `/events` stream receives only its own events, and verify completion/cancellation of one request does not remove the other request’s route.

### R6HTTP-02 — Repeated profile header values are accepted as an unambiguous profile

**Severity:** P2, strict profile-selection contract blocker.

[`profileMiddleware`](../../../../internal/acp/http_server.go:204) checks `Header.Get` only. With two `X-Go-CLI-ACP-Profile` values—first the supported profile, then an unsupported value—Go returns the first value and the request proceeds. A real HTTP `/connect` probe confirmed that the server creates a session and returns 200 for that ambiguous selector. The probe is preserved alongside R6HTTP-01.

Require exactly one profile header value and require that value to equal `go-cli-acp-json/1`; reject zero, multiple, empty, and unsupported values. Add tests for both orderings of supported/unsupported duplicates on every route (or a shared middleware test plus route-side-effect assertions). The existing missing-header coverage remains useful but does not exercise duplicate values.

## Verified behavior in this slice

- The route middleware authenticates before profile parsing and session lookup. Wrong credentials return 401 before a missing profile or unknown session can reveal lookup state.
- `/connect` requires the exact envelope version and allocates 32 bytes from `crypto/rand`; the resulting 64-hex-character ID is stored with the authenticated principal and alias. `/send`, `/stream`, `/events`, and `/disconnect` check the principal/session binding; a forged alias is rejected and `send` does not create a session.
- A single conflicting non-empty `/send` envelope version is rejected. Connect ID generation failure returns a generic error without exposing the underlying reader error or identity data in logs.
- Per-session replay/live event delivery remains atomic through `Session.SubscribeAfter`; the targeted cutover, future-cursor, retention-gap, and auth-scoped event tests passed. This finding concerns dispatch-to-session attribution before that correct session-local projection.
- The transport remains the repository’s private `go-cli-acp-json/1` JSON/HTTP profile. This review does not claim official ACP or gRPC compatibility.

## Validation

Confirmed that the following test names are present in `./internal/acp`: `TestHTTPServer_RequiresProfileOnEveryRoute`, `TestHTTPServer_ConnectRequiresExactEnvelopeProfile`, `TestHTTPServer_SendWithoutSessionDoesNotCreateOne`, `TestHTTPServer_RoutesRejectConflictingEnvelopeProfile`, `TestHTTPServer_ConnectRandomFailureIsVisibleAndDoesNotPanic`, `TestHTTPServer_DoesNotLogCredentialOrDisplayIdentity`, `TestHTTPServer_AuthPrecedesProfileAndSessionLookup`, `TestHTTPServer_OpaquePrincipalBoundSessionsAndAliases`, `TestHTTPServer_AuthEventsBoundToSubject`, the five `TestSessionSubscribeAfter*` contract tests, and the two temporary `TestReviewHTTP*` probes.

Ran those 16 tests with:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/acp -run '^(TestHTTPServer_(RequiresProfileOnEveryRoute|ConnectRequiresExactEnvelopeProfile|SendWithoutSessionDoesNotCreateOne|RoutesRejectConflictingEnvelopeProfile|ConnectRandomFailureIsVisibleAndDoesNotPanic|DoesNotLogCredentialOrDisplayIdentity|AuthPrecedesProfileAndSessionLookup|OpaquePrincipalBoundSessionsAndAliases|AuthEventsBoundToSubject)|TestSessionSubscribeAfter(AtomicReplayLiveCutover|CutoverPublishesNextEntryOnlyLive|RejectsFutureCursorWithoutRegistering|FutureCursorCheckSharesPublishLock|ReportsRetentionGapAndPreservesIDs)|TestReviewHTTP(SameAliasRoutesEventsToWrongSession|DuplicateProfileHeaderAccepted))$' -count=2
```

The command passed under the race detector. The two temporary probes are written to pass only when the current behavior is observed; their passing result records the reproductions, not acceptance. No full repository suite, official ACP client, real durable R2/R4 session, or process-level `serve` workflow was tested in this bounded review.

## Contract alignment

The approved R6 design requires the principal and opaque session ID to scope each request, explicitly says the sender label is not authority, and requires every dispatch’s task ID, session ID, and principal to remain attached to its events ([`r6-entrypoints-design.md`](../r6-entrypoints-design.md:43)). R6HTTP-01 shows that the last association is not maintained in the actual default dispatcher bridge. The local session replay/live cutover is sound, but it cannot correct an event already assigned to the wrong session.

## Fixed-snapshot follow-up

**Reviewed source:** `6359d856163a92a8ecd52cddd1b07185a458c3c6` (parent `2279ea2f7f56533926c84578c457a79bcc204a7a`), independently checked in a detached worktree.

**Verdict:** accept this bounded HTTP identity slice. The two findings above are closed in this snapshot. The original rejection and red reproductions remain as evidence for the parent snapshot; they do not describe the fixed source.

R6HTTP-01 is closed by assigning each `CoreHandler` dispatch a process-unique atomic `acp-dispatch-%d` task ID rather than deriving it from the sender alias ([`core_handler.go`](../../../../internal/acp/core_handler.go:378), [`core_handler.go`](../../../../internal/acp/core_handler.go:455)). The HTTP `/send` path passes the authenticated principal, exact session ID, and exact session pointer in a private context value ([`http_server.go`](../../../../internal/acp/http_server.go:574)); `CoreHandler` accepts that binding only when the pointer and ID match its supplied session, and records it on that dispatch’s route ([`core_handler.go`](../../../../internal/acp/core_handler.go:463)). Event forwarding looks up that unique task ID and verifies the route’s task/session binding before publishing. Deferred cleanup uses `CompareAndDelete` with the exact route pointer, so one dispatch cannot remove another route ([`core_handler.go`](../../../../internal/acp/core_handler.go:477)). Direct non-HTTP `CoreHandler` calls retain compatibility and do not invent an authenticated principal.

The regression tests use the real `DefaultSubagentDispatcher` with two HTTP sessions that share one display alias. They verify each dispatch retains its own principal/session/pointer, that the first event is delivered only to its originating session, and that completing one same-alias dispatch leaves the other route live and able to deliver its event ([`http_profile_identity_test.go`](../../../../internal/acp/http_profile_identity_test.go:396), [`http_profile_identity_test.go`](../../../../internal/acp/http_profile_identity_test.go:452)). A cancellation test also verifies exact cleanup and the non-HTTP compatibility path ([`http_profile_identity_test.go`](../../../../internal/acp/http_profile_identity_test.go:500)). The independent corrected probe source is preserved at [`r6-http-route-fix-probes.go.txt`](../evidence/r6-http-route-fix-probes.go.txt).

R6HTTP-02 is closed by requiring exactly one profile header value and exact equality to `go-cli-acp-json/1` ([`http_server.go`](../../../../internal/acp/http_server.go:204)). The regression test covers all five routes (`/connect`, `/send`, `/stream`, `/events`, `/disconnect`) with supported-then-unsupported duplicates, unsupported-then-supported duplicates, and a comma-combined value; every case returns 400 without creating or removing a session ([`http_profile_identity_test.go`](../../../../internal/acp/http_profile_identity_test.go:350)).

Validation on the fixed snapshot passed:

- Targeted identity/profile/event-cutover tests and the two corrected probes under `-race -count=3` passed with:

  ```text
  rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/acp -run '^(TestHTTPServer_(RequiresProfileOnEveryRoute|ConnectRequiresExactEnvelopeProfile|SendWithoutSessionDoesNotCreateOne|RoutesRejectConflictingEnvelopeProfile|ConnectRandomFailureIsVisibleAndDoesNotPanic|DoesNotLogCredentialOrDisplayIdentity|AuthPrecedesProfileAndSessionLookup|OpaquePrincipalBoundSessionsAndAliases|RejectsAmbiguousProfileHeaderOnEveryRoute|SameAliasDispatchEventsStaySessionScoped|SameAliasDispatchCleanupKeepsConcurrentRoute|AuthEventsBoundToSubject)|TestCoreHandler_CancelingSameAliasDispatchKeepsConcurrentRoute|TestReviewFixed.*|TestSessionSubscribeAfter.*)$' -count=3
  ```

  Output: [`r6-http-route-fix-target-race.txt`](../evidence/r6-http-route-fix-target-race.txt); the matching test-name inventory is [`r6-http-route-fix-test-list.txt`](../evidence/r6-http-route-fix-test-list.txt).
- Full `rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/acp -count=1` passed: [`r6-http-route-fix-full-acp-race.txt`](../evidence/r6-http-route-fix-full-acp-race.txt).
- `rtk env GOMAXPROCS=2 go vet ./internal/acp` passed: [`r6-http-route-fix-vet.txt`](../evidence/r6-http-route-fix-vet.txt).
- `rtk golangci-lint run ./internal/acp` passed: [`r6-http-route-fix-lint.txt`](../evidence/r6-http-route-fix-lint.txt).
- `git diff 2279ea2..HEAD --check` passed.

This acceptance is limited to the private HTTP identity and route-binding slice. It does not claim durable R2 session wiring or official ACP/gRPC compatibility.
