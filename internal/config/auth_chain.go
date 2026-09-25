package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/pengjunchen/go-cli/internal/privatefile"
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

var errAuthFileInvalid = errors.New("auth: credential file is invalid or insecure")
var errScopedCredentialSourceInvalid = errors.New("auth: scoped credential source is invalid")

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
	return lookupAuthFileAt(path, provider)
}

func lookupAuthFileAt(path, provider string) (string, error) {
	data, err := readPrivateAuthFile(path)
	if err != nil {
		if !errors.Is(err, ErrAuthNotFound) {
			slog.Warn("config.auth.file_read_error", "source", authSourceAuthFile, "err", err)
		}
		return "", err
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
		if m == nil {
			return "", errAuthFileInvalid
		}
		// Case-insensitive lookup to match provider names regardless of casing.
		for k, v := range m {
			if strings.EqualFold(k, provider) && v != "" {
				return v, nil
			}
		}
		return "", ErrAuthNotFound
	}
	return "", errAuthFileInvalid
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

type parsedScopedCredential struct {
	scope  AuthScope
	apiKey string
}

type scopedCredentialDocument struct {
	hasCredentials bool
	records        []parsedScopedCredential
}

// lookupScopedAuthFile accepts only explicit tuple records under a
// "credentials" array. Legacy single-key and provider-only formats remain
// readable by old tooling but are never candidates for scoped resolution.
func lookupScopedAuthFile(scope AuthScope) (string, error) {
	return lookupScopedAuthFilePath(authFilePath(), scope)
}

func lookupScopedModelsJSON(scope AuthScope) (string, error) {
	return lookupScopedCredentialFile(modelsConfigPath(), scope)
}

func lookupScopedAuthFilePath(path string, scope AuthScope) (string, error) {
	data, err := readPrivateAuthFile(path)
	if err != nil {
		return "", err
	}
	return lookupScopedCredentialData(data, scope)
}

func lookupScopedCredentialFile(path string, scope AuthScope) (string, error) {
	if path == "" {
		return "", ErrAuthNotFound
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if isMissingScopedCredentialFile(path, err) {
			return "", ErrAuthNotFound
		}
		return "", errScopedCredentialSourceInvalid
	}
	document, err := parseScopedCredentialDocument(data)
	if err != nil {
		return "", errScopedCredentialSourceInvalid
	}
	if len(document.records) != 0 {
		// models.json also carries public model metadata, so only its secret-bearing
		// form is subject to auth.json's private-file checks. Parse the strict read
		// snapshot, never the permissive metadata read above.
		strictData, err := readPrivateAuthFile(path)
		if err != nil {
			return "", err
		}
		document, err = parseScopedCredentialDocument(strictData)
		if err != nil {
			return "", errScopedCredentialSourceInvalid
		}
	}
	return lookupScopedCredentialDocument(document, scope)
}

// UpsertScopedCredentialFile writes one explicitly scoped credential to an
// auth file, preserving other valid scoped records. Legacy unscoped fields are
// not assigned a scope or copied into the scoped document.
func UpsertScopedCredentialFile(ctx context.Context, path string, scope AuthScope, apiKey string) error {
	if path == "" {
		return errors.New("auth: credential file path is empty")
	}
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("auth: scoped credential is empty")
	}
	normalizedScope, err := normalizeAuthScope(scope)
	if err != nil {
		return fmt.Errorf("auth: invalid scoped credential: %w", err)
	}
	return privatefile.WithExclusiveLock(ctx, path, func() error {
		return upsertScopedCredentialFileLocked(path, normalizedScope, apiKey)
	})
}

func upsertScopedCredentialFileLocked(path string, scope AuthScope, apiKey string) error {
	var currentDocument scopedCredentialDocument
	var currentRecords []json.RawMessage
	data, err := readAuthFileForUpdate(path)
	if err != nil {
		if !errors.Is(err, ErrAuthNotFound) {
			return err
		}
	} else {
		currentDocument, err = parseScopedCredentialDocument(data)
		if err != nil {
			return errScopedCredentialSourceInvalid
		}
		if currentDocument.hasCredentials {
			fields, err := decodeCredentialObject(data, "credentials")
			if err != nil || json.Unmarshal(fields["credentials"], &currentRecords) != nil || len(currentRecords) != len(currentDocument.records) {
				return errScopedCredentialSourceInvalid
			}
		}
	}

	newRecord, err := json.Marshal(struct { //nolint:gosec // G117: this intentionally writes scope-bound secrets through privatefile to 0600 auth.json.
		Provider string `json:"provider"`
		Protocol string `json:"protocol"`
		Origin   string `json:"origin"`
		APIKey   string `json:"api_key"`
	}{
		Provider: scope.ProviderID,
		Protocol: scope.ProtocolID,
		Origin:   scope.Origin,
		APIKey:   apiKey,
	})
	if err != nil {
		return fmt.Errorf("auth: encode scoped credential: %w", err)
	}
	updatedRecords := make([]json.RawMessage, 0, len(currentRecords)+1)
	replaced := false
	for i, existing := range currentDocument.records {
		if existing.scope == scope {
			if !replaced {
				updatedRecords = append(updatedRecords, newRecord)
				replaced = true
			}
			continue
		}
		updatedRecords = append(updatedRecords, currentRecords[i])
	}
	if !replaced {
		updatedRecords = append(updatedRecords, newRecord)
	}
	data, err = json.Marshal(struct {
		Credentials []json.RawMessage `json:"credentials"`
	}{Credentials: updatedRecords})
	if err != nil {
		return fmt.Errorf("auth: encode credential file: %w", err)
	}
	if err := privatefile.WriteAtomic(path, data); err != nil {
		return fmt.Errorf("auth: write credential file: %w", err)
	}
	return nil
}

// readAuthFileForUpdate permits a save to repair broad permissions on an
// existing regular file. Credential lookup remains stricter and rejects such
// a file; the caller holds the private sidecar lock and the replacement is
// committed with WriteAtomic's 0600 temporary file and rename.
func readAuthFileForUpdate(path string) ([]byte, error) {
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAuthNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: inspect credential file for update: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, errAuthFileInvalid
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("auth: open credential file for update: %w", err)
	}
	defer file.Close() //nolint:errcheck
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("auth: inspect open credential file: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return nil, errAuthFileInvalid
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("auth: read credential file for update: %w", err)
	}
	return data, nil
}

func lookupScopedCredentialData(data []byte, scope AuthScope) (string, error) {
	document, err := parseScopedCredentialDocument(data)
	if err != nil {
		return "", errScopedCredentialSourceInvalid
	}
	return lookupScopedCredentialDocument(document, scope)
}

func lookupScopedCredentialDocument(document scopedCredentialDocument, scope AuthScope) (string, error) {
	if !document.hasCredentials || len(document.records) == 0 {
		// Legacy provider-only models and auth files are valid JSON, but they do
		// not prove a complete credential scope.
		return "", ErrAuthNotFound
	}
	wantedScope, err := normalizeAuthScope(scope)
	if err != nil {
		return "", err
	}
	for _, record := range document.records {
		if record.scope == wantedScope {
			return record.apiKey, nil
		}
	}
	return "", ErrAuthNotFound
}

func parseScopedCredentialDocument(data []byte) (scopedCredentialDocument, error) {
	fields, err := decodeCredentialObject(data, "credentials")
	if err != nil {
		return scopedCredentialDocument{}, err
	}
	credentialsJSON, hasCredentials := fields["credentials"]
	document := scopedCredentialDocument{hasCredentials: hasCredentials}
	if !hasCredentials {
		return document, nil
	}

	var recordsJSON []json.RawMessage
	if err := json.Unmarshal(credentialsJSON, &recordsJSON); err != nil || recordsJSON == nil {
		return scopedCredentialDocument{}, errScopedCredentialSourceInvalid
	}
	document.records = make([]parsedScopedCredential, 0, len(recordsJSON))
	seen := make(map[AuthScope]string, len(recordsJSON))
	for _, recordJSON := range recordsJSON {
		recordFields, err := decodeCredentialObject(recordJSON, "provider", "protocol", "origin", "api_key")
		if err != nil {
			return scopedCredentialDocument{}, errScopedCredentialSourceInvalid
		}
		var entry struct {
			Provider string
			Protocol string
			Origin   string
			APIKey   string
		}
		for _, field := range []struct {
			name   string
			target *string
		}{
			{name: "provider", target: &entry.Provider},
			{name: "protocol", target: &entry.Protocol},
			{name: "origin", target: &entry.Origin},
			{name: "api_key", target: &entry.APIKey},
		} {
			value, ok := recordFields[field.name]
			if !ok || json.Unmarshal(value, field.target) != nil {
				return scopedCredentialDocument{}, errScopedCredentialSourceInvalid
			}
		}
		if entry.Provider == "" || entry.Protocol == "" || entry.Origin == "" || strings.TrimSpace(entry.APIKey) == "" {
			return scopedCredentialDocument{}, errScopedCredentialSourceInvalid
		}
		scope, scopeErr := normalizeAuthScope(AuthScope{ProviderID: entry.Provider, ProtocolID: entry.Protocol, Origin: entry.Origin})
		if scopeErr != nil {
			return scopedCredentialDocument{}, errScopedCredentialSourceInvalid
		}
		if existing, ok := seen[scope]; ok && existing != entry.APIKey {
			return scopedCredentialDocument{}, errScopedCredentialSourceInvalid
		}
		seen[scope] = entry.APIKey
		document.records = append(document.records, parsedScopedCredential{scope: scope, apiKey: entry.APIKey})
	}
	return document, nil
}

func decodeCredentialObject(data []byte, rejectDuplicateKeys ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	opening, ok := token.(json.Delim)
	if !ok || opening != '{' {
		return nil, errScopedCredentialSourceInvalid
	}

	fields := make(map[string]json.RawMessage)
	seen := make(map[string]struct{}, len(rejectDuplicateKeys))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errScopedCredentialSourceInvalid
		}
		canonicalKey := key
		for _, sensitiveKey := range rejectDuplicateKeys {
			if !strings.EqualFold(key, sensitiveKey) {
				continue
			}
			if _, duplicate := seen[sensitiveKey]; duplicate {
				return nil, errScopedCredentialSourceInvalid
			}
			seen[sensitiveKey] = struct{}{}
			canonicalKey = sensitiveKey
			break
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[canonicalKey] = value
	}
	closing, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return nil, errScopedCredentialSourceInvalid
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errScopedCredentialSourceInvalid
	}
	return fields, nil
}

func isMissingScopedCredentialFile(path string, readErr error) bool {
	if !errors.Is(readErr, os.ErrNotExist) {
		return false
	}
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false
	}
	for dir := filepath.Dir(path); ; {
		parentInfo, parentErr := os.Lstat(dir)
		if parentErr == nil {
			return parentInfo.Mode()&os.ModeSymlink == 0 && parentInfo.IsDir()
		}
		if !errors.Is(parentErr, os.ErrNotExist) {
			return false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return true
		}
		dir = parent
	}
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
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".config", "go-cli", "auth.json")
}

func readPrivateAuthFile(path string) ([]byte, error) {
	if path == "" {
		return nil, ErrAuthNotFound
	}
	if configDir, isDefaultPath := defaultCredentialConfigDir(path); isDefaultPath {
		configInfo, err := os.Lstat(configDir)
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrAuthNotFound
		}
		if err != nil {
			return nil, authFileError("inspect config directory", err)
		}
		if !configInfo.IsDir() {
			return nil, authFileError("config directory must be a real directory", nil)
		}
	}
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAuthNotFound
	}
	if err != nil {
		return nil, authFileError("inspect file", err)
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, authFileError("file must be regular", nil)
	}
	if pathInfo.Mode().Perm()&0o077 != 0 {
		return nil, authFileError("file permissions allow group or other access", nil)
	}

	dirInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, authFileError("inspect parent directory", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o077 != 0 {
		return nil, authFileError("parent directory allows group or other access", nil)
	}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAuthNotFound
	}
	if err != nil {
		return nil, authFileError("read file", err)
	}
	openedInfo, err := f.Stat()
	if err != nil {
		_ = f.Close() //nolint:errcheck // the inspection error is primary.
		return nil, authFileError("inspect opened file", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		_ = f.Close() //nolint:errcheck // the policy error is primary.
		return nil, authFileError("file changed while opening", nil)
	}
	if openedInfo.Mode().Perm()&0o077 != 0 {
		_ = f.Close() //nolint:errcheck // the policy error is primary.
		return nil, authFileError("file permissions allow group or other access", nil)
	}

	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil {
		return nil, authFileError("read file", readErr)
	}
	if closeErr != nil {
		return nil, authFileError("close file", closeErr)
	}
	return data, nil
}

func authFileError(reason string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", errAuthFileInvalid, reason)
	}
	return fmt.Errorf("%w: %s: %w", errAuthFileInvalid, reason, cause)
}

func defaultCredentialConfigDir(path string) (string, bool) {
	for _, defaultPath := range []string{authFilePath(), modelsConfigPath()} {
		if defaultPath != "" && filepath.Clean(path) == filepath.Clean(defaultPath) {
			return filepath.Dir(filepath.Dir(defaultPath)), true
		}
	}
	return "", false
}

// modelsConfigPath returns the location of models.json under the user config
// dir.
func modelsConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".config", "go-cli", "models.json")
}
