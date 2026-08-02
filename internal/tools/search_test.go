package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// searchTestTool is a small ToolDefinition used to populate search fixtures.
type searchTestTool struct {
	name        string
	description string
}

func (s *searchTestTool) Name() string { return s.name }
func (s *searchTestTool) Description() string {
	return s.description
}
func (s *searchTestTool) Execute(_ context.Context, _ ToolCall) (*ToolResult, error) {
	return &ToolResult{Output: ""}, nil
}

// newSearchFixture builds a registry with a spread of tools across categories.
func newSearchFixture(t *testing.T) *DefaultToolRegistry {
	t.Helper()
	reg := NewDefaultToolRegistry().(*DefaultToolRegistry)
	tools := []ToolDefinition{
		&searchTestTool{name: "read", description: "reads a file's contents"},
		&searchTestTool{name: "write", description: "writes content to a file"},
		&searchTestTool{name: "grep", description: "searches files for a pattern"},
		&searchTestTool{name: "curl", description: "fetches a URL over http"},
		&searchTestTool{name: "bash", description: "runs a shell command"},
	}
	for _, td := range tools {
		require.NoError(t, reg.Register(context.Background(), td))
	}
	return reg
}

func TestSearchByName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewToolSearchTool(newSearchFixture(t))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "grep"},
	})
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(res.Output), "\n")
	require.Len(t, lines, 1)
	assert.Equal(t, "grep", strings.Fields(lines[0])[0])
	assert.Equal(t, 1, res.Metadata["matches"])
}

func TestSearchByDescriptionKeyword(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewToolSearchTool(newSearchFixture(t))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "file"},
	})
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(res.Output), "\n")
	// read, write and grep all mention "file" in their description.
	assert.GreaterOrEqual(t, len(lines), 3)
}

func TestSearchByCategoryFilter(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewToolSearchTool(newSearchFixture(t))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "", "category": "shell"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "bash")
	assert.NotContains(t, res.Output, "read")
	assert.NotContains(t, res.Output, "curl")

	resNetwork, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"category": "network"},
	})
	require.NoError(t, err)
	assert.Contains(t, resNetwork.Output, "curl")
	assert.NotContains(t, resNetwork.Output, "bash")
}

func TestSearchEmptyQueryReturnsAll(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewToolSearchTool(newSearchFixture(t))
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	require.NoError(t, err)
	assert.Equal(t, 5, res.Metadata["matches"])
}

func TestSearchInvalidCategory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewToolSearchTool(newSearchFixture(t))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"category": "bogus"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid category")
}

func TestSearchNilRegistry(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewToolSearchTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
}

func TestSearchEmitsSearchSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	e := &captureExporter{}
	tracer := tracing.NewTracer("trace-search", e)
	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	tool := NewToolSearchTool(newSearchFixture(t))
	_, err := tool.Execute(ctx, ToolCall{
		Args: map[string]any{"query": "grep", "category": "file"},
	})
	require.NoError(t, err)

	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.hasSpan("tool.call") }, 2*time.Second, 10*time.Millisecond)
}

func TestToolSearchName(t *testing.T) {
	tool := NewToolSearchTool(newSearchFixture(t))
	assert.Equal(t, "tool_search", tool.Name())
	assert.Contains(t, tool.Description(), "query")
}
