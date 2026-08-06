package core

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// PlanModeMiddleware is an agent-level Middleware that blocks write-tool
// execution while plan mode is active. It wraps an AgentLoop so that, when plan
// mode is on, any submission carrying a write tool call is short-circuited with
// an error event instead of being delegated to the underlying loop.
//
// The middleware inspects the Submission's Metadata for tool-call information
// using the conventional keys "tool_calls" (a list of tool names) and
// "tool_name" (a single tool name). Submissions without these keys are passed
// through so the agent can still read and plan.
type PlanModeMiddleware struct {
	controller PlanModeController
	name       string
}

var _ Middleware = (*PlanModeMiddleware)(nil)

// NewPlanModeMiddleware builds a PlanModeMiddleware backed by the given
// controller.
func NewPlanModeMiddleware(controller PlanModeController) *PlanModeMiddleware {
	return &PlanModeMiddleware{controller: controller, name: "plan-mode"}
}

// Name returns the middleware identifier.
func (m *PlanModeMiddleware) Name() string {
	if m.name == "" {
		return "plan-mode"
	}
	return m.name
}

// Wrap returns a wrapped AgentLoop that enforces plan-mode write blocking.
func (m *PlanModeMiddleware) Wrap(next AgentLoop) AgentLoop {
	return &planModeLoop{controller: m.controller, next: next}
}

// planModeLoop is the concrete wrapped loop produced by PlanModeMiddleware.
type planModeLoop struct {
	controller PlanModeController
	next       AgentLoop
}

// Run inspects the submission for write tool calls when plan mode is active.
// If a blocked tool is detected it returns an error event and does not invoke
// the wrapped loop; otherwise it delegates normally.
func (l *planModeLoop) Run(ctx context.Context, submission Submission, stream ...EventStream) ([]AgentEvent, error) {
	if !l.controller.IsActive() {
		return l.next.Run(ctx, submission, stream...)
	}

	blocked := l.blockedToolFromSubmission(submission)
	if blocked != "" {
		msg := fmt.Sprintf("plan mode active: write tool %q is blocked", blocked)
		slog.Info("core.plan_mode_middleware.blocked", "tool", blocked)
		return []AgentEvent{{
			Kind:      "error",
			Content:   msg,
			Timestamp: time.Now(),
		}}, fmt.Errorf("plan mode: write tool %q blocked", blocked)
	}

	return l.next.Run(ctx, submission, stream...)
}

// blockedToolFromSubmission inspects the submission metadata for tool names and
// returns the first write tool that should be blocked, or "" if none.
func (l *planModeLoop) blockedToolFromSubmission(submission Submission) string {
	for _, name := range extractToolNames(submission) {
		if l.controller.ShouldBlockWrite(name) {
			return name
		}
	}
	return ""
}

// extractToolNames reads tool names from the submission's Metadata using the
// conventional keys "tool_calls" (a list) and "tool_name" (a single name).
func extractToolNames(submission Submission) []string {
	if submission.Metadata == nil {
		return nil
	}
	var names []string

	// Single tool name.
	if v, ok := submission.Metadata["tool_name"]; ok {
		if s, ok := v.(string); ok && s != "" {
			names = append(names, s)
		}
	}

	// List of tool names. Accept []string, []any, or []tools.ToolCall.
	switch v := submission.Metadata["tool_calls"].(type) {
	case []string:
		names = append(names, v...)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				names = append(names, s)
			}
		}
	}

	return names
}
