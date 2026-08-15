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

// --- stubs for bridge preservation tests ---

// bridgePassThroughMiddleware is a pass-through middleware that delegates to
// next without modifying input or output.
type bridgePassThroughMiddleware struct {
	name string
}

var _ extension.Middleware = (*bridgePassThroughMiddleware)(nil)

func (m *bridgePassThroughMiddleware) Name() string { return m.name }

func (m *bridgePassThroughMiddleware) WrapAgent(next extension.AgentFunc) extension.AgentFunc {
	return next
}

// bridgeCaptureLoop records the Submission and EventStream it receives so
// tests can verify what the bridge forwarded to the inner loop.
type bridgeCaptureLoop struct {
	mu        sync.Mutex
	gotSub    core.Submission
	gotStream core.EventStream
	events    []core.AgentEvent
}

func (l *bridgeCaptureLoop) Run(_ context.Context, sub core.Submission, stream ...core.EventStream) ([]core.AgentEvent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gotSub = sub
	if len(stream) > 0 {
		l.gotStream = stream[0]
	}
	return l.events, nil
}

func (l *bridgeCaptureLoop) GotSubmission() core.Submission {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gotSub
}

func (l *bridgeCaptureLoop) GotStream() core.EventStream {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gotStream
}

// bridgeStreamingLoop sends each event to the EventStream before returning
// them, mimicking a real loop that streams in real time.
type bridgeStreamingLoop struct {
	events []core.AgentEvent
}

func (l *bridgeStreamingLoop) Run(_ context.Context, _ core.Submission, stream ...core.EventStream) ([]core.AgentEvent, error) {
	for _, ev := range l.events {
		if len(stream) > 0 && stream[0] != nil {
			_ = stream[0].Send(ev)
		}
	}
	return l.events, nil
}

// --- History preservation tests ---

// The bridge forwards the full conversation History from Submission to the
// inner AgentLoop, not just the latest message.
func TestExtensionBridgeHistoryPassthrough(t *testing.T) {
	mw := &bridgePassThroughMiddleware{name: "pass-mw"}
	adapter := newExtensionMiddlewareAdapter(mw)

	history := []core.AgentMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "second"},
	}
	base := &bridgeCaptureLoop{events: []core.AgentEvent{{Kind: "message", Content: "ok"}}}
	wrapped := adapter.Wrap(base)

	_, err := wrapped.Run(context.Background(), core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "third",
		History: history,
	})
	require.NoError(t, err)

	got := base.GotSubmission()
	require.Len(t, got.History, 3, "full history must be forwarded")
	assert.Equal(t, "first", got.History[0].Content)
	assert.Equal(t, "reply", got.History[1].Content)
	assert.Equal(t, "second", got.History[2].Content)
}

// --- Stream forwarding tests ---

// The bridge forwards the EventStream to the inner loop so events are
// streamed in real time, not buffered and collapsed to a single message.
func TestExtensionBridgeStreamForwarded(t *testing.T) {
	mw := &bridgePassThroughMiddleware{name: "pass-mw"}
	adapter := newExtensionMiddlewareAdapter(mw)

	events := []core.AgentEvent{
		{Kind: "message", Content: "chunk1", Incremental: true},
		{Kind: "message", Content: "chunk2", Incremental: true},
		{Kind: "message", Content: "final answer"},
	}
	base := &bridgeStreamingLoop{events: events}
	wrapped := adapter.Wrap(base)

	stream := core.NewEventStream(16)
	go func() {
		_, _ = wrapped.Run(context.Background(), core.Submission{Content: "hi"}, stream)
		stream.Close()
	}()

	var received []core.AgentEvent
	for ev := range stream.Events() {
		received = append(received, ev)
	}

	require.Len(t, received, 3, "all streaming events must be forwarded")
	assert.Equal(t, "chunk1", received[0].Content)
	assert.Equal(t, "chunk2", received[1].Content)
	assert.Equal(t, "final answer", received[2].Content)
}

// Without a stream, the bridge still returns all events (not just the last
// message) so intermediate tool calls and incremental fragments are preserved.
func TestExtensionBridgeEventsNotTruncated(t *testing.T) {
	mw := &bridgePassThroughMiddleware{name: "pass-mw"}
	adapter := newExtensionMiddlewareAdapter(mw)

	events := []core.AgentEvent{
		{Kind: "tool", Content: "running search"},
		{Kind: "message", Content: "partial", Incremental: true},
		{Kind: "message", Content: "full answer"},
	}
	base := &bridgeCaptureLoop{events: events}
	wrapped := adapter.Wrap(base)

	got, err := wrapped.Run(context.Background(), core.Submission{Content: "hi"})
	require.NoError(t, err)
	require.Len(t, got, 3, "all events must be returned, not just the last message")
	assert.Equal(t, "tool", got[0].Kind)
	assert.Equal(t, "running search", got[0].Content)
	assert.Equal(t, "message", got[2].Kind)
	assert.Equal(t, "full answer", got[2].Content)
}

// --- Submission.Type preservation tests ---

// The bridge preserves steering and followup submission types through the
// extension middleware boundary.
func TestExtensionBridgeSubmissionTypePreserved(t *testing.T) {
	mw := &bridgePassThroughMiddleware{name: "pass-mw"}
	adapter := newExtensionMiddlewareAdapter(mw)

	tests := []struct {
		name string
		typ  core.SubmissionType
		want string
	}{
		{"user", core.SubmissionUserMessage, "user"},
		{"steering", core.SubmissionSteering, "steering"},
		{"followup", core.SubmissionFollowUp, "followup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &bridgeCaptureLoop{events: []core.AgentEvent{{Kind: "message", Content: "ok"}}}
			wrapped := adapter.Wrap(base)

			_, err := wrapped.Run(context.Background(), core.Submission{
				Type:    tt.typ,
				Content: "msg",
			})
			require.NoError(t, err)

			got := base.GotSubmission()
			assert.Equal(t, tt.typ, got.Type, "submission type %s must be preserved", tt.want)
		})
	}
}
