package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
)

func TestExtensionRegistryConcurrentRegisterGet(t *testing.T) {
	reg := NewExtensionRegistry()
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			//nolint:errcheck,gosec // Register* all return nil by design.
			reg.RegisterTool(ctx, testTool{name: "read"})
			//nolint:errcheck,gosec
			reg.RegisterCommand("cmd", func([]string) error { return nil })
			reg.Provider("default")
			reg.Tool("read")
			reg.Command("cmd")
		}()
	}
	wg.Wait()

	require.NotNil(t, reg.Tool("read"))
	require.NotNil(t, reg.Command("cmd"))
}

func TestExtensionRegistryConcurrentHooksAndMiddleware(t *testing.T) {
	reg := NewExtensionRegistry()
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			//nolint:errcheck,gosec
			reg.RegisterHook(ctx, &HookImpl{name: "h"})
			//nolint:errcheck,gosec
			reg.RegisterMiddleware(ctx, &MiddlewareImpl{name: "m"})
			reg.Hook("h")
			reg.Middleware("m")
		}()
	}
	wg.Wait()

	require.NotNil(t, reg.Hook("h"))
	require.NotNil(t, reg.Middleware("m"))
}

func TestHookChainConcurrentInvocation(t *testing.T) {
	var order []string
	h := &spyHook{name: "h", order: &order}
	chain := NewHookChain(h)

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := chain.Before(context.Background(), Submission{Content: "x"})
			assert.NoError(t, err)
			assert.True(t, res.Continue)
			require.NoError(t, chain.After(context.Background(), Submission{}, Result{}, nil))
		}()
	}
	wg.Wait()
	assert.NotEmpty(t, h.recorded())
}

func TestEinoTurnRunnerConcurrentRunAndGet(t *testing.T) {
	model := mock.NewMockLLMServer(nil)
	loop := NewLoopAgent(WithLLM(model))
	runner := NewEinoTurnRunner(loop)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := runner.RunTurn(context.Background(), Submission{Content: "go"})
			assert.NoError(t, err)
			assert.True(t, res.Success)
			// Concurrently reading every known turn must be safe.
			for _, tid := range runner.snapshotIDs() {
				_, gerr := runner.Get(context.Background(), tid)
				if gerr != nil {
					// Unknown/inactive turns are legitimate; no race expected.
					continue
				}
			}
		}()
	}
	wg.Wait()
}

func TestEinoTurnRunnerConcurrentCancelUnknown(t *testing.T) {
	runner := NewEinoTurnRunner(&stubLoop{})
	// Cancel/Steer/FollowUp on unknown ids fail fast and must be race-free.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			require.ErrorIs(t, runner.Cancel(context.Background(), id), errTurnUnknown)
			require.ErrorIs(t, runner.Steer(context.Background(), id, "x"), errTurnUnknown)
			require.ErrorIs(t, runner.FollowUp(context.Background(), id, "y"), errTurnUnknown)
		}("ghost-" + string(rune('a'+i%26)))
	}
	wg.Wait()
}

// snapshotIDs returns a copy of the currently registered turn ids. It is a
// test helper that peeks at internal state under the runner lock.
func (r *EinoTurnRunner) snapshotIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.turns))
	for id := range r.turns {
		ids = append(ids, id)
	}
	return ids
}

func TestMiddlewareChainExecuteRealLoop(t *testing.T) {
	// Compose logging middleware over a real single-turn loop and verify the
	// model event passes through unchanged.
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"MWEX-01", "real",
		mock.ConversationTurn{AssistantContent: "real reply"},
	))
	loop := NewLoopAgent(WithLLM(model))
	wrapped := NewMiddlewareChain(
		NewLoggingMiddleware("outer"),
		&MiddlewareImpl{name: "inner"},
	).Wrap(loop)

	events, err := wrapped.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	assert.Equal(t, 1, model.CallCount())
	assert.Equal(t, []string{"real reply"}, findEvents(events, "message"))
}

func TestAgentLoopChainPreservesIdempotency(t *testing.T) {
	// Wrapping a loop twice over the same base still yields exactly one
	// message from the underlying model (no double execution).
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"MWEX-02", "idem",
		mock.ConversationTurn{AssistantContent: "once"},
	))
	loop := NewLoopAgent(WithLLM(model))
	chain := NewMiddlewareChain(NewLoggingMiddleware("audit"))
	wrapped := chain.Wrap(chain.Wrap(loop))

	events, err := wrapped.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	assert.Equal(t, 1, model.CallCount())
	assert.Equal(t, []string{"once"}, findEvents(events, "message"))
}

func TestLastMessageEventTable(t *testing.T) {
	tests := []struct {
		name   string
		events []AgentEvent
		want   string
	}{
		{"nil", nil, ""},
		{"empty", []AgentEvent{}, ""},
		{"single message", []AgentEvent{{Kind: "message", Content: "a"}}, "a"},
		{"last non-empty wins", []AgentEvent{
			{Kind: "message", Content: "a"},
			{Kind: "message", Content: "b"},
		}, "b"},
		{"trailing empty keeps prior", []AgentEvent{
			{Kind: "message", Content: "a"},
			{Kind: "message", Content: ""},
		}, "a"},
		{"non-message ignored", []AgentEvent{{Kind: "tool", Content: "x"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lastMessageEvent(tt.events))
		})
	}
}

func TestHeuristicTokenEstimatorEstimateMessagesTable(t *testing.T) {
	est := HeuristicTokenEstimator{}
	tests := []struct {
		name string
		msgs []AgentMessage
		want int
	}{
		{"nil", nil, 0},
		{"empty", []AgentMessage{}, 0},
		{"single", []AgentMessage{{Content: "abcdefgh"}}, 2},
		{"multiple", []AgentMessage{{Content: "abcd"}, {Content: "abcdefgh"}}, 3},
		{"with empty content ignored", []AgentMessage{{Content: ""}, {Content: "abcdefgh"}}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, est.EstimateMessages(tt.msgs))
		})
	}
}

func TestEstimateMessagesMatchesPerMessageEstimate(t *testing.T) {
	// Two independent one-character messages each yield 0 (1/4 truncation);
	// the aggregate must equal the sum of per-message estimates.
	est := HeuristicTokenEstimator{}
	msgs := []AgentMessage{{Content: "a"}, {Content: "b"}}
	assert.Equal(t, est.Estimate("a")+est.Estimate("b"), est.EstimateMessages(msgs))
}

func TestAgentMessageStringTable(t *testing.T) {
	tests := []struct {
		name string
		msg  AgentMessage
		want string
	}{
		{"user", AgentMessage{Role: "user", Content: "hello"}, "user: hello"},
		{"assistant", AgentMessage{Role: "assistant", Content: "hi"}, "assistant: hi"},
		{"empty role", AgentMessage{Content: "x"}, ": x"},
		{"empty content", AgentMessage{Role: "user"}, "user: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.msg.String())
		})
	}
}

func TestAgentEventStringTable(t *testing.T) {
	tests := []struct {
		name string
		ev   AgentEvent
		sub  string
	}{
		{"message", AgentEvent{Kind: "message", Content: "c"}, "[message] c"},
		{"tool", AgentEvent{Kind: "tool_call", Content: "bash"}, "[tool_call] bash"},
		{"empty content", AgentEvent{Kind: "done"}, "[done] "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Contains(t, tt.ev.String(), tt.sub)
		})
	}
}
