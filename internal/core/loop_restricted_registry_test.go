package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type restrictedRegistryTestModel struct {
	*scriptedChatModel
	toolSchemas [][]llm.ToolDefinition
	candidates  []llm.ResolvedCandidateHandle
}

func (m *restrictedRegistryTestModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return m.candidates
}

func (m *restrictedRegistryTestModel) capture(opts []llm.Option) {
	var options llm.GenerationOptions
	for _, option := range opts {
		option(&options)
	}
	m.toolSchemas = append(m.toolSchemas, options.Tools)
}

func (m *restrictedRegistryTestModel) Generate(ctx context.Context, messages []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	m.capture(opts)
	return m.scriptedChatModel.Generate(ctx, messages, opts...)
}

func (m *restrictedRegistryTestModel) Stream(ctx context.Context, messages []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	m.capture(opts)
	return m.scriptedChatModel.Stream(ctx, messages, opts...)
}

// The model deliberately ignores its read-only schema. Both the schema and
// actual dispatch must honor the child restriction, including pinned runtimes
// without generation metadata and parallel mode's single-call fallback.
func TestLoopRestrictedRegistryBlocksSequentialEffects(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			for _, mode := range []ExecutionMode{ExecutionModeSequential, ExecutionModeParallel} {
				t.Run(fmt.Sprintf("pinned=%t/stream=%t/mode=%d", pinned, streaming, mode), func(t *testing.T) {
					model, registry, readPath, writePath, bashPath := newRestrictedRegistryTest(t)
					ctx := t.Context()
					fallback := registry
					if pinned {
						ctx = WithRunToolRegistry(ctx, registry)
						fallback = tools.NewDefaultToolRegistry()
					}
					loop := NewLoopAgent(WithLLM(model), WithTools(fallback), WithAllowedToolNames([]string{"read"}), WithExecutionMode(mode))
					var streams []EventStream
					if streaming {
						streams = append(streams, &collectingEventStream{})
					}
					result, err := loop.RunCanonical(ctx, Submission{Content: "read " + readPath}, streams...)
					require.NoError(t, err)
					assertRestrictedRegistryResult(t, model, result.Transcript, writePath, bashPath)
				})
			}
		}
	}
}

func TestRealSubAgentRunnerPinnedRegistryCannotWidenAllowedTools(t *testing.T) {
	model, registry, readPath, writePath, bashPath := newRestrictedRegistryTest(t)
	runner := &realSubAgentRunner{model: model, tools: registry, allowedTools: []string{"read"}, maxIter: 5}
	var events []AgentEvent
	_, err := runner.Run(WithRunToolRegistry(t.Context(), registry), "read "+readPath, nil, func(event AgentEvent) { events = append(events, event) })
	require.NoError(t, err)
	assertRestrictedRegistryResult(t, model, nil, writePath, bashPath)
	require.Contains(t, findEvents(events, "tool_result"), "Error: "+tools.ErrToolNotFound.Error())
}

func TestLoopRestrictedResolverWithoutGenerationID(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionModeSequential, ExecutionModeParallel} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			model, registry, readPath, writePath, bashPath := newRestrictedRegistryTest(t)
			model.candidates = []llm.ResolvedCandidateHandle{requestEnvelopeCandidate(t, llm.ModelInfo{Name: "restricted-tool-fixture", ContextWindow: 128000})}
			ctx := WithRunToolRegistry(t.Context(), registry)
			metadata, _ := RunGenerationMetadataFromContext(ctx)
			require.Empty(t, metadata.GenerationID)
			loop := NewLoopAgent(WithLLM(model), WithTools(tools.NewDefaultToolRegistry()), WithAllowedToolNames([]string{"read"}), WithExecutionMode(mode))
			loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
			result, err := loop.RunCanonical(ctx, Submission{Content: "read " + readPath}, &collectingEventStream{})
			require.NoError(t, err)
			assertRestrictedRegistryResult(t, model, result.Transcript, writePath, bashPath)
		})
	}
}

func TestLoopRestrictedToolSearchCannotDiscloseExcludedSchemas(t *testing.T) {
	registry := tools.NewDefaultToolRegistry()
	require.NoError(t, tools.RegisterDefaults(t.Context(), registry, tools.WithRegisteredBuiltinWhitelist([]string{"read", "write"})))
	require.NoError(t, registry.Register(t.Context(), tools.NewToolSearchTool(registry)))
	model := &restrictedRegistryTestModel{scriptedChatModel: &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, Content: "done"}}}}
	loop := NewLoopAgent(WithLLM(model), WithTools(registry), WithAllowedToolNames([]string{"read", "tool_search"})).WithToolSearchThreshold(1)
	_, err := loop.Run(t.Context(), Submission{Content: "write"})
	require.NoError(t, err)
	require.NotEmpty(t, model.toolSchemas)
	for _, schema := range model.toolSchemas {
		require.NotEmpty(t, schema)
		for _, definition := range schema {
			require.Contains(t, []string{"read", "tool_search"}, definition.Name)
		}
	}
}

func newRestrictedRegistryTest(t *testing.T) (*restrictedRegistryTestModel, tools.ToolRegistry, string, string, string) {
	t.Helper()
	root := t.TempDir()
	readPath := filepath.Join(root, "allowed.txt")
	writePath := filepath.Join(root, "forbidden-write.txt")
	bashPath := filepath.Join(root, "forbidden-bash.txt")
	require.NoError(t, os.WriteFile(readPath, []byte("allowed read content"), 0o600))
	registry := tools.NewDefaultToolRegistry()
	require.NoError(t, tools.RegisterDefaults(t.Context(), registry,
		tools.WithRegisteredBuiltinWhitelist([]string{"read", "write", "bash"}),
		tools.WithRegisteredBashSandbox(tools.AllowAllSandbox{}),
	))
	model := &restrictedRegistryTestModel{scriptedChatModel: &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "read", Name: "read", Args: map[string]any{"path": readPath}}}},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "write", Name: "write", Args: map[string]any{"path": writePath, "content": "forbidden"}}}},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "bash", Name: "bash", Args: map[string]any{"command": fmt.Sprintf("printf forbidden > %q", bashPath)}}}},
		{Role: llm.RoleAssistant, Content: "done"},
	}}}
	return model, registry, readPath, writePath, bashPath
}

func assertRestrictedRegistryResult(t *testing.T, model *restrictedRegistryTestModel, transcript []TranscriptEntry, writePath, bashPath string) {
	t.Helper()
	require.NotEmpty(t, model.toolSchemas)
	for _, schema := range model.toolSchemas {
		require.Len(t, schema, 1)
		require.Equal(t, "read", schema[0].Name)
	}
	for _, path := range []string{writePath, bashPath} {
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist, "disallowed executor must not create %s", path)
	}
	if transcript != nil {
		var readReceipt, writeReceipt, bashReceipt string
		for _, entry := range transcript {
			switch entry.Message.ToolCallID {
			case "read":
				readReceipt = entry.Message.Content
			case "write":
				writeReceipt = entry.Message.Content
			case "bash":
				bashReceipt = entry.Message.Content
			}
		}
		require.Contains(t, readReceipt, "allowed read content")
		require.Equal(t, "Error: "+tools.ErrToolNotFound.Error(), writeReceipt)
		require.Equal(t, "Error: "+tools.ErrToolNotFound.Error(), bashReceipt)
	}
}
