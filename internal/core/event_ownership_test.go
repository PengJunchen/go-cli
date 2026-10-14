package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func ownershipEvent() AgentEvent {
	return AgentEvent{Kind: "message", Usage: &llm.Usage{InputTokens: 7}, ToolCalls: []llm.ToolCall{{ID: "call", Name: "read", Args: map[string]any{"paths": []any{map[string]any{"path": "safe"}}}}}}
}

func changeEventArgs(event AgentEvent) {
	event.ToolCalls[0].Args.(map[string]any)["paths"].([]any)[0].(map[string]any)["path"] = "changed"
	event.ToolCalls[0].Name = "changed"
	event.Usage.InputTokens = 99
}

func assertOriginalEvent(t *testing.T, event AgentEvent) {
	t.Helper()
	require.Equal(t, "read", event.ToolCalls[0].Name)
	require.Equal(t, "safe", event.ToolCalls[0].Args.(map[string]any)["paths"].([]any)[0].(map[string]any)["path"])
	require.EqualValues(t, 7, event.Usage.InputTokens)
}

func TestEventBusToolArgumentsOwnership(t *testing.T) {
	bus := NewMemoryEventBus()
	defer bus.Close()
	first, second := bus.Subscribe(context.Background()), bus.Subscribe(context.Background())
	source := ownershipEvent()
	bus.Publish(source)
	changeEventArgs(<-first)
	assertOriginalEvent(t, source)
	assertOriginalEvent(t, <-second)
}

// The bus is entered after the stream consumer owns its event. Mutating the
// consumer's copy before Publish examines its argument makes the handoff
// ordering regression deterministic, without sleeps or scheduler assumptions.
type ownershipBarrierBus struct {
	received <-chan AgentEvent
	observed AgentEvent
}

func (b *ownershipBarrierBus) Publish(event AgentEvent) {
	changeEventArgs(<-b.received)
	b.observed = event
}
func (*ownershipBarrierBus) Subscribe(context.Context) <-chan AgentEvent { return nil }
func (*ownershipBarrierBus) Close()                                      {}

func TestEventStreamToolArgumentsOwnership(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "send", true: "terminal"}[terminal], func(t *testing.T) {
			bus := &ownershipBarrierBus{}
			stream := NewEventStream(1, WithEventBus(bus))
			defer stream.Close()
			bus.received = stream.Events()
			source := ownershipEvent()
			if terminal {
				require.NoError(t, stream.SendTerminal(source))
			} else {
				require.NoError(t, stream.Send(source))
			}
			assertOriginalEvent(t, source)
			assertOriginalEvent(t, bus.observed)
		})
	}
}

func TestPreToolCallObserverOwnership(t *testing.T) {
	args := map[string]any{"nested": map[string]any{"path": "safe"}}
	var seen string
	loop := NewLoopAgent()
	loop.SetToolInterceptor(func(_, _ string, received map[string]any) error {
		seen = received["nested"].(map[string]any)["path"].(string)
		received["nested"].(map[string]any)["path"] = "interceptor mutation"
		return nil
	})
	source := loop.newPreToolCallEvent(context.Background(), "read", "call", args)
	event := AgentEvent{PreToolCall: source}
	observer := cloneAgentEvent(event).PreToolCall
	observer.Args["nested"].(map[string]any)["path"] = "observer mutation"
	observer.ToolName = "changed"
	require.False(t, source.IsCancelled())
	require.Equal(t, "safe", seen)
	require.Equal(t, "safe", args["nested"].(map[string]any)["path"])
	observer.Cancel()
	require.True(t, source.IsCancelled(), "observer cancellation must remain connected")
}

func TestToolDispatchArgumentsOwnership(t *testing.T) {
	source := ownershipEvent()
	call := toToolsCall(source.ToolCalls[0])
	call.Args["paths"].([]any)[0].(map[string]any)["path"] = "tool mutation"
	assertOriginalEvent(t, source)
}

func TestEventFanoutConcurrentOwnership(t *testing.T) {
	bus := NewMemoryEventBus()
	defer bus.Close()
	stream := NewEventStream(1, WithEventBus(bus))
	defer stream.Close()
	channels := []<-chan AgentEvent{stream.Events(), bus.Subscribe(context.Background()), bus.Subscribe(context.Background())}
	var workers sync.WaitGroup
	for _, ch := range channels {
		workers.Add(1)
		go func(events <-chan AgentEvent) {
			defer workers.Done()
			event := <-events
			for i := 0; i < 100; i++ {
				changeEventArgs(event)
			}
		}(ch)
	}
	source := ownershipEvent()
	require.NoError(t, stream.Send(source))
	for i := 0; i < 100; i++ {
		changeEventArgs(source)
	}
	workers.Wait()
}

type modifyingOwnershipStream struct{ *EventStreamImpl }

func (s *modifyingOwnershipStream) Send(event AgentEvent) error {
	for _, call := range event.ToolCalls {
		call.Args.(map[string]any)["path"] = "observer mutation"
	}
	if event.PreToolCall != nil {
		event.PreToolCall.Args["path"] = "observer mutation"
		event.PreToolCall.ToolName = "changed"
	}
	return nil
}

func TestLoopToolArgumentsOwnership(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionModeSequential, ExecutionModeParallel} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			received := make(chan string, 2)
			registry := mock.NewMockToolServer()
			require.NoError(t, registry.Register(context.Background(), &testToolDef{name: "read", handler: func(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
				received <- call.Args["path"].(string)
				call.Args["path"] = "tool mutation"
				return &tools.ToolResult{Output: "ok"}, nil
			}}))
			model := &scriptedChatModel{seq: []*llm.Message{
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "one", Name: "read", Args: map[string]any{"path": "safe"}}, {ID: "two", Name: "read", Args: map[string]any{"path": "safe"}}}},
				{Role: llm.RoleAssistant, Content: "done"},
			}}
			loop := NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(mode), WithToolInterceptor(func(_, _ string, args map[string]any) error {
				if args["path"] != "safe" {
					return errors.New("observer changed approval arguments")
				}
				args["path"] = "interceptor mutation"
				return nil
			}))
			stream := &modifyingOwnershipStream{NewEventStream(1)}
			defer stream.Close()
			result, err := loop.RunCanonical(context.Background(), Submission{Content: "run"}, stream)
			require.NoError(t, err)
			require.Len(t, received, 2)
			require.Equal(t, "safe", <-received)
			require.Equal(t, "safe", <-received)
			for _, entry := range result.Transcript {
				for _, call := range entry.Message.ToolCalls {
					require.Equal(t, "safe", call.Args.(map[string]any)["path"])
				}
			}
			for _, event := range result.Events {
				for _, call := range event.ToolCalls {
					require.Equal(t, "safe", call.Args.(map[string]any)["path"])
				}
			}
		})
	}
}

func TestPreToolCallObserverDenialOwnership(t *testing.T) {
	denied := errors.New("approval denied")
	var calls atomic.Int32
	loop := NewLoopAgent(WithToolInterceptor(func(name, id string, args map[string]any) error {
		calls.Add(1)
		if name != "read" || id != "call" || args["path"] != "safe" {
			return errors.New("approval arguments changed")
		}
		return denied
	}))
	source := loop.newPreToolCallEvent(context.Background(), "read", "call", map[string]any{"path": "safe"})
	bus := NewMemoryEventBus()
	defer bus.Close()
	first, second := bus.Subscribe(context.Background()), bus.Subscribe(context.Background())
	bus.Publish(AgentEvent{PreToolCall: source})
	observer, other := (<-first).PreToolCall, (<-second).PreToolCall
	observer.ToolName, observer.ToolCallID = "changed", "changed"
	observer.Args["path"] = "changed"
	require.Equal(t, "safe", other.Args["path"])
	require.True(t, observer.IsCancelled())
	require.ErrorIs(t, observer.CancellationError(), denied)
	require.True(t, other.IsCancelled())
	require.True(t, source.IsCancelled())
	require.EqualValues(t, 1, calls.Load())
}

func TestToolInterceptorsOwnIndependentArguments(t *testing.T) {
	var observed []string
	loop := NewLoopAgent(
		WithToolInterceptor(func(_, _ string, args map[string]any) error {
			args["nested"].(map[string]any)["path"] = "first interceptor"
			return nil
		}),
		WithToolInterceptor(func(_, _ string, args map[string]any) error {
			observed = append(observed, args["nested"].(map[string]any)["path"].(string))
			args["nested"].(map[string]any)["path"] = "second interceptor"
			return nil
		}),
	)
	loop.SetContextToolInterceptor(func(_ context.Context, _, _ string, args map[string]any) error {
		observed = append(observed, args["nested"].(map[string]any)["path"].(string))
		return nil
	})
	event := loop.newPreToolCallEvent(context.Background(), "read", "call", map[string]any{"nested": map[string]any{"path": "safe"}})
	require.False(t, event.IsCancelled())
	require.Equal(t, []string{"safe", "safe"}, observed)
}

func TestPreToolCallConcurrentObserverOwnership(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	loop := NewLoopAgent(WithToolInterceptor(func(_, _ string, args map[string]any) error {
		close(started)
		<-release
		if args["path"] != "safe" {
			return errors.New("observer changed approval arguments")
		}
		return nil
	}))
	source := loop.newPreToolCallEvent(context.Background(), "read", "call", map[string]any{"path": "safe"})
	observer := cloneAgentEvent(AgentEvent{PreToolCall: source}).PreToolCall
	checked := make(chan bool, 1)
	go func() { checked <- source.IsCancelled() }()
	<-started
	observer.Args["path"] = "changed"
	observer.ToolName = "changed"
	close(release)
	require.False(t, <-checked)
	require.Equal(t, "safe", source.Args["path"])
}

func TestEventToolArgumentsCyclicOwnership(t *testing.T) {
	t.Run("map", func(t *testing.T) {
		source := map[string]any{"value": "safe"}
		source["self"] = source
		event := cloneAgentEvent(AgentEvent{ToolCalls: []llm.ToolCall{{Args: source}}})
		cloned := event.ToolCalls[0].Args.(map[string]any)
		cloned["self"].(map[string]any)["value"] = "changed"
		require.Equal(t, "changed", cloned["value"])
		require.Equal(t, "safe", source["value"])
		_, err := json.Marshal(cloned)
		require.Error(t, err, "copying must preserve invalid protocol cycles for rejection")
		gateArgs := cloneStringAnyMap(source)
		gateArgs["self"].(map[string]any)["value"] = "gate"
		require.Equal(t, "gate", gateArgs["value"])
		require.Equal(t, "safe", source["value"])
	})
	t.Run("slice", func(t *testing.T) {
		source := make([]any, 2)
		source[0], source[1] = source, "safe"
		cloned := cloneProtocolJSONValue(source).([]any)
		cloned[0].([]any)[1] = "changed"
		require.Equal(t, "changed", cloned[1])
		require.Equal(t, "safe", source[1])
		_, err := json.Marshal(cloned)
		require.Error(t, err)
	})
	t.Run("slice views", func(t *testing.T) {
		backing := []any{"safe", "second"}
		source := []any{backing[:1], backing[:2], backing[:1:1], []any{}, []any(nil)}
		cloned := cloneProtocolJSONValue(source).([]any)
		require.Len(t, cloned[0], 1)
		require.Len(t, cloned[1], 2)
		require.Len(t, cloned[2], 1)
		require.NotNil(t, cloned[3])
		require.Nil(t, cloned[4])
		cloned[1].([]any)[0] = "changed"
		require.Equal(t, "safe", backing[0])
	})
}
