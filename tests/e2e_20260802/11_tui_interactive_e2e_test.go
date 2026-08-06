// Package e2e_20260802 contains end-to-end integration tests for the TUI
// interactive mode. It exercises the full pipeline from MockLLMServer through
// LoopAgent → AgentImpl → HarnessImpl → BridgeEvents → BubbleteaApp,
// plus MCP tool execution, skill execution, automatic session compaction,
// and content-type mapping through the bridge.
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// waitForStream polls stream.Result() until it succeeds (i.e. SetResult has
// been called by the harness) or the context expires. This avoids the race
// between reading Result() and the harness goroutine still being in progress.
func waitForStream(ctx context.Context, stream core.EventStream) (core.AgentMessage, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		msg, err := stream.Result()
		if err == nil {
			return msg, nil
		}
		select {
		case <-ctx.Done():
			return msg, ctx.Err()
		case <-ticker.C:
			// keep polling
		}
	}
}

// =============================================================================
// 1. TestE2EInteractive_TUIBridgePipeline
// =============================================================================

// TestE2EInteractive_TUIBridgePipeline wires a MockLLMServer through
// LoopAgent → AgentImpl → HarnessImpl, bridges the EventStream to TUI events,
// and verifies the BubbleteaApp renders them correctly.
func TestE2EInteractive_TUIBridgePipeline(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create a MockLLMServer that returns a simple assistant message.
	tmpl := mock.NewConversationTemplate("T-01", "bridge-pipeline",
		mock.ConversationTurn{AssistantContent: "Hello from the pipeline!"},
	)
	llmServer := mock.NewMockLLMServer(tmpl)

	// Wire: LoopAgent → AgentImpl → HarnessImpl.
	// Use a buffered event stream so the harness goroutine does not block.
	tr := tools.NewDefaultToolRegistry()
	loop := core.NewLoopAgent(core.WithLLM(llmServer), core.WithTools(tr))
	agent := core.NewAgentImpl("interactive", loop)
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(16))

	// Submit a message and get the event stream.
	stream, err := h.Submit(ctx, "hello")
	require.NoError(t, err)
	require.NotNil(t, stream)

	// Bridge core events to TUI events.
	tuiEvents := tui.BridgeEvents(ctx, stream)

	// Create and run the BubbleteaApp.
	app := tui.NewBubbleteaApp(tuiEvents, tui.WithWidth(80))

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	// Wait for the stream to complete.
	result, streamErr := waitForStream(ctx, stream)
	app.Quit()

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("app did not shut down in time")
	}

	require.NoError(t, streamErr, "stream should complete without error")
	assert.Contains(t, result.Content, "Hello from the pipeline!")

	// Verify the app processed events.
	assert.GreaterOrEqual(t, app.EventsProcessed(), int64(1), "at least one event should be processed")

	// Verify the view contains the assistant message.
	view := app.View()
	assert.Contains(t, view, "Hello from the pipeline!", "rendered view should contain the assistant message")
}

// =============================================================================
// 2. TestE2EInteractive_MCPToolExecution
// =============================================================================

// TestE2EInteractive_MCPToolExecution creates a MockMCPServer, registers a tool,
// wraps it with MCPToolAdapter, registers into a ToolRegistry, then creates a
// MockLLMServer that returns a tool_call for the MCP tool, runs through the
// agent loop, and verifies the MCP tool was called.
func TestE2EInteractive_MCPToolExecution(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create MockMCPServer with a tool.
	mcpServer := mock.NewMockMCPServer("weather")
	mcpServer.RegisterTool("get_forecast", "Get weather forecast", func(args map[string]any) (any, error) {
		city := safeString(args["city"])
		return "Sunny in " + city, nil
	})

	// Wrap with MCPToolAdapter and register into ToolRegistry.
	adapter := mcp.NewMCPToolAdapter(mcpServer, mcp.MCPTool{
		Name:        "get_forecast",
		Description: "Get weather forecast",
	})

	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(ctx, adapter))

	// Verify the tool is registered under the MCP name.
	mcpToolName := mcp.NormalizeToolName("weather", "get_forecast")
	assert.Equal(t, mcpToolName, adapter.Name())

	// Create MockLLMServer that first returns a tool_call, then a final message.
	tmpl := mock.NewConversationTemplate("T-02", "mcp-tool-exec",
		mock.ConversationTurn{
			AssistantContent: "Let me check the weather.",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call-1", Name: mcpToolName, Args: map[string]any{"city": "Beijing"}},
			},
		},
		mock.ConversationTurn{AssistantContent: "The weather in Beijing is sunny."},
	)
	llmServer := mock.NewMockLLMServer(tmpl)

	// Wire the full agent pipeline.
	loop := core.NewLoopAgent(core.WithLLM(llmServer), core.WithTools(tr))
	agent := core.NewAgentImpl("interactive-mcp", loop)
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(16))

	// Submit and run.
	stream, err := h.Submit(ctx, "What's the weather in Beijing?")
	require.NoError(t, err)

	// Bridge to TUI.
	tuiEvents := tui.BridgeEvents(ctx, stream)
	app := tui.NewBubbleteaApp(tuiEvents, tui.WithWidth(80))

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	// Wait for stream completion.
	_, streamErr := waitForStream(ctx, stream)
	app.Quit()

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("app did not shut down in time")
	}

	require.NoError(t, streamErr)

	// Verify the MCP tool was called.
	logs := mcpServer.CallLog()
	require.Len(t, logs, 1, "MCP tool should have been called exactly once")
	assert.Equal(t, "get_forecast", logs[0].ToolName)
	assert.Equal(t, "Beijing", logs[0].Args["city"])
	assert.Equal(t, "Sunny in Beijing", logs[0].Result)

	// Verify the LLM was called twice (tool_call + final message).
	assert.Equal(t, 2, llmServer.CallCount(), "LLM should be called twice for tool_call + final response")

	// Verify the TUI rendered events.
	assert.GreaterOrEqual(t, app.EventsProcessed(), int64(2), "should process message + tool_call + tool_result + done events")
}

// =============================================================================
// 3. TestE2EInteractive_SkillExecution
// =============================================================================

// TestE2EInteractive_SkillExecution creates a SkillDefinition, wraps it with
// SkillAdapter, registers into a ToolRegistry, then creates a MockLLMServer
// that returns a tool_call for the skill, runs through the agent loop, and
// verifies the skill was executed.
func TestE2EInteractive_SkillExecution(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create a skill definition.
	skillDef := skill.NewSkill("code-review",
		skill.WithDescription("Automated code review skill"),
		skill.WithPrompt("You are a code reviewer. Check the diff for issues."),
		skill.WithTools("bash", "read", "grep"),
		skill.WithParameters(map[string]any{"max_lines": 500}),
	)

	// Wrap with SkillAdapter and register.
	skillAdapter := skill.NewSkillAdapter(skillDef)
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(ctx, skillAdapter))

	// Create MockLLMServer that calls the skill tool, then returns a final message.
	tmpl := mock.NewConversationTemplate("T-03", "skill-exec",
		mock.ConversationTurn{
			AssistantContent: "I'll review the code for you.",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call-sk-1", Name: "code-review", Args: map[string]any{}},
			},
		},
		mock.ConversationTurn{AssistantContent: "Code review complete. Found 3 issues."},
	)
	llmServer := mock.NewMockLLMServer(tmpl)

	// Wire the agent pipeline.
	loop := core.NewLoopAgent(core.WithLLM(llmServer), core.WithTools(tr))
	agent := core.NewAgentImpl("interactive-skill", loop)
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(16))

	// Submit and run.
	stream, err := h.Submit(ctx, "Review my code")
	require.NoError(t, err)

	tuiEvents := tui.BridgeEvents(ctx, stream)
	app := tui.NewBubbleteaApp(tuiEvents, tui.WithWidth(80))

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	// Wait for stream completion.
	_, streamErr := waitForStream(ctx, stream)
	app.Quit()

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("app did not shut down in time")
	}

	require.NoError(t, streamErr)

	// Verify the skill was executed by checking LLM call count:
	// First call returns tool_call, second call returns final message after tool result.
	assert.Equal(t, 2, llmServer.CallCount(), "LLM should be called twice")

	// Verify the LLM received the skill's output in the second call's messages.
	callLog := llmServer.CallLog()
	require.Len(t, callLog, 2)

	// The second call should contain the tool result message.
	secondMsgs := callLog[1].Messages
	hasToolMsg := false
	for _, m := range secondMsgs {
		if m.Role == llm.RoleTool {
			hasToolMsg = true
			assert.Contains(t, m.Content, "[skill code-review]", "tool result should contain skill marker")
			break
		}
	}
	assert.True(t, hasToolMsg, "second LLM call should receive a tool result message")

	// Verify the TUI rendered the events.
	assert.GreaterOrEqual(t, app.EventsProcessed(), int64(2), "should process multiple events")
}

// =============================================================================
// 4. TestE2EInteractive_AutoCompaction
// =============================================================================

// TestE2EInteractive_AutoCompaction creates a conversation with many TurnItems
// that exceed the token budget, calls CompactIfNeeded, and verifies that
// compaction was triggered and the result has fewer tokens.
func TestE2EInteractive_AutoCompaction(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	est := compaction.NewHeuristicTokenEstimator()

	// Build a conversation that exceeds the budget.
	maxTokens := 100
	var items []compaction.TurnItem

	// Add system prompt.
	items = append(items, compaction.TurnItem{
		ID:      "sys-0",
		Role:    compaction.RoleSystem,
		Content: "You are a helpful assistant.",
	})

	// Add many user/assistant turns with large content.
	for i := 0; i < 20; i++ {
		items = append(items, compaction.TurnItem{
			ID:      fmt.Sprintf("user-%d", i),
			Role:    compaction.RoleUser,
			Content: "Please read the file " + strings.Repeat("x", 80),
		})
		items = append(items, compaction.TurnItem{
			ID:      fmt.Sprintf("asst-%d", i),
			Role:    compaction.RoleAssistant,
			Content: "Here is the content of the file: " + strings.Repeat("y", 80),
		})
		items = append(items, compaction.TurnItem{
			ID:         fmt.Sprintf("tool-%d", i),
			Role:       compaction.RoleTool,
			ToolName:   "read",
			ToolResult: strings.Repeat("z", 120),
		})
	}

	// Verify the conversation exceeds the budget before compaction.
	beforeTokens := tokenSum(items, est)
	assert.Greater(t, beforeTokens, maxTokens, "conversation should exceed the budget before compaction")

	// Set up compaction infrastructure.
	compactor := compaction.NewUnifiedCompactor()
	midTurn := compaction.NewMidTurnCompact()

	// Trigger compaction.
	result, compactResult, err := midTurn.CompactIfNeeded(ctx, items, maxTokens, est, compactor)
	require.NoError(t, err)

	// Verify compaction was triggered.
	assert.True(t, compactResult.Triggered, "compaction should be triggered")
	assert.Equal(t, compaction.TriggerThreshold, compactResult.Reason)

	// Verify the result has fewer tokens.
	afterTokens := tokenSum(result, est)
	assert.LessOrEqual(t, afterTokens, maxTokens, "compacted items should fit within the budget")
	assert.Less(t, afterTokens, beforeTokens, "compacted items should have fewer tokens than before")
}

// =============================================================================
// 5. TestE2EInteractive_FullPipeline
// =============================================================================

// TestE2EInteractive_FullPipeline is an end-to-end test:
// MockLLM → LoopAgent → Harness → BridgeEvents → BubbleteaApp,
// verifying the complete event flow from agent to TUI rendering.
func TestE2EInteractive_FullPipeline(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Set up MCP tools.
	mcpServer := mock.NewMockMCPServer("db")
	mcpServer.RegisterTool("query", "Query database", func(args map[string]any) (any, error) {
		return "query result: " + safeString(args["sql"]), nil
	})
	mcpAdapter := mcp.NewMCPToolAdapter(mcpServer, mcp.MCPTool{
		Name:        "query",
		Description: "Query database",
	})

	// Set up skill tools.
	skillDef := skill.NewSkill("analyze",
		skill.WithDescription("Data analysis skill"),
		skill.WithPrompt("You are a data analyst."),
	)
	skillAdapter := skill.NewSkillAdapter(skillDef)

	// Register all tools.
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(ctx, mcpAdapter))
	require.NoError(t, tr.Register(ctx, skillAdapter))

	// Create a MockLLMServer with a multi-turn conversation:
	// 1st call: calls the MCP query tool
	// 2nd call: calls the skill
	// 3rd call: returns final answer
	dbToolName := mcp.NormalizeToolName("db", "query")
	tmpl := mock.NewConversationTemplate("T-05", "full-pipeline",
		mock.ConversationTurn{
			AssistantContent: "Let me query the database.",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call-db-1", Name: dbToolName, Args: map[string]any{"sql": "SELECT 1"}},
			},
		},
		mock.ConversationTurn{
			AssistantContent: "Now let me analyze the data.",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call-sk-1", Name: "analyze", Args: map[string]any{}},
			},
		},
		mock.ConversationTurn{AssistantContent: "Analysis complete. The result is 42."},
	)
	llmServer := mock.NewMockLLMServer(tmpl)

	// Wire the full pipeline.
	loop := core.NewLoopAgent(core.WithLLM(llmServer), core.WithTools(tr))
	agent := core.NewAgentImpl("interactive-full", loop)
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(16))

	// Submit.
	stream, err := h.Submit(ctx, "Analyze my database")
	require.NoError(t, err)

	// Bridge to TUI and run the app.
	tuiEvents := tui.BridgeEvents(ctx, stream)
	app := tui.NewBubbleteaApp(tuiEvents, tui.WithWidth(80))

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	// Wait for the stream to complete.
	result, streamErr := waitForStream(ctx, stream)
	app.Quit()

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("app did not shut down in time")
	}

	require.NoError(t, streamErr)
	assert.Equal(t, "Analysis complete. The result is 42.", result.Content)

	// Verify MCP tool was called.
	mcpLogs := mcpServer.CallLog()
	require.Len(t, mcpLogs, 1, "MCP query tool should be called once")
	assert.Equal(t, "query", mcpLogs[0].ToolName)
	assert.Equal(t, "SELECT 1", mcpLogs[0].Args["sql"])

	// Verify LLM was called 3 times.
	assert.Equal(t, 3, llmServer.CallCount())

	// Verify the TUI processed all events.
	// Expected events: message, tool_call, tool_result, message, tool_call, tool_result, message, done = 8+
	assert.GreaterOrEqual(t, app.EventsProcessed(), int64(5), "should process multiple events across the pipeline")

	// Verify the view contains key content.
	view := app.View()
	assert.Contains(t, view, "Analysis complete", "view should contain the final answer")
}

// =============================================================================
// 6. TestE2EInteractive_BridgeContentTypeMapping
// =============================================================================

// TestE2EInteractive_BridgeContentTypeMapping tests all core.AgentEvent.Kind
// values map to the correct tui.ContentType through the bridge.
func TestE2EInteractive_BridgeContentTypeMapping(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tests := []struct {
		name   string
		kind   string
		wantCT string
	}{
		{
			name:   "message maps to assistant",
			kind:   "message",
			wantCT: tui.ContentTypeAssistant,
		},
		{
			name:   "tool_call maps to tool_call",
			kind:   "tool_call",
			wantCT: tui.ContentTypeToolCall,
		},
		{
			name:   "tool_result maps to tool_result",
			kind:   "tool_result",
			wantCT: tui.ContentTypeToolResult,
		},
		{
			name:   "error maps to error",
			kind:   "error",
			wantCT: tui.ContentTypeError,
		},
		{
			name:   "done maps to status",
			kind:   "done",
			wantCT: tui.ContentTypeStatus,
		},
		{
			name:   "unknown maps to status fallback",
			kind:   "unknown_kind",
			wantCT: tui.ContentTypeStatus,
		},
		{
			name:   "empty kind maps to status fallback",
			kind:   "",
			wantCT: tui.ContentTypeStatus,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use KindToContentType directly for unit-level mapping check.
			gotCT := tui.KindToContentType(tt.kind)
			assert.Equal(t, tt.wantCT, gotCT, "KindToContentType(%q) = %q, want %q", tt.kind, gotCT, tt.wantCT)

			// Also verify via CoreEventToAgentEvent which is the actual bridge function.
			coreEv := core.AgentEvent{Kind: tt.kind, Content: "test-content"}
			tuiEv := tui.CoreEventToAgentEvent(coreEv)
			assert.Equal(t, tt.wantCT, tuiEv.ContentType, "CoreEventToAgentEvent ContentType mismatch")
			assert.Equal(t, tt.kind, tuiEv.Type, "Type should preserve the original Kind")
			assert.Equal(t, "test-content", tuiEv.Content, "Content should be preserved")
		})
	}

	// Integration: verify that a real EventStream → BridgeEvents → App pipeline
	// preserves content type mapping correctly.
	t.Run("end-to-end bridge mapping", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		// Create an EventStream manually and push events of each kind.
		stream := core.NewEventStream(16)

		kinds := []struct {
			kind   string
			wantCT string
		}{
			{"message", tui.ContentTypeAssistant},
			{"tool_call", tui.ContentTypeToolCall},
			{"tool_result", tui.ContentTypeToolResult},
			{"error", tui.ContentTypeError},
			{"done", tui.ContentTypeStatus},
		}

		for _, k := range kinds {
			_ = stream.Send(core.AgentEvent{Kind: k.kind, Content: "payload:" + k.kind}) //nolint:errcheck,gosec
		}
		stream.Close()

		// Bridge and collect TUI events.
		tuiCh := tui.BridgeEvents(ctx, stream)

		var received []tui.AgentEvent
		for ev := range tuiCh {
			received = append(received, ev)
		}

		require.Len(t, received, len(kinds), "should receive exactly one TUI event per core event")

		for i, ev := range received {
			assert.Equal(t, kinds[i].wantCT, ev.ContentType,
				"event %d: ContentType mismatch for kind=%q", i, kinds[i].kind)
			assert.Equal(t, kinds[i].kind, ev.Type,
				"event %d: Type should preserve the original Kind", i)
			assert.Equal(t, "payload:"+kinds[i].kind, ev.Content,
				"event %d: Content should be preserved", i)
		}
	})
}
