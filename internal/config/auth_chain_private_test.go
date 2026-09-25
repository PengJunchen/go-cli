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

func TestLookupAuthFileRejectsDefaultConfigDirectorySymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	external := t.TempDir()
	authDir := filepath.Join(external, "go-cli")
	require.NoError(t, os.Mkdir(authDir, 0o700))
	const secret = "symlink-secret-canary"
	require.NoError(t, os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"openai":"`+secret+`"}`), 0o600))
	require.NoError(t, os.Symlink(external, filepath.Join(home, ".config")))

	key, err := lookupAuthFile("openai")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrAuthNotFound))
	assert.Empty(t, key)
	assert.NotContains(t, err.Error(), secret)
}

func TestLookupScopedModelsJSONUsesNoProjectRelativePathWithoutHome(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv("HOME", "")
	modelsDir := filepath.Join(workDir, ".config", "go-cli")
	require.NoError(t, os.MkdirAll(modelsDir, 0o700))
	const secret = "project-relative-secret"
	data := []byte(`{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"` + secret + `"}]}`)
	require.NoError(t, os.WriteFile(filepath.Join(modelsDir, "models.json"), data, 0o644))

	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	var fallbackCalls int
	c := NewAuthChain().WithSources([]AuthSource{
		{Name: authSourceModels, LookupScoped: lookupScopedModelsJSON},
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
	assert.NotEqual(t, secret, key)
	assert.Equal(t, 1, fallbackCalls)
}

func TestLookupScopedModelsJSONReadAndParseFailuresAreSourceErrors(t *testing.T) {
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	cases := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "read error",
			setup: func(t *testing.T, path string) {
				parent := filepath.Dir(path)
				require.NoError(t, os.Remove(parent))
				require.NoError(t, os.WriteFile(parent, []byte("not a directory"), 0o600))
			},
		},
		{
			name: "non regular file",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.Mkdir(path, 0o755))
			},
		},
		{
			name: "dangling symlink",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.Symlink(filepath.Join(filepath.Dir(path), "missing-target"), path))
			},
		},
		{
			name: "invalid json",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte("{"), 0o644))
			},
		},
		{
			name: "wrong credentials shape",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte(`{"credentials":"not-an-array"}`), 0o644))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			modelsDir := filepath.Join(home, ".config", "go-cli")
			require.NoError(t, os.MkdirAll(modelsDir, 0o700))
			tc.setup(t, filepath.Join(modelsDir, "models.json"))

			key, err := lookupScopedModelsJSON(scope)
			require.Error(t, err)
			assert.False(t, errors.Is(err, ErrAuthNotFound), "a bad models credential source must stop fallback")
			assert.Empty(t, key)
		})
	}
}

func TestLookupScopedModelsJSONMetadataDoesNotRequirePrivatePermissions(t *testing.T) {
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	for _, document := range []string{
		`{"models":{"openai":{"name":"gpt-example"}}}`,
		`{"credentials":[]}`,
	} {
		t.Run(document, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			modelsDir := filepath.Join(home, ".config", "go-cli")
			require.NoError(t, os.MkdirAll(modelsDir, 0o755))
			require.NoError(t, os.Chmod(modelsDir, 0o755))
			path := filepath.Join(modelsDir, "models.json")
			require.NoError(t, os.WriteFile(path, []byte(document), 0o644))
			require.NoError(t, os.Chmod(path, 0o644))

			key, err := lookupScopedModelsJSON(scope)
			require.ErrorIs(t, err, ErrAuthNotFound)
			assert.Empty(t, key)
		})
	}
}

func TestLookupScopedModelsJSONCredentialRequiresPrivateFileAndPath(t *testing.T) {
	const secret = "models-secret-canary"
	data := []byte(`{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"` + secret + `"}]}`)
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	cases := []struct {
		name  string
		setup func(t *testing.T, home, path string)
	}{
		{
			name: "wide file",
			setup: func(t *testing.T, home, path string) {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, data, 0o644))
				require.NoError(t, os.Chmod(path, 0o644))
			},
		},
		{
			name: "wide parent",
			setup: func(t *testing.T, home, path string) {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.Chmod(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, data, 0o600))
			},
		},
		{
			name: "default config directory symlink",
			setup: func(t *testing.T, home, path string) {
				external := t.TempDir()
				modelsDir := filepath.Join(external, "go-cli")
				require.NoError(t, os.Mkdir(modelsDir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(modelsDir, "models.json"), data, 0o600))
				require.NoError(t, os.Symlink(external, filepath.Join(home, ".config")))
			},
		},
		{
			name: "app directory symlink",
			setup: func(t *testing.T, home, path string) {
				configDir := filepath.Join(home, ".config")
				require.NoError(t, os.Mkdir(configDir, 0o700))
				external := t.TempDir()
				require.NoError(t, os.Chmod(external, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(external, "models.json"), data, 0o600))
				require.NoError(t, os.Symlink(external, filepath.Join(configDir, "go-cli")))
			},
		},
		{
			name: "models file symlink",
			setup: func(t *testing.T, home, path string) {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				target := filepath.Join(filepath.Dir(path), "target.json")
				require.NoError(t, os.WriteFile(target, data, 0o600))
				require.NoError(t, os.Symlink(target, path))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, ".config", "go-cli", "models.json")
			tc.setup(t, home, path)

			key, err := lookupScopedModelsJSON(scope)
			require.Error(t, err)
			assert.False(t, errors.Is(err, ErrAuthNotFound))
			assert.Empty(t, key)
			assert.NotContains(t, err.Error(), secret)
		})
	}
}

func TestLookupScopedModelsJSONReadsCredentialFromPrivateFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	modelsDir := filepath.Join(home, ".config", "go-cli")
	require.NoError(t, os.MkdirAll(modelsDir, 0o700))
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	data := []byte(`{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"models-secret"}]}`)
	path := filepath.Join(modelsDir, "models.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	key, err := lookupScopedModelsJSON(scope)
	require.NoError(t, err)
	assert.Equal(t, "models-secret", key)
}

func TestLookupScopedCredentialDataLeavesLegacyDocumentsUnmatched(t *testing.T) {
	scope, err := normalizeAuthScope(AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"})
	require.NoError(t, err)
	for _, data := range []string{
		`{"openai":"legacy-key"}`,
		`{"openai":{"api_key":"legacy-key","model":"gpt-example"}}`,
		`{"api_key":"unscoped-key"}`,
	} {
		key, err := lookupScopedCredentialData([]byte(data), scope)
		require.ErrorIs(t, err, ErrAuthNotFound)
		assert.Empty(t, key)
	}
}

func TestLookupScopedModelsJSONMissingFileIsNotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	_, err := lookupScopedModelsJSON(scope)
	require.ErrorIs(t, err, ErrAuthNotFound)
}

func TestLookupScopedCredentialDataRejectsMalformedDocumentsAndRecords(t *testing.T) {
	scope, err := normalizeAuthScope(AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"})
	require.NoError(t, err)
	const secret = "credential-secret-canary"
	cases := []struct {
		name string
		data string
	}{
		{name: "top level null", data: `null`},
		{name: "credentials null", data: `{"credentials":null}`},
		{name: "credentials wrong shape", data: `{"credentials":{}}`},
		{name: "null record", data: `{"credentials":[null]}`},
		{name: "missing scope fields", data: `{"credentials":[{"provider":"openai","api_key":"` + secret + `"}]}`},
		{name: "invalid scope", data: `{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"file:///tmp","api_key":"` + secret + `"}]}`},
		{name: "missing key", data: `{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test"}]}`},
		{name: "empty key", data: `{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":""}]}`},
		{name: "whitespace key", data: `{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"   "}]}`},
		{name: "matched record followed by invalid record", data: `{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"` + secret + `"},null]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := lookupScopedCredentialData([]byte(tc.data), scope)
			require.Error(t, err)
			assert.False(t, errors.Is(err, ErrAuthNotFound))
			assert.Empty(t, key)
			assert.NotContains(t, err.Error(), secret)
		})
	}
}

func TestLookupScopedCredentialDataRejectsDuplicateMembers(t *testing.T) {
	scope, err := normalizeAuthScope(AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"})
	require.NoError(t, err)
	const secret = "duplicate-member-secret"
	validRecord := `{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"` + secret + `"}`
	cases := []struct {
		name string
		data string
	}{
		{name: "duplicate credentials", data: `{"credentials":[` + validRecord + `],"credentials":[]}`},
		{name: "case alias credentials", data: `{"credentials":[],"Credentials":[]}`},
		{name: "duplicate provider", data: `{"credentials":[{"provider":"openai","provider":"anthropic","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"` + secret + `"}]}`},
		{name: "duplicate protocol", data: `{"credentials":[{"provider":"openai","protocol":"openai-compatible","Protocol":"anthropic","origin":"https://api.example.test","api_key":"` + secret + `"}]}`},
		{name: "duplicate origin", data: `{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","Origin":"https://other.example.test","api_key":"` + secret + `"}]}`},
		{name: "duplicate api key", data: `{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"first-secret","API_KEY":"second-secret"}]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := lookupScopedCredentialData([]byte(tc.data), scope)
			require.Error(t, err)
			assert.False(t, errors.Is(err, ErrAuthNotFound))
			assert.Empty(t, key)
			assert.NotContains(t, err.Error(), secret)
		})
	}
}

func TestLookupScopedCredentialDataRejectsConflictingKeysForSameScope(t *testing.T) {
	scope, err := normalizeAuthScope(AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"})
	require.NoError(t, err)
	const firstSecret = "first-secret-canary"
	const secondSecret = "second-secret-canary"
	data := []byte(`{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"` + firstSecret + `"},{"provider":"OpenAI","protocol":"OPENAI-COMPATIBLE","origin":"https://API.EXAMPLE.TEST:443","api_key":"` + secondSecret + `"}]}`)

	key, err := lookupScopedCredentialData(data, scope)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrAuthNotFound))
	assert.Empty(t, key)
	assert.NotContains(t, err.Error(), firstSecret)
	assert.NotContains(t, err.Error(), secondSecret)
}

func TestLookupScopedCredentialDataAcceptsIdenticalDuplicateScope(t *testing.T) {
	scope, err := normalizeAuthScope(AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"})
	require.NoError(t, err)
	data := []byte(`{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"same-secret"},{"provider":"OpenAI","protocol":"OPENAI-COMPATIBLE","origin":"https://API.EXAMPLE.TEST:443","api_key":"same-secret"}]}`)

	key, err := lookupScopedCredentialData(data, scope)
	require.NoError(t, err)
	assert.Equal(t, "same-secret", key)
}

func TestLookupScopedCredentialDataAcceptsCaseInsensitiveCredentialsField(t *testing.T) {
	scope, err := normalizeAuthScope(AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"})
	require.NoError(t, err)
	data := []byte(`{"Credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"scoped-secret"}]}`)

	key, err := lookupScopedCredentialData(data, scope)
	require.NoError(t, err)
	assert.Equal(t, "scoped-secret", key)
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
