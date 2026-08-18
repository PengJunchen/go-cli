package core

import "fmt"

// NewPlanModeToolInterceptor returns a ToolInterceptor that blocks write tools
// when plan mode is active. When the controller is nil or plan mode is
// inactive, all tool calls are allowed.
//
// This replaces the former PlanModeMiddleware. Plan-mode write blocking now
// flows through the unified PreToolCallEvent / ToolInterceptor mechanism
// instead of a separate agent-level middleware inspecting Submission.Metadata.
func NewPlanModeToolInterceptor(controller PlanModeController) ToolInterceptor {
	return func(toolName, _ string, _ map[string]any) error {
		if controller == nil || !controller.IsActive() {
			return nil
		}
		if controller.ShouldBlockWrite(toolName) {
			return fmt.Errorf("plan mode: write tool %q blocked", toolName)
		}
		return nil
	}
}
