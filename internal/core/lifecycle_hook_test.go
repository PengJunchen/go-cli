package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// spyLifecycleHook records every lifecycle callback invocation into a shared
// order slice. It implements LifecycleHook so it can be used with HookChain.
type spyLifecycleHook struct {
	name  string
	order *[]string
	mu    sync.Mutex
}

func (h *spyLifecycleHook) Name() string { return h.name }

func (h *spyLifecycleHook) add(ev string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.order = append(*h.order, ev)
}

func (h *spyLifecycleHook) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, *h.order...)
}

func (h *spyLifecycleHook) BeforeRun(_ context.Context, _ Submission) error {
	h.add("beforeRun:" + h.name)
	return nil
}

func (h *spyLifecycleHook) AfterRun(_ context.Context, _ Submission, _ Result, _ error) error {
	h.add("afterRun:" + h.name)
	return nil
}

func (h *spyLifecycleHook) OnTurnStart(_ context.Context, turnID string) error {
	h.add("onTurnStart:" + h.name)
	return nil
}

func (h *spyLifecycleHook) OnTurnEnd(_ context.Context, _ string, _ Result, _ error) error {
	h.add("onTurnEnd:" + h.name)
	return nil
}

func (h *spyLifecycleHook) BeforeToolCall(_ context.Context, _ tools.ToolCall) error {
	h.add("beforeToolCall:" + h.name)
	return nil
}

func (h *spyLifecycleHook) AfterToolCall(_ context.Context, _ tools.ToolCall, _ *tools.ToolResult, _ error) error {
	h.add("afterToolCall:" + h.name)
	return nil
}

func (h *spyLifecycleHook) OnCompaction(_ context.Context, _, _ int) error {
	h.add("onCompaction:" + h.name)
	return nil
}

func (h *spyLifecycleHook) OnError(_ context.Context, _ string, _ error) error {
	h.add("onError:" + h.name)
	return nil
}

// compactingAgent is a test Agent that implements messageSource. Its Run
// method reduces the message count to simulate compaction, so the TurnRunner
// fires OnCompaction.
type compactingAgent struct {
	name string
	mu   sync.Mutex
	msgs []AgentMessage
}

func (a *compactingAgent) Name() string { return a.name }

func (a *compactingAgent) Run(_ context.Context, _ Submission, _ ...EventStream) (Result, error) {
	a.mu.Lock()
	// Simulate compaction: drop to a single message.
	a.msgs = a.msgs[:1]
	a.mu.Unlock()
	return Result{Message: "compacted", Success: true}, nil
}

func (a *compactingAgent) Messages() []AgentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AgentMessage{}, a.msgs...)
}

// TestLifecycleHook verifies that all six lifecycle callbacks are triggered
// when exercised through the HookChain and HookAwareToolMiddleware.
func TestLifecycleHook(t *testing.T) {
	var order []string
	h := &spyLifecycleHook{name: "lh", order: &order}
	chain := NewHookChain(h)
	ctx := context.Background()

	// Direct HookChain lifecycle calls.
	require.NoError(t, chain.OnTurnStart(ctx, "turn-1"))
	require.NoError(t, chain.OnTurnEnd(ctx, "turn-1", Result{Success: true}, nil))
	require.NoError(t, chain.OnCompaction(ctx, 10, 5))
	require.NoError(t, chain.OnError(ctx, "turn-1", errors.New("boom")))

	// Tool call lifecycle via HookAwareToolMiddleware.
	mw := NewHookAwareToolMiddleware(chain)
	called := false
	wrapped := mw.WrapToolCall(func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		called = true
		return &tools.ToolResult{Output: "ok"}, nil
	})
	res, err := wrapped(ctx, tools.ToolCall{Name: "echo"})
	require.NoError(t, err)
	assert.Equal(t, "ok", res.Output)
	assert.True(t, called)

	rec := h.recorded()
	assert.Contains(t, rec, "onTurnStart:lh")
	assert.Contains(t, rec, "onTurnEnd:lh")
	assert.Contains(t, rec, "onCompaction:lh")
	assert.Contains(t, rec, "onError:lh")
	assert.Contains(t, rec, "beforeToolCall:lh")
	assert.Contains(t, rec, "afterToolCall:lh")
}

// TestLifecycleHookTurnRunner verifies that OnTurnStart, OnTurnEnd, and
// OnCompaction fire through the TurnRunner path.
func TestLifecycleHookTurnRunner(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var order []string
	h := &spyLifecycleHook{name: "lh", order: &order}
	chain := NewHookChain(h)

	agent := &compactingAgent{
		name: "compacting",
		msgs: make([]AgentMessage, 5), // 5 messages before, 1 after => compaction
	}
	runner := NewEinoTurnRunner(&stubLoop{})
	runner.SetAgent(agent)
	runner.SetHookChain(chain)

	result, err := runner.RunTurn(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	assert.True(t, result.Success)

	rec := h.recorded()
	assert.Contains(t, rec, "onTurnStart:lh")
	assert.Contains(t, rec, "onCompaction:lh")
	assert.Contains(t, rec, "onTurnEnd:lh")
	// No error, so OnError should NOT fire.
	assert.NotContains(t, rec, "onError:lh")
}

// TestHookBackwardCompat verifies that a plain Hook (not implementing
// LifecycleHook) continues to work: Before/After are called, and lifecycle
// methods on the chain are no-ops that return nil without panicking.
func TestHookBackwardCompat(t *testing.T) {
	var order []string
	plain := &spyHook{name: "plain", order: &order} // implements only Hook
	chain := NewHookChain(plain)
	ctx := context.Background()

	// Before/After must still work.
	res, err := chain.Before(ctx, Submission{Content: "go"})
	require.NoError(t, err)
	assert.True(t, res.Continue)
	require.NoError(t, chain.After(ctx, Submission{}, Result{}, nil))
	assert.Equal(t, []string{"before:plain", "after:plain"}, plain.recorded())

	// Lifecycle methods must be no-ops (hook doesn't implement LifecycleHook).
	require.NoError(t, chain.OnTurnStart(ctx, "t1"))
	require.NoError(t, chain.OnTurnEnd(ctx, "t1", Result{}, nil))
	require.NoError(t, chain.BeforeToolCall(ctx, tools.ToolCall{Name: "x"}))
	require.NoError(t, chain.AfterToolCall(ctx, tools.ToolCall{Name: "x"}, nil, nil))
	require.NoError(t, chain.OnCompaction(ctx, 5, 3))
	require.NoError(t, chain.OnError(ctx, "t1", errors.New("e")))

	// No lifecycle events recorded by the plain hook.
	for _, ev := range plain.recorded() {
		assert.NotContains(t, ev, "onTurnStart")
		assert.NotContains(t, ev, "onTurnEnd")
		assert.NotContains(t, ev, "beforeToolCall")
		assert.NotContains(t, ev, "afterToolCall")
		assert.NotContains(t, ev, "onCompaction")
		assert.NotContains(t, ev, "onError")
	}
}

// TestOnError verifies that OnError fires when a turn fails.
func TestOnError(t *testing.T) {
	var order []string
	h := &spyLifecycleHook{name: "lh", order: &order}
	chain := NewHookChain(h)

	sentinel := errors.New("fatal")
	runner := NewEinoTurnRunner(&errorLoop{err: sentinel})
	runner.SetHookChain(chain)

	_, err := runner.RunTurn(context.Background(), Submission{Content: "go"})
	require.ErrorIs(t, err, sentinel)

	rec := h.recorded()
	assert.Contains(t, rec, "onTurnStart:lh")
	assert.Contains(t, rec, "onError:lh")
	assert.Contains(t, rec, "onTurnEnd:lh")
}

// TestHookOrdering verifies the expected callback order through a full turn:
// OnTurnStart -> BeforeRun -> AfterRun -> OnTurnEnd.
func TestHookOrdering(t *testing.T) {
	var order []string
	h := &spyLifecycleHook{name: "lh", order: &order}
	chain := NewHookChain(h)

	// Wrap a stub loop with HookMiddleware so BeforeRun/AfterRun fire.
	wrappedLoop := NewHookMiddleware(chain).Wrap(&stubLoop{})

	runner := NewEinoTurnRunner(wrappedLoop)
	runner.SetHookChain(chain)

	_, err := runner.RunTurn(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	rec := h.recorded()
	// Expected order: OnTurnStart, BeforeRun, AfterRun, OnTurnEnd.
	require.Len(t, rec, 4)
	assert.Equal(t, "onTurnStart:lh", rec[0])
	assert.Equal(t, "beforeRun:lh", rec[1])
	assert.Equal(t, "afterRun:lh", rec[2])
	assert.Equal(t, "onTurnEnd:lh", rec[3])
}

// TestHookOrderingWithError verifies that OnError fires between AfterRun and
// OnTurnEnd when the turn errors.
func TestHookOrderingWithError(t *testing.T) {
	var order []string
	h := &spyLifecycleHook{name: "lh", order: &order}
	chain := NewHookChain(h)

	sentinel := errors.New("boom")
	wrappedLoop := NewHookMiddleware(chain).Wrap(&errorLoop{err: sentinel})

	runner := NewEinoTurnRunner(wrappedLoop)
	runner.SetHookChain(chain)

	_, err := runner.RunTurn(context.Background(), Submission{Content: "go"})
	require.ErrorIs(t, err, sentinel)

	rec := h.recorded()
	// Expected order: OnTurnStart, BeforeRun, AfterRun, OnError, OnTurnEnd.
	require.Len(t, rec, 5)
	assert.Equal(t, "onTurnStart:lh", rec[0])
	assert.Equal(t, "beforeRun:lh", rec[1])
	assert.Equal(t, "afterRun:lh", rec[2])
	assert.Equal(t, "onError:lh", rec[3])
	assert.Equal(t, "onTurnEnd:lh", rec[4])
}

// TestHookAwareToolMiddlewareBlock verifies that a BeforeToolCall error blocks
// the tool call.
func TestHookAwareToolMiddlewareBlock(t *testing.T) {
	blockErr := errors.New("blocked by policy")

	blockingHook := &lifecycleHookWithBefore{
		name:      "blocker",
		beforeErr: blockErr,
		LifecycleHookImpl: LifecycleHookImpl{
			HookImpl: HookImpl{name: "blocker"},
		},
	}
	chain := NewHookChain(blockingHook)
	mw := NewHookAwareToolMiddleware(chain)

	innerCalled := false
	wrapped := mw.WrapToolCall(func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		innerCalled = true
		return &tools.ToolResult{Output: "should not reach"}, nil
	})
	res, err := wrapped(context.Background(), tools.ToolCall{Name: "danger"})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), "tool call blocked by hook")
	assert.False(t, innerCalled)
}

// TestHookAwareToolMiddlewareNilChain verifies that a nil chain is a
// pass-through.
func TestHookAwareToolMiddlewareNilChain(t *testing.T) {
	mw := NewHookAwareToolMiddleware(nil)
	assert.Equal(t, "hook-aware-tool", mw.Name())

	called := false
	wrapped := mw.WrapToolCall(func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		called = true
		return &tools.ToolResult{Output: "ok"}, nil
	})
	res, err := wrapped(context.Background(), tools.ToolCall{Name: "x"})
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, "ok", res.Output)
}

// TestLifecycleHookImplNoop verifies the stub implements all methods as
// no-ops.
func TestLifecycleHookImplNoop(t *testing.T) {
	h := &LifecycleHookImpl{}
	ctx := context.Background()

	require.NoError(t, h.OnTurnStart(ctx, "t1"))
	require.NoError(t, h.OnTurnEnd(ctx, "t1", Result{}, nil))
	require.NoError(t, h.BeforeToolCall(ctx, tools.ToolCall{Name: "x"}))
	require.NoError(t, h.AfterToolCall(ctx, tools.ToolCall{Name: "x"}, nil, nil))
	require.NoError(t, h.OnCompaction(ctx, 5, 3))
	require.NoError(t, h.OnError(ctx, "t1", fmt.Errorf("e")))
	// Base Hook methods also work.
	require.NoError(t, h.BeforeRun(ctx, Submission{}))
	require.NoError(t, h.AfterRun(ctx, Submission{}, Result{}, nil))
}

// TestHookChainMultipleLifecycleHooks verifies that lifecycle methods iterate
// all hooks that implement LifecycleHook.
func TestHookChainMultipleLifecycleHooks(t *testing.T) {
	var order []string
	h1 := &spyLifecycleHook{name: "h1", order: &order}
	h2 := &spyLifecycleHook{name: "h2", order: &order}
	// A plain hook in the middle that doesn't implement LifecycleHook.
	plain := &spyHook{name: "plain", order: &order}
	chain := NewHookChain(h1, plain, h2)
	ctx := context.Background()

	require.NoError(t, chain.OnTurnStart(ctx, "t1"))

	rec := h2.recorded()
	// Only lifecycle hooks are called; plain hook is skipped.
	assert.Contains(t, rec, "onTurnStart:h1")
	assert.Contains(t, rec, "onTurnStart:h2")
	assert.NotContains(t, rec, "onTurnStart:plain")
	// h1 fires before h2.
	idx1 := indexOf(rec, "onTurnStart:h1")
	idx2 := indexOf(rec, "onTurnStart:h2")
	assert.Greater(t, idx2, idx1)
}

// TestHookChainLifecycleErrorStops verifies that the first error halts the
// chain for lifecycle methods.
func TestHookChainLifecycleErrorStops(t *testing.T) {
	var order []string
	errSentinel := errors.New("hook error")

	errHook := &lifecycleHookWithError{
		name:         "err",
		turnStartErr: errSentinel,
		LifecycleHookImpl: LifecycleHookImpl{
			HookImpl: HookImpl{name: "err"},
		},
	}
	h2 := &spyLifecycleHook{name: "h2", order: &order}
	chain := NewHookChain(errHook, h2)
	ctx := context.Background()

	err := chain.OnTurnStart(ctx, "t1")
	require.ErrorIs(t, err, errSentinel)

	// h2 should NOT have been called.
	rec := h2.recorded()
	assert.NotContains(t, rec, "onTurnStart:h2")
}

// lifecycleHookWithBefore lets you inject a BeforeToolCall error.
type lifecycleHookWithBefore struct {
	LifecycleHookImpl
	name      string
	beforeErr error
}

func (h *lifecycleHookWithBefore) Name() string { return h.name }

func (h *lifecycleHookWithBefore) BeforeToolCall(_ context.Context, _ tools.ToolCall) error {
	return h.beforeErr
}

// lifecycleHookWithError lets you inject an OnTurnStart error.
type lifecycleHookWithError struct {
	LifecycleHookImpl
	name         string
	turnStartErr error
}

func (h *lifecycleHookWithError) Name() string { return h.name }

func (h *lifecycleHookWithError) OnTurnStart(_ context.Context, _ string) error {
	return h.turnStartErr
}

// indexOf returns the index of s in slice, or -1 if not found.
func indexOf(slice []string, s string) int {
	for i, v := range slice {
		if v == s {
			return i
		}
	}
	return -1
}
