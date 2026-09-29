package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// PlanModeController manages plan mode state. When plan mode is active, tools
// outside the explicit read-only set are blocked so the agent can research and
// propose a plan without invoking unreviewed mutations.
type PlanModeController interface {
	// Enter activates plan mode, recording the reason for activation.
	Enter(ctx context.Context, reason string) error
	// Exit deactivates plan mode, recording a summary of the plan produced.
	Exit(ctx context.Context, summary string) error
	// IsActive reports whether plan mode is currently active.
	IsActive() bool
	// ShouldBlockWrite reports whether the named tool should be blocked given
	// the current plan-mode state. In active Plan Mode, only explicitly
	// allowlisted controls pass this coarse name-based preflight.
	ShouldBlockWrite(toolName string) bool
}

// planModeReadOnlyToolNames is the R0 name-based stopgap allowlist for the
// explicitly approved built-in reads and Plan Mode control tools. All other
// names, including mutations, custom tools, and unknown tools, fail closed;
// the guard does not infer permission from prefixes or registration names.
var planModeReadOnlyToolNames = map[string]struct{}{
	"read": {}, "ls": {}, "grep": {}, "find": {},
	"web_search": {}, "web_fetch": {},
	"git_diff": {}, "git_status": {}, "git_log": {}, "git_branch": {}, "git_blame": {}, "git_remote": {},
	"lsp_query":   {},
	"tool_search": {}, "ask_user": {},
	"goal_get": {}, "goal_list": {}, "task_get": {}, "task_list": {},
	"enter_plan_mode": {}, "exit_plan_mode": {},
}

// DefaultPlanModeController is a thread-safe PlanModeController backed by a
// sync.RWMutex. It is safe for concurrent use by multiple goroutines.
type DefaultPlanModeController struct {
	mu     sync.RWMutex
	active bool
	reason string
	fence  *tools.DispatchFence
}

var _ PlanModeController = (*DefaultPlanModeController)(nil)

// NewDefaultPlanModeController returns a DefaultPlanModeController with plan
// mode initially inactive. Supplying a fence lets Plan transitions invalidate
// prepared tool calls against the same state used by the registry and approval
// policy. When omitted, the controller owns a fence for compatibility with
// callers that do not use prepared dispatch.
func NewDefaultPlanModeController(fences ...*tools.DispatchFence) *DefaultPlanModeController {
	fence := tools.NewDispatchFence()
	if len(fences) > 0 && fences[0] != nil {
		fence = fences[0]
	}
	return &DefaultPlanModeController{fence: fence}
}

// DispatchFence returns the fence used to publish Plan Mode state transitions.
func (c *DefaultPlanModeController) DispatchFence() *tools.DispatchFence {
	if c == nil {
		return nil
	}
	return c.fence
}

// Enter activates plan mode and logs the reason.
func (c *DefaultPlanModeController) Enter(_ context.Context, reason string) error {
	if _, err := c.fence.Publish(tools.DispatchFencePlan, nil, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.active = true
		c.reason = reason
	}); err != nil {
		return fmt.Errorf("publish Plan Mode entry: %w", err)
	}
	slog.Info("core.plan_mode.enter", "reason", reason)
	return nil
}

// Exit deactivates plan mode and logs the summary.
func (c *DefaultPlanModeController) Exit(_ context.Context, summary string) error {
	if _, err := c.fence.Publish(tools.DispatchFencePlan, nil, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.active = false
		c.reason = ""
	}); err != nil {
		return fmt.Errorf("publish Plan Mode exit: %w", err)
	}
	slog.Info("core.plan_mode.exit", "summary", summary)
	return nil
}

// IsActive reports whether plan mode is currently active.
func (c *DefaultPlanModeController) IsActive() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.active
}

// ShouldBlockWrite reports whether the named tool should be blocked. It returns
// true when plan mode is active and the tool name is not in the explicit R0
// built-in read/control allowlist.
func (c *DefaultPlanModeController) ShouldBlockWrite(toolName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.active {
		return false
	}
	_, readOnly := planModeReadOnlyToolNames[toolName]
	blocked := !readOnly
	if blocked {
		slog.Debug("core.plan_mode.block_write", "tool", toolName)
	}
	return blocked
}

// Reason returns the reason recorded when plan mode was last entered. It
// returns "" when plan mode is inactive.
func (c *DefaultPlanModeController) Reason() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reason
}

// ErrPlanModeBlocked is returned by NewPlanModeToolWrapper when a tool call is
// blocked because plan mode is active and the tool is not allowlisted.
var ErrPlanModeBlocked = errors.New("plan mode: tool blocked")

// NewPlanModeToolWrapper returns a tools.ToolExecutorWrapper that intercepts
// tool calls at the ToolExecutorWrapper chain level. When plan mode is active,
// only explicitly allowlisted read-only tools pass; unknown and side-effecting
// tools return a descriptive message and ErrPlanModeBlocked without invoking
// the underlying executor.
//
// When the controller is nil (zero-config fallback) or plan mode is inactive,
// all calls pass through to next unchanged.
func NewPlanModeToolWrapper(controller PlanModeController) tools.ToolExecutorWrapper {
	return func(next func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)) func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			// Zero-config fallback: nil controller passes through.
			if controller == nil {
				if next == nil {
					return nil, nil
				}
				return next(ctx, call)
			}
			// When plan mode is inactive, pass through.
			if !controller.IsActive() {
				if next == nil {
					return nil, nil
				}
				return next(ctx, call)
			}
			// When plan mode is active and the tool is a write tool, block.
			if controller.ShouldBlockWrite(call.Name) {
				return &tools.ToolResult{
					Output: fmt.Sprintf("plan mode: tool %q blocked", call.Name),
					Metadata: map[string]any{
						"plan_mode_blocked": true,
					},
				}, ErrPlanModeBlocked
			}
			// Read tools pass through.
			if next == nil {
				return nil, nil
			}
			return next(ctx, call)
		}
	}
}
