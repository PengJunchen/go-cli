package core

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/stretchr/testify/require"
)

type canonicalTranscriptTestModel struct{ calls int }

func (m *canonicalTranscriptTestModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return nil, fmt.Errorf("Generate must not be used by the stream loop")
}

func (m *canonicalTranscriptTestModel) Stream(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
	m.calls++
	chunks := make(chan llm.MessageChunk, 2)
	chunks <- llm.MessageChunk{Role: llm.RoleAssistant, Content: fmt.Sprintf("answer-%d", m.calls)}
	chunks <- llm.MessageChunk{Role: llm.RoleAssistant, Final: true, FinishReason: "stop"}
	close(chunks)
	return chunks, nil
}

func TestCanonicalLoopTranscriptIsPerCall(t *testing.T) {
	steer := make(chan string, 1)
	steer <- "steer now"
	followUp := make(chan string, 1)
	followUp <- "also do this"
	loop := NewLoopAgent(
		WithLLM(&canonicalTranscriptTestModel{}),
		WithSteeringChannel(steer),
		WithFollowUpChannel(followUp),
	)
	canonical, ok := any(loop).(CanonicalLoop)
	require.True(t, ok, "LoopAgent must expose the typed per-call canonical transcript")

	first, err := canonical.RunCanonical(context.Background(), Submission{
		Content: "first request",
		History: []AgentMessage{{Role: "user", Content: "old request"}, {Role: "assistant", Content: "old answer"}},
	})
	require.NoError(t, err)
	require.Equal(t, []TranscriptEntry{
		{Message: AgentMessage{Role: "user", Content: "first request", InputKind: InputKindUser}},
		{Message: AgentMessage{Role: "user", Content: "steer now", InputKind: InputKindSteering}},
		{Message: AgentMessage{Role: "user", Content: "also do this", InputKind: InputKindFollowUp}},
		{Message: AgentMessage{Role: "assistant", Content: "answer-1"}},
	}, first.Transcript)

	second, err := canonical.RunCanonical(context.Background(), Submission{
		Content: "second request",
		History: []AgentMessage{{Role: "user", Content: "first request"}, {Role: "assistant", Content: "answer-1"}},
	})
	require.NoError(t, err)
	require.Equal(t, []TranscriptEntry{
		{Message: AgentMessage{Role: "user", Content: "second request", InputKind: InputKindUser}},
		{Message: AgentMessage{Role: "assistant", Content: "answer-2"}},
	}, second.Transcript)
}

func TestCanonicalLoopIncludesUserWhenModelIsMissing(t *testing.T) {
	loop := NewLoopAgent()
	result, err := loop.RunCanonical(context.Background(), Submission{Content: "keep this input"})
	require.ErrorIs(t, err, errNilModel)
	require.Equal(t, []TranscriptEntry{{Message: AgentMessage{
		Role:      string(llm.RoleUser),
		Content:   "keep this input",
		InputKind: InputKindUser,
	}}}, result.Transcript)
	require.Empty(t, result.Events)
}

type pendingTestError struct{ cause error }

func (e *pendingTestError) Error() string        { return "tool completion is unknown" }
func (e *pendingTestError) Unwrap() error        { return e.cause }
func (e *pendingTestError) OutcomeUnknown() bool { return true }

func TestCanonicalLoopStopsAfterUnknownSequentialToolOutcome(t *testing.T) {
	cause := errors.New("transport lost after remote action")
	unknownErr := &pendingTestError{cause: cause}
	var unknownSideEffects atomic.Int32
	var laterSideEffects atomic.Int32
	registry := mock.NewMockToolServer()
	require.NoError(t, registry.Register(context.Background(), &testToolDef{
		name: "uncertain",
		handler: func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			unknownSideEffects.Add(1)
			return nil, unknownErr
		},
	}))
	require.NoError(t, registry.Register(context.Background(), &testToolDef{
		name: "later",
		handler: func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			laterSideEffects.Add(1)
			return &tools.ToolResult{Output: "later ran"}, nil
		},
	}))
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "uncertain-call", Name: "uncertain", Args: map[string]any{}},
			{ID: "later-call", Name: "later", Args: map[string]any{}},
		}},
		{Role: llm.RoleAssistant, Content: "the model would retry the uncertain call"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(registry))
	result, err := loop.RunCanonical(context.Background(), Submission{Content: "run tools"})
	require.ErrorIs(t, err, cause)
	var pending *pendingTestError
	require.ErrorAs(t, err, &pending, "the original typed unknown-outcome error must remain inspectable")
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, err, &unknown)
	require.True(t, unknown.OutcomeUnknown())
	require.Equal(t, 1, model.idx, "an unknown tool outcome must stop before another provider call")
	require.EqualValues(t, 1, unknownSideEffects.Load())
	require.Zero(t, laterSideEffects.Load(), "remaining calls must not be dispatched")
	require.Len(t, result.Transcript, 4)
	require.Equal(t, ReceiptOutcomeUnknown, result.Transcript[2].Message.ReceiptOutcome)
	require.Equal(t, "uncertain-call", result.Transcript[2].Message.ToolCallID)
	require.Equal(t, ReceiptOutcomeInterrupted, result.Transcript[3].Message.ReceiptOutcome)
	require.Equal(t, "later-call", result.Transcript[3].Message.ToolCallID)
}

func TestCanonicalLoopClosesParallelReceiptsOnUnknownToolOutcome(t *testing.T) {
	cause := errors.New("remote receipt unavailable")
	unknownErr := &pendingTestError{cause: cause}
	var sideEffects atomic.Int32
	registry := mock.NewMockToolServer()
	require.NoError(t, registry.Register(context.Background(), &testToolDef{
		name: "uncertain",
		handler: func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			sideEffects.Add(1)
			return nil, unknownErr
		},
	}))
	require.NoError(t, registry.Register(context.Background(), &testToolDef{
		name: "completed",
		handler: func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			sideEffects.Add(1)
			return &tools.ToolResult{Output: "completed"}, nil
		},
	}))
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "unknown-call", Name: "uncertain", Args: map[string]any{}},
			{ID: "completed-call", Name: "completed", Args: map[string]any{}},
		}},
		{Role: llm.RoleAssistant, Content: "must not be requested"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel))
	result, err := loop.RunCanonical(context.Background(), Submission{Content: "run tools"})
	require.ErrorIs(t, err, cause)
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, err, &unknown)
	require.True(t, unknown.OutcomeUnknown())
	require.Equal(t, 1, model.idx, "parallel unknown outcome must stop before another provider call")
	require.EqualValues(t, 2, sideEffects.Load(), "all already dispatched parallel calls must be accounted for")
	require.Len(t, result.Transcript, 4)
	var outcomes = map[string]string{}
	for _, entry := range result.Transcript {
		if entry.Message.Role == string(llm.RoleTool) {
			outcomes[entry.Message.ToolCallID] = entry.Message.ReceiptOutcome
		}
	}
	require.Equal(t, ReceiptOutcomeUnknown, outcomes["unknown-call"])
	require.Equal(t, ReceiptOutcomeSuccess, outcomes["completed-call"])
}

func TestCanonicalLoopMarksParallelCancellationSnapshotUnknown(t *testing.T) {
	started := make(chan struct{}, 1)
	completed := make(chan struct{}, 1)
	registry := mock.NewMockToolServer()
	require.NoError(t, registry.Register(context.Background(), &testToolDef{
		name: "wait",
		handler: func(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}))
	require.NoError(t, registry.Register(context.Background(), &testToolDef{
		name: "complete",
		handler: func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			completed <- struct{}{}
			return &tools.ToolResult{Output: "completed"}, nil
		},
	}))
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "wait-call", Name: "wait", Args: map[string]any{}},
			{ID: "complete-call", Name: "complete", Args: map[string]any{}},
		}},
		{Role: llm.RoleAssistant, Content: "must not be requested"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type runResult struct {
		result CanonicalRunResult
		err    error
	}
	runDone := make(chan runResult, 1)
	go func() {
		result, err := loop.RunCanonical(ctx, Submission{Content: "run tools"})
		runDone <- runResult{result: result, err: err}
	}()
	<-started
	<-completed
	cancel()
	run := <-runDone
	require.ErrorIs(t, run.err, context.Canceled)
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, run.err, &unknown)
	require.True(t, unknown.OutcomeUnknown())
	require.Equal(t, 1, model.idx, "the loop must not ask the provider to retry after an incomplete execution snapshot")
	outcomes := map[string]string{}
	for _, entry := range run.result.Transcript {
		if entry.Message.Role == string(llm.RoleTool) {
			outcomes[entry.Message.ToolCallID] = entry.Message.ReceiptOutcome
		}
	}
	require.Equal(t, ReceiptOutcomeUnknown, outcomes["wait-call"])
	require.Equal(t, ReceiptOutcomeSuccess, outcomes["complete-call"])
}
