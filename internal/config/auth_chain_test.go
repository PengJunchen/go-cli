package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthChainResolveCLIFlagWins(t *testing.T) {
	c := NewAuthChain()
	c.SetCLIFlag("openai", "flag-key")
	c.WithSources([]AuthSource{
		{Name: authSourceCLI, Lookup: c.lookupCLIFlag},
		{Name: authSourceEnv, Lookup: func(string) (string, error) { return "env-key", nil }},
	})
	_, err := c.Resolve("openai")
	require.ErrorIs(t, err, ErrCredentialScopeRequired)
}

func TestAuthChainResolveEnvFallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")
	c := NewAuthChain()
	c.WithSources([]AuthSource{
		{Name: authSourceCLI, Lookup: c.lookupCLIFlag},
		{Name: authSourceEnv, Lookup: lookupEnv},
	})
	_, err := c.Resolve("openai")
	require.ErrorIs(t, err, ErrCredentialScopeRequired)
}

func TestAuthChainResolveNotFound(t *testing.T) {
	c := NewAuthChain()
	c.WithSources([]AuthSource{
		{Name: authSourceCLI, Lookup: c.lookupCLIFlag},
		{Name: authSourceEnv, Lookup: func(string) (string, error) { return "", ErrAuthNotFound }},
	})
	_, err := c.Resolve("nobody")
	require.ErrorIs(t, err, ErrCredentialScopeRequired)
}

func TestAuthChainResolveAuthFile(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath, mustJSON(t, map[string]string{"openai": "file-key"}), 0o600))

	c := NewAuthChain().WithSources([]AuthSource{
		{Name: authSourceAuthFile, Lookup: func(provider string) (string, error) {
			return readJSONKeyFile(authPath, provider)
		}},
	})
	_, err := c.Resolve("openai")
	require.ErrorIs(t, err, ErrCredentialScopeRequired)
}

func TestAuthChainResolveModelsJSONFlat(t *testing.T) {
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")
	require.NoError(t, os.WriteFile(modelsPath, mustJSON(t, map[string]string{"anthropic": "models-key"}), 0o600))

	c := NewAuthChain().WithSources([]AuthSource{
		{Name: authSourceModels, Lookup: func(provider string) (string, error) {
			return readJSONKeyFile(modelsPath, provider)
		}},
	})
	_, err := c.Resolve("anthropic")
	require.ErrorIs(t, err, ErrCredentialScopeRequired)
}

func TestAuthChainResolveModelsJSONNested(t *testing.T) {
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")
	nested := map[string]map[string]any{
		"google": {"api_key": "gmodels-key", "model": "gemini-pro"},
	}
	require.NoError(t, os.WriteFile(modelsPath, mustJSON(t, nested), 0o600))

	c := NewAuthChain().WithSources([]AuthSource{
		{Name: authSourceModels, Lookup: func(provider string) (string, error) {
			data, err := os.ReadFile(modelsPath)
			if err != nil {
				return "", ErrAuthNotFound
			}
			var m map[string]map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				return "", ErrAuthNotFound
			}
			if entry, ok := m[provider]; ok {
				if v, ok := entry["api_key"].(string); ok && v != "" {
					return v, nil
				}
			}
			return "", ErrAuthNotFound
		}},
	})
	_, err := c.Resolve("google")
	require.ErrorIs(t, err, ErrCredentialScopeRequired)
}

func TestEnvVarNames(t *testing.T) {
	assert.Contains(t, envVarNames("openai"), "OPENAI_API_KEY")
	assert.Contains(t, envVarNames("gemini"), "GOOGLE_API_KEY")
	assert.Contains(t, envVarNames("gemini"), "GEMINI_API_KEY")
	assert.Contains(t, envVarNames("my-provider"), "MY_PROVIDER_API_KEY")
}

func TestAuthChainSetCLIFlagCaseInsensitive(t *testing.T) {
	c := NewAuthChain()
	c.SetCLIFlag("OpenAI", "flag-key")
	c.WithSources([]AuthSource{
		{Name: authSourceCLI, Lookup: c.lookupCLIFlag},
	})
	_, err := c.Resolve("openai")
	require.ErrorIs(t, err, ErrCredentialScopeRequired)
}

func TestAuthChainProviderOnlyResolveFailsClosed(t *testing.T) {
	lookups := 0
	c := NewAuthChain().WithSources([]AuthSource{{
		Name: "legacy-provider-only",
		Lookup: func(string) (string, error) {
			lookups++
			return "unscoped-canary", nil
		},
	}})

	_, err := c.Resolve("openai")
	require.ErrorIs(t, err, ErrCredentialScopeRequired)
	assert.Zero(t, lookups, "provider-only lookup cannot bind an unscoped credential")
	assert.NotContains(t, err.Error(), "unscoped-canary")
}

func TestAuthChainResolveCredentialUsesExactScope(t *testing.T) {
	want := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test:443"}
	lookups := 0
	c := NewAuthChain().WithSources([]AuthSource{{
		Name: "scoped",
		LookupScoped: func(scope AuthScope) (string, error) {
			lookups++
			if scope == want {
				return "scoped-canary", nil
			}
			return "", ErrAuthNotFound
		},
	}})

	got, found, err := c.ResolveCredential(context.Background(), want.ProviderID, want.ProtocolID, want.Origin)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "scoped-canary", got)
	assert.Equal(t, 1, lookups)

	got, found, err = c.ResolveCredential(context.Background(), want.ProviderID, want.ProtocolID, "https://other.example.test:443")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, got)
	assert.Equal(t, 2, lookups)
}

func TestAuthScopeNormalizesEffectivePort(t *testing.T) {
	scope, err := normalizeAuthScope(AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test:0443"})
	require.NoError(t, err)
	assert.Equal(t, "https://api.example.test:443", scope.Origin)
}

func TestAuthChainScopedCLIFlagDoesNotCrossProviderOrOrigin(t *testing.T) {
	scope := AuthScope{ProviderID: "local", ProtocolID: "openai-compatible", Origin: "http://127.0.0.1:8080"}
	c := NewAuthChain().WithScopedCLIFlag(scope, "local-canary")
	c.WithSources([]AuthSource{{Name: authSourceCLI, LookupScoped: c.lookupScopedCLIFlag}})

	got, found, err := c.ResolveCredential(context.Background(), scope.ProviderID, scope.ProtocolID, scope.Origin)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "local-canary", got)

	got, found, err = c.ResolveCredential(context.Background(), "openai", scope.ProtocolID, scope.Origin)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, got)
	got, found, err = c.ResolveCredential(context.Background(), scope.ProviderID, scope.ProtocolID, "http://127.0.0.1:8081")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, got)
}

func TestAuthChainScopedAuthFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	authPath := filepath.Join(dir, "auth.json")
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test:443"}
	data := map[string]any{"credentials": []map[string]string{{
		"provider": scope.ProviderID,
		"protocol": scope.ProtocolID,
		"origin":   scope.Origin,
		"api_key":  "scoped-file-canary",
	}}}
	require.NoError(t, os.WriteFile(authPath, mustJSON(t, data), 0o600))

	c := NewAuthChain().WithSources([]AuthSource{{
		Name: authSourceAuthFile,
		LookupScoped: func(scope AuthScope) (string, error) {
			return lookupScopedAuthFilePath(authPath, scope)
		},
	}})
	got, found, err := c.ResolveCredential(context.Background(), scope.ProviderID, scope.ProtocolID, scope.Origin)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "scoped-file-canary", got)
	got, found, err = c.ResolveCredential(context.Background(), scope.ProviderID, scope.ProtocolID, "https://different.example.test:443")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, got)
}

func TestAuthChainScopedEnvironmentAndLegacyFallbackIsolation(t *testing.T) {
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	name, err := ScopedCredentialEnvName(scope)
	require.NoError(t, err)
	t.Setenv("OPENAI_API_KEY", "legacy-environment-canary")
	t.Setenv(name, "scoped-environment-canary")
	c := NewAuthChain().WithSources([]AuthSource{{
		Name:   authSourceEnv,
		Lookup: lookupEnv,
		LookupScoped: func(scope AuthScope) (string, error) {
			return lookupScopedEnv(scope)
		},
	}})

	got, found, err := c.ResolveCredential(context.Background(), scope.ProviderID, scope.ProtocolID, scope.Origin)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "scoped-environment-canary", got)

	t.Setenv(name, "")
	got, found, err = c.ResolveCredential(context.Background(), scope.ProviderID, scope.ProtocolID, scope.Origin)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, got)
}

// readJSONKeyFile is a small helper used by the file-based source tests to
// avoid depending on the real home directory.
func readJSONKeyFile(path, provider string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ErrAuthNotFound
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return "", ErrAuthNotFound
	}
	if v, ok := m[provider]; ok && v != "" {
		return v, nil
	}
	return "", ErrAuthNotFound
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
