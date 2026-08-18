package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebFetchName(t *testing.T) {
	tool := NewWebFetchTool()
	assert.Equal(t, "web_fetch", tool.Name())
}

func TestWebFetchDescription(t *testing.T) {
	tool := NewWebFetchTool()
	assert.Contains(t, tool.Description(), "web_fetch")
	assert.Contains(t, tool.Description(), "url")
}

func TestWebFetchDefaults(t *testing.T) {
	tool := NewWebFetchTool()
	assert.Equal(t, 30*time.Second, tool.Timeout)
	assert.Equal(t, 1<<20, tool.MaxBytes)
}

func TestWebFetchOptions(t *testing.T) {
	tool := NewWebFetchTool(
		WithWebFetchTimeout(5*time.Second),
		WithWebFetchMaxBytes(1024),
	)
	assert.Equal(t, 5*time.Second, tool.Timeout)
	assert.Equal(t, 1024, tool.MaxBytes)
}

func TestWebFetchSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world")) //nolint:errcheck
	}))
	defer srv.Close()

	tool := NewWebFetchTool(WithWebFetchClient(srv.Client()))
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-1",
		Name: "web_fetch",
		Args: map[string]any{"url": srv.URL},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello world", res.Output)
	assert.Equal(t, 200, res.Metadata["status"])
	assert.Equal(t, "call-1", res.ToolCallID)
}

func TestWebFetchMissingURL(t *testing.T) {
	tool := NewWebFetchTool()
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing string argument 'url'")
}

func TestWebFetchInvalidURL(t *testing.T) {
	tool := NewWebFetchTool()
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"url": "http://127.0.0.1:0/notrunning"},
	})
	assert.Error(t, err)
}

func TestWebFetchTruncation(t *testing.T) {
	body := strings.Repeat("x", 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body)) //nolint:errcheck
	}))
	defer srv.Close()

	tool := NewWebFetchTool(WithWebFetchClient(srv.Client()), WithWebFetchMaxBytes(100))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"url": srv.URL},
	})
	require.NoError(t, err)
	assert.Len(t, res.Output, 100)
	assert.True(t, res.Metadata["truncated"].(bool)) //nolint:errcheck
}

func TestWebFetchTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tool := NewWebFetchTool(WithWebFetchClient(srv.Client()), WithWebFetchTimeout(100*time.Millisecond))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"url": srv.URL},
	})
	assert.Error(t, err)
}

func TestWebFetchContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tool := NewWebFetchTool(WithWebFetchClient(srv.Client()), WithWebFetchTimeout(10*time.Second))
	_, err := tool.Execute(ctx, ToolCall{
		Args: map[string]any{"url": srv.URL},
	})
	assert.Error(t, err)
}

func TestWebFetchReturnsBodyOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found")) //nolint:errcheck
	}))
	defer srv.Close()

	tool := NewWebFetchTool(WithWebFetchClient(srv.Client()))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"url": srv.URL},
	})
	require.NoError(t, err)
	assert.Equal(t, "not found", res.Output)
	assert.Equal(t, 404, res.Metadata["status"])
}
