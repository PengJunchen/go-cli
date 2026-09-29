package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
)

func trustCommandConfig(t *testing.T, manager approval.TrustManager) *config.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.json"), []byte(`{"model":{"name":"project"}}`), 0o600))
	for _, name := range []string{"zeta", "alpha"} {
		dir := filepath.Join(root, ".go-cli", "skills", name)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("name: "+name+"\n"), 0o600))
	}
	t.Chdir(root)
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	require.NotNil(t, cfg.CapturedProjectSourcesForGrant())
	return cfg
}

func TestTrustCommandGrantRequiresTTYAndExplicitConfirmation(t *testing.T) {
	t.Run("rejects non tty", func(t *testing.T) {
		manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
		cfg := trustCommandConfig(t, manager)
		var out bytes.Buffer
		command := NewTrustCommand(manager, strings.NewReader("TRUST\n"), &out, func() bool { return false })

		err := command.Run(context.Background(), cfg, []string{"grant"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "interactive terminal")
		entries, loadErr := manager.(interface {
			LoadTrustEntries() (map[string]approval.TrustEntry, error)
		}).LoadTrustEntries()
		require.NoError(t, loadErr)
		assert.Empty(t, entries)
	})

	t.Run("decline does not persist", func(t *testing.T) {
		manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
		cfg := trustCommandConfig(t, manager)
		var out bytes.Buffer
		command := NewTrustCommand(manager, strings.NewReader("no\n"), &out, func() bool { return true })

		require.NoError(t, command.Run(context.Background(), cfg, []string{"grant"}))
		assert.Contains(t, out.String(), "Grant cancelled")
		entries, loadErr := manager.(interface {
			LoadTrustEntries() (map[string]approval.TrustEntry, error)
		}).LoadTrustEntries()
		require.NoError(t, loadErr)
		assert.Empty(t, entries)
	})
}

func TestTrustCommandGrantPersistsRawSnapshotWithoutGrantingCurrentConfig(t *testing.T) {
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	cfg := trustCommandConfig(t, manager)
	snapshot := cfg.CapturedProjectSourcesForGrant()
	var out bytes.Buffer
	command := NewTrustCommand(manager, strings.NewReader("TRUST\n"), &out, func() bool { return true })

	require.NoError(t, command.Run(context.Background(), cfg, []string{"grant"}))
	assert.Contains(t, out.String(), snapshot.CanonicalRoot())
	assert.Contains(t, out.String(), snapshot.ManifestDigest())
	assert.Contains(t, out.String(), "config/.go-cli.json")
	configCandidates := []string{
		"file config/.go-cli.yaml",
		"file config/.go-cli.yml",
		"file config/.go-cli.json",
		"file config/go-cli.yaml",
		"file config/go-cli.yml",
		"file config/go-cli.json",
	}
	previous := -1
	for _, candidate := range configCandidates {
		index := strings.Index(out.String(), candidate)
		require.Greater(t, index, previous, "trust summary must show config candidates in loader priority order")
		previous = index
	}
	assert.Contains(t, out.String(), "directory skills/.go-cli/skills")
	alphaMember := strings.Index(out.String(), ".go-cli/skills/alpha/SKILL.md")
	zetaMember := strings.Index(out.String(), ".go-cli/skills/zeta/SKILL.md")
	require.Greater(t, alphaMember, 0)
	require.Greater(t, zetaMember, alphaMember, "directory members must appear in sorted order")
	assert.Nil(t, cfg.VerifiedProjectSources(), "grant must not mint or attach authority to this invocation")
	entries, err := manager.(interface {
		LoadTrustEntries() (map[string]approval.TrustEntry, error)
	}).LoadTrustEntries()
	require.NoError(t, err)
	entry, ok := entries[snapshot.CanonicalRoot()]
	require.True(t, ok)
	assert.Equal(t, snapshot.ManifestVersion(), entry.ManifestVersion)
	assert.Equal(t, snapshot.ManifestDigest(), entry.ManifestDigest)
}

func TestTrustCommandListLabelsLegacyWithoutGrantingIt(t *testing.T) {
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	legacyRoot := t.TempDir()
	require.NoError(t, manager.TrustProject(context.Background(), legacyRoot))
	cfg := trustCommandConfig(t, manager)
	var out bytes.Buffer
	command := NewTrustCommand(manager, strings.NewReader(""), &out, func() bool { return false })

	require.NoError(t, command.Run(context.Background(), cfg, []string{"list"}))
	assert.Contains(t, out.String(), "legacy")
	assert.Contains(t, out.String(), legacyRoot)
	assert.NotContains(t, out.String(), "authorized")
}

func TestTrustCommandListPropagatesTrustStoreError(t *testing.T) {
	home := t.TempDir()
	storeDir := filepath.Join(home, ".config", "go-cli")
	require.NoError(t, os.MkdirAll(storeDir, 0o700))
	storePath := filepath.Join(storeDir, "trust.json")
	require.NoError(t, os.WriteFile(storePath, []byte("{bad"), 0o600))
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(storePath))
	cfg := trustCommandConfig(t, manager)
	command := NewTrustCommand(manager, strings.NewReader(""), ioDiscard{}, func() bool { return false })

	err := command.Run(context.Background(), cfg, []string{"list"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, approval.ErrProjectSourceManagerUnavailable) == false)
	assert.Contains(t, err.Error(), "load trust entries")
}

func TestTrustCommandRevokeRequiresCanonicalAbsoluteRootButNotTTY(t *testing.T) {
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.NoError(t, manager.TrustProject(context.Background(), canonicalRoot))
	cfg := trustCommandConfig(t, manager)
	var out bytes.Buffer
	command := NewTrustCommand(manager, strings.NewReader(""), &out, func() bool { return false })

	require.NoError(t, command.Run(context.Background(), cfg, []string{"revoke", canonicalRoot}))
	assert.Contains(t, out.String(), "Revoked")
	entries, err := manager.(interface {
		LoadTrustEntries() (map[string]approval.TrustEntry, error)
	}).LoadTrustEntries()
	require.NoError(t, err)
	assert.NotContains(t, entries, canonicalRoot)

	err = command.Run(context.Background(), cfg, []string{"revoke", "relative/path"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "absolute") || strings.Contains(err.Error(), "canonical"))

	canonicalParent, err := filepath.EvalSymlinks(filepath.Dir(canonicalRoot))
	require.NoError(t, err)
	removedRoot := filepath.Join(canonicalParent, "removed-project")
	require.NoError(t, manager.TrustProject(context.Background(), removedRoot))
	require.NoError(t, command.Run(context.Background(), cfg, []string{"revoke", removedRoot}), "a removed root remains revocable by its canonical path")

	alias := filepath.Join(t.TempDir(), "parent-alias")
	require.NoError(t, os.Symlink(canonicalParent, alias))
	aliasedRemovedRoot := filepath.Join(alias, "removed-project")
	require.NoError(t, manager.TrustProject(context.Background(), aliasedRemovedRoot))
	err = command.Run(context.Background(), cfg, []string{"revoke", aliasedRemovedRoot})
	require.Error(t, err, "a missing path under a symlinked parent is not a canonical root")
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
