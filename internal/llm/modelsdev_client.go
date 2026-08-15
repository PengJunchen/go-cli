package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// modelsDevAPIURL is the upstream endpoint serving the provider/model catalog.
const modelsDevAPIURL = "https://models.dev/api.json"

// modelsDevDefaultTTL is the default cache time-to-live.
const modelsDevDefaultTTL = 24 * time.Hour

// modelsDevAPIResponse is the top-level shape returned by models.dev/api.json:
// a map of provider ID → provider data (with nested models).
type modelsDevAPIResponse map[string]modelsDevProvider

type modelsDevProvider struct {
	Name   string                    `json:"name"`
	Npm    string                    `json:"npm"`
	API    string                    `json:"api,omitempty"`
	Env    []string                  `json:"env,omitempty"`
	Doc    string                    `json:"doc,omitempty"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Name             string              `json:"name"`
	Attachment       bool                `json:"attachment"`
	Reasoning        bool                `json:"reasoning"`
	ToolCall         bool                `json:"tool_call"`
	StructuredOutput bool                `json:"structured_output"`
	Temperature      bool                `json:"temperature"`
	Knowledge        string              `json:"knowledge,omitempty"`
	ReleaseDate      string              `json:"release_date,omitempty"`
	LastUpdated      string              `json:"last_updated,omitempty"`
	Cost             modelsDevCost       `json:"cost"`
	Limit            modelsDevLimit      `json:"limit"`
	Modalities       modelsDevModalities `json:"modalities"`
}

type modelsDevCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

type modelsDevLimit struct {
	Context int `json:"context"`
	Input   int `json:"input"`
	Output  int `json:"output"`
}

type modelsDevModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// modelsDevCacheFile is the on-disk cache envelope.
type modelsDevCacheFile struct {
	FetchedAt time.Time            `json:"fetched_at"`
	Data      modelsDevAPIResponse `json:"data"`
}

// ModelsDevRegistry is a ModelRegistry backed by the models.dev API. It caches
// the upstream JSON to disk with a configurable TTL and falls back to a stale
// cache when the upstream is unreachable.
type ModelsDevRegistry struct {
	mu        sync.RWMutex
	data      modelsDevAPIResponse
	fetchedAt time.Time
	loaded    bool
	cachePath string
	ttl       time.Duration
	url       string
	client    *http.Client
}

// Compile-time assertion that ModelsDevRegistry satisfies ModelRegistry.
var _ ModelRegistry = (*ModelsDevRegistry)(nil)

// NewModelsDevRegistry creates a ModelsDevRegistry. When cachePath is empty it
// defaults to ~/.go-cli/cache/models.dev.json. When ttl is zero it defaults to
// 24 hours.
func NewModelsDevRegistry(cachePath string, ttl time.Duration) *ModelsDevRegistry {
	if cachePath == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			cachePath = filepath.Join(home, ".go-cli", "cache", "models.dev.json")
		}
	}
	if ttl <= 0 {
		ttl = modelsDevDefaultTTL
	}
	return &ModelsDevRegistry{
		cachePath: cachePath,
		ttl:       ttl,
		url:       modelsDevAPIURL,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Lookup returns enriched ModelInfo for the given provider and model ID. When
// the registry has not yet been loaded, it lazily loads from cache or fetches
// from the upstream.
func (r *ModelsDevRegistry) Lookup(ctx context.Context, provider, model string) (ModelInfo, bool) {
	_ = r.ensureLoaded(ctx)

	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.data[provider]
	if !ok {
		return ModelInfo{}, false
	}
	m, ok := p.Models[model]
	if !ok {
		return ModelInfo{}, false
	}
	return r.toModelInfo(provider, m), true
}

// Providers returns metadata for every provider known to the registry. When the
// registry has not yet been loaded, it lazily loads from cache or fetches from
// the upstream.
func (r *ModelsDevRegistry) Providers() []ProviderMetadata {
	_ = r.ensureLoaded(context.Background())

	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ProviderMetadata, 0, len(r.data))
	for id, p := range r.data {
		out = append(out, ProviderMetadata{
			ID:      id,
			Name:    p.Name,
			APIBase: p.API,
			Doc:     p.Doc,
			Env:     p.Env,
		})
	}
	return out
}

// ModelsForProvider returns the enriched ModelInfo entries for every model
// exposed by the given provider. It returns nil when the provider is unknown.
func (r *ModelsDevRegistry) ModelsForProvider(providerID string) []ModelInfo {
	_ = r.ensureLoaded(context.Background())

	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.data[providerID]
	if !ok {
		return nil
	}
	out := make([]ModelInfo, 0, len(p.Models))
	for _, m := range p.Models {
		out = append(out, r.toModelInfo(providerID, m))
	}
	return out
}

// Refresh fetches the latest data from the upstream API. On success it updates
// the in-memory data and writes the cache file. When the fetch fails it falls
// back to a stale cache (if one exists) so the registry remains usable offline.
func (r *ModelsDevRegistry) Refresh(ctx context.Context) error {
	raw, err := r.fetch(ctx)
	if err != nil {
		// Fetch failed: fall back to cache (stale ok).
		r.mu.Lock()
		if !r.loaded {
			r.mu.Unlock()
			if r.loadCache() {
				slog.Warn("modelsdev_refresh_fetch_failed_using_cache", "fetch_err", err)
				return nil
			}
			return err
		}
		r.mu.Unlock()
		slog.Warn("modelsdev_refresh_fetch_failed_using_stale_data", "err", err)
		return nil
	}

	var resp modelsDevAPIResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		// Parse failed: fall back to cache.
		r.mu.Lock()
		loaded := r.loaded
		r.mu.Unlock()
		if !loaded {
			if r.loadCache() {
				slog.Warn("modelsdev_refresh_parse_failed_using_cache", "parse_err", err)
				return nil
			}
		} else {
			slog.Warn("modelsdev_refresh_parse_failed_using_stale_data", "err", err)
			return nil
		}
		return fmt.Errorf("modelsdev: parse response: %w", err)
	}

	now := time.Now()
	r.mu.Lock()
	r.data = resp
	r.fetchedAt = now
	r.loaded = true
	r.mu.Unlock()

	if wErr := r.writeCache(resp, now); wErr != nil {
		slog.Warn("modelsdev_cache_write_failed", "err", wErr)
	}
	slog.Info("modelsdev_refresh_ok", "providers", len(resp))
	return nil
}

// ensureLoaded populates the registry from cache or upstream on first use. It
// is a no-op when the registry is already loaded.
func (r *ModelsDevRegistry) ensureLoaded(ctx context.Context) error {
	r.mu.RLock()
	loaded := r.loaded
	r.mu.RUnlock()
	if loaded {
		return nil
	}

	cacheLoaded := r.loadCache()
	if cacheLoaded && r.cacheFresh() {
		return nil
	}

	if err := r.Refresh(ctx); err != nil {
		// Refresh failed entirely; if we have a stale cache we still return nil
		// so callers can use the stale data.
		if cacheLoaded {
			return nil
		}
		return err
	}
	return nil
}

// fetch performs the HTTP GET against the upstream API.
func (r *ModelsDevRegistry) fetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("modelsdev: upstream returned %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 50<<20)) // 50 MB safety cap
}

// loadCache reads and parses the cache file. It acquires the write lock and
// updates r.data/r.fetchedAt/r.loaded. It returns true when the cache file
// existed and was parsed successfully.
func (r *ModelsDevRegistry) loadCache() bool {
	if r.cachePath == "" {
		return false
	}
	raw, err := os.ReadFile(r.cachePath)
	if err != nil {
		return false
	}
	var cf modelsDevCacheFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		slog.Warn("modelsdev_cache_parse_failed", "err", err)
		return false
	}
	r.mu.Lock()
	r.data = cf.Data
	r.fetchedAt = cf.FetchedAt
	r.loaded = true
	r.mu.Unlock()
	return true
}

// cacheFresh reports whether the loaded cache data is within the TTL.
func (r *ModelsDevRegistry) cacheFresh() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.fetchedAt.IsZero() {
		return false
	}
	return time.Since(r.fetchedAt) < r.ttl
}

// writeCache writes the raw API response plus a fetched_at timestamp to the
// cache file, creating parent directories as needed.
func (r *ModelsDevRegistry) writeCache(data modelsDevAPIResponse, fetchedAt time.Time) error {
	if r.cachePath == "" {
		return nil
	}
	if dir := filepath.Dir(r.cachePath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	cf := modelsDevCacheFile{FetchedAt: fetchedAt, Data: data}
	enc, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.cachePath, enc, 0o644)
}

// toModelInfo converts a models.dev model entry into a ModelInfo.
func (r *ModelsDevRegistry) toModelInfo(providerID string, m modelsDevModel) ModelInfo {
	info := ModelInfo{
		Name:             m.Name,
		ContextWindow:    m.Limit.Context,
		MaxOutputTokens:  m.Limit.Output,
		InputPrice:       m.Cost.Input,
		OutputPrice:      m.Cost.Output,
		Modality:         modalityString(m.Modalities),
		Reasoning:        m.Reasoning,
		ToolCall:         m.ToolCall,
		StructuredOutput: m.StructuredOutput,
		CacheReadPrice:   m.Cost.CacheRead,
		CacheWritePrice:  m.Cost.CacheWrite,
		InputTokenLimit:  m.Limit.Input,
		Knowledge:        m.Knowledge,
		ReleaseDate:      m.ReleaseDate,
	}
	if p, ok := r.data[providerID]; ok {
		info.APIBase = p.API
	}
	return info
}

// modalityString joins input and output modalities into a compact signature
// such as "text+image→text".
func modalityString(m modelsDevModalities) string {
	return strings.Join(m.Input, "+") + "→" + strings.Join(m.Output, "+")
}
