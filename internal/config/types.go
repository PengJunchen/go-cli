package config

// Config is the root application configuration. It is deeply nested into
// sections mirroring the go-cli configuration file / environment schema.
type Config struct {
	Provider   ProviderConfig   `json:"provider"`
	Model      ModelConfig      `json:"model"`
	Agent      AgentConfig      `json:"agent"`
	Tools      ToolsConfig      `json:"tools"`
	Tracing    TracingConfig    `json:"tracing"`
	Approval   ApprovalConfig   `json:"approval"`
	Session    SessionConfig    `json:"session"`
	Compaction CompactionConfig `json:"compaction"`
	MCP        MCPConfig        `json:"mcp"`
	Skill      SkillConfig      `json:"skill"`

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

// AgentConfig holds agent loop behavior settings.
type AgentConfig struct {
	// MaxIterations bounds the number of think → act → observe turns the
	// agent loop performs before giving up. Zero means use the built-in
	// default (200). A value of -1 disables the limit entirely.
	MaxIterations int `json:"max_iterations"`
}

// ToolsConfig controls which builtin tools and tool registries are available.
type ToolsConfig struct {
	Builtin  []string `json:"builtin"`
	Registry []string `json:"registry"`
	// Parallel enables concurrent tool execution within a single turn.
	// When nil or true, tools execute in parallel; when explicitly false,
	// tools execute sequentially.
	Parallel *bool `json:"parallel"`
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

// MCPServerConfig describes a single MCP server connection.
type MCPServerConfig struct {
	// Name is a unique identifier for the server.
	Name string `json:"name"`
	// Command is the executable to launch (stdio transport).
	Command string `json:"command"`
	// Args are command-line arguments passed to Command.
	Args []string `json:"args"`
	// URL is the server endpoint (HTTP/SSE transport).
	URL string `json:"url"`
	// Env holds optional environment variables for the server process.
	Env map[string]string `json:"env"`
}

// MCPConfig holds MCP server connection settings.
type MCPConfig struct {
	Servers []MCPServerConfig `json:"servers"`
}

// MCPServersMap holds MCP servers in the common mcpServers format:
//
//	{"server-name": {"url": "..."}, "other": {"command": "..."}}
//
// This is deserialized from JSON and then flattened into MCPConfig.Servers.
type MCPServersMap map[string]struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	URL     string            `json:"url"`
	Env     map[string]string `json:"env"`
}

// SkillConfig holds skill loading settings.
type SkillConfig struct {
	// Dir is the directory to load skill definitions from.
	Dir string `json:"dir"`
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
