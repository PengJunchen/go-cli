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
	reg := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
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

func TestToolSearchScore(t *testing.T) {
	tool := NewToolSearchTool(newSearchFixture(t))

	bashDef := &searchTestTool{name: "bash", description: "runs a shell command"}
	grepDef := &searchTestTool{name: "grep", description: "searches files for a pattern"}

	// Name-only match: "bash" matches the name of bashDef but not its
	// description or category.
	nameScore := tool.Score("bash", bashDef)
	// Description-only match: "command" matches the description of bashDef
	// but not its name or category.
	descScore := tool.Score("command", bashDef)
	// No match at all.
	noMatchScore := tool.Score("xyzzy", grepDef)
	// Empty query returns 0.
	emptyScore := tool.Score("", grepDef)

	assert.Equal(t, 3.0, nameScore, "name-only match should score 3.0")
	assert.Equal(t, 2.0, descScore, "description-only match should score 2.0")
	assert.Greater(t, nameScore, descScore, "name match should score higher than description match")
	assert.Equal(t, 0.0, noMatchScore)
	assert.Equal(t, 0.0, emptyScore)
}

func TestTopTools(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewToolSearchTool(newSearchFixture(t))

	// K < total: returns exactly K tools, ranked by relevance.
	result, err := tool.TopTools(context.Background(), "file", 3)
	require.NoError(t, err)
	require.Len(t, result, 3)

	names := make([]string, len(result))
	for i, d := range result {
		names[i] = d.Name()
	}
	// read, write, and grep all have "file" in their description or category.
	assert.Contains(t, names, "read")
	assert.Contains(t, names, "write")
	assert.Contains(t, names, "grep")
	// curl and bash do not match "file".
	assert.NotContains(t, names, "curl")
	assert.NotContains(t, names, "bash")

	// K >= total: returns all tools.
	all, err := tool.TopTools(context.Background(), "file", 10)
	require.NoError(t, err)
	assert.Len(t, all, 5)

	// K = 0: returns empty slice.
	zero, err := tool.TopTools(context.Background(), "file", 0)
	require.NoError(t, err)
	assert.Len(t, zero, 0)

	// Empty query with K < total: all scores are 0, so sorted by name.
	empty, err := tool.TopTools(context.Background(), "", 2)
	require.NoError(t, err)
	require.Len(t, empty, 2)
	assert.Equal(t, "bash", empty[0].Name())
	assert.Equal(t, "curl", empty[1].Name())
}
