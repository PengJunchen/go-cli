package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type heldSubAgentFixture struct {
	sub     *core.DefaultSubAgent
	ctx     context.Context
	cancel  context.CancelFunc
	started <-chan struct{}
	release func()
}

func newHeldSubAgentFixture(t *testing.T, config core.SubAgentConfig) *heldSubAgentFixture {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRunner := func() { releaseOnce.Do(func() { close(release) }) }

	blockTool := &recordingTool{
		name: "block",
		execute: func(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			close(started)
			select {
			case <-release:
				return &tools.ToolResult{Output: "ok"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	registry := tools.NewDefaultToolRegistry()
	require.NoError(t, registry.Register(ctx, blockTool))
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"You are a test sub-agent.", "subagent-hold",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "hold", Name: "block", Args: map[string]any{},
		}}},
		mock.ConversationTurn{AssistantContent: "done"},
	))
	factory := core.NewRealSubAgentRunnerFactory(model, nil, registry)
	sub, ok := core.NewDefaultSubAgent(config, core.WithSubAgentRunner(factory)).(*core.DefaultSubAgent)
	require.True(t, ok, "default sub-agent should use its concrete implementation")

	fixture := &heldSubAgentFixture{
		sub:     sub,
		ctx:     ctx,
		cancel:  cancel,
		started: started,
		release: releaseRunner,
	}
	t.Cleanup(func() {
		fixture.release()
		if fixture.sub.State() != core.SubAgentIdle {
			waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
			_, _ = fixture.sub.Wait(waitCtx)
			waitCancel()
			if fixture.sub.State() == core.SubAgentRunning {
				fixture.cancel()
				waitCtx, waitCancel = context.WithTimeout(context.Background(), time.Second)
				_, _ = fixture.sub.Wait(waitCtx)
				waitCancel()
				if fixture.sub.State() == core.SubAgentRunning {
					t.Errorf("sub-agent runner did not stop after release and context cancellation")
				}
			}
		}
		fixture.cancel()
	})
	return fixture
}

func waitForSubAgentRunner(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sub-agent runner did not reach its held tool")
	}
}
