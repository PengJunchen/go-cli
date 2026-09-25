package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupScopedAuthFileRejectsInsecureOrCorruptFiles(t *testing.T) {
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	const secret = "credential-secret-canary"
	cases := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "overbroad file",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte(`{"credentials":[{"api_key":"`+secret+`"}]}`), 0o644))
				require.NoError(t, os.Chmod(path, 0o644))
			},
		},
		{
			name: "non regular file",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.Mkdir(path, 0o700))
			},
		},
		{
			name: "corrupt json",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte(`{"credentials":[{"api_key":"`+secret+`"`), 0o600))
			},
		},
		{
			name: "overbroad parent",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte(`{"credentials":[]}`), 0o600))
				require.NoError(t, os.Chmod(filepath.Dir(path), 0o755))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0o700))
			path := filepath.Join(dir, "auth.json")
			tc.setup(t, path)

			key, err := lookupScopedAuthFilePath(path, scope)
			require.Error(t, err)
			assert.False(t, errors.Is(err, ErrAuthNotFound), "invalid auth files must not be treated as absent")
			assert.Empty(t, key)
			assert.NotContains(t, err.Error(), secret)
		})
	}
}

func TestLookupScopedAuthFileMissingPathIsNotFound(t *testing.T) {
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}

	_, err := lookupScopedAuthFilePath(filepath.Join(t.TempDir(), "missing.json"), scope)
	require.ErrorIs(t, err, ErrAuthNotFound)
	_, err = lookupScopedAuthFilePath("", scope)
	require.ErrorIs(t, err, ErrAuthNotFound)
}

func TestLookupScopedAuthFileRejectsSymlinks(t *testing.T) {
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	targetDir := filepath.Join(root, "target")
	require.NoError(t, os.Mkdir(targetDir, 0o700))
	targetFile := filepath.Join(targetDir, "auth.json")
	const secret = "symlink-secret-canary"
	require.NoError(t, os.WriteFile(targetFile, []byte(`{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"`+secret+`"}]}`), 0o600))

	t.Run("final auth file symlink", func(t *testing.T) {
		path := filepath.Join(root, "auth-link.json")
		require.NoError(t, os.Symlink(targetFile, path))
		key, err := lookupScopedAuthFilePath(path, scope)
		require.Error(t, err)
		assert.Empty(t, key)
		assert.False(t, errors.Is(err, ErrAuthNotFound))
		assert.NotContains(t, err.Error(), secret)
	})

	t.Run("parent directory symlink", func(t *testing.T) {
		parentLink := filepath.Join(root, "parent-link")
		require.NoError(t, os.Symlink(targetDir, parentLink))
		key, err := lookupScopedAuthFilePath(filepath.Join(parentLink, "auth.json"), scope)
		require.Error(t, err)
		assert.Empty(t, key)
		assert.False(t, errors.Is(err, ErrAuthNotFound))
		assert.NotContains(t, err.Error(), secret)
	})
}

func TestLookupAuthFileRejectsCorruptAndOverbroadFiles(t *testing.T) {
	const secret = "legacy-secret-canary"
	cases := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{name: "corrupt json", data: []byte(`{"openai":"` + secret), mode: 0o600},
		{name: "overbroad file", data: []byte(`{"openai":"` + secret + `"}`), mode: 0o644},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0o700))
			path := filepath.Join(dir, "auth.json")
			require.NoError(t, os.WriteFile(path, tc.data, tc.mode))
			require.NoError(t, os.Chmod(path, tc.mode))

			key, err := lookupAuthFileAt(path, "openai")
			require.Error(t, err)
			assert.False(t, errors.Is(err, ErrAuthNotFound), "invalid auth files must be reported as source errors")
			assert.Empty(t, key)
			assert.NotContains(t, err.Error(), secret)
		})
	}
}

func TestResolveCredentialStopsAfterCorruptAuthFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	path := filepath.Join(dir, "auth.json")
	const secret = "credential-secret-canary"
	require.NoError(t, os.WriteFile(path, []byte(`{"credentials":[{"api_key":"`+secret+`"`), 0o600))
	var fallbackCalls int
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	c := NewAuthChain().WithSources([]AuthSource{
		{
			Name: authSourceAuthFile,
			LookupScoped: func(scope AuthScope) (string, error) {
				return lookupScopedAuthFilePath(path, scope)
			},
		},
		{
			Name: "fallback",
			LookupScoped: func(AuthScope) (string, error) {
				fallbackCalls++
				return "fallback-key", nil
			},
		},
	})

	key, found, err := c.ResolveCredential(context.Background(), scope.ProviderID, scope.ProtocolID, scope.Origin)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrAuthNotFound), "corrupt auth must be reported as a source error")
	assert.Empty(t, key)
	assert.False(t, found)
	assert.Zero(t, fallbackCalls, "a corrupt auth file must stop lower-priority credential sources")
	assert.NotContains(t, err.Error(), secret)
}

func TestAuthFilePathWithoutHomeIsUnavailable(t *testing.T) {
	t.Setenv("HOME", "")
	path := authFilePath()
	assert.True(t, path == "" || filepath.IsAbs(path), "missing HOME must not resolve auth.json relative to the project directory")
}

func TestResolveCredentialContinuesWhenAuthFileIsUnavailable(t *testing.T) {
	var fallbackCalls int
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	c := NewAuthChain().WithSources([]AuthSource{
		{
			Name: authSourceAuthFile,
			LookupScoped: func(scope AuthScope) (string, error) {
				return lookupScopedAuthFilePath("", scope)
			},
		},
		{
			Name: "fallback",
			LookupScoped: func(AuthScope) (string, error) {
				fallbackCalls++
				return "fallback-key", nil
			},
		},
	})

	key, found, err := c.ResolveCredential(context.Background(), scope.ProviderID, scope.ProtocolID, scope.Origin)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "fallback-key", key)
	assert.Equal(t, 1, fallbackCalls)
}
