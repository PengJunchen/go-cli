package core

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnifiedInterception_PlanModeViaToolInterceptor verifies that plan-mode
// write blocking flows through the unified ToolInterceptor mechanism: when
// plan mode is active, a PreToolCallEvent for a write tool is cancelled by
// the registered NewPlanModeToolInterceptor.
func TestUnifiedInterception_PlanModeViaToolInterceptor(t *testing.T) {
	defer ClearToolInterceptors()

	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	RegisterToolInterceptor(NewPlanModeToolInterceptor(ctrl))

	ev := &PreToolCallEvent{ToolName: "write", ToolCallID: "call-1"}
	assert.True(t, ev.IsCancelled(), "write tool should be cancelled in plan mode")
}

// TestUnifiedInterception_ReadToolsNotBlocked verifies that read tools are
// not blocked by the plan-mode interceptor even when plan mode is active.
func TestUnifiedInterception_ReadToolsNotBlocked(t *testing.T) {
	defer ClearToolInterceptors()

	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	RegisterToolInterceptor(NewPlanModeToolInterceptor(ctrl))

	ev := &PreToolCallEvent{ToolName: "read", ToolCallID: "call-1"}
	assert.False(t, ev.IsCancelled(), "read tool should not be cancelled in plan mode")
}

// TestUnifiedInterception_MultipleInterceptors verifies that multiple
// interceptors run in registration order and the first error cancels the
// call.
func TestUnifiedInterception_MultipleInterceptors(t *testing.T) {
	defer ClearToolInterceptors()

	var callOrder []string
	var mu sync.Mutex

	// First interceptor: allows all, records order.
	RegisterToolInterceptor(func(toolName, callID string, args map[string]any) error {
		mu.Lock()
		callOrder = append(callOrder, "first")
		mu.Unlock()
		return nil
	})

	// Second interceptor: blocks "danger".
	RegisterToolInterceptor(func(toolName, callID string, args map[string]any) error {
		mu.Lock()
		callOrder = append(callOrder, "second")
		mu.Unlock()
		if toolName == "danger" {
			return errors.New("blocked by second interceptor")
		}
		return nil
	})

	// Third interceptor: should NOT run when second blocks.
	RegisterToolInterceptor(func(toolName, callID string, args map[string]any) error {
		mu.Lock()
		callOrder = append(callOrder, "third")
		mu.Unlock()
		return nil
	})

	// "safe" tool: all interceptors run, none block.
	ev1 := &PreToolCallEvent{ToolName: "safe", ToolCallID: "c1"}
	require.False(t, ev1.IsCancelled())
	mu.Lock()
	assert.Equal(t, []string{"first", "second", "third"}, callOrder)
	mu.Unlock()

	// "danger" tool: second interceptor blocks, third should not run.
	callOrder = nil
	ev2 := &PreToolCallEvent{ToolName: "danger", ToolCallID: "c2"}
	require.True(t, ev2.IsCancelled())
	mu.Lock()
	assert.Equal(t, []string{"first", "second"}, callOrder)
	mu.Unlock()
}

// TestHookChain_BeforeToolCallRemoved verifies that HookChain no longer has
// BeforeToolCall or AfterToolCall methods — tool-call interception is
// handled exclusively by the ToolInterceptor mechanism.
func TestHookChain_BeforeToolCallRemoved(t *testing.T) {
	// spyLifecycleHook no longer implements BeforeToolCall/AfterToolCall,
	// yet it still satisfies LifecycleHook because those methods were
	// removed from the interface. If they were still required, this would
	// fail to compile.
	var _ LifecycleHook = &spyLifecycleHook{name: "test"}

	// HookChain still works for lifecycle events.
	var order []string
	chain := NewHookChain(&spyLifecycleHook{name: "test", order: &order})
	require.NoError(t, chain.OnTurnStart(context.Background(), "t1"))
	require.NoError(t, chain.OnTurnEnd(context.Background(), "t1", Result{}, nil))

	// Tool-call interception is now via ToolInterceptor + PreToolCallEvent.
	defer ClearToolInterceptors()
	called := false
	RegisterToolInterceptor(func(toolName, callID string, args map[string]any) error {
		called = true
		return nil
	})
	ev := &PreToolCallEvent{ToolName: "test", ToolCallID: "c1"}
	_ = ev.IsCancelled()
	assert.True(t, called, "interceptor should be called via PreToolCallEvent")
}

// TestUnifiedInterception_Race verifies that concurrent interceptor
// registration and PreToolCallEvent.IsCancelled calls do not race.
func TestUnifiedInterception_Race(t *testing.T) {
	defer ClearToolInterceptors()

	// Register a base interceptor.
	RegisterToolInterceptor(func(toolName, callID string, args map[string]any) error {
		return nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)

		// Goroutine 1: create events and check IsCancelled.
		go func() {
			defer wg.Done()
			ev := &PreToolCallEvent{ToolName: "test", ToolCallID: "c1"}
			_ = ev.IsCancelled()
		}()

		// Goroutine 2: register interceptors concurrently.
		go func() {
			defer wg.Done()
			RegisterToolInterceptor(func(toolName, callID string, args map[string]any) error {
				return nil
			})
		}()
	}
	wg.Wait()
}
