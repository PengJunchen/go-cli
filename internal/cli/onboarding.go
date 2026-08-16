package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	xterm "golang.org/x/term"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"
)

// modelChoice is a single model entry shown during the onboarding model
// selection step.
type modelChoice struct {
	Name string
	Desc string
}

// onboardingModelChoices lists the models offered during the first-run wizard.
// It serves as the fallback when the dynamic registry fetch fails or returns no
// data.
var onboardingModelChoices = []modelChoice{
	{"gpt-4o-mini", "Fast and affordable (OpenAI)"},
	{"gpt-4o", "Most capable (OpenAI)"},
	{"claude-3-5-sonnet", "Balanced (Anthropic)"},
	{"claude-3-opus", "Most capable (Anthropic)"},
}

// onboardingRegistryBuilder creates the model registry used during onboarding.
// It is a package-level variable so tests can override it to avoid network
// access.
var onboardingRegistryBuilder = func() llm.ModelRegistry {
	return llm.NewModelsDevRegistry("", 0)
}

// onboardingModelProvider fetches the list of model choices from the registry.
// It is a package-level variable so tests can override it to simulate different
// registry states (e.g. network error, specific provider data).
var onboardingModelProvider = fetchModelsFromRegistry

// fetchModelsFromRegistry queries the registry for all models across all
// providers, filters out deprecated models, deduplicates by name, and returns
// them as modelChoice entries. It returns nil when the registry is nil or
// empty, prompting the caller to fall back to the hardcoded list.
func fetchModelsFromRegistry(registry llm.ModelRegistry) []modelChoice {
	if registry == nil {
		return nil
	}
	var choices []modelChoice
	seen := make(map[string]bool)
	for _, p := range registry.Providers() {
		for _, info := range registry.ModelsForProvider(p.ID) {
			if isDeprecatedModel(info) {
				continue
			}
			if seen[info.Name] {
				continue
			}
			seen[info.Name] = true
			choices = append(choices, modelChoice{
				Name: info.Name,
				Desc: buildModelDesc(info, p),
			})
		}
	}
	return choices
}

// isDeprecatedModel reports whether a model should be excluded from the
// onboarding list. It matches known deprecated model name patterns.
func isDeprecatedModel(info llm.ModelInfo) bool {
	name := strings.ToLower(info.Name)
	for _, pattern := range deprecatedModelPatterns {
		if strings.Contains(name, pattern) {
			return true
		}
	}
	return false
}

// deprecatedModelPatterns lists lowercase substrings that identify deprecated
// or superseded models. Models whose names contain any of these patterns are
// excluded from the dynamic onboarding list.
var deprecatedModelPatterns = []string{
	"claude-3-opus",
	"gpt-4-32k",
	"gpt-4-0314",
	"gpt-4-0613",
	"gpt-3.5-turbo-0301",
	"gpt-3.5-turbo-0613",
	"text-davinci",
}

// buildModelDesc constructs a human-readable description for a model from its
// registry metadata and provider info. It prefers the registry's own
// description and falls back to a compact "Provider · NK ctx" signature.
func buildModelDesc(info llm.ModelInfo, provider llm.ProviderMetadata) string {
	if info.Description != "" {
		return info.Description
	}
	var parts []string
	if provider.Name != "" {
		parts = append(parts, provider.Name)
	}
	if info.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("%dK ctx", info.ContextWindow/1000))
	}
	return strings.Join(parts, " · ")
}

// onboardingThemeChoices lists the themes offered during the first-run wizard.
var onboardingThemeChoices = []string{"dark", "light", "auto"}

// configExistsFunc checks whether any config file already exists in the
// standard search paths. It is a package-level variable so tests can override
// it to simulate first-run vs. existing-config scenarios.
var configExistsFunc = configFilesExist

// onboardingConfigPathFunc returns the path where the onboarding wizard saves
// the config file. It is a package-level variable so tests can override it to
// redirect writes to a temp directory.
var onboardingConfigPathFunc = defaultOnboardingConfigPath

// onboardingAuthPathFunc returns the path where the onboarding wizard saves
// the auth.json file containing the API key. Overridable for tests.
var onboardingAuthPathFunc = defaultAuthFilePath

// stdinIsTTYFunc reports whether stdin is an interactive terminal. The wizard
// uses it to decide between term.ReadPassword (no echo) and a plain
// bufio.ReadString fallback. Overridable for tests so the non-TTY path is
// always exercised.
var stdinIsTTYFunc = func() bool {
	return xterm.IsTerminal(int(os.Stdin.Fd()))
}

// saveAPIKeyFunc persists the API key to the keychain (preferred) or auth.json
// fallback. Overridable for tests to avoid touching the real keychain.
var saveAPIKeyFunc = defaultSaveAPIKey

// RunOnboarding runs the first-run onboarding wizard. When no config file
// exists it guides the user through API key input, model selection, and theme
// selection, then saves the config. When a config file exists but the API key
// is missing it prompts for the key only (in-memory, no file write).
//
// The wizard is skipped when the GO_CLI_NO_ONBOARDING environment variable is
// set to a truthy value (1, true, yes, on), or when the API key is already
// configured (e.g. via environment variable or CLI flag).
func RunOnboarding(cfg *config.Config, in io.Reader, out io.Writer) error {
	if isOnboardingDisabled() {
		return nil
	}

	hasConfig := configExistsFunc()
	hasAPIKey := cfg != nil && cfg.Provider.APIKey != ""

	// Skip the wizard entirely when the API key is already set. The wizard's
	// primary purpose is to collect an API key; model and theme can be
	// configured via CLI flags or the config file.
	if hasAPIKey {
		return nil
	}

	reader := bufio.NewReader(in)

	if !hasConfig {
		// Full first-run wizard.
		printWelcome(out)
		if err := promptAPIKey(cfg, reader, out); err != nil {
			return err
		}
		registry := onboardingRegistryBuilder()
		// Best-effort refresh with a short timeout. On failure the registry is
		// replaced with a no-op so promptModel falls back to the hardcoded list
		// without further network attempts.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := registry.Refresh(ctx); err != nil {
			registry = llm.NoopModelRegistry{}
		}
		cancel()
		if err := promptModel(cfg, reader, out, registry); err != nil {
			return err
		}
		if err := promptTheme(cfg, reader, out); err != nil {
			return err
		}
		return saveOnboardingConfig(cfg, out)
	}

	// Config file exists but API key is missing — prompt for it only.
	return promptAPIKey(cfg, reader, out)
}

// hasNoOnboardingFlag reports whether the --no-onboarding (or -no-onboarding)
// flag is present in args. Go's flag package treats single and double dash
// identically.
func hasNoOnboardingFlag(args []string) bool {
	for _, a := range args {
		if a == "--no-onboarding" || a == "-no-onboarding" {
			return true
		}
	}
	return false
}

// isOnboardingDisabled checks the GO_CLI_NO_ONBOARDING environment variable.
func isOnboardingDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("GO_CLI_NO_ONBOARDING")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// printWelcome writes the wizard welcome banner to out.
func printWelcome(out io.Writer) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "=== Welcome to go-cli! Let's get set up. ===")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "This quick wizard will configure your API key,")
	fmt.Fprintln(out, "default model, and theme preference.")
	fmt.Fprintln(out, "")
}

// promptAPIKey prompts the user for their API key and stores it in cfg. It
// retries on empty input until a non-empty key is provided or input ends.
// When stdin is a TTY the input is masked (no echo) via term.ReadPassword;
// otherwise it falls back to a plain ReadString so piped/CI input still works.
func promptAPIKey(cfg *config.Config, reader *bufio.Reader, out io.Writer) error {
	for {
		fmt.Fprintln(out, "Step 1: API Key")
		fmt.Fprintln(out, "  Enter your LLM provider API key (e.g. sk-...).")
		fmt.Fprint(out, "  API Key: ")
		line, err := readPasswordMasked(reader, out)
		key := strings.TrimSpace(line)
		if key != "" {
			if cfg != nil {
				cfg.Provider.APIKey = key
			}
			fmt.Fprintln(out, "  ✓ API key saved.")
			fmt.Fprintln(out, "")
			return nil
		}
		if err == io.EOF {
			return fmt.Errorf("onboarding: API key is required")
		}
		if err != nil {
			return fmt.Errorf("onboarding: read API key: %w", err)
		}
		fmt.Fprintln(out, "  ✗ API key cannot be empty. Please try again.")
		fmt.Fprintln(out, "")
	}
}

// readPasswordMasked reads a single line of input. In a TTY it uses
// term.ReadPassword so the key is not echoed; in non-TTY environments (pipes,
// CI) it falls back to reader.ReadString so automated input still works. A
// newline is printed after the masked read to keep the prompt formatting tidy.
func readPasswordMasked(reader *bufio.Reader, out io.Writer) (string, error) {
	if stdinIsTTYFunc() {
		b, err := xterm.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(out)
		return string(b), err
	}
	return reader.ReadString('\n')
}

// promptModel lists available models and lets the user pick one. It fetches
// the model list dynamically from the registry via onboardingModelProvider;
// when the registry returns no models (network error, empty data) it falls
// back to the hardcoded onboardingModelChoices. An invalid or empty choice
// falls back to the first model in the list.
func promptModel(cfg *config.Config, reader *bufio.Reader, out io.Writer, registry llm.ModelRegistry) error {
	fmt.Fprintln(out, "Step 2: Model Selection")

	choices := onboardingModelProvider(registry)
	if len(choices) == 0 {
		choices = onboardingModelChoices
	}

	for i, m := range choices {
		fmt.Fprintf(out, "  %d. %-22s %s\n", i+1, m.Name, m.Desc)
	}
	fmt.Fprintf(out, "  Choose a model [1-%d] (default: 1): ", len(choices))

	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("onboarding: read model choice: %w", err)
	}
	line = strings.TrimSpace(line)

	idx := 0
	if line != "" {
		n, parseErr := strconv.Atoi(line)
		if parseErr != nil || n < 1 || n > len(choices) {
			fmt.Fprintf(out, "  Invalid choice, using default: %s\n", choices[0].Name)
		} else {
			idx = n - 1
		}
	}

	chosen := choices[idx].Name
	if cfg != nil {
		if cfg.Provider.Model == "" {
			cfg.Provider.Model = chosen
		}
		if cfg.Model.Name == "" {
			cfg.Model.Name = chosen
		}
	}
	fmt.Fprintf(out, "  ✓ Model set to: %s\n", chosen)
	fmt.Fprintln(out, "")
	return nil
}

// promptTheme lets the user pick a UI theme. An invalid or empty choice falls
// back to the first theme in the list (dark).
func promptTheme(cfg *config.Config, reader *bufio.Reader, out io.Writer) error {
	fmt.Fprintln(out, "Step 3: Theme")
	for i, th := range onboardingThemeChoices {
		fmt.Fprintf(out, "  %d. %s\n", i+1, th)
	}
	fmt.Fprintf(out, "  Choose a theme [1-%d] (default: 1): ", len(onboardingThemeChoices))

	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("onboarding: read theme choice: %w", err)
	}
	line = strings.TrimSpace(line)

	idx := 0
	if line != "" {
		n, parseErr := strconv.Atoi(line)
		if parseErr != nil || n < 1 || n > len(onboardingThemeChoices) {
			fmt.Fprintf(out, "  Invalid choice, using default: %s\n", onboardingThemeChoices[0])
		} else {
			idx = n - 1
		}
	}

	chosen := onboardingThemeChoices[idx]
	if cfg != nil {
		cfg.TUI.Theme = chosen
	}
	fmt.Fprintf(out, "  ✓ Theme set to: %s\n", chosen)
	fmt.Fprintln(out, "")
	return nil
}

// saveOnboardingConfig writes the config to the path returned by
// onboardingConfigPathFunc, creating the parent directory if needed. The API
// key is NOT stored in config.yaml; it is persisted separately to the OS
// keychain (preferred) or auth.json fallback via saveAPIKeyFunc.
func saveOnboardingConfig(cfg *config.Config, out io.Writer) error {
	path := onboardingConfigPathFunc()
	if path == "" {
		return fmt.Errorf("onboarding: cannot determine config path")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("onboarding: create config dir: %w", err)
	}

	data := serializeOnboardingYAML(cfg)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		return fmt.Errorf("onboarding: write config: %w", err)
	}

	// Persist the API key to the keychain or auth.json — never config.yaml.
	if cfg.Provider.APIKey != "" {
		if err := saveAPIKeyFunc(cfg.Provider.APIKey); err != nil {
			return fmt.Errorf("onboarding: save API key: %w", err)
		}
	}

	fmt.Fprintf(out, "✓ Configuration saved to %s\n", path)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "You're all set! Type 'exit' or press Ctrl+C to quit.")
	fmt.Fprintln(out, "")
	return nil
}

// serializeOnboardingYAML produces a simple YAML representation of the config
// fields set by the onboarding wizard. Only the provider, model, and tui
// sections are written so the output stays minimal and human-editable. The
// API key is intentionally excluded — it is persisted to the keychain or
// auth.json, never to config.yaml.
func serializeOnboardingYAML(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("# go-cli configuration (generated by onboarding wizard)\n\n")

	b.WriteString("provider:\n")
	if cfg.Provider.Name != "" {
		fmt.Fprintf(&b, "  name: %s\n", cfg.Provider.Name)
	}
	if cfg.Provider.BaseURL != "" {
		fmt.Fprintf(&b, "  base_url: %s\n", cfg.Provider.BaseURL)
	}
	if cfg.Provider.Model != "" {
		fmt.Fprintf(&b, "  model: %s\n", cfg.Provider.Model)
	}
	if cfg.Provider.MaxTokens > 0 {
		fmt.Fprintf(&b, "  max_tokens: %d\n", cfg.Provider.MaxTokens)
	}

	b.WriteString("\nmodel:\n")
	if cfg.Model.Name != "" {
		fmt.Fprintf(&b, "  name: %s\n", cfg.Model.Name)
	}
	if cfg.Model.MaxTokens > 0 {
		fmt.Fprintf(&b, "  max_tokens: %d\n", cfg.Model.MaxTokens)
	}

	b.WriteString("\ntui:\n")
	if cfg.TUI.Theme != "" {
		fmt.Fprintf(&b, "  theme: %s\n", cfg.TUI.Theme)
	}

	return b.String()
}

// defaultOnboardingConfigPath returns the config file path used by the wizard,
// typically ~/.go-cli/config.yaml.
func defaultOnboardingConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".go-cli", "config.yaml")
}

// defaultAuthFilePath returns the auth.json path used by the wizard, typically
// ~/.config/go-cli/auth.json. This mirrors the path consulted by the config
// package's lookupAuthFile so the two stay in sync.
func defaultAuthFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "go-cli", "auth.json")
}

// saveAuthFile writes the API key to auth.json as {"api_key":"..."} with 0600
// permissions, creating the parent directory if needed.
func saveAuthFile(apiKey string) error {
	path := onboardingAuthPathFunc()
	if path == "" {
		return fmt.Errorf("onboarding: cannot determine auth file path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("onboarding: create auth dir: %w", err)
	}
	data, err := json.Marshal(map[string]string{"api_key": apiKey})
	if err != nil {
		return fmt.Errorf("onboarding: marshal auth: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("onboarding: write auth: %w", err)
	}
	return nil
}

// defaultSaveAPIKey persists the API key to the OS keychain when available
// (macOS), falling back to the auth.json file on other platforms or when the
// keychain write fails.
func defaultSaveAPIKey(key string) error {
	kc := config.NewKeychainSource()
	if kc.Available() {
		if err := kc.Set(key); err == nil {
			return nil
		}
		// Fall through to the file-based fallback on keychain errors.
	}
	return saveAuthFile(key)
}

// configFilesExist checks whether any config file already exists in the
// standard search paths (CWD then $HOME).
func configFilesExist() bool {
	home, _ := os.UserHomeDir()
	paths := []string{
		".go-cli.yaml",
		".go-cli.yml",
		".go-cli.json",
		"go-cli.yaml",
		"go-cli.yml",
		"go-cli.json",
	}
	if home != "" {
		paths = append(paths,
			filepath.Join(home, ".go-cli", "config.yaml"),
			filepath.Join(home, ".go-cli", "config.yml"),
			filepath.Join(home, ".go-cli", "config.json"),
		)
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}
