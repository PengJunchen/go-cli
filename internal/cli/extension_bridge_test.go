package cli

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
)

// bridgeTestHook is an extension.Hook that records the events it handles and
// returns a configurable HookResult.
type bridgeTestHook struct {
	mu     sync.Mutex
	name   string
	calls  []string
	action extension.HookAction
	reason string
}

var _ extension.Hook = (*bridgeTestHook)(nil)

func (h *bridgeTestHook) Name() string { return h.name }

func (h *bridgeTestHook) Handle(_ context.Context, event extension.HookEvent) extension.HookResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, event.Name)
	return extension.HookResult{Action: h.action, Reason: h.reason}
}

func (h *bridgeTestHook) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// bridgeTestMiddleware is an extension.Middleware that prefixes the wrapped
// agent output so the onion wrapping is observable.
type bridgeTestMiddleware struct {
	name string
	mu   sync.Mutex
	used bool
}

var _ extension.Middleware = (*bridgeTestMiddleware)(nil)

func (m *bridgeTestMiddleware) Name() string { return m.name }

func (m *bridgeTestMiddleware) WrapAgent(next extension.AgentFunc) extension.AgentFunc {
	m.mu.Lock()
	m.used = true
	m.mu.Unlock()
	return func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		out, err := next(ctx, input)
		if err != nil {
			return out, err
		}
		return extension.AgentOutput{Text: "[mw] " + out.Text}, nil
	}
}

// bridgeStubLoop is a minimal core.AgentLoop used to verify middleware wrapping.
type bridgeStubLoop struct {
	events []core.AgentEvent
	err    error
}

func (l *bridgeStubLoop) Run(_ context.Context, _ core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	return l.events, l.err
}

// A pass-action hook adapter lets BeforeRun proceed (returns nil) and forwards
// the agent.before_run event.
func TestExtensionHookAdapterBeforeRunPass(t *testing.T) {
	hook := &bridgeTestHook{name: "pass-hook", action: extension.HookActionPass}
	adapter := newExtensionHookAdapter(hook)

	err := adapter.BeforeRun(context.Background(), core.Submission{Content: "hi"})
	require.NoError(t, err)

	calls := hook.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "agent.before_run", calls[0])
}

// A non-pass action hook adapter halts BeforeRun with an error carrying the
// hook name and reason.
func TestExtensionHookAdapterBeforeRunBlock(t *testing.T) {
	hook := &bridgeTestHook{name: "block-hook", action: "block", reason: "denied"}
	adapter := newExtensionHookAdapter(hook)

	err := adapter.BeforeRun(context.Background(), core.Submission{Content: "hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "block-hook")
	assert.Contains(t, err.Error(), "denied")
}

// AfterRun forwards the agent.after_run event and does not error on pass.
func TestExtensionHookAdapterAfterRun(t *testing.T) {
	hook := &bridgeTestHook{name: "after-hook", action: extension.HookActionPass}
	adapter := newExtensionHookAdapter(hook)

	err := adapter.AfterRun(context.Background(), core.Submission{Content: "hi"}, core.Result{Success: true, Message: "done"}, nil)
	require.NoError(t, err)

	calls := hook.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "agent.after_run", calls[0])
}

// Name delegates to the underlying extension hook.
func TestExtensionHookAdapterName(t *testing.T) {
	hook := &bridgeTestHook{name: "named-hook", action: extension.HookActionPass}
	adapter := newExtensionHookAdapter(hook)
	assert.Equal(t, "named-hook", adapter.Name())
}

// The middleware adapter wraps the underlying loop so the wrapped output is
// observable, proving the extension.Middleware participates in the onion chain.
func TestExtensionMiddlewareAdapterWrap(t *testing.T) {
	mw := &bridgeTestMiddleware{name: "wrap-mw"}
	adapter := newExtensionMiddlewareAdapter(mw)

	base := &bridgeStubLoop{events: []core.AgentEvent{{Kind: "message", Content: "hello"}}}
	wrapped := adapter.Wrap(base)

	events, err := wrapped.Run(context.Background(), core.Submission{Content: "ping"})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "[mw] hello", events[0].Content)

	assert.True(t, mw.used, "WrapAgent should have been invoked")
}

// Name delegates to the underlying extension middleware.
func TestExtensionMiddlewareAdapterName(t *testing.T) {
	mw := &bridgeTestMiddleware{name: "named-mw"}
	adapter := newExtensionMiddlewareAdapter(mw)
	assert.Equal(t, "named-mw", adapter.Name())
}
