//go:build mock

package mock

import (
	"context"
	"sync"

	"github.com/pengjunchen/go-cli/internal/production"
)

// MockIdempotentCache is a test-only IdempotentCache that records every
// Get/Set/Delete invocation and allows callers to drive hit/miss behavior. It
// also supports assertable FIFO eviction so cache-capacity behavior can be
// verified without a production implementation.
type MockIdempotentCache struct {
	mu      sync.Mutex
	maxSize int
	values  map[string]any
	order   []string

	gets    []string
	sets    []string
	deletes []string

	// GetCalls, when set, returns the recorded result for the key on each Get.
	// Each call consumes one entry; a missing/empty entry behaves as a miss.
	GetCalls map[string][]mockCacheResult

	name string
}

type mockCacheResult struct {
	value any
	ok    bool
}

// Compile-time assertion that the mock satisfies the cache contract.
var _ production.IdempotentCache = (*MockIdempotentCache)(nil)

// NewMockIdempotentCache creates an empty mock cache. maxSize <= 0 disables
// FIFO eviction, so the cache stores every key.
func NewMockIdempotentCache(maxSize int) *MockIdempotentCache {
	return &MockIdempotentCache{
		maxSize:  maxSize,
		values:   make(map[string]any),
		GetCalls: make(map[string][]mockCacheResult),
		name:     "mock-idempotent-cache",
	}
}

// WithName overrides the identifier returned by Name.
func (m *MockIdempotentCache) WithName(name string) *MockIdempotentCache {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.name = name
	return m
}

// Get records the call and returns the programmed result, else a miss.
func (m *MockIdempotentCache) Get(_ context.Context, key string) (any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets = append(m.gets, key)

	if calls := m.GetCalls[key]; len(calls) > 0 {
		res := calls[0]
		m.GetCalls[key] = calls[1:]
		return res.value, res.ok
	}

	value, ok := m.values[key]
	return value, ok
}

// ProgramGet queues a canned result for the next Get(key) call.
func (m *MockIdempotentCache) ProgramGet(key string, value any, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetCalls[key] = append(m.GetCalls[key], mockCacheResult{value: value, ok: ok})
}

// Set records the call and stores the value, evicting the oldest key when at
// capacity (maxSize > 0).
func (m *MockIdempotentCache) Set(_ context.Context, key string, value any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sets = append(m.sets, key)

	if _, exists := m.values[key]; !exists {
		if m.maxSize > 0 && len(m.order) >= m.maxSize {
			oldest := m.order[0]
			m.order = m.order[1:]
			delete(m.values, oldest)
		}
		m.order = append(m.order, key)
	}
	m.values[key] = value
	return nil
}

// Delete records the call and removes the key.
func (m *MockIdempotentCache) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes = append(m.deletes, key)

	if _, ok := m.values[key]; !ok {
		return nil
	}
	delete(m.values, key)
	for i, k := range m.order {
		if k == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return nil
}

// GetCallsCount returns the number of Get invocations for key (0 if never).
func (m *MockIdempotentCache) GetCallsCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, k := range m.gets {
		if k == key {
			n++
		}
	}
	return n
}

// SetCallsCount returns the number of Set invocations for key.
func (m *MockIdempotentCache) SetCallsCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, k := range m.sets {
		if k == key {
			n++
		}
	}
	return n
}

// DeleteCallsCount returns the number of Delete invocations for key.
func (m *MockIdempotentCache) DeleteCallsCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, k := range m.deletes {
		if k == key {
			n++
		}
	}
	return n
}

// TotalSetCalls returns the total number of Set invocations.
func (m *MockIdempotentCache) TotalSetCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sets)
}

// Name returns the cache identifier.
func (m *MockIdempotentCache) Name() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.name
}
