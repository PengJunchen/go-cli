package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// AuthChain resolves API keys through an ordered priority chain, but only
// sources that prove the full provider/protocol/origin scope are eligible.
type AuthChain struct {
	mu            sync.RWMutex
	sources       []AuthSource
	cliKeys       map[string]string
	scopedCLIKeys map[AuthScope]string
}

// AuthSource is a lookup step in the auth chain. Lookup is retained for legacy
// callers; resolver paths use only LookupScoped.
type AuthSource struct {
	Name         string
	Lookup       func(key string) (string, error)
	LookupScoped func(scope AuthScope) (string, error)
}

// AuthScope binds a credential to one provider, wire protocol, and normalized
// origin. Origin includes the effective port and excludes endpoint paths.
type AuthScope struct {
	ProviderID string
	ProtocolID string
	Origin     string
}

// ErrAuthNotFound is returned by AuthSource.Lookup implementations when the
// provider has no key in that source.
var ErrAuthNotFound = errors.New("auth: key not found in source")

// ErrCredentialScopeRequired is returned when an API asks for a credential
// using only a provider name. Provider identity alone is insufficient to bind
// a secret to its protocol and destination origin.
var ErrCredentialScopeRequired = errors.New("auth: provider, protocol, and origin scope required")

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
	c := &AuthChain{cliKeys: make(map[string]string), scopedCLIKeys: make(map[AuthScope]string)}
	kc := NewKeychainSource()
	c.sources = []AuthSource{
		{Name: authSourceCLI, Lookup: c.lookupCLIFlag, LookupScoped: c.lookupScopedCLIFlag},
		{Name: authSourceKeychain, Lookup: kc.Lookup, LookupScoped: kc.LookupScoped},
		{Name: authSourceAuthFile, Lookup: lookupAuthFile, LookupScoped: lookupScopedAuthFile},
		{Name: authSourceEnv, Lookup: lookupEnv, LookupScoped: lookupScopedEnv},
		{Name: authSourceModels, Lookup: lookupModelsJSON, LookupScoped: lookupScopedModelsJSON},
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

// WithScopedCLIFlag injects a CLI credential that is valid only for the exact
// provider/protocol/origin scope.
func (c *AuthChain) WithScopedCLIFlag(scope AuthScope, key string) *AuthChain {
	c.SetScopedCLIFlag(scope, key)
	return c
}

// SetScopedCLIFlag stores a CLI credential under its full scope.
func (c *AuthChain) SetScopedCLIFlag(scope AuthScope, key string) {
	normalized, err := normalizeAuthScope(scope)
	if err != nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scopedCLIKeys[normalized] = key
}

// WithSources replaces the source chain. Intended for testing.
func (c *AuthChain) WithSources(sources []AuthSource) *AuthChain {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sources = append([]AuthSource(nil), sources...)
	return c
}

// Resolve rejects provider-only lookups. Call ResolveCredential when the
// caller has a complete provider/protocol/origin scope.
func (c *AuthChain) Resolve(provider string) (string, error) {
	return "", fmt.Errorf("%w (provider %q is not enough)", ErrCredentialScopeRequired, provider)
}

// ResolveCredential returns the first key from a source that can prove the
// exact provider/protocol/origin scope. Legacy provider-only sources are
// deliberately skipped, even if they contain a matching provider key.
func (c *AuthChain) ResolveCredential(ctx context.Context, provider, protocol, origin string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	scope, err := normalizeAuthScope(AuthScope{ProviderID: provider, ProtocolID: protocol, Origin: origin})
	if err != nil {
		return "", false, err
	}
	c.mu.RLock()
	sources := append([]AuthSource(nil), c.sources...)
	c.mu.RUnlock()
	for _, src := range sources {
		if src.LookupScoped == nil {
			continue
		}
		val, lookupErr := src.LookupScoped(scope)
		if lookupErr == nil && val != "" {
			slog.Info("config.auth.resolve_scoped", "provider", scope.ProviderID, "protocol", scope.ProtocolID, "source", src.Name, "found", true)
			return val, true, nil
		}
		if lookupErr != nil && !errors.Is(lookupErr, ErrAuthNotFound) {
			return "", false, fmt.Errorf("auth: scoped source %q failed: %w", src.Name, lookupErr)
		}
		slog.Debug("config.auth.resolve_scoped", "provider", scope.ProviderID, "protocol", scope.ProtocolID, "source", src.Name, "found", false)
	}
	return "", false, nil
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

func (c *AuthChain) lookupScopedCLIFlag(scope AuthScope) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if value := c.scopedCLIKeys[scope]; value != "" {
		return value, nil
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

type scopedCredentialRecord struct {
	Provider string `json:"provider"`
	Protocol string `json:"protocol"`
	Origin   string `json:"origin"`
	APIKey   string `json:"api_key"`
}

// lookupScopedAuthFile accepts only explicit tuple records under a
// "credentials" array. Legacy single-key and provider-only formats remain
// readable by old tooling but are never candidates for scoped resolution.
func lookupScopedAuthFile(scope AuthScope) (string, error) {
	return lookupScopedCredentialFile(authFilePath(), scope)
}

func lookupScopedModelsJSON(scope AuthScope) (string, error) {
	return lookupScopedCredentialFile(modelsConfigPath(), scope)
}

func lookupScopedAuthFilePath(path string, scope AuthScope) (string, error) {
	return lookupScopedCredentialFile(path, scope)
}

func lookupScopedCredentialFile(path string, scope AuthScope) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ErrAuthNotFound
	}
	var document struct {
		Credentials []scopedCredentialRecord `json:"credentials"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return "", ErrAuthNotFound
	}
	for _, entry := range document.Credentials {
		entryScope, scopeErr := normalizeAuthScope(AuthScope{ProviderID: entry.Provider, ProtocolID: entry.Protocol, Origin: entry.Origin})
		if scopeErr == nil && entryScope == scope && entry.APIKey != "" {
			return entry.APIKey, nil
		}
	}
	return "", ErrAuthNotFound
}

// ScopedCredentialEnvName returns the deterministic environment variable name
// for an exact scope. The origin is represented by a short SHA-256 digest so
// punctuation and endpoint details do not become environment variable syntax.
func ScopedCredentialEnvName(scope AuthScope) (string, error) {
	normalized, err := normalizeAuthScope(scope)
	if err != nil {
		return "", err
	}
	tuple := normalized.ProviderID + "\x00" + normalized.ProtocolID + "\x00" + normalized.Origin
	digest := sha256.Sum256([]byte(tuple))
	return "GO_CLI_CREDENTIAL_" + hex.EncodeToString(digest[:]) + "_API_KEY", nil
}

func lookupScopedEnv(scope AuthScope) (string, error) {
	name, err := ScopedCredentialEnvName(scope)
	if err != nil {
		return "", ErrAuthNotFound
	}
	if value := os.Getenv(name); value != "" {
		return value, nil
	}
	return "", ErrAuthNotFound
}

func normalizeAuthScope(scope AuthScope) (AuthScope, error) {
	scope.ProviderID = strings.ToLower(strings.TrimSpace(scope.ProviderID))
	scope.ProtocolID = strings.ToLower(strings.TrimSpace(scope.ProtocolID))
	if scope.ProviderID == "" || scope.ProtocolID == "" {
		return AuthScope{}, ErrCredentialScopeRequired
	}
	u, err := url.Parse(strings.TrimSpace(scope.Origin))
	if err != nil || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return AuthScope{}, ErrCredentialScopeRequired
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return AuthScope{}, ErrCredentialScopeRequired
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	} else if value, err := strconv.Atoi(port); err != nil || value < 1 || value > 65535 {
		return AuthScope{}, ErrCredentialScopeRequired
	} else {
		port = strconv.Itoa(value)
	}
	scope.Origin = scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
	return scope, nil
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
