package cli

import (
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type assemblyIsolationTool struct {
	calls atomic.Int32
}

func (*assemblyIsolationTool) Name() string        { return "assembly_b_mutation" }
func (*assemblyIsolationTool) Description() string { return "test mutation for assembly isolation" }
func (t *assemblyIsolationTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	t.calls.Add(1)
	return &tools.ToolResult{Output: "executed"}, nil
}

var _ tools.ToolDefinition = (*assemblyIsolationTool)(nil)

func TestAssemblyInterceptorsAreIsolated(t *testing.T) {
	core.ClearToolInterceptors()
	t.Cleanup(core.ClearToolInterceptors)

	newAssembly := func(name string) *AgentAssembly {
		t.Helper()
		cfg := newAssembleTestConfig()
		cfg.Git.WorkDir = t.TempDir()
		assembly, err := AssembleAgent(
			context.Background(),
			cfg,
			"openai",
			"test-model",
			io.Discard,
			WithAgentName(name),
			WithApproveMode(ApproveAuto),
		)
		require.NoError(t, err)
		t.Cleanup(assembly.Cleanup)
		return assembly
	}

	assemblyA := newAssembly("plan-owner")
	assemblyB := newAssembly("independent-agent")
	require.NoError(t, assemblyA.PlanCtrl.Enter(context.Background(), "assembly A is planning"))
	assert.False(t, assemblyB.PlanCtrl.IsActive(), "assembly B must retain its own inactive Plan Mode state")

	tool := &assemblyIsolationTool{}
	require.NoError(t, assemblyB.ToolRegistry.Register(context.Background(), tool))
	assemblyB.LoopAgent.SetModel(mock.NewMockLLMServer(mock.NewConversationTemplate(
		"R1", "assembly-interceptor-isolation",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID:   "assembly-b-call",
			Name: tool.Name(),
			Args: map[string]any{},
		}}},
		mock.ConversationTurn{AssistantContent: "done"},
	)))

	_, err := assemblyB.Agent.Run(context.Background(), core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "run assembly B's mutation",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, tool.calls.Load(),
		"an active interceptor in assembly A must not cancel assembly B's tool call")
}
