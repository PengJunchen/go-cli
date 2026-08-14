package core

import (
	"sync"
	"testing"
)

func TestPreToolCallEvent_Cancel(t *testing.T) {
	ev := &PreToolCallEvent{
		ToolName:   "bash",
		ToolCallID: "call_123",
		Args:       map[string]any{"command": "ls"},
	}
	if ev.IsCancelled() {
		t.Fatal("event should not be cancelled initially")
	}
	ev.Cancel()
	if !ev.IsCancelled() {
		t.Fatal("event should be cancelled after Cancel()")
	}
}

func TestPreToolCallEvent_NotCancelled(t *testing.T) {
	ev := &PreToolCallEvent{
		ToolName:   "read",
		ToolCallID: "call_456",
		Args:       map[string]any{"path": "/tmp"},
	}
	if ev.IsCancelled() {
		t.Fatal("event should not be cancelled")
	}
}

func TestPreToolCallEvent_ConcurrentSafe(t *testing.T) {
	ev := &PreToolCallEvent{
		ToolName:   "write",
		ToolCallID: "call_789",
		Args:       map[string]any{"path": "/tmp/test"},
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ev.Cancel()
		}()
		go func() {
			defer wg.Done()
			_ = ev.IsCancelled()
		}()
	}
	wg.Wait()
	if !ev.IsCancelled() {
		t.Fatal("event should be cancelled after concurrent Cancel calls")
	}
}
