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

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mcp"
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
}

// AssembleOption configures AssembleAgent behavior.
type AssembleOption func(*assembleConfig)

type assembleConfig struct {
	maxTokens     int
	resumeFlag    bool
	enableSession bool
	agentName     string
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
	diffGen := tools.NewUnifiedDiffGenerator(0, false)

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

	// Resolve the working directory for git tools. There is no explicit config
	// for cwd, so fall back to the process working directory.
	gitCwd, err := os.Getwd()
	if err != nil {
		gitCwd = "."
	}
	gitTool := tools.NewDefaultGitTool(gitCwd)

	if registerErr := tools.RegisterDefaults(ctx, tr,
		tools.WithRegisteredFileTracker(fileTracker),
		tools.WithRegisteredDiffGenerator(diffGen),
		tools.WithRegisteredBashSandbox(bashSandbox),
		tools.WithRegisteredResourceLimits(resourceLimits),
		tools.WithRegisteredGitTool(gitTool),
	); registerErr != nil {
		cleanup()
		return nil, fmt.Errorf("assemble: register tools: %w", registerErr)
	}

	// 3. Register MCP tools.
	if mcpErr := registerMCPTools(ctx, rc, tr); mcpErr != nil {
		logger.Warn("assemble_mcp_failed", "err", mcpErr)
	}

	// 4. Register skill tools.
	skillInfos := registerSkillTools(ctx, rc, tr)

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
	steerCh := make(chan string, 1)
	loopOpts := []core.LoopOption{
		core.WithLLM(model),
		core.WithTools(tr),
		core.WithModelWrapper(newModelWrapper(pw, circuitBreaker, guardChain, telemetry)),
		core.WithExecutionMode(core.ExecutionModeParallel),
		core.WithTracer(tracer),
		core.WithSteeringChannel(steerCh),
	}
	if rc != nil && rc.Agent.MaxIterations != 0 {
		loopOpts = append(loopOpts, core.WithMaxIterations(rc.Agent.MaxIterations))
	}
	// Allow config to override parallel execution to sequential.
	if rc != nil && rc.Tools.Parallel != nil && !*rc.Tools.Parallel {
		loopOpts = append(loopOpts, core.WithExecutionMode(core.ExecutionModeSequential))
	}

	// 10b. Wire dynamic system prompt builder with project context.
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
			CustomPrompt: customPrompt,
			AppendPrompt: appendPrompt,
		}),
	)

	var loop core.AgentLoop = core.NewLoopAgent(loopOpts...)

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

	// 10c. Wire failure synthesis and hook chain.
	failureSynthesizer := core.NewDefaultFailureTurnSynthesizer()
	hookChain := core.NewHookChain()

	// 11. Apply middleware chain (onion model) around the loop agent.
	// Order (outermost first): logging -> loop-detector -> plan-mode ->
	// system-reminder -> failure-synthesis -> hook. The loop detector
	// observes events after Run and registers a SystemReminder; the
	// SystemReminderInjector injects it on the next turn. FailureSynthesis
	// retries recoverable errors once with a synthesized message. The Hook
	// middleware runs pre/post-turn hooks.
	loop = core.NewMiddlewareChain(
		core.NewLoggingMiddleware(ac.agentName),
		&loopDetectorMiddleware{detector: loopDetector, manager: reminderMgr},
		core.NewPlanModeMiddleware(planCtrl),
		core.NewSystemReminderInjector(reminderMgr),
		core.NewFailureSynthesisMiddleware(failureSynthesizer),
		core.NewHookMiddleware(hookChain),
	).Wrap(loop)

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

	// Extend cleanup to include session store and trace exporter.
	prevCleanup := cleanup
	cleanup = func() {
		if sessionStore != nil {
			sessionStore.Close() //nolint:errcheck,gosec
		}
		if traceExporter != nil {
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
// tools into the tool registry.
func registerMCPTools(ctx context.Context, rc *config.Config, tr tools.ToolRegistry) error {
	if rc == nil || len(rc.MCP.Servers) == 0 {
		return nil
	}

	for _, srv := range rc.MCP.Servers {
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

// registerSkillTools loads skills from the configured directory and registers
// them as tools. It supports two directory layouts:
//   - Flat: {dir}/{name}.md
//   - Nested: {dir}/{name}/SKILL.md
//
// It returns a slice of SkillInfo describing each registered skill, suitable
// for injection into the system prompt.
func registerSkillTools(ctx context.Context, rc *config.Config, tr tools.ToolRegistry) []core.SkillInfo {
	if rc == nil || rc.Skill.Dir == "" {
		return nil
	}

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(ctx, rc.Skill.Dir)
	if err != nil {
		slog.Warn("assemble_skill_load_failed", "dir", rc.Skill.Dir, "err", err)
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
			Name:     (*def).Name(),
			Category: (*def).Category(),
		})
		count++
	}
	if count > 0 {
		slog.Info("assemble_skills_registered", "dir", rc.Skill.Dir, "count", count)
	}
	return infos
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
