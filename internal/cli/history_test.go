package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// HistoryStore tests
// ---------------------------------------------------------------------------

func TestHistoryStore_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	hs := NewHistoryStore(1000, path)
	hs.Add("first command")
	hs.Add("second command")
	hs.Add("third command")

	require.NoError(t, hs.Save())

	// Verify the file is in JSONL format.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	assert.Len(t, lines, 3)

	// Load into a fresh store with the same path.
	hs2 := NewHistoryStore(1000, path)
	require.NoError(t, hs2.Load())

	entries := hs2.List()
	assert.Equal(t, []string{"first command", "second command", "third command"}, entries)
}

func TestHistoryStore_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	// Nested non-existent parent directories.
	path := filepath.Join(dir, "a", "b", "c", "history.jsonl")

	hs := NewHistoryStore(1000, path)
	hs.Add("hello")
	require.NoError(t, hs.Save())

	// Parent directories should have been created.
	info, err := os.Stat(filepath.Join(dir, "a", "b", "c"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	// File should exist.
	_, err = os.Stat(path)
	require.NoError(t, err)
}

func TestHistoryStore_EmptyPathNoOp(t *testing.T) {
	dir := t.TempDir()

	hs := NewHistoryStore(1000, "")
	hs.Add("entry1")
	hs.Add("entry2")

	// Save should be a no-op — no file created.
	require.NoError(t, hs.Save())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)

	// Load should also be a no-op.
	require.NoError(t, hs.Load())
	assert.Equal(t, []string{"entry1", "entry2"}, hs.List())
}

func TestHistoryStore_LoadNonExistentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.jsonl")

	hs := NewHistoryStore(1000, path)
	// Load should return nil error for non-existent file.
	require.NoError(t, hs.Load())
	assert.Empty(t, hs.List())
}

func TestHistoryStore_MaxLenFIFOEviction(t *testing.T) {
	hs := NewHistoryStore(3, "")
	hs.Add("a")
	hs.Add("b")
	hs.Add("c")
	hs.Add("d") // "a" should be evicted

	assert.Equal(t, []string{"b", "c", "d"}, hs.List())
}

func TestHistoryStore_DedupConsecutive(t *testing.T) {
	hs := NewHistoryStore(1000, "")
	hs.Add("ls")
	hs.Add("ls")   // consecutive duplicate — skipped
	hs.Add("ls -la")
	hs.Add("ls -la") // consecutive duplicate — skipped
	hs.Add("ls")     // not consecutive duplicate — kept

	assert.Equal(t, []string{"ls", "ls -la", "ls"}, hs.List())
}

func TestHistoryStore_DedupEmptySkipped(t *testing.T) {
	hs := NewHistoryStore(1000, "")
	hs.Add("")
	hs.Add("   ")
	hs.Add("\t\n")
	hs.Add("real command")
	hs.Add("")
	hs.Add("another")

	assert.Equal(t, []string{"real command", "another"}, hs.List())
}

// ---------------------------------------------------------------------------
// NewDefaultLineEditor option tests
// ---------------------------------------------------------------------------

func TestNewDefaultLineEditor_BackwardCompat(t *testing.T) {
	le := NewDefaultLineEditor(nil, nil)
	hs := le.HistoryStore()
	require.NotNil(t, hs)
	assert.Equal(t, "", hs.filePath)
	assert.Equal(t, 1000, hs.maxLen)
}

func TestNewDefaultLineEditor_WithHistoryPath(t *testing.T) {
	le := NewDefaultLineEditor(nil, nil, WithHistoryPath("/tmp/test-history.jsonl"))
	hs := le.HistoryStore()
	require.NotNil(t, hs)
	assert.Equal(t, "/tmp/test-history.jsonl", hs.filePath)
}

func TestNewDefaultLineEditor_WithHistoryMaxLen(t *testing.T) {
	le := NewDefaultLineEditor(nil, nil, WithHistoryMaxLen(500))
	hs := le.HistoryStore()
	require.NotNil(t, hs)
	assert.Equal(t, 500, hs.maxLen)
}

func TestNewDefaultLineEditor_WithHistoryMaxLenZeroIgnored(t *testing.T) {
	le := NewDefaultLineEditor(nil, nil, WithHistoryMaxLen(0))
	hs := le.HistoryStore()
	require.NotNil(t, hs)
	assert.Equal(t, 1000, hs.maxLen)

	le2 := NewDefaultLineEditor(nil, nil, WithHistoryMaxLen(-5))
	hs2 := le2.HistoryStore()
	require.NotNil(t, hs2)
	assert.Equal(t, 1000, hs2.maxLen)
}

func TestNewDefaultLineEditor_NilOpts(t *testing.T) {
	// Nil options should not panic.
	le := NewDefaultLineEditor(nil, nil, nil, nil, WithHistoryPath("/x"), nil)
	hs := le.HistoryStore()
	require.NotNil(t, hs)
	assert.Equal(t, "/x", hs.filePath)
}

func TestNewDefaultLineEditor_MultipleOpts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.jsonl")

	le := NewDefaultLineEditor(nil, nil,
		WithHistoryPath(path),
		WithHistoryMaxLen(42),
	)
	hs := le.HistoryStore()
	require.NotNil(t, hs)
	assert.Equal(t, path, hs.filePath)
	assert.Equal(t, 42, hs.maxLen)
}

// ---------------------------------------------------------------------------
// History persistence integration tests
// ---------------------------------------------------------------------------

func TestHistoryPersistence_DefaultPath(t *testing.T) {
	// When cfg.History.Path is empty and HOME is available, the resolved
	// path should be ~/.go-cli/history.jsonl. We simulate this by setting
	// HOME to a temp directory.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// On macOS, UserHomeDir reads $HOME.
	t.Setenv("USERPROFILE", dir) // Windows fallback

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	expectedPath := filepath.Join(home, ".go-cli", "history.jsonl")

	// Simulate what interactive.go does when cfg.History.Path is empty.
	historyPath := ""
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		historyPath = filepath.Join(home, ".go-cli", "history.jsonl")
	}

	assert.Equal(t, expectedPath, historyPath)

	// Write some history and verify the file lands at the expected path.
	hs := NewHistoryStore(1000, historyPath)
	hs.Add("persisted command")
	require.NoError(t, hs.Save())

	_, err = os.Stat(expectedPath)
	require.NoError(t, err)

	// Load in a new store and verify round-trip.
	hs2 := NewHistoryStore(1000, historyPath)
	require.NoError(t, hs2.Load())
	assert.Equal(t, []string{"persisted command"}, hs2.List())
}

func TestHistoryPersistence_HomeUnavailable(t *testing.T) {
	// When HOME is not set and the config path is empty, historyPath stays
	// empty — Save/Load become no-ops and must not panic.
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	historyPath := ""
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		historyPath = filepath.Join(home, ".go-cli", "history.jsonl")
	}

	assert.Equal(t, "", historyPath)

	hs := NewHistoryStore(1000, historyPath)
	hs.Add("should not persist")

	// Save and Load are no-ops; no panic, no file created.
	require.NoError(t, hs.Save())
	require.NoError(t, hs.Load())
	assert.Equal(t, []string{"should not persist"}, hs.List())
}
