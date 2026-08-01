package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestRegisterDefaultsRegistersBuiltins(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := NewDefaultToolRegistry()
	require.NoError(t, RegisterDefaults(context.Background(), reg))

	list, err := reg.List(context.Background())
	require.NoError(t, err)

	names := map[string]bool{}
	for _, def := range list {
		names[def.Name()] = true
	}

	assert.Len(t, names, 3)
	assert.True(t, names["read"])
	assert.True(t, names["bash"])
	assert.True(t, names["write"])

	for _, name := range []string{"read", "bash", "write"} {
		def, err := reg.Get(context.Background(), name)
		require.NoError(t, err)
		assert.Equal(t, name, def.Name())
	}
}

func TestRegisterDefaultsNilRegistry(t *testing.T) {
	assert.Error(t, RegisterDefaults(context.Background(), nil))
}

func TestRegisterDefaultsTwiceOverwrites(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	// Behavior choice: duplicate names are overwritten (last wins), so a second
	// call succeeds and List still yields exactly the built-in names.
	require.NoError(t, RegisterDefaults(ctx, reg))
	require.NoError(t, RegisterDefaults(ctx, reg))

	list, err := reg.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 3)
}
