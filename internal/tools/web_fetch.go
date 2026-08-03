package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultWebFetchTimeout is the default HTTP request timeout.
	defaultWebFetchTimeout = 30 * time.Second

	// defaultWebFetchMaxBytes caps the response body size.
	defaultWebFetchMaxBytes = 1 << 20 // 1 MiB
)

// WebFetchToolOption configures a WebFetchTool.
type WebFetchToolOption func(*WebFetchTool)

// WithWebFetchTimeout sets the HTTP request timeout.
func WithWebFetchTimeout(d time.Duration) WebFetchToolOption {
	return func(t *WebFetchTool) { t.Timeout = d }
}

// WithWebFetchMaxBytes caps the number of bytes read from the response body.
func WithWebFetchMaxBytes(n int) WebFetchToolOption {
	return func(t *WebFetchTool) { t.MaxBytes = n }
}

// WithWebFetchClient sets the underlying *http.Client used for requests.
func WithWebFetchClient(c *http.Client) WebFetchToolOption {
	return func(t *WebFetchTool) { t.Client = c }
}

// WebFetchTool fetches a URL and returns its body as text. It implements the
// ToolDefinition interface.
type WebFetchTool struct {
	// Timeout bounds how long a request may run before it is canceled.
	Timeout time.Duration
	// MaxBytes caps the number of bytes read from the response body.
	MaxBytes int
	// Client is the HTTP client used for requests. When nil a default client
	// is used.
	Client *http.Client
}

var _ ToolDefinition = (*WebFetchTool)(nil)

// NewWebFetchTool returns a WebFetchTool with a default 30s timeout and 1 MiB
// body limit. Options may override the defaults.
func NewWebFetchTool(opts ...WebFetchToolOption) *WebFetchTool {
	t := &WebFetchTool{
		Timeout:  defaultWebFetchTimeout,
		MaxBytes: defaultWebFetchMaxBytes,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns the tool name.
func (t *WebFetchTool) Name() string { return "web_fetch" }

// Description returns a brief description of the tool.
func (t *WebFetchTool) Description() string {
	return "web_fetch: fetches a URL and returns its body as text. Args: url (string)."
}

// Execute performs an HTTP GET against the URL provided in args["url"] and
// returns the response body as text. The request is bounded by the configured
// timeout and the response body is capped at MaxBytes.
func (t *WebFetchTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	start := time.Now()

	url, ok := call.Args["url"].(string)
	if !ok || strings.TrimSpace(url) == "" {
		slog.Debug("web_fetch.missing_url")
		return nil, errors.New("web_fetch: missing string argument 'url'")
	}

	slog.Debug("web_fetch.start", "url", url, "timeout", t.Timeout.String())

	fetchCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, url, nil)
	if err != nil {
		slog.Debug("web_fetch.request_failed", "url", url, "err", err)
		return nil, fmt.Errorf("web_fetch: %w", err)
	}

	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("web_fetch.fetch_failed", "url", url, "err", err)
		return nil, fmt.Errorf("web_fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// LimitReader bounds the read so an unexpectedly large body cannot exhaust
	// memory. The +1 lets us detect truncation.
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(t.MaxBytes)+1))
	if err != nil {
		slog.Debug("web_fetch.read_failed", "url", url, "err", err)
		return nil, fmt.Errorf("web_fetch: %w", err)
	}

	truncated := false
	if len(data) > t.MaxBytes {
		data = data[:t.MaxBytes]
		truncated = true
	}

	ms := time.Since(start).Milliseconds()
	slog.Debug("web_fetch.done",
		"url", url,
		"status", resp.StatusCode,
		"bytes", len(data),
		"truncated", truncated,
		"duration_ms", ms)

	return &ToolResult{
		Output:   string(data),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"url":       url,
			"status":    resp.StatusCode,
			"bytes":     len(data),
			"truncated": truncated,
			"duration_ms": ms,
		},
	}, nil
}
