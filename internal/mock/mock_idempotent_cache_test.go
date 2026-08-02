//go:build mock

package mock

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Constructor / Name
// ---------------------------------------------------------------------------

func TestNewMockIdempotentCacheReturnsNonNil(t *testing.T) {
	c := NewMockIdempotentCache(0)
	require.NotNil(t, c)
	assert.Equal(t, "mock-idempotent-cache", c.Name())
}

func TestMockIdempotentCacheWithName(t *testing.T) {
	c := NewMockIdempotentCache(0).WithName("custom-cache")
	assert.Equal(t, "custom-cache", c.Name())
}

// ---------------------------------------------------------------------------
// Get / Set basic behavior
// ---------------------------------------------------------------------------

func TestMockIdempotentCacheGetMissOnEmpty(t *testing.T) {
	c := NewMockIdempotentCache(0)
	val, ok := c.Get(context.Background(), "nokey")
	assert.False(t, ok, "Get on empty cache should miss")
	assert.Nil(t, val)
}

func TestMockIdempotentCacheSetAndGet(t *testing.T) {
	c := NewMockIdempotentCache(0)
	require.NoError(t, c.Set(context.Background(), "k1", "v1"))

	val, ok := c.Get(context.Background(), "k1")
	assert.True(t, ok)
	assert.Equal(t, "v1", val)
}

func TestMockIdempotentCacheSetOverwrite(t *testing.T) {
	c := NewMockIdempotentCache(0)
	require.NoError(t, c.Set(context.Background(), "k", "first"))
	require.NoError(t, c.Set(context.Background(), "k", "second"))

	val, ok := c.Get(context.Background(), "k")
	assert.True(t, ok)
	assert.Equal(t, "second", val, "last Set should win")
}

func TestMockIdempotentCacheSetDifferentTypes(t *testing.T) {
	c := NewMockIdempotentCache(0)

	require.NoError(t, c.Set(context.Background(), "int", 42))
	require.NoError(t, c.Set(context.Background(), "float", 3.14))
	require.NoError(t, c.Set(context.Background(), "bool", true))
	require.NoError(t, c.Set(context.Background(), "nil", nil))

	val, ok := c.Get(context.Background(), "int")
	assert.True(t, ok)
	assert.Equal(t, 42, val)

	val, ok = c.Get(context.Background(), "float")
	assert.True(t, ok)
	assert.Equal(t, 3.14, val)

	val, ok = c.Get(context.Background(), "bool")
	assert.True(t, ok)
	assert.Equal(t, true, val)

	val, ok = c.Get(context.Background(), "nil")
	assert.True(t, ok)
	assert.Nil(t, val)
}

// ---------------------------------------------------------------------------
// FIFO eviction
// ---------------------------------------------------------------------------

func TestMockIdempotentCacheFIFOEviction(t *testing.T) {
	c := NewMockIdempotentCache(2)

	require.NoError(t, c.Set(context.Background(), "a", 1))
	require.NoError(t, c.Set(context.Background(), "b", 2))

	// At capacity; adding "c" should evict "a".
	require.NoError(t, c.Set(context.Background(), "c", 3))

	_, okA := c.Get(context.Background(), "a")
	assert.False(t, okA, "oldest key 'a' should be evicted")

	_, okB := c.Get(context.Background(), "b")
	assert.True(t, okB, "key 'b' should still be present")

	_, okC := c.Get(context.Background(), "c")
	assert.True(t, okC, "key 'c' should be present")
}

func TestMockIdempotentCacheNoEvictionWhenMaxSizeZero(t *testing.T) {
	c := NewMockIdempotentCache(0) // unlimited

	for i := 0; i < 100; i++ {
		require.NoError(t, c.Set(context.Background(), "key", i))
	}
	// All overwrites of the same key; no eviction needed.
	val, ok := c.Get(context.Background(), "key")
	assert.True(t, ok)
	assert.Equal(t, 99, val)
}

func TestMockIdempotentCacheFIFOEvictionWithOverwrite(t *testing.T) {
	c := NewMockIdempotentCache(2)

	require.NoError(t, c.Set(context.Background(), "a", 1))
	require.NoError(t, c.Set(context.Background(), "b", 2))
	// Overwrite "a" — does not change insertion order, so "a" is still oldest.
	require.NoError(t, c.Set(context.Background(), "a", 10))

	require.NoError(t, c.Set(context.Background(), "c", 3))

	_, okA := c.Get(context.Background(), "a")
	assert.False(t, okA, "overwriting existing key should not reset its FIFO position")

	_, okB := c.Get(context.Background(), "b")
	assert.True(t, okB)

	_, okC := c.Get(context.Background(), "c")
	assert.True(t, okC)
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestMockIdempotentCacheDeleteExisting(t *testing.T) {
	c := NewMockIdempotentCache(0)
	require.NoError(t, c.Set(context.Background(), "x", "val"))

	require.NoError(t, c.Delete(context.Background(), "x"))

	_, ok := c.Get(context.Background(), "x")
	assert.False(t, ok, "deleted key should not be found")
}

func TestMockIdempotentCacheDeleteNonExistent(t *testing.T) {
	c := NewMockIdempotentCache(0)
	err := c.Delete(context.Background(), "nokey")
	require.NoError(t, err, "Delete on missing key should not error")
}

func TestMockIdempotentCacheDeleteFreesCapacity(t *testing.T) {
	c := NewMockIdempotentCache(2)

	require.NoError(t, c.Set(context.Background(), "a", 1))
	require.NoError(t, c.Set(context.Background(), "b", 2))
	require.NoError(t, c.Delete(context.Background(), "a"))

	// "a" removed; now there is room for "c" without evicting "b".
	require.NoError(t, c.Set(context.Background(), "c", 3))

	_, okB := c.Get(context.Background(), "b")
	assert.True(t, okB, "b should still be present after a was deleted and c was added")

	_, okC := c.Get(context.Background(), "c")
	assert.True(t, okC)
}

// ---------------------------------------------------------------------------
// ProgramGet (canned results)
// ---------------------------------------------------------------------------

func TestMockIdempotentCacheProgramGet(t *testing.T) {
	c := NewMockIdempotentCache(0)
	c.ProgramGet("z", "canned-value", true)

	val, ok := c.Get(context.Background(), "z")
	assert.True(t, ok)
	assert.Equal(t, "canned-value", val)

	// Second call falls through to real values (miss since "z" was never Set).
	val, ok = c.Get(context.Background(), "z")
	assert.False(t, ok, "programmed result should be consumed after one use")
	assert.Nil(t, val)
}

func TestMockIdempotentCacheProgramGetMiss(t *testing.T) {
	c := NewMockIdempotentCache(0)
	c.ProgramGet("z", nil, false) // programmed miss

	val, ok := c.Get(context.Background(), "z")
	assert.False(t, ok)
	assert.Nil(t, val)
}

func TestMockIdempotentCacheProgramGetMultipleResults(t *testing.T) {
	c := NewMockIdempotentCache(0)
	c.ProgramGet("k", "first", true)
	c.ProgramGet("k", "second", true)

	val1, ok1 := c.Get(context.Background(), "k")
	assert.True(t, ok1)
	assert.Equal(t, "first", val1)

	val2, ok2 := c.Get(context.Background(), "k")
	assert.True(t, ok2)
	assert.Equal(t, "second", val2)

	// Third call falls through to real store.
	_, ok3 := c.Get(context.Background(), "k")
	assert.False(t, ok3)
}

// ---------------------------------------------------------------------------
// Call counting
// ---------------------------------------------------------------------------

func TestMockIdempotentCacheGetCallsCount(t *testing.T) {
	c := NewMockIdempotentCache(0)
	assert.Equal(t, 0, c.GetCallsCount("k"))

	_, _ = c.Get(context.Background(), "k")
	assert.Equal(t, 1, c.GetCallsCount("k"))

	_, _ = c.Get(context.Background(), "k")
	_, _ = c.Get(context.Background(), "other")
	assert.Equal(t, 2, c.GetCallsCount("k"))
	assert.Equal(t, 1, c.GetCallsCount("other"))
	assert.Equal(t, 0, c.GetCallsCount("never"))
}

func TestMockIdempotentCacheSetCallsCount(t *testing.T) {
	c := NewMockIdempotentCache(0)
	assert.Equal(t, 0, c.SetCallsCount("k"))

	_ = c.Set(context.Background(), "k", 1)
	assert.Equal(t, 1, c.SetCallsCount("k"))

	_ = c.Set(context.Background(), "k", 2)
	assert.Equal(t, 2, c.SetCallsCount("k"))
}

func TestMockIdempotentCacheDeleteCallsCount(t *testing.T) {
	c := NewMockIdempotentCache(0)
	assert.Equal(t, 0, c.DeleteCallsCount("k"))

	_ = c.Delete(context.Background(), "k")
	assert.Equal(t, 1, c.DeleteCallsCount("k"))

	_ = c.Delete(context.Background(), "k")
	assert.Equal(t, 2, c.DeleteCallsCount("k"))
}

func TestMockIdempotentCacheTotalSetCalls(t *testing.T) {
	c := NewMockIdempotentCache(0)
	assert.Equal(t, 0, c.TotalSetCalls())

	_ = c.Set(context.Background(), "a", 1)
	_ = c.Set(context.Background(), "b", 2)
	_ = c.Set(context.Background(), "a", 3) // overwrite counts
	assert.Equal(t, 3, c.TotalSetCalls())
}

// ---------------------------------------------------------------------------
// Concurrent access
// ---------------------------------------------------------------------------

func TestMockIdempotentCacheConcurrentAccess(t *testing.T) {
	c := NewMockIdempotentCache(10)
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	// Concurrent Set
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			_ = c.Set(context.Background(), "key", i)
		}(i)
	}
	// Concurrent Get
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = c.Get(context.Background(), "key")
		}()
	}
	// Concurrent Delete
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = c.Delete(context.Background(), "key")
		}()
	}

	wg.Wait()

	// Verify call counts are consistent (total = goroutines for each op).
	assert.Equal(t, goroutines, c.GetCallsCount("key"))
	assert.Equal(t, goroutines, c.SetCallsCount("key"))
	assert.Equal(t, goroutines, c.DeleteCallsCount("key"))
}

func TestMockIdempotentCacheConcurrentProgramGet(t *testing.T) {
	c := NewMockIdempotentCache(0)
	c.ProgramGet("z", "programmed", true)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	hits := int64(0)
	misses := int64(0)
	var mu sync.Mutex

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, ok := c.Get(context.Background(), "z")
			mu.Lock()
			if ok {
				hits++
			} else {
				misses++
			}
			mu.Unlock()
		}()
	}

	wg.Wait()
	// Exactly one goroutine should get the programmed result; the rest get a
	// miss (since "z" was never Set).
	assert.Equal(t, int64(1), hits, "exactly one Get should consume the programmed result")
	assert.Equal(t, int64(goroutines-1), misses, "remaining Gets should miss")
}
