package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/capability"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type generationTestGuard struct{ lease *capability.Lease[string] }

func (g generationTestGuard) BeginCall(ctx context.Context, id string) (RunCapabilityInvocation, error) {
	return g.lease.BeginCall(ctx, id)
}

type generationActionTool struct {
	name   string
	action func()
}

func (t generationActionTool) Name() string        { return t.name }
func (t generationActionTool) Description() string { return "progressive discovery fixture" }
func (t generationActionTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	if t.action != nil {
		t.action()
	}
	return &tools.ToolResult{Output: t.name}, nil
}

type generationDiscoveryModel struct {
	t      *testing.T
	calls  int
	attack bool
}

func (m *generationDiscoveryModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return nil, errors.New("expected streaming")
}
func (m *generationDiscoveryModel) Stream(_ context.Context, _ []llm.Message, options ...llm.Option) (<-chan llm.MessageChunk, error) {
	settings := llm.GenerationOptions{}
	for _, option := range options {
		option(&settings)
	}
	declared := map[string]bool{}
	for _, definition := range settings.Tools {
		declared[definition.Name] = true
	}
	m.calls++
	chunk := llm.MessageChunk{Role: llm.RoleAssistant, Final: true}
	switch m.calls {
	case 1:
		require.True(m.t, declared["discover"])
		require.False(m.t, declared["activated"])
		chunk.ToolCalls = []llm.ToolCall{{ID: "discover-call", Name: "discover", Args: map[string]any{}}}
		if m.attack {
			chunk.ToolCalls = append(chunk.ToolCalls, llm.ToolCall{ID: "premature", Name: "activated", Args: map[string]any{}})
		}
	case 2:
		require.True(m.t, declared["activated"], "next request failed to refresh the exposure catalog")
		chunk.ToolCalls = []llm.ToolCall{{ID: "activated-call", Name: "activated", Args: map[string]any{}}}
	default:
		chunk.Content = "completed"
	}
	output := make(chan llm.MessageChunk, 1)
	output <- chunk
	close(output)
	return output, nil
}

func TestProgressiveDiscoveryRefreshesNextModelRequest(t *testing.T) {
	for _, attack := range []bool{false, true} {
		t.Run(map[bool]string{false: "next-request", true: "reject-same-response"}[attack], func(t *testing.T) {
			registry := tools.NewDefaultToolRegistry()
			discoveries := 0
			executions := 0
			require.NoError(t, registry.Register(context.Background(), generationActionTool{name: "discover", action: func() {
				discoveries++
				require.NoError(t, registry.Register(context.Background(), generationActionTool{name: "activated", action: func() { executions++ }}))
			}}))
			model := &generationDiscoveryModel{t: t, attack: attack}
			loop := NewLoopAgent(WithLLM(model), WithTools(registry), WithMaxIterations(4))
			ctx := WithRunToolRegistry(WithRunGenerationMetadata(context.Background(), RunGenerationMetadata{GenerationID: "pinned"}), registry)
			_, err := loop.RunCanonical(ctx, Submission{Type: SubmissionUserMessage, Content: "discover and call"})
			if attack {
				require.ErrorContains(t, err, "not disclosed")
				require.Zero(t, discoveries)
				require.Zero(t, executions)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, discoveries)
				require.Equal(t, 1, executions)
				require.Equal(t, 3, model.calls)
			}
		})
	}
}
func (g generationTestGuard) Check() error {
	call, err := g.lease.BeginCall(context.Background(), "completion")
	if err != nil {
		return err
	}
	defer call.Release()
	return call.Complete()
}

type generationTestTool struct{ revoke func() }

func (t generationTestTool) Name() string        { return "generation_test" }
func (t generationTestTool) Description() string { return "test guarded callback" }
func (t generationTestTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	t.revoke()
	return &tools.ToolResult{Output: "late private result"}, nil
}

func TestCallResultFencedAfterRevoke(t *testing.T) {
	m := capability.NewManager[string]()
	snapshot, err := capability.NewSnapshot(capability.Input{Prompt: "guard fixture"})
	require.NoError(t, err)
	require.NoError(t, m.Reload(context.Background(), func(context.Context, *capability.Scope) (capability.Snapshot, string, error) {
		return snapshot, "runtime", nil
	}))
	lease, err := m.Acquire(context.Background())
	require.NoError(t, err)
	registry := tools.NewDefaultToolRegistry()
	require.NoError(t, registry.Register(context.Background(), generationTestTool{revoke: func() { m.Revoke() }}))
	ctx := WithRunCapabilityGuard(context.Background(), generationTestGuard{lease: lease})
	output, isError, err := executeSingleTool(ctx, registry, tools.ToolCall{ID: "call", Name: "generation_test"}, nil)
	require.Empty(t, output)
	require.True(t, isError)
	require.ErrorIs(t, err, capability.ErrRevoked)
	var unknown interface{ OutcomeUnknown() bool }
	require.True(t, errors.As(err, &unknown))
	require.True(t, unknown.OutcomeUnknown())
	lease.Release()
	require.NoError(t, m.Close(context.Background()))
}

type generationCountingLease struct{ releases atomic.Int32 }

func (l *generationCountingLease) Release() { l.releases.Add(1) }

func TestRunPinsGenerationUntilWorkerQuiesces(t *testing.T) {
	lease := &generationCountingLease{}
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "generation-owner"}, nil)
	owner.generationLease = lease
	entered := make(chan func(), 1)
	result := make(chan error, 1)
	go func() {
		_, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
			entered <- registerRunWorker(ctx, "child", "tool")
			return Result{Message: "callback completed"}, nil
		})
		result <- err
	}()
	workerDone := <-entered
	require.Zero(t, lease.releases.Load())
	workerDone()
	require.NoError(t, <-result)
	require.Equal(t, int32(1), lease.releases.Load())
}

func TestPinnedRegistrySchemaCacheUsesGeneration(t *testing.T) {
	first := tools.NewDefaultToolRegistry()
	second := tools.NewDefaultToolRegistry()
	require.NoError(t, first.Register(context.Background(), generationTestTool{}))
	require.NoError(t, second.Register(context.Background(), &blockingTool{name: "second"}))
	loop := NewLoopAgent(WithTools(first))
	ctx := WithRunToolRegistry(WithRunGenerationMetadata(context.Background(), RunGenerationMetadata{GenerationID: "first"}), first)
	definitions, err := loop.buildToolDefinitions(ctx)
	require.NoError(t, err)
	require.Equal(t, "generation_test", definitions[0].Name)
	ctx = WithRunToolRegistry(WithRunGenerationMetadata(context.Background(), RunGenerationMetadata{GenerationID: "second"}), second)
	definitions, err = loop.buildToolDefinitions(ctx)
	require.NoError(t, err)
	require.Equal(t, "second", definitions[0].Name)
}

type generationLateCanonicalLoop struct{ revoke func() }

func (l generationLateCanonicalLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return nil, nil
}
func (l generationLateCanonicalLoop) RunCanonical(_ context.Context, submission Submission, _ ...EventStream) (CanonicalRunResult, error) {
	l.revoke()
	return CanonicalRunResult{Events: []AgentEvent{{Kind: "message", Content: "late private answer"}}, Transcript: []TranscriptEntry{{Message: AgentMessage{Role: "user", Content: submission.Content}}, {Message: AgentMessage{Role: "assistant", Content: "late private answer"}}}}, nil
}

func TestRevokedCanonicalResultCannotEnterAgentHistory(t *testing.T) {
	m := capability.NewManager[string]()
	snapshot, err := capability.NewSnapshot(capability.Input{Prompt: "history fence"})
	require.NoError(t, err)
	require.NoError(t, m.Reload(context.Background(), func(context.Context, *capability.Scope) (capability.Snapshot, string, error) {
		return snapshot, "runtime", nil
	}))
	lease, err := m.Acquire(context.Background())
	require.NoError(t, err)
	agent := NewAgentImpl("history-fence", generationLateCanonicalLoop{revoke: func() { m.Revoke() }})
	result, err := agent.Run(WithRunCapabilityGuard(context.Background(), generationTestGuard{lease: lease}), Submission{Type: SubmissionUserMessage, Content: "input"})
	require.ErrorIs(t, err, capability.ErrRevoked)
	require.Empty(t, result.Message)
	require.Empty(t, agent.Messages())
	require.Empty(t, agent.Events())
	lease.Release()
	require.NoError(t, m.Close(context.Background()))
}
