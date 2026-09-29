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
	assembly, err := AssembleAgent(ctx, newAssembleTestConfig(), "openai", "schema-fixture", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NotNil(t, assembly.ToolRegistry)
	require.NotNil(t, assembly.LoopAgent)

	wantNames := []string{
		"todo_write", "task_create", "task_update", "goal_create", "goal_update", "git_pr_create",
		"task_get", "task_list", "goal_list", "goal_get", "ask_user", "tool_search",
		"web_fetch", "web_search",
	}
	wantByName := make(map[string]bool, len(wantNames))
	for _, name := range wantNames {
		wantByName[name] = true
	}

	definitions, err := assembly.ToolRegistry.List(ctx)
	require.NoError(t, err)
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
