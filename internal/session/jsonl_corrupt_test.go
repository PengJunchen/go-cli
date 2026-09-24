package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestJSONLOpenRejectsCorruptCompleteRow verifies that malformed complete
// rows fail closed instead of exposing a partial transcript.
func TestJSONLOpenRejectsCorruptCompleteRow(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

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

	// Re-open to trigger strict loading; the corrupt line is complete.
	reopened := NewJSONLSessionStore(path)
	err = reopened.Open(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode JSONL row")
	require.NoError(t, reopened.Close())
}
