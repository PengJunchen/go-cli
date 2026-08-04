package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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
		maxTokens:     defaultMaxTokens,
		enableSession: false,
		agentName:     "main",
	}
	for _, o := range opts {
		o(&ac)
	}
	if ac.maxTokens <= 0 {
		ac.maxTokens = defaultMaxTokens
	}

	logger := slog.Default()

	// Registry: assemble components are also registered here so callers can
	// retrieve or override any subsystem via RegisterXxx. The registry is
	// additive; it does not change how components are constructed or wired.
	reg := core.NewRegistry().(*core.DefaultRegistry)

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

	// 2. Create tool registry + register defaults.
	tr := tools.NewDefaultToolRegistry()
	if registerErr := tools.RegisterDefaults(ctx, tr); registerErr != nil {
		cleanup()
		return nil, fmt.Errorf("assemble: register tools: %w", registerErr)
	}

	// 3. Register MCP tools.
	if mcpErr := registerMCPTools(ctx, rc, tr); mcpErr != nil {
		logger.Warn("assemble_mcp_failed", "err", mcpErr)
	}

	// 4. Register skill tools.
	if skillErr := registerSkillTools(ctx, rc, tr); skillErr != nil {
		logger.Warn("assemble_skill_failed", "err", skillErr)
	}

	// 5. Wire approval + mutation middleware via decorator pattern.
	classifier := approval.NewSafetyPolicyClassifier([]string{"bash"})
	approvalStore := approval.NewInMemoryApprovalStore()
	approvalMW := approval.NewApprovalMiddleware(
		classifier,
		approvalStore,
		approval.WithAutoApprove(false),
	)
	reg.RegisterApprovalClassifier(&approvalClassifierAdapter{inner: classifier})
	reg.RegisterApprovalStore(&approvalStoreAdapter{inner: approvalStore})
	tr = tools.NewMiddlewareToolRegistry(tr, approvalMW.WrapToolCall, tools.NewMutationWrapper())
	reg.RegisterToolRegistry(tr)

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

	// 7. Wire output guards (PII + code injection + length).
	guardChain := production.NewOutputGuardChain([]production.OutputGuard{
		production.NewPIIOutputGuard(),
		production.NewCodeInjectionGuard(),
		production.NewLengthGuard(100000),
	})

	// 8. Wire real SubAgent execution (replaces simulated runner).
	subAgentFactory := core.NewRealSubAgentFactory(model, tr,
		core.WithModelWrapper(newModelWrapper(pw, guardChain)),
	)
	core.RegisterSubAgentFactory(subAgentFactory)
	dispatcher := core.NewDefaultSubagentDispatcher(nil)
	if subErr := tr.Register(ctx, core.NewSubagentTool(dispatcher)); subErr != nil {
		logger.Warn("assemble_subagent_tool_failed", "err", subErr)
	}

	// 9. Register remaining unconnected tools (todo, task, goal, web, plan_mode, etc.).
	todoStore := tools.NewTodoStore()
	taskStore := tools.NewTaskStore()
	goalStore, _ := tools.NewDefaultGoalStore("")
	planCtrl := core.NewDefaultPlanModeController()
	hitlEmitter := &cliHITLEmitter{out: out}

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
		tools.NewWebFetchTool(),
		tools.NewWebSearchTool(),
		core.NewAskUserQuestionTool(hitlEmitter, 30*time.Second),
		tools.NewEnterPlanModeTool(planCtrl),
		tools.NewExitPlanModeTool(planCtrl),
	}
	for _, t := range extraTools {
		if err := tr.Register(ctx, t); err != nil {
			logger.Warn("assemble_tool_register_failed", "tool", t.Name(), "err", err)
		}
	}
	// tool_search needs visibility into all registered tools, so register last.
	if err := tr.Register(ctx, tools.NewToolSearchTool(tr)); err != nil {
		logger.Warn("assemble_tool_register_failed", "tool", "tool_search", "err", err)
	}

	// 10. Build loop agent with model wrapper.
	loopOpts := []core.LoopOption{
		core.WithLLM(model),
		core.WithTools(tr),
		core.WithModelWrapper(newModelWrapper(pw, guardChain)),
		core.WithExecutionMode(core.ExecutionModeParallel),
	}
	if rc != nil && rc.Agent.MaxIterations != 0 {
		loopOpts = append(loopOpts, core.WithMaxIterations(rc.Agent.MaxIterations))
	}
	// Allow config to override parallel execution to sequential.
	if rc != nil && rc.Tools.Parallel != nil && !*rc.Tools.Parallel {
		loopOpts = append(loopOpts, core.WithExecutionMode(core.ExecutionModeSequential))
	}
	var loop core.AgentLoop = core.NewLoopAgent(loopOpts...)

	// 11. Apply middleware chain (onion model) around the loop agent.
	loop = core.NewMiddlewareChain(
		core.NewLoggingMiddleware(ac.agentName),
		core.NewPlanModeMiddleware(planCtrl),
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
	estimator := compaction.NewHeuristicTokenEstimator()
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

	// Extend cleanup to include session store.
	prevCleanup := cleanup
	cleanup = func() {
		if sessionStore != nil {
			sessionStore.Close()
		}
		prevCleanup()
	}

	// 14. Resume history from session store if requested.
	var restoredHistory []core.AgentMessage
	if ac.resumeFlag && sessionStore != nil {
		restoredHistory, _ = loadSessionHistory(sessionStore.FilePath())
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
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(64))

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
		Harness:       h,
		Agent:         agent,
		ToolRegistry:  tr,
		CostTracker:   costTracker,
		StatsRegistry: statsRegistry,
		SessionStore:  sessionStore,
		SessionID:     sessionID,
		Model:         model,
		ModelName:     modelName,
		Compactor:     compactor,
		Estimator:     estimator,
		MidTurn:       midTurn,
		MaxTokens:     ac.maxTokens,
		Cleanup:       cleanup,
		Registry:      reg,
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
func registerSkillTools(ctx context.Context, rc *config.Config, tr tools.ToolRegistry) error {
	if rc == nil || rc.Skill.Dir == "" {
		return nil
	}

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(ctx, rc.Skill.Dir)
	if err != nil {
		slog.Warn("assemble_skill_load_failed", "dir", rc.Skill.Dir, "err", err)
		return nil
	}

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
		count++
	}
	if count > 0 {
		slog.Info("assemble_skills_registered", "dir", rc.Skill.Dir, "count", count)
	}
	return nil
}
