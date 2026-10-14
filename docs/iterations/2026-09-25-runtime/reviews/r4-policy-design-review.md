# R4 policy design review

Review date: 2026-09-25
Review target: `docs/iterations/2026-09-25-runtime/r4-policy-design.md` at source snapshot `3fe4389`
Review type: independent design review only; no implementation approval.

## Decision

**Approved as an R4 design contract, subject to the documented R1/R3 prerequisites.** I re-reviewed the revised draft. It now specifies generation invalidation during approval, a short dispatch-commit linearization point with a pinned executor, Plan Mode generation revalidation, and a controlled List path. This is not implementation approval. Builders still need to make the identity source unforgeable, preserve the exact resolved handle through wrapper layers, and prove the races with the acceptance tests below. This report does not approve D-01 or implementation work depending on a change to its existing behavior.

## Reviewed contract requirements

1. **Bind authority at a trusted registration boundary.** The contract rejects model-supplied identity and does not trust third-party read claims. Implementation must attach first-party origin/effects in trusted registration code, leave generic extension/MCP/user registration unknown, and keep endpoint/cwd/effects bound to the identity generation. Carry that opaque identity through every nested wrapper without relying on mutable tool Args.

2. **Make resolution and dispatch one explicit protocol.** The revised contract binds definition and identity in one immutable Get snapshot, invalidates an approval when registration or policy generation changes while waiting, and requires a short final commit point that validates generations and pins the executor. After commit, later replacement applies only to future calls. No lock is held over user wait or tool execution. The implementation still needs an atomic resolved handle and a well-defined generation/lock ordering.

3. **Include Plan Mode in the policy snapshot and final gate.** The revised contract snapshots Plan Mode state/generation and rechecks it at dispatch commitment, closing the inactive-check/approval-wait race. Plan Mode remains higher priority than `auto_full`. A Plan Mode transition after commit cannot reverse an action already in flight; R1 owner and UI must preserve that distinction. D-01 remains deferred; this review does not authorize changing `exit_plan_mode` behavior.

4. **Close the executable `List` escape hatch.** The revised contract requires either a fully controlled wrapper preserving required optional interfaces or an inert descriptor; it prefers wrappers when an API split is too broad. This prevents direct execution from bypassing `Get` policy.

5. **Bind cache, callback, audit, and dispatch to one snapshot.** The revised contract includes permission mode/generation, Plan Mode state/generation, classifier, executor identity/registration generation, and canonical args hash, and rejects unknown modes/classifications or missing identity. During implementation, define which policy mutations advance the generation and ensure the callback, cache key, audit record, and final gate all use that same snapshot. Preserve exact effect sets and canonical *effective* arguments after argument preparation.

## Source evidence and test needs

At the reviewed source snapshot, `DefaultToolRegistry.Register` is last-registration-wins; `Get` and `Version` are separate operations; `MiddlewareToolRegistry.Get` wraps execution but `List` delegates raw definitions. `ApprovalMiddleware.WrapToolCall` builds a key from mode/name/args before calling `decide`; `decide` and `record` read the mode/resolver again, and `resolveAsk` may wait on a callback. `PermissionMode.String` and `Classification.String` both map unknown enum values to permissive-looking defaults, while the approval wrapper rejects only `Deny`. These source facts support the draft's reported findings and the snapshot/closed-enum requirements above.

Before implementation acceptance, require barrier-controlled tests for: same-name trusted-to-untrusted replacement invalidating an old cached grant; replacement while a human prompt is pending; policy and Plan Mode generation changes during that prompt; the exact pinned executor at dispatch commitment; a replacement after commitment not swapping the in-flight executor; and direct `List` execution being unable to bypass approval. Include a real CLI/Assembly extension collision using the name `read`, proving it does not receive trusted read effects. Add interface-preservation tests for the chosen wrapper/descriptor strategy.

## Deferred decision

D-01 is still pending the user's product decision. The draft correctly records that R0 preserves current `exit_plan_mode` behavior and that R4 must not present the long-term policy as settled. No approval for a proposed D-01 change is given here.
