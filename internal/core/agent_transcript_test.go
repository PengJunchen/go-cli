package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type agentTranscriptTool struct {
	name    string
	handler func(context.Context, tools.ToolCall) (*tools.ToolResult, error)
}

type scriptedCanonicalAgentLoop struct {
	results    []CanonicalRunResult
	calls      []Submission
	legacyRuns int
}

func (l *scriptedCanonicalAgentLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	l.legacyRuns++
	return nil, nil
}

func (l *scriptedCanonicalAgentLoop) RunCanonical(_ context.Context, submission Submission, _ ...EventStream) (CanonicalRunResult, error) {
	l.calls = append(l.calls, submission)
	return l.results[len(l.calls)-1], nil
}

func (t *agentTranscriptTool) Name() string        { return t.name }
func (t *agentTranscriptTool) Description() string { return t.name + " test tool" }
func (t *agentTranscriptTool) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	return t.handler(ctx, call)
}

var _ tools.ToolDefinition = (*agentTranscriptTool)(nil)

func TestCancelClosesToolCallHistory(t *testing.T) {
	started := make(chan struct{})
	toolRegistry := mock.NewMockToolServer()
	require.NoError(t, toolRegistry.Register(context.Background(), &agentTranscriptTool{
		name: "block",
		handler: func(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}))

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"R1", "cancel-history",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "cancel-call", Name: "block"}}},
	))
	agent := NewAgentImpl("cancel-history", NewLoopAgent(WithLLM(model), WithTools(toolRegistry)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		_, err := agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "run blocking call"})
		runDone <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	select {
	case err := <-runDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not return after cancellation")
	}

	var assistantCalls []llm.ToolCall
	var toolReceipts []AgentMessage
	for _, message := range agent.Messages() {
		if message.Role == "assistant" {
			assistantCalls = append(assistantCalls, message.ToolCalls...)
		}
		if message.Role == "tool" {
			toolReceipts = append(toolReceipts, message)
		}
	}
	require.Len(t, assistantCalls, 1, "the requested tool call must remain in history")
	assert.Equal(t, "cancel-call", assistantCalls[0].ID)
	require.Len(t, toolReceipts, 1, "cancellation must close the assistant tool call with one receipt")
	assert.Equal(t, "cancel-call", toolReceipts[0].ToolCallID)
	assert.Equal(t, "block", toolReceipts[0].ToolName)
}

func TestAgentHistoryIncludesSteeringAndFollowUp(t *testing.T) {
	steerCh := make(chan string, 1)
	steerCh <- "steer toward tests"
	followUpCh := make(chan string, 1)
	followUpCh <- "also explain the result"
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"R1", "steer-followup-history",
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(
		WithLLM(model),
		WithSteeringChannel(steerCh),
		WithFollowUpChannel(followUpCh),
	)
	agent := NewAgentImpl("steer-followup-history", loop)

	_, err := agent.Run(context.Background(), Submission{Type: SubmissionUserMessage, Content: "start"})
	require.NoError(t, err)

	callLog := model.CallLog()
	require.Len(t, callLog, 1)
	var submitted []string
	for _, message := range callLog[0].Messages {
		if message.Role == llm.RoleUser {
			submitted = append(submitted, message.Content)
		}
	}
	assert.Equal(t, []string{"start", "steer toward tests", "also explain the result"}, submitted,
		"the loop must consume steering before follow-up")

	var history []string
	for _, message := range agent.Messages() {
		if message.Role == "user" {
			history = append(history, message.Content)
		}
	}
	assert.Equal(t, submitted, history,
		"the agent history must retain every consumed user, steering, and follow-up input in order")
}

type transcriptCompletionRegistry struct {
	tools.ToolRegistry
	returned map[string]chan struct{}
	once     map[string]*sync.Once
}

func (r transcriptCompletionRegistry) Get(ctx context.Context, name string) (tools.ToolDefinition, error) {
	definition, err := r.ToolRegistry.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return transcriptCompletionTool{
		ToolDefinition: definition,
		returned:       r.returned[name],
		once:           r.once[name],
	}, nil
}

type transcriptCompletionTool struct {
	tools.ToolDefinition
	returned chan struct{}
	once     *sync.Once
}

func (t transcriptCompletionTool) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	result, err := t.ToolDefinition.Execute(ctx, call)
	if t.returned != nil {
		t.once.Do(func() { close(t.returned) })
	}
	return result, err
}

func TestAgentHistoryPairsParallelToolResultsByID(t *testing.T) {
	started := make(chan string, 2)
	releaseFirst := make(chan struct{})
	var releaseFirstOnce sync.Once
	defer releaseFirstOnce.Do(func() { close(releaseFirst) })
	toolRegistry := mock.NewMockToolServer()
	for _, item := range []struct {
		name   string
		output string
	}{
		{name: "slow_tool", output: "slow-result"},
		{name: "fast_tool", output: "fast-result"},
	} {
		item := item
		require.NoError(t, toolRegistry.Register(context.Background(), &agentTranscriptTool{
			name: item.name,
			handler: func(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
				started <- item.name
				if item.name == "slow_tool" {
					select {
					case <-releaseFirst:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return &tools.ToolResult{Output: item.output}, nil
			},
		}))
	}
	fastReturned := make(chan struct{})
	registry := transcriptCompletionRegistry{
		ToolRegistry: toolRegistry,
		returned:     map[string]chan struct{}{"fast_tool": fastReturned},
		once:         map[string]*sync.Once{"fast_tool": new(sync.Once)},
	}
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"R1", "parallel-history",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{
			{ID: "slow-call", Name: "slow_tool"},
			{ID: "fast-call", Name: "fast_tool"},
		}},
		mock.ConversationTurn{AssistantContent: "finished"},
	))
	agent := NewAgentImpl("parallel-history", NewLoopAgent(
		WithLLM(model),
		WithTools(registry),
		WithExecutionMode(ExecutionModeParallel),
	))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		_, err := agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "run both"})
		runDone <- err
	}()

	seen := make(map[string]bool)
	for range 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-ctx.Done():
			t.Fatal("parallel tools did not both start")
		}
	}
	require.True(t, seen["slow_tool"])
	require.True(t, seen["fast_tool"])
	select {
	case <-fastReturned:
	case <-ctx.Done():
		t.Fatal("fast tool did not finish before the gated tool")
	}
	releaseFirstOnce.Do(func() { close(releaseFirst) })
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("agent did not finish after both tools were released")
	}

	var assistantCalls []llm.ToolCall
	var toolReceipts []AgentMessage
	for _, message := range agent.Messages() {
		if message.Role == "assistant" {
			assistantCalls = append(assistantCalls, message.ToolCalls...)
		}
		if message.Role == "tool" {
			toolReceipts = append(toolReceipts, message)
		}
	}
	require.Len(t, assistantCalls, 2, "the assistant's parallel calls must remain in history")
	assert.Equal(t, []string{"slow-call", "fast-call"}, []string{assistantCalls[0].ID, assistantCalls[1].ID})
	require.Len(t, toolReceipts, 2, "each parallel call must have one history receipt")
	assert.Equal(t, []string{"slow-call", "fast-call"}, []string{toolReceipts[0].ToolCallID, toolReceipts[1].ToolCallID})
	assert.Equal(t, []string{"slow-result", "fast-result"}, []string{toolReceipts[0].Content, toolReceipts[1].Content})
}

func TestAgentConsumesCanonicalDeltaAcrossRepeatedUserTurns(t *testing.T) {
	loop := &scriptedCanonicalAgentLoop{results: []CanonicalRunResult{
		{
			Events: []AgentEvent{{Kind: "message", Content: "first answer"}},
			Transcript: []TranscriptEntry{
				{Message: AgentMessage{Role: "user", Content: "repeat", InputKind: InputKindUser}},
				{Message: AgentMessage{Role: "assistant", Content: "first answer"}},
			},
		},
		{
			Events: []AgentEvent{{Kind: "message", Content: "second answer"}},
			Transcript: []TranscriptEntry{
				{Message: AgentMessage{Role: "user", Content: "repeat", InputKind: InputKindUser}},
				{Message: AgentMessage{Role: "assistant", Content: "second answer"}},
			},
		},
	}}
	agent := NewAgentImpl("repeated-user", loop)

	first, err := agent.Run(context.Background(), Submission{Type: SubmissionUserMessage, Content: "repeat"})
	require.NoError(t, err)
	assert.Equal(t, "first answer", first.Message)
	second, err := agent.Run(context.Background(), Submission{Type: SubmissionUserMessage, Content: "repeat"})
	require.NoError(t, err)
	assert.Equal(t, "second answer", second.Message)

	assert.Zero(t, loop.legacyRuns, "canonical loops must be invoked through RunCanonical exactly once")
	require.Len(t, loop.calls, 2)
	var secondInput []AgentMessage
	for _, message := range loop.calls[1].History {
		if message.Role == "user" || message.Role == "assistant" {
			secondInput = append(secondInput, message)
		}
	}
	assert.Equal(t, []AgentMessage{
		{Role: "user", Content: "repeat", InputKind: InputKindUser},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "repeat", InputKind: InputKindUser},
	}, secondInput, "the second turn must receive old history plus exactly one current user message")

	var history []AgentMessage
	for _, message := range agent.Messages() {
		if message.Role == "user" || message.Role == "assistant" {
			history = append(history, message)
		}
	}
	assert.Equal(t, []AgentMessage{
		{Role: "user", Content: "repeat", InputKind: InputKindUser},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "repeat", InputKind: InputKindUser},
		{Role: "assistant", Content: "second answer"},
	}, history, "each canonical per-call delta must append once even when user text repeats")
}

func TestAgentRejectsInvalidCanonicalDelta(t *testing.T) {
	tests := []struct {
		name       string
		transcript []TranscriptEntry
	}{
		{name: "empty"},
		{
			name:       "missing initial user",
			transcript: []TranscriptEntry{{Message: AgentMessage{Role: "assistant", Content: "answer"}}},
		},
		{
			name: "initial user mismatch",
			transcript: []TranscriptEntry{{Message: AgentMessage{
				Role: "user", Content: "different request", InputKind: InputKindUser,
			}}},
		},
		{
			name:       "initial user kind missing",
			transcript: []TranscriptEntry{{Message: AgentMessage{Role: "user", Content: "request"}}},
		},
		{
			name: "duplicate initial user",
			transcript: []TranscriptEntry{
				{Message: AgentMessage{Role: "user", Content: "request", InputKind: InputKindUser}},
				{Message: AgentMessage{Role: "user", Content: "request", InputKind: InputKindUser}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loop := &scriptedCanonicalAgentLoop{results: []CanonicalRunResult{{
				Events:     []AgentEvent{{Kind: "message", Content: "answer"}},
				Transcript: tt.transcript,
			}}}
			baseHistory := []AgentMessage{{Role: "system", Content: "prior context"}}
			agent := NewAgentImpl("invalid-canonical-delta", loop, WithHistory(baseHistory))

			result, err := agent.Run(context.Background(), Submission{Type: SubmissionUserMessage, Content: "request"})

			require.ErrorIs(t, err, ErrCanonicalTranscriptInvalid)
			assert.False(t, result.Success, "an invalid canonical delta must not report success")
			assert.Empty(t, result.Message, "an invalid canonical delta must not expose an unverified assistant answer")
			assert.Equal(t, baseHistory, agent.Messages(), "an invalid delta must not contaminate previously valid history")
		})
	}
}

func TestAgentAcceptsCanonicalDeltaForEmptyUserInput(t *testing.T) {
	loop := &scriptedCanonicalAgentLoop{results: []CanonicalRunResult{{
		Events: []AgentEvent{{Kind: "message", Content: "answer"}},
		Transcript: []TranscriptEntry{
			{Message: AgentMessage{Role: "user", Content: "", InputKind: InputKindUser}},
			{Message: AgentMessage{Role: "assistant", Content: "answer"}},
		},
	}}}
	agent := NewAgentImpl("empty-user-content", loop)

	result, err := agent.Run(context.Background(), Submission{Type: SubmissionUserMessage, Content: ""})

	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "answer", result.Message)
	assert.Equal(t, []AgentMessage{
		{Role: "user", Content: "", InputKind: InputKindUser},
		{Role: "assistant", Content: "answer"},
	}, agent.Messages())
}

func TestAgentCanonicalHistoryDeepCopiesToolCallArgs(t *testing.T) {
	args := map[string]any{"nested": map[string]any{"value": "original"}}
	blocks := []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "original"}}}
	usage := &llm.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}
	loop := &scriptedCanonicalAgentLoop{results: []CanonicalRunResult{{
		Events: []AgentEvent{{Kind: "message", Content: "done"}},
		Transcript: []TranscriptEntry{
			{Message: AgentMessage{Role: "user", Content: "run", InputKind: InputKindUser}},
			{Message: AgentMessage{
				Role:          "assistant",
				ContentBlocks: blocks,
				Usage:         usage,
				ToolCalls:     []llm.ToolCall{{ID: "arg-call", Name: "inspect", Args: args}},
			}},
		},
	}}}
	agent := NewAgentImpl("canonical-copy", loop)
	_, err := agent.Run(context.Background(), Submission{Type: SubmissionUserMessage, Content: "run"})
	require.NoError(t, err)

	// Mutating the CanonicalLoop's returned backing data after RunCanonical
	// returns must not mutate the Agent's committed history.
	args["nested"].(map[string]any)["value"] = "changed by loop"
	blocks[0].ImageURL.URL = "changed by loop"
	usage.OutputTokens = 99

	returned := agent.Messages()
	require.Len(t, returned, 2)
	returnedArgs := returned[1].ToolCalls[0].Args.(map[string]any)
	returnedArgs["nested"].(map[string]any)["value"] = "changed by caller"
	returned[1].ContentBlocks[0].ImageURL.URL = "changed by caller"
	returned[1].Usage.TotalTokens = 99

	stored := agent.Messages()
	storedArgs := stored[1].ToolCalls[0].Args.(map[string]any)
	assert.Equal(t, "original", storedArgs["nested"].(map[string]any)["value"],
		"neither the loop result nor Messages() caller may mutate authoritative history")
	assert.Equal(t, "original", stored[1].ContentBlocks[0].ImageURL.URL)
	assert.Equal(t, 3, stored[1].Usage.TotalTokens)
}
