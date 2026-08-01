package cli

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCommand is a minimal Command implementation for registry tests.
type fakeCommand struct {
	name     string
	synopsis string
	runErr   error
}

func (f *fakeCommand) Name() string     { return f.name }
func (f *fakeCommand) Synopsis() string { return f.synopsis }
func (f *fakeCommand) Run(ctx context.Context, cfg Config, args []string) error {
	return f.runErr
}

func TestDefaultCommandRegistry_RegisterAndGet(t *testing.T) {
	reg := NewDefaultCommandRegistry()
	cmd := &fakeCommand{name: "hello", synopsis: "say hello"}

	require.NoError(t, reg.Register(cmd))
	got, ok := reg.Get("hello")
	require.True(t, ok)
	assert.Same(t, cmd, got)
}

func TestDefaultCommandRegistry_GetMissing(t *testing.T) {
	reg := NewDefaultCommandRegistry()
	_, ok := reg.Get("nope")
	assert.False(t, ok)
}

func TestDefaultCommandRegistry_RegisterDuplicate(t *testing.T) {
	reg := NewDefaultCommandRegistry()
	require.NoError(t, reg.Register(&fakeCommand{name: "hello"}))

	err := reg.Register(&fakeCommand{name: "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hello")
}

func TestDefaultCommandRegistry_ListOrder(t *testing.T) {
	reg := NewDefaultCommandRegistry()
	a := &fakeCommand{name: "a"}
	b := &fakeCommand{name: "b"}
	c := &fakeCommand{name: "c"}

	for _, cmd := range []Command{a, b, c} {
		require.NoError(t, reg.Register(cmd))
	}

	got := reg.List()
	assert.Len(t, got, 3)
	assert.Equal(t, []Command{a, b, c}, got)
}

func TestDefaultCommandRegistry_ConcurrentSafety(t *testing.T) {
	reg := NewDefaultCommandRegistry()
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a' + i%26))
			if err := reg.Register(&fakeCommand{name: name}); err != nil {
				// duplicate names are expected because n > alphabet size; skip
				_ = err
			}
			_, _ = reg.Get(name)
		}(i)
	}
	wg.Wait()

	assert.NotZero(t, len(reg.List()))
}

var _ Command = (*fakeCommand)(nil)
