package core

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spyHook records into a shared order slice and exposes knobs to interrupt or
// fail the before/after phases.
type spyHook struct {
	name       string
	order      *[]string
	mu         sync.Mutex
	failBefore bool
	failAfter  bool
}

func (h *spyHook) Name() string { return h.name }

func (h *spyHook) add(ev string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.order == nil {
		return
	}
	*h.order = append(*h.order, ev)
}

func (h *spyHook) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.order == nil {
		return nil
	}
	return append([]string{}, *h.order...)
}

func (h *spyHook) BeforeRun(_ context.Context, _ Submission) error {
	h.add("before:" + h.name)
	if h.failBefore {
		return errors.New("before failed: " + h.name)
	}
	return nil
}

func (h *spyHook) AfterRun(_ context.Context, _ Submission, _ Result, _ error) error {
	h.add("after:" + h.name)
	if h.failAfter {
		return errors.New("after failed: " + h.name)
	}
	return nil
}

func TestHookBeforeCallsInOrder(t *testing.T) {
	var order []string
	h1 := &spyHook{name: "h1", order: &order}
	h2 := &spyHook{name: "h2", order: &order}
	chain := NewHookChain(h1, h2)

	res, err := chain.Before(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	assert.True(t, res.Continue)
	assert.Equal(t, []string{"before:h1", "before:h2"}, h2.recorded())
}

func TestHookAfterCallsInOrder(t *testing.T) {
	var order []string
	h1 := &spyHook{name: "h1", order: &order}
	h2 := &spyHook{name: "h2", order: &order}
	chain := NewHookChain(h1, h2)

	err := chain.After(context.Background(), Submission{Content: "hi"}, Result{Message: "m", Success: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"after:h1", "after:h2"}, h2.recorded())
}

func TestHookAfterRunsAllEvenOnError(t *testing.T) {
	var order []string
	h1 := &spyHook{name: "h1", order: &order, failAfter: true}
	h2 := &spyHook{name: "h2", order: &order}
	chain := NewHookChain(h1, h2)

	err := chain.After(context.Background(), Submission{}, Result{}, nil)
	require.Error(t, err)
	// Both hooks observe the outcome despite the first failing.
	assert.Equal(t, []string{"after:h1", "after:h2"}, h2.recorded())
}

func TestHookChainDefaultHookImpl(t *testing.T) {
	chain := NewHookChain(&HookImpl{name: "h1"}, &HookImpl{name: "h2"})
	res, err := chain.Before(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	assert.True(t, res.Continue)
	require.NoError(t, chain.After(context.Background(), Submission{}, Result{}, nil))
}

func TestHookChainEmptyPasses(t *testing.T) {
	chain := NewHookChain()
	res, err := chain.Before(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	assert.True(t, res.Continue)
	require.NoError(t, chain.After(context.Background(), Submission{}, Result{}, nil))
}

func TestHookBeforeInterruptionStopsChain(t *testing.T) {
	var order []string
	h1 := &spyHook{name: "h1", order: &order}
	h2 := &spyHook{name: "h2", order: &order}
	h2.failBefore = true
	h3 := &spyHook{name: "h3", order: &order}
	chain := NewHookChain(h1, h2, h3)

	res, err := chain.Before(context.Background(), Submission{Content: "hi"})
	require.Error(t, err)
	assert.False(t, res.Continue)
	assert.True(t, res.Interrupted())
	// h3 must not have been reached after h2 failed.
	assert.Equal(t, []string{"before:h1", "before:h2"}, h3.recorded())
	assert.Contains(t, res.Output, "before failed: h2")
}

func TestHookResultHelpers(t *testing.T) {
	assert.True(t, ContinueHookResult.Continue)
	assert.False(t, ContinueHookResult.Interrupted())
	assert.False(t, InterruptHookResult.Continue)
	assert.True(t, InterruptHookResult.Interrupted())
	r := NewInterruptHookResult("blocked by policy")
	assert.False(t, r.Continue)
	assert.Equal(t, "blocked by policy", r.Output)
}
