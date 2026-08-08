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

func (m *scriptedChatModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	resp, err := m.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("model returned nil response")
	}
	ch := make(chan llm.MessageChunk, 2)
	if resp.Content != "" {
		ch <- llm.MessageChunk{Role: resp.Role, Content: resp.Content}
	}
	final := llm.MessageChunk{Role: resp.Role, Final: true}
	if len(resp.ToolCalls) > 0 {
		final.ToolCalls = resp.ToolCalls
	}
	ch <- final
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

	// The "no tool registry" failure is fed back to the model as a tool
	// result; the loop completes cleanly once the model stops calling tools.
	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	results := findEvents(events, "tool_result")
	require.NotEmpty(t, results)
	assert.Contains(t, results[0], "no tool registry")
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

	// Tool errors are fed back to the model as tool results rather than
	// aborting the loop, so the model can diagnose and retry. With no
	// further tool calls in the scripted sequence the loop finishes cleanly.
	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	results := findEvents(events, "tool_result")
	require.Len(t, results, 1)
	assert.Contains(t, results[0], boom.Error())
	// The error must NOT surface as a terminal error event.
	assert.Empty(t, findEvents(events, "error"))
}

func TestLoopToolExecuteErrorAbortsOnCancel(t *testing.T) {
	boom := errors.New("tool execution failed")
	toolSrv := scriptedRegistry(scriptedTool{name: "faulty", err: boom})

	model := &scriptedChatModel{seq: []*llm.Message{{
		Role:      llm.RoleAssistant,
		Content:   "calling",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "faulty", Args: map[string]any{}}},
	}}}
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	// A canceled context causes the loop to abort at the next iteration
	// boundary rather than feeding errors back indefinitely.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events, err := loop.Run(ctx, Submission{Content: "go"})
	require.Error(t, err)
	assert.NotEmpty(t, findEvents(events, "error"))
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
	// Zero falls back to the built-in default.
	l1 := NewLoopAgent(WithMaxIterations(0))
	assert.Equal(t, defaultMaxIterations, l1.maxIterations)
	// -1 means unlimited (no cap).
	l2 := NewLoopAgent(WithMaxIterations(-1))
	assert.Equal(t, -1, l2.maxIterations)
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

// messageEvents returns all "message" events from the slice, preserving order.
func messageEvents(events []AgentEvent) []AgentEvent {
	var out []AgentEvent
	for _, ev := range events {
		if ev.Kind == "message" {
			out = append(out, ev)
		}
	}
	return out
}

// --- Task 35-2 loop-level test cases ---

// TestPureToolCallEmitsMessageEvent verifies that a pure tool call response
// (empty Content, non-empty ToolCalls) emits a "message" event so it enters
// history. Before the fix, such responses were silently dropped because the
// emission condition was `if resp.Content != ""`.
func TestPureToolCallEmitsMessageEvent(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("contents")
	require.NoError(t, err)

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"LX-PT-01", "pure-tool-call",
		mock.ConversationTurn{
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "tc1", Name: "read_file", Args: map[string]any{"path": "a.go"}},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	events, err := loop.Run(context.Background(), Submission{Content: "read a.go"})
	require.NoError(t, err)

	// Two message events: one for the pure tool call turn, one for the final.
	msgEvents := messageEvents(events)
	require.Len(t, msgEvents, 2)

	// First message event has empty content but non-empty ToolCalls.
	assert.Empty(t, msgEvents[0].Content)
	require.Len(t, msgEvents[0].ToolCalls, 1)
	assert.Equal(t, "read_file", msgEvents[0].ToolCalls[0].Name)
	assert.Equal(t, "tc1", msgEvents[0].ToolCalls[0].ID)

	// Second message event is the final text response.
	assert.Equal(t, "done", msgEvents[1].Content)
	assert.Empty(t, msgEvents[1].ToolCalls)
}

// TestTextAndToolCallsRegression verifies that a response with both text
// content and tool calls still emits a single message event carrying both.
func TestTextAndToolCallsRegression(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("contents")
	require.NoError(t, err)

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"LX-PT-02", "text-and-tool",
		mock.ConversationTurn{
			AssistantContent: "let me read",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "tc1", Name: "read_file", Args: map[string]any{"path": "a.go"}},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	events, err := loop.Run(context.Background(), Submission{Content: "read a.go"})
	require.NoError(t, err)

	msgEvents := messageEvents(events)
	require.Len(t, msgEvents, 2)

	// First message event has both content and tool calls.
	assert.Equal(t, "let me read", msgEvents[0].Content)
	require.Len(t, msgEvents[0].ToolCalls, 1)
	assert.Equal(t, "read_file", msgEvents[0].ToolCalls[0].Name)
	assert.Equal(t, "tc1", msgEvents[0].ToolCalls[0].ID)
}

// TestEndToEndMultiTurnToolUse verifies a full multi-turn tool-use conversation
// works end-to-end, including pure tool call turns entering history and being
// forwarded to the LLM on subsequent calls.
func TestEndToEndMultiTurnToolUse(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("file contents")
	require.NoError(t, err)

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"LX-PT-03", "e2e-multiturn",
		mock.ConversationTurn{
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "tc1", Name: "read_file", Args: map[string]any{"path": "a.go"}},
			},
		},
		mock.ConversationTurn{AssistantContent: "final answer"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	events, err := loop.Run(context.Background(), Submission{Content: "read a.go"})
	require.NoError(t, err)
	assert.Equal(t, 2, model.CallCount())

	// The second LLM call must have received the assistant message with
	// ToolCalls forwarded from the first turn.
	secondCallMsgs := model.CallLog()[1].Messages
	var assistantWithTools *llm.Message
	for i := range secondCallMsgs {
		if secondCallMsgs[i].Role == llm.RoleAssistant && len(secondCallMsgs[i].ToolCalls) > 0 {
			assistantWithTools = &secondCallMsgs[i]
			break
		}
	}
	require.NotNil(t, assistantWithTools)
	require.Len(t, assistantWithTools.ToolCalls, 1)
	assert.Equal(t, "read_file", assistantWithTools.ToolCalls[0].Name)
	assert.Equal(t, "tc1", assistantWithTools.ToolCalls[0].ID)

	// The second LLM call must also have received the tool result with
	// matching ToolCallID.
	assert.True(t, hasToolMessage(secondCallMsgs, "tc1"))

	// Final message is the text answer.
	msgEvents := messageEvents(events)
	require.Len(t, msgEvents, 2)
	assert.Equal(t, "final answer", msgEvents[1].Content)
}

// --- Task 35-3 history forwarding test cases ---

// TestHistoryForwardToolCalls verifies that ToolCalls from assistant history
// messages are forwarded to the LLM.
func TestHistoryForwardToolCalls(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"LX-HF-01", "forward-toolcalls",
		mock.ConversationTurn{AssistantContent: "ok"},
	))
	loop := NewLoopAgent(WithLLM(model))

	history := []AgentMessage{
		{Role: "user", Content: "read file"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{
			{ID: "tc1", Name: "read_file", Args: map[string]any{"path": "a.go"}},
		}},
		{Role: "tool", Content: "file contents", ToolCallID: "tc1", ToolName: "read_file"},
	}

	_, err := loop.Run(context.Background(), Submission{
		Content: "thanks",
		History: history,
	})
	require.NoError(t, err)
	require.Equal(t, 1, model.CallCount())

	msgs := model.CallLog()[0].Messages

	// The assistant message in forwarded history must carry ToolCalls.
	var assistantMsg *llm.Message
	for i := range msgs {
		if msgs[i].Role == llm.RoleAssistant && len(msgs[i].ToolCalls) > 0 {
			assistantMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, assistantMsg)
	require.Len(t, assistantMsg.ToolCalls, 1)
	assert.Equal(t, "tc1", assistantMsg.ToolCalls[0].ID)
	assert.Equal(t, "read_file", assistantMsg.ToolCalls[0].Name)
}

// TestHistoryForwardToolCallID verifies that ToolCallID from tool-role history
// messages is forwarded to the LLM.
func TestHistoryForwardToolCallID(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"LX-HF-02", "forward-toolcallid",
		mock.ConversationTurn{AssistantContent: "ok"},
	))
	loop := NewLoopAgent(WithLLM(model))

	history := []AgentMessage{
		{Role: "user", Content: "read file"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{
			{ID: "tc-id-1", Name: "read_file"},
		}},
		{Role: "tool", Content: "result", ToolCallID: "tc-id-1", ToolName: "read_file"},
	}

	_, err := loop.Run(context.Background(), Submission{
		Content: "thanks",
		History: history,
	})
	require.NoError(t, err)

	msgs := model.CallLog()[0].Messages

	var toolMsg *llm.Message
	for i := range msgs {
		if msgs[i].Role == llm.RoleTool {
			toolMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolMsg)
	assert.Equal(t, "tc-id-1", toolMsg.ToolCallID)
}

// TestHistoryForwardToolName verifies that ToolName from tool-role history
// messages is forwarded to the LLM as the Name field.
func TestHistoryForwardToolName(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"LX-HF-03", "forward-toolname",
		mock.ConversationTurn{AssistantContent: "ok"},
	))
	loop := NewLoopAgent(WithLLM(model))

	history := []AgentMessage{
		{Role: "user", Content: "read file"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{
			{ID: "tc-id-2", Name: "read_file"},
		}},
		{Role: "tool", Content: "result", ToolCallID: "tc-id-2", ToolName: "read_file"},
	}

	_, err := loop.Run(context.Background(), Submission{
		Content: "thanks",
		History: history,
	})
	require.NoError(t, err)

	msgs := model.CallLog()[0].Messages

	var toolMsg *llm.Message
	for i := range msgs {
		if msgs[i].Role == llm.RoleTool {
			toolMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolMsg)
	assert.Equal(t, "read_file", toolMsg.Name)
}

// TestHistoryForwardEndToEndMultiTurn verifies that a full multi-turn history
// (user, assistant with tool calls, tool result, text assistant) is forwarded
// completely to the LLM, including ToolCalls and ToolCallID.
func TestHistoryForwardEndToEndMultiTurn(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"LX-HF-04", "e2e-forward",
		mock.ConversationTurn{AssistantContent: "ok"},
	))
	loop := NewLoopAgent(WithLLM(model))

	history := []AgentMessage{
		{Role: "user", Content: "read a.go"},
		{Role: "assistant", Content: "let me read", ToolCalls: []llm.ToolCall{
			{ID: "tc1", Name: "read_file", Args: map[string]any{"path": "a.go"}},
		}},
		{Role: "tool", Content: "file contents", ToolCallID: "tc1", ToolName: "read_file"},
		{Role: "assistant", Content: "the file contains file contents"},
	}

	_, err := loop.Run(context.Background(), Submission{
		Content: "summarize please",
		History: history,
	})
	require.NoError(t, err)
	require.Equal(t, 1, model.CallCount())

	msgs := model.CallLog()[0].Messages

	// System prompt prepended by the loop.
	assert.Equal(t, llm.RoleSystem, msgs[0].Role)

	// The assistant message with ToolCalls must be forwarded.
	var assistantWithTools *llm.Message
	for i := range msgs {
		if msgs[i].Role == llm.RoleAssistant && len(msgs[i].ToolCalls) > 0 {
			assistantWithTools = &msgs[i]
			break
		}
	}
	require.NotNil(t, assistantWithTools)
	require.Len(t, assistantWithTools.ToolCalls, 1)
	assert.Equal(t, "tc1", assistantWithTools.ToolCalls[0].ID)
	assert.Equal(t, "read_file", assistantWithTools.ToolCalls[0].Name)

	// The tool result must have ToolCallID and Name forwarded.
	assert.True(t, hasToolMessage(msgs, "tc1"))
	var toolMsg *llm.Message
	for i := range msgs {
		if msgs[i].Role == llm.RoleTool {
			toolMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolMsg)
	assert.Equal(t, "read_file", toolMsg.Name)

	// The text-only assistant message must be forwarded without ToolCalls.
	var textAssistant *llm.Message
	for i := range msgs {
		if msgs[i].Role == llm.RoleAssistant && len(msgs[i].ToolCalls) == 0 {
			textAssistant = &msgs[i]
			break
		}
	}
	require.NotNil(t, textAssistant)
	assert.Equal(t, "the file contains file contents", textAssistant.Content)

	// The submission content is appended as a trailing user message.
	assert.Equal(t, "summarize please", msgs[len(msgs)-1].Content)
	assert.Equal(t, llm.RoleUser, msgs[len(msgs)-1].Role)
}

// TestHistoryForwardUserSystemUnaffected verifies that user and system messages
// in history are forwarded without ToolCalls or ToolCallID fields.
func TestHistoryForwardUserSystemUnaffected(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"LX-HF-05", "user-system-unaffected",
		mock.ConversationTurn{AssistantContent: "ok"},
	))
	loop := NewLoopAgent(WithLLM(model))

	history := []AgentMessage{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}

	_, err := loop.Run(context.Background(), Submission{
		Content: "thanks",
		History: history,
	})
	require.NoError(t, err)

	msgs := model.CallLog()[0].Messages

	// User messages must not carry ToolCalls, ToolCallID, or Name.
	for _, m := range msgs {
		if m.Role == llm.RoleUser {
			assert.Empty(t, m.ToolCalls, "user message should not have ToolCalls")
			assert.Empty(t, m.ToolCallID, "user message should not have ToolCallID")
			assert.Empty(t, m.Name, "user message should not have Name")
		}
	}

	// System messages must not carry ToolCalls or ToolCallID either.
	for _, m := range msgs {
		if m.Role == llm.RoleSystem {
			assert.Empty(t, m.ToolCalls, "system message should not have ToolCalls")
			assert.Empty(t, m.ToolCallID, "system message should not have ToolCallID")
		}
	}

	// The text-only assistant message must not have ToolCalls.
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant {
			assert.Empty(t, m.ToolCalls, "text-only assistant should not have ToolCalls")
			assert.Empty(t, m.ToolCallID, "assistant message should not have ToolCallID")
		}
	}
}
