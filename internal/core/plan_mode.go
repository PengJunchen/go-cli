package core

import (
	"context"
	"log/slog"
	"sync"
)

// PlanModeController manages plan mode state. When plan mode is active, write
// tools are blocked so the agent can only research and propose a plan without
// mutating the filesystem or executing commands.
type PlanModeController interface {
	// Enter activates plan mode, recording the reason for activation.
	Enter(ctx context.Context, reason string) error
	// Exit deactivates plan mode, recording a summary of the plan produced.
	Exit(ctx context.Context, summary string) error
	// IsActive reports whether plan mode is currently active.
	IsActive() bool
	// ShouldBlockWrite reports whether the named tool should be blocked given
	// the current plan-mode state.
	ShouldBlockWrite(toolName string) bool
}

// writeToolNames is the set of built-in tool names that produce side effects
// and are therefore blocked while plan mode is active.
var writeToolNames = map[string]bool{
	"write":    true,
	"edit":     true,
	"bash":     true,
	"mutation": true,
}

// DefaultPlanModeController is a thread-safe PlanModeController backed by a
// sync.RWMutex. It is safe for concurrent use by multiple goroutines.
type DefaultPlanModeController struct {
	mu     sync.RWMutex
	active bool
	reason string
}

var _ PlanModeController = (*DefaultPlanModeController)(nil)

// NewDefaultPlanModeController returns a DefaultPlanModeController with plan
// mode initially inactive.
func NewDefaultPlanModeController() *DefaultPlanModeController {
	return &DefaultPlanModeController{}
}

// Enter activates plan mode and logs the reason.
func (c *DefaultPlanModeController) Enter(_ context.Context, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = true
	c.reason = reason
	slog.Info("core.plan_mode.enter", "reason", reason)
	return nil
}

// Exit deactivates plan mode and logs the summary.
func (c *DefaultPlanModeController) Exit(_ context.Context, summary string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = false
	c.reason = ""
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
// true only when plan mode is active and the tool is a known write tool.
func (c *DefaultPlanModeController) ShouldBlockWrite(toolName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.active {
		return false
	}
	blocked := writeToolNames[toolName]
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
