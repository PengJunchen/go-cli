package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/memory"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// stubParameterizedTool is a stubTool that also implements Parameterized so
// /context can measure its JSON schema tokens.
type stubParameterizedTool struct {
	stubTool
	params any
}

func (s *stubParameterizedTool) Parameters() any { return s.params }

// stubContextLoader is a minimal ProjectContextLoader for testing.
type stubContextLoader struct {
	files []core.ContextFile
}

func (l *stubContextLoader) Load(_ context.Context, _ string) ([]core.ContextFile, error) {
	return l.files, nil
}

// TestContextHandlerTokenBreakdown verifies that /context produces output
// containing token counts for every required category (AC-1).
func TestContextHandlerTokenBreakdown(t *testing.T) {
	estimator := compaction.NewHeuristicTokenEstimator()

	// Tool registry with one tool.
	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), &stubParameterizedTool{
		stubTool: stubTool{name: "read_file", desc: "reads a file from disk"},
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
		},
	}))

	// Context files (CLAUDE.md).
	loader := &stubContextLoader{
		files: []core.ContextFile{
			{Path: "/project/CLAUDE.md", Content: "This is the project guide for CLAUDE.md."},
		},
	}

	// Memory store with one memory.
	store := newTestMemoryStore(t)
	require.NoError(t, store.Add(context.Background(), memory.Memory{
		ID: "mem_1", Content: "User prefers concise answers", Category: "preference", Source: "manual",
	}))

	// Agent with conversation messages.
	agent := core.NewAgentImpl("test", &stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "Hello, can you help me?"},
		{Role: "assistant", Content: "Of course! What do you need?"},
	}))

	var buf bytes.Buffer
	sc := &slashContext{
		out:           &buf,
		contextWindow: 128000,
		estimator:     estimator,
		promptBuilder: core.NewDefaultSystemPromptBuilder(),
		contextLoader: loader,
		memoryStore:   store,
		toolRegistry:  reg,
		agent:         agent,
	}

	h := &ContextHandler{}
	_, err := h.Handle(context.Background(), nil, sc)
	require.NoError(t, err)

	output := buf.String()

	// AC-1: each category must appear with a token count.
	assert.Contains(t, output, "System prompt")
	assert.Contains(t, output, "Tool definitions")
	assert.Contains(t, output, "CLAUDE.md / context files")
	assert.Contains(t, output, "Memories")
	assert.Contains(t, output, "Messages")

	// Every category line should contain "tokens".
	for _, label := range []string{"System prompt", "Tool definitions", "CLAUDE.md / context files", "Memories", "Messages"} {
		line := findLineContaining(output, label)
		assert.NotEmpty(t, line, "expected a line containing %q", label)
		assert.Contains(t, line, "tokens", "line for %q should contain token count", label)
	}

	// Total and remaining lines.
	assert.Contains(t, output, "Total:")
	assert.Contains(t, output, "Remaining:")
	assert.Contains(t, output, "128000")
}

// TestContextHandlerPercentageBasedOnWindow verifies that the percentage is
// based on the context window, not cumulative API calls (AC-2). We check that
// the percentage shown for "Total" matches total_tokens * 100 / context_window.
func TestContextHandlerPercentageBasedOnWindow(t *testing.T) {
	estimator := compaction.NewHeuristicTokenEstimator()

	agent := core.NewAgentImpl("test", &stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: strings.Repeat("a", 400)}, // ~100 tokens
	}))

	var buf bytes.Buffer
	// Use a small context window so the percentage is non-trivial.
	sc := &slashContext{
		out:           &buf,
		contextWindow: 1000,
		estimator:     estimator,
		agent:         agent,
		// promptBuilder, contextLoader, toolRegistry, memoryStore are nil;
		// the handler degrades gracefully.
	}

	h := &ContextHandler{}
	_, err := h.Handle(context.Background(), nil, sc)
	require.NoError(t, err)

	output := buf.String()

	// The messages category should show a non-zero token count.
	msgsLine := findLineContaining(output, "Messages")
	assert.Contains(t, msgsLine, "tokens")

	// The total percentage should be based on the context window (1000),
	// not on any cumulative call count. Since only messages contribute,
	// the total percentage should equal the messages percentage.
	totalLine := findLineContaining(output, "Total:")
	assert.Contains(t, totalLine, "of 1000")

	// Extract the percentage from the total line and verify it's
	// total_tokens * 100 / 1000.
	// The total line format is: "Total:     N tokens (P% of 1000)"
	totalPct := extractPercentage(totalLine)
	msgPct := extractPercentage(msgsLine)
	assert.Greater(t, totalPct, 0, "total percentage should be non-zero")
	assert.Equal(t, msgPct, totalPct, "total %% should equal messages %% since only messages contribute")
}

// TestContextHandlerNoEstimatorFallback verifies that /context works when the
// estimator is nil (falls back to HeuristicTokenEstimator).
func TestContextHandlerNoEstimatorFallback(t *testing.T) {
	agent := core.NewAgentImpl("test", &stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "test message"},
	}))

	var buf bytes.Buffer
	sc := &slashContext{
		out:           &buf,
		contextWindow: 50000,
		agent:         agent,
		// estimator is nil — handler should fall back.
	}

	h := &ContextHandler{}
	_, err := h.Handle(context.Background(), nil, sc)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "Messages")
	assert.Contains(t, output, "tokens")
}

// TestContextHandlerNoContextWindow verifies that /context prints a diagnostic
// when the context window is not configured.
func TestContextHandlerNoContextWindow(t *testing.T) {
	var buf bytes.Buffer
	sc := &slashContext{
		out:           &buf,
		contextWindow: 0,
	}

	h := &ContextHandler{}
	_, err := h.Handle(context.Background(), nil, sc)
	require.NoError(t, err)

	assert.Contains(t, buf.String(), "not configured")
}

// TestSlashContextDispatch verifies that /context is wired into the slash
// command registry and can be dispatched via handleSlashCommand.
func TestSlashContextDispatch(t *testing.T) {
	agent := core.NewAgentImpl("test", &stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "hello"},
	}))

	c, buf := newTestCmd()
	sc := &slashContext{
		out:           buf,
		contextWindow: 64000,
		agent:         agent,
	}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "context"}, sc)

	output := buf.String()
	assert.Contains(t, output, "Context window breakdown:")
	assert.Contains(t, output, "Messages")
}

// findLineContaining returns the first line in s that contains substr, or
// empty string when not found.
func findLineContaining(s, substr string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

// extractPercentage extracts the integer percentage from a line of the form
// "... (N%) ...". Returns 0 when no percentage is found.
func extractPercentage(line string) int {
	idx := strings.Index(line, "(")
	if idx < 0 {
		return 0
	}
	rest := line[idx+1:]
	pctIdx := strings.Index(rest, "%")
	if pctIdx < 0 {
		return 0
	}
	numStr := strings.TrimSpace(rest[:pctIdx])
	n := 0
	for _, ch := range numStr {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}
