package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	// defaultEnvPrefix is the prefix used to derive environment variable
	// names, e.g. GO_CLI_VERBOSE, GO_CLI_MODEL_NAME.
	defaultEnvPrefix = "GO_CLI"

	defaultMaxTokens = 4096

	defaultCompactionMaxTokens = 128000

	// maxExpandDepth bounds recursive environment expansion to guard against
	// self-referential variables.
	maxExpandDepth = 32

	defaultTracingExporter = "jsonl"
	defaultTracingLevel    = "info"
)

// Loader merges configuration from five ordered sources: Default < File < Env
// < Flag < Override. A later source overrides an earlier one on a per-field
// basis (Override wins).
type Loader struct {
	filePath  string
	envPrefix string
	flag      *Config
	override  *Config
	validate  Validator
}

// NewLoader returns a Loader that uses the default environment prefix and the
// default validator. Callers may chain the With* setters to add layers.
func NewLoader() *Loader {
	return &Loader{
		envPrefix: defaultEnvPrefix,
		validate:  NewDefaultValidator(),
	}
}

// WithFile sets the configuration file layer source.
func (l *Loader) WithFile(path string) *Loader {
	l.filePath = path
	return l
}

// WithEnvPrefix overrides the environment variable prefix.
func (l *Loader) WithEnvPrefix(prefix string) *Loader {
	l.envPrefix = prefix
	return l
}

// WithFlag sets the flag layer source.
func (l *Loader) WithFlag(cfg *Config) *Loader {
	l.flag = cfg
	return l
}

// WithOverride sets the override layer source (highest priority).
func (l *Loader) WithOverride(cfg *Config) *Loader {
	l.override = cfg
	return l
}

// WithValidator replaces the validator used to validate the merged config.
func (l *Loader) WithValidator(v Validator) *Loader {
	l.validate = v
	return l
}

// Load merges configuration from all configured sources in ascending priority
// order, validates the result, and returns it. It emits a config.load span
// with a sub-span per source, followed by a config.merged span describing the
// final set of resolved keys.
func (l *Loader) Load(ctx context.Context) (*Config, error) {
	span, ctx := tracing.SpanFromContext(ctx, "config.load", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.InfoContext(ctx, "config_load",
		"op", "config_load",
		"source", "merged",
		"file_path", l.filePath,
	)

	cfg := defaultConfig()
	applied := []string{SourceDefault.String()}

	// Detect the config file format from the configured path. The coexistence
	// rule is: an explicitly configured .json path wins over a sibling .yaml
	// file (the Loader uses only the path it was given), an explicit
	// .yaml/.yml path is parsed as YAML, and any unknown extension falls back
	// to JSON for backward compatibility.
	format := ConfigFormatJSON
	if l.filePath != "" {
		if f, err := DetectConfigFormat(l.filePath); err == nil {
			format = f
		} else {
			logger.DebugContext(ctx, "config_load_detect_format_fallback",
				"op", "config_load_detect_format",
				"path", l.filePath,
				"format", ConfigFormatJSON.String(),
			)
		}
	}
	span.SetAttributes(tracing.Attribute{Key: "format", Value: format.String()})

	// File layer.
	if l.filePath != "" {
		fileCfg, err := l.loadFile(ctx, format)
		if err != nil {
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return nil, err
		}
		cfg = mergeConfigs(cfg, fileCfg)
		applied = append(applied, SourceFile.String())
	}

	// Env layer.
	envCfg := loadFromEnv(l.envPrefix)
	cfg = mergeConfigs(cfg, envCfg)
	applied = append(applied, SourceEnv.String())

	// Flag layer.
	if l.flag != nil {
		cfg = mergeConfigs(cfg, l.flag)
		applied = append(applied, SourceFlag.String())
	}

	// Override layer (highest priority).
	if l.override != nil {
		cfg = mergeConfigs(cfg, l.override)
		applied = append(applied, SourceOverride.String())
	}

	cfg.verbose = l.resolveVerbose()

	// Emit the merged span describing the final resolved config.
	mergedSpan, _ := tracing.SpanFromContext(ctx, "config.merged", tracing.SpanKindInternal)
	mergedSpan.SetAttributes(
		tracing.Attribute{Key: "sources_merged", Value: len(applied)},
		tracing.Attribute{Key: "final_keys", Value: countKeys(cfg)},
	)
	mergedSpan.SetStatus(tracing.SpanStatusOK, "")
	mergedSpan.End()

	if err := l.validate.Validate(*cfg); err != nil {
		logger.ErrorContext(ctx, "config_error",
			"op", "config_error",
			"error_type", "validation_failed",
			"error", err,
		)
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "config_keys", Value: countKeys(cfg)})
	span.SetStatus(tracing.SpanStatusOK, "")
	return cfg, nil
}

// loadFile reads the configuration file (JSON or YAML, chosen by format) into
// a partial Config and emits a per-source trace span.
func (l *Loader) loadFile(ctx context.Context, format ConfigFormat) (*Config, error) {
	span, _ := tracing.SpanFromContext(ctx, "config.load.file", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(
		tracing.Attribute{Key: "source", Value: SourceFile.String()},
		tracing.Attribute{Key: "path", Value: l.filePath},
		tracing.Attribute{Key: "format", Value: format.String()},
	)

	data, err := os.ReadFile(l.filePath)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	if err := UnmarshalConfig(data, format, &cfg); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, fmt.Errorf("parse config file %s as %s: %w", l.filePath, format, err)
	}

	span.SetAttributes(tracing.Attribute{Key: "config_keys", Value: countKeys(&cfg)})
	span.SetStatus(tracing.SpanStatusOK, "")
	return &cfg, nil
}

// resolveVerbose resolves the verbose flag respecting source priority:
// override > flag > env > default(false).
func (l *Loader) resolveVerbose() bool {
	if l.override != nil && l.override.verbose {
		return true
	}
	if l.flag != nil && l.flag.verbose {
		return true
	}
	return os.Getenv(envKey(l.envPrefix, "VERBOSE")) == "1"
}

// envKey returns the environment variable name for the given key using the
// configured prefix.
func envKey(prefix, key string) string { return prefix + "_" + key }

// loadFromEnv builds a partial Config from GO_CLI_* environment variables.
func loadFromEnv(prefix string) *Config {
	cfg := &Config{}
	if os.Getenv(envKey(prefix, "VERBOSE")) == "1" {
		cfg.verbose = true
	}
	cfg.Provider.Name = os.Getenv(envKey(prefix, "PROVIDER_NAME"))
	cfg.Provider.APIKey = os.Getenv(envKey(prefix, "PROVIDER_API_KEY"))
	cfg.Provider.BaseURL = os.Getenv(envKey(prefix, "PROVIDER_BASE_URL"))
	cfg.Provider.Model = os.Getenv(envKey(prefix, "PROVIDER_MODEL"))
	cfg.Provider.Temperature = parseFloatEnv(envKey(prefix, "PROVIDER_TEMPERATURE"))
	cfg.Provider.MaxTokens = parseIntEnv(envKey(prefix, "PROVIDER_MAX_TOKENS"))

	cfg.Model.Name = os.Getenv(envKey(prefix, "MODEL_NAME"))
	cfg.Model.Temperature = parseFloatEnv(envKey(prefix, "MODEL_TEMPERATURE"))
	cfg.Model.MaxTokens = parseIntEnv(envKey(prefix, "MODEL_MAX_TOKENS"))

	cfg.Tracing.Exporter = os.Getenv(envKey(prefix, "TRACING_EXPORTER"))
	cfg.Tracing.Level = os.Getenv(envKey(prefix, "TRACING_LEVEL"))
	cfg.Tracing.FilePath = os.Getenv(envKey(prefix, "TRACING_FILE_PATH"))
	if v, ok := parseBoolEnv(envKey(prefix, "TRACING_ENABLED")); ok {
		cfg.Tracing.Enabled = &v
	}

	cfg.Approval.Mode = os.Getenv(envKey(prefix, "APPROVAL_MODE"))
	cfg.Approval.Classifier = os.Getenv(envKey(prefix, "APPROVAL_CLASSIFIER"))

	cfg.Compaction.Strategy = os.Getenv(envKey(prefix, "COMPACTION_STRATEGY"))
	cfg.Compaction.MaxTokens = parseIntEnv(envKey(prefix, "COMPACTION_MAX_TOKENS"))

	// LSP server command: space-separated string (e.g. "gopls serve").
	if cmd := os.Getenv(envKey(prefix, "LSP_SERVER_COMMAND")); cmd != "" {
		cfg.LSP.ServerCommand = parseCommandString(cmd)
	}
	if root := os.Getenv(envKey(prefix, "LSP_WORKSPACE_ROOT")); root != "" {
		cfg.LSP.WorkspaceRoot = root
	}

	return cfg
}

// parseCommandString splits a command string by whitespace into a command
// and its arguments, suitable for the LSP ServerCommand field.
func parseCommandString(s string) []string {
	return strings.Fields(s)
}

// defaultConfig returns the built-in default configuration.
func defaultConfig() *Config {
	enabled := true
	auditEnabled := true
	return &Config{
		Provider: ProviderConfig{MaxTokens: defaultMaxTokens},
		Model:    ModelConfig{MaxTokens: defaultMaxTokens},
		Tracing: TracingConfig{
			Enabled:  &enabled,
			Exporter: defaultTracingExporter,
			Level:    defaultTracingLevel,
		},
		Compaction: CompactionConfig{
			Strategy:  "micro_first",
			MaxTokens: defaultCompactionMaxTokens,
		},
		Production: ProductionConfig{
			Audit: AuditConfig{Enabled: &auditEnabled},
		},
	}
}

// mergeConfigs returns a new Config overlaying non-zero values of over onto
// base, giving over higher priority on a per-field basis.
func mergeConfigs(base, over *Config) *Config {
	out := new(Config)
	*out = *base
	overlayValue(reflect.ValueOf(out).Elem(), reflect.ValueOf(over).Elem())
	return out
}

// overlayValue copies non-zero exported fields from src onto dst recursively.
// Pointers are replaced wholesale (nil means "unset"), enabling explicit
// false/zero overrides.
func overlayValue(dst, src reflect.Value) {
	if !dst.IsValid() || !src.IsValid() || dst.Type() != src.Type() {
		return
	}
	switch dst.Kind() {
	case reflect.Struct:
		for i := 0; i < dst.NumField(); i++ {
			if dst.Type().Field(i).PkgPath != "" {
				continue // skip unexported fields.
			}
			overlayValue(dst.Field(i), src.Field(i))
		}
	case reflect.String:
		if v := src.String(); v != "" {
			dst.SetString(v)
		}
	case reflect.Bool:
		if src.Bool() {
			dst.SetBool(true)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v := src.Int(); v != 0 {
			dst.SetInt(v)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if v := src.Uint(); v != 0 {
			dst.SetUint(v)
		}
	case reflect.Float32, reflect.Float64:
		if v := src.Float(); v != 0 {
			dst.SetFloat(v)
		}
	case reflect.Pointer:
		if !src.IsNil() {
			dst.Set(src)
		}
	case reflect.Slice:
		if src.Len() > 0 {
			dst.Set(src)
		}
	case reflect.Map:
		if src.Len() > 0 {
			dst.Set(src)
		}
	}
}

// countKeys returns the number of resolved (non-zero / set) leaf config keys
// in a Config, for span attributes and diagnostics.
func countKeys(c *Config) int {
	if c == nil {
		return 0
	}
	n := 0
	countReflect(reflect.ValueOf(c).Elem(), &n)
	return n
}

func countReflect(v reflect.Value, n *int) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue
			}
			countReflect(v.Field(i), n)
		}
	case reflect.String:
		if v.String() != "" {
			*n++
		}
	case reflect.Bool:
		if v.Bool() {
			*n++
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Int() != 0 {
			*n++
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if v.Uint() != 0 {
			*n++
		}
	case reflect.Float32, reflect.Float64:
		if v.Float() != 0 {
			*n++
		}
	case reflect.Pointer:
		if !v.IsNil() {
			*n++
		}
	case reflect.Slice:
		if v.Len() > 0 {
			*n++
		}
	case reflect.Map:
		if v.Len() > 0 {
			*n++
		}
	}
}

func parseFloatEnv(key string) float64 {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return f
}

func parseIntEnv(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return i
}

func parseBoolEnv(key string) (bool, bool) {
	v := os.Getenv(key)
	if v == "" {
		return false, false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

// ExpandEnv expands ${VAR} and $VAR references in input using the process
// environment. Expansion is recursive, so a variable whose value contains
// other variables is expanded to a fixed point (bounded to avoid cycles).
func ExpandEnv(input string) string {
	return expandEnvDeep(input, 0)
}

func expandEnvDeep(input string, depth int) string {
	if depth > maxExpandDepth {
		return input
	}
	return os.Expand(input, func(key string) string {
		val, ok := os.LookupEnv(key)
		if !ok {
			return ""
		}
		return expandEnvDeep(val, depth+1)
	})
}
