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

func testProjectSnapshot(t *testing.T, root string, configBytes, shadowBytes []byte, members []string) *ProjectSourceSnapshot {
	t.Helper()
	snapshot, err := NewProjectSourceSnapshot(root,
		[]ProjectSourceFileInput{
			{ID: "config/.go-cli.yaml", RelativePath: ".go-cli.yaml", Present: true, Bytes: configBytes},
			{ID: "config/go-cli.json", RelativePath: "go-cli.json", Present: true, Bytes: shadowBytes},
			{ID: "config/.go-cli.yml", RelativePath: ".go-cli.yml", Present: false},
		},
		[]ProjectSourceDirectoryInput{
			{ID: "skills/.go-cli/skills", RelativePath: ".go-cli/skills", Present: true, Members: members},
			{ID: "skills/optional", RelativePath: ".go-cli/optional", Present: false},
		},
	)
	require.NoError(t, err)
	return snapshot
}

func TestProjectSourceSnapshotCanonicalizesRootAndCopiesBytes(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "workspace-link")
	require.NoError(t, os.Symlink(root, alias))
	original := []byte("before")
	snapshot := testProjectSnapshot(t, alias, original, []byte("shadow"), []string{"a/SKILL.md"})
	original[0] = 'X'

	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	assert.Equal(t, canonicalRoot, snapshot.CanonicalRoot())
	bytes, ok := snapshot.SourceBytes("config/.go-cli.yaml")
	require.True(t, ok)
	assert.Equal(t, []byte("before"), bytes)
	bytes[0] = 'Y'
	bytesAgain, _ := snapshot.SourceBytes("config/.go-cli.yaml")
	assert.Equal(t, []byte("before"), bytesAgain)

	members, ok := snapshot.DirectoryMembers("skills/.go-cli/skills")
	require.True(t, ok)
	members[0] = "changed"
	membersAgain, _ := snapshot.DirectoryMembers("skills/.go-cli/skills")
	assert.Equal(t, []string{"a/SKILL.md"}, membersAgain)
}

func TestProjectSourceSnapshotBindsPresenceShadowedBytesAndMembership(t *testing.T) {
	root := t.TempDir()
	base := testProjectSnapshot(t, root, []byte("winner"), []byte("shadow-one"), []string{"a/SKILL.md", "b/SKILL.md"})
	changedShadow := testProjectSnapshot(t, root, []byte("winner"), []byte("shadow-two"), []string{"a/SKILL.md", "b/SKILL.md"})
	changedMembership := testProjectSnapshot(t, root, []byte("winner"), []byte("shadow-one"), []string{"a/SKILL.md", "c/SKILL.md"})
	changedPresence, err := NewProjectSourceSnapshot(root,
		[]ProjectSourceFileInput{
			{ID: "config/.go-cli.yaml", RelativePath: ".go-cli.yaml", Present: true, Bytes: []byte("winner")},
			{ID: "config/go-cli.json", RelativePath: "go-cli.json", Present: true, Bytes: []byte("shadow-one")},
			{ID: "config/.go-cli.yml", RelativePath: ".go-cli.yml", Present: true, Bytes: nil},
		},
		[]ProjectSourceDirectoryInput{{ID: "skills/.go-cli/skills", RelativePath: ".go-cli/skills", Present: true, Members: []string{"a/SKILL.md", "b/SKILL.md"}}},
	)
	require.NoError(t, err)
	require.NotEqual(t, base.ManifestDigest(), changedShadow.ManifestDigest())
	require.NotEqual(t, base.ManifestDigest(), changedMembership.ManifestDigest())
	require.NotEqual(t, base.ManifestDigest(), changedPresence.ManifestDigest())

	reordered := testProjectSnapshot(t, root, []byte("winner"), []byte("shadow-one"), []string{"b/SKILL.md", "a/SKILL.md"})
	assert.Equal(t, base.ManifestDigest(), reordered.ManifestDigest(), "input member ordering must not affect manifest identity")
}

func TestProjectSourceSnapshotRejectsUnsafeInputs(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name  string
		files []ProjectSourceFileInput
		dirs  []ProjectSourceDirectoryInput
	}{
		{name: "absolute source path", files: []ProjectSourceFileInput{{ID: "x", RelativePath: filepath.Join(root, "file"), Present: true}}},
		{name: "escaping source path", files: []ProjectSourceFileInput{{ID: "x", RelativePath: "../outside", Present: true}}},
		{name: "absent with bytes", files: []ProjectSourceFileInput{{ID: "x", RelativePath: "file", Bytes: []byte("unexpected")}}},
		{name: "duplicate identity", files: []ProjectSourceFileInput{{ID: "x", RelativePath: "one"}, {ID: "x", RelativePath: "two"}}},
		{name: "escaping member", dirs: []ProjectSourceDirectoryInput{{ID: "d", RelativePath: "tree", Present: true, Members: []string{"../escape"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewProjectSourceSnapshot(root, tt.files, tt.dirs)
			require.Error(t, err)
		})
	}
}

func TestDefaultTrustManagerChecksPersistentProjectManifest(t *testing.T) {
	root := t.TempDir()
	store := NewInMemoryTrustStore()
	manager := NewDefaultTrustManager(store)
	frozen, ok := manager.(FrozenProjectSourceTrust)
	require.True(t, ok)

	snapshot := testProjectSnapshot(t, root, []byte("project config"), []byte("shadowed config"), []string{"skill/SKILL.md"})
	_, err := frozen.CheckProjectSourceSnapshot(context.Background(), snapshot)
	require.ErrorIs(t, err, ErrProjectSourceUntrusted)

	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), snapshot))
	verified, err := frozen.CheckProjectSourceSnapshot(context.Background(), snapshot)
	require.NoError(t, err)
	require.NotNil(t, verified)
	assert.True(t, verified.Valid())
	assert.Equal(t, snapshot.ManifestDigest(), verified.ManifestDigest())
	verifiedBytes := mustVerifiedBytes(t, verified, "config/.go-cli.yaml")
	assert.Equal(t, []byte("project config"), verifiedBytes)
	verifiedBytes[0] = 'X'
	assert.Equal(t, []byte("project config"), mustVerifiedBytes(t, verified, "config/.go-cli.yaml"))
	verifiedMembers, ok := verified.DirectoryMembers("skills/.go-cli/skills")
	require.True(t, ok)
	verifiedMembers[0] = "changed"
	verifiedMembersAgain, ok := verified.DirectoryMembers("skills/.go-cli/skills")
	require.True(t, ok)
	assert.Equal(t, []string{"skill/SKILL.md"}, verifiedMembersAgain)

	changed := testProjectSnapshot(t, root, []byte("project config"), []byte("edited shadow"), []string{"skill/SKILL.md"})
	_, err = frozen.CheckProjectSourceSnapshot(context.Background(), changed)
	require.ErrorIs(t, err, ErrProjectSourceManifestMismatch)

	require.NoError(t, manager.RevokeTrust(context.Background(), root))
	_, err = frozen.CheckProjectSourceSnapshot(context.Background(), snapshot)
	require.ErrorIs(t, err, ErrProjectSourceUntrusted)
}

func TestProjectSourceTrustRejectsLegacyExpiredAndCanceledChecks(t *testing.T) {
	root := t.TempDir()
	snapshot := testProjectSnapshot(t, root, []byte("config"), nil, nil)
	store := NewInMemoryTrustStore()
	manager := NewDefaultTrustManager(store)
	frozen := manager.(FrozenProjectSourceTrust)
	canonical := snapshot.CanonicalRoot()

	require.NoError(t, store.Add(canonical, TrustEntry{Path: canonical, Fingerprint: "legacy"}))
	_, err := frozen.CheckProjectSourceSnapshot(context.Background(), snapshot)
	require.ErrorIs(t, err, ErrProjectSourceLegacy)

	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), snapshot))
	entries, err := store.Load()
	require.NoError(t, err)
	entry := entries[canonical]
	entry.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	require.NoError(t, store.Add(canonical, entry))
	_, err = frozen.CheckProjectSourceSnapshot(context.Background(), snapshot)
	require.ErrorIs(t, err, ErrProjectSourceExpired)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = frozen.CheckProjectSourceSnapshot(ctx, snapshot)
	require.True(t, errors.Is(err, context.Canceled))
}

func TestProjectSourceManifestPersistsAcrossTrustManagers(t *testing.T) {
	root := t.TempDir()
	privateDir := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.Mkdir(privateDir, 0o700))
	storePath := filepath.Join(privateDir, "trust.json")
	snapshot := testProjectSnapshot(t, root, []byte("captured configuration"), []byte("shadowed"), []string{"skills/a/SKILL.md"})
	first := NewDefaultTrustManager(NewFileTrustStore(storePath))
	frozen := first.(FrozenProjectSourceTrust)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), snapshot))

	second := NewDefaultTrustManager(NewFileTrustStore(storePath))
	verified, err := second.(FrozenProjectSourceTrust).CheckProjectSourceSnapshot(context.Background(), snapshot)
	require.NoError(t, err)
	require.NotNil(t, verified)
	assert.Equal(t, snapshot.ManifestDigest(), verified.ManifestDigest())
	assert.Equal(t, []byte("captured configuration"), mustVerifiedBytes(t, verified, "config/.go-cli.yaml"))

	persisted, err := os.ReadFile(storePath)
	require.NoError(t, err)
	assert.Contains(t, string(persisted), `"manifest_version":1`)
	assert.Contains(t, string(persisted), snapshot.ManifestDigest())
	assert.NotContains(t, string(persisted), "captured configuration", "the trust store persists digests, not source contents")
}

func TestProjectSourceStoreCorruptionFailsClosed(t *testing.T) {
	root := t.TempDir()
	snapshot := testProjectSnapshot(t, root, []byte("config"), nil, nil)
	storeDir := t.TempDir()
	require.NoError(t, os.Chmod(storeDir, 0o700))
	storePath := filepath.Join(storeDir, "trust.json")
	require.NoError(t, os.WriteFile(storePath, []byte("{"), 0o600))
	manager := NewDefaultTrustManager(NewFileTrustStore(storePath))
	verified, err := manager.(FrozenProjectSourceTrust).CheckProjectSourceSnapshot(context.Background(), snapshot)
	require.Error(t, err)
	assert.Nil(t, verified)
}

func TestVerifiedProjectSourcesZeroValueHasNoAuthority(t *testing.T) {
	var forged VerifiedProjectSources
	assert.False(t, forged.Valid())
	_, ok := forged.SourceBytes("config/.go-cli.yaml")
	assert.False(t, ok)
	_, ok = forged.DirectoryMembers("skills/.go-cli/skills")
	assert.False(t, ok)
	assert.Empty(t, forged.CanonicalRoot())
}

func mustVerifiedBytes(t *testing.T, verified *VerifiedProjectSources, id string) []byte {
	t.Helper()
	data, ok := verified.SourceBytes(id)
	require.True(t, ok)
	return data
}
