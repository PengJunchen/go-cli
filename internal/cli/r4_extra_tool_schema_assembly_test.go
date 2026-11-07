package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestAssembleAgentExtraToolSchemasReachLoopModel(t *testing.T) {
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, newAssembleTestConfig(t), "openai", "schema-fixture", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NotNil(t, assembly.ToolRegistry)
	require.NotNil(t, assembly.LoopAgent)

	wantNames := []string{
		"todo_write", "task_create", "task_update", "goal_create", "goal_update", "git_pr_create",
		"task_get", "task_list", "goal_list", "goal_get", "ask_user", "tool_search",
		"web_fetch",
	}
	wantByName := make(map[string]bool, len(wantNames))
	for _, name := range wantNames {
		wantByName[name] = true
	}

	definitions, err := assembly.ToolRegistry.List(ctx)
	require.NoError(t, err)
	_, err = assembly.ToolRegistry.Get(ctx, "web_search")
	require.ErrorIs(t, err, tools.ErrToolNotFound, "an unconfigured provider must not register an unusable tool")
	registrySchemas := make(map[string]any, len(wantNames))
	for _, definition := range definitions {
		if !wantByName[definition.Name()] {
			continue
		}
		parameterized, ok := definition.(tools.Parameterized)
		require.Truef(t, ok, "%s must retain Parameterized through the real assembly registry", definition.Name())
		registrySchemas[definition.Name()] = parameterized.Parameters()
	}
	require.Len(t, registrySchemas, len(wantNames), "the assembly registry must expose all fixed extra tools")
	for _, name := range wantNames {
		assert.Contains(t, registrySchemas, name)
	}
	searchProxy, err := assembly.ToolRegistry.Get(ctx, "tool_search")
	require.NoError(t, err)
	topTools, ok := searchProxy.(tools.TopToolsProvider)
	require.True(t, ok, "tool_search must retain ranked-subset capability through protected assembly proxies")
	ranked, err := topTools.TopTools(ctx, "read_file", 3)
	require.NoError(t, err)
	require.NotEmpty(t, ranked)

	model := &r4SchemaRecordingModel{}
	assembly.LoopAgent.SetModel(model)
	assembly.LoopAgent.WithToolSearchThreshold(1000)
	_, err = assembly.LoopAgent.Run(ctx, core.Submission{Content: "show the available tool schemas"})
	require.NoError(t, err)

	modelTools := model.capturedTools()
	for _, definition := range modelTools {
		assert.NotEqual(t, "web_search", definition.Name, "an unconfigured provider must not be offered to the model")
	}
	modelSchemas := make(map[string]any, len(modelTools))
	for _, definition := range modelTools {
		if wantByName[definition.Name] {
			modelSchemas[definition.Name] = definition.Parameters
		}
	}
	require.Len(t, modelSchemas, len(wantNames), "Loop must forward all fixed extras to the model when filtering is disabled")
	for _, name := range wantNames {
		assert.Equalf(t, registrySchemas[name], modelSchemas[name], "%s Parameters copied from its protected registry proxy", name)
	}

	// Send those exact Loop-produced definitions through the real
	// OpenAI-compatible HTTP serializer and inspect the request received by a
	// local endpoint.
	requestBodies := make(chan []byte, 1)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadRequest)
			return
		}
		requestBodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(providerServer.Close)
	providerModel, providerCleanup, err := llm.NewEinoProvider(llm.WithBaseURL(providerServer.URL)).Build(ctx, llm.ModelConfig{Model: "schema-fixture"})
	require.NoError(t, err)
	t.Cleanup(providerCleanup)
	_, err = providerModel.Generate(ctx, []llm.Message{{Role: llm.RoleUser, Content: "inspect schemas"}}, llm.WithTools(modelTools))
	require.NoError(t, err)
	body := <-requestBodies
	var openAIRequest struct {
		Tools []struct {
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(body, &openAIRequest))
	providerSchemas := make(map[string]any, len(wantNames))
	for _, toolDefinition := range openAIRequest.Tools {
		if wantByName[toolDefinition.Function.Name] {
			providerSchemas[toolDefinition.Function.Name] = toolDefinition.Function.Parameters
		}
	}
	require.Len(t, providerSchemas, len(wantNames), "OpenAI-compatible request must serialize all fixed extra schemas")
	for _, name := range wantNames {
		wantJSON, marshalErr := json.Marshal(modelSchemas[name])
		require.NoError(t, marshalErr)
		gotJSON, marshalErr := json.Marshal(providerSchemas[name])
		require.NoError(t, marshalErr)
		assert.JSONEqf(t, string(wantJSON), string(gotJSON), "%s schema survives provider request serialization", name)
	}
}

type r4SchemaRecordingModel struct {
	mu    sync.Mutex
	tools []llm.ToolDefinition
}

type r4CompactVisibilityCall struct {
	messages []llm.Message
	tools    []llm.ToolDefinition
}

type r4CompactVisibilityRecordingModel struct {
	mu    sync.Mutex
	calls []r4CompactVisibilityCall
}

func (m *r4CompactVisibilityRecordingModel) Generate(_ context.Context, messages []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	m.capture(messages, opts)
	return &llm.Message{Role: llm.RoleAssistant, Content: "schemas captured", FinishReason: "stop"}, nil
}

func (m *r4CompactVisibilityRecordingModel) Stream(_ context.Context, messages []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	m.capture(messages, opts)
	chunks := make(chan llm.MessageChunk, 1)
	chunks <- llm.MessageChunk{Role: llm.RoleAssistant, Content: "schemas captured", Final: true, FinishReason: "stop"}
	close(chunks)
	return chunks, nil
}

func (m *r4CompactVisibilityRecordingModel) capture(messages []llm.Message, opts []llm.Option) {
	options := &llm.GenerationOptions{}
	for _, option := range opts {
		option(options)
	}
	call := r4CompactVisibilityCall{
		messages: append([]llm.Message(nil), messages...),
		tools:    append([]llm.ToolDefinition(nil), options.Tools...),
	}
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
}

func (m *r4CompactVisibilityRecordingModel) snapshot() []r4CompactVisibilityCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]r4CompactVisibilityCall(nil), m.calls...)
}

func (m *r4SchemaRecordingModel) Generate(_ context.Context, _ []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	m.capture(opts)
	return &llm.Message{Role: llm.RoleAssistant, Content: "schemas captured", FinishReason: "stop"}, nil
}

func (m *r4SchemaRecordingModel) Stream(_ context.Context, _ []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	m.capture(opts)
	chunks := make(chan llm.MessageChunk, 1)
	chunks <- llm.MessageChunk{Role: llm.RoleAssistant, Content: "schemas captured", Final: true, FinishReason: "stop"}
	close(chunks)
	return chunks, nil
}

func (m *r4SchemaRecordingModel) capture(opts []llm.Option) {
	options := &llm.GenerationOptions{}
	for _, option := range opts {
		option(options)
	}
	m.mu.Lock()
	m.tools = append([]llm.ToolDefinition(nil), options.Tools...)
	m.mu.Unlock()
}

func (m *r4SchemaRecordingModel) capturedTools() []llm.ToolDefinition {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]llm.ToolDefinition(nil), m.tools...)
}

func TestRuntimeDynamicCompactsWireSchemasAndToolGuidelines(t *testing.T) {
	ctx := context.Background()
	run := func(dynamic bool) (r4CompactVisibilityCall, bool) {
		cfg := newAssembleTestConfig(t)
		cfg.Runtime.Dynamic = dynamic
		assembly, err := AssembleAgent(ctx, cfg, "openai", "compact-schema-fixture", io.Discard, WithApproveMode(ApproveAuto))
		require.NoError(t, err)
		t.Cleanup(assembly.Cleanup)
		allDefinitions, err := assembly.ToolRegistry.List(ctx)
		require.NoError(t, err)
		require.Greater(t, len(allDefinitions), compactToolVisibilityLimitForTest)
		registered := make(map[string]bool, len(allDefinitions))
		for _, definition := range allDefinitions {
			registered[definition.Name()] = true
		}
		model := &r4CompactVisibilityRecordingModel{}
		assembly.LoopAgent.SetModel(model)
		_, err = assembly.LoopAgent.Run(ctx, core.Submission{Content: "perform generic work"})
		require.NoError(t, err)
		calls := model.snapshot()
		require.Len(t, calls, 1)
		return calls[0], registered["process_session"]
	}
	legacy, _ := run(false)
	compact, processSessionRegistered := run(true)
	compactRepeat, _ := run(true)
	legacySchemaBytes := r4ToolSchemaBytes(t, legacy.tools)
	compactSchemaBytes := r4ToolSchemaBytes(t, compact.tools)
	legacyPromptBytes := len(legacy.messages[0].Content)
	compactPromptBytes := len(compact.messages[0].Content)
	t.Logf("dynamic=false schemas=%d schema_bytes=%d system_prompt_bytes=%d", len(legacy.tools), legacySchemaBytes, legacyPromptBytes)
	t.Logf("dynamic=true schemas=%d schema_bytes=%d system_prompt_bytes=%d", len(compact.tools), compactSchemaBytes, compactPromptBytes)
	require.Len(t, legacy.tools, 30, "Dynamic=false keeps the existing default threshold behavior")
	require.Len(t, compact.tools, compactToolVisibilityLimitForTest)
	require.Less(t, compactSchemaBytes, legacySchemaBytes)
	require.Less(t, compactPromptBytes, legacyPromptBytes, "tool names and guidelines must follow the visible schema set")
	assert.Equal(t, compact.tools, compactRepeat.tools, "the initial schema set is deterministic across identical runs")
	assert.Equal(t, compact.messages[0].Content, compactRepeat.messages[0].Content, "the initial system prompt is a stable cacheable prefix")

	visible := make(map[string]bool, len(compact.tools))
	for _, definition := range compact.tools {
		visible[definition.Name] = true
	}
	for _, required := range []string{"read", "write", "edit", "bash", "grep", "find", "ls", "tool_search", "dispatch_subagent"} {
		require.Truef(t, visible[required], "%s must remain visible", required)
	}
	if processSessionRegistered {
		require.True(t, visible["process_session"], "process_session must remain visible when registered")
	}
}

const compactToolVisibilityLimitForTest = 12

func r4ToolSchemaBytes(t *testing.T, definitions []llm.ToolDefinition) int {
	t.Helper()
	encoded, err := json.Marshal(definitions)
	require.NoError(t, err)
	return len(encoded)
}
