package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestRegistryNoGlobalState verifies that the registry has no package-level
// global singleton. Each call to NewDefaultToolRegistry must produce a fresh,
// independent instance — there is no shared Global variable that tests could
// accidentally mutate.
func TestRegistryNoGlobalState(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()

	reg1 := NewDefaultToolRegistry()
	require.NoError(t, reg1.Register(ctx, &stubTool{name: "only-in-reg1"}))

	reg2 := NewDefaultToolRegistry()

	// reg2 must not see tools registered in reg1.
	_, err := reg2.Get(ctx, "only-in-reg1")
	assert.ErrorIs(t, err, ErrToolNotFound)

	// reg1 must still see its own tool.
	got, err := reg1.Get(ctx, "only-in-reg1")
	require.NoError(t, err)
	assert.Equal(t, "only-in-reg1", got.Name())
}

// TestRegistryInjectable verifies that the registry is constructed via a
// constructor and can be injected as a dependency into any component that
// accepts the ToolRegistry interface — no global lookup required.
func TestRegistryInjectable(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()
	require.NoError(t, reg.Register(ctx, &stubTool{name: "injectable-tool"}))

	// Simulate a component that receives the registry as a dependency.
	consumer := struct {
		tools ToolRegistry
	}{tools: reg}

	got, err := consumer.tools.Get(ctx, "injectable-tool")
	require.NoError(t, err)
	assert.Equal(t, "injectable-tool", got.Name())

	list, err := consumer.tools.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

// TestRegistryIsolation verifies that two independently constructed registries
// are fully isolated: registering a tool in one does not leak into the other.
func TestRegistryIsolation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	regA := NewDefaultToolRegistry()
	regB := NewDefaultToolRegistry()

	require.NoError(t, regA.Register(ctx, &stubTool{name: "alpha"}))
	require.NoError(t, regB.Register(ctx, &stubTool{name: "beta"}))

	// regA has alpha, not beta.
	gotA, err := regA.Get(ctx, "alpha")
	require.NoError(t, err)
	assert.Equal(t, "alpha", gotA.Name())

	_, err = regA.Get(ctx, "beta")
	assert.ErrorIs(t, err, ErrToolNotFound)

	// regB has beta, not alpha.
	gotB, err := regB.Get(ctx, "beta")
	require.NoError(t, err)
	assert.Equal(t, "beta", gotB.Name())

	_, err = regB.Get(ctx, "alpha")
	assert.ErrorIs(t, err, ErrToolNotFound)

	// List sizes are independent.
	listA, err := regA.List(ctx)
	require.NoError(t, err)
	assert.Len(t, listA, 1)

	listB, err := regB.List(ctx)
	require.NoError(t, err)
	assert.Len(t, listB, 1)
}

// TestRegistryConcurrentSafe verifies that concurrent Register, Get, and List
// calls on the same registry instance do not race or corrupt state.
func TestRegistryConcurrentSafe(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	// Pre-register a set of tools.
	for i := 0; i < 10; i++ {
		require.NoError(t, reg.Register(ctx, &stubTool{name: toolName(i)}))
	}

	const workers = 30
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Mix of reads and writes.
			_ = reg.Register(ctx, &stubTool{name: toolName(n % 10)}) // overwrite
			_, _ = reg.Get(ctx, toolName(n%10))
			_, _ = reg.List(ctx)
		}(w)
	}
	wg.Wait()

	// After all goroutines settle, the registry should still be consistent.
	list, err := reg.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 10)
}

func toolName(n int) string {
	return "tool_" + string(rune('a'+n))
}
