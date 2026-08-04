package tools

import (
	"context"
	"fmt"
	"log/slog"
)

// PlanModeController is the minimal contract the plan-mode tools depend on. It
// is satisfied by core.DefaultPlanModeController via structural typing, so the
// tools package does not need to import core (which would create an import
// cycle, since core already imports tools).
//
//nolint:scan012 // consumer-side interface; default impl in core package
type PlanModeController interface {
	Enter(ctx context.Context, reason string) error
	Exit(ctx context.Context, summary string) error
	IsActive() bool
}

// EnterPlanModeTool activates plan mode on the bound controller. While plan
// mode is active, write tools are blocked.
type EnterPlanModeTool struct {
	controller PlanModeController
}

var _ ToolDefinition = (*EnterPlanModeTool)(nil)

// NewEnterPlanModeTool returns an EnterPlanModeTool backed by the given
// controller.
func NewEnterPlanModeTool(controller PlanModeController) *EnterPlanModeTool {
	return &EnterPlanModeTool{controller: controller}
}

// Name returns the tool name.
func (t *EnterPlanModeTool) Name() string { return "enter_plan_mode" }

// Description returns a brief description of the tool.
func (t *EnterPlanModeTool) Description() string {
	return "enter_plan_mode: activates plan mode so the agent researches and proposes a plan without executing write tools. Args: reason (string)."
}

// Execute activates plan mode. An optional "reason" argument is read from the
// call args.
func (t *EnterPlanModeTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	reason := ""
	if v, ok := call.Args["reason"].(string); ok {
		reason = v
	}
	slog.Debug("tools.enter_plan_mode.execute", "reason", reason)
	if err := t.controller.Enter(ctx, reason); err != nil {
		return nil, fmt.Errorf("enter_plan_mode: %w", err)
	}
	return &ToolResult{
		Output:     fmt.Sprintf("plan mode activated: %s", reason),
		ToolCallID: call.ID,
		Metadata:   map[string]any{"plan_mode": true, "reason": reason},
	}, nil
}

// ExitPlanModeTool deactivates plan mode on the bound controller, allowing
// write tools to execute again.
type ExitPlanModeTool struct {
	controller PlanModeController
}

var _ ToolDefinition = (*ExitPlanModeTool)(nil)

// NewExitPlanModeTool returns an ExitPlanModeTool backed by the given
// controller.
func NewExitPlanModeTool(controller PlanModeController) *ExitPlanModeTool {
	return &ExitPlanModeTool{controller: controller}
}

// Name returns the tool name.
func (t *ExitPlanModeTool) Name() string { return "exit_plan_mode" }

// Description returns a brief description of the tool.
func (t *ExitPlanModeTool) Description() string {
	return "exit_plan_mode: deactivates plan mode so write tools may execute again. Args: summary (string)."
}

// Execute deactivates plan mode. An optional "summary" argument is read from
// the call args.
func (t *ExitPlanModeTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	summary := ""
	if v, ok := call.Args["summary"].(string); ok {
		summary = v
	}
	slog.Debug("tools.exit_plan_mode.execute", "summary", summary)
	if err := t.controller.Exit(ctx, summary); err != nil {
		return nil, fmt.Errorf("exit_plan_mode: %w", err)
	}
	return &ToolResult{
		Output:     fmt.Sprintf("plan mode deactivated: %s", summary),
		ToolCallID: call.ID,
		Metadata:   map[string]any{"plan_mode": false, "summary": summary},
	}, nil
}
