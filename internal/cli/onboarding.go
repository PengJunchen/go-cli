package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pengjunchen/go-cli/internal/config"
)

// onboardingModelChoices lists the models offered during the first-run wizard.
var onboardingModelChoices = []struct {
	Name string
	Desc string
}{
	{"gpt-4o-mini", "Fast and affordable (OpenAI)"},
	{"gpt-4o", "Most capable (OpenAI)"},
	{"claude-3-5-sonnet", "Balanced (Anthropic)"},
	{"claude-3-opus", "Most capable (Anthropic)"},
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
		if err := promptModel(cfg, reader, out); err != nil {
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
func promptAPIKey(cfg *config.Config, reader *bufio.Reader, out io.Writer) error {
	for {
		fmt.Fprintln(out, "Step 1: API Key")
		fmt.Fprintln(out, "  Enter your LLM provider API key (e.g. sk-...).")
		fmt.Fprint(out, "  API Key: ")
		line, err := reader.ReadString('\n')
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

// promptModel lists available models and lets the user pick one. An invalid or
// empty choice falls back to the first model in the list.
func promptModel(cfg *config.Config, reader *bufio.Reader, out io.Writer) error {
	fmt.Fprintln(out, "Step 2: Model Selection")
	for i, m := range onboardingModelChoices {
		fmt.Fprintf(out, "  %d. %-22s %s\n", i+1, m.Name, m.Desc)
	}
	fmt.Fprintf(out, "  Choose a model [1-%d] (default: 1): ", len(onboardingModelChoices))

	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("onboarding: read model choice: %w", err)
	}
	line = strings.TrimSpace(line)

	idx := 0
	if line != "" {
		n, parseErr := strconv.Atoi(line)
		if parseErr != nil || n < 1 || n > len(onboardingModelChoices) {
			fmt.Fprintf(out, "  Invalid choice, using default: %s\n", onboardingModelChoices[0].Name)
		} else {
			idx = n - 1
		}
	}

	chosen := onboardingModelChoices[idx].Name
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
// onboardingConfigPathFunc, creating the parent directory if needed.
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

	fmt.Fprintf(out, "✓ Configuration saved to %s\n", path)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "You're all set! Type 'exit' or press Ctrl+C to quit.")
	fmt.Fprintln(out, "")
	return nil
}

// yamlSingleQuote wraps s in YAML single-quote style. In single-quoted YAML
// strings every character is literal except the single quote itself, which is
// escaped by doubling it (” → '). This safely handles special YAML
// characters such as :, #, {, }, ", and \ in values without relying on
// escape-sequence processing by the reader.
func yamlSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// serializeOnboardingYAML produces a simple YAML representation of the config
// fields set by the onboarding wizard. Only the provider, model, and tui
// sections are written so the output stays minimal and human-editable.
func serializeOnboardingYAML(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("# go-cli configuration (generated by onboarding wizard)\n\n")

	b.WriteString("provider:\n")
	if cfg.Provider.Name != "" {
		fmt.Fprintf(&b, "  name: %s\n", cfg.Provider.Name)
	}
	if cfg.Provider.APIKey != "" {
		fmt.Fprintf(&b, "  api_key: %s\n", yamlSingleQuote(cfg.Provider.APIKey))
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
