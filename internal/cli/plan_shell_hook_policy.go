package cli

import (
	"context"

	"github.com/pengjunchen/go-cli/internal/core"
)

const planModePreToolShellHookDenialMessage = "Plan mode blocked this tool call because a project pre-tool shell hook is configured. Exit Plan mode to run it."

type planModePreToolShellHookDenial struct{}

var _ core.SafeToolInterceptionDenial = planModePreToolShellHookDenial{}

func (planModePreToolShellHookDenial) Error() string {
	return planModePreToolShellHookDenialMessage
}

func (planModePreToolShellHookDenial) SafeToolInterceptionMessage() string {
	return planModePreToolShellHookDenialMessage
}

type planModeLifecycleShellHookSkipped struct{}

func (planModeLifecycleShellHookSkipped) Error() string {
	return "Plan mode skipped a project lifecycle shell hook."
}

type planModeShellHookStartBoundaryUnavailable struct{}

func (planModeShellHookStartBoundaryUnavailable) Error() string {
	return "A project shell hook was blocked because atomic Plan mode start authorization is unavailable."
}

func (planModeShellHookStartBoundaryUnavailable) SafeToolInterceptionMessage() string {
	return "A project shell hook was blocked because atomic Plan mode start authorization is unavailable."
}

type planModeShellHookStartBoundary interface {
	StartIfInactive(start func() error) (bool, error)
}

// planModeShellHookStart authorizes the process start against the same fence
// that publishes Plan transitions. Controllers without the atomic boundary
// fail closed so an external implementation cannot silently reintroduce a
// check-then-start race.
func planModeShellHookStart(
	event core.HookEvent,
	planCtrl core.PlanModeController,
	start func() error,
) error {
	if start == nil {
		return planModeShellHookStartBoundaryUnavailable{}
	}
	if planCtrl == nil {
		return start()
	}
	boundary, ok := planCtrl.(planModeShellHookStartBoundary)
	if !ok {
		return planModeShellHookStartBoundaryUnavailable{}
	}
	authorized, err := boundary.StartIfInactive(start)
	if err != nil {
		if !authorized {
			return planModeShellHookStartBoundaryUnavailable{}
		}
		return err
	}
	if authorized {
		return nil
	}
	if event == core.EventPreToolUse {
		return planModePreToolShellHookDenial{}
	}
	return planModeLifecycleShellHookSkipped{}
}

func planModePreToolShellHookInterceptor(
	planCtrl core.PlanModeController,
	shellHooks []core.HookSystem,
	next core.ContextToolInterceptor,
) core.ContextToolInterceptor {
	if next == nil || !hasShellHookEvent(shellHooks, core.EventPreToolUse) {
		return next
	}

	return func(ctx context.Context, toolName, toolCallID string, args map[string]any) error {
		if planCtrl != nil && planCtrl.IsActive() {
			return planModePreToolShellHookDenial{}
		}
		return next(ctx, toolName, toolCallID, args)
	}
}

func hasShellHookEvent(hooks []core.HookSystem, event core.HookEvent) bool {
	for _, hook := range hooks {
		shellHook, ok := hook.(*core.ShellHook)
		if ok && shellHook.Event() == event {
			return true
		}
	}
	return false
}
