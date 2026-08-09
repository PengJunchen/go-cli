package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// HookEvent enumerates the lifecycle events a shell hook can bind to.
type HookEvent string

const (
	EventPreToolUse   HookEvent = "pre_tool_use"
	EventPostToolUse  HookEvent = "post_tool_use"
	EventSessionStart HookEvent = "session_start"
	EventSessionEnd   HookEvent = "session_end"
)

// HookSystem is the interface for user-configured hooks loaded from
// .go-cli/hooks.yaml. Each method is invoked only when the hook's configured
// event matches.
type HookSystem interface {
	// PreToolUse is invoked before a tool executes. Returns allow=false to
	// block the tool call.
	PreToolUse(ctx context.Context, tool string, args map[string]any) (allow bool, err error)
	// PostToolUse is invoked after a tool executes.
	PostToolUse(ctx context.Context, tool string, args map[string]any, result *tools.ToolResult)
	// SessionStart is invoked when a session/turn begins.
	SessionStart(ctx context.Context)
	// SessionEnd is invoked when a session/turn ends.
	SessionEnd(ctx context.Context)
}

// hookPayload is the JSON object piped to a shell hook's stdin.
type hookPayload struct {
	Event  string            `json:"event"`
	Tool   string            `json:"tool,omitempty"`
	Args   map[string]any    `json:"args,omitempty"`
	Result *tools.ToolResult `json:"result,omitempty"`
}

// ShellHook is a HookSystem backed by a shell command. The command receives a
// JSON payload on stdin describing the event, and (for tool events) the tool
// name, arguments and result.
type ShellHook struct {
	name    string
	event   HookEvent
	command string
	timeout time.Duration
}

var _ HookSystem = (*ShellHook)(nil)

// NewShellHook builds a ShellHook that runs command when event fires, bounded
// by timeout.
func NewShellHook(name string, event HookEvent, command string, timeout time.Duration) *ShellHook {
	return &ShellHook{name: name, event: event, command: command, timeout: timeout}
}

// Name returns the hook identifier.
func (h *ShellHook) Name() string { return h.name }

// Event returns the event this hook is bound to.
func (h *ShellHook) Event() HookEvent { return h.event }

// PreToolUse runs the shell command before a tool call. Exit code 0 allows the
// call; non-zero denies it. Execution failures (timeout, cannot start) are
// logged and treated as allow (AC-5).
func (h *ShellHook) PreToolUse(ctx context.Context, tool string, args map[string]any) (bool, error) {
	if h.event != EventPreToolUse {
		return true, nil
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	payload := hookPayload{Event: string(h.event), Tool: tool, Args: args}
	output, err := h.execCommand(timeoutCtx, payload)
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			slog.Warn("core.hook.timeout", "hook", h.name, "event", h.event, "command", h.command)
			return true, nil // AC-5
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			slog.Info("core.hook.deny", "hook", h.name, "tool", tool, "exit_code", exitErr.ExitCode(), "output", output)
			return false, nil // non-zero exit = deny
		}
		slog.Warn("core.hook.exec_error", "hook", h.name, "event", h.event, "err", err)
		return true, nil // AC-5
	}
	return true, nil // exit 0 = allow
}

// PostToolUse runs the shell command after a tool call completes. The exit code
// is ignored; execution failures are logged (AC-5).
func (h *ShellHook) PostToolUse(ctx context.Context, tool string, args map[string]any, result *tools.ToolResult) {
	if h.event != EventPostToolUse {
		return
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	payload := hookPayload{Event: string(h.event), Tool: tool, Args: args, Result: result}
	output, err := h.execCommand(timeoutCtx, payload)
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			slog.Warn("core.hook.timeout", "hook", h.name, "event", h.event)
			return
		}
		slog.Warn("core.hook.post_tool_use_error", "hook", h.name, "err", err, "output", output)
		return
	}
	if output != "" {
		slog.Info("core.hook.post_tool_use", "hook", h.name, "output", output)
	}
}

// SessionStart runs the shell command when a session begins. The exit code is
// ignored; execution failures are logged (AC-5).
func (h *ShellHook) SessionStart(ctx context.Context) {
	if h.event != EventSessionStart {
		return
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	payload := hookPayload{Event: string(h.event)}
	output, err := h.execCommand(timeoutCtx, payload)
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			slog.Warn("core.hook.timeout", "hook", h.name, "event", h.event)
			return
		}
		slog.Warn("core.hook.session_start_error", "hook", h.name, "err", err, "output", output)
		return
	}
	if output != "" {
		slog.Info("core.hook.session_start", "hook", h.name, "output", output)
	}
}

// SessionEnd runs the shell command when a session ends. The exit code is
// ignored; execution failures are logged (AC-5).
func (h *ShellHook) SessionEnd(ctx context.Context) {
	if h.event != EventSessionEnd {
		return
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	payload := hookPayload{Event: string(h.event)}
	output, err := h.execCommand(timeoutCtx, payload)
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			slog.Warn("core.hook.timeout", "hook", h.name, "event", h.event)
			return
		}
		slog.Warn("core.hook.session_end_error", "hook", h.name, "err", err, "output", output)
		return
	}
	if output != "" {
		slog.Info("core.hook.session_end", "hook", h.name, "output", output)
	}
}

// execCommand runs the shell command with the JSON payload on stdin and returns
// the combined stdout/stderr output.
func (h *ShellHook) execCommand(ctx context.Context, payload hookPayload) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", h.command)
	cmd.Stdin = bytes.NewReader(data)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

// HookManager bridges one or more HookSystem instances into the existing
// LifecycleHook infrastructure. It implements LifecycleHook so it can be
// registered with a HookChain alongside other hooks.
type HookManager struct {
	hooks []HookSystem
}

var _ LifecycleHook = (*HookManager)(nil)

// NewHookManager builds a HookManager over the given hook systems.
func NewHookManager(hooks ...HookSystem) *HookManager {
	return &HookManager{hooks: append([]HookSystem{}, hooks...)}
}

// Name returns the hook identifier.
func (m *HookManager) Name() string { return "hook-manager" }

// BeforeRun is a no-op (HookSystem does not observe run-level events).
func (m *HookManager) BeforeRun(_ context.Context, _ Submission) error { return nil }

// AfterRun is a no-op (HookSystem does not observe run-level events).
func (m *HookManager) AfterRun(_ context.Context, _ Submission, _ Result, _ error) error {
	return nil
}

// BeforeToolCall invokes PreToolUse on every managed hook. A hook returning
// allow=false blocks the tool call (AC-4). A hook returning an error is logged
// and the chain continues (AC-5).
func (m *HookManager) BeforeToolCall(ctx context.Context, call tools.ToolCall) error {
	for _, h := range m.hooks {
		allow, err := h.PreToolUse(ctx, call.Name, call.Args)
		if err != nil {
			slog.Warn("core.hook_manager.pre_tool_use_error", "err", err)
			continue // AC-5
		}
		if !allow {
			return fmt.Errorf("tool %s blocked by hook", call.Name) // AC-4
		}
	}
	return nil
}

// AfterToolCall invokes PostToolUse on every managed hook. It always returns
// nil; post-tool hooks are fire-and-forget.
func (m *HookManager) AfterToolCall(ctx context.Context, call tools.ToolCall, result *tools.ToolResult, _ error) error {
	for _, h := range m.hooks {
		h.PostToolUse(ctx, call.Name, call.Args, result)
	}
	return nil
}

// OnTurnStart invokes SessionStart on every managed hook.
func (m *HookManager) OnTurnStart(ctx context.Context, _ string) error {
	for _, h := range m.hooks {
		h.SessionStart(ctx)
	}
	return nil
}

// OnTurnEnd invokes SessionEnd on every managed hook.
func (m *HookManager) OnTurnEnd(ctx context.Context, _ string, _ Result, _ error) error {
	for _, h := range m.hooks {
		h.SessionEnd(ctx)
	}
	return nil
}

// OnCompaction is a no-op (HookSystem does not observe compaction).
func (m *HookManager) OnCompaction(_ context.Context, _, _ int) error { return nil }

// OnError is a no-op (HookSystem does not observe errors).
func (m *HookManager) OnError(_ context.Context, _ string, _ error) error { return nil }
