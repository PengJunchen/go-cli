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

	assert.Len(t, names, 7)
	assert.True(t, names["read"])
	assert.True(t, names["bash"])
	assert.True(t, names["write"])
	assert.True(t, names["edit"])
	assert.True(t, names["grep"])
	assert.True(t, names["find"])
	assert.True(t, names["ls"])

	for _, name := range []string{"read", "bash", "write", "edit", "grep", "find", "ls"} {
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
	assert.Len(t, list, 7)
}

// TestRegisterDefaultsWithFileTracker verifies that RegisterDefaults wires the
// FileTracker into both WriteTool and EditFileTool when the option is passed.
func TestRegisterDefaultsWithFileTracker(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()
	ft := NewFileTracker()

	require.NoError(t, RegisterDefaults(ctx, reg, WithRegisteredFileTracker(ft)))

	// WriteTool should have the fileTracker set.
	wt, err := reg.Get(ctx, "write")
	require.NoError(t, err)
	wtConcrete, ok := wt.(*WriteTool)
	require.True(t, ok)
	assert.Equal(t, ft, wtConcrete.fileTracker)

	// EditFileTool should have the fileTracker set.
	et, err := reg.Get(ctx, "edit")
	require.NoError(t, err)
	etConcrete, ok := et.(*EditFileTool)
	require.True(t, ok)
	assert.Equal(t, ft, etConcrete.fileTracker)
}

// TestRegisterDefaultsWithDiffGenerator verifies that RegisterDefaults wires the
// DiffGenerator into both WriteTool and EditFileTool when the option is passed.
func TestRegisterDefaultsWithDiffGenerator(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()
	dg := NewUnifiedDiffGenerator(0, false)

	require.NoError(t, RegisterDefaults(ctx, reg, WithRegisteredDiffGenerator(dg)))

	// WriteTool should have the diffGenerator set.
	wt, err := reg.Get(ctx, "write")
	require.NoError(t, err)
	wtConcrete, ok := wt.(*WriteTool)
	require.True(t, ok)
	assert.Equal(t, dg, wtConcrete.diffGenerator)

	// EditFileTool should have the diffGenerator set.
	et, err := reg.Get(ctx, "edit")
	require.NoError(t, err)
	etConcrete, ok := et.(*EditFileTool)
	require.True(t, ok)
	assert.Equal(t, dg, etConcrete.diffGenerator)
}

// TestRegisterDefaultsWithBashSandbox verifies that RegisterDefaults wires the
// BashSandbox into BashTool when the option is passed.
func TestRegisterDefaultsWithBashSandbox(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()
	sb := NewDefaultBashSandbox()

	require.NoError(t, RegisterDefaults(ctx, reg, WithRegisteredBashSandbox(sb)))

	bt, err := reg.Get(ctx, "bash")
	require.NoError(t, err)
	btConcrete, ok := bt.(*BashTool)
	require.True(t, ok)
	assert.Equal(t, sb, btConcrete.Sandbox)
}

// TestRegisterDefaultsBackwardCompatible verifies that calling RegisterDefaults
// with no options still works (variadic parameter is backward compatible).
func TestRegisterDefaultsBackwardCompatible(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	require.NoError(t, RegisterDefaults(ctx, reg))

	// Tools should still be registered without any options wired.
	wt, err := reg.Get(ctx, "write")
	require.NoError(t, err)
	wtConcrete, ok := wt.(*WriteTool)
	require.True(t, ok)
	assert.Nil(t, wtConcrete.fileTracker)
	assert.Nil(t, wtConcrete.diffGenerator)
}

// TestRegisterDefaultsBuiltinWhitelist verifies that when a whitelist is
// configured, only the named builtin tools are registered.
func TestRegisterDefaultsBuiltinWhitelist(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	require.NoError(t, RegisterDefaults(ctx, reg,
		WithRegisteredBuiltinWhitelist([]string{"read", "ls"}),
	))

	list, err := reg.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)

	names := map[string]bool{}
	for _, def := range list {
		names[def.Name()] = true
	}
	assert.True(t, names["read"])
	assert.True(t, names["ls"])

	// Whitelisted tools are retrievable.
	_, err = reg.Get(ctx, "read")
	assert.NoError(t, err)
	_, err = reg.Get(ctx, "ls")
	assert.NoError(t, err)

	// Non-whitelisted builtins are NOT registered.
	_, err = reg.Get(ctx, "bash")
	assert.ErrorIs(t, err, ErrToolNotFound)
	_, err = reg.Get(ctx, "write")
	assert.ErrorIs(t, err, ErrToolNotFound)
}

// TestRegisterDefaultsEmptyWhitelistRegistersAll verifies that an empty/nil
// whitelist preserves the default behavior (all builtins registered).
func TestRegisterDefaultsEmptyWhitelistRegistersAll(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	require.NoError(t, RegisterDefaults(ctx, reg,
		WithRegisteredBuiltinWhitelist(nil),
	))

	list, err := reg.List(ctx)
	require.NoError(t, err)
	// All 7 core builtins are registered (no git tool wired).
	assert.Len(t, list, 7)

	_, err = reg.Get(ctx, "bash")
	assert.NoError(t, err)
}
