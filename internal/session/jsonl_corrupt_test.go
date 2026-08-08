package session

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestJSONLLogsCorruptLines verifies that the JSONL reader logs corrupt lines
// with their line numbers instead of silently skipping them.
func TestJSONLLogsCorruptLines(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Capture slog output at Warn level.
	var buf bytes.Buffer
	orig := slog.Default()
	defer slog.SetDefault(orig)
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Close())

	// Append a corrupt line directly to the file.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("this is not json\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Re-open to trigger load; the corrupt line is on line 2.
	reopened := NewJSONLSessionStore(path)
	got, err := reopened.Get(context.Background(), "a")
	require.NoError(t, err)
	assert.Equal(t, "content-a", got.Content)

	// The corrupt line was logged with the event name and line number.
	logOutput := buf.String()
	assert.Contains(t, logOutput, "session.jsonl.corrupt_line")
	assert.Contains(t, logOutput, "line=2")

	require.NoError(t, reopened.Close())
}
