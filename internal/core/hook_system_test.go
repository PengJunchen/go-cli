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

func TestShellHookStartGuardReturnsBeforeWaitingForChild(t *testing.T) {
	started := make(chan struct{})
	guardReturned := make(chan struct{})
	marker := filepath.Join(t.TempDir(), "hook-ran")
	guard := ShellHookStartGuard(func(_ context.Context, start func() error) error {
		close(started)
		if err := start(); err != nil {
			return err
		}
		close(guardReturned)
		return nil
	})
	hook := NewShellHookWithStartGuard("guarded", EventSessionStart,
		"sleep 0.25; printf done >> '"+marker+"'", 3*time.Second, guard)
	done := make(chan struct{})
	go func() {
		hook.SessionStart(context.Background())
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shell hook did not enter its start guard")
	}
	select {
	case <-guardReturned:
	case <-time.After(time.Second):
		t.Fatal("start guard did not return after starting the child")
	}
	select {
	case <-done:
		t.Fatal("shell hook waited for the child while still inside its start guard")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shell hook did not join its child")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "done" {
		t.Fatalf("guarded child output = %q, error = %v", data, err)
	}
}

func TestShellHookStartGuardErrorStillJoinsStartedChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "hook-ran")
	guardErr := errors.New("trust state changed after launch")
	guard := ShellHookStartGuard(func(_ context.Context, start func() error) error {
		if err := start(); err != nil {
			return err
		}
		return guardErr
	})
	hook := NewShellHookWithStartGuard("guarded", EventSessionStart,
		"sleep 0.15; printf done > '"+marker+"'", time.Second, guard)
	_, err := hook.execCommand(context.Background(), hookPayload{Event: string(EventSessionStart)})
	if !errors.Is(err, guardErr) {
		t.Fatalf("expected launch guard error, got %v", err)
	}
	if data, readErr := os.ReadFile(marker); readErr != nil || string(data) != "done" {
		t.Fatalf("started child was not joined: data=%q error=%v", data, readErr)
	}
}

func TestShellHookStartGuardCannotSilentlySkipLaunch(t *testing.T) {
	hook := NewShellHookWithStartGuard("guarded", EventSessionStart, "exit 0", time.Second,
		func(context.Context, func() error) error { return nil })
	_, err := hook.execCommand(context.Background(), hookPayload{Event: string(EventSessionStart)})
	if !errors.Is(err, errShellHookGuardDidNotStart) {
		t.Fatalf("expected a fail-closed guard error, got %v", err)
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
	interceptor := manager.PreToolUseInterceptor()
	err := interceptor("bash", "call-1", nil)
	if err == nil {
		t.Fatal("expected error for blocked tool call (AC-4), got nil")
	}
}

func TestHookSystem_HookManagerAllowsToolCall(t *testing.T) {
	hook := NewShellHook("test", EventPreToolUse, "exit 0", 5*time.Second)
	manager := NewHookManager(hook)
	interceptor := manager.PreToolUseInterceptor()
	err := interceptor("bash", "call-1", nil)
	if err != nil {
		t.Fatalf("expected nil error for allowed tool call, got %v", err)
	}
}

func TestHookSystem_HookManagerExecutionFailureContinues(t *testing.T) {
	// A ShellHook that times out returns (true, nil), so the HookManager
	// should continue and return nil (AC-5).
	timeoutHook := NewShellHook("timeout", EventPreToolUse, "sleep 30", 50*time.Millisecond)
	manager := NewHookManager(timeoutHook)
	interceptor := manager.PreToolUseInterceptor()
	err := interceptor("bash", "call-1", nil)
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
	interceptor2 := manager2.PreToolUseInterceptor()
	err = interceptor2("bash", "call-1", nil)
	if err != nil {
		t.Fatalf("expected nil on hook error (AC-5), got %v", err)
	}
}

func TestHookSystem_HookManagerPostToolUse(t *testing.T) {
	mock := &mockHookSystem{event: EventPostToolUse}
	manager := NewHookManager(mock)
	wrapper := manager.PostToolUseWrapper()

	called := false
	wrapped := wrapper(func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		called = true
		return &tools.ToolResult{Output: "test-output"}, nil
	})
	result, err := wrapped(context.Background(), tools.ToolCall{Name: "bash"})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !called {
		t.Fatal("expected inner executor to be called")
	}
	if result == nil || result.Output != "test-output" {
		t.Fatalf("expected result output=test-output, got %v", result)
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
