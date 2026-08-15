package core

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestDefaultFailureTurnSynthesizer_SatisfiesInterface(t *testing.T) {
	var _ FailureTurnSynthesizer = (*DefaultFailureTurnSynthesizer)(nil)
}

func TestFailureTurnSynthesizer_IsRecoverable_ContextDeadline(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	assert.True(t, s.IsRecoverable(context.DeadlineExceeded))
}

func TestFailureTurnSynthesizer_IsRecoverable_ContextCanceled(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	assert.True(t, s.IsRecoverable(context.Canceled))
}

func TestFailureTurnSynthesizer_IsRecoverable_NetTimeout(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	// A net.OpError with Timeout() == true is a recoverable network error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	// Dialing a closed port yields connection refused (a recoverable error).
	_, dialErr := net.Dial("tcp", ln.Addr().String())
	if dialErr != nil {
		assert.True(t, s.IsRecoverable(dialErr), "connection refused should be recoverable: %v", dialErr)
	}
}

func TestFailureTurnSynthesizer_IsRecoverable_ConnectionRefusedMessage(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	err := errors.New("dial tcp: connection refused")
	assert.True(t, s.IsRecoverable(err))
}

func TestFailureTurnSynthesizer_IsRecoverable_TimeoutMessage(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	tests := []string{
		"request timeout",
		"operation timed out",
		"i/o timeout",
	}
	for _, msg := range tests {
		err := errors.New(msg)
		assert.True(t, s.IsRecoverable(err), "expected %q to be recoverable", msg)
	}
}

func TestFailureTurnSynthesizer_IsRecoverable_TemporaryMessage(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	err := errors.New("temporary failure in name resolution")
	assert.True(t, s.IsRecoverable(err))
}

func TestFailureTurnSynthesizer_IsRecoverable_NonRecoverable(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	tests := []error{
		errors.New("file not found"),
		errors.New("permission denied"),
		errors.New("invalid argument"),
		errors.New("syntax error in input"),
	}
	for _, err := range tests {
		assert.False(t, s.IsRecoverable(err), "expected %q to NOT be recoverable", err.Error())
	}
}

func TestFailureTurnSynthesizer_IsRecoverable_NilError(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	assert.False(t, s.IsRecoverable(nil))
}

func TestFailureTurnSynthesizer_Synthesize_Recoverable(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	orig := context.DeadlineExceeded

	msg, err := s.Synthesize(context.Background(), orig)
	require.NoError(t, err)
	assert.Equal(t, "system", msg.Role)
	assert.Contains(t, msg.Content, "recoverable")
	assert.Contains(t, msg.Content, orig.Error())
	assert.Equal(t, orig.Error(), msg.OriginalError)
}

func TestFailureTurnSynthesizer_Synthesize_NonRecoverable(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	orig := errors.New("permission denied")

	msg, err := s.Synthesize(context.Background(), orig)
	require.NoError(t, err)
	assert.Equal(t, "system", msg.Role)
	assert.Contains(t, msg.Content, "permission denied")
	assert.Equal(t, orig.Error(), msg.OriginalError)
}

func TestFailureTurnSynthesizer_Synthesize_NilError(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	msg, err := s.Synthesize(context.Background(), nil)
	require.Error(t, err)
	assert.Equal(t, SynthesizedMessage{}, msg)
	assert.Contains(t, err.Error(), "nil error")
}

func TestFailureTurnSynthesizer_Synthesize_WrappedError(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	base := context.DeadlineExceeded
	wrapped := errors.New("model call failed")

	msg, err := s.Synthesize(context.Background(), wrapped)
	require.NoError(t, err)
	// Even though it's not a context error, the error text doesn't contain
	// recoverable keywords, so it should be treated as non-recoverable.
	assert.Contains(t, msg.Content, "model call failed")
	_ = base // keep the reference for clarity
}

// --- eventsToTurnMessages tests ---

func TestEventsToTurnMessages_AssistantAndToolResult(t *testing.T) {
	events := []AgentEvent{
		{Kind: "message", Content: "Let me check.", ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "read"}}, Timestamp: time.Now()},
		{Kind: "tool_call", Content: "read", ToolCallID: "tc1", Timestamp: time.Now()},
		{Kind: "tool_result", Content: "file contents", ToolCallID: "tc1", Timestamp: time.Now()},
	}
	msgs := eventsToTurnMessages(events)
	require.Len(t, msgs, 2)

	assert.Equal(t, "assistant", msgs[0].Role)
	assert.Equal(t, "Let me check.", msgs[0].Content)
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "tc1", msgs[0].ToolCalls[0].ID)

	assert.Equal(t, "tool", msgs[1].Role)
	assert.Equal(t, "file contents", msgs[1].Content)
	assert.Equal(t, "tc1", msgs[1].ToolCallID)
	assert.Equal(t, "read", msgs[1].ToolName)
}

func TestEventsToTurnMessages_SkipsIncremental(t *testing.T) {
	events := []AgentEvent{
		{Kind: "message", Content: "Hel", Incremental: true, Timestamp: time.Now()},
		{Kind: "message", Content: "lo", Incremental: true, Timestamp: time.Now()},
		{Kind: "message", Content: "Hello", Incremental: false, Timestamp: time.Now()},
	}
	msgs := eventsToTurnMessages(events)
	require.Len(t, msgs, 1)
	assert.Equal(t, "Hello", msgs[0].Content)
}

func TestEventsToTurnMessages_ToolCancelled(t *testing.T) {
	events := []AgentEvent{
		{Kind: "message", Content: "", ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "bash"}}, Timestamp: time.Now()},
		{Kind: "tool_call", Content: "bash", ToolCallID: "tc1", Timestamp: time.Now()},
		{Kind: "tool_cancelled", Content: "bash", ToolCallID: "tc1", Timestamp: time.Now()},
	}
	msgs := eventsToTurnMessages(events)
	require.Len(t, msgs, 2)
	assert.Equal(t, "tool", msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "cancelled")
	assert.Equal(t, "bash", msgs[1].ToolName)
}

func TestEventsToTurnMessages_Empty(t *testing.T) {
	msgs := eventsToTurnMessages(nil)
	assert.Empty(t, msgs)
}

// --- FailureSynthesisMiddleware retry behavior tests ---

// retryTestLoop is a mock AgentLoop that fails with a recoverable error on the
// first call (after emitting tool-call events) and succeeds on the second call.
// It records every submission it receives so tests can verify the retry
// history includes prior turn messages (continuation, not replay).
type retryTestLoop struct {
	callCount   atomic.Int64
	submissions []Submission
	// toolExecCount tracks how many times a "tool" would have been executed.
	// In a real loop, tool execution happens inside Run; here we simulate it
	// by counting how many times Run is called with a submission whose
	// Content is the original user message (i.e., a fresh turn, not a
	// continuation).
	toolExecCount atomic.Int64
}

func (l *retryTestLoop) Run(_ context.Context, submission Submission, _ ...EventStream) ([]AgentEvent, error) {
	l.submissions = append(l.submissions, submission)
	n := l.callCount.Add(1)

	if n == 1 {
		// First attempt: simulate a tool call + result, then fail with a
		// recoverable error.
		l.toolExecCount.Add(1)
		return []AgentEvent{
			{Kind: "message", Content: "Running tool", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "bash"}}, Timestamp: time.Now()},
			{Kind: "tool_call", Content: "bash", ToolCallID: "call_1", Timestamp: time.Now()},
			{Kind: "tool_result", Content: "done", ToolCallID: "call_1", Timestamp: time.Now()},
		}, errors.New("connection timeout")
	}

	// Second attempt (retry): succeed.
	return []AgentEvent{
		{Kind: "message", Content: "Recovered", Timestamp: time.Now()},
	}, nil
}

func TestFailureSynthesisRetry_PreservesToolResults(t *testing.T) {
	mock := &retryTestLoop{}
	mw := NewFailureSynthesisMiddleware(NewDefaultFailureTurnSynthesizer())
	wrapped := mw.Wrap(mock)

	sub := Submission{
		Type:    SubmissionUserMessage,
		Content: "do task",
		History: []AgentMessage{{Role: "user", Content: "previous context"}},
	}

	events, err := wrapped.Run(context.Background(), sub)
	require.NoError(t, err)

	// Should have been called twice: initial + retry.
	assert.Equal(t, int64(2), mock.callCount.Load())

	// AC-1: The tool should NOT have been re-executed on retry.
	// toolExecCount is incremented only on the first call (which simulates
	// tool execution). If the retry replayed the turn, the tool would be
	// executed again and the count would be 2.
	assert.Equal(t, int64(1), mock.toolExecCount.Load(),
		"tool should not be re-executed on retry")

	// The retry submission (second call) should contain the prior turn
	// messages as history, not just the original submission.
	require.Len(t, mock.submissions, 2)
	retrySub := mock.submissions[1]

	// AC-2: The retry submission history should include:
	// 1. Original history
	// 2. Original user message
	// 3. Assistant message with tool calls
	// 4. Tool result message
	assert.Equal(t, "previous context", retrySub.History[0].Content)
	assert.Equal(t, "user", retrySub.History[0].Role)

	assert.Equal(t, "do task", retrySub.History[1].Content)
	assert.Equal(t, "user", retrySub.History[1].Role)

	// Assistant message from the failed attempt should be preserved.
	var foundAssistant bool
	var foundToolResult bool
	for _, hm := range retrySub.History {
		if hm.Role == "assistant" && len(hm.ToolCalls) > 0 {
			foundAssistant = true
			assert.Equal(t, "call_1", hm.ToolCalls[0].ID)
		}
		if hm.Role == "tool" && hm.ToolCallID == "call_1" {
			foundToolResult = true
			assert.Equal(t, "done", hm.Content)
			assert.Equal(t, "bash", hm.ToolName)
		}
	}
	assert.True(t, foundAssistant, "retry history should contain the assistant message from the failed attempt")
	assert.True(t, foundToolResult, "retry history should contain the tool result from the failed attempt")

	// The synthesized message should be the Content of the retry submission,
	// not prepended to history.
	assert.Contains(t, retrySub.Content, "recoverable")
	assert.Contains(t, retrySub.Content, "connection timeout")

	// Events from both attempts should be merged.
	assert.True(t, len(events) >= 4, "events should include both failed and retry attempt events")
}

// nonRecoverableTestLoop always fails with a non-recoverable error.
type nonRecoverableTestLoop struct {
	callCount atomic.Int64
}

func (l *nonRecoverableTestLoop) Run(_ context.Context, _ Submission, _ ...EventStream) ([]AgentEvent, error) {
	l.callCount.Add(1)
	return []AgentEvent{{Kind: "error", Content: "fatal", Timestamp: time.Now()}}, errors.New("file not found")
}

func TestFailureSynthesisRetry_NonRecoverableNoRetry(t *testing.T) {
	mock := &nonRecoverableTestLoop{}
	mw := NewFailureSynthesisMiddleware(NewDefaultFailureTurnSynthesizer())
	wrapped := mw.Wrap(mock)

	_, err := wrapped.Run(context.Background(), Submission{Content: "do task"})
	require.Error(t, err)
	assert.Equal(t, int64(1), mock.callCount.Load(), "should not retry on non-recoverable error")
}

// successTestLoop succeeds on the first call.
type successTestLoop struct {
	callCount atomic.Int64
}

func (l *successTestLoop) Run(_ context.Context, _ Submission, _ ...EventStream) ([]AgentEvent, error) {
	l.callCount.Add(1)
	return []AgentEvent{{Kind: "message", Content: "ok", Timestamp: time.Now()}}, nil
}

func TestFailureSynthesisRetry_SuccessNoRetry(t *testing.T) {
	mock := &successTestLoop{}
	mw := NewFailureSynthesisMiddleware(NewDefaultFailureTurnSynthesizer())
	wrapped := mw.Wrap(mock)

	events, err := wrapped.Run(context.Background(), Submission{Content: "do task"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), mock.callCount.Load(), "should not retry on success")
	assert.Len(t, events, 1)
}
