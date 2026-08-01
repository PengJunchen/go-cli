package approval

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInMemoryStoreGetMissingReturnsNotFound(t *testing.T) {
	s := NewInMemoryApprovalStore()
	c, ok, err := s.Get(context.Background(), "nope")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, Allow, c)
}

func TestInMemoryStoreSetGetDelete(t *testing.T) {
	s := NewInMemoryApprovalStore()
	require.NoError(t, s.Set(context.Background(), "read_file:abc", Allow))
	require.NoError(t, s.Set(context.Background(), "del:abc", Deny))

	c, ok, err := s.Get(context.Background(), "read_file:abc")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Allow, c)

	c, ok, err = s.Get(context.Background(), "del:abc")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Deny, c)

	require.NoError(t, s.Delete(context.Background(), "read_file:abc"))
	c, ok, err = s.Get(context.Background(), "read_file:abc")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, Allow, c)
}

func TestInMemoryStoreIsConcurrencySafe(t *testing.T) {
	s := NewInMemoryApprovalStore()
	const workers = 16
	const iterations = 100

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var errs []error
	record := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := "read_file:key"
			for i := 0; i < iterations; i++ {
				err := s.Set(context.Background(), key, Allow)
				record(err)
				_, _, err = s.Get(context.Background(), key)
				record(err)
				err = s.Delete(context.Background(), key)
				record(err)
				err = s.Set(context.Background(), "w"+key, Deny)
				record(err)
			}
			_ = w
		}(w)
	}
	wg.Wait()

	require.Empty(t, errs, "no store operation should fail under concurrency")
	_, ok, err := s.Get(context.Background(), "read_file:key")
	require.NoError(t, err)
	assert.False(t, ok, "key deleted by last writer should be absent")
}

func TestStoreSatisfiesInterface(t *testing.T) {
	var _ ApprovalStore = (*InMemoryApprovalStore)(nil)
}
