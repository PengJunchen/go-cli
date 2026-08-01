package config

// Config is the root application configuration. It is deeply nested into
// sections mirroring the go-cli configuration file / environment schema.
type Config struct {
	Provider   ProviderConfig   `json:"provider"`
	Model      ModelConfig      `json:"model"`
	Tools      ToolsConfig      `json:"tools"`
	Tracing    TracingConfig    `json:"tracing"`
	Approval   ApprovalConfig   `json:"approval"`
	Session    SessionConfig    `json:"session"`
	Compaction CompactionConfig `json:"compaction"`

	verbose bool
}

// Verbose returns whether verbose output is enabled.
func (c *Config) Verbose() bool { return c.verbose }

// ProviderConfig holds the LLM provider connection settings.
type ProviderConfig struct {
	Name        string  `json:"name"`
	APIKey      string  `json:"api_key"`
	BaseURL     string  `json:"base_url"`
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
}

// ModelConfig holds the default model selection and generation parameters.
type ModelConfig struct {
	Name        string  `json:"name"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
}

// ToolsConfig controls which builtin tools and tool registries are available.
type ToolsConfig struct {
	Builtin  []string `json:"builtin"`
	Registry []string `json:"registry"`
}

// TracingConfig controls the trace logging exporter and level.
type TracingConfig struct {
	Enabled  *bool  `json:"enabled"`
	Exporter string `json:"exporter"`
	Level    string `json:"level"`
	FilePath string `json:"file_path"`
}

// ApprovalConfig controls the approval mode and classifier selection.
type ApprovalConfig struct {
	Mode       string `json:"mode"`
	Classifier string `json:"classifier"`
}

// SessionConfig controls the active session and its persistence location.
type SessionConfig struct {
	ID        string `json:"id"`
	StorePath string `json:"store_path"`
}

// CompactionConfig controls context compaction strategy and thresholds.
type CompactionConfig struct {
	Strategy  string `json:"strategy"`
	MaxTokens int    `json:"max_tokens"`
}

// Source enumerates the five configuration layers, ordered by ascending
// priority so that a later source overrides an earlier one.
type Source int

const (
	// SourceDefault is the built-in default layer, used as the base.
	SourceDefault Source = iota
	// SourceFile is the configuration file layer.
	SourceFile
	// SourceEnv is the environment variable layer.
	SourceEnv
	// SourceFlag is the CLI flag layer.
	SourceFlag
	// SourceOverride is the programmatic override layer (highest priority).
	SourceOverride
)

// String returns the human-readable name of the source layer.
func (s Source) String() string {
	if s < SourceDefault || s > SourceOverride {
		return "unknown"
	}
	return [...]string{"default", "file", "env", "flag", "override"}[s]
}
