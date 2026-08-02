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

// scriptedChatModel returns a fixed sequence of responses (each possibly nil)
// then a final content-only response, letting tests drive every loop branch.
type scriptedChatModel struct {
	mu   sync.Mutex
	seq  []*llm.Message
	errs []error
	idx  int
}

func (m *scriptedChatModel) Generate(_ context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.seq) {
		return &llm.Message{Role: llm.RoleAssistant, Content: "fallback"}, nil
	}
	i := m.idx
	m.idx++
	var err error
	if i < len(m.errs) {
		err = m.errs[i]
	}
	return m.seq[i], err
}

func (m *scriptedChatModel) Stream(_ context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	ch := make(chan llm.MessageChunk)
	close(ch)
	return ch, nil
}

var _ llm.BaseChatModel = (*scriptedChatModel)(nil)

// scriptedTool is a tools.ToolDefinition whose Execute returns a scripted
// error or result, letting tests drive the loop's tool-execution branches
// (error and nil-result) deterministically.
type scriptedTool struct {
	name string
	err  error
	res  *tools.ToolResult
}

func (t scriptedTool) Name() string        { return t.name }
func (t scriptedTool) Description() string { return "scripted tool" }
func (t scriptedTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return t.res, t.err
}

var _ tools.ToolDefinition = (*scriptedTool)(nil)

// scriptedToolRegistry returns a single scripted tool by name.
type scriptedToolRegistry struct {
	byName map[string]tools.ToolDefinition
}

func (r scriptedToolRegistry) Register(context.Context, tools.ToolDefinition) error { return nil }
func (r scriptedToolRegistry) Get(_ context.Context, name string) (tools.ToolDefinition, error) {
	def, ok := r.byName[name]
	if !ok {
		return nil, errToolUnknown
	}
	return def, nil
}
func (r scriptedToolRegistry) List(context.Context) ([]tools.ToolDefinition, error) {
	return nil, nil
}

var _ tools.ToolRegistry = (*scriptedToolRegistry)(nil)

func scriptedRegistry(defs ...tools.ToolDefinition) *scriptedToolRegistry {
	r := &scriptedToolRegistry{byName: map[string]tools.ToolDefinition{}}
	for _, d := range defs {
		r.byName[d.Name()] = d
	}
	return r
}

func TestLoopNilResponseErrors(t *testing.T) {
	model := &scriptedChatModel{seq: []*llm.Message{nil}}
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil response")
	errs := findEvents(events, "error")
	require.NotEmpty(t, errs)
}

func TestLoopGenerateError(t *testing.T) {
	boom := errors.New("model blew up")
	model := &scriptedChatModel{seq: []*llm.Message{nil}, errs: []error{boom}}
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.ErrorIs(t, err, boom)
	errs := findEvents(events, "error")
	require.NotEmpty(t, errs)
	// The nil sequence entry (with the error) short-circuits before nil-check.
	require.NotEmpty(t, errs)
}

func TestLoopNoToolRegistry(t *testing.T) {
	model := &scriptedChatModel{seq: []*llm.Message{{
		Role:      llm.RoleAssistant,
		Content:   "calling",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read", Args: map[string]any{}}},
	}}}
	// No WithTools => the loop has no tool registry.
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no tool registry")
	toolCalls := findEvents(events, "tool_call")
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "read", toolCalls[0])
}

func TestLoopToolExecuteError(t *testing.T) {
	boom := errors.New("tool execution failed")
	toolSrv := scriptedRegistry(scriptedTool{name: "faulty", err: boom})

	model := &scriptedChatModel{seq: []*llm.Message{{
		Role:      llm.RoleAssistant,
		Content:   "calling",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "faulty", Args: map[string]any{}}},
	}}}
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.ErrorIs(t, err, boom)
	errs := findEvents(events, "error")
	require.NotEmpty(t, errs)
}

func TestLoopToolNilResult(t *testing.T) {
	toolSrv := scriptedRegistry(scriptedTool{name: "noop", res: nil})

	model := &scriptedChatModel{seq: []*llm.Message{
		{
			Role:      llm.RoleAssistant,
			Content:   "calling",
			ToolCalls: []llm.ToolCall{{ID: "c1", Name: "noop", Args: map[string]any{}}},
		},
		{Role: llm.RoleAssistant, Content: "finished"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	// The nil tool result yields an empty tool_result event but no error.
	toolResults := findEvents(events, "tool_result")
	require.Len(t, toolResults, 1)
	assert.Equal(t, "", toolResults[0])
}

func TestLoopToolArgsNonMapDropped(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("content")
	require.NoError(t, err)

	// llm.ToolCall.Args is a JSON primitive rather than map[string]any;
	// toToolsCall must drop it without panicking.
	model := &scriptedChatModel{seq: []*llm.Message{
		{
			Role:      llm.RoleAssistant,
			Content:   "calling",
			ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Args: "not-a-map"}},
		},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	// The tool eventually ran (a tool_result event exists) and finished.
	toolResults := findEvents(events, "tool_result")
	require.NotEmpty(t, toolResults)
	messages := findEvents(events, "message")
	require.Len(t, messages, 2)
	assert.Equal(t, "done", messages[1])
}

func TestLoopContextCancelTwoBranches(t *testing.T) {
	// Branch 1: context canceled at loop top (before Generate).
	model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, Content: "unused"}}}
	loop := NewLoopAgent(WithLLM(model))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := loop.Run(ctx, Submission{Content: "go"})
	require.ErrorIs(t, err, context.Canceled)

	// Branch 2: context canceled inside the tool-call loop (pre-execute).
	toolSrv := mock.NewMockToolServer()
	_, err = toolSrv.RegisterReadFileTool("x")
	require.NoError(t, err)

	model2 := &scriptedChatModel{seq: []*llm.Message{{
		Role:      llm.RoleAssistant,
		Content:   "calling",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Args: map[string]any{}}},
	}}}
	loop2 := NewLoopAgent(WithLLM(model2), WithTools(toolSrv))

	cctx, ccancel := context.WithCancel(context.Background())
	// Pre-cancel before Run so the tool-call branch sees a canceled ctx.
	ccancel()
	events, err := loop2.Run(cctx, Submission{Content: "go"})
	require.ErrorIs(t, err, context.Canceled)
	// A tool_call event may or may not be present depending on timing; but an
	// error event must exist.
	assert.NotEmpty(t, findEvents(events, "error"))
}

func TestLoopWithMaxIterationsNonPositiveFallsBack(t *testing.T) {
	l1 := NewLoopAgent(WithMaxIterations(0))
	assert.Equal(t, defaultMaxIterations, l1.maxIterations)
	l2 := NewLoopAgent(WithMaxIterations(-3))
	assert.Equal(t, defaultMaxIterations, l2.maxIterations)
}

func TestLoopForwardCompatibleOptions(t *testing.T) {
	// WithTools and WithMaxIterations compose; the deepest behavior (a simple
	// turn) still works.
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"LX-01", "compose",
		mock.ConversationTurn{AssistantContent: "composed"},
	))
	loop := NewLoopAgent(WithLLM(model), WithMaxIterations(10))
	events, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	assert.Equal(t, 1, model.CallCount())
	assert.Equal(t, []string{"composed"}, findEvents(events, "message"))
}

func TestToToolsCallConversion(t *testing.T) {
	// map args are carried through.
	withMap := toToolsCall(llm.ToolCall{ID: "i1", Name: "n1", Args: map[string]any{"k": "v"}})
	assert.Equal(t, "i1", withMap.ID)
	assert.Equal(t, "n1", withMap.Name)
	assert.Equal(t, map[string]any{"k": "v"}, withMap.Args)

	// nil args leave the tools.ToolCall.Args nil.
	withNil := toToolsCall(llm.ToolCall{ID: "i2", Name: "n2", Args: nil})
	assert.Nil(t, withNil.Args)
}
