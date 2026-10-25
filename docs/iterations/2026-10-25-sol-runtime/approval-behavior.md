# Effective Bash approval behavior

Date: 2026-10-25. This is a read-only source clarification following the real TUI observation reported by root. No approval implementation or policy rule was changed for this explanation.

In the interactive TUI's default ask mode, Bash is an approval request. Root reported that A06 displayed approval for a harmless `printf`, and that explicitly approving it produced the expected output. That observation agrees with the effective prepared-dispatch policy. It must not be reported as a hard policy denial.

The misleading static reading stops too early in [assembleApproval](../../../internal/cli/assemble.go). That function initially creates `SafetyPolicyClassifier([]string{"bash"})`. When an approval channel is present, it also installs `AuditResolver(NewDefaultPermissionModeResolver())` through `WithPermissionModeResolver`.

[NewApprovalMiddleware](../../../internal/approval/middleware.go) chooses between two construction paths. With a resolver, it fills `preparedClassifiers` from `resolver.Resolve(mode)` for each permission mode. Without a resolver, it uses the supplied static classifier for the default mode. It does not merge the static classifier's forbidden-tool set into the resolved classifier.

[DefaultPermissionModeResolver.Resolve](../../../internal/approval/resolver.go) returns `NewSafetyPolicyClassifier(nil)` for the default mode. [AuditResolver](../../../internal/approval/classifier.go) decorates that classifier with audit recording; it does not restore the earlier forbidden list. [PreparedPolicy](../../../internal/approval/prepared_policy.go) consults `preparedClassifiers[mode]`, so Bash reaches the classifier's ordinary `Ask` result and then the interactive callback. An operator's explicit approval can authorize that exact prepared call, subject to the existing trust, generation and dispatch checks.

| Actual assembly path | Effective default classifier | Bash behavior |
| --- | --- | --- |
| Interactive TUI ask with an approval channel and the default resolver | Audited safety classifier with an empty forbidden set | Ask; an explicit operator decision determines execution |
| Non-TUI ask without an approval channel/resolver | Supplied audited safety classifier with Bash forbidden | Policy denial before executor entry |
| CLI auto | Supplied allow-all classifier and automatic approval | Existing auto approval path |
| CLI deny | Supplied deny-all classifier | Policy denial |

The live matrix should retain the distinction between an explicit user denial, an approval dismissed by cancellation, an actual classifier denial, and an approved execution. The A06 description should identify the observed ask-and-approve behavior, while preserving the fact that its earlier static hard-denial expectation was disproved. Root owns the live evidence and final case counts; this note establishes the implementation path and does not claim to have inspected personal audit or history content.

## New candidate: parity correction

The table above describes the original released candidate. Its TUI/non-TUI difference conflicts with the user's requirement for consistent UI behavior: display transport should not change the meaning of the same configuration and `--approve ask` option. Retaining the difference merely because it predates this iteration would leave that requirement unmet.

Root authorized the correction at the CLI Assembly's default ask wiring, and its production diff passed independent source review. The new candidate uses one default permission resolver, decorated for audit, for both TUI and stdio ask. The base classifier exposed through the assembly registry comes from that same resolver's default policy. The presentation transport chooses the callback. Explicit auto and deny branches remain unchanged. No default resolver is installed unconditionally inside `NewApprovalMiddleware`; existing caller-supplied direct classifiers and explicitly supplied custom resolvers retain their documented behavior. No new generalized policy interface was added. Its final 1,253-Go-file freeze, release builds and original verification/E2E gates are complete; root separately owns the new-candidate live UI/stdio evidence.

The compatibility change is deliberate and bounded: interactive stdio/headless ask changes Bash from an unconditional policy refusal to a request requiring an affirmative operator decision. A non-TTY input file or pipe is still denied without consuming an embedded `a` or treating it as human authorization. Bash never becomes implicitly allowed. Missing input, EOF, cancellation and a negative answer remain fail-closed. Plan restrictions, trusted registration/effect checks, generation fences, project trust, approval cache keys, audit persistence and durable completion preflight retain their existing contracts.

Minimum executable acceptance criteria:

- For the same harmless Bash call in ask mode, TUI-channel and stdio callbacks both observe an ask proposal. An explicit yes executes once; no, EOF and cancellation execute zero times and do not mint a positive grant.
- Auto and deny retain their explicit behavior with a channel supplied or absent: auto permits under its existing contract; deny never executes. The assembly still ignores an approval channel outside ask.
- A lower-level custom direct deny classifier stays effective without a resolver, and an explicitly supplied custom deny resolver stays effective with one. The assembly change does not modify middleware-wide fallback behavior.
- Ordinary tools reusing a read-only name do not gain trusted read exemptions. Project revocation, Plan restrictions and stale fences still prevent entry, and audit/receipt records preserve whether denial came from policy, the user or cancellation.
- New production bytes form a separate candidate. The old candidate's real-model matrix remains evidence for those original bytes. Validate the changed ask behavior through the newly built TUI and stdio/headless paths and verify another submission on the same physical session before attributing parity to the new candidate.

## Cancellation must also preserve authority parity

Review of the original stdio callback found that it ignored its context. If cancellation happened while input was pending, a later `a` could still return an always-allow choice and leave a positive grant, even though prepared dispatch rejected the already canceled invocation. The TUI callback already checks cancellation when accepting a response. Blocking the canceled invocation alone therefore does not prove that cancellation minted no future grant.

The bounded correction is to check the context before reading and immediately after each stdio read returns, before interpreting `y`/`a` or assigning a read-error/user-denial category. If cancellation won, return denial with an empty choice and the context error. The required test cancels a genuinely pending prompt, then sends a late `a` to release and join the read; a fresh context issuing the same approval key must prompt again and can be denied without execution. A pre-canceled-context test alone is insufficient.

This correction does not make an ordinary caller-owned blocking reader interruptible. No detached reader goroutine or forced closure of stdin should be introduced. Request authority and joining the input reader remain separate obligations: a caller or test must release the read, and the returned input must not authorize an operation after cancellation won. Accepting a valid choice while its context was still current remains a distinct ordering from receiving a choice after cancellation.

## Audit labels and physical session identity

Approval audit `session_id` is the Assembly's configured logical label. In `newAssembleState`, it is `config.Session.ID` when set and otherwise the configured agent name, whose default is `main`. `assembleApproval` passes this value to the audit classifier and resolver. It is not automatically derived from a trace ID or the durable physical session ID. The [Assembly session fields](../../../internal/cli/assemble.go) explicitly keep `SessionID` as that logical label and expose `PhysicalSessionID` separately from the prepared durable snapshot.

Consequently, a cut can correctly contain many approval records sharing one audit label while that label differs from the physical SID. Those records cannot establish physical-session continuity by themselves. Use the durable session, receipt and checkpoint identities for that claim; do not rewrite audit labels to make them match.

Only audit `tool_outcome` records currently carry a call reference: `tool_call_id` is `sha256:` followed by the SHA-256 of the captured original tool-call ID, as defined by [prepared terminal observation](../../../internal/tools/prepared.go). A canonical receipt's raw call ID can be hashed for this cross-reference. Classifier-proposal and approval-resolution rows omit that call reference, and [AuditEntry](../../../internal/approval/audit_trail.go) has no run ID, action ID or prepared-invocation ID. Reusing an original call ID across runs also reuses its fingerprint. The fingerprint is therefore useful within an independently established occurrence/capture scope and is not a globally unique physical-session association.
