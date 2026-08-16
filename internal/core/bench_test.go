package core

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// BenchmarkEventStreamPushPop measures Send + drain throughput across
// different buffer capacities. Each iteration creates a stream, fills it
// to capacity, closes it, and drains all events.
func BenchmarkEventStreamPushPop(b *testing.B) {
	for _, bufSize := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("buf_%d", bufSize), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stream := NewEventStream(bufSize, WithEventDiscardPolicy(DiscardOldest))
				for j := 0; j < bufSize; j++ {
					_ = stream.Send(AgentEvent{
						Kind:    "message",
						Content: "bench-event",
					})
				}
				stream.Close()
				for range stream.Events() {
				}
			}
		})
	}
}

// BenchmarkLoopRun measures a single ReAct loop iteration with a mock LLM.
// The mock model returns one tool call followed by a final text response,
// exercising the full streaming → tool-execution → final-response path.
func BenchmarkLoopRun(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		toolSrv := mock.NewMockToolServer()
		_, _ = toolSrv.RegisterReadFileTool("file contents")

		model := mock.NewMockLLMServer(mock.NewConversationTemplate(
			"B-LR", "bench-loop-run",
			mock.ConversationTurn{
				AssistantContent: "let me read the file",
				AssistantToolCalls: []mock.ExpectedToolCall{
					{ID: "call1", Name: "read_file", Args: map[string]any{"path": "a.go"}},
				},
			},
			mock.ConversationTurn{AssistantContent: "done"},
		))
		loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

		_, _ = loop.Run(context.Background(), Submission{Content: "read a.go"})
	}
}

// BenchmarkAgentRun measures AgentImpl overhead (history copy, event
// recording, state transitions) on top of the loop. The mock setup mirrors
// BenchmarkLoopRun so the delta between the two isolates AgentImpl cost.
func BenchmarkAgentRun(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		toolSrv := mock.NewMockToolServer()
		_, _ = toolSrv.RegisterReadFileTool("file contents")

		model := mock.NewMockLLMServer(mock.NewConversationTemplate(
			"B-AR", "bench-agent-run",
			mock.ConversationTurn{
				AssistantContent: "let me read the file",
				AssistantToolCalls: []mock.ExpectedToolCall{
					{ID: "call1", Name: "read_file", Args: map[string]any{"path": "a.go"}},
				},
			},
			mock.ConversationTurn{AssistantContent: "done"},
		))
		loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))
		agent := NewAgentImpl("bench", loop)

		_, _ = agent.Run(context.Background(), Submission{Content: "read a.go"})
	}
}

// BenchmarkBuildToolDefinitions measures the tool-definition cache: the
// cache_miss sub-benchmark creates a fresh LoopAgent each iteration (forcing
// a rebuild), while cache_hit reuses a primed agent (returning the cached
// slice).
func BenchmarkBuildToolDefinitions(b *testing.B) {
	ctx := context.Background()

	b.Run("cache_miss", func(b *testing.B) {
		b.ReportAllocs()
		reg := tools.NewDefaultToolRegistry()
		_ = reg.Register(ctx, &nameDescTool{name: "tool_a", description: "does a"})
		_ = reg.Register(ctx, &nameDescTool{name: "tool_b", description: "does b"})

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			loop := NewLoopAgent(WithTools(reg))
			_, _ = loop.buildToolDefinitions(ctx)
		}
	})

	b.Run("cache_hit", func(b *testing.B) {
		b.ReportAllocs()
		reg := tools.NewDefaultToolRegistry()
		_ = reg.Register(ctx, &nameDescTool{name: "tool_a", description: "does a"})
		_ = reg.Register(ctx, &nameDescTool{name: "tool_b", description: "does b"})
		loop := NewLoopAgent(WithTools(reg))
		_, _ = loop.buildToolDefinitions(ctx) // prime the cache

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = loop.buildToolDefinitions(ctx)
		}
	})
}

// BenchmarkEventStreamConcurrentSend measures concurrent send throughput.
// Multiple producer goroutines push events into a DiscardOldest stream while
// a single consumer drains them. This stresses the lock contention and
// channel operations under parallel access.
func BenchmarkEventStreamConcurrentSend(b *testing.B) {
	const numProducers = 4
	const eventsPerProducer = 50

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		stream := NewEventStream(100, WithEventDiscardPolicy(DiscardOldest))

		var producerWG sync.WaitGroup
		producerWG.Add(numProducers)
		for p := 0; p < numProducers; p++ {
			go func() {
				defer producerWG.Done()
				for j := 0; j < eventsPerProducer; j++ {
					_ = stream.Send(AgentEvent{
						Kind:    "message",
						Content: "concurrent-bench-event",
					})
				}
			}()
		}

		done := make(chan struct{})
		go func() {
			for range stream.Events() {
			}
			close(done)
		}()

		producerWG.Wait()
		stream.Close()
		<-done
	}
}
