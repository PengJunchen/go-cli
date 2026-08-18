package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AuthChain resolves API keys through a priority chain:
// CLI flag -> keychain -> auth.json -> environment variable -> models.json.
// Each source is consulted in order; the first non-empty key wins.
type AuthChain struct {
	mu      sync.RWMutex
	sources []AuthSource
	cliKeys map[string]string
}

// AuthSource is a single lookup step in the auth chain. Name identifies the
// source for logging; Lookup returns the API key for the given provider key,
// or an error (including a sentinel "not found" error) when unavailable.
type AuthSource struct {
	Name   string
	Lookup func(key string) (string, error)
}

// ErrAuthNotFound is returned by AuthSource.Lookup implementations when the
// provider has no key in that source.
var ErrAuthNotFound = errors.New("auth: key not found in source")

// authSourceCLI, authSourceKeychain, authSourceAuthFile, authSourceEnv,
// authSourceModels are the ordered names of the default auth sources.
const (
	authSourceCLI      = "cli-flag"
	authSourceKeychain = "keychain"
	authSourceAuthFile = "auth.json"
	authSourceEnv      = "env"
	authSourceModels   = "models.json"
)

// NewAuthChain returns an AuthChain with the default sources configured in
// priority order: CLI flag -> keychain -> auth.json -> environment variable ->
// models.json.
func NewAuthChain() *AuthChain {
	c := &AuthChain{cliKeys: make(map[string]string)}
	kc := NewKeychainSource()
	c.sources = []AuthSource{
		{Name: authSourceCLI, Lookup: c.lookupCLIFlag},
		{Name: authSourceKeychain, Lookup: kc.Lookup},
		{Name: authSourceAuthFile, Lookup: lookupAuthFile},
		{Name: authSourceEnv, Lookup: lookupEnv},
		{Name: authSourceModels, Lookup: lookupModelsJSON},
	}
	slog.Info("config.auth.init", "op", "config.auth.init", "sources", len(c.sources))
	return c
}

// WithCLIFlag injects a CLI-provided API key for provider. This is the highest
// priority source. It is chainable.
func (c *AuthChain) WithCLIFlag(provider, key string) *AuthChain {
	c.SetCLIFlag(provider, key)
	return c
}

// SetCLIFlag stores a CLI-provided API key for provider.
func (c *AuthChain) SetCLIFlag(provider, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cliKeys[strings.ToLower(provider)] = key
}

// WithSources replaces the source chain. Intended for testing.
func (c *AuthChain) WithSources(sources []AuthSource) *AuthChain {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sources = append([]AuthSource(nil), sources...)
	return c
}

// Resolve returns the first API key found for provider across the source
// chain. It returns an error when every source fails.
func (c *AuthChain) Resolve(provider string) (string, error) {
	c.mu.RLock()
	sources := c.sources
	c.mu.RUnlock()

	key := strings.ToLower(provider)
	for _, src := range sources {
		val, err := src.Lookup(key)
		if err == nil && val != "" {
			slog.Info("config.auth.resolve",
				"op", "config.auth.resolve",
				"provider", provider,
				"source", src.Name,
				"found", true,
			)
			return val, nil
		}
		slog.Debug("config.auth.resolve",
			"op", "config.auth.resolve",
			"provider", provider,
			"source", src.Name,
			"found", false,
		)
	}
	return "", fmt.Errorf("auth: no API key found for provider %q", provider)
}

// lookupCLIFlag reads the CLI-provided keys injected via SetCLIFlag.
func (c *AuthChain) lookupCLIFlag(provider string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.cliKeys[provider]; ok && v != "" {
		return v, nil
	}
	return "", ErrAuthNotFound
}

// lookupAuthFile reads the API key for provider from
// ~/.config/go-cli/auth.json. The file is expected to have 0600 permissions
// and may be either a single-key object {"api_key": "..."} (written by the
// onboarding wizard) or a JSON object mapping provider names to API keys for
// backward compatibility.
func lookupAuthFile(provider string) (string, error) {
	path := authFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("config.auth.file_read_error", "path", path, "err", err)
		}
		return "", ErrAuthNotFound
	}
	// Try the single-key {"api_key": "..."} format first.
	var single struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(data, &single); err == nil && single.APIKey != "" {
		return single.APIKey, nil
	}
	// Fall back to a provider -> key map.
	var m map[string]string
	if err := json.Unmarshal(data, &m); err == nil {
		// Case-insensitive lookup to match provider names regardless of casing.
		for k, v := range m {
			if strings.EqualFold(k, provider) && v != "" {
				return v, nil
			}
		}
	}
	return "", ErrAuthNotFound
}

// lookupEnv maps the provider to candidate environment variable names and
// returns the first non-empty value.
func lookupEnv(provider string) (string, error) {
	for _, name := range envVarNames(provider) {
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
	}
	return "", ErrAuthNotFound
}

// lookupModelsJSON reads the API key for provider from
// ~/.config/go-cli/models.json. The file may either be a flat map of provider
// -> key or a nested map of provider -> {"api_key": "..."}.
func lookupModelsJSON(provider string) (string, error) {
	path := modelsConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("config.auth.models_read_error", "path", path, "err", err)
		}
		return "", ErrAuthNotFound
	}

	// Try flat map[string]string first.
	var flat map[string]string
	if err := json.Unmarshal(data, &flat); err == nil {
		for k, v := range flat {
			if strings.EqualFold(k, provider) && v != "" {
				return v, nil
			}
		}
	}

	// Fall back to nested map[string]map[string]any.
	var nested map[string]map[string]any
	if err := json.Unmarshal(data, &nested); err == nil {
		for k, entry := range nested {
			if strings.EqualFold(k, provider) {
				if v, ok := entry["api_key"].(string); ok && v != "" {
					return v, nil
				}
			}
		}
	}
	return "", ErrAuthNotFound
}

// envVarNames returns candidate environment variable names for a provider,
// ordered from most specific to the generic fallback.
func envVarNames(provider string) []string {
	upper := strings.ToUpper(strings.ReplaceAll(provider, "-", "_"))
	names := []string{upper + "_API_KEY"}
	switch provider {
	case "openai":
		names = append(names, "OPENAI_API_KEY")
	case "anthropic":
		names = append(names, "ANTHROPIC_API_KEY")
	case "google", "gemini":
		names = append(names, "GOOGLE_API_KEY", "GEMINI_API_KEY")
	case "azure":
		names = append(names, "AZURE_OPENAI_API_KEY")
	case "mistral":
		names = append(names, "MISTRAL_API_KEY")
	case "cohere":
		names = append(names, "COHERE_API_KEY")
	}
	return names
}

// authFilePath returns the location of auth.json under the user config dir.
func authFilePath() string {
	return configFilePath("auth.json")
}

// modelsConfigPath returns the location of models.json under the user config
// dir.
func modelsConfigPath() string {
	return configFilePath("models.json")
}

// configFilePath returns ~/.config/go-cli/<name>.
func configFilePath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "go-cli", name)
	}
	return filepath.Join(home, ".config", "go-cli", name)
}
