package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// stubTool is a minimal ToolDefinition used to exercise the registry.
type stubTool struct {
	name string
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "stub " + s.name }
func (s *stubTool) Execute(_ context.Context, call ToolCall) (*ToolResult, error) {
	return &ToolResult{
		Output:   "ran " + s.name,
		Metadata: map[string]any{"name": s.name, "id": call.ID},
	}, nil
}

func TestRegistryRegisterGetList(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	a := &stubTool{name: "alpha"}
	b := &stubTool{name: "beta"}

	require.NoError(t, reg.Register(ctx, a))
	require.NoError(t, reg.Register(ctx, b))

	got, err := reg.Get(ctx, "alpha")
	require.NoError(t, err)
	assert.Same(t, a, got)

	gotB, err := reg.Get(ctx, "beta")
	require.NoError(t, err)
	assert.Same(t, b, gotB)

	list, err := reg.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	// Registration order preserved.
	assert.Same(t, a, list[0])
	assert.Same(t, b, list[1])
}

func TestRegistryGetUnknown(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	_, err := reg.Get(ctx, "nope")
	assert.ErrorIs(t, err, ErrToolNotFound)
}

func TestRegistryRegisterNilAndEmptyName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	assert.Error(t, reg.Register(ctx, nil))
	assert.Error(t, reg.Register(ctx, &stubTool{name: ""}))
}

func TestRegistryDuplicateOverwrite(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	first := &stubTool{name: "tool"}
	second := &stubTool{name: "tool"}

	require.NoError(t, reg.Register(ctx, first))
	require.NoError(t, reg.Register(ctx, second))

	got, err := reg.Get(ctx, "tool")
	require.NoError(t, err)
	// Last registration wins.
	assert.Same(t, second, got)

	// List still reports one entry (name preserved in position).
	list, err := reg.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Same(t, second, list[0])
}

func TestRegistryListReturnsCopy(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()
	require.NoError(t, reg.Register(ctx, &stubTool{name: "x"}))

	list1, err := reg.List(ctx)
	require.NoError(t, err)
	list1[0] = nil

	// The registry's internal slice is untouched.
	list2, err := reg.List(ctx)
	require.NoError(t, err)
	assert.NotNil(t, list2[0])
}

func TestRegistryConcurrentRegisterGet(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	const workers = 20
	const toolsPerWorker = 10

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < toolsPerWorker; j++ {
				name := "t"
				require.NoError(t, reg.Register(ctx, &stubTool{name: name}))
				_, err := reg.Get(ctx, name)
				if err != nil {
					t.Errorf("concurrent Get failed: %v", err)
				}
				_, err = reg.List(ctx)
				if err != nil {
					t.Errorf("concurrent List failed: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()

	// Sanity: reads succeed after all registrations settle.
	_, err := reg.Get(ctx, "t")
	assert.NoError(t, err)
}

func TestRegistryExecute(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry().(*DefaultToolRegistry)
	require.NoError(t, reg.Register(ctx, &stubTool{name: "alpha"}))

	res, err := reg.Execute(ctx, ToolCall{ID: "call-1", Name: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, "ran alpha", res.Output)
	assert.Equal(t, "call-1", res.Metadata["id"])

	// Unknown tool returns an error via Execute.
	_, err = reg.Execute(ctx, ToolCall{ID: "call-2", Name: "missing"})
	assert.ErrorIs(t, err, ErrToolNotFound)

	// Empty name is rejected.
	_, err = reg.Execute(ctx, ToolCall{ID: "call-3"})
	assert.Error(t, err)
}

func TestRegistryExecuteWithSpanContext(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := NewDefaultToolRegistry().(*DefaultToolRegistry)
	ctx := context.Background()
	require.NoError(t, reg.Register(ctx, &stubTool{name: "alpha"}))

	// A tracer-injected context should not change behavior; Execute must still
	// succeed. This exercises the SpanFromContext path.
	res, err := reg.Execute(ctx, ToolCall{ID: "c", Name: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, "ran alpha", res.Output)
}
