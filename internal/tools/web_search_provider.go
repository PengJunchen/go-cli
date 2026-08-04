package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// defaultFetchSearchTimeout is the default HTTP timeout for fetch-based search.
	defaultFetchSearchTimeout = 10 * time.Second

	// defaultFetchSearchMaxResults is the default maximum number of results.
	defaultFetchSearchMaxResults = 10

	// defaultFetchSearchBaseURL is the default DuckDuckGo HTML endpoint.
	defaultFetchSearchBaseURL = "https://html.duckduckgo.com/html/"

	// defaultBraveSearchTimeout is the default HTTP timeout for Brave search.
	defaultBraveSearchTimeout = 10 * time.Second

	// defaultBraveSearchMaxResults is the default maximum number of Brave results.
	defaultBraveSearchMaxResults = 10

	// defaultBraveSearchBaseURL is the default Brave Search API endpoint.
	defaultBraveSearchBaseURL = "https://api.search.brave.com/res/v1/web/search"

	// defaultCacheTTL is the default TTL for cached search results.
	defaultCacheTTL = 5 * time.Minute
)

// SearchResult represents a single search result item.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// SearchOptions configures a search request.
type SearchOptions struct {
	MaxResults int
	Timeout    time.Duration
}

// WebSearchProvider abstracts search backends.
type WebSearchProvider interface {
	Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error)
}

// ---------------------------------------------------------------------------
// MockSearchProvider
// ---------------------------------------------------------------------------

// MockSearchProvider returns deterministic preset data (backward compatible).
type MockSearchProvider struct{}

var _ WebSearchProvider = (*MockSearchProvider)(nil)

// NewMockSearchProvider returns a MockSearchProvider.
func NewMockSearchProvider() *MockSearchProvider {
	return &MockSearchProvider{}
}

// Name returns the provider name.
func (MockSearchProvider) Name() string { return "mock" }

// Search returns deterministic preset data derived from the query.
func (MockSearchProvider) Search(_ context.Context, query string, _ SearchOptions) ([]SearchResult, error) {
	const numResults = 3
	results := make([]SearchResult, 0, numResults)
	for i := 1; i <= numResults; i++ {
		results = append(results, SearchResult{
			Title:   fmt.Sprintf("Result %d", i),
			URL:     fmt.Sprintf("https://example.com/result%d", i),
			Snippet: fmt.Sprintf("Snippet %d for %s", i, query),
		})
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// FetchSearchProvider
// ---------------------------------------------------------------------------

// FetchSearchProvider performs real HTTP fetch + HTML parsing.
type FetchSearchProvider struct {
	httpClient *http.Client
	timeout    time.Duration
	baseURL    string
}

var _ WebSearchProvider = (*FetchSearchProvider)(nil)

// FetchSearchProviderOption configures a FetchSearchProvider.
type FetchSearchProviderOption func(*FetchSearchProvider)

// WithFetchSearchURL sets the search endpoint URL.
func WithFetchSearchURL(u string) FetchSearchProviderOption {
	return func(p *FetchSearchProvider) { p.baseURL = u }
}

// WithFetchSearchTimeout sets the HTTP request timeout.
func WithFetchSearchTimeout(d time.Duration) FetchSearchProviderOption {
	return func(p *FetchSearchProvider) { p.timeout = d }
}

// WithFetchSearchClient sets the HTTP client.
func WithFetchSearchClient(c *http.Client) FetchSearchProviderOption {
	return func(p *FetchSearchProvider) { p.httpClient = c }
}

// NewFetchSearchProvider returns a FetchSearchProvider with defaults.
func NewFetchSearchProvider(opts ...FetchSearchProviderOption) *FetchSearchProvider {
	p := &FetchSearchProvider{
		httpClient: &http.Client{},
		timeout:    defaultFetchSearchTimeout,
		baseURL:    defaultFetchSearchBaseURL,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name returns the provider name.
func (p *FetchSearchProvider) Name() string { return "fetch" }

// Search fetches results from the configured endpoint and parses the HTML.
func (p *FetchSearchProvider) Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = p.timeout
	}
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = defaultFetchSearchMaxResults
	}

	searchURL := buildQueryURL(p.baseURL, query)

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch_search: %w", err)
	}
	req.Header.Set("User-Agent", "go-cli/1.0")

	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch_search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch_search: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("fetch_search: %w", err)
	}

	return parseSearchHTML(string(body), maxResults), nil
}

// ---------------------------------------------------------------------------
// BraveSearchProvider
// ---------------------------------------------------------------------------

// BraveSearchProvider performs search via the Brave Search API (JSON).
type BraveSearchProvider struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
}

var _ WebSearchProvider = (*BraveSearchProvider)(nil)

// BraveSearchProviderOption configures a BraveSearchProvider.
type BraveSearchProviderOption func(*BraveSearchProvider)

// WithBraveSearchURL sets the Brave Search API endpoint.
func WithBraveSearchURL(u string) BraveSearchProviderOption {
	return func(p *BraveSearchProvider) { p.baseURL = u }
}

// WithBraveSearchClient sets the HTTP client.
func WithBraveSearchClient(c *http.Client) BraveSearchProviderOption {
	return func(p *BraveSearchProvider) { p.httpClient = c }
}

// NewBraveSearchProvider returns a BraveSearchProvider with the given API key.
func NewBraveSearchProvider(apiKey string, opts ...BraveSearchProviderOption) *BraveSearchProvider {
	p := &BraveSearchProvider{
		apiKey:     apiKey,
		httpClient: &http.Client{},
		baseURL:    defaultBraveSearchBaseURL,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name returns the provider name.
func (p *BraveSearchProvider) Name() string { return "brave" }

// Search queries the Brave Search API and returns parsed results.
func (p *BraveSearchProvider) Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	if strings.TrimSpace(p.apiKey) == "" {
		return nil, errors.New("brave_search: api key is required")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultBraveSearchTimeout
	}
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = defaultBraveSearchMaxResults
	}

	searchURL := buildQueryURL(p.baseURL, query)
	if u, err := url.Parse(searchURL); err == nil {
		q := u.Query()
		q.Set("count", fmt.Sprintf("%d", maxResults))
		u.RawQuery = q.Encode()
		searchURL = u.String()
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("brave_search: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", "Bearer "+p.apiKey)

	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave_search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave_search: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("brave_search: %w", err)
	}

	return parseBraveJSON(body, maxResults)
}

// braveResponse models the relevant portion of the Brave Search API response.
type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

// parseBraveJSON extracts search results from a Brave API JSON response.
func parseBraveJSON(data []byte, max int) ([]SearchResult, error) {
	var resp braveResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("brave_search: %w", err)
	}
	results := make([]SearchResult, 0, len(resp.Web.Results))
	for _, r := range resp.Web.Results {
		if len(results) >= max {
			break
		}
		results = append(results, SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Description,
		})
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// RateLimiter
// ---------------------------------------------------------------------------

// RateLimiter enforces a minimum interval between successive Wait calls.
type RateLimiter struct {
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
	stop     chan struct{}
}

// NewRateLimiterWithInterval returns a RateLimiter that enforces the given
// minimum interval between calls.
func NewRateLimiterWithInterval(d time.Duration) *RateLimiter {
	return &RateLimiter{
		interval: d,
		stop:     make(chan struct{}),
	}
}

// Wait blocks until at least interval has elapsed since the previous Wait.
// The first call returns immediately.
func (rl *RateLimiter) Wait() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if !rl.last.IsZero() {
		elapsed := time.Since(rl.last)
		if remaining := rl.interval - elapsed; remaining > 0 {
			select {
			case <-time.After(remaining):
			case <-rl.stop:
			}
		}
	}
	rl.last = time.Now()
}

// Stop releases resources and unblocks any pending Wait.
func (rl *RateLimiter) Stop() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	select {
	case <-rl.stop:
	default:
		close(rl.stop)
	}
}

// ---------------------------------------------------------------------------
// ResultCache
// ---------------------------------------------------------------------------

// cacheEntry holds cached search results with an expiry time.
type cacheEntry struct {
	results []SearchResult
	expires time.Time
}

// ResultCache is a simple TTL cache for search results.
type ResultCache struct {
	ttl  time.Duration
	mu   sync.RWMutex
	data map[string]cacheEntry
}

// NewResultCacheWithTTL returns a ResultCache with the given TTL.
func NewResultCacheWithTTL(ttl time.Duration) *ResultCache {
	return &ResultCache{
		ttl:  ttl,
		data: make(map[string]cacheEntry),
	}
}

// Set stores results for the given key with the configured TTL.
func (c *ResultCache) Set(key string, results []SearchResult) {
	c.mu.Lock()
	c.data[key] = cacheEntry{
		results: results,
		expires: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
}

// Get returns the cached results for the key if present and not expired.
func (c *ResultCache) Get(key string) ([]SearchResult, bool) {
	c.mu.RLock()
	entry, ok := c.data[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.results, true
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

var (
	h2Re     = regexp.MustCompile(`(?s)<h2[^>]*>(.*?)</h2>`)
	anchorRe = regexp.MustCompile(`(?s)<a\s+[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	paraRe   = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
	tagRe    = regexp.MustCompile(`<[^>]*>`)
)

// buildQueryURL appends the query parameter to the base URL.
func buildQueryURL(base, query string) string {
	v := url.Values{}
	v.Set("q", query)
	if strings.Contains(base, "?") {
		return base + "&" + v.Encode()
	}
	return base + "?" + v.Encode()
}

// parseSearchHTML extracts search results from an HTML page. It looks for
// <h2> titles, <a href> URLs, and <p> snippets and zips them by index.
func parseSearchHTML(htmlStr string, max int) []SearchResult {
	titles := h2Re.FindAllStringSubmatch(htmlStr, max)
	links := anchorRe.FindAllStringSubmatch(htmlStr, max)
	snippets := paraRe.FindAllStringSubmatch(htmlStr, max)

	n := len(titles)
	if len(links) < n {
		n = len(links)
	}
	if n == 0 {
		return []SearchResult{}
	}

	results := make([]SearchResult, 0, n)
	for i := 0; i < n; i++ {
		r := SearchResult{
			Title: cleanText(titles[i][1]),
			URL:   cleanURL(links[i][1]),
		}
		if i < len(snippets) {
			r.Snippet = cleanText(snippets[i][1])
		}
		results = append(results, r)
	}
	return results
}

// cleanText strips HTML tags and unescapes HTML entities.
func cleanText(s string) string {
	return strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(s, "")))
}

// cleanURL unwraps DuckDuckGo redirect URLs to the destination URL.
func cleanURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Host == "duckduckgo.com" && u.Path == "/l/" {
		if dest := u.Query().Get("uddg"); dest != "" {
			return dest
		}
	}
	return raw
}
