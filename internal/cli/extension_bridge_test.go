package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
)

// bridgeTestHook is an extension.Hook that records the events it handles and
// returns a configurable HookResult.
type bridgeTestHook struct {
	mu     sync.Mutex
	name   string
	calls  []string
	action extension.HookAction
	reason string
	result extension.HookResult
	handle func(context.Context, extension.HookEvent) extension.HookResult
	events []extension.HookEvent
}

var _ extension.Hook = (*bridgeTestHook)(nil)

func (h *bridgeTestHook) Name() string { return h.name }

func (h *bridgeTestHook) Handle(ctx context.Context, event extension.HookEvent) extension.HookResult {
	h.mu.Lock()
	h.calls = append(h.calls, event.Name)
	h.events = append(h.events, event)
	handle := h.handle
	result := h.result
	if result.Action == "" {
		result = extension.HookResult{Action: h.action, Reason: h.reason}
	}
	h.mu.Unlock()
	if handle != nil {
		return handle(ctx, event)
	}
	return result
}

func (h *bridgeTestHook) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *bridgeTestHook) Events() []extension.HookEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]extension.HookEvent(nil), h.events...)
}

func bridgeHookAdapter(hook extension.Hook) *extensionHookAdapter {
	return newExtensionHookAdapter(extension.HookRegistration{Owner: "registered-owner", Hook: hook})
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
	adapter := bridgeHookAdapter(hook)

	err := adapter.BeforeRun(context.Background(), core.Submission{Content: "hi"})
	require.NoError(t, err)

	calls := hook.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "agent.before_run", calls[0])
	event := hook.Events()[0]
	assert.Equal(t, "registered-owner", event.Source)
	_, ok := event.Data.(extension.HookBeforeRunSnapshot)
	assert.True(t, ok, "the event data must be the documented concrete snapshot")
}

// A non-pass action hook adapter halts BeforeRun with an error carrying the
// hook name and reason.
func TestExtensionHookAdapterBeforeRunBlock(t *testing.T) {
	const reasonCanary = "hook-reason-secret-71f6"
	hook := &bridgeTestHook{name: "block-hook", action: extension.HookActionBlock, reason: reasonCanary}
	adapter := bridgeHookAdapter(hook)

	err := adapter.BeforeRun(context.Background(), core.Submission{Content: "hi"})
	require.ErrorIs(t, err, extension.ErrHookBlocked)
	assert.NotContains(t, err.Error(), reasonCanary)
	assert.NotContains(t, err.Error(), "block-hook")
}

// AfterRun forwards the agent.after_run event and does not error on pass.
func TestExtensionHookAdapterAfterRun(t *testing.T) {
	hook := &bridgeTestHook{name: "after-hook", action: extension.HookActionPass}
	adapter := bridgeHookAdapter(hook)

	err := adapter.AfterRun(context.Background(), core.Submission{Content: "hi"}, core.Result{Success: true, Message: "done"}, nil)
	require.NoError(t, err)

	calls := hook.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "agent.after_run", calls[0])
}

// Name delegates to the underlying extension hook.
func TestExtensionHookAdapterName(t *testing.T) {
	hook := &bridgeTestHook{name: "named-hook", action: extension.HookActionPass}
	adapter := bridgeHookAdapter(hook)
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

type invocationCountingExtensionMiddleware struct{ calls int }

func (*invocationCountingExtensionMiddleware) Name() string { return "invocation-counting" }

func (m *invocationCountingExtensionMiddleware) WrapAgent(next extension.AgentFunc) extension.AgentFunc {
	return func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		m.calls++
		return next(ctx, input)
	}
}

func TestExtensionMiddlewareFailsClosedBeforeCanonicalLoopExecution(t *testing.T) {
	middleware := &invocationCountingExtensionMiddleware{}
	base := &canonicalAssemblyTestLoop{}
	wrapped := core.NewMiddlewareChain(newExtensionMiddlewareAdapter(middleware)).Wrap(base)

	canonical, ok := wrapped.(core.CanonicalLoop)
	require.True(t, ok)
	result, err := canonical.RunCanonical(context.Background(), core.Submission{Content: "request"})

	require.ErrorIs(t, err, core.ErrCanonicalMiddlewareUnsupported)
	require.Len(t, result.Transcript, 1)
	assert.Equal(t, core.InputKindUser, result.Transcript[0].Message.InputKind)
	assert.Zero(t, base.calls, "the wrapped loop must not start under event-only extension middleware")
	assert.Zero(t, middleware.calls, "unsupported extension middleware must not run")
}

// Name delegates to the underlying extension middleware.
func TestExtensionMiddlewareAdapterName(t *testing.T) {
	mw := &bridgeTestMiddleware{name: "named-mw"}
	adapter := newExtensionMiddlewareAdapter(mw)
	assert.Equal(t, "named-mw", adapter.Name())
}

func TestHookSnapshotDeepCopyIsolatedPerObserver(t *testing.T) {
	submission := core.Submission{
		Content:  "request",
		Metadata: map[string]any{"nested": map[string]any{"items": []any{"original", int64(7)}}},
		History: []core.AgentMessage{{
			Role: "assistant", Content: "history",
			ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://image.invalid/a", Detail: "high"}}},
			Usage:         &llm.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5},
			ToolCalls:     []llm.ToolCall{{ID: "call-1", Name: "lookup", Args: map[string]any{"nested": []any{true, "before"}}}},
			ToolCallID:    "call-1", ToolName: "lookup", InputKind: "followup", ReceiptOutcome: "success",
		}},
	}
	first := &bridgeTestHook{name: "first", action: extension.HookActionPass, handle: func(_ context.Context, event extension.HookEvent) extension.HookResult {
		before := event.Data.(extension.HookBeforeRunSnapshot)
		before.Submission.Metadata["nested"].(map[string]any)["items"].([]any)[0] = "mutated"
		before.Submission.Messages[0].ContentBlocks[0].ImageURL.URL = "mutated-url"
		before.Submission.Messages[0].Usage.InputTokens = 999
		before.Submission.Messages[0].ToolCalls[0].Args.(map[string]any)["nested"].([]any)[1] = "mutated-args"
		return extension.HookResult{Action: extension.HookActionPass}
	}}
	second := &bridgeTestHook{name: "second", action: extension.HookActionPass}
	require.NoError(t, bridgeHookAdapter(first).BeforeRun(context.Background(), submission))
	require.NoError(t, bridgeHookAdapter(second).BeforeRun(context.Background(), submission))

	secondSnapshot := second.Events()[0].Data.(extension.HookBeforeRunSnapshot).Submission
	assert.Equal(t, "original", secondSnapshot.Metadata["nested"].(map[string]any)["items"].([]any)[0])
	assert.Equal(t, "https://image.invalid/a", secondSnapshot.Messages[0].ContentBlocks[0].ImageURL.URL)
	assert.Equal(t, 2, secondSnapshot.Messages[0].Usage.InputTokens)
	assert.Equal(t, "before", secondSnapshot.Messages[0].ToolCalls[0].Args.(map[string]any)["nested"].([]any)[1])
	assert.Equal(t, "original", submission.Metadata["nested"].(map[string]any)["items"].([]any)[0])
	assert.Equal(t, "https://image.invalid/a", submission.History[0].ContentBlocks[0].ImageURL.URL)
	assert.Equal(t, 2, submission.History[0].Usage.InputTokens)
	assert.Equal(t, "before", submission.History[0].ToolCalls[0].Args.(map[string]any)["nested"].([]any)[1])
}

type bridgeUnsupportedValue struct{ secret string }

type bridgeCanaryError struct {
	secret string
	calls  int
}

func (e *bridgeCanaryError) Error() string {
	e.calls++
	return e.secret
}

type bridgeWrappedCanaryError struct{ cause error }

func (e bridgeWrappedCanaryError) Error() string { return "provider-secret-error-canary" }
func (e bridgeWrappedCanaryError) Unwrap() error { return e.cause }

func TestHookSnapshotRejectsUnsupportedPayload(t *testing.T) {
	const canary = "unsupported-payload-canary-882a"
	cycle := map[string]any{}
	cycle["self"] = cycle
	canaryErr := &bridgeCanaryError{secret: canary}
	cases := []struct {
		name  string
		value any
	}{
		{name: "cycle", value: cycle},
		{name: "struct", value: bridgeUnsupportedValue{secret: canary}},
		{name: "pointer", value: &bridgeUnsupportedValue{secret: canary}},
		{name: "error", value: canaryErr},
		{name: "non-string map key", value: map[any]any{canary: true}},
		{name: "non-finite number", value: math.NaN()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			hook := &bridgeTestHook{name: "rejecting", action: extension.HookActionPass}
			err := bridgeHookAdapter(hook).BeforeRun(context.Background(), core.Submission{Metadata: map[string]any{"secret": testCase.value}})
			require.ErrorIs(t, err, extension.ErrHookPayloadUnsupported)
			assert.NotContains(t, err.Error(), canary)
			assert.Empty(t, hook.Calls(), "the observer must not be called for an unsupported payload")
		})
	}
	assert.Zero(t, canaryErr.calls, "the bridge must reject error values without calling Error")

	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	hook := &bridgeTestHook{name: "rejecting", action: extension.HookActionPass}
	_, err := core.NewHookChain(bridgeHookAdapter(hook)).Before(context.Background(), core.Submission{Metadata: map[string]any{"secret": &bridgeUnsupportedValue{secret: canary}}})
	require.ErrorIs(t, err, extension.ErrHookPayloadUnsupported)
	assert.NotContains(t, logs.String(), canary)
}

func nestedHookMaps(count int) map[string]any {
	var value any = "leaf"
	for range count {
		value = map[string]any{"k": value}
	}
	return value.(map[string]any)
}

func TestHookSnapshotEnforcesPayloadLimits(t *testing.T) {
	t.Run("depth exact and one over", func(t *testing.T) {
		exactHook := &bridgeTestHook{name: "exact-depth", action: extension.HookActionPass}
		require.NoError(t, bridgeHookAdapter(exactHook).BeforeRun(context.Background(), core.Submission{Metadata: nestedHookMaps(60)}))
		assert.Len(t, exactHook.Calls(), 1)
		overHook := &bridgeTestHook{name: "over-depth", action: extension.HookActionPass}
		err := bridgeHookAdapter(overHook).BeforeRun(context.Background(), core.Submission{Metadata: nestedHookMaps(61)})
		require.ErrorIs(t, err, extension.ErrHookPayloadLimitExceeded)
		assert.Empty(t, overHook.Calls())
	})

	t.Run("node exact and one over", func(t *testing.T) {
		makeMetadata := func(extraItems int) map[string]any {
			metadata := make(map[string]any, 32762)
			for index := 0; index < 32761; index++ {
				metadata[fmt.Sprintf("k%05d", index)] = true
			}
			items := make([]any, extraItems)
			for index := range items {
				items[index] = true
			}
			metadata["extra"] = items
			return metadata
		}
		exactHook := &bridgeTestHook{name: "exact-nodes", action: extension.HookActionPass}
		require.NoError(t, bridgeHookAdapter(exactHook).BeforeRun(context.Background(), core.Submission{Metadata: makeMetadata(2)}))
		assert.Len(t, exactHook.Calls(), 1)
		overHook := &bridgeTestHook{name: "over-nodes", action: extension.HookActionPass}
		err := bridgeHookAdapter(overHook).BeforeRun(context.Background(), core.Submission{Metadata: makeMetadata(3)})
		require.ErrorIs(t, err, extension.ErrHookPayloadLimitExceeded)
		assert.Empty(t, overHook.Calls())
	})

	t.Run("aggregate bytes exact and one over", func(t *testing.T) {
		contentLength := hookPayloadMaxBytes - len("agent.before_run") - len("user")
		exactHook := &bridgeTestHook{name: "exact-bytes", action: extension.HookActionPass}
		exactAdapter := newExtensionHookAdapter(extension.HookRegistration{Hook: exactHook})
		require.NoError(t, exactAdapter.BeforeRun(context.Background(), core.Submission{Content: strings.Repeat("x", contentLength)}))
		assert.Len(t, exactHook.Calls(), 1)
		overHook := &bridgeTestHook{name: "over-bytes", action: extension.HookActionPass}
		overAdapter := newExtensionHookAdapter(extension.HookRegistration{Hook: overHook})
		err := overAdapter.BeforeRun(context.Background(), core.Submission{Content: strings.Repeat("x", contentLength+1)})
		require.ErrorIs(t, err, extension.ErrHookPayloadLimitExceeded)
		assert.Empty(t, overHook.Calls())
	})
}

func TestHookErrorSnapshotOmitsRawError(t *testing.T) {
	const secret = "provider-secret-error-canary-2049"
	secretErr := &bridgeCanaryError{secret: secret}
	runErr := &bridgeWrappedCanaryError{cause: secretErr}
	first := &bridgeTestHook{name: "observer-one", action: extension.HookActionPass, handle: func(_ context.Context, event extension.HookEvent) extension.HookResult {
		if event.Name == "agent.after_run" {
			snapshot := event.Data.(extension.HookAfterRunSnapshot)
			assert.True(t, snapshot.Error.Present)
			assert.Equal(t, extension.HookErrorClassUnknown, snapshot.Error.Class)
		}
		if event.Name == "agent.after_run" {
			return extension.HookResult{Action: extension.HookActionBlock, Reason: secret}
		}
		return extension.HookResult{Action: extension.HookActionPass}
	}}
	second := &bridgeTestHook{name: "observer-two", action: extension.HookActionPass}
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	baseEvents := []core.AgentEvent{{Kind: "message", Content: "original result"}}
	wrapped := core.NewHookMiddleware(core.NewHookChain(bridgeHookAdapter(first), bridgeHookAdapter(second))).Wrap(&bridgeStubLoop{events: baseEvents, err: runErr})
	gotEvents, gotErr := wrapped.Run(context.Background(), core.Submission{Content: "original request"})
	require.Same(t, runErr, gotErr, "AfterRun observer errors must preserve the original run error")
	assert.Equal(t, baseEvents, gotEvents, "AfterRun observer errors must preserve the original run result")
	assert.Equal(t, []string{"agent.before_run", "agent.after_run"}, first.Calls())
	assert.Equal(t, []string{"agent.before_run", "agent.after_run"}, second.Calls(), "later observers still run after a failure")
	assert.Zero(t, secretErr.calls, "error classification must not call Error")
	assert.NotContains(t, logs.String(), secret)
	assert.NotContains(t, logs.String(), "provider-secret-error-canary")
}

func TestHookErrorClassesUseStableAllowlist(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want extension.HookErrorClass
	}{
		{name: "canceled", err: context.Canceled, want: extension.HookErrorClassCanceled},
		{name: "deadline", err: context.DeadlineExceeded, want: extension.HookErrorClassDeadline},
		{name: "blocked sentinel", err: extension.ErrHookBlocked, want: extension.HookErrorClassBlocked},
		{name: "blocked typed family", err: &core.AgentHarnessError{Code: core.ErrCodeHookRejected, Message: "ignored"}, want: extension.HookErrorClassBlocked},
		{name: "deadline typed family", err: &core.AgentHarnessError{Code: core.ErrCodeTimeout, Message: "ignored"}, want: extension.HookErrorClassDeadline},
		{name: "action", err: extension.ErrUnsupportedHookAction, want: extension.HookErrorClassUnsupportedAction},
		{name: "payload", err: extension.ErrHookPayloadUnsupported, want: extension.HookErrorClassUnsupported},
		{name: "limit", err: extension.ErrHookPayloadLimitExceeded, want: extension.HookErrorClassUnsupported},
		{name: "unknown", err: errors.New("not exposed"), want: extension.HookErrorClassUnknown},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := classifyHookError(testCase.err)
			assert.True(t, got.Present)
			assert.Equal(t, testCase.want, got.Class)
		})
	}
	assert.Equal(t, extension.HookErrorSnapshot{}, classifyHookError(nil))
}

func TestHookActionsAreBounded(t *testing.T) {
	for _, action := range []extension.HookAction{"terminate", "replace", "future"} {
		t.Run(string(action), func(t *testing.T) {
			hook := &bridgeTestHook{name: "unsupported-action", result: extension.HookResult{Action: action}}
			err := bridgeHookAdapter(hook).BeforeRun(context.Background(), core.Submission{})
			require.ErrorIs(t, err, extension.ErrUnsupportedHookAction)
		})
	}
	t.Run("pass with replacement", func(t *testing.T) {
		hook := &bridgeTestHook{name: "replacement", result: extension.HookResult{Action: extension.HookActionPass, Replacement: "ignored"}}
		err := bridgeHookAdapter(hook).BeforeRun(context.Background(), core.Submission{})
		require.ErrorIs(t, err, extension.ErrUnsupportedHookAction)
	})
	t.Run("block with replacement", func(t *testing.T) {
		hook := &bridgeTestHook{name: "replacement", result: extension.HookResult{Action: extension.HookActionBlock, Replacement: "ignored"}}
		err := bridgeHookAdapter(hook).BeforeRun(context.Background(), core.Submission{})
		require.ErrorIs(t, err, extension.ErrUnsupportedHookAction)
	})
}

func TestHookRunSnapshotReportsOnlyAvailableRunFields(t *testing.T) {
	ctx := context.Background()
	hook := &bridgeTestHook{name: "run-snapshot", action: extension.HookActionPass, handle: func(_ context.Context, event extension.HookEvent) extension.HookResult {
		if event.Name != "agent.after_run" {
			return extension.HookResult{Action: extension.HookActionPass}
		}
		run := event.Data.(extension.HookAfterRunSnapshot).Run
		assert.Empty(t, run.RunID, "result.RunID is not a source of owner identity")
		assert.False(t, run.RunIDAvailable)
		assert.Equal(t, extension.HookRunOutcome("success"), run.Outcome)
		assert.True(t, run.OutcomeAvailable)
		assert.Equal(t, extension.HookRunExecutionState("quiescent"), run.ExecutionState)
		assert.True(t, run.ExecutionStateAvailable)
		return extension.HookResult{Action: extension.HookActionPass}
	}}
	adapter := bridgeHookAdapter(hook)
	require.NoError(t, adapter.AfterRun(ctx, core.Submission{}, core.Result{
		RunID: "must-not-copy-result-run-id", Outcome: core.RunOutcomeSuccess, ExecutionState: core.RunExecutionQuiescent, Success: true,
	}, nil))
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
