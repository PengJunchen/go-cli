package approval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateGlobalConfigFile_CorrectPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))

	err := ValidateGlobalConfigFile(path)
	assert.NoError(t, err, "0600 permissions with correct owner must be accepted")
}

func TestValidateGlobalConfigFile_WrongPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o644))

	err := ValidateGlobalConfigFile(path)
	require.Error(t, err, "0644 permissions must be rejected")
	assert.Contains(t, err.Error(), "expected 0600")
}

func TestValidateGlobalConfigFile_WorldWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o666))

	err := ValidateGlobalConfigFile(path)
	require.Error(t, err, "0666 permissions must be rejected")
	assert.Contains(t, err.Error(), "expected 0600")
}

func TestValidateGlobalConfigFile_NonExistent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	err := ValidateGlobalConfigFile(path)
	require.Error(t, err, "missing file must return error")
}

func TestTrustProject_UsesContentFingerprint(t *testing.T) {
	dir := t.TempDir()
	// Create a .go-cli/mcp.json config file with specific content.
	configDir := filepath.Join(dir, ".go-cli")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	configPath := filepath.Join(configDir, "mcp.json")
	configContent := []byte(`{"servers":[{"name":"test","command":"echo"}]}`)
	require.NoError(t, os.WriteFile(configPath, configContent, 0o600))

	store := NewInMemoryTrustStore()
	tm := NewDefaultTrustManager(store)
	require.NoError(t, tm.TrustProject(t.Context(), dir))

	// The fingerprint should match the content hash of the config file.
	entries, err := store.Load()
	require.NoError(t, err)
	entry, ok := entries[dir]
	require.True(t, ok, "trusted project must have an entry")
	assert.Equal(t, contentFingerprint(configPath), entry.Fingerprint,
		"fingerprint must be content hash of .go-cli/mcp.json")

	// Change the config content and trust again — fingerprint must differ.
	require.NoError(t, os.WriteFile(configPath, []byte(`{"servers":[]}`), 0o600))
	require.NoError(t, tm.TrustProject(t.Context(), dir))
	entries, err = store.Load()
	require.NoError(t, err)
	updated := entries[dir]
	assert.NotEqual(t, entry.Fingerprint, updated.Fingerprint,
		"fingerprint must change when config content changes")
}
