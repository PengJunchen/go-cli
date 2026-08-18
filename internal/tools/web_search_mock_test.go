package tools

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebSearchMockWarning verifies that Execute logs a warning when the
// configured provider is a MockSearchProvider.
func TestWebSearchMockWarning(t *testing.T) {
	var logBuf bytes.Buffer
	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	tool := NewWebSearchTool() // defaults to MockSearchProvider
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "golang"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "web_search.using_mock_provider")
	assert.Contains(t, logOutput, "golang")
}

// stubSearchProvider is a non-mock provider used to verify the warning is only
// emitted for MockSearchProvider, not for every provider.
type stubSearchProvider struct{}

func (stubSearchProvider) Name() string { return "stub" }
func (stubSearchProvider) Search(_ context.Context, _ string, _ SearchOptions) ([]SearchResult, error) {
	return []SearchResult{{Title: "Stub", URL: "https://stub.example", Snippet: "s"}}, nil
}

// TestWebSearchNoWarningForNonMockProvider ensures the warning is NOT emitted
// when a non-mock provider is used.
func TestWebSearchNoWarningForNonMockProvider(t *testing.T) {
	var logBuf bytes.Buffer
	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	tool := NewWebSearchTool(WithSearchProvider(stubSearchProvider{}))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "test"},
	})
	require.NoError(t, err)

	assert.NotContains(t, logBuf.String(), "web_search.using_mock_provider")
}
