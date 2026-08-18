package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tui"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// drainPhase39Events reads all events from a stream until the channel closes.
func drainPhase39Events(stream core.EventStream) []core.AgentEvent {
	var evs []core.AgentEvent
	for ev := range stream.Events() {
		evs = append(evs, ev)
	}
	return evs
}

// ---------------------------------------------------------------------------
// 1. Slash commands integration (39-4, 39-5, 39-6, 39-10)
// ---------------------------------------------------------------------------

// TestPhase39_SlashHelpListsCommands verifies that /help lists all commands
// including /edit, /model, and /retry.
func TestPhase39_SlashHelpListsCommands(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "help"}, sc)
	output := buf.String()

	for _, want := range []string{"/help", "/edit", "/model", "/retry"} {
		assert.Contains(t, output, want, "help output should list %s", want)
	}
}

// TestPhase39_SlashModelSwitch verifies that /model <name> switches the model
// at runtime via the model selector with a switch callback.
func TestPhase39_SlashModelSwitch(t *testing.T) {
	var switched llm.BaseChatModel
	sel := llm.NewDefaultModelSelector(&stubModel{}, nil).
		WithModelNames("openai", "gpt-4o", "", "").
		WithModelBuilder(func(_ context.Context, _ string) (llm.BaseChatModel, func(), error) {
			return &stubModel{}, nil, nil
		}).
		WithModelSwitchCallback(func(m llm.BaseChatModel) {
			switched = m
		}).
		WithModelLister(func() []llm.ModelInfo {
			return []llm.ModelInfo{
				{Name: "gpt-4o"},
				{Name: "gpt-4o-mini"},
			}
		})

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, modelSelector: sel}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "model",
		Args: []string{"gpt-4o-mini"},
	}, sc)

	assert.Contains(t, buf.String(), "Switched to model: gpt-4o-mini")
	assert.Equal(t, "gpt-4o-mini", sel.PrimaryModelName())
	assert.NotNil(t, switched, "switch callback should have been called")
}

// TestPhase39_SlashRetryResubmits verifies that /retry re-submits the last
// user message as pendingInput and truncates history.
func TestPhase39_SlashRetryResubmits(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "what is 2+2?"},
		{Role: "assistant", Content: "4"},
	}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "retry"}, sc)

	assert.Equal(t, "what is 2+2?", pendingInput, "pendingInput should be the last user message")
	assert.Contains(t, buf.String(), "Retrying last message...")
	assert.Empty(t, agent.Messages(), "history should be empty after removing the user+assistant pair")
}

// TestPhase39_SlashEditContentSubmitted verifies that /edit with a test editor
// returns the editor content as pendingInput.
func TestPhase39_SlashEditContentSubmitted(t *testing.T) {
	cleanup := withTestEditor(t, "Hello from editor!\nMultiple lines.\n")
	defer cleanup()

	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "edit"}, sc)

	assert.Equal(t, "Hello from editor!\nMultiple lines.\n", pendingInput)
}

// TestPhase39_SlashVimAlias verifies that /vim works the same as /edit.
func TestPhase39_SlashVimAlias(t *testing.T) {
	cleanup := withTestEditor(t, "vim alias works\n")
	defer cleanup()

	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "vim"}, sc)

	assert.Equal(t, "vim alias works\n", pendingInput)
}

// ---------------------------------------------------------------------------
// 2. Mention resolver integration (39-11)
// ---------------------------------------------------------------------------

// TestPhase39_MentionSymbolResolves verifies that @symbol:func:main resolves
// in a temp directory with Go files.
func TestPhase39_MentionSymbolResolves(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"), 0o644))

	resolver := NewSymbolMentionResolver(nil, "", dir)
	result, err := resolver.Resolve(context.Background(), "func:main")
	require.NoError(t, err)
	assert.Contains(t, result, "func main")
	assert.Contains(t, result, "main.go")
}

// TestPhase39_MentionURLBlocked verifies that @url: with an internal address
// (localhost) is blocked with a security error.
func TestPhase39_MentionURLBlocked(t *testing.T) {
	resolver := NewURLMentionResolver()
	_, err := resolver.Resolve(context.Background(), "http://localhost:8080")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blocked for security")
}

// TestPhase39_MentionExpanderExpands verifies that the MentionExpander
// correctly expands typed @-mentions in a message.
func TestPhase39_MentionExpanderExpands(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"), 0o644))

	e := NewMentionExpander(dir, 0)
	e.SetResolver("symbol", NewSymbolMentionResolver(nil, "", dir))
	expanded, files, _, err := e.Expand(context.Background(), "explain @symbol:func:main")
	require.NoError(t, err)
	assert.Contains(t, expanded, `<mention type="symbol">`)
	assert.Contains(t, expanded, "func main")
	require.Len(t, files, 1)
	assert.Contains(t, files[0], "symbol:func:main")
}

// ---------------------------------------------------------------------------
// 3. Line editor features (39-7, 39-8)
// ---------------------------------------------------------------------------

// TestPhase39_NonTTYReadLineCancellation verifies that non-TTY readLine can
// be cancelled via context and returns context.Canceled promptly.
func TestPhase39_NonTTYReadLineCancellation(t *testing.T) {
	r, w := io.Pipe()
	le := NewDefaultLineEditor(r, io.Discard)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_, err := le.ReadLine(ctx, "> ")
		assert.ErrorIs(t, err, context.Canceled)
		close(done)
	}()

	// Give the goroutine time to enter scanner.Scan().
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// ReadLine returned promptly after cancel.
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ReadLine did not return within 200ms of cancel")
	}

	// Close the pipe writer to unblock the scanner goroutine.
	w.Close()
}

// TestPhase39_FindReverseMatch verifies that findReverseMatch correctly finds
// matches in history, searching newest-first and case-insensitively.
func TestPhase39_FindReverseMatch(t *testing.T) {
	entries := []string{"hello world", "foo bar", "hello there", "World peace"}

	// Search "hello" — newest match first (index 2).
	idx := findReverseMatch(entries, "hello", -1)
	assert.Equal(t, 2, idx)

	// Next match (older): index 0.
	idx = findReverseMatch(entries, "hello", idx)
	assert.Equal(t, 0, idx)

	// No more matches.
	idx = findReverseMatch(entries, "hello", idx)
	assert.Equal(t, -1, idx)

	// Case-insensitive: "world" matches both "hello world" and "World peace".
	idx = findReverseMatch(entries, "world", -1)
	assert.Equal(t, 3, idx) // "World peace" (newest)
	idx = findReverseMatch(entries, "world", idx)
	assert.Equal(t, 0, idx) // "hello world"

	// No match.
	assert.Equal(t, -1, findReverseMatch(entries, "xyz", -1))

	// Empty query returns -1.
	assert.Equal(t, -1, findReverseMatch(entries, "", -1))
}

// ---------------------------------------------------------------------------
// 4. EventStream capacity (39-9)
// ---------------------------------------------------------------------------

// TestPhase39_EventStreamOverflow256 verifies that EventStream with
// DiscardOldest policy and 256 capacity handles overflow correctly by
// discarding the oldest events and retaining the most recent 256.
func TestPhase39_EventStreamOverflow256(t *testing.T) {
	const capacity = 256
	stream := core.NewEventStream(capacity, core.WithEventDiscardPolicy(core.DiscardOldest))

	// Send more events than capacity to trigger overflow.
	totalEvents := 300
	for i := 0; i < totalEvents; i++ {
		require.NoError(t, stream.Send(core.AgentEvent{
			Kind:    "message",
			Content: fmt.Sprintf("event-%d", i),
		}))
	}
	stream.Close()

	got := drainPhase39Events(stream)

	// Only the most recent `capacity` events should remain.
	assert.Len(t, got, capacity, "should retain exactly capacity events after overflow")

	// The oldest events should have been discarded; first retained event
	// should be event-(totalEvents - capacity).
	firstContent := fmt.Sprintf("event-%d", totalEvents-capacity)
	assert.Equal(t, firstContent, got[0].Content, "oldest events should have been discarded")
	assert.Equal(t, fmt.Sprintf("event-%d", totalEvents-1), got[len(got)-1].Content,
		"newest event should be last")
}

// ---------------------------------------------------------------------------
// 5. REPL session integration (39-1, 39-2, 39-3)
// ---------------------------------------------------------------------------

// TestPhase39_REPLSessionCompleteTurn verifies that REPLSession can handle a
// complete turn (user input → assistant response) with memory extraction
// persisted to the memory store.
func TestPhase39_REPLSessionCompleteTurn(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t, memoryExtractionResponse, 0, 0)
	defer srv.Close()

	cfg, homeDir := newMemoryTestConfig(t, srv.URL)

	var out bytes.Buffer
	in := strings.NewReader("hello\nexit\n")
	cmd := newInteractiveCmd(in, &out)

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)

	// Verify the session completed normally.
	assert.Contains(t, out.String(), "Session ended")

	// Verify the assistant response was printed (non-TTY mode prints "AI: <msg>").
	assert.Contains(t, out.String(), "AI:")

	// Verify memory extraction occurred and the memory was persisted.
	memPath := filepath.Join(homeDir, ".go-cli", "memories.jsonl")
	data, err := os.ReadFile(memPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "user prefers Go")
}

// TestPhase39_HITLApprovalChannel verifies that the HITL approval channel
// works with capacity 32, matching the buffer used by REPLSession in TTY mode.
func TestPhase39_HITLApprovalChannel(t *testing.T) {
	approvalCh := make(chan tui.ApprovalRequest, 32)

	// Send 32 requests without blocking (fills the buffer).
	for i := 0; i < 32; i++ {
		approvalCh <- tui.ApprovalRequest{
			ToolName:   fmt.Sprintf("tool-%d", i),
			Args:       map[string]any{"index": i},
			ResponseCh: make(chan tui.ApprovalResponse, 1),
		}
	}

	// The 33rd send should block because the channel is full.
	select {
	case approvalCh <- tui.ApprovalRequest{ToolName: "tool-32"}:
		t.Fatal("channel should be full, 33rd send should block")
	case <-time.After(50 * time.Millisecond):
		// Expected: send blocked.
	}

	// All 32 requests should be receivable in FIFO order.
	for i := 0; i < 32; i++ {
		req := <-approvalCh
		assert.Equal(t, fmt.Sprintf("tool-%d", i), req.ToolName)
	}

	// Channel should now be empty.
	assert.Equal(t, 0, len(approvalCh))
}

// ---------------------------------------------------------------------------
// 6. Comprehensive REPL flow (39-13 subtask 3: E2E integration)
// ---------------------------------------------------------------------------

// TestPhase39_REPLFlow_SlashMentionModelRetryEdit exercises the full REPL
// flow in a single test: slash commands, @mention resolution, runtime model
// switching, /retry, and /edit — all through the same interactiveCmd and
// slashContext, simulating a real user session.
func TestPhase39_REPLFlow_SlashMentionModelRetryEdit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// --- Setup: temp dir with Go source for @mention resolution ---
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"), 0o644))

	// --- Setup: model selector with two models ---
	sel := llm.NewDefaultModelSelector(&stubModel{resp: &llm.Message{Role: llm.RoleAssistant, Content: "ok"}}, nil).
		WithModelNames("openai", "gpt-4o", "", "").
		WithModelBuilder(func(_ context.Context, _ string) (llm.BaseChatModel, func(), error) {
			return &stubModel{resp: &llm.Message{Role: llm.RoleAssistant, Content: "switched-ok"}}, nil, nil
		}).
		WithModelLister(func() []llm.ModelInfo {
			return []llm.ModelInfo{{Name: "gpt-4o"}, {Name: "gpt-4o-mini"}}
		})

	// --- Setup: agent with history for /retry ---
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "what is 1+1?"},
		{Role: "assistant", Content: "2"},
	}))

	// --- Setup: mention expander with symbol resolver ---
	mentionExpander := NewMentionExpander(dir, 0)
	mentionExpander.SetResolver("symbol", NewSymbolMentionResolver(nil, "", dir))

	// --- Setup: interactiveCmd + slashContext ---
	c, buf := newTestCmd()
	sc := &slashContext{
		out:           buf,
		agent:         agent,
		modelSelector: sel,
	}

	ctx := context.Background()

	// Step 1: /help — verify all Phase 39 commands are listed.
	c.handleSlashCommand(ctx, session.SlashCommand{Name: "help"}, sc)
	helpOutput := buf.String()
	for _, want := range []string{"/help", "/model", "/retry", "/edit"} {
		assert.Contains(t, helpOutput, want, "/help should list %s", want)
	}
	buf.Reset()

	// Step 2: @symbol:func:main — verify mention resolution works.
	expanded, files, _, err := mentionExpander.Expand(ctx, "explain @symbol:func:main")
	require.NoError(t, err)
	assert.Contains(t, expanded, "func main")
	require.Len(t, files, 1)
	assert.Contains(t, files[0], "symbol:func:main")

	// Step 3: /model — list available models.
	c.handleSlashCommand(ctx, session.SlashCommand{Name: "model"}, sc)
	assert.Contains(t, buf.String(), "gpt-4o")
	assert.Contains(t, buf.String(), "gpt-4o-mini")
	buf.Reset()

	// Step 4: /model gpt-4o-mini — switch model at runtime.
	c.handleSlashCommand(ctx, session.SlashCommand{
		Name: "model",
		Args: []string{"gpt-4o-mini"},
	}, sc)
	assert.Contains(t, buf.String(), "Switched to model: gpt-4o-mini")
	assert.Equal(t, "gpt-4o-mini", sel.PrimaryModelName())
	buf.Reset()

	// Step 5: /retry — re-submit last user message, truncate history.
	pendingInput := c.handleSlashCommand(ctx, session.SlashCommand{Name: "retry"}, sc)
	assert.Equal(t, "what is 1+1?", pendingInput, "/retry should return last user message")
	assert.Contains(t, buf.String(), "Retrying last message...")
	assert.Empty(t, agent.Messages(), "history should be empty after retry")
	buf.Reset()

	// Step 6: /edit — compose via external editor.
	cleanup := withTestEditor(t, "Composed in external editor.\n")
	defer cleanup()
	editInput := c.handleSlashCommand(ctx, session.SlashCommand{Name: "edit"}, sc)
	assert.Equal(t, "Composed in external editor.\n", editInput)

	// Step 7: /vim — alias should also work.
	buf.Reset()
	cleanupVim := withTestEditor(t, "From vim alias.\n")
	defer cleanupVim()
	vimInput := c.handleSlashCommand(ctx, session.SlashCommand{Name: "vim"}, sc)
	assert.Equal(t, "From vim alias.\n", vimInput)

	// Step 8: /model gpt-4o — switch back to original model.
	c.handleSlashCommand(ctx, session.SlashCommand{
		Name: "model",
		Args: []string{"gpt-4o"},
	}, sc)
	assert.Contains(t, buf.String(), "Switched to model: gpt-4o")
	assert.Equal(t, "gpt-4o", sel.PrimaryModelName())
}
