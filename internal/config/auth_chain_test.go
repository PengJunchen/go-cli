package config

import (
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
	got, err := c.Resolve("openai")
	require.NoError(t, err)
	assert.Equal(t, "flag-key", got)
}

func TestAuthChainResolveEnvFallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")
	c := NewAuthChain()
	c.WithSources([]AuthSource{
		{Name: authSourceCLI, Lookup: c.lookupCLIFlag},
		{Name: authSourceEnv, Lookup: lookupEnv},
	})
	got, err := c.Resolve("openai")
	require.NoError(t, err)
	assert.Equal(t, "env-key", got)
}

func TestAuthChainResolveNotFound(t *testing.T) {
	c := NewAuthChain()
	c.WithSources([]AuthSource{
		{Name: authSourceCLI, Lookup: c.lookupCLIFlag},
		{Name: authSourceEnv, Lookup: func(string) (string, error) { return "", ErrAuthNotFound }},
	})
	_, err := c.Resolve("nobody")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nobody")
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
	got, err := c.Resolve("openai")
	require.NoError(t, err)
	assert.Equal(t, "file-key", got)
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
	got, err := c.Resolve("anthropic")
	require.NoError(t, err)
	assert.Equal(t, "models-key", got)
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
	got, err := c.Resolve("google")
	require.NoError(t, err)
	assert.Equal(t, "gmodels-key", got)
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
	got, err := c.Resolve("openai")
	require.NoError(t, err)
	assert.Equal(t, "flag-key", got)
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
