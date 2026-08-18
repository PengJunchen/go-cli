package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/memory"
	"github.com/pengjunchen/go-cli/internal/session"
)

// newTestMemoryStore returns a FileMemoryStore backed by a fresh temp file.
// The store is closed automatically when the test finishes.
func newTestMemoryStore(t *testing.T) *memory.FileMemoryStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memories.jsonl")
	s, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSlashMemoryListEmpty(t *testing.T) {
	store := newTestMemoryStore(t)
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "memory"}, sc)

	assert.Contains(t, buf.String(), "No memories stored.")
}

func TestSlashMemoryListExplicit(t *testing.T) {
	store := newTestMemoryStore(t)
	require.NoError(t, store.Add(context.Background(), memory.Memory{
		ID: "mem_1", Content: "prefers dark mode", Category: "preference", Source: "manual",
	}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "memory", Args: []string{"list"}}, sc)
	output := buf.String()

	assert.Contains(t, output, "Memories (1):")
	assert.Contains(t, output, "mem_1")
	assert.Contains(t, output, "[preference]")
	assert.Contains(t, output, "prefers dark mode")
}

func TestSlashMemoryListMultiple(t *testing.T) {
	store := newTestMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, store.Add(ctx, memory.Memory{ID: "a", Content: "alpha memory", Category: "fact"}))
	require.NoError(t, store.Add(ctx, memory.Memory{ID: "b", Content: "beta memory", Category: "convention"}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "memory"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Memories (2):")
	assert.Contains(t, output, "alpha memory")
	assert.Contains(t, output, "beta memory")
}

func TestSlashMemoryAdd(t *testing.T) {
	store := newTestMemoryStore(t)
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"add", "user", "likes", "Go"},
	}, sc)
	output := buf.String()

	assert.Contains(t, output, "Added memory")
	assert.Contains(t, output, "user likes Go")

	// Verify it was actually stored.
	list, err := store.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "user likes Go", list[0].Content)
	assert.Equal(t, "manual", list[0].Category)
	assert.Equal(t, "manual", list[0].Source)
}

func TestSlashMemoryAddNoText(t *testing.T) {
	store := newTestMemoryStore(t)
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"add"},
	}, sc)

	assert.Contains(t, buf.String(), "Usage: /memory add <text>")
}

func TestSlashMemoryDelete(t *testing.T) {
	store := newTestMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, store.Add(ctx, memory.Memory{ID: "mem_x", Content: "to be deleted", Category: "fact"}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"delete", "mem_x"},
	}, sc)
	output := buf.String()

	assert.Contains(t, output, "Deleted memory mem_x")

	list, err := store.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestSlashMemoryDeleteNotFound(t *testing.T) {
	store := newTestMemoryStore(t)
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"delete", "nonexistent"},
	}, sc)

	assert.Contains(t, buf.String(), "Error deleting memory")
}

func TestSlashMemoryDeleteNoID(t *testing.T) {
	store := newTestMemoryStore(t)
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"delete"},
	}, sc)

	assert.Contains(t, buf.String(), "Usage: /memory delete <id>")
}

func TestSlashMemorySearch(t *testing.T) {
	store := newTestMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, store.Add(ctx, memory.Memory{ID: "d1", Content: "The user prefers Go for backend", Category: "preference"}))
	require.NoError(t, store.Add(ctx, memory.Memory{ID: "d2", Content: "Python is used for analysis", Category: "fact"}))
	require.NoError(t, store.Add(ctx, memory.Memory{ID: "d3", Content: "Deployments happen on Fridays", Category: "convention"}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"search", "backend"},
	}, sc)
	output := buf.String()

	assert.Contains(t, output, "Search results (1):")
	assert.Contains(t, output, "d1")
	assert.Contains(t, output, "prefers Go for backend")
}

func TestSlashMemorySearchNoMatch(t *testing.T) {
	store := newTestMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, store.Add(ctx, memory.Memory{ID: "d1", Content: "alpha beta", Category: "fact"}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"search", "nonexistent"},
	}, sc)

	assert.Contains(t, buf.String(), "No matching memories.")
}

func TestSlashMemorySearchNoQuery(t *testing.T) {
	store := newTestMemoryStore(t)
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"search"},
	}, sc)

	assert.Contains(t, buf.String(), "Usage: /memory search <query>")
}

func TestSlashMemoryClearWithoutConfirm(t *testing.T) {
	store := newTestMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, store.Add(ctx, memory.Memory{ID: "d1", Content: "keep me", Category: "fact"}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"clear"},
	}, sc)

	assert.Contains(t, buf.String(), "Type /memory clear confirm to proceed")

	// Memories should still exist.
	list, err := store.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestSlashMemoryClearConfirm(t *testing.T) {
	store := newTestMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, store.Add(ctx, memory.Memory{ID: "d1", Content: "first", Category: "fact"}))
	require.NoError(t, store.Add(ctx, memory.Memory{ID: "d2", Content: "second", Category: "convention"}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"clear", "confirm"},
	}, sc)
	output := buf.String()

	assert.Contains(t, output, "Cleared 2 memories")

	list, err := store.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestSlashMemoryClearConfirmEmpty(t *testing.T) {
	store := newTestMemoryStore(t)
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"clear", "confirm"},
	}, sc)

	assert.Contains(t, buf.String(), "Cleared 0 memories")
}

func TestSlashMemoryNotConfigured(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "memory"}, sc)

	assert.Contains(t, buf.String(), "Memory store not configured.")
}

func TestSlashMemoryUnknownSubcommand(t *testing.T) {
	store := newTestMemoryStore(t)
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, memoryStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "memory", Args: []string{"foobar"},
	}, sc)
	output := buf.String()

	assert.Contains(t, output, "Unknown subcommand: foobar")
	assert.Contains(t, output, "Usage: /memory")
}
