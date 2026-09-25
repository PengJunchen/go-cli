package cli

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type assemblyIsolationProbe struct {
	output string
	calls  atomic.Int32
}

func (*assemblyIsolationProbe) Name() string        { return "assembly_probe" }
func (*assemblyIsolationProbe) Description() string { return "assembly isolation probe" }
func (p *assemblyIsolationProbe) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	p.calls.Add(1)
	return &tools.ToolResult{Output: p.output}, nil
}

func newSubagentIsolationState(t *testing.T, model *mock.MockLLMServer, output string) (*assembleState, *assemblyIsolationProbe) {
	t.Helper()
	ctx := context.Background()
	registry := tools.NewDefaultToolRegistry()
	probe := &assemblyIsolationProbe{output: output}
	require.NoError(t, registry.Register(ctx, probe))

	return &assembleState{
		ctx:        ctx,
		logger:     slog.Default(),
		model:      model,
		modelChain: llm.NewModelMiddlewareChain(),
		pw:         production.NewProductionModelWrapper(),
		tr:         registry,
	}, probe
}

func TestAssembleSubAgentDispatchersStayBoundToTheirRegistry(t *testing.T) {
	previousFactory := core.GetSubAgentFactory()
	t.Cleanup(func() { core.RegisterSubAgentFactory(previousFactory) })
	sentinelFactory := core.NewSubAgentFactory()
	core.RegisterSubAgentFactory(sentinelFactory)

	newModel := func(name string) *mock.MockLLMServer {
		return mock.NewMockLLMServer(mock.NewConversationTemplate(name, name,
			mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
				ID:   "probe-call",
				Name: "assembly_probe",
				Args: map[string]any{},
			}}},
			mock.ConversationTurn{AssistantContent: name + " complete"},
		))
	}

	modelA := newModel("assembly-a")
	modelB := newModel("assembly-b")
	stateA, probeA := newSubagentIsolationState(t, modelA, "from assembly A registry")
	stateB, probeB := newSubagentIsolationState(t, modelB, "from assembly B registry")
	stateA.assembleOutputGuards()
	stateB.assembleOutputGuards()

	stateA.assembleSubAgent()
	stateB.assembleSubAgent()
	assert.Same(t, sentinelFactory, core.GetSubAgentFactory(), "an Assembly must not mutate the process-wide subagent factory")

	ctx := context.Background()
	resultA, err := stateA.dispatcher.Dispatch(ctx, core.SubagentTask{
		ID: "assembly-a-child", Prompt: "invoke the assembly probe", Tools: []string{"assembly_probe"}, MaxTurns: 3,
	})
	require.NoError(t, err)
	assert.Equal(t, "assembly-a complete", resultA.Content)
	assert.EqualValues(t, 1, probeA.calls.Load())
	assert.Zero(t, probeB.calls.Load(), "Assembly A's child must not execute Assembly B's tool")
	assert.Equal(t, 2, modelA.CallCount())
	assert.Zero(t, modelB.CallCount(), "Assembly A's dispatcher must use Assembly A's model")

	resultB, err := stateB.dispatcher.Dispatch(ctx, core.SubagentTask{
		ID: "assembly-b-child", Prompt: "invoke the assembly probe", Tools: []string{"assembly_probe"}, MaxTurns: 3,
	})
	require.NoError(t, err)
	assert.Equal(t, "assembly-b complete", resultB.Content)
	assert.EqualValues(t, 1, probeB.calls.Load())
	assert.EqualValues(t, 1, probeA.calls.Load())
	assert.Equal(t, 2, modelA.CallCount())
	assert.Equal(t, 2, modelB.CallCount())
}
