package production

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// IdempotentCache is a bounded cache that tracks which operations have already
// executed, so a repeated request with the same key returns the stored result
// without re-executing. A cache miss implies the caller should run the
// operation and populate the result with Set.
type IdempotentCache interface {
	// Get returns the cached value for key and whether it was present.
	Get(ctx context.Context, key string) (any, bool)
	// Set stores value under key, evicting the oldest entry when the cache is
	// at capacity.
	Set(ctx context.Context, key string, value any) error
	// Delete removes key from the cache. It is idempotent.
	Delete(ctx context.Context, key string) error
	// Name returns the cache identifier.
	Name() string
}

// FIFOIdempotentCache is an insertion-ordered idempotency cache that evicts
// the oldest entry once the number of keys reaches maxSize. Lookups and
// mutations are guarded by a single read-write lock.
type FIFOIdempotentCache struct {
	mu      sync.RWMutex
	maxSize int
	name    string
	values  map[string]any
	order   []string
}

// Compile-time assertion that FIFOIdempotentCache satisfies IdempotentCache.
var _ IdempotentCache = (*FIFOIdempotentCache)(nil)

// NewFIFOIdempotentCache returns a FIFOIdempotentCache with the given maxSize.
// A non-positive maxSize falls back to a safe default so the cache never grows
// unbounded.
func NewFIFOIdempotentCache(maxSize int, opts ...Option) *FIFOIdempotentCache {
	if maxSize <= 0 {
		maxSize = 1024
	}
	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "fifo-idempotent-cache"
	}
	return &FIFOIdempotentCache{
		maxSize: maxSize,
		name:    name,
		values:  make(map[string]any),
	}
}

// Get returns the cached value for key and whether it was present. It emits an
// idempotent.hit span carrying the key and whether the lookup hit.
func (c *FIFOIdempotentCache) Get(ctx context.Context, key string) (any, bool) {
	span, ctx := tracing.SpanFromContext(ctx, "idempotent.hit", tracing.SpanKindInternal)

	c.mu.RLock()
	value, ok := c.values[key]
	c.mu.RUnlock()

	span.SetAttributes(
		tracing.Attribute{Key: "cache_key", Value: key},
		tracing.Attribute{Key: "hit", Value: ok},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.DebugContext(ctx, "idempotent.hit",
		"cache_key", key,
		"hit", ok,
	)
	span.End()
	return value, ok
}

// Set stores value under key, evicting the oldest entry when at capacity.
func (c *FIFOIdempotentCache) Set(_ context.Context, key string, value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.values[key]; !exists {
		if len(c.order) >= c.maxSize {
			oldest := c.order[0]
			c.order[0] = "" // drop reference so the evicted key can be GC'd
			c.order = c.order[1:]
			delete(c.values, oldest)
		}
		c.order = append(c.order, key)
	}
	c.values[key] = value
	return nil
}

// Delete removes key from the cache. It is idempotent.
func (c *FIFOIdempotentCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.values[key]; !ok {
		return nil
	}
	delete(c.values, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	return nil
}

// Name returns the cache identifier.
func (c *FIFOIdempotentCache) Name() string { return c.name }
