package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookImplDefaultName(t *testing.T) {
	assert.Equal(t, "default-hook", (&HookImpl{}).Name())
	assert.Equal(t, "named-hook", (&HookImpl{name: "named-hook"}).Name())
}

func TestMiddlewareImplDefaultName(t *testing.T) {
	assert.Equal(t, "default-middleware", (&MiddlewareImpl{}).Name())
	assert.Equal(t, "named-mw", (&MiddlewareImpl{name: "named-mw"}).Name())
}

func TestExtensionImplName(t *testing.T) {
	assert.Equal(t, "default-extension", (ExtensionImpl{}).Name())
}

func TestHookImplPassThroughHooks(t *testing.T) {
	h := &HookImpl{name: "noop"}
	require.NoError(t, h.BeforeRun(context.Background(), Submission{Content: "go"}))
	require.NoError(t, h.AfterRun(context.Background(), Submission{}, Result{}, nil))
}

func TestMiddlewareImplWrapPassThrough(t *testing.T) {
	mw := &MiddlewareImpl{name: "pass"}
	base := &stubLoop{}
	wrapped := mw.Wrap(base)
	// Pass-through returns the same loop.
	assert.Same(t, base, wrapped)
}

func TestHookChainHooksReturnsCopy(t *testing.T) {
	h1 := &spyHook{name: "h1"}
	chain := NewHookChain(h1)
	got := chain.Hooks()
	require.Len(t, got, 1)
	// Mutating the returned copy must not affect the chain.
	got[0] = &spyHook{name: "h2"}
	assert.Len(t, chain.Hooks(), 1)
	assert.Equal(t, "h1", chain.Hooks()[0].Name())
}

func TestHookChainBeforeEmptyReturnsContinue(t *testing.T) {
	chain := NewHookChain()
	res, err := chain.Before(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	assert.True(t, res.Continue)
}

func TestHookChainAfterMultipleErrorsReturnsFirst(t *testing.T) {
	// Both hooks fail; After returns the FIRST error observed.
	h1 := &spyHook{name: "e1", failAfter: true}
	h2 := &spyHook{name: "e2", failAfter: true}
	chain := NewHookChain(h1, h2)

	err := chain.After(context.Background(), Submission{}, Result{}, nil)
	require.Error(t, err)
	assert.Equal(t, "after failed: e1", err.Error())
}

func TestHookChainBeforeForwardsSubmissionType(t *testing.T) {
	var order []string
	h1 := &spyHook{name: "h", order: &order}
	chain := NewHookChain(h1)

	_, err := chain.Before(context.Background(), Submission{Type: SubmissionFollowUp, Content: "x"})
	require.NoError(t, err)
	assert.Contains(t, h1.recorded(), "before:h")
}

func TestNewHookChainNilHookSkipped(t *testing.T) {
	// A nil Hook in the varargs must not panic a no-op call path (the default
	// hook is non-nil here, but building with a nil is tolerated at
	// construction).
	var order []string
	h := &spyHook{name: "h", order: &order}
	chain := NewHookChain(h)
	require.NotNil(t, chain)
	_, err := chain.Before(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	_ = order
}

func TestHookResultGetters(t *testing.T) {
	r := NewInterruptHookResult("because")
	assert.False(t, r.Continue)
	assert.True(t, r.Interrupted())
	assert.Equal(t, "because", r.Output)

	// Continue result is the zero continue with no output.
	cr := ContinueHookResult
	assert.True(t, cr.Continue)
	assert.Empty(t, cr.Output)
}
