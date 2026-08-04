package config

import (
	"errors"
	"log/slog"
	"strings"
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
	minTemperature = 0.0
	maxTemperature = 2.0
)

var (
	validTracingLevels   = []string{"debug", "info", "warn", "error"}
	validTracingExporter = []string{"jsonl", "stdout", "none"}
	validCompactionStrategies = []string{"", "unified", "micro", "micro_first", "summary", "truncating"}
)

// Validate evaluates the configuration and returns a combined error listing
// every constraint violation, or nil when the config is valid.
func (v *DefaultValidator) Validate(cfg Config) error {
	var errs []string

	if cfg.Provider.Temperature < minTemperature || cfg.Provider.Temperature > maxTemperature {
		errs = append(errs, "provider temperature out of range")
	}
	if cfg.Provider.MaxTokens < 0 {
		errs = append(errs, "provider max_tokens must be non-negative")
	}

	if cfg.Model.Temperature < minTemperature || cfg.Model.Temperature > maxTemperature {
		errs = append(errs, "model temperature out of range")
	}
	if cfg.Model.MaxTokens < 0 {
		errs = append(errs, "model max_tokens must be non-negative")
	}

	if cfg.Tracing.Level != "" && !contains(validTracingLevels, cfg.Tracing.Level) {
		errs = append(errs, "invalid tracing level")
	}
	if cfg.Tracing.Exporter != "" && !contains(validTracingExporter, cfg.Tracing.Exporter) {
		errs = append(errs, "invalid tracing exporter")
	}

	if cfg.Compaction.MaxTokens <= 0 {
		errs = append(errs, "compaction max_tokens must be positive")
	}
	if !contains(validCompactionStrategies, cfg.Compaction.Strategy) {
		errs = append(errs, "invalid compaction strategy (supported: unified, micro, summary, truncating)")
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

func contains(all []string, target string) bool {
	for _, v := range all {
		if v == target {
			return true
		}
	}
	return false
}
