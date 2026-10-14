# R4 Policy Contract Review

Status: **approved for approval-builder handoff**. No policy-contract blocker remains for implementation to start. This is design review only; it does not approve or establish implementation completion.

## Reviewed snapshot

| Artifact | SHA256 / revision |
|---|---|
| Source tree | `cac5a86f717fc012f6adfa47f3cf004cb2e3e454` |
| `r4-policy-design.md` | `2863a863510a8553ac57f0dc60dee02c2f332d5a5f338a36b78e72de806782d6` |
| `r4-prepared-dispatch-design.md` | `3dff4e89b91bc0aa170638020d2d16cde0f9c5899208d97c3a94cbe07b7a6724` |
| `r4-tool-inventory.md` | `884299758b7365fee93966da74cc7cbcf0a814c00d5da732c48ea556d5543767` |

## Review

The policy correctly requires exactly legal `Allow` to reach an executor, rejects unknown permission modes/resolver results and missing identity, and keeps Plan restrictions independent of `auto_full` (`r4-policy-design.md:25-29`). This closes a real current source gap: `ApprovalMiddleware.WrapToolCall` rejects only `Deny`, while `Classification.String` maps unrecognized values to `allow` (`internal/approval/middleware.go:143-160`, `internal/approval/classification.go:20-37`). Unknown `PermissionMode` currently falls through to the default safety classifier and `PermissionMode.String` reports `default`; the design changes this to explicit fail-closed validation (`internal/approval/permission.go:6-27`, `internal/approval/resolver.go:35-46`).

Allow Once is explicitly non-reusable. Always Allow is scoped to the captured trusted executor identity and registration generation, with secret-free identity data; the permit for the current call remains one-use (`r4-policy-design.md:19-27`, `r4-prepared-dispatch-design.md:39-48,72-82`). The existing callback path returns a one-call Allow with `cacheDecision=false`, while Always Allow writes reusable state; current session/store/cache keys are only mode, tool name, and argument hash (`internal/approval/middleware.go:130-140,191-203,206-240`; `internal/approval/callback.go:281-294`). The builder must make **all reusable decision lookups and writes**, including the session and `ApprovalStore` memoization, use the same identity/generation-aware snapshot; fixing only the explicit Always Allow cache would leave the documented same-name replacement bug in the other caches. The prepared design supplies the snapshot and says policy/cache writers share the final fence; it does not require a competing callback or permit type.

The approval wait and final dispatch sequence is implementable as designed: snapshot identity/effects/schema/mode/Plan generations, evaluate policy without holding locks through callback, then atomically revalidate and consume the one-use permit at direct dispatch or queue dequeue. Registration replacement, approval mode/cache updates, and Plan transitions publish under the same narrow fence (or a fixed lock order); the executor runs after that fence is released. The companion assigns this path to the prepared-dispatch and approval builders and includes the relevant race cases (`r4-prepared-dispatch-design.md:39-48,68-88,119-131`). This handles a mode downgrade, entry into Plan Mode, or registration replacement during a prompt by rejecting or re-evaluating the stale request before executor entry.

Control effects are not treated as blanket Plan Mode permission. The current source and tests provide a concrete compatibility matrix for implementation: under active Plan Mode, the existing controller allows `ask_user`, `tool_search`, `task_get/list`, `goal_get/list`, `enter_plan_mode`, and `exit_plan_mode`; it blocks `todo_write`, task/goal create/update, and `dispatch_subagent` (`internal/core/plan_mode.go:29-40,85-99`; `internal/core/plan_mode_test.go:47-82`). The policy requires each control capability to retain an explicit rule and requires unknown/composite tools to remain denied; the tool inventory supplies their distinct proposed effects (`r4-policy-design.md:15-17,29`; `r4-tool-inventory.md`, “Assembly 额外工具”). Preserve and test this existing per-tool behavior rather than permitting every `control` effect. The current Plan controller is name-based; R4 must bind the decision to the trusted registration/effect snapshot so same-name replacement cannot inherit the old exception.

Subagent inheritance is also implementable: CLI assembly passes the already wrapped shared `s.tr` to `NewRealSubAgentFactory`, and the child applies only a filtered view over that same registry (`internal/cli/assemble.go:1190-1200`; `internal/core/real_subagent_runner.go:94-108,199-240`). Thus child calls remain subject to the same approval and Plan wrappers; a child cannot gain authority by constructing a separate registry. Keep an active-Plan attempted subagent dispatch in the builder's tests.

D-01 remains an unresolved product risk, not a prerequisite for starting this builder. Current `PlanModeController` and tests allow the model to call `exit_plan_mode` and immediately deactivate Plan Mode. The design records this existing behavior as the R0 baseline and defers any user-confirmation change until the product decision arrives (`internal/core/plan_mode.go:32-40`; `internal/tools/plan_mode_tools.go:75-96`; `r4-policy-design.md:31`; `r4-prepared-dispatch-design.md:133`). Do not implement the proposed confirmation model while D-01 is unanswered; retain and document the current model-exit behavior.

No full test suite was run; this was a source-and-design review only.
