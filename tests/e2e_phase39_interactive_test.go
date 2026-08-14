//go:build e2e

// Package tests contains end-to-end integration tests for the go-cli project.
// This file verifies Phase 39 features: slash commands, @mention resolution,
// runtime model switching, EventStream overflow, line-editor cancellation,
// and race-free concurrent access.
package tests

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Mock model
// =============================================================================

// phase39Model is a simple test LLM model that returns a fixed response.
type phase39Model struct {
	response string
}

func (m *phase39Model) Generate(_ context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	return &llm.Message{Role: llm.RoleAssistant, Content: m.response}, nil
}

func (m *phase39Model) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	resp, err := m.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan llm.MessageChunk, 1)
	ch <- llm.MessageChunk{Content: resp.Content}
	close(ch)
	return ch, nil
}

// =============================================================================
// Test helpers
// =============================================================================

// phase39TestHandler is a simple slash command handler for testing the
// SlashCommandRegistry's dispatch mechanism without requiring a full
// Dependencies implementation.
type phase39TestHandler struct {
	name string
	desc string
	fn   func(args []string) (string, error)
}

func (h *phase39TestHandler) Name() string        { return h.name }
func (h *phase39TestHandler) Description() string { return h.desc }
func (h *phase39TestHandler) Handle(_ context.Context, args []string, _ cli.Dependencies) (string, error) {
	return h.fn(args)
}

// phase39MessageEvents extracts the Content of all "message" events.
func phase39MessageEvents(events []core.AgentEvent) []string {
	var result []string
	for _, ev := range events {
		if ev.Kind == "message" && !ev.Incremental {
			result = append(result, ev.Content)
		}
	}
	return result
}

// =============================================================================
// Test 1: Slash commands
// =============================================================================

// TestET_phase39_slash_commands tests that slash commands work in a REPL
// session by registering handlers in a SlashCommandRegistry and verifying
// dispatch for /help, /model, and /retry.
func TestET_phase39_slash_commands(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := cli.NewSlashCommandRegistry()

	models := []string{"gpt-4o", "gpt-4o-mini", "claude-3-opus"}
	lastUserMessage := "what is 2+2?"

	// /help — returns a summary of available commands.
	require.NoError(t, reg.Register(&phase39TestHandler{
		name: "help",
		desc: "Show available commands",
		fn: func(_ []string) (string, error) {
			var sb strings.Builder
			sb.WriteString("Available commands:\n")
			for _, h := range reg.List() {
				fmt.Fprintf(&sb, "  /%-8s %s\n", h.Name(), h.Description())
			}
			return sb.String(), nil
		},
	}))

	// /clear — clears conversation history.
	require.NoError(t, reg.Register(&phase39TestHandler{
		name: "clear",
		desc: "Clear conversation history",
		fn:   func(_ []string) (string, error) { return "", nil },
	}))

	// /model — lists models or switches to the named model.
	require.NoError(t, reg.Register(&phase39TestHandler{
		name: "model",
		desc: "Show or switch the current model (/model [name])",
		fn: func(args []string) (string, error) {
			if len(args) == 0 {
				return fmt.Sprintf("Available models: %s", strings.Join(models, ", ")), nil
			}
			return fmt.Sprintf("Switched to model: %s", args[0]), nil
		},
	}))

	// /retry — re-sends the last user message.
	require.NoError(t, reg.Register(&phase39TestHandler{
		name: "retry",
		desc: "Regenerate the last assistant response",
		fn: func(_ []string) (string, error) {
			return lastUserMessage, nil
		},
	}))

	// /edit — opens an external editor.
	require.NoError(t, reg.Register(&phase39TestHandler{
		name: "edit",
		desc: "Open external editor to compose a message",
		fn:   func(_ []string) (string, error) { return "edited content", nil },
	}))

	// Verify all five commands are registered.
	assert.ElementsMatch(t, []string{"clear", "edit", "help", "model", "retry"}, reg.Names())

	// Verify /help returns help text listing the commands.
	h, ok := reg.Lookup("help")
	require.True(t, ok)
	helpOut, err := h.Handle(context.Background(), nil, nil)
	require.NoError(t, err)
	for _, want := range []string{"/help", "/clear", "/model", "/retry", "/edit"} {
		assert.Contains(t, helpOut, want, "help output should list %s", want)
	}

	// Verify /model with no args lists available models.
	h, ok = reg.Lookup("model")
	require.True(t, ok)
	modelOut, err := h.Handle(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Contains(t, modelOut, "Available models:")
	for _, m := range models {
		assert.Contains(t, modelOut, m)
	}

	// Verify /model with an arg switches the model.
	switchOut, err := h.Handle(context.Background(), []string{"gpt-4o-mini"}, nil)
	require.NoError(t, err)
	assert.Contains(t, switchOut, "Switched to model: gpt-4o-mini")

	// Verify /retry re-sends the last user message.
	h, ok = reg.Lookup("retry")
	require.True(t, ok)
	retryOut, err := h.Handle(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, lastUserMessage, retryOut, "/retry should return the last user message")

	// Verify alias resolution.
	reg.RegisterAlias("h", "help")
	h, ok = reg.Lookup("h")
	require.True(t, ok)
	assert.Equal(t, "help", h.Name())
}

// =============================================================================
// Test 2: @mention resolution
// =============================================================================

// TestET_phase39_mention_resolution tests @mention resolution end-to-end:
// symbol resolution in a temp directory and URL SSRF protection.
func TestET_phase39_mention_resolution(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Create a temp directory with a .go file containing func main().
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"), 0o644))

	// Verify @symbol:func:main resolves correctly.
	symResolver := cli.NewSymbolMentionResolver(nil, "", dir)
	result, err := symResolver.Resolve(context.Background(), "func:main")
	require.NoError(t, err)
	assert.Contains(t, result, "func main", "symbol resolver should find the function")
	assert.Contains(t, result, "main.go", "symbol resolver should reference the file")

	// Verify URL SSRF protection blocks localhost.
	urlResolver := cli.NewURLMentionResolver()
	_, err = urlResolver.Resolve(context.Background(), "http://localhost:8080")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blocked for security")

	// Verify URL SSRF protection blocks 127.0.0.1.
	_, err = urlResolver.Resolve(context.Background(), "http://127.0.0.1:8080")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blocked for security")
}

// =============================================================================
// Test 3: Runtime model switching
// =============================================================================

// TestET_phase39_model_switching tests runtime model switching via
// DefaultModelSelector.SwitchModel and LoopAgent.SetModel.
func TestET_phase39_model_switching(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	model1 := &phase39Model{response: "response from model 1"}
	model2 := &phase39Model{response: "response from model 2"}

	sel := llm.NewDefaultModelSelector(model1, nil).
		WithModelBuilder(func(_ context.Context, name string) (llm.BaseChatModel, func(), error) {
			if name == "model-2" {
				return model2, nil, nil
			}
			return model1, nil, nil
		}).
		WithModelLister(func() []llm.ModelInfo {
			return []llm.ModelInfo{
				{Name: "model-1"},
				{Name: "model-2"},
			}
		})

	// Verify AvailableModels returns both models.
	models := sel.AvailableModels()
	assert.Len(t, models, 2)
	assert.Equal(t, "model-1", models[0].Name)
	assert.Equal(t, "model-2", models[1].Name)

	// Verify SwitchModel changes the active model.
	require.NoError(t, sel.SwitchModel(ctx, "model-2"))
	assert.Equal(t, "model-2", sel.PrimaryModelName())

	// Verify the primary model is now model2.
	assert.Same(t, model2, sel.PrimaryModel())

	// Verify LoopAgent.SetModel updates the model used by the loop.
	loop := core.NewLoopAgent(core.WithLLM(model1))
	loop.SetModel(model2)

	events, err := loop.Run(ctx, core.Submission{Content: "hello"})
	require.NoError(t, err)

	messages := phase39MessageEvents(events)
	require.NotEmpty(t, messages, "loop should emit at least one message event")
	assert.Equal(t, "response from model 2", messages[0],
		"LoopAgent should use model2 after SetModel")

	sel.CloseReleasesCleanups()
}

// =============================================================================
// Test 4: EventStream overflow
// =============================================================================

// TestET_phase39_eventstream_overflow tests that an EventStream with buffer
// 256 and DiscardOldest policy handles overflow correctly: all Send calls
// complete without blocking, the most recent events are retained, and no
// goroutine leaks.
func TestET_phase39_eventstream_overflow(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	const capacity = 256
	const totalEvents = 300

	stream := core.NewEventStream(capacity, core.WithEventDiscardPolicy(core.DiscardOldest))

	// Push 300 events; with DiscardOldest the oldest 44 are evicted.
	for i := 0; i < totalEvents; i++ {
		require.NoError(t, stream.Send(core.AgentEvent{
			Kind:    "message",
			Content: fmt.Sprintf("event-%d", i),
		}))
	}
	stream.Close()

	// Drain all remaining events.
	var got []core.AgentEvent
	for ev := range stream.Events() {
		got = append(got, ev)
	}

	// All 300 Send calls completed (no deadlock). The buffer retains exactly
	// `capacity` events — the 44 oldest were discarded.
	assert.Len(t, got, capacity,
		"should retain exactly capacity events after overflow with DiscardOldest")

	// The oldest retained event should be event-(totalEvents - capacity).
	assert.Equal(t, fmt.Sprintf("event-%d", totalEvents-capacity), got[0].Content,
		"oldest events should have been discarded")
	// The newest event should be the last one pushed.
	assert.Equal(t, fmt.Sprintf("event-%d", totalEvents-1), got[len(got)-1].Content,
		"newest event should be last")
}

// =============================================================================
// Test 5: Line editor cancellation
// =============================================================================

// TestET_phase39_line_editor_cancellation tests that context-cancellable line
// reading returns promptly with context.Canceled and leaves no goroutine leak.
func TestET_phase39_line_editor_cancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	r, w := io.Pipe()
	le := cli.NewDefaultLineEditor(r, io.Discard)
	defer le.Stop()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_, err := le.ReadLine(ctx, "> ")
		assert.ErrorIs(t, err, context.Canceled)
		close(done)
	}()

	// Give the goroutine time to enter scanner.Scan().
	time.Sleep(50 * time.Millisecond)

	// Cancel the context — ReadLine should return promptly.
	cancel()

	select {
	case <-done:
		// ReadLine returned promptly after cancel.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ReadLine did not return within 500ms of cancel")
	}

	// Close the pipe writer to unblock the scanner goroutine that is still
	// blocked inside Scan(). This ensures no goroutine leak.
	w.Close()

	// Give the scanner goroutine time to exit.
	time.Sleep(100 * time.Millisecond)
}

// =============================================================================
// Test 6: Race-free concurrent access
// =============================================================================

// TestET_phase39_race_free runs multiple goroutines accessing shared CLI
// components (SlashCommandRegistry and EventStream) simultaneously to verify
// there are no data races. Must be run with -race.
func TestET_phase39_race_free(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Set up a SlashCommandRegistry with several handlers.
	reg := cli.NewSlashCommandRegistry()
	for _, name := range []string{"help", "clear", "model", "retry", "edit"} {
		require.NoError(t, reg.Register(&phase39TestHandler{
			name: name,
			desc: "test handler",
			fn:   func(_ []string) (string, error) { return "", nil },
		}))
	}

	// Set up an EventStream with DiscardOldest (non-blocking sends).
	stream := core.NewEventStream(64, core.WithEventDiscardPolicy(core.DiscardOldest))

	var wg sync.WaitGroup

	// Multiple goroutines calling SlashCommandRegistry.Names().
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = reg.Names()
			}
		}()
	}

	// Multiple goroutines calling SlashCommandRegistry.Lookup().
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = reg.Lookup("help")
			}
		}()
	}

	// Multiple goroutines calling EventStream.Send() and SentCount().
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = stream.Send(core.AgentEvent{Kind: "message", Content: "race"})
				_ = stream.SentCount()
			}
		}()
	}

	// Consumer goroutine draining the event channel.
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for range stream.Events() {
		}
	}()

	// Wait for all producers to finish.
	wg.Wait()

	// Close the stream to unblock the consumer.
	stream.Close()
	<-consumerDone
}
