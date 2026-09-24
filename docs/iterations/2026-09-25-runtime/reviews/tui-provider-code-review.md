# TUI and provider integration review

Review date: 2026-09-25  
Review targets: `15be450` (TUI approval/HITL lifecycle) and `87ff770` (small-model credential scope).  
Review type: independent source review plus focused race tests; no product files changed.

## TUI approval and HITL lifecycle

Accepted. Each queued interaction has a private monotonic token, so duplicate or missing request IDs cannot make a cancellation target the wrong modal. Approval and `ask_user` requests share one arrival-ordered queue; each request's context watcher exits when the interaction resolves, is canceled, or the run ends. Expired approval contexts are checked both before a modal key is applied and immediately before the response is sent. `TeaApprovalCallback` checks the context again when mapping the response, so an `ApprovalAlwaysAllow` delivered after expiration becomes `ApprovalDeny` plus the context error and cannot reach the reusable approval cache. Closing the approval input channel cancels approval waiters, stops its reader, and leaves unrelated `ask_user` interactions and the TUI running.

The added regression coverage includes `TestCanceledApprovalCannotBeApprovedBeforeCancelMessage`, `TestTeaApprovalCallback_DeniesAlwaysAllowResponseAfterCancellation`, queued cancellation with a duplicate request ID, arrival-order modal handling, and answered-watcher cleanup. I ran the focused race selection for the TUI, approval, and CLI packages; all three passed.

## Small-model credential scope

Accepted. `buildSmallModel` now passes both provider identities to `resolveSmallModelProviderSettings`. The primary key is inherited only for the same normalized provider identity and either an inherited endpoint or an explicitly selected endpoint with the same normalized origin (scheme, hostname, effective port). Path changes do not change origin. An explicit different origin uses only the small model's own key, and a different provider with neither its own endpoint nor key fails before constructing a request. Explicit unauthenticated custom endpoints remain possible. Native Claude/Anthropic/Gemini names still pass through the existing fail-closed resolver and are not redirected to the OpenAI-compatible protocol.

The local HTTP regressions verify same-provider reuse, different-provider fail-closed behavior, no primary-key header at a different origin, same-origin path reuse, small-model-owned credentials, and default-origin normalization. I ran those tests with the expired-approval regressions under `go test -race`; the TUI, approval, and CLI packages passed. No source-level blocker remains in these two integrated slices.
