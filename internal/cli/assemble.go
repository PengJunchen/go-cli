package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/memory"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// defaultMaxTokens is the default compaction token budget used when no
// explicit value is provided via AssembleOption.
const defaultMaxTokens = 8000

// AgentAssembly holds the fully wired agent runtime components produced by
// AssembleAgent. Callers use the Harness to submit prompts and access the
// other fields for slash commands, cost reporting, and session management.
type AgentAssembly struct {
	Harness       *core.HarnessImpl
	Agent         *core.AgentImpl
	ToolRegistry  tools.ToolRegistry
	CostTracker   *production.CostTracker
	StatsRegistry *production.StatsRegistry
	SessionStore  *session.JSONLSessionStore
	SessionID     string
	Model         llm.BaseChatModel
	ModelName     string
	Compactor     compaction.Compactor
	Estimator     compaction.TokenEstimator
	MidTurn       *compaction.MidTurnCompact
	MaxTokens     int
	Cleanup       func()
	// Registry exposes the assembled components through the core.DefaultRegistry
	// so callers can retrieve or override any subsystem via RegisterXxx. It is
	// additive: the components above are still constructed and wired exactly as
	// before, they are also registered here for dependency-injection use.
	Registry *core.DefaultRegistry
	// FileTracker is the shared file tracker wired into WriteTool and
	// EditFileTool for backup/restore checkpoints.
	FileTracker *tools.FileTracker
	// DiffGenerator is the shared diff generator wired into WriteTool and
	// EditFileTool for change previews.
	DiffGenerator tools.DiffGenerator
	// PlanCtrl is the plan-mode controller exposed for slash commands.
	PlanCtrl core.PlanModeController
	// ModeResolver is the PermissionModeResolver wired into the ApprovalMiddleware
	// so the effective classifier can switch dynamically based on PermissionMode.
	ModeResolver approval.PermissionModeResolver
	// PromptBuilder is the SystemPromptBuilder wired into the LoopAgent for
	// dynamic system prompt assembly.
	PromptBuilder core.SystemPromptBuilder
	// ContextLoader is the ProjectContextLoader used to discover and load
	// AGENTS.md / CLAUDE.md files from the file system.
	ContextLoader core.ProjectContextLoader
	// Tracer is the tracing.Tracer created from config.Tracing. It is nil when
	// tracing is disabled (noop spans, zero overhead).
	Tracer *tracing.Tracer
	// ReminderManager manages system-reminder injections wired into the
	// middleware chain.
	ReminderManager core.SystemReminderManager
	// HookChain is the hook chain wired into the middleware chain. Hooks are
	// invoked before and after each turn.
	HookChain *core.HookChain
	// FailureSynthesizer converts recoverable errors into retry messages.
	FailureSynthesizer core.FailureTurnSynthesizer
	// CircuitBreaker protects the LLM model from cascading failures. When the
	// breaker is Open, Generate returns a fallback response.
	CircuitBreaker production.CircuitBreaker
	// LoopDetector monitors the agent event stream for recurring patterns
	// (repeated edits, test failures, identical tool calls).
	LoopDetector production.LoopDetector
	// IdempotentCache caches tool call results so that identical calls with
	// identical arguments return the stored result without re-executing.
	IdempotentCache production.IdempotentCache
	// AuditLog records tool calls as JSON-lines for later inspection. It is
	// nil when audit logging is disabled.
	AuditLog production.AuditLog
	// Telemetry collects runtime metrics (LLM token usage, tool call counts,
	// execution duration) queryable for cost reporting.
	Telemetry production.Telemetry
	// TurnRunner is the TurnRunner wired with the same steering channel as
	// the LoopAgent, so Steer calls are delivered to the running loop
	// between LLM iterations.
	TurnRunner *core.EinoTurnRunner
	// SteerChannel is the shared steering channel between the
	// InterruptHandler (writer) and the LoopAgent (reader). The REPL writes
	// steering messages here when the user presses Esc and types an
	// instruction.
	SteerChannel chan string
	// LoopAgent is the raw LoopAgent before middleware wrapping. It is
	// exposed so the REPL can call Pause()/Resume() on it.
	LoopAgent *core.LoopAgent
	// GitTool is the default git tool wired into the tool registry. It is
	// exposed so the interactive session can wire it into the session tree
	// for Git-aware branch linkage.
	GitTool tools.GitTool
	// MemoryStore persists cross-session memories for the /memory slash
	// command and system prompt injection.
	MemoryStore *memory.FileMemoryStore
	// MemoryExtractor extracts key facts from conversations for memory
	// storage.
	MemoryExtractor memory.MemoryExtractor
	// ThinkingLevel is the resolved LLM reasoning depth applied to every
	// Generate/Stream call. Defaults to ThinkingMedium when no explicit
	// option or config value is provided.
	ThinkingLevel llm.ThinkingLevel
	// ModelCycler rotates model selection across multiple providers. It is
	// nil when model cycling is not configured.
	ModelCycler *llm.ModelCycler
}

// AssembleOption configures AssembleAgent behavior.
type AssembleOption func(*assembleConfig)

type assembleConfig struct {
	maxTokens     int
	resumeFlag    bool
	enableSession bool
	agentName     string
	thinkingLevel *llm.ThinkingLevel
}

// WithMaxTokens sets the compaction token budget.
func WithMaxTokens(n int) AssembleOption {
	return func(c *assembleConfig) { c.maxTokens = n }
}

// WithResume enables session history restoration on startup.
func WithResume(b bool) AssembleOption {
	return func(c *assembleConfig) { c.resumeFlag = b }
}

// WithSessionPersistence enables JSONL session store persistence.
func WithSessionPersistence(b bool) AssembleOption {
	return func(c *assembleConfig) { c.enableSession = b }
}

// WithAgentName sets the agent's name used in tracing, logging, and session ID.
func WithAgentName(name string) AssembleOption {
	return func(c *assembleConfig) { c.agentName = name }
}

// WithThinkingLevel sets the LLM reasoning depth applied to every Generate/Stream
// call. When omitted, the level is resolved from config.Agent.Thinking, falling
// back to ThinkingMedium.
func WithThinkingLevel(level llm.ThinkingLevel) AssembleOption {
	return func(c *assembleConfig) { c.thinkingLevel = &level }
}

// AssembleAgent wires all production components (model wrapping, tool
// registration, approval gates, production resilience, subagent, session
// persistence, compaction) and returns a ready-to-use AgentAssembly.
//
// This function is shared by the interactive and prompt commands to eliminate
// assembly duplication and ensure both commands have identical wiring
// (approval gates, production resilience, output guards, etc.).
func AssembleAgent(
	ctx context.Context,
	rc *config.Config,
	providerName, modelName string,
	out io.Writer,
	opts ...AssembleOption,
) (*AgentAssembly, error) {
	ac := assembleConfig{
		maxTokens:     0, // 0 means "not set"; resolved below
		enableSession: false,
		agentName:     "main",
	}
	for _, o := range opts {
		o(&ac)
	}
	// Resolve maxTokens: explicit flag > config.Compaction.MaxTokens > default.
	if ac.maxTokens <= 0 {
		if rc != nil && rc.Compaction.MaxTokens > 0 {
			ac.maxTokens = rc.Compaction.MaxTokens
		} else {
			ac.maxTokens = defaultMaxTokens
		}
	}

	// Resolve thinking level: explicit flag > config.Agent.Thinking > medium.
	thinkingLevel := llm.ThinkingMedium
	if ac.thinkingLevel != nil {
		thinkingLevel = *ac.thinkingLevel
	} else if rc != nil && rc.Agent.Thinking != "" {
		if parsed, pErr := llm.ParseThinkingLevel(rc.Agent.Thinking); pErr == nil {
			thinkingLevel = parsed
		}
	}

	logger := slog.Default()

	// Registry: assemble components are also registered here so callers can
	// retrieve or override any subsystem via RegisterXxx. The registry is
	// additive; it does not change how components are constructed or wired.
	reg := core.NewRegistry().(*core.DefaultRegistry) //nolint:errcheck

	// Resolve session ID: config.Session.ID takes priority, fallback to agentName.
	sessionID := ac.agentName
	if rc != nil && rc.Session.ID != "" {
		sessionID = rc.Session.ID
	}

	// 1. Build model.
	model, modelCleanup, err := buildModel(ctx, rc, providerName, modelName)
	if err != nil {
		return nil, fmt.Errorf("assemble: build model: %w", err)
	}
	reg.RegisterModelProvider(&chatModelProvider{name: modelName, model: model})

	cleanup := func() {
		if modelCleanup != nil {
			modelCleanup()
		}
	}

	// 1c. Wire ModelCycler when enabled models are configured. The cycler
	// wraps the primary model so each Generate/Stream call can be routed to
	// a different provider according to the configured strategy.
	var modelCycler *llm.ModelCycler
	if rc != nil && rc.ModelCycler.Enabled && len(rc.ModelCycler.Models) > 0 {
		entries := make([]llm.ModelEntry, len(rc.ModelCycler.Models))
		for i, m := range rc.ModelCycler.Models {
			entries[i] = llm.ModelEntry{
				Provider: m.Provider,
				Model:    m.Model,
				Weight:   m.Weight,
			}
		}
		modelCycler = llm.NewModelCycler(llm.ModelCyclerConfig{
			Models:   entries,
			Strategy: rc.ModelCycler.Strategy,
		})
		modelCycler.WithRegistry(llm.NewProviderRegistry())
		model = modelCycler.WrapModel(model)
		logger.Info("assemble_model_cycler_enabled",
			"strategy", rc.ModelCycler.Strategy,
			"models", len(entries),
		)
	}

	// 1b. Wire tracing from config. When tracing.enabled is true, create the
	// appropriate TraceExporter (jsonl, stdout, otlp) and a Tracer. When
	// disabled, tracer remains nil so all SpanFromContext calls return noop
	// spans (zero overhead).
	var tracer *tracing.Tracer
	var traceExporter tracing.TraceExporter
	if rc != nil && rc.Tracing.Enabled != nil && *rc.Tracing.Enabled {
		traceExporter = buildTraceExporter(rc.Tracing, sessionID, logger)
		if traceExporter != nil {
			tracer = tracing.NewTracer(sessionID, traceExporter)
			reg.RegisterTraceExporter(traceExporter)
			logger.Info("assemble_tracing_enabled", "exporter", rc.Tracing.Exporter, "level", rc.Tracing.Level)
		}
	}

	// 2. Create tool registry + register defaults.
	tr := tools.NewDefaultToolRegistry()

	// Create shared component instances for the PARTIAL tools (D5, D6, D7, D9).
	fileTracker := tools.NewFileTracker()
	var diffGen tools.DiffGenerator = tools.NewUnifiedDiffGenerator(0, false)
	// Build the bash sandbox from config. WithAllowedPaths defaults to the
	// current working directory when no paths are configured (safe default).
	var sandboxOpts []tools.SandboxOption
	var resourceLimits tools.ResourceLimits
	if rc != nil {
		sandboxOpts = append(sandboxOpts, tools.WithAllowedPaths(rc.Sandbox.AllowedPaths))
		resourceLimits = tools.ResourceLimits{
			MaxCPU:    rc.Sandbox.MaxCPU,
			MaxMemory: rc.Sandbox.MaxMemory,
		}
	} else {
		sandboxOpts = append(sandboxOpts, tools.WithAllowedPaths(nil))
	}
	bashSandbox := tools.NewDefaultBashSandbox(sandboxOpts...)

	htmlConverter := tools.NewDefaultHTMLConverter()

	// Resolve the working directory for git tools. Use GitConfig.WorkDir when
	// configured, otherwise fall back to the process working directory.
	gitCwd, err := os.Getwd()
	if err != nil {
		gitCwd = "."
	}
	if rc != nil && rc.Git.WorkDir != "" {
		gitCwd = rc.Git.WorkDir
	}
	gitTool := tools.NewDefaultGitTool(gitCwd)

	// Wrap the UnifiedDiffGenerator with GitDiffGenerator so that /diff uses
	// `git diff` when inside a git repository (better rename/binary handling)
	// and falls back to the LCS-based diff otherwise.
	diffGen = tools.NewGitDiffGenerator(gitTool, diffGen)

	regOpts := []tools.RegisterDefaultsOption{
		tools.WithRegisteredFileTracker(fileTracker),
		tools.WithRegisteredDiffGenerator(diffGen),
		tools.WithRegisteredBashSandbox(bashSandbox),
		tools.WithRegisteredResourceLimits(resourceLimits),
		tools.WithRegisteredGitTool(gitTool),
	}
	// When a builtin whitelist is configured, only the named builtins are
	// registered; otherwise all builtins are registered (default behavior).
	if rc != nil && len(rc.Tools.Builtin) > 0 {
		regOpts = append(regOpts, tools.WithRegisteredBuiltinWhitelist(rc.Tools.Builtin))
	}
	if registerErr := tools.RegisterDefaults(ctx, tr, regOpts...); registerErr != nil {
		cleanup()
		return nil, fmt.Errorf("assemble: register tools: %w", registerErr)
	}

	// 2b. Register config-driven custom command tools. They are registered
	// after the builtins so they extend the toolset; custom tool names that
	// collide with an existing (builtin) tool are skipped with a warning.
	registerCustomTools(ctx, rc, tr, logger)

	// 3. Register MCP tools.
	if mcpErr := registerMCPTools(ctx, rc, tr); mcpErr != nil {
		logger.Warn("assemble_mcp_failed", "err", mcpErr)
	}

	// 3b. Register LSP tool (if configured). Supports both the legacy
	// single-server format (ServerCommand/WorkspaceRoot) and the new
	// multi-server format (Servers). When multiple servers are configured,
	// a MultiLSPClient routes requests by file extension. A single server
	// uses a plain DefaultLSPClient (backward compatible). LSP server
	// startup failures are logged as warnings and do not crash the main
	// flow (graceful degradation).
	if rc != nil && (len(rc.LSP.ServerCommand) > 0 || len(rc.LSP.Servers) > 0) {
		lspClient, lspStarted := buildLSPClient(ctx, rc, logger)
		if lspClient != nil && lspStarted {
			if regErr := tr.Register(ctx, tools.NewLSPTool(lspClient)); regErr != nil {
				logger.Warn("assemble_lsp_register_failed", "err", regErr)
				_ = lspClient.Shutdown(context.Background()) //nolint:errcheck
			} else {
				lspCleanup := cleanup
				cleanup = func() {
					_ = lspClient.Shutdown(context.Background()) //nolint:errcheck
					lspCleanup()
				}
				logger.Info("assemble_lsp_ready")
			}
		}
	}

	// 3c. Register remote bash tool (if SSH hosts are configured).
	if rc != nil && len(rc.Remote.Hosts) > 0 {
		remoteTool := buildRemoteBashTool(ctx, rc, logger)
		if remoteTool != nil {
			if regErr := tr.Register(ctx, remoteTool); regErr != nil {
				logger.Warn("assemble_remote_bash_register_failed", "err", regErr)
			} else {
				logger.Info("assemble_remote_bash_ready", "default_host", rc.Remote.DefaultHost)
			}
		}
	}

	// 4. Register skill tools.
	skillInfos := registerSkillTools(ctx, rc, tr)

	// 4b. Load and initialize extensions (if configured). Extension-provided
	// tools are registered into the tool registry before the middleware wraps
	// it, so they participate in approval gates and production wrappers.
	// Extension hooks and middleware are bridged into the runtime via adapters
	// (extension_bridge.go) and wired into the HookChain and MiddlewareChain
	// assembled below.
	var extHooks []core.Hook
	var extMiddleware []core.Middleware
	if rc != nil && rc.Extensions.Enabled && len(rc.Extensions.PluginPaths) > 0 {
		pm := extension.NewPluginManager(extension.NewDefaultPluginLoader())
		_ = pm.Load(ctx, rc.Extensions.PluginPaths) //nolint:errcheck
		_ = pm.Init(ctx)                            //nolint:errcheck
		for _, t := range pm.Tools() {
			if regErr := tr.Register(ctx, t); regErr != nil {
				logger.Warn("assemble_extension_tool_register_failed", "tool", t.Name(), "err", regErr)
			}
		}
		for _, h := range pm.Hooks() {
			extHooks = append(extHooks, newExtensionHookAdapter(h))
		}
		for _, m := range pm.Middleware() {
			extMiddleware = append(extMiddleware, newExtensionMiddlewareAdapter(m))
		}
		logger.Info("assemble_extensions_ready", "extensions", len(pm.Extensions()), "paths", len(rc.Extensions.PluginPaths), "hooks", len(extHooks), "middleware", len(extMiddleware))
		extCleanup := cleanup
		cleanup = func() {
			_ = pm.Shutdown(context.Background()) //nolint:errcheck
			extCleanup()
		}
	}

	// 5. Wire approval + mutation middleware via decorator pattern.
	classifier := approval.NewSafetyPolicyClassifier([]string{"bash"})
	approvalStore := approval.NewInMemoryApprovalStore()
	approvalCallback := approval.NewInteractiveApprovalCallback(os.Stdin, out)
	approvalCache := approval.NewApprovalCache("")
	modeResolver := approval.NewDefaultPermissionModeResolver()
	approvalMW := approval.NewApprovalMiddleware(
		classifier,
		approvalStore,
		approval.WithAutoApprove(false),
		approval.WithCallback(approvalCallback),
		approval.WithCache(approvalCache),
		approval.WithPermissionModeResolver(modeResolver),
	)
	reg.RegisterApprovalClassifier(&approvalClassifierAdapter{inner: classifier})
	reg.RegisterApprovalStore(&approvalStoreAdapter{inner: approvalStore})
	mutationQueue := tools.NewDefaultFileMutationQueue(
		tools.WithMutationFileTracker(fileTracker),
		tools.WithMutationDiffGenerator(diffGen),
	)
	tr = tools.NewMiddlewareToolRegistry(tr, approvalMW.WrapToolCall, tools.NewMutationQueueWrapper(mutationQueue))
	reg.RegisterToolRegistry(tr)

	// Close the mutation queue on cleanup so pending mutations are flushed and
	// worker goroutines are released.
	mqCleanup := cleanup
	cleanup = func() {
		if cq, ok := mutationQueue.(*tools.DefaultFileMutationQueue); ok {
			_ = cq.Close() //nolint:errcheck
		}
		mqCleanup()
	}

	// 6. Wire production resilience (retry + cost tracking).
	retryPolicy := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Second,
		MaxDelay:    10 * time.Second,
	})
	costTracker := production.NewCostTracker(nil)
	statsRegistry := production.NewStatsRegistry()
	pw := production.NewProductionModelWrapper(
		production.WithWrapperRetryPolicy(retryPolicy),
		production.WithWrapperCostTracker(costTracker),
		production.WithWrapperStatsRegistry(statsRegistry),
		production.WithWrapperModelName(modelName),
		production.WithWrapperSessionID(sessionID),
	)

	// 6b. Wire circuit breaker to protect the LLM model from cascading failures.
	cbCfg := production.CircuitBreakerConfig{}
	if rc != nil {
		cbCfg.FailureThreshold = rc.Production.CircuitBreaker.Threshold
		cbCfg.RecoveryTimeout = rc.Production.CircuitBreaker.ResetTimeout
	}
	circuitBreaker := production.NewDefaultCircuitBreaker(cbCfg,
		production.WithName("model-breaker"),
	)
	production.RegisterCircuitBreaker(circuitBreaker)

	// 6c. Wire idempotent cache, audit log, and telemetry for tool calls.
	idempotentCache := production.NewFIFOIdempotentCache(1024)
	production.RegisterIdempotentCache(idempotentCache)

	telemetry := production.NewDefaultTelemetry()
	production.RegisterTelemetry(telemetry)

	var auditLog production.AuditLog
	if rc != nil && rc.Production.Audit.Enabled {
		auditPath := rc.Production.Audit.Path
		if auditPath == "" {
			if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
				auditPath = filepath.Join(home, ".go-cli", "audit.jsonl")
			}
		}
		if auditPath != "" {
			auditLog = production.NewDefaultAuditLog(auditPath)
			production.RegisterAuditLog(auditLog)
		}
	}

	// 6d. Wrap tool registry with idempotent cache + audit + telemetry so that
	// identical tool calls return cached results and every call is recorded.
	tr = tools.NewMiddlewareToolRegistry(tr,
		newProductionToolWrapper(idempotentCache, auditLog, telemetry, sessionID),
	)
	reg.RegisterToolRegistry(tr)

	// 7. Wire output guards (PII + code injection + length).
	guardChain := production.NewOutputGuardChain([]production.OutputGuard{
		production.NewPIIOutputGuard(),
		production.NewCodeInjectionGuard(),
		production.NewLengthGuard(100000),
	})

	// 8. Wire real SubAgent execution (replaces simulated runner).
	subAgentFactory := core.NewRealSubAgentFactory(model, llm.NewProviderRegistry(), tr,
		core.WithModelWrapper(newModelWrapper(pw, circuitBreaker, guardChain, telemetry)),
	)
	core.RegisterSubAgentFactory(subAgentFactory)
	dispatcher := core.NewDefaultSubagentDispatcher(nil)
	if subErr := tr.Register(ctx, core.NewSubagentTool(dispatcher)); subErr != nil {
		logger.Warn("assemble_subagent_tool_failed", "err", subErr)
	}

	// 8b. Wire ACP multi-agent communication (if configured). When the config
	// supplies a transport and at least one endpoint, create an ACPClient,
	// connect it, and build an ACPMiddlewareAdapter that routes inbound ACP
	// messages to the SubagentDispatcher above.
	var acpAdapter *acp.ACPMiddlewareAdapter
	var acpClient acp.ACPClient
	if rc != nil && rc.ACP.Transport != "" && len(rc.ACP.Endpoints) > 0 {
		switch rc.ACP.Transport {
		case "stdio":
			acpClient = acp.NewStdioAdapter(os.Stdin, os.Stdout)
		case "grpc":
			acpClient = acp.NewGRPCAdapter(rc.ACP.Endpoints[0])
		default:
			logger.Warn("assemble_acp_unknown_transport", "transport", rc.ACP.Transport)
		}
		if acpClient != nil {
			if connErr := acpClient.Connect(ctx); connErr != nil {
				logger.Warn("assemble_acp_connect_failed", "err", connErr)
				acpClient = nil
			} else {
				acpMW := acp.NewACPMiddleware("acp-bridge", acpClient)
				acpAdapter = acp.NewACPMiddlewareAdapter(acpMW, dispatcher, acpClient)
				logger.Info("assemble_acp_connected", "transport", rc.ACP.Transport)
			}
		}
	}

	// 9. Register remaining unconnected tools (todo, task, goal, web, plan_mode, etc.).
	todoStore := tools.NewTodoStore()
	taskStore := tools.NewTaskStore()
	goalStore, _ := tools.NewDefaultGoalStore("") //nolint:errcheck
	planCtrl := core.NewDefaultPlanModeController()
	hitlEmitter := &cliHITLEmitter{out: out}

	// Build the web search tool from config. Default is MockSearchProvider;
	// "fetch" uses DuckDuckGo HTML scraping, "brave" uses the Brave API.
	webSearchTool := tools.NewWebSearchTool()
	if rc != nil {
		switch rc.WebSearch.Provider {
		case "brave":
			if rc.WebSearch.APIKey != "" {
				webSearchTool = tools.NewWebSearchTool(tools.WithSearchProvider(
					tools.NewBraveSearchProvider(rc.WebSearch.APIKey),
				))
			}
		case "fetch":
			webSearchTool = tools.NewWebSearchTool(tools.WithSearchProvider(
				tools.NewFetchSearchProvider(),
			))
		}
	}

	extraTools := []tools.ToolDefinition{
		tools.NewTodoWriteTool(todoStore),
		tools.NewTaskCreateTool(taskStore),
		tools.NewTaskGetTool(taskStore),
		tools.NewTaskListTool(taskStore),
		tools.NewTaskUpdateTool(taskStore),
		tools.NewGoalCreateTool(goalStore),
		tools.NewGoalUpdateTool(goalStore),
		tools.NewGoalListTool(goalStore, taskStore),
		tools.NewGoalGetTool(goalStore, taskStore),
		tools.NewWebFetchTool(tools.WithHTMLConverter(htmlConverter)),
		webSearchTool,
		core.NewAskUserQuestionTool(hitlEmitter, 30*time.Second),
		tools.NewEnterPlanModeTool(planCtrl),
		tools.NewExitPlanModeTool(planCtrl),
		tools.NewGitPRCreateTool(gitCwd),
	}
	for _, t := range extraTools {
		if regErr := tr.Register(ctx, t); regErr != nil {
			logger.Warn("assemble_tool_register_failed", "tool", t.Name(), "err", regErr)
		}
	}
	// tool_search needs visibility into all registered tools, so register last.
	if searchErr := tr.Register(ctx, tools.NewToolSearchTool(tr)); searchErr != nil {
		logger.Warn("assemble_tool_register_failed", "tool", "tool_search", "err", searchErr)
	}

	// 10. Build loop agent with model wrapper.
	steerCh := make(chan string, 16)
	loopOpts := []core.LoopOption{
		core.WithLLM(model),
		core.WithTools(tr),
		core.WithModelWrapper(newModelWrapper(pw, circuitBreaker, guardChain, telemetry)),
		core.WithExecutionMode(core.ExecutionModeParallel),
		core.WithTracer(tracer),
		core.WithSteeringChannel(steerCh),
		core.WithThinkingConfig(llm.ThinkingConfigForLevel(thinkingLevel)),
	}
	if rc != nil && rc.Agent.MaxIterations != 0 {
		loopOpts = append(loopOpts, core.WithMaxIterations(rc.Agent.MaxIterations))
	}
	// Allow config to override parallel execution to sequential.
	if rc != nil && rc.Tools.Parallel != nil && !*rc.Tools.Parallel {
		loopOpts = append(loopOpts, core.WithExecutionMode(core.ExecutionModeSequential))
	}

	// 10b. Wire memory store for cross-session memory persistence. The store
	// is backed by a JSONL file in the config directory (~/.go-cli/ by
	// default). Existing memories are loaded and injected into the system
	// prompt so the agent is aware of them from the first turn.
	var memStore *memory.FileMemoryStore
	memoryPath := ""
	if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
		memoryPath = filepath.Join(home, ".go-cli", "memories.jsonl")
	}
	if memoryPath != "" {
		if mkErr := os.MkdirAll(filepath.Dir(memoryPath), 0o755); mkErr != nil {
			logger.Warn("assemble_memory_mkdir_failed", "err", mkErr)
		}
		ms, msErr := memory.NewFileMemoryStore(memoryPath)
		if msErr != nil {
			logger.Warn("assemble_memory_store_failed", "err", msErr)
		} else {
			memStore = ms
		}
	}

	var memoryEntries []core.MemoryEntry
	if memStore != nil {
		memories, memErr := memStore.List(ctx)
		if memErr != nil {
			logger.Warn("assemble_memory_list_failed", "err", memErr)
		}
		for _, m := range memories {
			memoryEntries = append(memoryEntries, core.MemoryEntry{
				ID:       m.ID,
				Content:  m.Content,
				Category: m.Category,
			})
		}
	}

	// Create memory extractor for LLM-based fact extraction. The extractor
	// uses the model to analyze conversations and the store for deduplication.
	var memExtractor memory.MemoryExtractor
	if memStore != nil {
		memExtractor = memory.NewLLMMemoryExtractor(model, memStore)
	}

	// 10c. Wire dynamic system prompt builder with project context.
	contextLoader := core.NewDefaultProjectContextLoader()
	promptBuilder := core.NewDefaultSystemPromptBuilder()
	cwd, _ := os.Getwd() //nolint:errcheck
	contextFiles, ctxErr := contextLoader.Load(ctx, cwd)
	if ctxErr != nil {
		logger.Warn("assemble_context_load_failed", "err", ctxErr)
	}
	var customPrompt, appendPrompt string
	if rc != nil {
		customPrompt = rc.Agent.SystemPrompt
		appendPrompt = rc.Agent.AppendSystemPrompt
	}
	loopOpts = append(loopOpts,
		core.WithSystemPromptBuilder(promptBuilder),
		core.WithSystemPromptOptions(core.SystemPromptOptions{
			Cwd:          cwd,
			ContextFiles: contextFiles,
			Skills:       skillInfos,
			Memories:     memoryEntries,
			CustomPrompt: customPrompt,
			AppendPrompt: appendPrompt,
		}),
	)

	var loop core.AgentLoop = core.NewLoopAgent(loopOpts...)
	// Keep a reference to the raw LoopAgent so the REPL can call Pause/Resume.
	var loopAgent *core.LoopAgent
	if la, ok := loop.(*core.LoopAgent); ok {
		loopAgent = la
	}

	// 10b. Wire loop detector + system reminder injector.
	ldCfg := production.LoopDetectionConfig{}
	if rc != nil {
		ldCfg.EditThreshold = rc.Production.LoopDetector.EditThreshold
		ldCfg.TestFailureThreshold = rc.Production.LoopDetector.TestFailureThreshold
		ldCfg.SameToolCallThreshold = rc.Production.LoopDetector.SameToolCallThreshold
	}
	loopDetector := production.NewDefaultLoopDetector(ldCfg)
	production.RegisterLoopDetector(loopDetector)
	reminderMgr := core.NewDefaultSystemReminderManager()

	// 10c. Wire failure synthesis and hook chain. Extension-registered hooks
	// (bridged from extension.Hook to core.Hook) are included so they observe
	// the agent lifecycle.
	failureSynthesizer := core.NewDefaultFailureTurnSynthesizer()
	hookChain := core.NewHookChain(extHooks...)

	// 11. Apply middleware chain (onion model) around the loop agent.
	// Order (outermost first): logging -> loop-detector -> plan-mode ->
	// system-reminder -> failure-synthesis -> hook. The loop detector
	// observes events after Run and registers a SystemReminder; the
	// SystemReminderInjector injects it on the next turn. FailureSynthesis
	// retries recoverable errors once with a synthesized message. The Hook
	// middleware runs pre/post-turn hooks. When ACP is configured the
	// ACPMiddlewareAdapter is appended innermost so it runs closest to the
	// core loop while still routing inbound peer messages.
	chain := []core.Middleware{
		core.NewLoggingMiddleware(ac.agentName),
		&loopDetectorMiddleware{detector: loopDetector, manager: reminderMgr},
		core.NewPlanModeMiddleware(planCtrl),
		core.NewSystemReminderInjector(reminderMgr),
		core.NewFailureSynthesisMiddleware(failureSynthesizer),
		core.NewHookMiddleware(hookChain),
	}
	if acpAdapter != nil {
		chain = append(chain, acpAdapter)
	}
	// Extension-registered middleware (bridged from extension.Middleware to
	// core.Middleware) are appended innermost so they run closest to the core
	// loop while still participating in the onion chain.
	chain = append(chain, extMiddleware...)
	loop = core.NewMiddlewareChain(chain...).Wrap(loop)

	// 12. Create compaction components (strategy from config, default unified).
	compactorFactory := compaction.NewDefaultCompactorFactory()
	strategy := "unified"
	if rc != nil && rc.Compaction.Strategy != "" {
		strategy = rc.Compaction.Strategy
	}
	compactor, err := compactorFactory.Create(strategy)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("assemble: create compactor: %w", err)
	}
	estimator := compaction.NewUnicodeTokenEstimator()
	midTurn := compaction.NewMidTurnCompact()
	reg.RegisterCompactor(&compactorAdapter{inner: compactor, estimator: estimator})
	reg.RegisterTokenEstimator(&tokenEstimatorAdapter{inner: estimator})

	// 13. Wire session persistence (if enabled).
	var sessionStore *session.JSONLSessionStore
	if ac.enableSession && rc != nil && rc.Session.StorePath != "" {
		sessionStore = session.NewJSONLSessionStore(rc.Session.StorePath)
		if openErr := sessionStore.Open(ctx); openErr != nil {
			logger.Warn("assemble_session_open_failed", "err", openErr)
			sessionStore = nil
		}
	}

	// Extend cleanup to include ACP client, session store and trace exporter.
	prevCleanup := cleanup
	cleanup = func() {
		if acpAdapter != nil {
			acpAdapter.Close()
		}
		if acpClient != nil {
			_ = acpClient.Disconnect(context.Background()) //nolint:errcheck
		}
		if sessionStore != nil {
			sessionStore.Close() //nolint:errcheck,gosec
		}
		if memStore != nil {
			_ = memStore.Close() //nolint:errcheck
		}
		if traceExporter != nil {
			if tracer != nil {
				tracer.Flush()
			}
			_ = traceExporter.Shutdown(context.Background()) //nolint:errcheck
		}
		prevCleanup()
	}

	// 14. Resume history from session store if requested.
	var restoredHistory []core.AgentMessage
	if ac.resumeFlag && sessionStore != nil {
		restoredHistory, _ = loadSessionHistory(sessionStore.FilePath()) //nolint:errcheck
		if len(restoredHistory) > 0 {
			logger.Info("assemble_session_resumed", "messages", len(restoredHistory))
		}
	}

	// 15. Build AgentImpl + HarnessImpl.
	agentOpts := []core.AgentOption{
		core.WithCompactionHook(newCompactionHook(compactor, estimator, ac.maxTokens)),
	}
	if len(restoredHistory) > 0 {
		agentOpts = append(agentOpts, core.WithHistory(restoredHistory))
	}
	agent := core.NewAgentImpl(ac.agentName, loop, agentOpts...)
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(64), core.WithHarnessTracer(tracer))

	// 15b. Build TurnRunner wired with the shared steering channel and agent
	// so Steer calls are delivered to the running loop between LLM
	// iterations, and RunTurn delegates to agent.Run (with history).
	turnRunner := core.NewEinoTurnRunner(loop)
	turnRunner.SetSteerChannel(steerCh)
	turnRunner.SetAgent(agent)
	reg.RegisterTurnRunner(turnRunner)

	// Emit assemble span for observability.
	asmSpan, _ := tracing.SpanFromContext(ctx, "assemble.agent", tracing.SpanKindInternal)
	asmSpan.SetAttributes(
		tracing.Attribute{Key: "agent_name", Value: ac.agentName},
		tracing.Attribute{Key: "model", Value: modelName},
		tracing.Attribute{Key: "session_enabled", Value: ac.enableSession},
		tracing.Attribute{Key: "resume", Value: ac.resumeFlag},
	)
	asmSpan.SetStatus(tracing.SpanStatusOK, "")
	asmSpan.End()

	return &AgentAssembly{
		Harness:            h,
		Agent:              agent,
		ToolRegistry:       tr,
		CostTracker:        costTracker,
		StatsRegistry:      statsRegistry,
		SessionStore:       sessionStore,
		SessionID:          sessionID,
		Model:              model,
		ModelName:          modelName,
		Compactor:          compactor,
		Estimator:          estimator,
		MidTurn:            midTurn,
		MaxTokens:          ac.maxTokens,
		Cleanup:            cleanup,
		Registry:           reg,
		FileTracker:        fileTracker,
		DiffGenerator:      diffGen,
		PlanCtrl:           planCtrl,
		ModeResolver:       modeResolver,
		PromptBuilder:      promptBuilder,
		ContextLoader:      contextLoader,
		Tracer:             tracer,
		ReminderManager:    reminderMgr,
		HookChain:          hookChain,
		FailureSynthesizer: failureSynthesizer,
		CircuitBreaker:     circuitBreaker,
		LoopDetector:       loopDetector,
		IdempotentCache:    idempotentCache,
		AuditLog:           auditLog,
		Telemetry:          telemetry,
		TurnRunner:         turnRunner,
		SteerChannel:       steerCh,
		LoopAgent:          loopAgent,
		GitTool:            gitTool,
		MemoryStore:        memStore,
		MemoryExtractor:    memExtractor,
		ThinkingLevel:      thinkingLevel,
		ModelCycler:        modelCycler,
	}, nil
}

// buildModel resolves an llm.BaseChatModel from the loaded configuration.
// When the configuration supplies a BaseURL or APIKey, a custom provider is
// built with those settings; otherwise the default provider registry is used.
func buildModel(ctx context.Context, rc *config.Config, providerName, modelName string) (llm.BaseChatModel, func(), error) {
	cfg := llm.ModelConfig{Model: modelName}

	if rc != nil && (rc.Provider.BaseURL != "" || rc.Provider.APIKey != "") {
		provider := llm.NewEinoProvider(
			llm.WithProviderName(providerName),
			llm.WithBaseURL(rc.Provider.BaseURL),
			llm.WithAPIKey(rc.Provider.APIKey),
			llm.WithDefaultModel(modelName),
		)
		return provider.Build(ctx, cfg)
	}

	reg := llm.NewProviderRegistry()
	return reg.GetModel(ctx, providerName, cfg)
}

// registerMCPTools connects to configured MCP servers and registers their
// tools into the tool registry. When no MCP servers are configured in the
// main config file, it auto-loads from .go-cli/mcp.json or
// ~/.config/go-cli/mcp.json if either exists.
func registerMCPTools(ctx context.Context, rc *config.Config, tr tools.ToolRegistry) error {
	servers := loadMCPServers(rc)
	if len(servers) == 0 {
		return nil
	}

	for _, srv := range servers {
		cfg := mcp.MCPServerConfig{
			Name: srv.Name,
			URL:  srv.URL,
		}
		if srv.Command != "" {
			cfg.Transport = mcp.MCPTransportStdio
			cfg.Command = srv.Command
			cfg.Args = srv.Args
			for k, v := range srv.Env {
				cfg.Env = append(cfg.Env, k+"="+v)
			}
		} else if srv.URL != "" {
			cfg.Transport = mcp.MCPTransportSSE
		} else {
			continue
		}

		var client mcp.MCPClient
		if cfg.Transport == mcp.MCPTransportSSE {
			client = mcp.NewHTTPClientAdapter(cfg)
		} else {
			client = mcp.NewOfficialSDKAdapter(cfg)
		}

		if err := client.Connect(ctx); err != nil {
			slog.Warn("assemble_mcp_connect_failed", "server", srv.Name, "err", err)
			continue
		}

		mcpTools, err := client.ListTools(ctx)
		if err != nil {
			slog.Warn("assemble_mcp_list_failed", "server", srv.Name, "err", err)
			continue
		}

		for _, t := range mcpTools {
			if regErr := tr.Register(ctx, mcp.NewMCPToolAdapter(client, t)); regErr != nil {
				slog.Warn("assemble_mcp_register_failed", "tool", t.Name, "err", regErr)
			}
		}
		slog.Info("assemble_mcp_registered", "server", srv.Name, "tools", len(mcpTools))
	}
	return nil
}

// loadMCPServers returns MCP server configs from the main config, or
// auto-discovered from default paths when the main config has none.
func loadMCPServers(rc *config.Config) []config.MCPServerConfig {
	if rc != nil && len(rc.MCP.Servers) > 0 {
		return rc.MCP.Servers
	}

	// Auto-discover MCP config from default paths.
	candidates := []string{".go-cli/mcp.json"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "go-cli", "mcp.json"))
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// Support both the array format ({"servers": [...]}) and the map
		// format ({"mcpServers": {"name": {...}}}).
		var servers struct {
			Servers    []config.MCPServerConfig `json:"servers"`
			MCPServers config.MCPServersMap     `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &servers); err != nil {
			slog.Warn("assemble_mcp_config_parse_failed", "path", path, "err", err)
			continue
		}
		result := servers.Servers
		if len(servers.MCPServers) > 0 {
			for name, s := range servers.MCPServers {
				result = append(result, config.MCPServerConfig{
					Name:    name,
					Command: s.Command,
					Args:    s.Args,
					URL:     s.URL,
					Env:     s.Env,
				})
			}
		}
		if len(result) > 0 {
			slog.Info("assemble_mcp_config_discovered", "path", path, "servers", len(result))
			return result
		}
	}
	return nil
}

// registerSkillTools loads skills from the configured directory (or default
// discovery paths) and registers them as tools. It supports two directory
// layouts:
//   - Flat: {dir}/{name}.md
//   - Nested: {dir}/{name}/SKILL.md
//
// It returns a slice of SkillInfo describing each registered skill, suitable
// for injection into the system prompt.
func registerSkillTools(ctx context.Context, rc *config.Config, tr tools.ToolRegistry) []core.SkillInfo {
	skillDir := ""
	if rc != nil && rc.Skill.Dir != "" {
		skillDir = rc.Skill.Dir
	} else {
		// Auto-discover default skill directories.
		skillDir = discoverSkillDir()
	}
	if skillDir == "" {
		return nil
	}

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(ctx, skillDir)
	if err != nil {
		slog.Warn("assemble_skill_load_failed", "dir", skillDir, "err", err)
		return nil
	}

	var infos []core.SkillInfo
	count := 0
	for _, def := range defs {
		if def == nil {
			continue
		}
		adapter := skill.NewSkillAdapter(*def)
		if regErr := tr.Register(ctx, adapter); regErr != nil {
			slog.Warn("assemble_skill_register_failed", "skill", (*def).Name(), "err", regErr)
			continue
		}
		infos = append(infos, core.SkillInfo{
			Name:        (*def).Name(),
			Description: (*def).Description(),
			Category:    (*def).Category(),
		})
		count++
	}
	if count > 0 {
		slog.Info("assemble_skills_registered", "dir", skillDir, "count", count)
	}
	return infos
}

// discoverSkillDir probes default skill directories and returns the first one
// that exists. The search order is:
//  1. .go-cli/skills (project-local, conventional location)
//  2. ~/.config/go-cli/skills (global user-level)
func discoverSkillDir() string {
	candidates := []string{".go-cli/skills"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "go-cli", "skills"))
	}
	for _, dir := range candidates {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			slog.Info("assemble_skill_dir_discovered", "dir", dir)
			return dir
		}
	}
	return ""
}

// registerCustomTools registers config-driven custom command tools from
// rc.Tools.CustomTools. They are registered after the builtins so they extend
// the available toolset. A custom tool with an empty name or command, or whose
// name collides with an already-registered tool, is skipped with a warning.
func registerCustomTools(ctx context.Context, rc *config.Config, tr tools.ToolRegistry, logger *slog.Logger) {
	if rc == nil || len(rc.Tools.CustomTools) == 0 {
		return
	}

	existing, err := tr.List(ctx)
	if err != nil {
		logger.Warn("assemble_custom_tools_list_failed", "err", err)
		return
	}
	existingNames := make(map[string]bool, len(existing))
	for _, t := range existing {
		existingNames[t.Name()] = true
	}

	for _, cfg := range rc.Tools.CustomTools {
		if cfg.Name == "" {
			logger.Warn("assemble_custom_tool_skipped", "reason", "empty name")
			continue
		}
		if len(cfg.Command) == 0 {
			logger.Warn("assemble_custom_tool_skipped", "name", cfg.Name, "reason", "empty command")
			continue
		}
		if existingNames[cfg.Name] {
			logger.Warn("assemble_custom_tool_skipped", "name", cfg.Name, "reason", "collides with existing tool")
			continue
		}
		timeout := time.Duration(0)
		if cfg.Timeout > 0 {
			timeout = time.Duration(cfg.Timeout) * time.Second
		}
		if regErr := tr.Register(ctx, tools.NewCustomCommandTool(cfg.Name, cfg.Description, cfg.Command, cfg.Args, cfg.Env, timeout, cfg.WorkingDir)); regErr != nil {
			logger.Warn("assemble_custom_tool_register_failed", "name", cfg.Name, "err", regErr)
			continue
		}
		existingNames[cfg.Name] = true
		logger.Info("assemble_custom_tool_registered", "name", cfg.Name)
	}
}

// loopDetectorMiddleware wraps an AgentLoop, feeding returned events to a
// LoopDetector after each Run. When a loop is detected, a SystemReminder is
// added to the reminder manager so the SystemReminderInjector can inject it
// on the next turn.
type loopDetectorMiddleware struct {
	detector production.LoopDetector
	manager  *core.DefaultSystemReminderManager
	name     string
}

var _ core.Middleware = (*loopDetectorMiddleware)(nil)

// Name returns the middleware identifier.
func (m *loopDetectorMiddleware) Name() string {
	if m.name == "" {
		return "loop-detector"
	}
	return m.name
}

// Wrap returns a wrapped AgentLoop that monitors events for loops.
func (m *loopDetectorMiddleware) Wrap(next core.AgentLoop) core.AgentLoop {
	return &loopDetectorLoop{detector: m.detector, manager: m.manager, next: next}
}

// loopDetectorLoop is the concrete wrapped loop produced by
// loopDetectorMiddleware.
type loopDetectorLoop struct {
	detector production.LoopDetector
	manager  *core.DefaultSystemReminderManager
	next     core.AgentLoop
}

// Run delegates to the wrapped loop, then feeds the returned events to the
// LoopDetector. When a loop is detected, a SystemReminder is registered for
// injection on the next turn.
func (l *loopDetectorLoop) Run(ctx context.Context, submission core.Submission, stream ...core.EventStream) ([]core.AgentEvent, error) {
	events, err := l.next.Run(ctx, submission, stream...)
	if err != nil {
		return events, err
	}

	for _, ev := range events {
		_ = l.detector.Observe(ctx, ev) //nolint:errcheck
	}

	res := l.detector.Check(ctx)
	if res.Detected {
		slog.WarnContext(ctx, "loop_detected",
			"dimension", res.Dimension,
			"count", res.Count,
			"threshold", res.Threshold,
			"message", res.Message,
		)
		l.manager.AddReminder(core.SystemReminder{
			ID:      "loop-detector",
			Content: fmt.Sprintf("Loop detected: %s. Please try a different approach.", res.Message),
		})
	}

	return events, err
}

// defaultCacheTTL is the default time-to-live for idempotent tool call results.
const defaultCacheTTL = 5 * time.Minute

// cachedToolResult holds a cached tool result with an expiry timestamp for
// TTL-based cache invalidation. The FIFOIdempotentCache itself does not
// support TTL, so expiry is tracked alongside the value.
type cachedToolResult struct {
	result *tools.ToolResult
	expiry time.Time
}

// newProductionToolWrapper returns a ToolExecutorWrapper that applies
// idempotent caching, audit logging, and telemetry recording to every tool
// call. On a cache hit (within the TTL) the wrapper short-circuits and returns
// the stored result without invoking the underlying tool. The wrapper is
// safe to use with nil cache, auditLog, or telemetry; each concern is
// skipped when its component is absent.
func newProductionToolWrapper(
	cache production.IdempotentCache,
	auditLog production.AuditLog,
	telemetry production.Telemetry,
	sessionID string,
) tools.ToolExecutorWrapper {
	return func(next func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)) func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			start := time.Now()
			cacheKey := toolCacheKey(call.Name, call.Args)

			// Check idempotent cache.
			if cache != nil {
				if cached, ok := cache.Get(ctx, cacheKey); ok {
					if entry, ok := cached.(*cachedToolResult); ok && time.Now().Before(entry.expiry) {
						duration := time.Since(start)
						recordAudit(ctx, auditLog, call, entry.result, duration, sessionID, true)
						recordToolTelemetry(ctx, telemetry, call.Name, duration, true)
						return entry.result, nil
					}
				}
			}

			// Cache miss: execute the tool.
			result, err := next(ctx, call)
			duration := time.Since(start)

			if err == nil && result != nil && cache != nil {
				_ = cache.Set(ctx, cacheKey, &cachedToolResult{ //nolint:errcheck
					result: result,
					expiry: time.Now().Add(defaultCacheTTL),
				})
			}

			recordAudit(ctx, auditLog, call, result, duration, sessionID, false)
			recordToolTelemetry(ctx, telemetry, call.Name, duration, false)

			return result, err
		}
	}
}

// toolCacheKey generates a deterministic cache key from the tool name and
// arguments by JSON-marshaling the args map.
func toolCacheKey(name string, args map[string]any) string {
	data, err := json.Marshal(args)
	if err != nil {
		return name
	}
	return name + ":" + string(data)
}

// hashValue computes a SHA-256 hex digest of the JSON representation of v.
func hashValue(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// recordAudit logs a tool call to the audit log if one is configured. It
// records the args hash, result hash, duration, and whether the result was
// served from cache.
func recordAudit(ctx context.Context, auditLog production.AuditLog, call tools.ToolCall, result *tools.ToolResult, duration time.Duration, sessionID string, cacheHit bool) {
	if auditLog == nil {
		return
	}
	entry := production.AuditEntry{
		Timestamp: time.Now(),
		Operation: "tool.run",
		ToolName:  call.Name,
		SessionID: sessionID,
		Args: map[string]any{
			"args_hash": hashValue(call.Args),
		},
	}
	if result != nil {
		entry.Result = map[string]any{
			"result_hash": hashValue(result.Output),
			"duration_ms": duration.Milliseconds(),
			"cache_hit":   cacheHit,
		}
	}
	_ = auditLog.Log(ctx, entry) //nolint:errcheck
}

// recordToolTelemetry records tool call count and duration metrics if a
// telemetry instance is configured.
func recordToolTelemetry(ctx context.Context, telemetry production.Telemetry, toolName string, duration time.Duration, cacheHit bool) {
	if telemetry == nil {
		return
	}
	_ = telemetry.Record(ctx, production.TelemetryMetric{ //nolint:errcheck
		Name:  "tool.call.count",
		Value: 1,
		Labels: map[string]string{
			"tool":      toolName,
			"cache_hit": fmt.Sprintf("%v", cacheHit),
		},
	})
	_ = telemetry.Record(ctx, production.TelemetryMetric{ //nolint:errcheck
		Name:  "tool.call.duration_ms",
		Value: float64(duration.Milliseconds()),
		Labels: map[string]string{
			"tool": toolName,
		},
	})
}

// buildTraceExporter creates the appropriate TraceExporter from the tracing
// config. Supported exporter types: "jsonl" (default), "stdout", "otlp".
// When the exporter cannot be created (e.g. file open failure), nil is
// returned and a warning is logged so assembly continues without tracing.
func buildTraceExporter(tc config.TracingConfig, sessionID string, logger *slog.Logger) tracing.TraceExporter {
	switch tc.Exporter {
	case "stdout":
		return tracing.NewStdoutTraceExporter(false)
	case "otlp":
		endpoint := tc.FilePath
		if endpoint == "" {
			endpoint = "http://localhost:4318/v1/traces"
		}
		return tracing.NewOTLPTraceExporter(tracing.OTLPTraceExporterConfig{
			Endpoint: endpoint,
		})
	default: // "jsonl" or empty
		dir := tc.FilePath
		if dir == "" {
			dir = ".go-cli/traces"
		}
		exp, err := tracing.NewJSONLTraceExporter(dir, sessionID)
		if err != nil {
			logger.Warn("assemble_tracing_jsonl_failed", "dir", dir, "err", err)
			return nil
		}
		return exp
	}
}

// buildRemoteBashTool constructs a RemoteBashTool from the remote config. It
// creates an SSH client for each configured host, selects the default host's
// client as the primary, and registers the rest via WithRemoteBashHosts. A
// sandbox with the default command blacklist (but no path whitelist, since
// remote execution has no local working directory) is attached. Returns nil
// when no default host is configured or the default host is missing from the
// hosts map.
func buildRemoteBashTool(_ context.Context, rc *config.Config, logger *slog.Logger) *tools.RemoteBashTool {
	if rc == nil || len(rc.Remote.Hosts) == 0 {
		return nil
	}

	// Build SSH clients for every configured host (simple client cache by
	// host name). Since exec-based SSH is stateless, there is no persistent
	// connection to pool.
	hostClients := make(map[string]tools.SSHClient, len(rc.Remote.Hosts))
	for name, hc := range rc.Remote.Hosts {
		sshCfg := tools.SSHConfig{
			Host:           hc.Host,
			Port:           hc.Port,
			User:           hc.User,
			KeyPath:        hc.KeyPath,
			Password:       hc.Password,
			KnownHostsPath: hc.KnownHostsPath,
		}
		hostClients[name] = tools.NewDefaultSSHClient(sshCfg)
	}

	// Resolve the default host's client.
	defaultName := rc.Remote.DefaultHost
	if defaultName == "" {
		// Fall back to the first configured host.
		for name := range rc.Remote.Hosts {
			defaultName = name
			break
		}
	}
	defaultClient, ok := hostClients[defaultName]
	if !ok {
		logger.Warn("assemble_remote_bash_default_host_missing", "default_host", defaultName)
		return nil
	}

	// Build a sandbox with the default command blacklist but no path
	// whitelist (remote execution has no local working directory).
	// NewDefaultBashSandbox() already sets the default blacklist and an
	// empty whitelist (allow all paths), which is the correct behavior for
	// remote command validation.
	remoteSandbox := tools.NewDefaultBashSandbox()

	// Remove the default from the hostClients map since it's passed as the
	// primary client; the rest are registered as additional hosts.
	delete(hostClients, defaultName)

	opts := []tools.RemoteBashToolOption{
		tools.WithRemoteBashSandbox(remoteSandbox),
	}
	if len(hostClients) > 0 {
		opts = append(opts, tools.WithRemoteBashHosts(hostClients))
	}

	return tools.NewRemoteBashTool(defaultClient, opts...)
}

// buildLSPClient constructs the LSP client from config. When the legacy
// ServerCommand field is set, it is normalized into a single-element Servers
// list for backward compatibility. A single server returns a plain
// DefaultLSPClient; multiple servers return a MultiLSPClient that routes by
// file extension. Server startup failures are logged as warnings (graceful
// degradation); the function returns (nil, false) when no server could start.
func buildLSPClient(ctx context.Context, rc *config.Config, logger *slog.Logger) (tools.LSPClient, bool) {
	// Normalize: merge legacy ServerCommand into Servers if Servers is empty.
	servers := rc.LSP.Servers
	if len(servers) == 0 && len(rc.LSP.ServerCommand) > 0 {
		servers = []config.LSPServerConfig{
			{
				ServerCommand:  rc.LSP.ServerCommand,
				WorkspaceRoot:  rc.LSP.WorkspaceRoot,
				FileExtensions: nil, // default client
			},
		}
	}

	if len(servers) == 0 {
		return nil, false
	}

	// Single server: use DefaultLSPClient directly (backward compatible).
	if len(servers) == 1 {
		return buildSingleLSPClient(ctx, servers[0], logger)
	}

	// Multiple servers: use MultiLSPClient.
	multi := tools.NewMultiLSPClient()
	anyStarted := false
	for _, srv := range servers {
		client, started := buildSingleLSPClient(ctx, srv, logger)
		if !started {
			continue
		}
		anyStarted = true
		if len(srv.FileExtensions) > 0 {
			multi.Register(client, srv.FileExtensions...)
		} else {
			multi.SetDefaultClient(client)
		}
	}
	if !anyStarted {
		return nil, false
	}
	return multi, true
}

// buildSingleLSPClient starts and initializes a single DefaultLSPClient from
// the given LSPServerConfig. Returns (nil, false) on failure (graceful
// degradation).
func buildSingleLSPClient(ctx context.Context, srv config.LSPServerConfig, logger *slog.Logger) (tools.LSPClient, bool) {
	workspaceRoot := srv.WorkspaceRoot
	if workspaceRoot == "" {
		workspaceRoot, _ = os.Getwd() //nolint:errcheck
	}
	client, err := tools.NewDefaultLSPClient(ctx, srv.ServerCommand, workspaceRoot)
	if err != nil {
		logger.Warn("assemble_lsp_start_failed", "command", srv.ServerCommand, "err", err)
		return nil, false
	}
	if initErr := client.Initialize(ctx, "file://"+workspaceRoot); initErr != nil {
		logger.Warn("assemble_lsp_init_failed", "command", srv.ServerCommand, "err", initErr)
		_ = client.Shutdown(context.Background()) //nolint:errcheck
		return nil, false
	}
	logger.Info("assemble_lsp_server_ready", "command", srv.ServerCommand, "extensions", srv.FileExtensions)
	return client, true
}
