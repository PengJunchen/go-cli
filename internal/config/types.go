package config

import "time"

// Config is the root application configuration. It is deeply nested into
// sections mirroring the go-cli configuration file / environment schema.
type Config struct {
	Provider      ProviderConfig      `json:"provider"`
	Model         ModelConfig         `json:"model"`
	SmallModel    SmallModelConfig    `json:"small_model"`
	Agent         AgentConfig         `json:"agent"`
	Tools         ToolsConfig         `json:"tools"`
	Tracing       TracingConfig       `json:"tracing"`
	Approval      ApprovalConfig      `json:"approval"`
	Session       SessionConfig       `json:"session"`
	Compaction    CompactionConfig    `json:"compaction"`
	MCP           MCPConfig           `json:"mcp"`
	Skill         SkillConfig         `json:"skill"`
	Commands      CommandsConfig      `json:"commands"`
	WebSearch     WebSearchConfig     `json:"web_search"`
	Production    ProductionConfig    `json:"production"`
	Sandbox       SandboxConfig       `json:"sandbox"`
	LSP           LSPConfig           `json:"lsp"`
	Remote        RemoteConfig        `json:"remote"`
	Extensions    ExtensionsConfig    `json:"extensions"`
	ACP           ACPConfig           `json:"acp"`
	Git           GitConfig           `json:"git"`
	ModelCycler   ModelCyclerConfig   `json:"model_cycler"`
	ModelRegistry ModelRegistryConfig `json:"model_registry"`
	History       HistoryConfig       `json:"history"`
	TUI           TUIConfig           `json:"tui" yaml:"tui"`

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

// SmallModelConfig holds settings for a lightweight model used for background
// tasks (summaries, title generation, memory extraction). When Provider and
// Model are empty, the small model is not built and all tasks fall back to the
// primary model.
type SmallModelConfig struct {
	Provider    string  `json:"provider"`
	APIKey      string  `json:"api_key"`
	BaseURL     string  `json:"base_url"`
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
}

// AgentConfig holds agent loop behavior settings.
type AgentConfig struct {
	// MaxIterations bounds the number of think -> act -> observe turns the
	// agent loop performs before giving up. Zero means use the built-in
	// default (200). A value of -1 disables the limit entirely.
	MaxIterations int `json:"max_iterations"`
	// SystemPrompt, when non-empty, replaces the default base system prompt
	// entirely. This corresponds to the content of a SYSTEM.md file.
	SystemPrompt string `json:"system_prompt"`
	// AppendSystemPrompt, when non-empty, is appended to the end of the
	// assembled system prompt. This corresponds to the content of an
	// APPEND_SYSTEM.md file.
	AppendSystemPrompt string `json:"append_system_prompt"`
	// Thinking controls the LLM reasoning depth. Valid values:
	// none|minimal|low|medium|high|max. Empty defaults to medium.
	Thinking string `json:"thinking"`
}

// ToolsConfig controls which builtin tools and tool registries are available.
type ToolsConfig struct {
	Builtin     []string           `json:"builtin"`
	Registry    []string           `json:"registry"`
	CustomTools []CustomToolConfig `json:"custom_tools"`
	// Parallel enables concurrent tool execution within a single turn.
	// When nil or true, tools execute in parallel; when explicitly false,
	// tools execute sequentially.
	Parallel *bool `json:"parallel"`
}

// CustomToolConfig describes a user-defined command-line tool that wraps an
// external executable as a ToolDefinition. The executable receives the dynamic
// "input" argument appended after the static Args.
type CustomToolConfig struct {
	// Name is the unique tool name exposed to the agent.
	Name string `json:"name"`
	// Description is a human-readable description of what the tool does.
	Description string `json:"description"`
	// Command is the executable (Command[0]) and its base arguments
	// (Command[1:]). At least one element is required.
	Command []string `json:"command"`
	// Args are static arguments appended after the base command arguments and
	// before the dynamic input.
	Args []string `json:"args"`
	// Env holds optional environment variables applied on top of the process
	// environment.
	Env map[string]string `json:"env"`
	// Timeout bounds execution in seconds. Zero means no timeout.
	Timeout int `json:"timeout"`
	// WorkingDir is the directory the command runs in. Empty inherits the
	// process working directory.
	WorkingDir string `json:"working_dir"`
}

// TracingConfig controls the trace logging exporter and level.
type TracingConfig struct {
	Enabled        *bool  `json:"enabled"`
	Exporter       string `json:"exporter"`
	Level          string `json:"level"`
	FilePath       string `json:"file_path"`
	RedactionLevel string `json:"redaction_level"` // full, redact (default), off
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
	// GitAwareBranch, when true, enables Git branch linkage during session
	// fork/resume operations. Defaults to false for backward compatibility.
	// Pointer allows explicit false override via env/flag.
	GitAwareBranch *bool `json:"git_aware_branch"`
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

// CommandsConfig holds custom slash command loading settings.
type CommandsConfig struct {
	// Dir is the directory to load custom Markdown command definitions from.
	// When empty, auto-discovery checks .go-cli/commands and
	// ~/.config/go-cli/commands.
	Dir string `json:"dir"`
}

// WebSearchConfig controls the web search provider selection. When Provider
// is empty or "mock", the MockSearchProvider is used (default). "fetch" selects
// the DuckDuckGo HTML scraping provider, and "brave" selects the Brave Search
// API provider (requires APIKey).
type WebSearchConfig struct {
	Provider string `json:"provider"` // "mock" (default), "fetch", "brave"
	APIKey   string `json:"api_key"`  // for brave provider
	Timeout  string `json:"timeout"`  // duration string, default "10s"
}

// ProductionConfig holds production resilience settings (circuit breaker, loop
// detection, audit logging).
type ProductionConfig struct {
	CircuitBreaker CircuitBreakerConfig `json:"circuit_breaker"`
	LoopDetector   LoopDetectorConfig   `json:"loop_detector"`
	Audit          AuditConfig          `json:"audit"`
}

// AuditConfig controls the audit log that records tool calls and their
// outcomes as JSON-lines for later inspection.
type AuditConfig struct {
	// Enabled controls whether the audit log is active. Defaults to true
	// when nil; set to a pointer to false to explicitly disable.
	Enabled *bool `json:"enabled"`
	// Path is the JSONL file path where audit entries are appended.
	Path string `json:"path"`
}

// CircuitBreakerConfig tunes the model-protection circuit breaker.
type CircuitBreakerConfig struct {
	// Threshold is the number of consecutive failures before the breaker opens.
	Threshold int `json:"threshold"`
	// ResetTimeout is how long the breaker stays open before probing.
	ResetTimeout time.Duration `json:"reset_timeout"`
}

// LoopDetectorConfig tunes the agent loop detector thresholds.
type LoopDetectorConfig struct {
	// EditThreshold triggers when a single file is edited this many times.
	EditThreshold int `json:"edit_threshold"`
	// TestFailureThreshold triggers after this many consecutive test failures.
	TestFailureThreshold int `json:"test_failure_threshold"`
	// SameToolCallThreshold triggers after this many repeated identical tool calls.
	SameToolCallThreshold int `json:"same_tool_call_threshold"`
}

// SandboxConfig controls the workspace sandbox applied to the bash tool.
type SandboxConfig struct {
	// AllowedPaths restricts command execution to these directories (and
	// their subdirectories). When empty, the current working directory is
	// used as a safe default.
	AllowedPaths []string `json:"allowed_paths"`
	// MaxCPU bounds the CPU time a single command may consume.
	MaxCPU time.Duration `json:"max_cpu"`
	// MaxMemory bounds the maximum memory (in bytes) a single command may
	// consume.
	MaxMemory int64 `json:"max_memory"`
}

// LSPServerConfig describes a single LSP server instance bound to specific
// file extensions.
type LSPServerConfig struct {
	// ServerCommand is the command and arguments used to launch the LSP
	// server subprocess (e.g. ["gopls", "serve"]).
	ServerCommand []string `json:"server_command" yaml:"server_command"`
	// WorkspaceRoot is the root directory of the workspace, passed as the
	// LSP root URI. When empty, the current working directory is used.
	WorkspaceRoot string `json:"workspace_root" yaml:"workspace_root"`
	// FileExtensions lists the file extensions (without dot, e.g. "go",
	// "ts") that this server handles.
	FileExtensions []string `json:"file_extensions" yaml:"file_extensions"`
}

// LSPConfig controls the Language Server Protocol integration.
type LSPConfig struct {
	// ServerCommand is the command and arguments used to launch the LSP
	// server subprocess (e.g. ["gopls", "serve"]). This is the legacy
	// single-server field; when set, it is treated as a single-element
	// Servers list for backward compatibility.
	ServerCommand []string `json:"server_command" yaml:"server_command"`
	// WorkspaceRoot is the root directory of the workspace, passed as the
	// LSP root URI. When empty, the current working directory is used.
	WorkspaceRoot string `json:"workspace_root" yaml:"workspace_root"`
	// Servers allows configuring multiple LSP servers, each handling
	// different file extensions. When non-empty, this takes precedence
	// over the legacy ServerCommand/WorkspaceRoot fields.
	Servers []LSPServerConfig `json:"servers" yaml:"servers"`
}

// RemoteConfig holds SSH remote execution settings.
type RemoteConfig struct {
	// Hosts maps host names to their SSH connection configurations.
	Hosts map[string]SSHHostConfig `json:"hosts"`
	// DefaultHost is the host name used when the tool's host argument is
	// omitted. It must match a key in Hosts.
	DefaultHost string `json:"default_host"`
}

// SSHHostConfig describes a single SSH host connection.
type SSHHostConfig struct {
	// Host is the hostname or IP address of the remote server.
	Host string `json:"host"`
	// Port is the SSH port (0 or 22 means the default 22).
	Port int `json:"port"`
	// User is the SSH login user.
	User string `json:"user"`
	// KeyPath is the path to the private key file for key-based auth.
	KeyPath string `json:"key_path"`
	// Password is the password for password-based auth (requires sshpass).
	Password string `json:"password"`
	// KnownHostsPath is the path to the known_hosts file for host key
	// verification.
	KnownHostsPath string `json:"known_hosts_path"`
}

// ExtensionsConfig controls the plugin/extension ecosystem. When Enabled is
// true and PluginPaths is non-empty, the CLI loads extensions from each path
// during assembly and initializes them before the agent loop starts.
type ExtensionsConfig struct {
	// PluginPaths is the list of .so file paths or HTTP endpoints to load
	// extensions from.
	PluginPaths []string `json:"plugin_paths"`
	// Enabled controls whether extension loading is active.
	// Pointer allows explicit false override via env/flag.
	Enabled *bool `json:"enabled"`
}

// ACPConfig controls Agent Communication Protocol (ACP) multi-agent
// communication. When Transport and Endpoints are both non-empty, the CLI
// creates an ACPClient, connects to the first endpoint, and wires an
// ACPMiddlewareAdapter into the loop middleware chain so inbound ACP messages
// are dispatched to sub-agents.
type ACPConfig struct {
	// Transport selects the wire format: "stdio" (newline-delimited JSON over
	// stdin/stdout) or "grpc" (JSON-over-HTTP). When empty, ACP is disabled.
	Transport string `json:"transport"`
	// Endpoints lists the peer endpoints. For "grpc" transport each entry is an
	// HTTP URL; for "stdio" the first entry is informational (stdin/stdout are
	// used automatically).
	Endpoints []string `json:"endpoints"`
	// Timeout bounds ACP operations in seconds. Zero means no explicit timeout.
	Timeout int `json:"timeout"`
}

// GitConfig controls Git integration settings: the working directory for git
// operations, default remote, branch prefix, auto-commit behavior, and the
// platform for PR creation.
type GitConfig struct {
	// Enabled controls whether git tools are registered. Defaults to false.
	// Pointer allows explicit false override via env/flag.
	Enabled *bool `json:"enabled"`
	// WorkDir is the working directory for git commands. When empty, the
	// process working directory is used.
	WorkDir string `json:"workdir"`
	// DefaultRemote is the default remote name (e.g. "origin").
	DefaultRemote string `json:"default_remote"`
	// BranchPrefix is an optional prefix applied to branches created by the
	// session fork --git command.
	BranchPrefix string `json:"branch_prefix"`
	// AutoCommit enables automatic committing of changes after tool mutations.
	// Pointer allows explicit false override via env/flag.
	AutoCommit *bool `json:"auto_commit"`
	// Platform selects the hosting platform for PR creation: "github",
	// "gitlab", or "bitbucket". Required when APIToken is non-empty.
	Platform string `json:"platform"`
	// APIToken is the API token for the hosting platform. When non-empty,
	// Platform must be set.
	APIToken string `json:"api_token"`
}

// ModelCyclerConfig controls model rotation across multiple providers. When
// Enabled is true and Models is non-empty, the CLI creates a ModelCycler that
// rotates model selection across the configured providers using the specified
// Strategy.
type ModelCyclerConfig struct {
	// Enabled controls whether model cycling is active.
	// Pointer allows explicit false override via env/flag.
	Enabled *bool `json:"enabled"`
	// Strategy selects the rotation strategy: round_robin, weighted, or
	// cost_priority. Empty defaults to round_robin.
	Strategy string `json:"strategy"`
	// Models lists the models in the rotation pool.
	Models []ModelEntry `json:"models"`
}

// ModelEntry represents a single model in the rotation pool.
type ModelEntry struct {
	// Provider is the provider name (e.g. openai, claude, gemini).
	Provider string `json:"provider"`
	// Model is the model name (e.g. gpt-4o, claude-3).
	Model string `json:"model"`
	// Weight is used by the weighted and cost_priority strategies.
	Weight int `json:"weight"`
	// TaskType optionally tags this model for a specific task type
	// (chat, summary, title, extraction). When non-empty, the cycler
	// prefers this model for calls with a matching task type.
	TaskType string `json:"task_type,omitempty"`
}

// ModelRegistryConfig controls the models.dev model registry integration. When
// Enabled is true, a ModelsDevRegistry is created and refreshed on startup so
// model metadata (pricing, context window, modality) is available to the CLI.
type ModelRegistryConfig struct {
	Enabled   bool   `json:"enabled"`
	CachePath string `json:"cache_path,omitempty"`
	TTLHours  int    `json:"ttl_hours,omitempty"`
}

// HistoryConfig controls REPL command history persistence.
type HistoryConfig struct {
	Path   string `yaml:"path" json:"path"`
	MaxLen int    `yaml:"max_len" json:"max_len"`
}

// TUIConfig configures the terminal UI layer.
type TUIConfig struct {
	// Theme selects the color theme: dark, light, monokai, solarized, or auto.
	// Empty defaults to dark at runtime.
	Theme string `json:"theme" yaml:"theme"`
	// WordWrap sets the render word-wrap width in columns. Zero disables wrapping.
	WordWrap int `json:"word_wrap" yaml:"word_wrap"`
	// DiffStyle selects the diff preview style: unified, split, or auto.
	// Empty defaults to auto.
	DiffStyle string `json:"diff_style" yaml:"diff_style"`
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
