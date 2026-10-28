package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionCatalogStrictlyReadOnly(t *testing.T) {
	prepared, dir := seedHistoryAdmin(t)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.NoError(t, prepared.Close())
	path := filepath.Join(dir, "sessions", snapshot.SessionID+".jsonl")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(original, []byte(`{"id":"torn"`)...), 0600))
	before, err := os.Stat(path)
	require.NoError(t, err)
	entries, err := ListSessionCatalog(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "invalid", entries[0].Status)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, append(original, []byte(`{"id":"torn"`)...), after)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), info.ModTime())
	files, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, files, 1)
}

func TestSessionCatalogReadyAndBusy(t *testing.T) {
	prepared, dir := seedHistoryAdmin(t)
	entries, err := ListSessionCatalog(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "busy", entries[0].Status)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.NoError(t, prepared.Close())
	entries, err = ListSessionCatalog(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, "ready", entries[0].Status)
	require.Equal(t, snapshot.SessionID, entries[0].SessionID)
	require.Equal(t, "question", entries[0].Preview)
}
