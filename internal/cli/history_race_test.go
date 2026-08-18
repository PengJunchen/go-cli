package cli

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHistoryStoreConcurrentAppendRead verifies that concurrent Add and List
// calls do not race or produce corrupted slices. Run with -race to detect data
// races.
func TestHistoryStoreConcurrentAppendRead(t *testing.T) {
	t.Parallel()
	hs := NewHistoryStore(10000, "")

	const writers = 8
	const readers = 8
	const ops = 500

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	// Writers: concurrently append entries.
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				hs.Add("entry")
			}
		}()
	}

	// Readers: concurrently read entries.
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				_ = hs.List()
			}
		}()
	}

	wg.Wait()

	// All writers appended "entry" but consecutive duplicates are skipped, so
	// the store should contain exactly one entry.
	entries := hs.List()
	assert.Len(t, entries, 1)
	assert.Equal(t, "entry", entries[0])
}

// TestHistoryStoreConcurrentAppendClear verifies that concurrent Add and Load
// (which replaces entries) do not race. Run with -race to detect data races.
func TestHistoryStoreConcurrentAppendClear(t *testing.T) {
	t.Parallel()
	hs := NewHistoryStore(10000, "")

	const goroutines = 8
	const ops = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				// Alternate between appending and clearing (Load with no file
				// is a no-op that still acquires the write lock).
				if id%2 == 0 {
					hs.Add("concurrent-entry")
				} else {
					_ = hs.Load()
				}
				_ = hs.List()
			}
		}(g)
	}

	wg.Wait()

	// Should not panic or race; the final state is non-deterministic but must
	// be a valid slice.
	entries := hs.List()
	for _, e := range entries {
		assert.Equal(t, "concurrent-entry", e)
	}
}
