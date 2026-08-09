package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestHookSystem_ShellHookPreToolUse_Allow(t *testing.T) {
	hook := NewShellHook("test", EventPreToolUse, "exit 0", 5*time.Second)
	allow, err := hook.PreToolUse(context.Background(), "bash", nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !allow {
		t.Fatal("expected allow=true for exit 0, got false")
	}
}

func TestHookSystem_ShellHookPreToolUse_Deny(t *testing.T) {
	hook := NewShellHook("test", EventPreToolUse, "exit 1", 5*time.Second)
	allow, err := hook.PreToolUse(context.Background(), "bash", nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if allow {
		t.Fatal("expected allow=false for exit 1, got true")
	}
}

func TestHookSystem_ShellHookPreToolUse_EventMismatch(t *testing.T) {
	hook := NewShellHook("test", EventPostToolUse, "exit 1", 5*time.Second)
	allow, err := hook.PreToolUse(context.Background(), "bash", nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !allow {
		t.Fatal("expected allow=true for non-matching event, got false")
	}
}

func TestHookSystem_ShellHookExecutionFailure(t *testing.T) {
	// A command that will timeout, simulating execution failure (AC-5).
	hook := NewShellHook("test", EventPreToolUse, "sleep 30", 50*time.Millisecond)
	allow, err := hook.PreToolUse(context.Background(), "bash", nil)
	if err != nil {
		t.Fatalf("expected nil error on execution failure, got %v", err)
	}
	if !allow {
		t.Fatal("expected allow=true on execution failure (AC-5), got false")
	}
}

func TestHookSystem_ShellHookReceivesJSON(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "payload.json")
	hook := NewShellHook("test", EventPreToolUse, "cat > "+tmpFile, 5*time.Second)
	args := map[string]any{"command": "ls", "timeout": float64(10)}
	allow, err := hook.PreToolUse(context.Background(), "bash", args)
	if err != nil || !allow {
		t.Fatalf("expected (true, nil), got (%v, %v)", allow, err)
	}

	data, readErr := os.ReadFile(tmpFile)
	if readErr != nil {
		t.Fatalf("failed to read payload file: %v", readErr)
	}
	var payload hookPayload
	if jsonErr := json.Unmarshal(data, &payload); jsonErr != nil {
		t.Fatalf("failed to parse JSON payload: %v", jsonErr)
	}
	if payload.Event != "pre_tool_use" {
		t.Errorf("expected event=pre_tool_use, got %s", payload.Event)
	}
	if payload.Tool != "bash" {
		t.Errorf("expected tool=bash, got %s", payload.Tool)
	}
	if payload.Args["command"] != "ls" {
		t.Errorf("expected args[command]=ls, got %v", payload.Args["command"])
	}
}

func TestHookSystem_HookManagerBlocksToolCall(t *testing.T) {
	hook := NewShellHook("test", EventPreToolUse, "exit 1", 5*time.Second)
	manager := NewHookManager(hook)
	err := manager.BeforeToolCall(context.Background(), tools.ToolCall{Name: "bash"})
	if err == nil {
		t.Fatal("expected error for blocked tool call (AC-4), got nil")
	}
}

func TestHookSystem_HookManagerAllowsToolCall(t *testing.T) {
	hook := NewShellHook("test", EventPreToolUse, "exit 0", 5*time.Second)
	manager := NewHookManager(hook)
	err := manager.BeforeToolCall(context.Background(), tools.ToolCall{Name: "bash"})
	if err != nil {
		t.Fatalf("expected nil error for allowed tool call, got %v", err)
	}
}

func TestHookSystem_HookManagerExecutionFailureContinues(t *testing.T) {
	// A ShellHook that times out returns (true, nil), so the HookManager
	// should continue and return nil (AC-5).
	timeoutHook := NewShellHook("timeout", EventPreToolUse, "sleep 30", 50*time.Millisecond)
	manager := NewHookManager(timeoutHook)
	err := manager.BeforeToolCall(context.Background(), tools.ToolCall{Name: "bash"})
	if err != nil {
		t.Fatalf("expected nil on hook timeout (AC-5), got %v", err)
	}

	// A hook that returns an error should also not block (AC-5).
	errHook := &mockHookSystem{
		event:        EventPreToolUse,
		preToolAllow: true,
		preToolErr:   errors.New("mock execution error"),
	}
	manager2 := NewHookManager(errHook)
	err = manager2.BeforeToolCall(context.Background(), tools.ToolCall{Name: "bash"})
	if err != nil {
		t.Fatalf("expected nil on hook error (AC-5), got %v", err)
	}
}

func TestHookSystem_HookManagerPostToolUse(t *testing.T) {
	mock := &mockHookSystem{event: EventPostToolUse}
	manager := NewHookManager(mock)
	result := &tools.ToolResult{Output: "test-output"}
	err := manager.AfterToolCall(context.Background(), tools.ToolCall{Name: "bash"}, result, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !mock.postToolCalled {
		t.Fatal("expected PostToolUse to be called")
	}
}

func TestHookSystem_HookManagerSessionLifecycle(t *testing.T) {
	mock := &mockHookSystem{event: EventSessionStart}
	manager := NewHookManager(mock)

	// OnTurnStart should call SessionStart.
	if err := manager.OnTurnStart(context.Background(), "turn-1"); err != nil {
		t.Fatalf("OnTurnStart error: %v", err)
	}
	if !mock.sessionStartCalled {
		t.Fatal("expected SessionStart to be called")
	}

	// OnTurnEnd should call SessionEnd.
	if err := manager.OnTurnEnd(context.Background(), "turn-1", Result{}, nil); err != nil {
		t.Fatalf("OnTurnEnd error: %v", err)
	}
	if !mock.sessionEndCalled {
		t.Fatal("expected SessionEnd to be called")
	}
}

// mockHookSystem is a test double for HookSystem.
type mockHookSystem struct {
	event              HookEvent
	preToolAllow       bool
	preToolErr         error
	preToolCalled      bool
	postToolCalled     bool
	sessionStartCalled bool
	sessionEndCalled   bool
}

func (m *mockHookSystem) PreToolUse(_ context.Context, _ string, _ map[string]any) (bool, error) {
	m.preToolCalled = true
	return m.preToolAllow, m.preToolErr
}

func (m *mockHookSystem) PostToolUse(_ context.Context, _ string, _ map[string]any, _ *tools.ToolResult) {
	m.postToolCalled = true
}

func (m *mockHookSystem) SessionStart(_ context.Context) {
	m.sessionStartCalled = true
}

func (m *mockHookSystem) SessionEnd(_ context.Context) {
	m.sessionEndCalled = true
}
