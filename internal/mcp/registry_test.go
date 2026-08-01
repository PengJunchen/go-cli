package mcp

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryRegisterAndGet(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()
	c := newRecordingMCPClient("srv", nil)

	require.NoError(t, reg.Register("srv", c))

	got, err := reg.Get("srv")
	require.NoError(t, err)
	assert.Same(t, c, got)
}

func TestRegistryGetUnknownReturnsErrMCPClientNotFound(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()

	_, err := reg.Get("missing")
	require.ErrorIs(t, err, ErrMCPClientNotFound)
}

func TestRegistryRegisterRejectsNilClient(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()

	err := reg.Register("srv", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil client")

	// A rejected client must not be observable through Get.
	_, getErr := reg.Get("srv")
	require.ErrorIs(t, getErr, ErrMCPClientNotFound)
}

func TestRegistryRegisterRejectsEmptyName(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()

	err := reg.Register("", newRecordingMCPClient("srv", nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty name")
}

func TestRegistryRegisterOverwritesPreservingOrder(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()
	a := newRecordingMCPClient("a", nil)
	b := newRecordingMCPClient("b", nil)
	a2 := newRecordingMCPClient("a2", nil)

	require.NoError(t, reg.Register("a", a))
	require.NoError(t, reg.Register("b", b))
	// Overwriting an existing name must replace in place, not re-append.
	require.NoError(t, reg.Register("a", a2))

	clients := reg.List(context.Background())
	require.Len(t, clients, 2)
	assert.Same(t, a2, clients[0], "overwritten client must replace in place")
	assert.Same(t, b, clients[1])

	// Get must return the latest registration.
	got, err := reg.Get("a")
	require.NoError(t, err)
	assert.Same(t, a2, got)
}

func TestRegistryListEmpty(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()
	assert.Empty(t, reg.List(context.Background()))
}

func TestRegistryListPreservesRegistrationOrder(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()
	names := []string{"alpha", "beta", "gamma"}
	for _, n := range names {
		require.NoError(t, reg.Register(n, newRecordingMCPClient(n, nil)))
	}

	clients := reg.List(context.Background())
	require.Len(t, clients, len(names))
	for i, n := range names {
		assert.Equal(t, n, clients[i].Name(), "List must preserve registration order")
	}
}

func TestRegisterMCPClientDefaultsNameFromClient(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()
	c := newRecordingMCPClient("derived-name", nil)

	require.NoError(t, RegisterMCPClient(reg, "", c))

	got, err := reg.Get("derived-name")
	require.NoError(t, err)
	assert.Same(t, c, got)
}

func TestRegisterMCPClientWithExplicitNameDoesNotOverride(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()
	c := newRecordingMCPClient("client-name", nil)

	// An explicit name wins over the client's own Name().
	require.NoError(t, RegisterMCPClient(reg, "explicit", c))

	got, err := reg.Get("explicit")
	require.NoError(t, err)
	assert.Same(t, c, got)

	if _, err := reg.Get("client-name"); err == nil {
		t.Fatal("client's own name must not be used when an explicit name is given")
	}
}

func TestRegisterMCPClientWithNilRegistryDoesNotPanic(t *testing.T) {
	t.Parallel()
	c := newRecordingMCPClient("srv", nil)

	// A nil registry is replaced with a fresh one internally; the call must
	// succeed without panicking.
	require.NoError(t, RegisterMCPClient(nil, "srv", c))
}

func TestRegisterMCPClientRejectsNilClientWithEmptyName(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()

	// nil client + empty name: the name cannot be derived, so Register must
	// reject the nil client rather than storing an empty-named entry.
	err := RegisterMCPClient(reg, "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil client")
}

func TestRegistryConcurrentAccessIsSafe(t *testing.T) {
	t.Parallel()
	reg := NewMCPClientRegistry()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		name := "c" + strconv.Itoa(i)
		wg.Add(3)
		go func(name string) {
			defer wg.Done()
			//nolint:errcheck // registry may reject a re-registered name; not fatal under the stress test
			_ = reg.Register(name, newRecordingMCPClient(name, nil))
		}(name)
		go func(name string) {
			defer wg.Done()
			//nolint:errcheck // Get may race Register and yield not-found; expected under concurrency
			_, _ = reg.Get(name)
		}(name)
		go func() {
			defer wg.Done()
			//nolint:errcheck // List has no error to propagate
			_ = reg.List(context.Background())
		}()
	}
	wg.Wait()

	// After every Register completes, all distinct clients are retrievable and
	// listed exactly once (no duplicates, no drops).
	clients := reg.List(context.Background())
	require.Len(t, clients, n, "every registered client must be listed once")
	for _, c := range clients {
		_, err := reg.Get(c.Name())
		require.NoError(t, err, "listed client must be retrievable by name")
	}
}
