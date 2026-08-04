package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compile-time interface conformance checks.
var (
	_ WebSearchProvider = (*MockSearchProvider)(nil)
	_ WebSearchProvider = (*FetchSearchProvider)(nil)
	_ WebSearchProvider = (*BraveSearchProvider)(nil)
)

// ---------------------------------------------------------------------------
// MockSearchProvider
// ---------------------------------------------------------------------------

func TestMockSearchProviderReturns3Results(t *testing.T) {
	p := NewMockSearchProvider()
	results, err := p.Search(context.Background(), "golang", SearchOptions{})
	require.NoError(t, err)
	assert.Len(t, results, 3)
	for i, r := range results {
		assert.Equal(t, "Result "+itoa(i+1), r.Title)
		assert.Equal(t, "https://example.com/result"+itoa(i+1), r.URL)
		assert.Contains(t, r.Snippet, "Snippet")
	}
}

func TestMockSearchProviderName(t *testing.T) {
	p := NewMockSearchProvider()
	assert.Equal(t, "mock", p.Name())
}

// ---------------------------------------------------------------------------
// FetchSearchProvider
// ---------------------------------------------------------------------------

func TestFetchSearchProviderParsesHTML(t *testing.T) {
	html := `<html><body>
<h2>Result 1</h2>
<a href="https://example.com/r1">Link 1</a>
<p>Snippet for result 1</p>
<h2>Result 2</h2>
<a href="https://example.com/r2">Link 2</a>
<p>Snippet for result 2</p>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(html)) //nolint:errcheck
	}))
	defer srv.Close()

	p := NewFetchSearchProvider(WithFetchSearchURL(srv.URL))
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.NoError(t, err)
	require.Len(t, results, 2)

	assert.Equal(t, "Result 1", results[0].Title)
	assert.Equal(t, "https://example.com/r1", results[0].URL)
	assert.Equal(t, "Snippet for result 1", results[0].Snippet)

	assert.Equal(t, "Result 2", results[1].Title)
	assert.Equal(t, "https://example.com/r2", results[1].URL)
	assert.Equal(t, "Snippet for result 2", results[1].Snippet)
}

func TestFetchSearchProviderName(t *testing.T) {
	p := NewFetchSearchProvider()
	assert.Equal(t, "fetch", p.Name())
}

func TestFetchSearchProviderServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewFetchSearchProvider(WithFetchSearchURL(srv.URL))
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.Error(t, err)
	assert.Empty(t, results)
	assert.Contains(t, err.Error(), "500")
}

func TestFetchSearchProviderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := NewFetchSearchProvider(
		WithFetchSearchURL(srv.URL),
		WithFetchSearchTimeout(100*time.Millisecond),
	)
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.Error(t, err)
	assert.Empty(t, results)
}

func TestFetchSearchProviderMaxResults(t *testing.T) {
	html := `<html><body>
<h2>A</h2><a href="https://a.com">a</a><p>sa</p>
<h2>B</h2><a href="https://b.com">b</a><p>sb</p>
<h2>C</h2><a href="https://c.com">c</a><p>sc</p>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(html)) //nolint:errcheck
	}))
	defer srv.Close()

	p := NewFetchSearchProvider(WithFetchSearchURL(srv.URL))
	results, err := p.Search(context.Background(), "test", SearchOptions{MaxResults: 2})
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

// ---------------------------------------------------------------------------
// BraveSearchProvider
// ---------------------------------------------------------------------------

func TestBraveSearchProviderParsesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-key", r.Header.Get("X-Subscription-Token"))
		w.WriteHeader(http.StatusOK)
		body := []byte(`{"web":{"results":[
			{"title":"Brave Result","url":"https://brave.com/r1","description":"Brave snippet"},
			{"title":"Second","url":"https://brave.com/r2","description":"Second snippet"}
		]}}`)
		_, _ = w.Write(body) //nolint:errcheck
	}))
	defer srv.Close()

	p := NewBraveSearchProvider("test-key",
		WithBraveSearchURL(srv.URL),
		WithBraveSearchClient(srv.Client()),
	)
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "Brave Result", results[0].Title)
	assert.Equal(t, "https://brave.com/r1", results[0].URL)
	assert.Equal(t, "Brave snippet", results[0].Snippet)
}

func TestBraveSearchProviderName(t *testing.T) {
	p := NewBraveSearchProvider("key")
	assert.Equal(t, "brave", p.Name())
}

func TestBraveSearchProviderMissingKey(t *testing.T) {
	p := NewBraveSearchProvider("")
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.Error(t, err)
	assert.Empty(t, results)
	assert.Contains(t, err.Error(), "api key")
}

func TestBraveSearchProviderServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := NewBraveSearchProvider("bad-key",
		WithBraveSearchURL(srv.URL),
		WithBraveSearchClient(srv.Client()),
	)
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.Error(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// RateLimiter
// ---------------------------------------------------------------------------

func TestRateLimiterTwoRequestsApart(t *testing.T) {
	rl := NewRateLimiterWithInterval(100 * time.Millisecond)
	defer rl.Stop()

	start := time.Now()
	rl.Wait()
	first := time.Now()
	rl.Wait()
	second := time.Now()

	gap := second.Sub(first)
	assert.GreaterOrEqual(t, gap, 90*time.Millisecond, "second request should be at least ~interval after the first")
	total := second.Sub(start)
	assert.GreaterOrEqual(t, total, 100*time.Millisecond)
}

// ---------------------------------------------------------------------------
// ResultCache
// ---------------------------------------------------------------------------

func TestResultCacheSetGetWithinTTL(t *testing.T) {
	cache := NewResultCacheWithTTL(5 * time.Minute)
	results := []SearchResult{
		{Title: "Cached", URL: "https://cached.com", Snippet: "cached snippet"},
	}
	cache.Set("query-1", results)

	got, ok := cache.Get("query-1")
	require.True(t, ok)
	assert.Equal(t, results, got)
}

func TestResultCacheMiss(t *testing.T) {
	cache := NewResultCacheWithTTL(5 * time.Minute)
	_, ok := cache.Get("nonexistent")
	assert.False(t, ok)
}

func TestResultCacheExpiredReturnsMiss(t *testing.T) {
	cache := NewResultCacheWithTTL(50 * time.Millisecond)
	results := []SearchResult{
		{Title: "Expiring", URL: "https://expiring.com", Snippet: "soon gone"},
	}
	cache.Set("query-2", results)

	// Within TTL.
	got, ok := cache.Get("query-2")
	require.True(t, ok)
	assert.Equal(t, results, got)

	// Wait for expiry.
	time.Sleep(80 * time.Millisecond)

	_, ok = cache.Get("query-2")
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// WebSearchTool with different providers
// ---------------------------------------------------------------------------

func TestWebSearchToolWithMockProviderNoPanic(t *testing.T) {
	tool := NewWebSearchTool()
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "golang"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
	assert.True(t, res.Metadata["mock"].(bool)) //nolint:errcheck
}

func TestWebSearchToolWithFetchProviderNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<h2>Fetched Title</h2><a href="https://fetched.com/x">x</a><p>Fetched snippet</p>`)) //nolint:errcheck
	}))
	defer srv.Close()

	tool := NewWebSearchTool(WithSearchProvider(
		NewFetchSearchProvider(WithFetchSearchURL(srv.URL)),
	))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "test"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
	assert.Contains(t, res.Output, "https://fetched.com/x")
	assert.False(t, res.Metadata["mock"].(bool)) //nolint:errcheck
}

func TestWebSearchToolWithBraveProviderNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"Brave Title","url":"https://brave.com/y","description":"Brave snippet"}]}}`)) //nolint:errcheck
	}))
	defer srv.Close()

	tool := NewWebSearchTool(WithSearchProvider(
		NewBraveSearchProvider("key",
			WithBraveSearchURL(srv.URL),
			WithBraveSearchClient(srv.Client()),
		),
	))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "brave test"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
	assert.Contains(t, res.Output, "https://brave.com/y")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// itoa is a dependency-free int-to-string for test assertions.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
