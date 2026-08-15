package approval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type errTrustStore struct {
	loadErr   error
	addErr    error
	removeErr error
	saveErr   error
}

func (s *errTrustStore) Load() (map[string]TrustEntry, error) {
	return nil, s.loadErr
}

func (s *errTrustStore) Save(map[string]TrustEntry) error {
	return s.saveErr
}

func (s *errTrustStore) Add(string, TrustEntry) error {
	return s.addErr
}

func (s *errTrustStore) Remove(string) error {
	return s.removeErr
}

func TestExpired_EmptyExpiresAt(t *testing.T) {
	entry := TrustEntry{Path: "/repo/a", ExpiresAt: ""}
	assert.False(t, expired(entry, time.Now()), "empty ExpiresAt must not be expired")
}

func TestExpired_ValidFuture(t *testing.T) {
	entry := TrustEntry{Path: "/repo/a", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	assert.False(t, expired(entry, time.Now()), "future expiry must not be expired")
}

func TestExpired_ValidPast(t *testing.T) {
	entry := TrustEntry{Path: "/repo/a", ExpiresAt: time.Now().Add(-time.Hour).Format(time.RFC3339)}
	assert.True(t, expired(entry, time.Now()), "past expiry must be expired")
}

func TestExpired_MalformedTimestamp(t *testing.T) {
	entry := TrustEntry{Path: "/repo/a", ExpiresAt: "not-a-timestamp"}
	assert.True(t, expired(entry, time.Now()), "malformed timestamp must be treated as expired")
}

func TestExpired_EmptyString(t *testing.T) {
	entry := TrustEntry{Path: "/repo/a", ExpiresAt: ""}
	assert.False(t, expired(entry, time.Now()), "empty ExpiresAt must not be expired")
}

func TestNewDefaultTrustManager_NilStore(t *testing.T) {
	tm := NewDefaultTrustManager(nil)
	require.NotNil(t, tm)
	require.NoError(t, tm.TrustProject(context.Background(), "/repo/a"))
	assert.True(t, tm.IsTrusted(context.Background(), "/repo/a"))
}

func TestIsTrusted_StoreLoadError(t *testing.T) {
	storeErr := errors.New("disk failure")
	tm := NewDefaultTrustManager(&errTrustStore{loadErr: storeErr})
	assert.False(t, tm.IsTrusted(context.Background(), "/repo/a"), "store error must resolve to deny")
}

func TestTrustProject_StoreAddError(t *testing.T) {
	addErr := errors.New("write failed")
	tm := NewDefaultTrustManager(&errTrustStore{addErr: addErr})
	err := tm.TrustProject(context.Background(), "/repo/a")
	require.Error(t, err)
	assert.Equal(t, addErr, err)
}

func TestRevokeTrust_StoreRemoveError(t *testing.T) {
	removeErr := errors.New("remove failed")
	tm := NewDefaultTrustManager(&errTrustStore{removeErr: removeErr})
	err := tm.RevokeTrust(context.Background(), "/repo/a")
	require.Error(t, err)
	assert.Equal(t, removeErr, err)
}

func TestTrustedProjects_StoreLoadError(t *testing.T) {
	storeErr := errors.New("disk failure")
	tm := NewDefaultTrustManager(&errTrustStore{loadErr: storeErr})
	assert.Nil(t, tm.TrustedProjects(), "store error must yield nil")
}

func TestContentFingerprint_PathFallback(t *testing.T) {
	// When the file does not exist, contentFingerprint falls back to hashing
	// the path string.
	fp1 := contentFingerprint("/repo/a")
	fp2 := contentFingerprint("/repo/a")
	assert.Equal(t, fp1, fp2, "same input must yield same fingerprint")
	assert.Len(t, fp1, 64, "SHA-256 hex must be 64 characters")

	fp3 := contentFingerprint("/repo/b")
	assert.NotEqual(t, fp1, fp3, "different input must yield different fingerprint")
}

func TestContentFingerprint_BasedOnContent(t *testing.T) {
	// Two different paths with the same content must yield the same fingerprint.
	dir := t.TempDir()
	path1 := filepath.Join(dir, "config1.json")
	path2 := filepath.Join(dir, "config2.json")
	content := []byte(`{"servers":[]}`)
	require.NoError(t, os.WriteFile(path1, content, 0o600))
	require.NoError(t, os.WriteFile(path2, content, 0o600))

	fp1 := contentFingerprint(path1)
	fp2 := contentFingerprint(path2)
	assert.Equal(t, fp1, fp2, "same content must yield same fingerprint regardless of path")

	// Change content of path2 — fingerprint must differ.
	require.NoError(t, os.WriteFile(path2, []byte(`{"servers":["x"]}`), 0o600))
	fp3 := contentFingerprint(path2)
	assert.NotEqual(t, fp1, fp3, "different content must yield different fingerprint")
}

func TestFileTrustStore_SaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	store := NewFileTrustStore(path)

	entries := map[string]TrustEntry{
		"/repo/a": {Path: "/repo/a", Fingerprint: "abc", TrustedAt: time.Now().Format(time.RFC3339)},
		"/repo/b": {Path: "/repo/b", Fingerprint: "def", TrustedAt: time.Now().Format(time.RFC3339)},
	}
	require.NoError(t, store.Save(entries))

	reloaded := NewFileTrustStore(path)
	got, err := reloaded.Load()
	require.NoError(t, err)
	assert.Equal(t, entries, got)
}

func TestFileTrustStore_EmptyFileYieldsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	require.NoError(t, os.WriteFile(path, []byte{}, 0o600))

	store := NewFileTrustStore(path)
	entries, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestFileTrustStore_CorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	require.NoError(t, os.WriteFile(path, []byte("{invalid json"), 0o600))

	store := NewFileTrustStore(path)
	_, err := store.Load()
	require.Error(t, err, "corrupt JSON must return an error")
}

func TestInMemoryTrustStore_Save(t *testing.T) {
	store := NewInMemoryTrustStore()
	entries := map[string]TrustEntry{
		"/repo/x": {Path: "/repo/x", Fingerprint: "xyz"},
	}
	require.NoError(t, store.Save(entries))

	got, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, entries, got)
}
