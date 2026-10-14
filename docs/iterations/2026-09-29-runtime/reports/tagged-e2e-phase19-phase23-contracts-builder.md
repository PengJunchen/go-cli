# Tagged Phase19 and Phase23 E2E contract repair

**Baseline:** `8f0c96f0ef3e3eb917cbc5b1992d7e4b99d26bf1`
**Branch:** `fix/tagged-e2e-phase19-phase23-contracts`

## Finding and change

The five focused failures came from fixtures that no longer matched current dispatch and lifecycle contracts. Phase19 passed an opaque legacy approval wrapper into prepared dispatch, expected model-facing `enter_plan_mode` and `exit_plan_mode` tools, and treated an unverified `read` name as allowed in Plan Mode. Phase23 attempted to queue an inbox message while its sub-agent was idle.

The Phase19 fixtures now assert that prepared dispatch rejects opaque wrappers before invoking an approval callback or tool, while retaining direct sandbox checks for destructive and out-of-scope commands. The wiring fixture confirms model-facing plan controls remain absent while the exposed controller still changes Plan Mode state. The resolver fixture now uses configured prepared policy and asserts `PermissionPlan` denies a write without executing it. The goal fixture checks an explicitly allowlisted read-only tool name. The Phase23 fixture now verifies idle `Send` fails without recording a message, then sends during a deterministically held running state and asserts that the follow-up is emitted and recorded. No production code changed.

Before edits, the focused tagged race run reproduced all five failures on the assigned baseline.

## Validation

- `rtk go test -race -tags=e2e ./tests/e2e_20260802 -run 'TestE2E_Phase19_(ApprovalCallbackWithSandbox|GoalManagementWithPlanMode)|TestET_Phase19_AC[23]|TestET_Phase23_InboxConsumption' -count=5` — 25 test executions passed.
- `rtk go test -race ./internal/cli -run '^TestAutoFullModelCannotChangePlanModeButUserSlashCan$' -count=5` — passed (5 executions); confirms model attempts are rejected and user `/plan` still works.
- `rtk git diff --check` — passed.

This repairs only the assigned Phase19 and Phase23 E2E contracts. The full tagged E2E gate was not run and is not claimed to pass.
