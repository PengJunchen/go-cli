package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
)

// setupOnboardingTest overrides the config-existence check, config path, auth
// path, TTY detection, and API-key saver to use a temp directory and the
// non-TTY file fallback. It returns the config path and a cleanup function.
func setupOnboardingTest(t *testing.T, configExists bool) (string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	authPath := filepath.Join(tmpDir, "auth.json")

	oldExists := configExistsFunc
	oldPath := onboardingConfigPathFunc
	oldAuthPath := onboardingAuthPathFunc
	oldTTY := stdinIsTTYFunc
	oldSaveAPIKey := saveAPIKeyFunc
	configExistsFunc = func() bool { return configExists }
	onboardingConfigPathFunc = func() string { return configPath }
	onboardingAuthPathFunc = func() string { return authPath }
	stdinIsTTYFunc = func() bool { return false }
	saveAPIKeyFunc = saveAuthFile // file fallback; skip the real keychain

	return configPath, func() {
		configExistsFunc = oldExists
		onboardingConfigPathFunc = oldPath
		onboardingAuthPathFunc = oldAuthPath
		stdinIsTTYFunc = oldTTY
		saveAPIKeyFunc = oldSaveAPIKey
	}
}

// TestRunOnboarding_FirstRun_TriggersWizard verifies that a first run with no
// config file triggers the full onboarding wizard.
func TestRunOnboarding_FirstRun_TriggersWizard(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := "sk-test-key\n2\n3\n"
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
// is stored in the config struct.
func TestRunOnboarding_APIKeySaved(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := "sk-my-secret-key\n1\n1\n"
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Equal(t, "sk-my-secret-key", cfg.Provider.APIKey)
}

// TestRunOnboarding_ModelSelection verifies the selected model is stored in
// both Provider.Model and Model.Name.
func TestRunOnboarding_ModelSelection(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := "sk-test\n2\n1\n" // model index 2 = gpt-4o
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
	input := "sk-test\n1\n3\n" // theme index 3 = auto
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
	input := "sk-test-key\n1\n1\n"
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
	assert.Contains(t, string(authData), "sk-test-key")

	// auth.json should have 0600 permissions.
	info, err := os.Stat(authPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestRunOnboarding_SkippedWhenConfigExistsAndAPIKeyPresent verifies the
// wizard does not run when a config file exists and the API key is set.
func TestRunOnboarding_SkippedWhenConfigExistsAndAPIKeyPresent(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, true)
	defer cleanup()

	cfg := &config.Config{
		Provider: config.ProviderConfig{
			APIKey: "existing-key",
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
	_, cleanup := setupOnboardingTest(t, true)
	defer cleanup()

	cfg := &config.Config{}
	input := "sk-new-key\n"
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Equal(t, "sk-new-key", cfg.Provider.APIKey)
	// Should NOT run full wizard (no model/theme steps).
	assert.NotContains(t, out.String(), "Step 2")
	assert.NotContains(t, out.String(), "Step 3")
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
	input := "\nsk-real-key\n1\n1\n"
	var out bytes.Buffer

	err := RunOnboarding(cfg, strings.NewReader(input), &out)
	require.NoError(t, err)

	assert.Equal(t, "sk-real-key", cfg.Provider.APIKey)
	assert.Contains(t, out.String(), "cannot be empty")
}

// TestRunOnboarding_DefaultModelWhenInvalidChoice verifies an invalid model
// choice falls back to the default (first) model.
func TestRunOnboarding_DefaultModelWhenInvalidChoice(t *testing.T) {
	_, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := "sk-test\n99\n1\n" // invalid model choice
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
	input := "sk-test\n1\n99\n" // invalid theme choice
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
	input := "sk-test\n\n\n" // empty model and theme choices
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
	cfg := &config.Config{
		Provider: config.ProviderConfig{
			Name:      "openai",
			APIKey:    "sk-test",
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
	assert.NotContains(t, yaml, "sk-test")
	assert.Contains(t, yaml, "model: gpt-4o-mini")
	assert.Contains(t, yaml, "max_tokens: 4096")
	assert.Contains(t, yaml, "tui:")
	assert.Contains(t, yaml, "theme: dark")
}

// TestSerializeOnboardingYAML_APIKeyNotLeaked verifies that API keys containing
// special characters are never written to config.yaml regardless of their
// content. The key is persisted to auth.json or the keychain instead.
func TestSerializeOnboardingYAML_APIKeyNotLeaked(t *testing.T) {
	cases := []struct {
		name   string
		apiKey string
	}{
		{"normal alphanumeric", "sk-test-key-123"},
		{"contains colon", "sk-test:key"},
		{"contains hash", "sk-test#key"},
		{"contains braces", "sk-test{key}"},
		{"contains brackets", "sk-test[key]"},
		{"contains double quote", `sk-test"key`},
		{"contains backslash", `sk-test\key`},
		{"contains multiple special", `sk:test#"key{}\`},
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

	reader := bufio.NewReader(strings.NewReader("sk-secret-key\n"))
	var out bytes.Buffer
	line, err := readPasswordMasked(reader, &out)
	require.NoError(t, err)
	assert.Equal(t, "sk-secret-key\n", line)
}

// TestSaveAuthFile_ContentAndPermissions verifies that saveAuthFile writes the
// API key as JSON with 0600 permissions.
func TestSaveAuthFile_ContentAndPermissions(t *testing.T) {
	tmpDir := t.TempDir()
	authPath := filepath.Join(tmpDir, "auth.json")

	oldPath := onboardingAuthPathFunc
	onboardingAuthPathFunc = func() string { return authPath }
	defer func() { onboardingAuthPathFunc = oldPath }()

	require.NoError(t, saveAuthFile("sk-my-secret"))

	data, err := os.ReadFile(authPath)
	require.NoError(t, err)

	var m map[string]string
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Equal(t, "sk-my-secret", m["api_key"])

	info, err := os.Stat(authPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestRunOnboarding_APIKeyInAuthJSONNotYAML verifies end-to-end that the API
// key entered during onboarding ends up in auth.json and never in config.yaml.
func TestRunOnboarding_APIKeyInAuthJSONNotYAML(t *testing.T) {
	configPath, cleanup := setupOnboardingTest(t, false)
	defer cleanup()

	cfg := &config.Config{}
	input := "sk-onboarding-key\n1\n1\n"
	var out bytes.Buffer

	require.NoError(t, RunOnboarding(cfg, strings.NewReader(input), &out))

	// config.yaml must not contain the key.
	yamlData, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotContains(t, string(yamlData), "sk-onboarding-key")
	assert.NotContains(t, string(yamlData), "api_key")

	// auth.json must contain the key as JSON.
	authPath := filepath.Join(filepath.Dir(configPath), "auth.json")
	authData, err := os.ReadFile(authPath)
	require.NoError(t, err)

	var m map[string]string
	require.NoError(t, json.Unmarshal(authData, &m))
	assert.Equal(t, "sk-onboarding-key", m["api_key"])
}
