package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// nameDescTool is a minimal ToolDefinition with configurable name and
// description, used to populate tool registries in dynamic filtering tests.
type nameDescTool struct {
	name        string
	description string
}

func (t *nameDescTool) Name() string        { return t.name }
func (t *nameDescTool) Description() string { return t.description }
func (t *nameDescTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "ok"}, nil
}

var _ tools.ToolDefinition = (*nameDescTool)(nil)

// topToolsProxy models a non-concrete ToolSearch wrapper that preserves the
// ranked-search capability without exposing the concrete ToolSearchTool type.
type topToolsProxy struct {
	tools.ToolDefinition
	provider tools.TopToolsProvider
}

var _ tools.TopToolsProvider = (*topToolsProxy)(nil)

func (p *topToolsProxy) TopTools(ctx context.Context, query string, k int) ([]tools.ToolDefinition, error) {
	return p.provider.TopTools(ctx, query, k)
}

type topToolsProxyRegistry struct {
	tools.ToolRegistry
}

func (r *topToolsProxyRegistry) List(ctx context.Context) ([]tools.ToolDefinition, error) {
	defs, err := r.ToolRegistry.List(ctx)
	if err != nil {
		return nil, err
	}
	for i, def := range defs {
		provider, ok := def.(tools.TopToolsProvider)
		if ok {
			defs[i] = &topToolsProxy{ToolDefinition: def, provider: provider}
		}
	}
	return defs, nil
}

// toolsFromOpts extracts the tool definitions from LLM generation options.
func toolsFromOpts(opts []llm.Option) []llm.ToolDefinition {
	var genOpts llm.GenerationOptions
	for _, opt := range opts {
		opt(&genOpts)
	}
	return genOpts.Tools
}

func TestDynamicToolInjection(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()

	// Register 20 tools with varied names/descriptions.
	for i := 0; i < 20; i++ {
		require.NoError(t, reg.Register(context.Background(), &nameDescTool{
			name:        fmt.Sprintf("tool_%d", i),
			description: fmt.Sprintf("performs operation %d on data", i),
		}))
	}

	// Register a ToolSearchTool so the loop can use it for filtering.
	searchTool := tools.NewToolSearchTool(reg)
	require.NoError(t, reg.Register(context.Background(), searchTool))

	// Mock LLM that returns a single turn (no tool calls).
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-DS", "dynamic-search",
		mock.ConversationTurn{AssistantContent: "done"},
	))

	loop := NewLoopAgent(WithLLM(model), WithTools(reg))
	loop.WithToolSearchThreshold(5) // 21 tools > 5, filtering kicks in.

	_, err := loop.Run(context.Background(), Submission{Content: "use tool_3"})
	require.NoError(t, err)

	callLog := model.CallLog()
	require.NotEmpty(t, callLog)

	passedTools := toolsFromOpts(callLog[0].Options)
	assert.LessOrEqual(t, len(passedTools), 5, "should pass at most threshold tools")

	// The most relevant tool (tool_3) should be in the filtered set.
	var names []string
	for _, td := range passedTools {
		names = append(names, td.Name)
	}
	assert.Contains(t, names, "tool_3")
}

func TestDynamicToolFilteringUsesTopToolsProviderProxy(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()
	for i := 0; i < 20; i++ {
		require.NoError(t, reg.Register(context.Background(), &nameDescTool{
			name:        fmt.Sprintf("tool_%d", i),
			description: fmt.Sprintf("performs operation %d on data", i),
		}))
	}
	searchTool := tools.NewToolSearchTool(reg)
	require.NoError(t, reg.Register(context.Background(), searchTool))

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-DSP", "dynamic-search-proxy",
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(&topToolsProxyRegistry{ToolRegistry: reg}))
	loop.WithToolSearchThreshold(5)

	_, err := loop.Run(context.Background(), Submission{Content: "use tool_3"})
	require.NoError(t, err)

	callLog := model.CallLog()
	require.NotEmpty(t, callLog)
	passedTools := toolsFromOpts(callLog[0].Options)
	assert.LessOrEqual(t, len(passedTools), 5, "proxy capability should preserve dynamic filtering")

	var names []string
	for _, td := range passedTools {
		names = append(names, td.Name)
	}
	assert.Contains(t, names, "tool_3")
}

func TestBelowThresholdNoSearch(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()

	// Register just 3 tools (below threshold).
	for i := 0; i < 3; i++ {
		require.NoError(t, reg.Register(context.Background(), &nameDescTool{
			name:        fmt.Sprintf("tool_%d", i),
			description: fmt.Sprintf("performs operation %d", i),
		}))
	}

	searchTool := tools.NewToolSearchTool(reg)
	require.NoError(t, reg.Register(context.Background(), searchTool))

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-BT", "below-threshold",
		mock.ConversationTurn{AssistantContent: "done"},
	))

	loop := NewLoopAgent(WithLLM(model), WithTools(reg))
	loop.WithToolSearchThreshold(10) // 4 tools < 10, no filtering.

	_, err := loop.Run(context.Background(), Submission{Content: "use tool_1"})
	require.NoError(t, err)

	callLog := model.CallLog()
	require.NotEmpty(t, callLog)

	passedTools := toolsFromOpts(callLog[0].Options)
	// All 4 tools should be passed (no filtering).
	assert.Len(t, passedTools, 4)
}

func TestCompactToolVisibilityPinsEssentialsPromotionsAndRankedFill(t *testing.T) {
	essentialNames := []string{"read", "write", "edit", "bash", "grep", "find", "ls", "tool_search", "dispatch_subagent", "process_session"}
	catalog := make([]tools.ToolDefinition, 0, 16)
	byName := make(map[string]tools.ToolDefinition)
	for _, name := range essentialNames {
		definition := &nameDescTool{name: name, description: "essential " + name}
		catalog = append(catalog, definition)
		byName[name] = definition
	}
	for _, name := range []string{"promoted_old", "promoted_new", "ranked_a", "ranked_b", "ranked_c"} {
		definition := &nameDescTool{name: name, description: "available " + name}
		catalog = append(catalog, definition)
		byName[name] = definition
	}

	ranked := []tools.ToolDefinition{byName["ranked_a"], byName["ranked_b"], byName["ranked_c"]}
	selected := compactToolDefinitions(ranked, catalog, []string{"promoted_old", "promoted_new"})
	require.Len(t, selected, compactToolVisibilityLimit)
	assert.Equal(t, essentialNames, toolDefinitionNames(selected[:len(essentialNames)]), "essential order is stable")
	assert.Equal(t, []string{"promoted_new", "promoted_old"}, toolDefinitionNames(selected[len(essentialNames):]), "newest successful search promotions take the remaining slots")
	for _, definition := range selected {
		assert.Same(t, byName[definition.Name()], definition, "selection must reuse the current authoritative definition")
	}

	delete(byName, "process_session")
	catalog = catalog[:0]
	for _, name := range essentialNames[:len(essentialNames)-1] {
		catalog = append(catalog, byName[name])
	}
	for _, name := range []string{"promoted_new", "ranked_a", "ranked_b", "ranked_c"} {
		catalog = append(catalog, byName[name])
	}
	selected = compactToolDefinitions(ranked, catalog, []string{"promoted_new"})
	require.Len(t, selected, compactToolVisibilityLimit)
	assert.Equal(t, essentialNames[:len(essentialNames)-1], toolDefinitionNames(selected[:len(essentialNames)-1]))
	assert.Equal(t, []string{"promoted_new", "ranked_a", "ranked_b"}, toolDefinitionNames(selected[len(essentialNames)-1:]))
}

func TestCompactToolPromotionStateInvalidatesAndStaysBoundedAcrossLongRuns(t *testing.T) {
	catalog := []tools.ToolDefinition{
		&nameDescTool{name: "read"}, &nameDescTool{name: "write"}, &nameDescTool{name: "edit"},
		&nameDescTool{name: "bash"}, &nameDescTool{name: "grep"}, &nameDescTool{name: "find"},
		&nameDescTool{name: "ls"}, &nameDescTool{name: "tool_search"}, &nameDescTool{name: "dispatch_subagent"},
	}
	for index := 0; index < 100; index++ {
		catalog = append(catalog, &nameDescTool{name: fmt.Sprintf("result_%03d", index)})
	}
	state := &toolSearchPromotionState{}
	state.beginRun("run-a", 7, 0, "inspect project")
	for index := 0; index < 100; index++ {
		name := fmt.Sprintf("result_%03d", index)
		state.promote([]string{name}, nil, maxCompactToolPromotions)
		selected := compactToolDefinitions(catalog[9:12], catalog, state.snapshot())
		assert.LessOrEqual(t, len(selected), compactToolVisibilityLimit)
		assert.LessOrEqual(t, len(state.snapshot()), maxCompactToolPromotions)
		assert.Contains(t, toolDefinitionNames(selected), name, "the newest completed search remains visible")
	}
	assert.Equal(t, []string{"result_097", "result_098", "result_099"}, state.snapshot(), "long runs evict the oldest promoted names")

	state.beginRun("run-a", 7, 1, "inspect project")
	assert.Empty(t, state.snapshot(), "new user input clears prior promotions even when the text is unchanged")
	state.promote([]string{"result_000"}, nil, maxCompactToolPromotions)
	state.beginRun("run-a", 8, 1, "inspect project")
	assert.Empty(t, state.snapshot(), "catalog generation change clears prior promotions")
	state.promote([]string{"result_001"}, nil, maxCompactToolPromotions)
	state.beginRun("run-b", 8, 1, "inspect project")
	assert.Empty(t, state.snapshot(), "a new run cannot inherit promoted schemas")
}

func TestCompactToolPromotionNoticeNamesOnlySchemasThatFitAfterTenEssentials(t *testing.T) {
	state := &toolSearchPromotionState{}
	state.beginRun("run-a", 1, 0, "inspect")
	visible := []tools.ToolDefinition{
		&nameDescTool{name: "read"}, &nameDescTool{name: "write"}, &nameDescTool{name: "edit"},
		&nameDescTool{name: "bash"}, &nameDescTool{name: "grep"}, &nameDescTool{name: "find"},
		&nameDescTool{name: "ls"}, &nameDescTool{name: "tool_search"}, &nameDescTool{name: "dispatch_subagent"},
		&nameDescTool{name: "process_session"},
	}
	notice := state.promote([]string{"result_001", "result_002", "result_003"}, visible, compactToolVisibilityLimit)
	assert.Contains(t, notice, "result_002, result_003")
	assert.NotContains(t, notice, "result_001")
	assert.Equal(t, []string{"result_002", "result_003"}, state.snapshot())

	catalog := append(append([]tools.ToolDefinition(nil), visible...),
		&nameDescTool{name: "result_001"}, &nameDescTool{name: "result_002"}, &nameDescTool{name: "result_003"})
	selected := compactToolDefinitions(nil, catalog, state.snapshot())
	assert.Len(t, selected, compactToolVisibilityLimit)
	assert.NotContains(t, toolDefinitionNames(selected), "result_001")
	assert.Contains(t, toolDefinitionNames(selected), "result_002")
	assert.Contains(t, toolDefinitionNames(selected), "result_003")
}

func toolDefinitionNames(definitions []tools.ToolDefinition) []string {
	names := make([]string, len(definitions))
	for index, definition := range definitions {
		names[index] = definition.Name()
	}
	return names
}

type schemaNameDescTool struct {
	name        string
	description string
}

func (t *schemaNameDescTool) Name() string        { return t.name }
func (t *schemaNameDescTool) Description() string { return t.description }
func (t *schemaNameDescTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{"type": "string"},
		},
		"additionalProperties": false,
	}
}
func (t *schemaNameDescTool) PromptGuidelines() []string {
	return []string{"Use " + t.name + " safely."}
}
func (t *schemaNameDescTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "ok"}, nil
}

func newCompactToolRegistry(t *testing.T) *tools.MiddlewareToolRegistry {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	for _, name := range []string{"read", "write", "edit", "bash", "grep", "find", "ls", "dispatch_subagent"} {
		require.NoError(t, registry.Register(ctx, &schemaNameDescTool{name: name, description: "essential " + name}))
	}
	for index := 0; index < 24; index++ {
		name := fmt.Sprintf("a_extra_%02d", index)
		require.NoError(t, registry.Register(ctx, &schemaNameDescTool{name: name, description: "unrelated fixture capability"}))
	}
	require.NoError(t, registry.Register(ctx, &schemaNameDescTool{name: "zz_target", description: "needle_target exact match capability"}))
	require.NoError(t, registry.RegisterTrustedControl(ctx, tools.NewToolSearchTool(registry)))
	return registry
}

func TestCompactToolSearchPromotesExactTrustedResultAndItsGuideline(t *testing.T) {
	ctx := context.Background()
	registry := newCompactToolRegistry(t)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"compact-promote", "compact tool promotion",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "search-target", Name: "tool_search", Args: map[string]any{"query": "needle_target"}}}},
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "invoke-target", Name: "zz_target", Args: map[string]any{"value": "ok"}}}},
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(
		WithLLM(model), WithTools(registry), WithMaxIterations(20),
		WithSystemPromptBuilder(NewDefaultSystemPromptBuilder()),
	).WithToolSearchThreshold(compactToolVisibilityLimit).WithCompactToolVisibility(true)

	events, err := loop.Run(ctx, Submission{Content: "perform generic work"})
	require.NoError(t, err)
	var toolResults []string
	for _, event := range events {
		if event.Kind == "tool_result" {
			toolResults = append(toolResults, event.Content)
		}
	}
	require.NotEmpty(t, toolResults)
	assert.Contains(t, toolResults[0], "Tool schema(s) available in the next request: zz_target.")
	calls := model.CallLog()
	require.Len(t, calls, 3)
	first := toolsFromOpts(calls[0].Options)
	second := toolsFromOpts(calls[1].Options)
	assert.Len(t, first, compactToolVisibilityLimit)
	assert.NotContains(t, toolNamesForLLM(first), "zz_target", "unsearched schemas stay hidden")
	assert.Len(t, second, compactToolVisibilityLimit)
	assert.Contains(t, toolNamesForLLM(second), "zz_target", "a successful trusted search exposes its exact current schema")
	assert.NotContains(t, calls[0].Messages[0].Content, "Use zz_target safely.")
	assert.Contains(t, calls[1].Messages[0].Content, "Use zz_target safely.", "system-prompt guidelines track the visible schema set")
	target, err := registry.Get(ctx, "zz_target")
	require.NoError(t, err)
	targetParameters, ok := target.(tools.Parameterized)
	require.True(t, ok)
	for _, definition := range second {
		if definition.Name == "zz_target" {
			assert.Equal(t, target.Description(), definition.Description)
			assert.Equal(t, targetParameters.Parameters(), definition.Parameters)
		}
	}
}

func TestCompactToolSchemasAndPromptDoNotGrowAcrossHundredModelTurns(t *testing.T) {
	ctx := context.Background()
	registry := newCompactToolRegistry(t)
	require.NoError(t, registry.Register(ctx, &schemaNameDescTool{name: "cycle", description: "repeat a bounded cycle"}))
	turns := make([]mock.ConversationTurn, 0, 101)
	for index := 0; index < 100; index++ {
		turns = append(turns, mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: fmt.Sprintf("cycle-%03d", index), Name: "cycle", Args: map[string]any{"value": "ok"},
		}}})
	}
	turns = append(turns, mock.ConversationTurn{AssistantContent: "done"})
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("compact-100", "compact hundred turns", turns...))
	loop := NewLoopAgent(
		WithLLM(model), WithTools(registry), WithMaxIterations(120),
		WithSystemPromptBuilder(NewDefaultSystemPromptBuilder()),
	).WithToolSearchThreshold(compactToolVisibilityLimit).WithCompactToolVisibility(true)

	_, err := loop.Run(ctx, Submission{Content: "perform generic work"})
	require.NoError(t, err)
	calls := model.CallLog()
	require.Len(t, calls, 101)
	baselineNames := toolNamesForLLM(toolsFromOpts(calls[0].Options))
	baselineSchemaBytes := llmToolSchemaBytes(t, toolsFromOpts(calls[0].Options))
	baselinePromptBytes := len(calls[0].Messages[0].Content)
	require.Len(t, baselineNames, compactToolVisibilityLimit)
	for index, call := range calls {
		definitions := toolsFromOpts(call.Options)
		assert.Len(t, definitions, compactToolVisibilityLimit, "model iteration %d", index)
		assert.Equal(t, baselineNames, toolNamesForLLM(definitions), "visible names stay fixed without new searches")
		assert.Equal(t, baselineSchemaBytes, llmToolSchemaBytes(t, definitions), "schema bytes stay bounded")
		assert.Equal(t, baselinePromptBytes, len(call.Messages[0].Content), "prompt bytes do not grow with completed cycles")
	}
}

func toolNamesForLLM(definitions []llm.ToolDefinition) []string {
	names := make([]string, len(definitions))
	for index, definition := range definitions {
		names[index] = definition.Name
	}
	return names
}

func llmToolSchemaBytes(t *testing.T, definitions []llm.ToolDefinition) int {
	t.Helper()
	encoded, err := json.Marshal(definitions)
	require.NoError(t, err)
	return len(encoded)
}
