package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

const cachePrivacyMarker = "CACHE_PRIVACY_SENTINEL_DO_NOT_EMIT_9d0e1a"

func TestToolCacheUsesBoundedDomainSeparatedDigest(t *testing.T) {
	argsA := map[string]any{
		"token": cachePrivacyMarker,
		"nested": map[string]any{
			"z": 2,
			"a": 1,
		},
	}
	argsB := map[string]any{}
	argsB["nested"] = map[string]any{"a": 1, "z": 2}
	argsB["token"] = cachePrivacyMarker

	keyA := runAndCaptureToolCacheKey(t, "read", argsA)
	keyB := runAndCaptureToolCacheKey(t, "read", argsB)
	keyDifferentTool := runAndCaptureToolCacheKey(t, "find", argsA)
	keyDifferentArgs := runAndCaptureToolCacheKey(t, "read", map[string]any{"token": "different"})

	assert.Equal(t, keyA, keyB, "JSON map insertion order must not affect the cache key")
	assert.NotEqual(t, keyA, keyDifferentTool, "tool names must be part of the digest input")
	assert.NotEqual(t, keyA, keyDifferentArgs, "different arguments must not alias")
	assert.NotContains(t, keyA, cachePrivacyMarker)
	assert.NotContains(t, keyA, `"token"`)
	assert.Regexp(t, `^tool-cache:v1:[0-9a-f]{64}$`, keyA)

	encodedArgs, err := json.Marshal(argsA)
	require.NoError(t, err)
	digestInput := append([]byte("go-cli/tool-cache/v1\x00read\x00"), encodedArgs...)
	sum := sha256.Sum256(digestInput)
	assert.Equal(t, "tool-cache:v1:"+hex.EncodeToString(sum[:]), keyA)
}

func TestToolCacheDoesNotLogRawArgs(t *testing.T) {
	var logBuffer bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	exporter := &cachePrivacyTraceExporter{}
	tracer := tracing.NewTracer("cache-privacy-test", exporter)
	root, ctx := tracer.Start(context.Background(), "test.root", tracing.SpanKindInternal)
	t.Cleanup(root.End)

	cache := production.NewFIFOIdempotentCache(4)
	nextCalls := 0
	execute := newProductionToolWrapper(cache, nil, nil, "session")(
		func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			nextCalls++
			return &tools.ToolResult{Output: "safe result"}, nil
		},
	)
	call := tools.ToolCall{
		Name: "read",
		Args: map[string]any{"api_key": cachePrivacyMarker, "query": "private search text"},
	}

	for range 2 {
		result, err := execute(ctx, call)
		require.NoError(t, err)
		require.Equal(t, "safe result", result.Output)
	}
	assert.Equal(t, 1, nextCalls, "the second identical call should be served from the FIFO cache")

	encodedArgs, err := json.Marshal(call.Args)
	require.NoError(t, err)
	assert.NotContains(t, logBuffer.String(), cachePrivacyMarker)
	assert.NotContains(t, logBuffer.String(), string(encodedArgs))

	spanJSON, err := json.Marshal(exporter.snapshot())
	require.NoError(t, err)
	assert.NotContains(t, string(spanJSON), cachePrivacyMarker)
	assert.NotContains(t, string(spanJSON), string(encodedArgs))

	var sawCacheKey bool
	for _, span := range exporter.snapshot() {
		for _, attribute := range span.Attributes {
			if attribute.Key != "cache_key" {
				continue
			}
			sawCacheKey = true
			key, ok := attribute.Value.(string)
			require.True(t, ok)
			assert.Regexp(t, `^tool-cache:v1:[0-9a-f]{64}$`, key)
		}
	}
	assert.True(t, sawCacheKey, "the real FIFO cache should emit a cache lookup span")
}

func TestToolCacheMarshalFailureBypassesGetAndSet(t *testing.T) {
	cache := &privacySpyCache{values: make(map[string]any)}
	nextCalls := 0
	execute := newProductionToolWrapper(cache, nil, nil, "session")(
		func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			nextCalls++
			return &tools.ToolResult{Output: "executed"}, nil
		},
	)
	call := tools.ToolCall{
		Name: "read",
		Args: map[string]any{"api_key": cachePrivacyMarker, "unmarshalable": make(chan int)},
	}

	for range 2 {
		result, err := execute(context.Background(), call)
		require.NoError(t, err)
		require.Equal(t, "executed", result.Output)
	}

	assert.Equal(t, 2, nextCalls, "unmarshalable calls must execute every time")
	assert.Empty(t, cache.getKeys, "marshal failure must bypass cache lookup")
	assert.Empty(t, cache.setKeys, "marshal failure must not cache under a bare tool name")
}

func TestToolCacheMutationAndNilCacheRemainBypassed(t *testing.T) {
	spy := &privacySpyCache{values: make(map[string]any)}
	mutationCalls := 0
	mutation := newProductionToolWrapper(spy, nil, nil, "session")(
		func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			mutationCalls++
			return &tools.ToolResult{Output: "mutated"}, nil
		},
	)
	for range 2 {
		_, err := mutation(context.Background(), tools.ToolCall{Name: "write", Args: map[string]any{"path": "file.txt"}})
		require.NoError(t, err)
	}
	assert.Equal(t, 2, mutationCalls)
	assert.Empty(t, spy.getKeys)
	assert.Empty(t, spy.setKeys)

	nilCacheCalls := 0
	nilCache := newProductionToolWrapper(nil, nil, nil, "session")(
		func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			nilCacheCalls++
			return &tools.ToolResult{Output: "uncached"}, nil
		},
	)
	for range 2 {
		_, err := nilCache(context.Background(), tools.ToolCall{Name: "read", Args: map[string]any{"query": "x"}})
		require.NoError(t, err)
	}
	assert.Equal(t, 2, nilCacheCalls, "a nil cache must leave calls uncached")
}

func runAndCaptureToolCacheKey(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	cache := &privacySpyCache{values: make(map[string]any)}
	execute := newProductionToolWrapper(cache, nil, nil, "session")(
		func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			return &tools.ToolResult{Output: "ok"}, nil
		},
	)
	_, err := execute(context.Background(), tools.ToolCall{Name: name, Args: args})
	require.NoError(t, err)
	require.Len(t, cache.getKeys, 1)
	require.Len(t, cache.setKeys, 1)
	assert.Equal(t, cache.getKeys[0], cache.setKeys[0])
	return cache.setKeys[0]
}

type privacySpyCache struct {
	getKeys []string
	setKeys []string
	values  map[string]any
}

func (c *privacySpyCache) Get(_ context.Context, key string) (any, bool) {
	c.getKeys = append(c.getKeys, key)
	value, ok := c.values[key]
	return value, ok
}

func (c *privacySpyCache) Set(_ context.Context, key string, value any) error {
	c.setKeys = append(c.setKeys, key)
	c.values[key] = value
	return nil
}

func (c *privacySpyCache) Delete(_ context.Context, key string) error {
	delete(c.values, key)
	return nil
}

func (*privacySpyCache) Name() string { return "privacy-spy" }

type cachePrivacyTraceExporter struct {
	mu       sync.Mutex
	gotSpans []tracing.SpanData
}

func (e *cachePrivacyTraceExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gotSpans = append(e.gotSpans, tracing.SpanToData(span))
	return nil
}

func (*cachePrivacyTraceExporter) Shutdown(context.Context) error { return nil }

func (e *cachePrivacyTraceExporter) snapshot() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]tracing.SpanData(nil), e.gotSpans...)
}

var _ production.IdempotentCache = (*privacySpyCache)(nil)
var _ tracing.TraceExporter = (*cachePrivacyTraceExporter)(nil)
