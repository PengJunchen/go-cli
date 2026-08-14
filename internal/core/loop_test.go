package core

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// findKind returns the contents of all events whose Kind equals k.
func findEvents(events []AgentEvent, kind string) []string {
	var out []string
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev.Content)
		}
	}
	return out
}

func hasToolMessage(msgs []llm.Message, callID string) bool {
	for _, m := range msgs {
		if m.Role == llm.RoleTool && m.ToolCallID == callID {
			return true
		}
	}
	return false
}

func TestNewLoopAgentDefaults(t *testing.T) {
	loop := NewLoopAgent()
	require.NotNil(t, loop)
	assert.Equal(t, defaultMaxIterations, loop.maxIterations)
}

func TestLoopSingleTurnNoTools(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-01", "single",
		mock.ConversationTurn{AssistantContent: "hello from model"},
	))
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	assert.Equal(t, 1, model.CallCount())

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "hello from model", messages[0])
}

func TestLoopMultiTurnWithToolCall(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("file contents")
	require.NoError(t, err)

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-02", "multiturn",
		mock.ConversationTurn{
			AssistantContent: "let me read the file",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call1", Name: "read_file", Args: map[string]any{"path": "a.go"}},
			},
		},
		mock.ConversationTurn{AssistantContent: "final answer"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	events, err := loop.Run(context.Background(), Submission{Content: "read a.go"})
	require.NoError(t, err)
	assert.Equal(t, 2, model.CallCount())

	// A tool_call event was emitted naming the tool.
	toolCalls := findEvents(events, "tool_call")
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "read_file", toolCalls[0])

	// A tool_result event was fed back.
	toolResults := findEvents(events, "tool_result")
	require.Len(t, toolResults, 1)

	// The final message event is present.
	messages := findEvents(events, "message")
	require.Len(t, messages, 2)
	assert.Equal(t, "final answer", messages[1])
}

func TestLoopToolResultFedBack(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("file contents")
	require.NoError(t, err)

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-03", "feed-back",
		mock.ConversationTurn{
			AssistantContent: "calling",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call-x", Name: "read_file", Args: nil},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	_, err = loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	// The second Generate call must have received the tool-role message
	// carrying the matching tool call id, proving the result was fed back.
	require.Equal(t, 2, model.CallCount())
	secondCall := model.CallLog()[1].Messages
	assert.True(t, hasToolMessage(secondCall, "call-x"))
}

func TestLoopMaxIterationsGuardsRunaway(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("x")
	require.NoError(t, err)

	turns := make([]mock.ConversationTurn, 0, 2)
	for i := 0; i < 2; i++ {
		turns = append(turns, mock.ConversationTurn{
			AssistantContent: "again",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c", Name: "read_file"},
			},
		})
	}

	model := mock.NewMockLLMServer(mock.NewConversationTemplate("L-04", "runaway", turns...))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv), WithMaxIterations(2))

	events, err := loop.Run(context.Background(), Submission{Content: "loop"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errMaxIterations)

	errs := findEvents(events, "error")
	assert.NotEmpty(t, errs)
}

func TestLoopContextCancellation(t *testing.T) {
	model := mock.NewMockLLMServer(nil)
	loop := NewLoopAgent(WithLLM(model))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	events, err := loop.Run(ctx, Submission{Content: "hi"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))

	errs := findEvents(events, "error")
	assert.NotEmpty(t, errs)
}

func TestLoopNilModel(t *testing.T) {
	loop := NewLoopAgent()
	_, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errNilModel)
}

func TestLoopToolExecutionError(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	// No tool registered under the requested name -> Get fails.

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-05", "tool-error",
		mock.ConversationTurn{
			AssistantContent: "calling",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c1", Name: "missing_tool"},
			},
		},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	// Tool-not-found errors are fed back to the model as tool results so
	// the model can adapt. The loop completes cleanly when the model stops
	// calling tools.
	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	results := findEvents(events, "tool_result")
	require.NotEmpty(t, results)
	assert.Contains(t, results[0], "missing_tool")
	assert.Empty(t, findEvents(events, "error"))
}

// countingRegistry wraps a ToolRegistry and counts List calls so tests can
// verify whether the tool-definition cache is working.
type countingRegistry struct {
	inner     tools.ToolRegistry
	listCount int
	mu        sync.Mutex
}

func (r *countingRegistry) Register(ctx context.Context, def tools.ToolDefinition) error {
	return r.inner.Register(ctx, def)
}

func (r *countingRegistry) Get(ctx context.Context, name string) (tools.ToolDefinition, error) {
	return r.inner.Get(ctx, name)
}

func (r *countingRegistry) List(ctx context.Context) ([]tools.ToolDefinition, error) {
	r.mu.Lock()
	r.listCount++
	r.mu.Unlock()
	return r.inner.List(ctx)
}

func (r *countingRegistry) Version() int {
	if v, ok := r.inner.(interface{ Version() int }); ok {
		return v.Version()
	}
	return 0
}

func (r *countingRegistry) ListCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCount
}

// TestLoopToolDefCache_Hit verifies that the tool-definition cache avoids
// rebuilding tool definitions on the second Run call when the registry
// version has not changed.
func TestLoopToolDefCache_Hit(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), &nameDescTool{name: "tool_a", description: "does a"}))
	require.NoError(t, reg.Register(context.Background(), &nameDescTool{name: "tool_b", description: "does b"}))

	cr := &countingRegistry{inner: reg}

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"TC-01", "cache-hit",
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(cr))

	// First Run — cache is cold, List is called by buildToolDefinitions
	// and again by systemPrompt().
	_, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	firstCount := cr.ListCount()
	assert.GreaterOrEqual(t, firstCount, 1, "first Run should call List at least once")

	// Second Run — buildToolDefinitions returns cached definitions (no List
	// call for that path). Only systemPrompt() calls List again, so the
	// count increases by exactly 1.
	_, err = loop.Run(context.Background(), Submission{Content: "hi again"})
	require.NoError(t, err)
	secondCount := cr.ListCount()
	assert.Equal(t, firstCount+1, secondCount,
		"second Run should only call List for systemPrompt (cache hit for tool defs)")
}

// TestLoopToolDefCache_InvalidateOnRegister verifies that registering a new
// tool invalidates the cache so the next Run sees the new tool.
func TestLoopToolDefCache_InvalidateOnRegister(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), &nameDescTool{name: "tool_a", description: "does a"}))

	cr := &countingRegistry{inner: reg}

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"TC-02", "cache-invalidate",
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(cr))

	// First Run — cache is cold.
	_, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	firstCount := cr.ListCount()

	// Register a new tool — this bumps the registry version.
	require.NoError(t, reg.Register(context.Background(), &nameDescTool{name: "tool_b", description: "does b"}))

	// Second Run — cache should be invalidated, List called again.
	_, err = loop.Run(context.Background(), Submission{Content: "hi again"})
	require.NoError(t, err)
	secondCount := cr.ListCount()
	assert.Greater(t, secondCount, firstCount, "second Run should call List after tool registration")

	// Verify the second Run's tools include the newly registered tool.
	callLog := model.CallLog()
	require.NotEmpty(t, callLog)
	passedTools := toolsFromOpts(callLog[len(callLog)-1].Options)
	var names []string
	for _, td := range passedTools {
		names = append(names, td.Name)
	}
	assert.Contains(t, names, "tool_b")
}

// TestLoopToolDefCache_NonVersionedRegistry verifies that a registry without
// Version() still benefits from caching (version stays 0, cache always valid
// after first build).
func TestLoopToolDefCache_NonVersionedRegistry(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("contents")
	require.NoError(t, err)

	cr := &countingRegistry{inner: toolSrv}

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"TC-03", "non-versioned",
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(cr))

	// First Run.
	_, err = loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	firstCount := cr.ListCount()

	// Second Run — MockToolServer has no Version(), so cache is always valid.
	// Only systemPrompt() calls List again (count increases by 1, not 2).
	_, err = loop.Run(context.Background(), Submission{Content: "again"})
	require.NoError(t, err)
	secondCount := cr.ListCount()
	assert.Equal(t, firstCount+1, secondCount,
		"non-versioned registry should still cache tool defs (only systemPrompt calls List)")
}

// TestBuildToolDefinitions_NilTools verifies that buildToolDefinitions
// returns nil when no tool registry is wired.
func TestBuildToolDefinitions_NilTools(t *testing.T) {
	loop := NewLoopAgent()
	defs, err := loop.buildToolDefinitions(context.Background())
	require.NoError(t, err)
	assert.Nil(t, defs)
}
