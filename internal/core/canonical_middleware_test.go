package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

type canonicalMiddlewareLoopStub struct {
	canonicalCalls int
	legacyCalls    int
	submissions    []Submission
	results        []CanonicalRunResult
	errors         []error
}

func (l *canonicalMiddlewareLoopStub) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	l.legacyCalls++
	return nil, nil
}

func (l *canonicalMiddlewareLoopStub) RunCanonical(_ context.Context, submission Submission, _ ...EventStream) (CanonicalRunResult, error) {
	index := l.canonicalCalls
	l.canonicalCalls++
	l.submissions = append(l.submissions, submission)
	var result CanonicalRunResult
	if index < len(l.results) {
		result = l.results[index]
	}
	var err error
	if index < len(l.errors) {
		err = l.errors[index]
	}
	return result, err
}

type unsupportedCanonicalMiddleware struct{ runCalls int }

func (*unsupportedCanonicalMiddleware) Name() string { return "unsupported-canonical" }

func (m *unsupportedCanonicalMiddleware) Wrap(next AgentLoop) AgentLoop {
	return &countingUnsupportedMiddlewareLoop{middleware: m, next: next}
}

type countingUnsupportedMiddlewareLoop struct {
	middleware *unsupportedCanonicalMiddleware
	next       AgentLoop
}

func (l *countingUnsupportedMiddlewareLoop) Run(ctx context.Context, submission Submission, stream ...EventStream) ([]AgentEvent, error) {
	l.middleware.runCalls++
	return l.next.Run(ctx, submission, stream...)
}

type rejectBeforeCanonicalHook struct{ calls int }

func (h *rejectBeforeCanonicalHook) Name() string { return "reject-before-canonical" }

func (h *rejectBeforeCanonicalHook) BeforeRun(context.Context, Submission) error {
	h.calls++
	return errors.New("blocked before loop")
}

func (*rejectBeforeCanonicalHook) AfterRun(context.Context, Submission, Result, error) error {
	return nil
}

func TestMiddlewareChainPreservesCanonicalLoopOnlyWhenSupported(t *testing.T) {
	base := &canonicalMiddlewareLoopStub{results: []CanonicalRunResult{{
		Transcript: []TranscriptEntry{
			{Message: AgentMessage{Role: "user", Content: "request", InputKind: InputKindUser}},
			{Message: AgentMessage{Role: "assistant", Content: "answer"}},
		},
	}}}
	wrapped := NewMiddlewareChain(NewLoggingMiddleware("canonical-test")).Wrap(base)

	canonical, ok := wrapped.(CanonicalLoop)
	require.True(t, ok, "logging middleware must preserve the typed transcript capability")
	result, err := canonical.RunCanonical(context.Background(), Submission{Content: "request"})
	require.NoError(t, err)
	assert.Equal(t, 1, base.canonicalCalls)
	assert.Zero(t, base.legacyCalls)
	assert.Len(t, result.Transcript, 2)

	legacyWrapped := NewMiddlewareChain(NewLoggingMiddleware("legacy-test")).Wrap(countingLoop{})
	_, legacyClaimsCanonical := legacyWrapped.(CanonicalLoop)
	assert.False(t, legacyClaimsCanonical, "wrapping a legacy loop must not advertise canonical support")
}

func TestMiddlewareChainUnsupportedWrapperFailsBeforeCanonicalExecution(t *testing.T) {
	base := &canonicalMiddlewareLoopStub{}
	unsupported := &unsupportedCanonicalMiddleware{}
	wrapped := NewMiddlewareChain(NewLoggingMiddleware("outer"), unsupported).Wrap(base)

	canonical, ok := wrapped.(CanonicalLoop)
	require.True(t, ok, "a canonical inner loop must fail closed through an unsupported wrapper")
	result, err := canonical.RunCanonical(context.Background(), Submission{Content: "request"})

	require.ErrorIs(t, err, ErrCanonicalMiddlewareUnsupported)
	require.Len(t, result.Transcript, 1)
	assert.Equal(t, AgentMessage{Role: "user", Content: "request", InputKind: InputKindUser}, result.Transcript[0].Message)
	assert.Zero(t, base.canonicalCalls, "the side-effecting canonical loop must not start")
	assert.Zero(t, base.legacyCalls)
	assert.Zero(t, unsupported.runCalls, "unsupported middleware must not start")

	agent := NewAgentImpl("unsupported-canonical", wrapped)
	agentResult, agentErr := agent.Run(context.Background(), Submission{Content: "request"})
	require.ErrorIs(t, agentErr, ErrCanonicalMiddlewareUnsupported)
	assert.False(t, agentResult.Success)
	assert.Equal(t, []AgentMessage{{Role: "user", Content: "request", InputKind: InputKindUser}}, agent.Messages(),
		"a known pre-dispatch rejection commits the user input without quarantining an invalid delta")
	assert.Zero(t, base.canonicalCalls)
	assert.Zero(t, unsupported.runCalls)
}

func TestCanonicalHookRejectionReturnsUserDeltaWithoutStartingLoop(t *testing.T) {
	hook := &rejectBeforeCanonicalHook{}
	base := &canonicalMiddlewareLoopStub{}
	wrapped := NewMiddlewareChain(NewHookMiddleware(NewHookChain(hook))).Wrap(base)

	canonical, ok := wrapped.(CanonicalLoop)
	require.True(t, ok, "hook middleware must preserve canonical support")
	result, err := canonical.RunCanonical(context.Background(), Submission{Content: "request"})

	require.ErrorContains(t, err, "blocked before loop")
	require.Len(t, result.Transcript, 1)
	assert.Equal(t, InputKindUser, result.Transcript[0].Message.InputKind)
	assert.Equal(t, "request", result.Transcript[0].Message.Content)
	assert.Equal(t, 1, hook.calls)
	assert.Zero(t, base.canonicalCalls, "a before-hook rejection must not start the loop")
}

func TestCanonicalSystemReminderInjectorPreservesHistoryTransformation(t *testing.T) {
	manager := NewDefaultSystemReminderManager()
	manager.AddReminder(SystemReminder{ID: "canonical-reminder", Content: "remember this"})
	base := &canonicalMiddlewareLoopStub{results: []CanonicalRunResult{{
		Transcript: []TranscriptEntry{
			{Message: AgentMessage{Role: "user", Content: "request", InputKind: InputKindUser}},
			{Message: AgentMessage{Role: "assistant", Content: "done"}},
		},
	}}}
	wrapped := NewMiddlewareChain(NewSystemReminderInjector(manager)).Wrap(base)
	canonical, ok := wrapped.(CanonicalLoop)
	require.True(t, ok)

	_, err := canonical.RunCanonical(context.Background(), Submission{
		Content: "request",
		History: []AgentMessage{{Role: "user", Content: "earlier"}},
	})
	require.NoError(t, err)
	require.Len(t, base.submissions, 1)
	require.Len(t, base.submissions[0].History, 2)
	assert.Equal(t, AgentMessage{Role: "system", Content: "remember this"}, base.submissions[0].History[0])
	assert.Equal(t, "earlier", base.submissions[0].History[1].Content)
}

func TestCanonicalFailureSynthesisDoesNotRetryOrRebuildTranscriptFromEvents(t *testing.T) {
	first := CanonicalRunResult{
		Events: []AgentEvent{
			{Kind: "message", Content: "event projection must not become history"},
		},
		Transcript: []TranscriptEntry{
			{Message: AgentMessage{Role: "user", Content: "request", InputKind: InputKindUser}},
			{Message: AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "bash"}}}},
			{Message: AgentMessage{Role: "tool", ToolCallID: "call_1", ToolName: "bash", ReceiptOutcome: ReceiptOutcomeUnknown}},
		},
	}
	base := &canonicalMiddlewareLoopStub{
		results: []CanonicalRunResult{first, {
			Transcript: []TranscriptEntry{{Message: AgentMessage{Role: "user", Content: "retry", InputKind: InputKindUser}}},
		}},
		errors: []error{errors.New("connection timeout"), nil},
	}
	wrapped := NewMiddlewareChain(NewFailureSynthesisMiddleware(NewDefaultFailureTurnSynthesizer())).Wrap(base)

	canonical, ok := wrapped.(CanonicalLoop)
	require.True(t, ok, "failure synthesis must preserve canonical support")
	result, err := canonical.RunCanonical(context.Background(), Submission{Content: "request"})

	require.ErrorContains(t, err, "connection timeout")
	assert.Equal(t, 1, base.canonicalCalls, "unknown tool outcome must never be retried")
	assert.Equal(t, first, result, "typed transcript must be returned unchanged; event projections are not history authority")
}

type canonicalPrivacyHook struct {
	beforeErr error
	afterErr  error
}

func (*canonicalPrivacyHook) Name() string { return "privacy-test" }

func (h *canonicalPrivacyHook) BeforeRun(context.Context, Submission) error { return h.beforeErr }

func (h *canonicalPrivacyHook) AfterRun(context.Context, Submission, Result, error) error {
	return h.afterErr
}

type alwaysRecoverableSynthesizer struct{}

func (alwaysRecoverableSynthesizer) Synthesize(context.Context, error) (SynthesizedMessage, error) {
	return SynthesizedMessage{}, nil
}

func (alwaysRecoverableSynthesizer) IsRecoverable(error) bool { return true }

func TestCanonicalMiddlewareLogsErrorMetadataWithoutRawText(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	marker := strings.Repeat("z", 32) + strconv.FormatInt(time.Now().UnixNano(), 10)
	synthesisErr := errors.New(marker)
	synthesisBase := &canonicalMiddlewareLoopStub{errors: []error{synthesisErr}}
	synthesisWrapped := NewMiddlewareChain(NewFailureSynthesisMiddleware(alwaysRecoverableSynthesizer{})).Wrap(synthesisBase)
	synthesisCanonical, ok := synthesisWrapped.(CanonicalLoop)
	require.True(t, ok)
	_, _ = synthesisCanonical.RunCanonical(context.Background(), Submission{Content: "request"})

	beforeHook := &canonicalPrivacyHook{beforeErr: errors.New(marker + "-before")}
	beforeWrapped := NewMiddlewareChain(NewHookMiddleware(NewHookChain(beforeHook))).Wrap(&canonicalMiddlewareLoopStub{})
	beforeCanonical, ok := beforeWrapped.(CanonicalLoop)
	require.True(t, ok)
	_, _ = beforeCanonical.RunCanonical(context.Background(), Submission{Content: "request"})

	afterHook := &canonicalPrivacyHook{afterErr: errors.New(marker + "-after")}
	afterBase := &canonicalMiddlewareLoopStub{results: []CanonicalRunResult{{
		Transcript: []TranscriptEntry{{Message: AgentMessage{Role: "user", Content: "request", InputKind: InputKindUser}}},
	}}}
	afterWrapped := NewMiddlewareChain(NewHookMiddleware(NewHookChain(afterHook))).Wrap(afterBase)
	afterCanonical, ok := afterWrapped.(CanonicalLoop)
	require.True(t, ok)
	_, _ = afterCanonical.RunCanonical(context.Background(), Submission{Content: "request"})

	logged := output.String()
	assert.NotContains(t, logged, marker, "canonical logs must not contain caller-controlled error text")
	assert.Contains(t, logged, "error_type=*errors.errorString")
	assert.Contains(t, logged, "error_len="+strconv.Itoa(len(marker)))
	assert.Contains(t, logged, "core.failure_synthesis.canonical_retry_skipped")
	assert.Contains(t, logged, "core.hookchain.before.halt")
	assert.Contains(t, logged, "core.hookchain.after.error")
	assert.Contains(t, logged, "core.hook.after_error")
}
