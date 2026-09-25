//exempt:scan003
package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"
)

// setupOnboardingTest overrides the config-existence check, config path, auth
// path, TTY detection, and API-key saver to use a temp directory and the
// non-TTY file fallback. It returns the config path and a cleanup function.
func setupOnboardingTest(t *testing.T, configExists bool) (string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	require.NoError(t, os.Chmod(tmpDir, 0o700))
	configPath := filepath.Join(tmpDir, "config.yaml")
	authPath := filepath.Join(tmpDir, "auth.json")

	oldExists := configExistsFunc
	oldPath := onboardingConfigPathFunc
	oldAuthPath := onboardingAuthPathFunc
	oldTTY := stdinIsTTYFunc
	oldSaveAPIKey := saveAPIKeyFunc
	oldLookup := onboardingCredentialLookupFunc
	oldScope := onboardingCredentialScopeFunc
	oldRegistryBuilder := onboardingRegistryBuilder
	configExistsFunc = func() bool { return configExists }
	onboardingConfigPathFunc = func() string { return configPath }
	onboardingAuthPathFunc = func() string { return authPath }
	stdinIsTTYFunc = func() bool { return false }
	saveAPIKeyFunc = func(ctx context.Context, scope config.AuthScope, key string) error {
		return saveAuthFile(ctx, scope, key)
	}
	onboardingCredentialScopeFunc = func(_ *config.Config, providerName string) (config.AuthScope, error) {
		return onboardingTestScope(providerName), nil
	}
	onboardingCredentialLookupFunc = func(_ context.Context, cfg *config.Config, providerName string) (config.AuthScope, string, bool, error) {
		scope := onboardingTestScope(providerName)
		if cfg != nil && cfg.Provider.APIKey != "" && providerIdentityMatches(resolveProviderName("", cfg), providerName) {
			return scope, cfg.Provider.APIKey, true, nil
		}
		return scope, "", false, nil
	}
	onboardingRegistryBuilder = func() llm.ModelRegistry { return llm.NoopModelRegistry{} }

	return configPath, func() {
		configExistsFunc = oldExists
		onboardingConfigPathFunc = oldPath
		onboardingAuthPathFunc = oldAuthPath
		stdinIsTTYFunc = oldTTY
		saveAPIKeyFunc = oldSaveAPIKey
		onboardingCredentialLookupFunc = oldLookup
		onboardingCredentialScopeFunc = oldScope
		onboardingRegistryBuilder = oldRegistryBuilder
	}
}

func onboardingTestScope(providerName string) config.AuthScope {
	if providerName == "" {
		providerName = "eino"
	}
	return config.AuthScope{ProviderID: providerName, ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
}

type onboardingAuthDocument struct {
	Credentials []struct {
		Provider string `json:"provider"`
		Protocol string `json:"protocol"`
		Origin   string `json:"origin"`
		APIKey   string `json:"api_key"`
	} `json:"credentials"`
}

func readOnboardingAuth(t *testing.T, path string) onboardingAuthDocument {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc onboardingAuthDocument
	require.NoError(t, json.Unmarshal(data, &doc))
	return doc
}

func onboardingSecretCanary(t *testing.T) string {
	t.Helper()
	var value [16]byte
	_, err := rand.Read(value[:])
	require.NoError(t, err)
	return "auth-test-" + hex.EncodeToString(value[:])
}

func useMissingSecurityShim(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	shim := filepath.Join(binDir, "security")
	script := "#!/bin/sh\nprintf '%s\\n' 'security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.' >&2\nexit 44\n"
	require.NoError(t, os.WriteFile(shim, []byte(script), 0o700))
	t.Setenv("PATH", binDir)
}

// TestRunOnboarding_FirstRun_TriggersWizard verifies that a first run with no
// config file triggers the full onboarding wizard.
func TestRunOnboarding_FirstRun_TriggersWizard(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := onboardingSecretCanary(t) + "\n2\n3\n"
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "Welcome to go-cli")
	assert.Contains(t, output, "Step 1: API Key")
	assert.Contains(t, output, "Step 2: Model Selection")
	assert.Contains(t, output, "Step 3: Theme")
}

// TestRunOnboarding_APIKeySaved verifies the API key entered during onboarding
// is persisted separately from provider config.
func TestRunOnboarding_APIKeySaved(t *testing.T) {
	configPath, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	apiKey := onboardingSecretCanary(t)
	input := apiKey + "\n1\n1\n"
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Empty(t, cfg.Provider.APIKey)
	assert.Contains(t, out.String(), "API key saved")
	doc := readOnboardingAuth(t, filepath.Join(filepath.Dir(configPath), "auth.json"))
	require.Len(t, doc.Credentials, 1)
	assert.Equal(t, apiKey, doc.Credentials[0].APIKey)
}

func TestPromptAPIKeyPreservesExistingConfigKey(t *testing.T) {
	configuredKey := onboardingSecretCanary(t)
	promptedKey := onboardingSecretCanary(t)
	cfg := &config.Config{Provider: config.ProviderConfig{APIKey: configuredKey}}
	reader := bufio.NewReader(strings.NewReader(promptedKey + "\n"))
	oldTTY := stdinIsTTYFunc
	stdinIsTTYFunc = func() bool { return false }
	t.Cleanup(func() { stdinIsTTYFunc = oldTTY })
	var out bytes.Buffer

	key, err := promptAPIKey(reader, &out)
	require.NoError(t, err)
	assert.Equal(t, promptedKey, key)
	assert.Equal(t, configuredKey, cfg.Provider.APIKey, "prompted key must not rebind the configured provider credential")
}

// TestRunOnboarding_ModelSelection verifies the selected model is stored in
// both Provider.Model and Model.Name.
func TestRunOnboarding_ModelSelection(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := onboardingSecretCanary(t) + "\n2\n1\n" // model index 2 = gpt-4o
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Equal(t, "gpt-4o", cfg.Provider.Model)
	assert.Equal(t, "gpt-4o", cfg.Model.Name)
}

// TestRunOnboarding_ThemeSelection verifies the selected theme is stored in
// cfg.TUI.Theme.
func TestRunOnboarding_ThemeSelection(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := onboardingSecretCanary(t) + "\n1\n3\n" // theme index 3 = auto
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Equal(t, "auto", cfg.TUI.Theme)
}

// TestRunOnboarding_ConfigFileWritten verifies the config file is created on
// disk with the correct content after onboarding. The API key must NOT appear
// in config.yaml; it is stored separately in auth.json with 0600 permissions.
func TestRunOnboarding_ConfigFileWritten(t *testing.T) {
	configPath, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	apiKey := onboardingSecretCanary(t)
	input := apiKey + "\n1\n1\n"
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	content := string(data)
	assert.NotContains(t, content, "api_key")
	assert.Contains(t, content, "model: gpt-4o-mini")
	assert.Contains(t, content, "theme: dark")

	// API key should be in auth.json, not config.yaml.
	authPath := filepath.Join(filepath.Dir(configPath), "auth.json")
	authData, err := os.ReadFile(authPath)
	require.NoError(t, err)
	var auth struct {
		Credentials []struct {
			Provider string `json:"provider"`
			APIKey   string `json:"api_key"`
		} `json:"credentials"`
	}
	require.NoError(t, json.Unmarshal(authData, &auth))
	require.Len(t, auth.Credentials, 1)
	assert.Equal(t, apiKey, auth.Credentials[0].APIKey)
	assert.Equal(t, onboardingTestScope("eino").ProviderID, auth.Credentials[0].Provider)

	// auth.json should have 0600 permissions.
	info, err := os.Stat(authPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.NotContains(t, out.String(), apiKey)
}

// TestRunOnboarding_SkippedWhenConfigExistsAndAPIKeyPresent verifies the
// wizard does not run when a config file exists and the API key is set.
func TestRunOnboarding_SkippedWhenConfigExistsAndAPIKeyPresent(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, true)
	defer cleanup()

	cfg := &config.Config{
		Provider: config.ProviderConfig{
			APIKey: onboardingSecretCanary(t),
		},
	}
	var out bytes.Buffer
	input := "should-not-be-read\n"

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	// Output should be empty (wizard did not run).
	assert.Empty(t, out.String())
}

// TestRunOnboarding_ConfigExistsButNoAPIKey_PromptsForKey verifies that when a
// config file exists but the API key is missing, only the API key prompt is
// shown (not the full wizard).
func TestRunOnboarding_ConfigExistsButNoAPIKey_PromptsForKey(t *testing.T) {
	configPath, cleanup := setupOnboardingTest(t, true)
	defer cleanup()

	cfg := &config.Config{}
	apiKey := onboardingSecretCanary(t)
	input := apiKey + "\n"
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Empty(t, cfg.Provider.APIKey)
	authData, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "auth.json"))
	require.NoError(t, err)
	assert.Contains(t, string(authData), apiKey)
	// Should NOT run full wizard (no model/theme steps).
	assert.NotContains(t, out.String(), "Step 2")
	assert.NotContains(t, out.String(), "Step 3")
}

func TestRunOnboardingProviderOverrideKeepsInlineKeyOnConfiguredScope(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, true)
	defer cleanup()

	configuredKey := onboardingSecretCanary(t)
	promptedKey := onboardingSecretCanary(t)
	cfg := &config.Config{Provider: config.ProviderConfig{Name: "openai", APIKey: configuredKey}}
	var out bytes.Buffer
	require.NoError(t, runOnboarding(context.Background(), cfg, "eino", strings.NewReader(promptedKey+"\n"), &out))
	assert.Equal(t, configuredKey, cfg.Provider.APIKey)
	assert.NotContains(t, out.String(), configuredKey)
	assert.NotContains(t, out.String(), promptedKey)

	// setupOnboardingTest uses its own directory, captured by the current auth path.
	authPath := onboardingAuthPathFunc()
	doc := readOnboardingAuth(t, authPath)
	require.Len(t, doc.Credentials, 1)
	assert.Equal(t, "eino", doc.Credentials[0].Provider)
	assert.Equal(t, promptedKey, doc.Credentials[0].APIKey)
	configuredScope, err := cliPrimaryCredentialScope(cfg, "openai")
	require.NoError(t, err)
	configuredResolver, err := newCLIModelResolver(cfg, "openai", false)
	require.NoError(t, err)
	key, found, err := configuredResolver.authChain.ResolveCredential(context.Background(), configuredScope.ProviderID, configuredScope.ProtocolID, configuredScope.Origin)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, configuredKey, key)

	overrideScope, err := cliPrimaryCredentialScope(cfg, "eino")
	require.NoError(t, err)
	assert.NotEqual(t, configuredScope, overrideScope, "override credential must use a distinct provider scope")
}

type stubOnboardingKeychain struct {
	available bool
	setErr    error
	scope     config.AuthScope
	key       string
}

func (s *stubOnboardingKeychain) Available() bool { return s.available }
func (s *stubOnboardingKeychain) SetScoped(scope config.AuthScope, key string) error {
	s.scope = scope
	if s.setErr == nil {
		s.key = key
	}
	return s.setErr
}

func TestDefaultSaveAPIKeyDoesNotFallbackAfterAvailableKeychainFailure(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	authPath := filepath.Join(dir, "auth.json")
	oldPath := onboardingAuthPathFunc
	oldFactory := onboardingKeychainFactory
	onboardingAuthPathFunc = func() string { return authPath }
	oldKey := onboardingSecretCanary(t)
	fake := &stubOnboardingKeychain{available: true, setErr: errors.New("keychain access denied"), key: oldKey}
	onboardingKeychainFactory = func() onboardingKeychain { return fake }
	t.Cleanup(func() {
		onboardingAuthPathFunc = oldPath
		onboardingKeychainFactory = oldFactory
	})

	scope := onboardingTestScope("openai")
	newKey := onboardingSecretCanary(t)
	err := defaultSaveAPIKey(context.Background(), scope, newKey)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), newKey)
	assert.Equal(t, scope, fake.scope)
	assert.Equal(t, oldKey, fake.key, "failed -U must leave the previously stored exact-scope item unchanged")
	_, statErr := os.Lstat(authPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "available keychain failure must not be reported as a successful file fallback")
}

func TestDefaultSaveAPIKeyUsesScopedFileWhenKeychainUnavailable(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	authPath := filepath.Join(dir, "auth.json")
	oldPath := onboardingAuthPathFunc
	oldFactory := onboardingKeychainFactory
	onboardingAuthPathFunc = func() string { return authPath }
	onboardingKeychainFactory = func() onboardingKeychain { return &stubOnboardingKeychain{} }
	t.Cleanup(func() {
		onboardingAuthPathFunc = oldPath
		onboardingKeychainFactory = oldFactory
	})

	scope := onboardingTestScope("openai")
	fileKey := onboardingSecretCanary(t)
	require.NoError(t, defaultSaveAPIKey(context.Background(), scope, fileKey))
	doc := readOnboardingAuth(t, authPath)
	require.Len(t, doc.Credentials, 1)
	assert.Equal(t, scope.ProviderID, doc.Credentials[0].Provider)
	assert.Equal(t, fileKey, doc.Credentials[0].APIKey)
}

func TestOnboardingWithoutHomeDoesNotReadProjectRelativeCredentialFile(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GO_CLI_NO_ONBOARDING", "")
	t.Setenv("OPENAI_API_KEY", "")
	useMissingSecurityShim(t)

	projectAuthDir := filepath.Join(workDir, ".config", "go-cli")
	require.NoError(t, os.MkdirAll(projectAuthDir, 0o700))
	require.NoError(t, os.Chmod(projectAuthDir, 0o700))
	projectAuthPath := filepath.Join(projectAuthDir, "auth.json")
	cfg := &config.Config{Provider: config.ProviderConfig{Name: "openai"}}
	scope, err := cliPrimaryCredentialScope(cfg, "openai")
	require.NoError(t, err)
	projectSecret := onboardingSecretCanary(t)
	require.NoError(t, config.UpsertScopedCredentialFile(context.Background(), projectAuthPath, scope, projectSecret))
	projectAuthBefore, err := os.ReadFile(projectAuthPath)
	require.NoError(t, err)

	oldExists := configExistsFunc
	oldAuthPath := onboardingAuthPathFunc
	oldTTY := stdinIsTTYFunc
	oldLookup := onboardingCredentialLookupFunc
	oldScope := onboardingCredentialScopeFunc
	oldSave := saveAPIKeyFunc
	configExistsFunc = func() bool { return true }
	onboardingAuthPathFunc = defaultAuthFilePath
	stdinIsTTYFunc = func() bool { return false }
	onboardingCredentialLookupFunc = resolveCLIPrimaryCredential
	onboardingCredentialScopeFunc = cliPrimaryCredentialScope
	promptedSecret := onboardingSecretCanary(t)
	saved := 0
	saveAPIKeyFunc = func(context.Context, config.AuthScope, string) error { saved++; return nil }
	t.Cleanup(func() {
		configExistsFunc = oldExists
		onboardingAuthPathFunc = oldAuthPath
		stdinIsTTYFunc = oldTTY
		onboardingCredentialLookupFunc = oldLookup
		onboardingCredentialScopeFunc = oldScope
		saveAPIKeyFunc = oldSave
	})

	var out bytes.Buffer
	err = runOnboarding(context.Background(), cfg, "openai", strings.NewReader(promptedSecret+"\n"), &out)
	require.NoError(t, err)
	assert.Equal(t, 1, saved, "the bait file must not satisfy the default credential source")
	assert.Contains(t, out.String(), "Step 1: API Key")
	assert.NotContains(t, out.String(), projectSecret)
	assert.NotContains(t, out.String(), promptedSecret)
	assert.Empty(t, defaultAuthFilePath(), "an unset HOME must disable the default auth path")
	projectAuthAfter, err := os.ReadFile(projectAuthPath)
	require.NoError(t, err)
	assert.Equal(t, projectAuthBefore, projectAuthAfter, "the project-relative bait remains untouched")
}

// TestRunOnboarding_DisabledViaEnv verifies the wizard is skipped when
// GO_CLI_NO_ONBOARDING is set.
func TestRunOnboarding_DisabledViaEnv(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	t.Setenv("GO_CLI_NO_ONBOARDING", "1")

	cfg := &config.Config{}
	var out bytes.Buffer
	input := "should-not-be-read\n"

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Empty(t, out.String())
	assert.Empty(t, cfg.Provider.APIKey)
}

// TestRunOnboarding_EmptyAPIKeyRetries verifies that an empty API key input is
// rejected and the user is prompted again.
func TestRunOnboarding_EmptyAPIKeyRetries(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	// First attempt empty, second attempt provides a key.
	input := "\n" + onboardingSecretCanary(t) + "\n1\n1\n"
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Empty(t, cfg.Provider.APIKey)
	assert.Contains(t, out.String(), "cannot be empty")
}

// TestRunOnboarding_DefaultModelWhenInvalidChoice verifies an invalid model
// choice falls back to the default (first) model.
func TestRunOnboarding_DefaultModelWhenInvalidChoice(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := onboardingSecretCanary(t) + "\n99\n1\n" // invalid model choice
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	// Should fall back to default (first model).
	assert.Equal(t, onboardingModelChoices[0].Name, cfg.Provider.Model)
}

// TestRunOnboarding_DefaultThemeWhenInvalidChoice verifies an invalid theme
// choice falls back to the default (first) theme.
func TestRunOnboarding_DefaultThemeWhenInvalidChoice(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := onboardingSecretCanary(t) + "\n1\n99\n" // invalid theme choice
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Equal(t, onboardingThemeChoices[0], cfg.TUI.Theme)
}

// TestRunOnboarding_DefaultModelWhenEmptyChoice verifies an empty model choice
// (just Enter) falls back to the default model.
func TestRunOnboarding_DefaultModelWhenEmptyChoice(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := onboardingSecretCanary(t) + "\n\n\n" // empty model and theme choices
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Equal(t, onboardingModelChoices[0].Name, cfg.Provider.Model)
	assert.Equal(t, onboardingThemeChoices[0], cfg.TUI.Theme)
}

// TestHasNoOnboardingFlag verifies the flag detection helper.
func TestHasNoOnboardingFlag(t *testing.T) {
	assert.True(t, hasNoOnboardingFlag([]string{"--no-onboarding"}))
	assert.True(t, hasNoOnboardingFlag([]string{"-no-onboarding"}))
	assert.True(t, hasNoOnboardingFlag([]string{"--model", "gpt-4o", "--no-onboarding"}))
	assert.False(t, hasNoOnboardingFlag([]string{"--model", "gpt-4o"}))
	assert.False(t, hasNoOnboardingFlag([]string{}))
}

// TestSerializeOnboardingYAML verifies the YAML output contains the expected
// fields and excludes the API key.
func TestSerializeOnboardingYAML(t *testing.T) {
	apiKey := onboardingSecretCanary(t)
	cfg := &config.Config{
		Provider: config.ProviderConfig{
			Name:      "openai",
			APIKey:    apiKey,
			Model:     "gpt-4o-mini",
			MaxTokens: 4096,
		},
		Model: config.ModelConfig{
			Name:      "gpt-4o-mini",
			MaxTokens: 4096,
		},
	}
	cfg.TUI.Theme = "dark"

	yaml := serializeOnboardingYAML(cfg)
	assert.Contains(t, yaml, "provider:")
	assert.Contains(t, yaml, "name: openai")
	assert.NotContains(t, yaml, "api_key")
	assert.NotContains(t, yaml, apiKey)
	assert.Contains(t, yaml, "model: gpt-4o-mini")
	assert.Contains(t, yaml, "max_tokens: 4096")
	assert.Contains(t, yaml, "tui:")
	assert.Contains(t, yaml, "theme: dark")
}

// TestSerializeOnboardingYAML_APIKeyNotLeaked verifies that API keys containing
// special characters are never written to config.yaml regardless of their
// content. The key is persisted to auth.json or the keychain instead.
func TestSerializeOnboardingYAML_APIKeyNotLeaked(t *testing.T) {
	base := onboardingSecretCanary(t)
	cases := []struct {
		name   string
		apiKey string
	}{
		{"normal alphanumeric", base},
		{"contains colon", base + ":key"},
		{"contains hash", base + "#key"},
		{"contains braces", base + "{key}"},
		{"contains brackets", base + "[key]"},
		{"contains double quote", base + `"key`},
		{"contains backslash", base + `\key`},
		{"contains multiple special", base + `:#"key{}\`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Provider: config.ProviderConfig{
					APIKey: tc.apiKey,
					Model:  "gpt-4o-mini",
				},
			}
			cfg.TUI.Theme = "dark"

			yaml := serializeOnboardingYAML(cfg)
			assert.NotContains(t, yaml, "api_key", "API key field must not appear in config.yaml")
			assert.NotContains(t, yaml, tc.apiKey, "API key value must not leak into YAML")
		})
	}
}

// TestReadPasswordMasked_NonTTYFallback verifies that when stdin is not a TTY
// (pipes, CI), readPasswordMasked falls back to a plain ReadString so the
// input is still consumed correctly.
func TestReadPasswordMasked_NonTTYFallback(t *testing.T) {
	oldTTY := stdinIsTTYFunc
	stdinIsTTYFunc = func() bool { return false }
	defer func() { stdinIsTTYFunc = oldTTY }()

	secret := onboardingSecretCanary(t)
	reader := bufio.NewReader(strings.NewReader(secret + "\n"))
	var out bytes.Buffer
	line, err := readPasswordMasked(reader, &out)
	require.NoError(t, err)
	assert.Equal(t, secret+"\n", line)
}

// TestSaveAuthFile_ContentAndPermissions verifies that saveAuthFile writes the
// API key as JSON with 0600 permissions.
func TestSaveAuthFile_ContentAndPermissions(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.Chmod(tmpDir, 0o700))
	authPath := filepath.Join(tmpDir, "auth.json")

	oldPath := onboardingAuthPathFunc
	onboardingAuthPathFunc = func() string { return authPath }
	defer func() { onboardingAuthPathFunc = oldPath }()

	oldSecret := onboardingSecretCanary(t)
	oldData, err := json.Marshal(map[string]string{"api_key": oldSecret})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(authPath, oldData, 0o644))
	require.NoError(t, os.Chmod(authPath, 0o644))
	newSecret := onboardingSecretCanary(t)
	require.NoError(t, saveAuthFile(context.Background(), onboardingTestScope("openai"), newSecret))

	data, err := os.ReadFile(authPath)
	require.NoError(t, err)

	var auth onboardingAuthDocument
	require.NoError(t, json.Unmarshal(data, &auth))
	require.Len(t, auth.Credentials, 1)
	assert.Equal(t, newSecret, auth.Credentials[0].APIKey)

	info, err := os.Stat(authPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSaveAuthFileDefaultDirectoryIsPrivate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "go-cli")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))

	oldPath := onboardingAuthPathFunc
	onboardingAuthPathFunc = defaultAuthFilePath
	defer func() { onboardingAuthPathFunc = oldPath }()

	secret := onboardingSecretCanary(t)
	require.NoError(t, saveAuthFile(context.Background(), onboardingTestScope("openai"), secret))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	authData, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	require.NoError(t, err)
	var auth onboardingAuthDocument
	require.NoError(t, json.Unmarshal(authData, &auth))
	require.Len(t, auth.Credentials, 1)
	assert.Equal(t, secret, auth.Credentials[0].APIKey)
}

func TestSaveAuthFileRejectsSymlinkedDefaultDirectory(t *testing.T) {
	t.Run("config directory symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		shared := filepath.Join(home, "shared")
		require.NoError(t, os.Mkdir(shared, 0o755))
		require.NoError(t, os.Chmod(shared, 0o755))
		configDir := filepath.Join(home, ".config")
		require.NoError(t, os.Symlink(shared, configDir))

		oldPath := onboardingAuthPathFunc
		onboardingAuthPathFunc = defaultAuthFilePath
		t.Cleanup(func() { onboardingAuthPathFunc = oldPath })

		secret := onboardingSecretCanary(t)
		err := saveAuthFile(context.Background(), onboardingTestScope("openai"), secret)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secret)
		_, statErr := os.Lstat(filepath.Join(shared, "go-cli"))
		assert.ErrorIs(t, statErr, os.ErrNotExist, "external directory must not be created through the link")
	})

	t.Run("app directory symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := filepath.Join(home, ".config")
		require.NoError(t, os.Mkdir(configDir, 0o700))
		shared := filepath.Join(home, "shared")
		require.NoError(t, os.Mkdir(shared, 0o755))
		require.NoError(t, os.Chmod(shared, 0o755))
		appDir := filepath.Join(configDir, "go-cli")
		require.NoError(t, os.Symlink(shared, appDir))

		oldPath := onboardingAuthPathFunc
		onboardingAuthPathFunc = defaultAuthFilePath
		t.Cleanup(func() { onboardingAuthPathFunc = oldPath })

		secret := onboardingSecretCanary(t)
		err := saveAuthFile(context.Background(), onboardingTestScope("openai"), secret)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secret)
		info, statErr := os.Stat(shared)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "external directory permissions must not be changed")
		_, statErr = os.Lstat(filepath.Join(appDir, "auth.json"))
		assert.ErrorIs(t, statErr, os.ErrNotExist)
	})
}

func TestSaveAuthFileRejectsAccessibleCustomDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))
	authPath := filepath.Join(dir, "auth.json")
	oldSecret := onboardingSecretCanary(t)
	oldData, err := json.Marshal(map[string]string{"api_key": oldSecret})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(authPath, oldData, 0o644))
	require.NoError(t, os.Chmod(authPath, 0o644))

	oldPath := onboardingAuthPathFunc
	onboardingAuthPathFunc = func() string { return authPath }
	defer func() { onboardingAuthPathFunc = oldPath }()

	newSecret := onboardingSecretCanary(t)
	err = saveAuthFile(context.Background(), onboardingTestScope("openai"), newSecret)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), newSecret)
	data, readErr := os.ReadFile(authPath)
	require.NoError(t, readErr)
	assert.Equal(t, oldData, data)
	info, statErr := os.Stat(authPath)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	dirInfo, statErr := os.Stat(dir)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o755), dirInfo.Mode().Perm())
}

// TestRunOnboarding_APIKeyInAuthJSONNotYAML verifies end-to-end that the API
// key entered during onboarding ends up in auth.json and never in config.yaml.
func TestRunOnboarding_APIKeyInAuthJSONNotYAML(t *testing.T) {
	configPath, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	apiKey := onboardingSecretCanary(t)
	input := apiKey + "\n1\n1\n"
	var out bytes.Buffer

	require.NoError(t, RunOnboarding(cfg, strings.NewReader(input), &out))

	// config.yaml must not contain the key.
	yamlData, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotContains(t, string(yamlData), apiKey)
	assert.NotContains(t, string(yamlData), "api_key")

	// auth.json must contain the key as JSON.
	authPath := filepath.Join(filepath.Dir(configPath), "auth.json")
	authData, err := os.ReadFile(authPath)
	require.NoError(t, err)

	var auth onboardingAuthDocument
	require.NoError(t, json.Unmarshal(authData, &auth))
	require.Len(t, auth.Credentials, 1)
	assert.Equal(t, apiKey, auth.Credentials[0].APIKey)
}

// mockModelRegistry is a test double for llm.ModelRegistry. It returns
// canned provider and model data without any network access.
type mockModelRegistry struct {
	providers []llm.ProviderMetadata
	models    map[string][]llm.ModelInfo
	refreshFn func(context.Context) error
}

func (m *mockModelRegistry) Lookup(_ context.Context, _, _ string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}
func (m *mockModelRegistry) Providers() []llm.ProviderMetadata { return m.providers }
func (m *mockModelRegistry) ModelsForProvider(id string) []llm.ModelInfo {
	return m.models[id]
}
func (m *mockModelRegistry) Refresh(ctx context.Context) error {
	if m.refreshFn != nil {
		return m.refreshFn(ctx)
	}
	return nil
}

// TestPromptModelDynamicFromRegistry verifies that when the registry contains
// model data, promptModel displays those models and stores the user's choice.
func TestPromptModelDynamicFromRegistry(t *testing.T) {
	registry := &mockModelRegistry{
		providers: []llm.ProviderMetadata{
			{ID: "openai", Name: "OpenAI"},
		},
		models: map[string][]llm.ModelInfo{
			"openai": {
				{Name: "gpt-4o", ContextWindow: 128000},
				{Name: "gpt-4o-mini", ContextWindow: 128000},
			},
		},
	}

	cfg := &config.Config{}
	reader := bufio.NewReader(strings.NewReader("2\n"))
	var out bytes.Buffer

	err := promptModel(cfg, reader, &out, registry)
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "gpt-4o")
	assert.Contains(t, output, "gpt-4o-mini")
	// Index 2 = gpt-4o-mini (second in the dynamic list)
	assert.Equal(t, "gpt-4o-mini", cfg.Provider.Model)
	assert.Equal(t, "gpt-4o-mini", cfg.Model.Name)
}

// TestPromptModelFallbackOnNetworkError verifies that when the registry returns
// no models (simulating a network error or empty registry), promptModel falls
// back to the hardcoded onboardingModelChoices list.
func TestPromptModelFallbackOnNetworkError(t *testing.T) {
	cfg := &config.Config{}
	reader := bufio.NewReader(strings.NewReader("1\n"))
	var out bytes.Buffer

	// NoopModelRegistry returns no providers/models — simulates network failure.
	err := promptModel(cfg, reader, &out, llm.NoopModelRegistry{})
	require.NoError(t, err)

	output := out.String()
	// Should contain the first hardcoded fallback model.
	assert.Contains(t, output, onboardingModelChoices[0].Name)
	assert.Equal(t, onboardingModelChoices[0].Name, cfg.Provider.Model)
}

func TestPromptModelOverridesPreloadedDefaultsAndReportsCLIOverride(t *testing.T) {
	cfg := &config.Config{
		Provider: config.ProviderConfig{Model: "gpt-4o-mini"},
		Model:    config.ModelConfig{Name: "legacy-conflict"},
	}
	reader := bufio.NewReader(strings.NewReader("2\n"))
	var out bytes.Buffer

	require.NoError(t, promptModelWithOverride(cfg, reader, &out, llm.NoopModelRegistry{}, "gpt-4o-mini"))

	assert.Equal(t, "gpt-4o", cfg.Provider.Model)
	assert.Equal(t, "gpt-4o", cfg.Model.Name)
	assert.Contains(t, out.String(), "Default model selected: gpt-4o")
	assert.Contains(t, out.String(), "Current session uses --model gpt-4o-mini")
}

// TestPromptModelProviderFilter verifies that fetchModelsFromRegistry collects
// models from all providers returned by the registry, filters out deprecated
// models, and deduplicates by name.
func TestPromptModelProviderFilter(t *testing.T) {
	registry := &mockModelRegistry{
		providers: []llm.ProviderMetadata{
			{ID: "openai", Name: "OpenAI"},
			{ID: "anthropic", Name: "Anthropic"},
		},
		models: map[string][]llm.ModelInfo{
			"openai": {
				{Name: "gpt-4o", ContextWindow: 128000},
				{Name: "gpt-4-32k", ContextWindow: 32768}, // deprecated
			},
			"anthropic": {
				{Name: "claude-3-5-sonnet", ContextWindow: 200000},
				{Name: "claude-3-opus", ContextWindow: 200000}, // deprecated
			},
		},
	}

	choices := fetchModelsFromRegistry(registry)

	names := make(map[string]bool)
	for _, c := range choices {
		names[c.Name] = true
	}

	// Non-deprecated models from both providers should be present.
	assert.True(t, names["gpt-4o"], "gpt-4o should be included")
	assert.True(t, names["claude-3-5-sonnet"], "claude-3-5-sonnet should be included")

	// Deprecated models should be filtered out.
	assert.False(t, names["gpt-4-32k"], "deprecated gpt-4-32k should be filtered")
	assert.False(t, names["claude-3-opus"], "deprecated claude-3-opus should be filtered")

	// Descriptions should include the provider name.
	descByName := make(map[string]string)
	for _, c := range choices {
		descByName[c.Name] = c.Desc
	}
	assert.Contains(t, descByName["gpt-4o"], "OpenAI")
	assert.Contains(t, descByName["claude-3-5-sonnet"], "Anthropic")
}
