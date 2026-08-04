package cli

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHandler is a minimal SlashCommandHandler used by registry tests. It
// writes its name and args to the output so callers can verify dispatch.
type fakeHandler struct {
	name string
	desc string
}

func (h *fakeHandler) Name() string        { return h.name }
func (h *fakeHandler) Description() string { return h.desc }
func (h *fakeHandler) Handle(_ context.Context, args []string, sc *slashContext) error {
	fmt.Fprintf(sc.out, "%s called with %v\n", h.name, args) //nolint:errcheck
	return nil
}

var _ SlashCommandHandler = (*fakeHandler)(nil)

func TestSlashRegistryRegisterAndLookup(t *testing.T) {
	reg := NewSlashCommandRegistry()
	require.NoError(t, reg.Register(&fakeHandler{name: "alpha", desc: "alpha desc"}))

	h, ok := reg.Lookup("alpha")
	require.True(t, ok)
	assert.Equal(t, "alpha", h.Name())
	assert.Equal(t, "alpha desc", h.Description())
}

func TestSlashRegistryLookupUnknown(t *testing.T) {
	reg := NewSlashCommandRegistry()
	_, ok := reg.Lookup("missing")
	assert.False(t, ok)
}

func TestSlashRegistryRegisterDuplicate(t *testing.T) {
	reg := NewSlashCommandRegistry()
	require.NoError(t, reg.Register(&fakeHandler{name: "dup", desc: "first"}))
	err := reg.Register(&fakeHandler{name: "dup", desc: "second"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already registered")
}

func TestSlashRegistryRegisterNilHandler(t *testing.T) {
	reg := NewSlashCommandRegistry()
	err := reg.Register(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestSlashRegistryRegisterEmptyName(t *testing.T) {
	reg := NewSlashCommandRegistry()
	err := reg.Register(&fakeHandler{name: "", desc: "empty"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestSlashRegistryAlias(t *testing.T) {
	reg := NewSlashCommandRegistry()
	require.NoError(t, reg.Register(&fakeHandler{name: "help", desc: "help desc"}))
	reg.RegisterAlias("h", "help")

	h, ok := reg.Lookup("h")
	require.True(t, ok)
	assert.Equal(t, "help", h.Name())

	// Original name still resolves.
	h2, ok := reg.Lookup("help")
	require.True(t, ok)
	assert.Equal(t, "help", h2.Name())
}

func TestSlashRegistryAliasUnknownTarget(t *testing.T) {
	reg := NewSlashCommandRegistry()
	reg.RegisterAlias("x", "does-not-exist")
	_, ok := reg.Lookup("x")
	assert.False(t, ok, "alias whose target is not registered should not resolve")
}

func TestSlashRegistryListSortedByName(t *testing.T) {
	reg := NewSlashCommandRegistry()
	require.NoError(t, reg.Register(&fakeHandler{name: "zeta", desc: "z"}))
	require.NoError(t, reg.Register(&fakeHandler{name: "alpha", desc: "a"}))
	require.NoError(t, reg.Register(&fakeHandler{name: "mid", desc: "m"}))

	list := reg.List()
	require.Len(t, list, 3)
	assert.Equal(t, "alpha", list[0].Name())
	assert.Equal(t, "mid", list[1].Name())
	assert.Equal(t, "zeta", list[2].Name())
}

func TestSlashRegistryNamesSorted(t *testing.T) {
	reg := NewSlashCommandRegistry()
	require.NoError(t, reg.Register(&fakeHandler{name: "zeta", desc: "z"}))
	require.NoError(t, reg.Register(&fakeHandler{name: "alpha", desc: "a"}))

	names := reg.Names()
	require.Len(t, names, 2)
	assert.Equal(t, []string{"alpha", "zeta"}, names)
}

func TestSlashRegistryConcurrentAccess(t *testing.T) {
	reg := NewSlashCommandRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = reg.Register(&fakeHandler{name: fmt.Sprintf("c%d", i), desc: "c"}) //nolint:errcheck
			_, _ = reg.Lookup("c0")
			_ = reg.List()
			_ = reg.Names()
		}(i)
	}
	wg.Wait()
	assert.Len(t, reg.List(), 20)
}
