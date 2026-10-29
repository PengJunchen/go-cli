package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// Validator defines the contract for validating a merged Config.
type Validator interface {
	// Validate checks the configuration for missing, invalid or out-of-range
	// values and returns an error describing all problems found.
	Validate(Config) error
}

// DefaultValidator is the default Validator implementation. It validates
// required fields and numeric/type/range constraints.
type DefaultValidator struct {
	logger *slog.Logger
}

// Compile-time assertion that DefaultValidator satisfies Validator.
var _ Validator = (*DefaultValidator)(nil)

// NewDefaultValidator returns a DefaultValidator that logs validation
// diagnostics to slog.Default().
func NewDefaultValidator() Validator {
	return &DefaultValidator{logger: slog.Default()}
}

// minTemperature and maxTemperature bound the allowed model temperature.
const (
	minTemperature  = 0.0
	maxTemperature  = 2.0
	maxModelTimeout = 24 * time.Hour
)

func validTemperature(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minTemperature && value <= maxTemperature
}

var (
	validTracingLevels            = []string{"debug", "info", "warn", "error"}
	validCompactionStrategies     = []string{"", "unified", "micro", "micro_first", "summary", "truncating"}
	validGitPlatforms             = []string{"", "github", "gitlab", "bitbucket"}
	validWebSearchProviders       = []string{"", "mock", "fetch", "brave"}
	validTUIThemes                = []string{"", "dark", "light", "monokai", "solarized", "auto"}
	validTUIDiffStyles            = []string{"", defaultTUIDiffStyle}
	validLoopDetectorDispositions = []string{"", "warn", "steer", "terminate"}
)

// Validate evaluates the configuration and returns a combined error listing
// every constraint violation, or nil when the config is valid.
func (v *DefaultValidator) Validate(cfg Config) error {
	var errs []string
	if _, _, err := cfg.Tools.Bash.ExecutionTimeout(); err != nil {
		errs = append(errs, err.Error())
	}
	if cfg.Runtime.Profile != "" && cfg.Runtime.Profile != "default" && cfg.Runtime.Profile != "edge" {
		errs = append(errs, "invalid runtime profile (supported: default, edge)")
	}
	if cfg.MCP.Loading != "" && cfg.MCP.Loading != "eager" && cfg.MCP.Loading != "lazy" {
		errs = append(errs, "invalid mcp loading (supported: eager, lazy)")
	}
	if cfg.Runtime.MaxSkills < 0 || cfg.Runtime.MaxSkills > 1024 {
		errs = append(errs, "runtime max_skills must be between 0 and 1024")
	}
	if cfg.Runtime.MaxMCPServers < 0 || cfg.Runtime.MaxMCPServers > 128 {
		errs = append(errs, "runtime max_mcp_servers must be between 0 and 128")
	}
	if cfg.Runtime.MaxPromptBytes < 0 || cfg.Runtime.MaxPromptBytes > 16<<20 {
		errs = append(errs, "runtime max_prompt_bytes must be between 0 and 16777216")
	}

	if !validTemperature(cfg.Provider.Temperature) {
		errs = append(errs, "provider temperature out of range")
	}
	if cfg.Provider.MaxTokens < 0 {
		errs = append(errs, "provider max_tokens must be non-negative")
	}

	if !validTemperature(cfg.Model.Temperature) {
		errs = append(errs, "model temperature out of range")
	}
	if cfg.Model.MaxTokens < 0 {
		errs = append(errs, "model max_tokens must be non-negative")
	}

	if !validTemperature(cfg.SmallModel.Temperature) {
		errs = append(errs, "small model temperature out of range")
	}
	if cfg.SmallModel.MaxTokens < 0 {
		errs = append(errs, "small model max_tokens must be non-negative")
	}

	if cfg.Tracing.Level != "" && !contains(validTracingLevels, cfg.Tracing.Level) {
		errs = append(errs, "invalid tracing level")
	}
	if cfg.Tracing.Exporter != "" && !tracing.IsSupportedExporter(cfg.Tracing.Exporter) {
		errs = append(errs, "invalid tracing exporter")
	}

	if cfg.Compaction.MaxTokens <= 0 {
		errs = append(errs, "compaction max_tokens must be positive")
	}
	if !contains(validCompactionStrategies, cfg.Compaction.Strategy) {
		errs = append(errs, "invalid compaction strategy (supported: unified, micro, summary, truncating)")
	}

	// Git config: when APIToken is non-empty, Platform must be one of the
	// supported platforms.
	if cfg.Git.APIToken != "" && !contains(validGitPlatforms, cfg.Git.Platform) {
		errs = append(errs, "invalid git platform (supported: github, gitlab, bitbucket) required when api_token is set")
	}
	if !contains(validWebSearchProviders, cfg.WebSearch.Provider) {
		errs = append(errs, "invalid web_search provider (supported: fetch, brave; empty and mock remain unavailable)")
	}
	if !contains(validLoopDetectorDispositions, cfg.Production.LoopDetector.Disposition) {
		errs = append(errs, "invalid production.loop_detector disposition (supported: warn, steer, terminate)")
	}
	validateProductionModelTimeout(cfg.Production.ModelTimeout, &errs)

	// LSP config: when ServerCommand is non-empty, WorkspaceRoot (if set)
	// must be an existing directory.
	if len(cfg.LSP.ServerCommand) > 0 && cfg.LSP.WorkspaceRoot != "" {
		if fi, err := os.Stat(cfg.LSP.WorkspaceRoot); err != nil || !fi.IsDir() {
			errs = append(errs, "lsp workspace_root does not exist or is not a directory")
		}
	}
	for i, srv := range cfg.LSP.Servers {
		if len(srv.ServerCommand) > 0 && srv.WorkspaceRoot != "" {
			if fi, err := os.Stat(srv.WorkspaceRoot); err != nil || !fi.IsDir() {
				errs = append(errs, fmt.Sprintf("lsp servers[%d] workspace_root does not exist or is not a directory", i))
			}
		}
	}

	if !contains(validTUIThemes, cfg.TUI.Theme) {
		errs = append(errs, "invalid tui theme (supported: dark, light, monokai, solarized, auto)")
	}
	if !contains(validTUIDiffStyles, cfg.TUI.DiffStyle) {
		errs = append(errs, "invalid tui diff_style (supported: unified)")
	}
	if cfg.TUI.WordWrap < 0 {
		errs = append(errs, "tui word_wrap must be non-negative")
	}

	// Security: BaseURL HTTPS verification. Non-HTTPS URLs to non-localhost
	// hosts risk exposing API keys over unencrypted connections, so log a
	// warning rather than hard-failing to allow internal HTTP endpoints.
	if cfg.Provider.BaseURL != "" {
		if err := validateBaseURLHTTPS(cfg.Provider.BaseURL, "provider"); err != nil {
			v.logger.Warn("insecure_base_url",
				"op", "config_validate",
				"component", "provider",
				"reason", err.Error(),
			)
		}
	}
	if cfg.SmallModel.BaseURL != "" {
		if err := validateBaseURLHTTPS(cfg.SmallModel.BaseURL, "small_model"); err != nil {
			v.logger.Warn("insecure_base_url",
				"op", "config_validate",
				"component", "small_model",
				"reason", err.Error(),
			)
		}
	}

	// Security: Path traversal check. Rejects file paths that escape their
	// expected directory via ".." components after cleaning.
	for _, p := range []struct{ value, name string }{
		{cfg.Tracing.FilePath, "tracing file_path"},
		{cfg.Session.StorePath, "session store_path"},
		{cfg.Memory.StorePath, "memory store_path"},
		{cfg.Skill.Dir, "skill dir"},
		{cfg.Commands.Dir, "commands dir"},
		{cfg.Production.Audit.Path, "audit path"},
		{cfg.ModelRegistry.CachePath, "model_registry cache_path"},
		{cfg.History.Path, "history path"},
		{cfg.Git.WorkDir, "git workdir"},
		{cfg.Git.WorktreeDir, "git worktree_dir"},
		{cfg.LSP.WorkspaceRoot, "lsp workspace_root"},
	} {
		if err := validatePathTraversal(p.value, p.name); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for i, srv := range cfg.LSP.Servers {
		if err := validatePathTraversal(srv.WorkspaceRoot, fmt.Sprintf("lsp servers[%d] workspace_root", i)); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for name, host := range cfg.Remote.Hosts {
		if err := validatePathTraversal(host.KeyPath, fmt.Sprintf("remote host %q key_path", name)); err != nil {
			errs = append(errs, err.Error())
		}
		if err := validatePathTraversal(host.KnownHostsPath, fmt.Sprintf("remote host %q known_hosts_path", name)); err != nil {
			errs = append(errs, err.Error())
		}
	}

	// Security: SSH password storage warning. Plaintext passwords are valid
	// but insecure; log a warning recommending key-based authentication.
	for name, host := range cfg.Remote.Hosts {
		if host.Password != "" {
			v.logger.Warn("insecure_ssh_password",
				"op", "config_validate",
				"host", name,
				"reason", "plaintext SSH password detected; consider using key-based authentication instead",
			)
		}
	}

	if len(errs) > 0 {
		v.logger.Warn("config_validation_failed",
			"op", "config_validate",
			"error_count", len(errs),
			"reasons", strings.Join(errs, "; "),
		)
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func validateProductionModelTimeout(cfg ModelTimeoutConfig, errs *[]string) {
	total := 30 * time.Second
	idle := 10 * time.Second
	totalValid := true
	idleValid := true
	if cfg.Total != "" {
		parsed, err := time.ParseDuration(cfg.Total)
		if err != nil || parsed <= 0 || parsed > maxModelTimeout {
			*errs = append(*errs, "production.model_timeout.total must be a duration greater than 0 and no more than 24h")
			totalValid = false
		} else {
			total = parsed
		}
	}
	if cfg.StreamIdle != "" {
		parsed, err := time.ParseDuration(cfg.StreamIdle)
		if err != nil || parsed <= 0 || parsed > maxModelTimeout {
			*errs = append(*errs, "production.model_timeout.stream_idle must be a duration greater than 0 and no more than 24h")
			idleValid = false
		} else {
			idle = parsed
		}
	}
	if totalValid && idleValid && idle > total {
		*errs = append(*errs, "production.model_timeout.stream_idle must be no greater than total")
	}
}

func contains(all []string, target string) bool {
	for _, v := range all {
		if v == target {
			return true
		}
	}
	return false
}

// validateBaseURLHTTPS returns an error when the URL uses a non-HTTPS scheme
// and does not point to localhost or 127.0.0.1, which would risk exposing API
// keys over unencrypted connections. URLs without an explicit scheme are
// skipped because they are ambiguous shorthand handled by the HTTP client.
func validateBaseURLHTTPS(rawURL, label string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%s base_url is not a valid URL", label)
	}
	if u.Scheme == "" {
		return nil
	}
	if u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		return fmt.Errorf("%s base_url must use HTTPS to avoid exposing API keys over unencrypted connections (got scheme %q)", label, u.Scheme)
	}
	return nil
}

// validatePathTraversal returns an error when the cleaned path still contains
// a ".." component, indicating the path traverses outside its base directory.
func validatePathTraversal(path, label string) error {
	if path == "" {
		return nil
	}
	cleaned := filepath.Clean(path)
	if strings.Contains(cleaned, "..") {
		return fmt.Errorf("%s contains path traversal (..) sequence", label)
	}
	return nil
}
