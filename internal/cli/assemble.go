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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

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
	"github.com/pengjunchen/go-cli/internal/tui"
)

// defaultMaxTokens is the default compaction token budget used when no
// explicit value is provided via AssembleOption.
const defaultMaxTokens = 8000

// CoreRuntime holds the primary agent execution components.
type CoreRuntime struct {
	Harness      *core.HarnessImpl
	Agent        *core.AgentImpl
	ToolRegistry tools.ToolRegistry
	TurnRunner   *core.EinoTurnRunner
	LoopAgent    *core.LoopAgent
	// Dispatcher is the sub-agent dispatcher used for ACP message dispatch.
	// It is exposed so the serve command can wire it into the CoreHandler
	// for remote SSE bridging.
	Dispatcher *core.DefaultSubagentDispatcher
	// EventBus is the shared event bus that receives a copy of every event
	// emitted by the harness's EventStream. It is exposed so the SSE server
	// can Subscribe for fan-out to multiple /events consumers.
	EventBus core.EventBus
}

// ModelConfig holds LLM model configuration and selection.
type ModelConfig struct {
	Model         llm.BaseChatModel
	ModelName     string
	ModelSelector *llm.DefaultModelSelector
	// ModelCycler rotates model selection across multiple providers. It is
	// nil when model cycling is not configured.
	ModelCycler *llm.ModelCycler
	// ModelRegistry is the external model metadata registry (e.g.
	// models.dev) used to enrich model info with pricing, context window and
	// modality. It is nil when the model registry is not enabled.
	ModelRegistry llm.ModelRegistry
	// ContextWindow is the model's total context window size (in tokens),
	// used for TUI status bar and /cost occupancy display. Falls back to
	// MaxTokens when the model doesn't report a context window.
	ContextWindow int
	// ThinkingLevel is the resolved LLM reasoning depth applied to every
	// Generate/Stream call. Defaults to ThinkingMedium when no explicit
	// option or config value is provided.
	ThinkingLevel llm.ThinkingLevel
}

// ProductionResilience holds fault tolerance and monitoring components.
type ProductionResilience struct {
	// CircuitBreaker protects the LLM model from cascading failures. When the
	// breaker is Open, Generate returns a fallback response.
	CircuitBreaker production.CircuitBreaker
	// LoopDetector monitors the agent event stream for recurring patterns
	// (repeated edits, test failures, identical tool calls).
	LoopDetector production.LoopDetector
	// IdempotentCache caches tool call results so that identical calls with
	// identical arguments return the stored result without re-executing.
	IdempotentCache production.IdempotentCache
	CostTracker     *production.CostTracker
	StatsRegistry   *production.StatsRegistry
	// Telemetry collects runtime metrics (LLM token usage, tool call counts,
	// execution duration) queryable for cost reporting.
	Telemetry production.Telemetry
}

// SessionManagement holds session persistence and memory components.
type SessionManagement struct {
	SessionStore *session.JSONLSessionStore
	SessionID    string
	// MemoryStore persists cross-session memories for the /memory slash
	// command and system prompt injection.
	MemoryStore *memory.FileMemoryStore
	// MemoryExtractor extracts key facts from conversations for memory
	// storage.
	MemoryExtractor memory.MemoryExtractor
	// MemoryWG tracks in-flight memory extraction goroutines so Cleanup
	// can wait for them before closing the MemoryStore.
	MemoryWG  *sync.WaitGroup
	Compactor compaction.Compactor
	Estimator compaction.TokenEstimator
	MidTurn   *compaction.MidTurnCompact
	MaxTokens int
}

// GitIntegration holds Git-related tools and managers.
type GitIntegration struct {
	// GitTool is the default git tool wired into the tool registry. It is
	// exposed so the interactive session can wire it into the session tree
	// for Git-aware branch linkage.
	GitTool tools.GitTool
	// WorktreeManager manages git worktrees for parallel session isolation.
	// It is nil when worktree isolation is not enabled in config.
	WorktreeManager *tools.WorktreeManager
	// SnapshotMgr captures git working-tree snapshots before file mutations so
	// files can be reverted to a previous state via the /revert slash command.
	// It is nil when the working directory is not a git repository.
	SnapshotMgr *tools.SnapshotManager
}

// Observability holds tracing and audit components.
type Observability struct {
	// Tracer is the tracing.Tracer created from config.Tracing. It is nil when
	// tracing is disabled (noop spans, zero overhead).
	Tracer *tracing.Tracer
	// AuditLog records tool calls as JSON-lines for later inspection. It is
	// nil when audit logging is disabled.
	AuditLog production.AuditLog
}

// InteractiveControl holds channels and emitters for interactive sessions.
type InteractiveControl struct {
	// SteerChannel is the shared steering channel between the
	// InterruptHandler (writer) and the LoopAgent (reader). The REPL writes
	// steering messages here when the user presses Esc and types an
	// instruction.
	SteerChannel chan string
	// ApprovalChannel is the shared channel between the TeaApprovalCallback
	// (writer, inside the approval middleware) and the BubbleteaApp (reader,
	// via tui.WithApprovalChannel). It is nil when the assembly was built
	// without TUI-based approval (non-interactive mode).
	ApprovalChannel chan tui.ApprovalRequest
	// FollowUpChannel is the shared follow-up channel between the
	// TurnRunner (writer) and the LoopAgent (reader). The REPL writes
	// follow-up messages here so the running loop picks them up between LLM
	// iterations.
	FollowUpChannel chan string
	// HITLEmitter is the CLI HITL question emitter used by the ask_user
	// tool. It is exposed so the REPL can set its program field to the
	// running tea.Program, routing HITL questions through the TUI instead
	// of stdout.
	HITLEmitter *cliHITLEmitter
}

// ToolingSupport holds tooling auxiliaries wired into the assembly.
type ToolingSupport struct {
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
	// ReminderManager manages system-reminder injections wired into the
	// middleware chain.
	ReminderManager core.SystemReminderManager
	// HookChain is the hook chain wired into the middleware chain. Hooks are
	// invoked before and after each turn.
	HookChain *core.HookChain
	// FailureSynthesizer converts recoverable errors into retry messages.
	FailureSynthesizer core.FailureTurnSynthesizer
	// LSPClient is the Language Server Protocol client wired for code
	// completion. It is nil when no LSP server is configured or started.
	LSPClient tools.LSPClient
	// LSPWorkspaceRoot is the workspace root used to initialize the LSP
	// server. Used by the LSPCompleter to construct file URIs.
	LSPWorkspaceRoot string
}

// AgentAssembly holds the fully wired agent runtime components produced by
// AssembleAgent. Callers use the Harness to submit prompts and access the
// other fields for slash commands, cost reporting, and session management.
//
// The struct is composed of focused sub-structs, each grouping fields by
// architectural responsibility. Fields are accessible both via the sub-struct
// (e.g. assembly.CoreRuntime.Harness) and via Go's promoted-field shorthand
// (e.g. assembly.Harness) for backward compatibility.
type AgentAssembly struct {
	CoreRuntime
	ModelConfig
	ProductionResilience
	SessionManagement
	GitIntegration
	Observability
	InteractiveControl
	ToolingSupport

	// Registry exposes the assembled components through the core.DefaultRegistry
	// so callers can retrieve or override any subsystem via RegisterXxx. It is
	// additive: the components above are still constructed and wired exactly as
	// before, they are also registered here for dependency-injection use.
	Registry *core.DefaultRegistry
	Cleanup  func()
}

// AssembleOption configures AssembleAgent behavior.
type AssembleOption func(*assembleConfig)

type assembleConfig struct {
	maxTokens     int
	resumeFlag    bool
	enableSession bool
	agentName     string
	thinkingLevel *llm.ThinkingLevel
	// noSandbox disables bash sandbox enforcement, replacing the default
	// sandbox with an AllowAllSandbox that permits every command.
	noSandbox bool
	// approvalCh, when non-nil, enables TUI-based approval via TeaApprovalCallback.
	approvalCh chan tui.ApprovalRequest
	// approveMode controls headless tool approval behavior when approvalCh is
	// nil. ApproveAsk (default) uses SafetyPolicyClassifier with interactive
	// callback; ApproveAuto uses AllowAllClassifier; ApproveDeny uses
	// DenyAllClassifier.
	approveMode ApproveMode
}

// WithMaxTokens sets the compaction token budget.
func WithMaxTokens(n int) AssembleOption {
	return func(c *assembleConfig) { c.maxTokens = n }
}

// WithApprovalChannel enables TUI-based interactive approval. When set,
// AssembleAgent constructs a TeaApprovalCallback backed by this channel instead
// of the fallback InteractiveApprovalCallback. The same channel must be passed
// to the BubbleteaApp via tui.WithApprovalChannel.
func WithApprovalChannel(ch chan tui.ApprovalRequest) AssembleOption {
	return func(c *assembleConfig) { c.approvalCh = ch }
}

// WithApproveMode sets the headless tool approval mode. This is used by the
// prompt command when no TUI approval channel is available.
// ApproveAuto automatically approves all tool calls; ApproveDeny automatically
// denies all tool calls; ApproveAsk (default) prompts via InteractiveApprovalCallback.
func WithApproveMode(mode ApproveMode) AssembleOption {
	return func(c *assembleConfig) { c.approveMode = mode }
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

// WithNoSandbox disables bash sandbox enforcement. When set, the bash tool
// uses an AllowAllSandbox that permits every command instead of the default
// sandbox derived from configuration.
func WithNoSandbox() AssembleOption {
	return func(c *assembleConfig) { c.noSandbox = true }
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
	s, thinkingLevel := newAssembleState(ctx, rc, providerName, modelName, out, opts...)

	// Assemble subsystems in dependency order.
	if err := s.assembleModel(); err != nil {
		s.runCleanup()
		return nil, err
	}
	s.assembleTracing()
	if err := s.assembleTools(); err != nil {
		s.runCleanup()
		return nil, err
	}
	s.assembleExtensions()
	s.assembleApproval()
	s.assembleProductionResilience()
	s.assembleOutputGuards()
	s.assembleSubAgent()
	s.assembleExtraTools()
	s.assembleLoopAgent()
	s.assembleMiddleware()
	if err := s.assembleCompactor(); err != nil {
		s.runCleanup()
		return nil, err
	}
	s.assembleSession()
	s.appendFinalCleanup()

	// Resume history from session store if requested.
	var restoredHistory []core.AgentMessage
	if s.ac.resumeFlag && s.sessionStore != nil {
		restoredHistory, _ = loadSessionHistory(s.sessionStore.FilePath()) //nolint:errcheck
		if len(restoredHistory) > 0 {
			s.logger.Info("assemble_session_resumed", "messages", len(restoredHistory))
		}
	}

	// Build AgentImpl + HarnessImpl.
	agentOpts := []core.AgentOption{
		core.WithCompactionHook(newCompactionHook(s.compactor, s.estimator, s.ac.maxTokens)),
	}
	if len(restoredHistory) > 0 {
		agentOpts = append(agentOpts, core.WithHistory(restoredHistory))
	}
	agent := core.NewAgentImpl(s.ac.agentName, s.loop, agentOpts...)
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(256), core.WithHarnessTracer(s.tracer), core.WithRunSlotGuard(s.runSlotGuard), core.WithHarnessEventBus(s.eventBus))

	// Build TurnRunner wired with the shared steering channel and agent.
	turnRunner := core.NewEinoTurnRunner(s.loop)
	turnRunner.SetSteerChannel(s.steerCh)
	turnRunner.SetFollowUpChannel(s.followUpCh)
	turnRunner.SetAgent(agent)
	turnRunner.SetHookChain(s.hookChain)
	turnRunner.SetRunSlotGuard(s.runSlotGuard)
	s.reg.RegisterTurnRunner(turnRunner)

	// Emit assemble span for observability.
	asmSpan, _ := tracing.SpanFromContext(ctx, "assemble.agent", tracing.SpanKindInternal)
	asmSpan.SetAttributes(
		tracing.Attribute{Key: "agent_name", Value: s.ac.agentName},
		tracing.Attribute{Key: "model", Value: modelName},
		tracing.Attribute{Key: "session_enabled", Value: s.ac.enableSession},
		tracing.Attribute{Key: "resume", Value: s.ac.resumeFlag},
	)
	asmSpan.SetStatus(tracing.SpanStatusOK, "")
	asmSpan.End()

	// Resolve the context window for TUI status bar and /cost occupancy.
	contextWindow := s.modelInfo.ContextWindow
	if contextWindow <= 0 {
		contextWindow = s.ac.maxTokens
	}

	return &AgentAssembly{
		CoreRuntime: CoreRuntime{
			Harness:      h,
			Agent:        agent,
			ToolRegistry: s.tr,
			TurnRunner:   turnRunner,
			LoopAgent:    s.loopAgent,
			Dispatcher:   s.dispatcher,
			EventBus:     s.eventBus,
		},
		ModelConfig: ModelConfig{
			Model:         s.model,
			ModelName:     modelName,
			ModelSelector: s.modelSelector,
			ModelCycler:   s.modelCycler,
			ModelRegistry: s.modelRegistry,
			ContextWindow: contextWindow,
			ThinkingLevel: thinkingLevel,
		},
		ProductionResilience: ProductionResilience{
			CircuitBreaker:  s.circuitBreaker,
			LoopDetector:    s.loopDetector,
			IdempotentCache: s.idempotentCache,
			CostTracker:     s.costTracker,
			StatsRegistry:   s.statsRegistry,
			Telemetry:       s.telemetry,
		},
		SessionManagement: SessionManagement{
			SessionStore:    s.sessionStore,
			SessionID:       s.sessionID,
			MemoryStore:     s.memStore,
			MemoryExtractor: s.memExtractor,
			MemoryWG:        &s.memWG,
			Compactor:       s.compactor,
			Estimator:       s.estimator,
			MidTurn:         s.midTurn,
			MaxTokens:       s.ac.maxTokens,
		},
		GitIntegration: GitIntegration{
			GitTool:         s.gitTool,
			WorktreeManager: s.worktreeMgr,
			SnapshotMgr:     s.snapshotMgr,
		},
		Observability: Observability{
			Tracer:   s.tracer,
			AuditLog: s.auditLog,
		},
		InteractiveControl: InteractiveControl{
			SteerChannel:    s.steerCh,
			ApprovalChannel: s.ac.approvalCh,
			FollowUpChannel: s.followUpCh,
			HITLEmitter:     s.hitlEmitter,
		},
		ToolingSupport: ToolingSupport{
			FileTracker:        s.fileTracker,
			DiffGenerator:      s.diffGen,
			PlanCtrl:           s.planCtrl,
			ModeResolver:       s.modeResolver,
			PromptBuilder:      s.promptBuilder,
			ContextLoader:      s.contextLoader,
			ReminderManager:    s.reminderMgr,
			HookChain:          s.hookChain,
			FailureSynthesizer: s.failureSynthesizer,
			LSPClient:          s.lspClientField,
			LSPWorkspaceRoot:   s.lspWorkspaceRoot,
		},
		Registry: s.reg,
		Cleanup:  s.runCleanup,
	}, nil
}

// newAssembleState resolves the assembleConfig, thinking level, logger,
// registry, and session ID, then constructs the shared assembleState.
func newAssembleState(
	ctx context.Context,
	rc *config.Config,
	providerName, modelName string,
	out io.Writer,
	opts ...AssembleOption,
) (*assembleState, llm.ThinkingLevel) {
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
	reg := core.NewRegistry().(*core.DefaultRegistry) //nolint:errcheck

	// Resolve session ID: config.Session.ID takes priority, fallback to agentName.
	sessionID := ac.agentName
	if rc != nil && rc.Session.ID != "" {
		sessionID = rc.Session.ID
	}

	s := &assembleState{
		ctx:           ctx,
		rc:            rc,
		ac:            ac,
		logger:        logger,
		reg:           reg,
		sessionID:     sessionID,
		providerName:  providerName,
		modelName:     modelName,
		out:           out,
		thinkingLevel: thinkingLevel,
		eventBus:      core.NewMemoryEventBus(),
	}
	return s, thinkingLevel
}

// assembleState holds intermediate assembly state shared across assembleXXX
// methods. It is internal to the assembly process and not exposed to callers.
type assembleState struct {
	// Common dependencies
	ctx           context.Context
	rc            *config.Config
	ac            assembleConfig
	logger        *slog.Logger
	reg           *core.DefaultRegistry
	sessionID     string
	providerName  string
	modelName     string
	out           io.Writer
	thinkingLevel llm.ThinkingLevel

	// Model section
	model         llm.BaseChatModel
	modelInfo     llm.ModelInfo
	modelCycler   *llm.ModelCycler
	modelRegistry llm.ModelRegistry
	smallModel    llm.BaseChatModel
	modelSelector *llm.DefaultModelSelector

	// Tracing section
	tracer        *tracing.Tracer
	traceExporter tracing.TraceExporter

	// Tools section
	tr               tools.ToolRegistry
	underlyingReg    tools.ToolRegistry
	fileTracker      *tools.FileTracker
	diffGen          tools.DiffGenerator
	gitTool          tools.GitTool
	gitCwd           string
	worktreeMgr      *tools.WorktreeManager
	snapshotMgr      *tools.SnapshotManager
	htmlConverter    *tools.DefaultHTMLConverter
	lspClientField   tools.LSPClient
	lspWorkspaceRoot string
	skillInfos       []core.SkillInfo

	// Extensions section
	extHooks      []core.Hook
	extMiddleware []core.Middleware
	hookChain     *core.HookChain
	hookMgr       *core.HookManager

	// Approval section
	modeResolver approval.PermissionModeResolver

	// Production resilience section
	planCtrl        core.PlanModeController
	pw              *production.ProductionModelWrapper
	modelChain      *llm.DefaultModelMiddlewareChain
	circuitBreaker  production.CircuitBreaker
	guardChain      *production.OutputGuardChain
	telemetry       production.Telemetry
	costTracker     *production.CostTracker
	statsRegistry   *production.StatsRegistry
	idempotentCache production.IdempotentCache
	auditLog        production.AuditLog

	// SubAgent section
	acpAdapter *acp.ACPMiddlewareAdapter
	acpClient  acp.ACPClient
	dispatcher *core.DefaultSubagentDispatcher

	// Extra tools section
	hitlEmitter *cliHITLEmitter

	// LoopAgent section
	steerCh       chan string
	followUpCh    chan string
	runSlotGuard  core.RunSlotGuard
	loopAgent     *core.LoopAgent
	loop          core.AgentLoop
	memStore      *memory.FileMemoryStore
	memExtractor  memory.MemoryExtractor
	memWG         sync.WaitGroup
	promptBuilder core.SystemPromptBuilder
	contextLoader core.ProjectContextLoader

	// EventBus section
	eventBus core.EventBus

	// Middleware section
	loopDetector       production.LoopDetector
	reminderMgr        *core.DefaultSystemReminderManager
	failureSynthesizer core.FailureTurnSynthesizer

	// Compactor section
	compactor compaction.Compactor
	estimator compaction.TokenEstimator
	midTurn   *compaction.MidTurnCompact

	// Session section
	sessionStore *session.JSONLSessionStore

	// Cleanup
	cleanupList []func()
}

// runCleanup executes all registered cleanup functions in reverse order (LIFO).
func (s *assembleState) runCleanup() {
	for i := len(s.cleanupList) - 1; i >= 0; i-- {
		s.cleanupList[i]()
	}
}

// appendFinalCleanup adds the terminal cleanup closure for ACP client,
// session store, memory store, and trace exporter. These run first during
// LIFO cleanup because they are appended after all subsystem cleanups.
func (s *assembleState) appendFinalCleanup() {
	s.cleanupList = append(s.cleanupList, func() {
		StopHotReloaders()
		if s.eventBus != nil {
			s.eventBus.Close()
		}
		if s.acpAdapter != nil {
			s.acpAdapter.Close()
		}
		if s.acpClient != nil {
			_ = s.acpClient.Disconnect(context.Background()) //nolint:errcheck
		}
		if s.sessionStore != nil {
			s.sessionStore.Close() //nolint:errcheck,gosec
		}
		if s.memStore != nil {
			s.memWG.Wait()
			_ = s.memStore.Close() //nolint:errcheck
		}
		if s.traceExporter != nil {
			if s.tracer != nil {
				s.tracer.Flush()
			}
			_ = s.traceExporter.Shutdown(context.Background()) //nolint:errcheck
		}
	})
}

// assembleModel builds the primary model, ModelCycler, ModelRegistry, and small
// model. It registers the model provider and appends model cleanups.
func (s *assembleState) assembleModel() error {
	model, modelInfo, modelCleanup, err := buildModel(s.ctx, s.rc, s.providerName, s.modelName)
	if err != nil {
		return fmt.Errorf("assemble: build model: %w", err)
	}
	s.reg.RegisterModelProvider(&chatModelProvider{name: s.modelName, model: model})
	s.model = model
	s.modelInfo = modelInfo
	if modelCleanup != nil {
		s.cleanupList = append(s.cleanupList, modelCleanup)
	}

	// Wire ModelCycler when enabled models are configured.
	if s.rc != nil && s.rc.ModelCycler.Enabled != nil && *s.rc.ModelCycler.Enabled && len(s.rc.ModelCycler.Models) > 0 {
		entries := make([]llm.ModelEntry, len(s.rc.ModelCycler.Models))
		for i, m := range s.rc.ModelCycler.Models {
			entries[i] = llm.ModelEntry{
				Provider: m.Provider,
				Model:    m.Model,
				Weight:   m.Weight,
				TaskType: llm.TaskType(m.TaskType),
			}
		}
		s.modelCycler = llm.NewModelCycler(llm.ModelCyclerConfig{
			Models:   entries,
			Strategy: s.rc.ModelCycler.Strategy,
		})
		s.modelCycler.WithRegistry(llm.NewProviderRegistry())
		s.model = s.modelCycler.WrapModel(s.model)
		s.logger.Info("assemble_model_cycler_enabled",
			"strategy", s.rc.ModelCycler.Strategy,
			"models", len(entries),
		)
	}

	// Wire the models.dev model registry when enabled.
	s.modelRegistry = llm.NoopModelRegistry{}
	if s.rc != nil && s.rc.ModelRegistry.Enabled {
		ttl := time.Duration(s.rc.ModelRegistry.TTLHours) * time.Hour
		mr := llm.NewModelsDevRegistry(s.rc.ModelRegistry.CachePath, ttl)
		if rErr := mr.Refresh(s.ctx); rErr != nil {
			s.logger.Warn("assemble_model_registry_refresh_failed", "err", rErr)
		} else {
			s.logger.Info("assemble_model_registry_ready",
				"providers", len(mr.Providers()),
			)
		}
		s.modelRegistry = mr
	}

	// Build small model for background tasks.
	if s.rc != nil && s.rc.SmallModel.Model != "" {
		smallProvider := s.rc.SmallModel.Provider
		if smallProvider == "" {
			smallProvider = s.providerName
		}
		sm, _, smallCleanup, smErr := buildSmallModel(s.ctx, s.rc, smallProvider, s.rc.SmallModel.Model)
		if smErr != nil {
			s.logger.Warn("assemble_small_model_failed", "err", smErr, "model", s.rc.SmallModel.Model)
		} else {
			s.smallModel = sm
			if smallCleanup != nil {
				s.cleanupList = append(s.cleanupList, smallCleanup)
			}
			s.logger.Info("assemble_small_model_enabled", "model", s.rc.SmallModel.Model)
		}
	}

	// Build the DefaultModelSelector with registry awareness for token-aware
	// model routing. The selector wraps the primary and small models and,
	// when the registry is available, can query it for model limits via
	// SelectModelWithTokens.
	smallProvider := ""
	smallModelName := ""
	if s.rc != nil && s.rc.SmallModel.Model != "" {
		smallProvider = s.rc.SmallModel.Provider
		if smallProvider == "" {
			smallProvider = s.providerName
		}
		smallModelName = s.rc.SmallModel.Model
	}
	s.modelSelector = llm.NewDefaultModelSelector(s.model, s.smallModel).
		WithModelRegistry(s.modelRegistry).
		WithModelNames(s.providerName, s.modelName, smallProvider, smallModelName).
		WithModelBuilder(func(ctx context.Context, name string) (llm.BaseChatModel, func(), error) {
			m, _, cleanup, err := buildModel(ctx, s.rc, s.providerName, name)
			return m, cleanup, err
		}).
		WithModelLister(func() []llm.ModelInfo {
			return listAvailableModels(s.rc, s.providerName)
		}).
		WithModelSwitchCallback(func(m llm.BaseChatModel) {
			if s.loopAgent != nil {
				s.loopAgent.SetModel(m)
			}
		})

	return nil
}

// assembleTracing wires the tracer and trace exporter from config.
func (s *assembleState) assembleTracing() {
	if s.rc != nil && s.rc.Tracing.Enabled != nil && *s.rc.Tracing.Enabled {
		s.traceExporter = buildTraceExporter(s.rc.Tracing, s.sessionID, s.logger)
		if s.traceExporter != nil {
			redactionLevel := tracing.RedactionLevelRedact
			if s.rc.Tracing.RedactionLevel != "" {
				redactionLevel = tracing.RedactionLevel(s.rc.Tracing.RedactionLevel)
			}
			s.traceExporter = tracing.NewRedactingExporter(s.traceExporter, redactionLevel)
			s.tracer = tracing.NewTracer(s.sessionID, s.traceExporter)
			s.reg.RegisterTraceExporter(s.traceExporter)
			s.logger.Info("assemble_tracing_enabled", "exporter", s.rc.Tracing.Exporter, "level", s.rc.Tracing.Level)
		}
	}
}

// assembleTools creates the tool registry, registers builtins, custom tools,
// MCP tools, LSP tools, remote bash, and skill tools.
func (s *assembleState) assembleTools() error {
	s.underlyingReg = tools.NewDefaultToolRegistry()
	dtr := tools.NewDeferredToolRegistryAdapter(s.underlyingReg)
	s.tr = dtr

	// Create shared component instances for the PARTIAL tools.
	s.fileTracker = tools.NewFileTracker()
	s.diffGen = tools.NewUnifiedDiffGenerator(0, false)

	// Build the bash sandbox from config.
	var bashSandbox tools.BashSandbox
	var resourceLimits tools.ResourceLimits
	if s.ac.noSandbox {
		bashSandbox = tools.AllowAllSandbox{}
	} else {
		var sandboxOpts []tools.SandboxOption
		if s.rc != nil {
			sandboxOpts = append(sandboxOpts, tools.WithAllowedPaths(s.rc.Sandbox.AllowedPaths))
			resourceLimits = tools.ResourceLimits{
				MaxCPU:    s.rc.Sandbox.MaxCPU,
				MaxMemory: s.rc.Sandbox.MaxMemory,
			}
		} else {
			sandboxOpts = append(sandboxOpts, tools.WithAllowedPaths(nil))
		}
		bashSandbox = tools.NewDefaultBashSandbox(sandboxOpts...)
	}

	s.htmlConverter = tools.NewDefaultHTMLConverter()

	// Resolve the working directory for git tools.
	gitCwd, err := os.Getwd()
	if err != nil {
		gitCwd = "."
	}
	if s.rc != nil && s.rc.Git.WorkDir != "" {
		gitCwd = s.rc.Git.WorkDir
	}
	s.gitCwd = gitCwd
	s.gitTool = tools.NewDefaultGitTool(gitCwd)

	// Create a SnapshotManager anchored at the git working directory. When the
	// directory is not a git repository the manager disables itself (AC-5).
	// Wire it into the FileTracker so every queued mutation captures a
	// pre-mutation snapshot that /revert can roll back to.
	s.snapshotMgr = tools.NewSnapshotManager(gitCwd)
	s.fileTracker.SetSnapshotManager(s.snapshotMgr)

	// Create a WorktreeManager when worktree isolation is enabled.
	if s.rc != nil && s.rc.Git.WorktreeEnabled {
		wtDir := s.rc.Git.WorktreeDir
		if wtDir == "" {
			wtDir = filepath.Join(s.gitCwd, ".go-cli", "worktrees")
		}
		s.worktreeMgr = tools.NewWorktreeManager(s.gitTool, wtDir)
		if err := s.worktreeMgr.EnsureBaseDir(); err != nil {
			s.logger.Warn("assemble_worktree_base_dir_failed", "err", err)
		}
		wtMgr := s.worktreeMgr
		// Register a SIGINT/SIGTERM handler so that worktrees are cleaned up
		// even when the process is interrupted before normal shutdown. The
		// goroutine exits without cleaning up when wtDone is closed (normal
		// shutdown path), avoiding duplicate cleanup. Cleanup is idempotent
		// (sync.Once), so a concurrent signal during shutdown is safe.
		wtSigCh := make(chan os.Signal, 1)
		signal.Notify(wtSigCh, syscall.SIGINT, syscall.SIGTERM)
		wtDone := make(chan struct{})
		wtMgr.StartSignalCleanup(wtSigCh, wtDone)
		s.cleanupList = append(s.cleanupList, func() {
			signal.Stop(wtSigCh)
			close(wtDone)
			if err := wtMgr.Cleanup(context.Background()); err != nil {
				s.logger.Warn("assemble_worktree_cleanup_failed", "err", err)
			}
		})
	}

	// Wrap the UnifiedDiffGenerator with GitDiffGenerator.
	s.diffGen = tools.NewGitDiffGenerator(s.gitTool, s.diffGen)

	regOpts := []tools.RegisterDefaultsOption{
		tools.WithRegisteredFileTracker(s.fileTracker),
		tools.WithRegisteredDiffGenerator(s.diffGen),
		tools.WithRegisteredBashSandbox(bashSandbox),
		tools.WithRegisteredResourceLimits(resourceLimits),
		tools.WithRegisteredGitTool(s.gitTool),
	}
	if s.rc != nil && len(s.rc.Tools.Builtin) > 0 {
		regOpts = append(regOpts, tools.WithRegisteredBuiltinWhitelist(s.rc.Tools.Builtin))
	}
	if registerErr := tools.RegisterDefaults(s.ctx, s.tr, regOpts...); registerErr != nil {
		return fmt.Errorf("assemble: register tools: %w", registerErr)
	}

	// Register config-driven custom command tools.
	registerCustomTools(s.ctx, s.rc, s.tr, s.logger)

	// Register MCP tools.
	if mcpErr := registerMCPTools(s.ctx, s.rc, dtr); mcpErr != nil {
		s.logger.Warn("assemble_mcp_failed", "err", mcpErr)
	}

	// Register LSP tool (if configured).
	if s.rc != nil && (len(s.rc.LSP.ServerCommand) > 0 || len(s.rc.LSP.Servers) > 0) {
		lspClient, lspStarted := buildLSPClient(s.ctx, s.rc, s.logger)
		if lspClient != nil && lspStarted {
			if regErr := s.tr.Register(s.ctx, tools.NewLSPTool(lspClient)); regErr != nil {
				s.logger.Warn("assemble_lsp_register_failed", "err", regErr)
				_ = lspClient.Shutdown(context.Background()) //nolint:errcheck
			} else {
				s.lspClientField = lspClient
				s.lspWorkspaceRoot = resolveLSPWorkspaceRoot(s.rc)
				client := lspClient
				s.cleanupList = append(s.cleanupList, func() {
					_ = client.Shutdown(context.Background()) //nolint:errcheck
				})
				s.logger.Info("assemble_lsp_ready")
			}
		}
	}

	// Register remote bash tool (if SSH hosts are configured).
	if s.rc != nil && len(s.rc.Remote.Hosts) > 0 {
		remoteTool := buildRemoteBashTool(s.ctx, s.rc, s.logger)
		if remoteTool != nil {
			if regErr := s.tr.Register(s.ctx, remoteTool); regErr != nil {
				s.logger.Warn("assemble_remote_bash_register_failed", "err", regErr)
			} else {
				s.logger.Info("assemble_remote_bash_ready", "default_host", s.rc.Remote.DefaultHost)
			}
		}
	}

	// Register skill tools.
	s.skillInfos = registerSkillTools(s.ctx, s.rc, dtr)

	return nil
}

// assembleExtensions loads and initializes extensions, registering their tools,
// hooks, and middleware. It also builds the hook chain.
func (s *assembleState) assembleExtensions() {
	if s.rc != nil && s.rc.Extensions.Enabled != nil && *s.rc.Extensions.Enabled && len(s.rc.Extensions.PluginPaths) > 0 {
		pm := extension.NewPluginManager(extension.NewDefaultPluginLoader())
		_ = pm.Load(s.ctx, s.rc.Extensions.PluginPaths) //nolint:errcheck
		_ = pm.Init(s.ctx)                              //nolint:errcheck
		for _, t := range pm.Tools() {
			if regErr := s.tr.Register(s.ctx, t); regErr != nil {
				s.logger.Warn("assemble_extension_tool_register_failed", "tool", t.Name(), "err", regErr)
			}
		}
		for _, h := range pm.Hooks() {
			s.extHooks = append(s.extHooks, newExtensionHookAdapter(h))
		}
		for _, m := range pm.Middleware() {
			s.extMiddleware = append(s.extMiddleware, newExtensionMiddlewareAdapter(m))
		}
		s.logger.Info("assemble_extensions_ready", "extensions", len(pm.Extensions()), "paths", len(s.rc.Extensions.PluginPaths), "hooks", len(s.extHooks), "middleware", len(s.extMiddleware))
		s.cleanupList = append(s.cleanupList, func() {
			_ = pm.Shutdown(context.Background()) //nolint:errcheck
		})
	}

	// Load user-configured shell hooks from .go-cli/hooks.yaml.
	hooksPath := filepath.Join(s.gitCwd, ".go-cli", "hooks.yaml")
	if _, statErr := os.Stat(hooksPath); statErr == nil {
		s.loadShellHooks(hooksPath)
	}

	s.hookChain = core.NewHookChain(s.extHooks...)
}

// loadShellHooks reads a hooks.yaml file, parses shell hook definitions, and
// appends a HookManager to s.extHooks.
func (s *assembleState) loadShellHooks(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		s.logger.Warn("assemble_hooks_read_failed", "path", path, "err", err)
		return
	}
	var hooksCfg config.HooksConfig
	if err := config.UnmarshalConfig(data, config.ConfigFormatYAML, &hooksCfg); err != nil {
		s.logger.Warn("assemble_hooks_parse_failed", "path", path, "err", err)
		return
	}
	var shellHooks []core.HookSystem
	for i, hc := range hooksCfg.Hooks {
		event, eventErr := parseHookEvent(hc.Event)
		if eventErr != nil {
			s.logger.Warn("assemble_hook_event_unknown", "index", i, "event", hc.Event, "err", eventErr)
			continue
		}
		timeoutStr := hc.Timeout
		if timeoutStr == "" {
			timeoutStr = "10s"
		}
		timeout, timeoutErr := time.ParseDuration(timeoutStr)
		if timeoutErr != nil {
			s.logger.Warn("assemble_hook_timeout_parse_failed", "index", i, "timeout", hc.Timeout, "err", timeoutErr)
			timeout = 10 * time.Second
		}
		name := fmt.Sprintf("shell-hook-%d", i)
		shellHooks = append(shellHooks, core.NewShellHook(name, event, hc.Command, timeout))
	}
	if len(shellHooks) > 0 {
		s.hookMgr = core.NewHookManager(shellHooks...)
		s.extHooks = append(s.extHooks, s.hookMgr)
	}
	s.logger.Info("assemble_hooks_ready", "path", path, "count", len(shellHooks))
}

// parseHookEvent converts a config event string to a core.HookEvent.
func parseHookEvent(s string) (core.HookEvent, error) {
	switch core.HookEvent(s) {
	case core.EventPreToolUse, core.EventPostToolUse, core.EventSessionStart, core.EventSessionEnd:
		return core.HookEvent(s), nil
	default:
		return "", fmt.Errorf("unknown hook event: %q", s)
	}
}

// assembleApproval wires the approval middleware and mutation queue, wrapping
// the tool registry.
func (s *assembleState) assembleApproval() {
	var classifier approval.ApprovalClassifier
	var approvalCallback approval.ApprovalCallback
	var autoApprove bool

	if s.ac.approvalCh != nil {
		classifier = approval.NewSafetyPolicyClassifier([]string{"bash"})
		approvalCallback = approval.NewTeaApprovalCallback(s.ac.approvalCh,
			approval.WithDiffPreviewFunc(buildDiffPreviewFn(s.diffGen)),
		)
		autoApprove = false
	} else {
		switch s.ac.approveMode {
		case ApproveAuto:
			classifier = approval.AllowAllClassifier{}
			approvalCallback = nil
			autoApprove = true
		case ApproveDeny:
			classifier = approval.DenyAllClassifier{}
			approvalCallback = nil
			autoApprove = false
		default:
			classifier = approval.NewSafetyPolicyClassifier([]string{"bash"})
			approvalCallback = approval.NewInteractiveApprovalCallback(os.Stdin, os.Stderr)
			autoApprove = false
		}
	}

	// Determine the permission mode for audit metadata.
	auditMode := approval.PermissionDefault
	if s.ac.approvalCh == nil {
		switch s.ac.approveMode {
		case ApproveAuto:
			auditMode = approval.PermissionAutoFull
		case ApproveDeny:
			auditMode = approval.PermissionDefault
		default:
			auditMode = approval.PermissionDefault
		}
	}

	// Wrap the classifier with an append-only audit trail so every classification
	// decision is recorded to .go-cli/audit/{date}.jsonl.
	auditDir := filepath.Join(s.gitCwd, ".go-cli", "audit")
	auditTrail := approval.NewAuditTrail(auditDir)
	classifier = approval.NewAuditClassifier(classifier, auditTrail, auditMode, s.sessionID)

	approvalStore := approval.NewInMemoryApprovalStore()
	approvalCache := approval.NewApprovalCache("")
	s.modeResolver = approval.NewDefaultPermissionModeResolver()

	var mwOpts []approval.Option
	mwOpts = append(mwOpts,
		approval.WithAutoApprove(autoApprove),
		approval.WithCache(approvalCache),
	)
	if s.ac.approvalCh != nil {
		// Wrap the resolver with an AuditResolver so that classifiers produced
		// by Resolve are also audit-recording. Without this the middleware's
		// effectiveClassifier path would bypass the AuditClassifier wrapper and
		// TUI interactive mode (the primary approval scenario) would not write
		// audit records.
		auditResolver := approval.NewAuditResolver(s.modeResolver, auditTrail, s.sessionID)
		mwOpts = append(mwOpts, approval.WithPermissionModeResolver(auditResolver))
	}
	if approvalCallback != nil {
		mwOpts = append(mwOpts, approval.WithCallback(approvalCallback))
	}
	approvalMW := approval.NewApprovalMiddleware(classifier, approvalStore, mwOpts...)
	s.reg.RegisterApprovalClassifier(&approvalClassifierAdapter{inner: classifier})
	s.reg.RegisterApprovalStore(&approvalStoreAdapter{inner: approvalStore})
	mutationQueue := tools.NewDefaultFileMutationQueue(
		tools.WithMutationFileTracker(s.fileTracker),
		tools.WithMutationDiffGenerator(s.diffGen),
	)
	s.tr = tools.NewMiddlewareToolRegistry(s.tr, approvalMW.WrapToolCall, tools.NewMutationQueueWrapper(mutationQueue))
	s.reg.RegisterToolRegistry(s.tr)

	mq := mutationQueue
	s.cleanupList = append(s.cleanupList, func() {
		if cq, ok := mq.(*tools.DefaultFileMutationQueue); ok {
			_ = cq.Close() //nolint:errcheck
		}
	})
}

// assembleProductionResilience wires retry, cost tracking, circuit breaker,
// idempotent cache, audit log, telemetry, and the plan-mode controller. It
// wraps the tool registry with production middleware.
func (s *assembleState) assembleProductionResilience() {
	retryPolicy := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Second,
		MaxDelay:    10 * time.Second,
	})
	s.costTracker = production.NewCostTracker(nil)
	s.statsRegistry = production.NewStatsRegistry()
	s.pw = production.NewProductionModelWrapper(
		production.WithWrapperCostTracker(s.costTracker),
		production.WithWrapperStatsRegistry(s.statsRegistry),
		production.WithWrapperModelName(s.modelName),
		production.WithWrapperSessionID(s.sessionID),
	)

	s.modelChain = llm.NewStandardMiddlewareChain(
		llm.NewFailoverModelMiddleware(),
		llm.NewRetryModelMiddleware(llm.WithRetryPolicy(&retryPolicyAdapter{inner: retryPolicy})),
		llm.NewTimeoutModelMiddleware(),
		llm.NewSanitizeModelMiddleware(),
		llm.NewLoopDetectionModelMiddleware(),
		llm.NewValidateModelMiddleware(),
		llm.NewOverflowRecoveryMiddleware(),
	)

	// Wire circuit breaker.
	cbCfg := production.CircuitBreakerConfig{}
	if s.rc != nil {
		cbCfg.FailureThreshold = s.rc.Production.CircuitBreaker.Threshold
		cbCfg.RecoveryTimeout = s.rc.Production.CircuitBreaker.ResetTimeout
	}
	s.circuitBreaker = production.NewDefaultCircuitBreaker(cbCfg,
		production.WithName("model-breaker"),
	)
	production.RegisterCircuitBreaker(s.circuitBreaker)

	// Wire idempotent cache, audit log, and telemetry.
	s.idempotentCache = production.NewFIFOIdempotentCache(1024)
	production.RegisterIdempotentCache(s.idempotentCache)

	s.telemetry = production.NewDefaultTelemetry()
	production.RegisterTelemetry(s.telemetry)

	if s.rc != nil && s.rc.Production.Audit.Enabled != nil && *s.rc.Production.Audit.Enabled {
		auditPath := s.rc.Production.Audit.Path
		if auditPath == "" {
			if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
				auditPath = filepath.Join(home, ".go-cli", "audit.jsonl")
			}
		}
		if auditPath != "" {
			s.auditLog = production.NewDefaultAuditLog(auditPath)
			production.RegisterAuditLog(s.auditLog)
		}
	}

	// Wrap tool registry with production middleware.
	s.planCtrl = core.NewDefaultPlanModeController()
	pathNormalizer := tools.NewPathNormalizer("")
	schemaValidator := tools.NewSchemaValidator(s.underlyingReg)
	resultMasker := tools.NewResultMasker(nil)
	promptInjectionGuard := production.NewPromptInjectionGuard()
	wrappers := []tools.ToolExecutorWrapper{
		production.NewPromptInjectionToolWrapper(promptInjectionGuard),
		core.NewPlanModeToolWrapper(s.planCtrl),
		tools.WithArgumentPreparation(pathNormalizer, schemaValidator),
		newProductionToolWrapper(s.idempotentCache, s.auditLog, s.telemetry, s.sessionID),
		tools.NewResultMaskingWrapper(resultMasker),
	}
	if s.hookMgr != nil {
		wrappers = append(wrappers, s.hookMgr.PostToolUseWrapper())
	}
	s.tr = tools.NewMiddlewareToolRegistry(s.tr, wrappers...)
	s.reg.RegisterToolRegistry(s.tr)
}

// assembleOutputGuards wires the output guard chain (PII + code injection +
// length + redacting). The redacting guard masks common API key formats so
// that secrets leaking into model output are replaced with [REDACTED] before
// the output propagates downstream.
func (s *assembleState) assembleOutputGuards() {
	redactingGuard := production.NewRedactingOutputGuard()
	production.RegisterAPIKeyRedaction(redactingGuard)

	s.guardChain = production.NewOutputGuardChain([]production.OutputGuard{
		production.NewPIIOutputGuard(),
		production.NewCodeInjectionGuard(),
		production.NewLengthGuard(100000),
		redactingGuard,
	})
}

// assembleSubAgent wires the real SubAgent factory, dispatcher, and ACP
// multi-agent communication.
func (s *assembleState) assembleSubAgent() {
	subAgentFactory := core.NewRealSubAgentFactory(s.model, llm.NewProviderRegistry(), s.tr,
		core.WithModelWrapper(newModelWrapperWithChain(s.pw, s.modelChain, s.circuitBreaker, s.guardChain, s.telemetry)),
	)
	core.RegisterSubAgentFactory(subAgentFactory)
	dispatcher := core.NewDefaultSubagentDispatcher(nil)
	s.dispatcher = dispatcher
	if subErr := s.tr.Register(s.ctx, core.NewSubagentTool(dispatcher)); subErr != nil {
		s.logger.Warn("assemble_subagent_tool_failed", "err", subErr)
	}

	// Wire ACP multi-agent communication (if configured).
	if s.rc != nil && s.rc.ACP.Transport != "" && len(s.rc.ACP.Endpoints) > 0 {
		switch s.rc.ACP.Transport {
		case "stdio":
			s.acpClient = acp.NewStdioAdapter(os.Stdin, os.Stdout)
		case "grpc":
			s.acpClient = acp.NewGRPCAdapter(s.rc.ACP.Endpoints[0])
		default:
			s.logger.Warn("assemble_acp_unknown_transport", "transport", s.rc.ACP.Transport)
		}
		if s.acpClient != nil {
			if connErr := s.acpClient.Connect(s.ctx); connErr != nil {
				s.logger.Warn("assemble_acp_connect_failed", "err", connErr)
				s.acpClient = nil
			} else {
				acpMW := acp.NewACPMiddleware("acp-bridge", s.acpClient)
				s.acpAdapter = acp.NewACPMiddlewareAdapter(acpMW, dispatcher, s.acpClient)
				s.logger.Info("assemble_acp_connected", "transport", s.rc.ACP.Transport)
			}
		}
	}
}

// assembleExtraTools registers remaining tools (todo, task, goal, web, plan_mode, etc.).
func (s *assembleState) assembleExtraTools() {
	todoStore := tools.NewTodoStore()
	taskStore := tools.NewTaskStore()
	goalStore, _ := tools.NewDefaultGoalStore("") //nolint:errcheck
	s.hitlEmitter = &cliHITLEmitter{out: s.out}

	webSearchTool := tools.NewWebSearchTool()
	if s.rc != nil {
		switch s.rc.WebSearch.Provider {
		case "brave":
			if s.rc.WebSearch.APIKey != "" {
				webSearchTool = tools.NewWebSearchTool(tools.WithSearchProvider(
					tools.NewBraveSearchProvider(s.rc.WebSearch.APIKey),
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
		tools.NewWebFetchTool(tools.WithHTMLConverter(s.htmlConverter)),
		webSearchTool,
		core.NewAskUserQuestionTool(s.hitlEmitter, 30*time.Second),
		tools.NewEnterPlanModeTool(s.planCtrl),
		tools.NewExitPlanModeTool(s.planCtrl),
		tools.NewGitPRCreateTool(s.gitCwd),
	}
	for _, t := range extraTools {
		if regErr := s.tr.Register(s.ctx, t); regErr != nil {
			s.logger.Warn("assemble_tool_register_failed", "tool", t.Name(), "err", regErr)
		}
	}
	if searchErr := s.tr.Register(s.ctx, tools.NewToolSearchTool(s.tr)); searchErr != nil {
		s.logger.Warn("assemble_tool_register_failed", "tool", "tool_search", "err", searchErr)
	}
}

// assembleLoopAgent builds the LoopAgent with model wrapper, memory store,
// and system prompt builder.
func (s *assembleState) assembleLoopAgent() {
	s.steerCh = make(chan string, 16)
	s.followUpCh = make(chan string, 16)
	s.runSlotGuard = core.NewDefaultRunSlotGuard()
	loopOpts := []core.LoopOption{
		core.WithLLM(s.model),
		core.WithTools(s.tr),
		core.WithModelWrapper(newModelWrapperWithChain(s.pw, s.modelChain, s.circuitBreaker, s.guardChain, s.telemetry)),
		core.WithExecutionMode(core.ExecutionModeParallel),
		core.WithTracer(s.tracer),
		core.WithSteeringChannel(s.steerCh),
		core.WithFollowUpChannel(s.followUpCh),
		core.WithThinkingConfig(llm.ThinkingConfigForLevel(s.thinkingLevel)),
	}
	if s.rc != nil && s.rc.Agent.MaxIterations != 0 {
		loopOpts = append(loopOpts, core.WithMaxIterations(s.rc.Agent.MaxIterations))
	}
	if s.rc != nil && s.rc.Tools.Parallel != nil && !*s.rc.Tools.Parallel {
		loopOpts = append(loopOpts, core.WithExecutionMode(core.ExecutionModeSequential))
	}

	// Wire memory store for cross-session memory persistence.
	memoryPath := ""
	if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
		memoryPath = filepath.Join(home, ".go-cli", "memories.jsonl")
	}
	if memoryPath != "" {
		if mkErr := os.MkdirAll(filepath.Dir(memoryPath), 0o755); mkErr != nil {
			s.logger.Warn("assemble_memory_mkdir_failed", "err", mkErr)
		}
		ms, msErr := memory.NewFileMemoryStore(memoryPath)
		if msErr != nil {
			s.logger.Warn("assemble_memory_store_failed", "err", msErr)
		} else {
			s.memStore = ms
		}
	}

	var memoryEntries []core.MemoryEntry
	if s.memStore != nil {
		memories, memErr := s.memStore.List(s.ctx)
		if memErr != nil {
			s.logger.Warn("assemble_memory_list_failed", "err", memErr)
		}
		for _, m := range memories {
			memoryEntries = append(memoryEntries, core.MemoryEntry{
				ID:       m.ID,
				Content:  m.Content,
				Category: m.Category,
			})
		}
	}

	// Create memory extractor.
	if s.memStore != nil {
		extractModel := s.model
		if s.smallModel != nil {
			extractModel = s.smallModel
		}
		s.memExtractor = memory.NewLLMMemoryExtractor(extractModel, s.memStore)
	}

	// Wire dynamic system prompt builder with project context.
	s.contextLoader = core.NewDefaultProjectContextLoader()
	s.promptBuilder = core.NewDefaultSystemPromptBuilder()
	cwd, _ := os.Getwd() //nolint:errcheck
	contextFiles, ctxErr := s.contextLoader.Load(s.ctx, cwd)
	if ctxErr != nil {
		s.logger.Warn("assemble_context_load_failed", "err", ctxErr)
	}
	var customPrompt, appendPrompt string
	if s.rc != nil {
		customPrompt = s.rc.Agent.SystemPrompt
		appendPrompt = s.rc.Agent.AppendSystemPrompt
	}
	loopOpts = append(loopOpts,
		core.WithSystemPromptBuilder(s.promptBuilder),
		core.WithSystemPromptOptions(core.SystemPromptOptions{
			Cwd:          cwd,
			ContextFiles: contextFiles,
			Skills:       s.skillInfos,
			Memories:     memoryEntries,
			CustomPrompt: customPrompt,
			AppendPrompt: appendPrompt,
		}),
	)

	s.loop = core.NewLoopAgent(loopOpts...)
	if la, ok := s.loop.(*core.LoopAgent); ok {
		s.loopAgent = la
		s.loopAgent.WithToolSearchThreshold(30)
	}
}

// assembleMiddleware wires the loop detector, reminder manager, failure
// synthesizer, and applies the middleware chain around the loop agent.
func (s *assembleState) assembleMiddleware() {
	ldCfg := production.LoopDetectionConfig{}
	if s.rc != nil {
		ldCfg.EditThreshold = s.rc.Production.LoopDetector.EditThreshold
		ldCfg.TestFailureThreshold = s.rc.Production.LoopDetector.TestFailureThreshold
		ldCfg.SameToolCallThreshold = s.rc.Production.LoopDetector.SameToolCallThreshold
	}
	s.loopDetector = production.NewDefaultLoopDetector(ldCfg)
	production.RegisterLoopDetector(s.loopDetector)
	s.reminderMgr = core.NewDefaultSystemReminderManager()
	s.failureSynthesizer = core.NewDefaultFailureTurnSynthesizer()

	// Register unified ToolInterceptors (PreToolCallEvent is the single
	// interception mechanism). Plan-mode blocking and shell-hook pre-tool-use
	// both go through this path instead of separate middlewares.
	core.RegisterToolInterceptor(core.NewPlanModeToolInterceptor(s.planCtrl))
	if s.hookMgr != nil {
		core.RegisterToolInterceptor(s.hookMgr.PreToolUseInterceptor())
	}

	chain := []core.Middleware{
		core.NewLoggingMiddleware(s.ac.agentName),
		&loopDetectorMiddleware{detector: s.loopDetector, manager: s.reminderMgr},
		core.NewSystemReminderInjector(s.reminderMgr),
		core.NewFailureSynthesisMiddleware(s.failureSynthesizer),
		core.NewHookMiddleware(s.hookChain),
	}
	if s.acpAdapter != nil {
		chain = append(chain, s.acpAdapter)
	}
	chain = append(chain, s.extMiddleware...)
	s.loop = core.NewMiddlewareChain(chain...).Wrap(s.loop)
}

// assembleCompactor creates the compaction components and wires mid-turn
// compaction into the loop agent.
func (s *assembleState) assembleCompactor() error {
	// The estimator is needed both for the quality evaluator and the
	// compactor adapter, so build it before the factory. Use the composite
	// estimator so short texts get precise per-rune weighting while long
	// texts fall back to the O(1) length-based fast path, avoiding the
	// O(n²) per-character scan of UnicodeTokenEstimator on large contexts.
	s.estimator = compaction.NewCompositeTokenEstimator(0)

	qualityEvaluator := compaction.NewDefaultQualityEvaluator(s.estimator)

	strategy := "unified"
	if s.rc != nil && s.rc.Compaction.Strategy != "" {
		strategy = s.rc.Compaction.Strategy
	}

	// When a small model is available, inject its summarizer through the
	// factory rather than bypassing it, so the factory owns compactor
	// construction for every strategy.
	var factoryOpts []compaction.DefaultCompactorFactoryOption
	if s.smallModel != nil && (strategy == "" || strategy == "unified" || strategy == "micro_first") {
		summarizer := &llmModelSummarizer{model: s.smallModel}
		factoryOpts = append(factoryOpts, compaction.WithFactorySummarizer(summarizer))
		s.logger.Info("assemble_compaction_small_model_wired")
	}
	compactorFactory := compaction.NewDefaultCompactorFactory(factoryOpts...)

	compactor, err := compactorFactory.Create(strategy)
	if err != nil {
		return fmt.Errorf("assemble: create compactor: %w", err)
	}

	// Inject the quality evaluator into the UnifiedCompactor. The option is a
	// closure over *UnifiedCompactor, so it can be applied to an existing
	// instance. For non-unified strategies the assertion fails and evaluation
	// is skipped, which is the desired behavior.
	if uc, ok := compactor.(*compaction.UnifiedCompactor); ok {
		compaction.WithQualityEvaluator(qualityEvaluator)(uc)
	}

	s.compactor = compactor
	s.midTurn = compaction.NewMidTurnCompact()
	s.reg.RegisterCompactor(&compactorAdapter{inner: s.compactor, estimator: s.estimator})
	s.reg.RegisterTokenEstimator(&tokenEstimatorAdapter{inner: s.estimator})

	if s.loopAgent != nil {
		s.loopAgent.SetMidTurnCompaction(s.midTurn, s.compactor, s.estimator, s.ac.maxTokens)
	}
	return nil
}

// assembleSession wires session persistence (if enabled).
func (s *assembleState) assembleSession() {
	if s.ac.enableSession && s.rc != nil && s.rc.Session.StorePath != "" {
		s.sessionStore = session.NewJSONLSessionStore(s.rc.Session.StorePath)
		if openErr := s.sessionStore.Open(s.ctx); openErr != nil {
			s.logger.Warn("assemble_session_open_failed", "err", openErr)
			s.sessionStore = nil
		}
	}
}

// buildModel resolves an llm.BaseChatModel from the loaded configuration.
// When the configuration supplies a BaseURL or APIKey, a custom provider is
// built with those settings; otherwise the default provider registry is used.
// It also returns the ModelInfo for the resolved model so callers can inspect
// the context window and other metadata.
func buildModel(ctx context.Context, rc *config.Config, providerName, modelName string) (llm.BaseChatModel, llm.ModelInfo, func(), error) {
	cfg := llm.ModelConfig{Model: modelName}

	if rc != nil && (rc.Provider.BaseURL != "" || rc.Provider.APIKey != "") {
		provider := llm.NewEinoProvider(
			llm.WithProviderName(providerName),
			llm.WithBaseURL(rc.Provider.BaseURL),
			llm.WithAPIKey(rc.Provider.APIKey),
			llm.WithDefaultModel(modelName),
		)
		m, cleanup, err := provider.Build(ctx, cfg)
		if err != nil {
			return nil, llm.ModelInfo{}, cleanup, err
		}
		return m, findModelInfo(provider.Models(), modelName), cleanup, nil
	}

	reg := llm.NewProviderRegistry()
	provider, err := reg.Get(providerName)
	if err != nil {
		return nil, llm.ModelInfo{}, nil, err
	}
	m, cleanup, err := provider.Build(ctx, cfg)
	if err != nil {
		return nil, llm.ModelInfo{}, cleanup, err
	}
	return m, findModelInfo(provider.Models(), modelName), cleanup, nil
}

// listAvailableModels returns the models exposed by the provider identified by
// providerName. It mirrors the provider-resolution logic in buildModel: when
// the config supplies a BaseURL or APIKey, a custom EinoProvider is built;
// otherwise the default provider registry is used.
func listAvailableModels(rc *config.Config, providerName string) []llm.ModelInfo {
	if rc != nil && (rc.Provider.BaseURL != "" || rc.Provider.APIKey != "") {
		provider := llm.NewEinoProvider(
			llm.WithProviderName(providerName),
			llm.WithBaseURL(rc.Provider.BaseURL),
			llm.WithAPIKey(rc.Provider.APIKey),
		)
		return provider.Models()
	}
	reg := llm.NewProviderRegistry()
	provider, err := reg.Get(providerName)
	if err != nil {
		return nil
	}
	return provider.Models()
}

// buildSmallModel builds a lightweight model from the SmallModelConfig section.
// It uses the small model's own provider/BaseURL/APIKey when provided, falling
// back to the primary provider settings.
func buildSmallModel(ctx context.Context, rc *config.Config, providerName, modelName string) (llm.BaseChatModel, llm.ModelInfo, func(), error) {
	if rc == nil {
		return nil, llm.ModelInfo{}, nil, fmt.Errorf("assemble: buildSmallModel: config is nil")
	}
	cfg := llm.ModelConfig{
		Model:       modelName,
		Temperature: rc.SmallModel.Temperature,
		MaxTokens:   rc.SmallModel.MaxTokens,
	}

	baseURL := rc.SmallModel.BaseURL
	apiKey := rc.SmallModel.APIKey
	if baseURL == "" {
		baseURL = rc.Provider.BaseURL
	}
	if apiKey == "" {
		apiKey = rc.Provider.APIKey
	}

	if baseURL != "" || apiKey != "" {
		provider := llm.NewEinoProvider(
			llm.WithProviderName(providerName),
			llm.WithBaseURL(baseURL),
			llm.WithAPIKey(apiKey),
			llm.WithDefaultModel(modelName),
		)
		m, cleanup, err := provider.Build(ctx, cfg)
		if err != nil {
			return nil, llm.ModelInfo{}, cleanup, err
		}
		return m, findModelInfo(provider.Models(), modelName), cleanup, nil
	}

	reg := llm.NewProviderRegistry()
	provider, err := reg.Get(providerName)
	if err != nil {
		return nil, llm.ModelInfo{}, nil, err
	}
	m, cleanup, err := provider.Build(ctx, cfg)
	if err != nil {
		return nil, llm.ModelInfo{}, cleanup, err
	}
	return m, findModelInfo(provider.Models(), modelName), cleanup, nil
}

// llmModelSummarizer adapts an llm.BaseChatModel to the compaction.Summarizer
// interface. It lives in the cli package to avoid an import cycle between
// compaction and llm.
type llmModelSummarizer struct {
	model llm.BaseChatModel
}

// Compile-time assertion that llmModelSummarizer satisfies compaction.Summarizer.
var _ compaction.Summarizer = (*llmModelSummarizer)(nil)

// Summarize sends the conversation text as a single user message and returns
// the model's response as the summary.
func (s *llmModelSummarizer) Summarize(ctx context.Context, conversation string) (string, error) {
	ctx = llm.WithTaskType(ctx, llm.TaskTypeSummary)
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: conversation},
	}
	resp, err := s.model.Generate(ctx, msgs)
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	return resp.Content, nil
}

// findModelInfo searches a slice of ModelInfo for a model whose Name matches
// the given name. It returns a zero ModelInfo when no match is found.
func findModelInfo(models []llm.ModelInfo, name string) llm.ModelInfo {
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	return llm.ModelInfo{}
}

// hotReloadersMu guards activeHotReloaders.
var hotReloadersMu sync.Mutex

// activeHotReloaders holds all running MCP HotReloaders so they can be
// stopped on session cleanup and manually triggered by /mcp reload.
var activeHotReloaders []mcp.HotReloader

// addHotReloader appends a running HotReloader to the package-level list.
func addHotReloader(hr mcp.HotReloader) {
	hotReloadersMu.Lock()
	defer hotReloadersMu.Unlock()
	activeHotReloaders = append(activeHotReloaders, hr)
}

// StopHotReloaders stops all active MCP HotReloaders. It is safe to call from
// session cleanup and is idempotent.
func StopHotReloaders() {
	hotReloadersMu.Lock()
	reloaders := activeHotReloaders
	activeHotReloaders = nil
	hotReloadersMu.Unlock()

	for _, hr := range reloaders {
		if err := hr.Stop(); err != nil {
			slog.Warn("assemble_mcp_hot_reload_stop_failed", "reloader", hr.Name(), "err", err)
		}
	}
}

// ReloadAllHotReloaders manually triggers Reload on every active HotReloader.
// It returns the number of reloaders that were triggered.
func ReloadAllHotReloaders(ctx context.Context) int {
	hotReloadersMu.Lock()
	reloaders := make([]mcp.HotReloader, len(activeHotReloaders))
	copy(reloaders, activeHotReloaders)
	hotReloadersMu.Unlock()

	for _, hr := range reloaders {
		if err := hr.Reload(ctx); err != nil {
			slog.Warn("assemble_mcp_hot_reload_manual_failed", "reloader", hr.Name(), "err", err)
		}
	}
	return len(reloaders)
}

// registerMCPTools connects to configured MCP servers and registers their
// tools into the tool registry. When no MCP servers are configured in the
// main config file, it auto-loads from .go-cli/mcp.json or
// ~/.config/go-cli/mcp.json if either exists. Servers are connected in
// parallel using errgroup to reduce startup latency.
//
// When servers are auto-discovered from a config file, a HotReloader is
// created for each connected server so that editing the config file
// automatically triggers a reconnect and tool re-registration.
func registerMCPTools(ctx context.Context, rc *config.Config, tr tools.DeferredRegistry) error {
	servers, configPath := loadMCPServersWithConfigPath(ctx, rc)
	if len(servers) == 0 {
		return nil
	}

	// connected collects successfully connected clients so HotReloaders can be
	// created after the errgroup completes.
	var (
		connectedMu sync.Mutex
		connected   []mcp.MCPClient
	)

	g, gctx := errgroup.WithContext(ctx)
	for _, srv := range servers {
		srv := srv
		g.Go(func() error {
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
				return nil
			}

			var client mcp.MCPClient
			if cfg.Transport == mcp.MCPTransportSSE {
				client = mcp.NewHTTPClientAdapter(cfg)
			} else {
				client = mcp.NewOfficialSDKAdapter(cfg)
			}

			if err := client.Connect(gctx); err != nil {
				slog.Warn("assemble_mcp_connect_failed", "server", srv.Name, "err", err)
				return nil
			}

			mcpTools, err := client.ListTools(gctx)
			if err != nil {
				slog.Warn("assemble_mcp_list_failed", "server", srv.Name, "err", err)
				return nil
			}

			registerMCPToolBatch(gctx, tr, client, mcpTools)
			slog.Info("assemble_mcp_registered", "server", srv.Name, "tools", len(mcpTools))

			connectedMu.Lock()
			connected = append(connected, client)
			connectedMu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// Create a HotReloader for each connected server when a config file path
	// was discovered. The reloader watches the file and, on change, reconnects
	// the client and re-registers its tools.
	if configPath != "" {
		for _, client := range connected {
			createAndStartHotReloader(ctx, configPath, client, tr)
		}
	}

	return nil
}

// registerMCPToolBatch registers a batch of MCP tools from a single server
// into the deferred registry. It is shared by the initial connection path and
// the HotReloader re-registration callback.
func registerMCPToolBatch(ctx context.Context, tr tools.DeferredRegistry, client mcp.MCPClient, mcpTools []mcp.MCPTool) {
	for _, t := range mcpTools {
		toolName := mcp.NormalizeToolName(client.Name(), t.Name)
		clientRef := client
		toolRef := t
		if regErr := tr.RegisterDeferred(ctx, toolName, func() (tools.ToolDefinition, error) {
			return mcp.NewMCPToolAdapter(clientRef, toolRef), nil
		}); regErr != nil {
			slog.Warn("assemble_mcp_register_failed", "tool", t.Name, "err", regErr)
		}
	}
}

// createAndStartHotReloader builds a HotReloader for a single MCP client,
// starts watching the config file, and registers it for shutdown.
func createAndStartHotReloader(ctx context.Context, configPath string, client mcp.MCPClient, tr tools.DeferredRegistry) {
	clientRef := client
	registerFn := mcp.RegisterToolsFunc(func(mcpTools []mcp.MCPTool) {
		registerMCPToolBatch(context.Background(), tr, clientRef, mcpTools)
		slog.Info("assemble_mcp_hot_reload_reregistered", "server", clientRef.Name(), "tools", len(mcpTools))
	})
	hr := mcp.NewDefaultHotReloader(client, registerFn)
	if watchErr := hr.Watch(ctx, configPath); watchErr != nil {
		slog.Warn("assemble_mcp_hot_reload_watch_failed", "server", client.Name(), "err", watchErr)
		return
	}
	addHotReloader(hr)
	slog.Info("assemble_mcp_hot_reload_started", "server", client.Name(), "config_path", configPath)
}

// loadMCPServers returns MCP server configs from the main config, or
// auto-discovered from default paths when the main config has none. It is a
// thin wrapper around loadMCPServersWithConfigPath for callers that do not
// need the discovered config file path.
func loadMCPServers(ctx context.Context, rc *config.Config) []config.MCPServerConfig {
	servers, _ := loadMCPServersWithConfigPath(ctx, rc)
	return servers
}

// loadMCPServersWithConfigPath is like loadMCPServers but also returns the
// config file path that was auto-discovered (e.g. ".go-cli/mcp.json"). The
// path is empty when servers come from the main config or when no config file
// was found. The path is used by the HotReloader to watch for changes.
func loadMCPServersWithConfigPath(ctx context.Context, rc *config.Config) ([]config.MCPServerConfig, string) {
	if rc != nil && len(rc.MCP.Servers) > 0 {
		return rc.MCP.Servers, ""
	}

	// Auto-discover MCP config from default paths.
	candidates := []string{".go-cli/mcp.json"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "go-cli", "mcp.json"))
	}

	cwd, _ := os.Getwd()
	tm := approval.GetTrustManager()

	for _, path := range candidates {
		isProjectLevel := path == ".go-cli/mcp.json"
		if isProjectLevel {
			if !tm.IsTrusted(ctx, cwd) {
				slog.Warn("assemble_mcp_config_skip_untrusted", "path", path, "reason", "project not trusted")
				continue
			}
		} else {
			if err := approval.ValidateGlobalConfigFile(path); err != nil {
				slog.Warn("assemble_mcp_config_global_rejected", "path", path, "err", err)
				continue
			}
		}

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
			return result, path
		}
	}
	return nil, ""
}

// registerSkillTools loads skills from the configured directory (or default
// discovery paths) and registers them as tools. It supports two directory
// layouts:
//   - Flat: {dir}/{name}.md
//   - Nested: {dir}/{name}/SKILL.md
//
// It returns a slice of SkillInfo describing each registered skill, suitable
// for injection into the system prompt.
func registerSkillTools(ctx context.Context, rc *config.Config, tr tools.DeferredRegistry) []core.SkillInfo {
	skillDir := ""
	autoDiscovered := false
	if rc != nil && rc.Skill.Dir != "" {
		skillDir = rc.Skill.Dir
	} else {
		// Auto-discover default skill directories.
		autoDiscovered = true
		skillDir = discoverSkillDir()
	}
	if skillDir == "" {
		return nil
	}

	// Trust check: skip auto-discovered project-level skill directory if
	// project is not trusted.
	if autoDiscovered && !filepath.IsAbs(skillDir) {
		cwd, _ := os.Getwd()
		if !approval.GetTrustManager().IsTrusted(ctx, cwd) {
			slog.Warn("assemble_skill_skip_untrusted", "dir", skillDir, "reason", "project not trusted")
			return nil
		}
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
		defCopy := def
		if regErr := tr.RegisterDeferred(ctx, (*defCopy).Name(), func() (tools.ToolDefinition, error) {
			return skill.NewSkillAdapter(*defCopy), nil
		}); regErr != nil {
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

	// Trust check: skip custom tools if project is not trusted. Custom tools
	// from project config are already gated by LoadTrusted, but this provides
	// defense-in-depth for code paths that bypass LoadTrusted.
	cwd, _ := os.Getwd()
	if !approval.GetTrustManager().IsTrusted(ctx, cwd) {
		logger.Warn("assemble_custom_tools_skip_untrusted", "reason", "project not trusted")
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

// noCacheTools lists tools that must never be served from the idempotent
// cache. These are mutation tools whose every invocation must pass through
// the approval middleware; caching their results would let a duplicate call
// bypass user approval.
var noCacheTools = map[string]bool{
	"write": true,
	"edit":  true,
	"bash":  true,
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

			// Mutation tools bypass the cache so they always reach the
			// approval middleware. Caching their results would let a
			// duplicate call skip user approval.
			skipCache := noCacheTools[call.Name]

			// Check idempotent cache (skipped for mutation tools).
			if !skipCache && cache != nil {
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

			if !skipCache && err == nil && result != nil && cache != nil {
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

	// Multiple servers: use MultiLSPClient, starting each server in parallel.
	multi := tools.NewMultiLSPClient()
	var anyStarted atomic.Bool
	g, gctx := errgroup.WithContext(ctx)
	for _, srv := range servers {
		srv := srv
		g.Go(func() error {
			client, started := buildSingleLSPClient(gctx, srv, logger)
			if !started {
				return nil
			}
			anyStarted.Store(true)
			if len(srv.FileExtensions) > 0 {
				multi.Register(client, srv.FileExtensions...)
			} else {
				multi.SetDefaultClient(client)
			}
			return nil
		})
	}
	_ = g.Wait()
	if !anyStarted.Load() {
		return nil, false
	}
	return multi, true
}

// resolveLSPWorkspaceRoot determines the workspace root to use for LSP
// completion. It prefers the legacy single-server WorkspaceRoot, then the
// first multi-server entry, and finally falls back to the current working
// directory.
func resolveLSPWorkspaceRoot(rc *config.Config) string {
	if rc != nil {
		if rc.LSP.WorkspaceRoot != "" {
			return rc.LSP.WorkspaceRoot
		}
		for _, srv := range rc.LSP.Servers {
			if srv.WorkspaceRoot != "" {
				return srv.WorkspaceRoot
			}
		}
	}
	wd, _ := os.Getwd() //nolint:errcheck
	return wd
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

// buildDiffPreviewFn returns a DiffPreviewFunc that generates a unified diff
// preview for edit and write tool calls using the shared DiffGenerator. For
// other tools it returns "". The function reads the current file content from
// disk to compute the before/after diff.
func buildDiffPreviewFn(diffGen tools.DiffGenerator) approval.DiffPreviewFunc {
	return func(ctx context.Context, toolName string, args map[string]any) string {
		if diffGen == nil {
			return ""
		}
		switch toolName {
		case "write":
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			if path == "" {
				return ""
			}
			old, err := os.ReadFile(path)
			if err != nil {
				// New file: show all lines as additions.
				old = nil
			}
			diff, err := diffGen.Generate(ctx, string(old), content, path)
			if err != nil || diff == "" {
				return ""
			}
			return diff
		case "edit":
			path, _ := args["file_path"].(string)
			oldStr, _ := args["old_string"].(string)
			newStr, _ := args["new_string"].(string)
			if path == "" {
				return ""
			}
			old, err := os.ReadFile(path)
			if err != nil {
				return ""
			}
			newContent := strings.Replace(string(old), oldStr, newStr, 1)
			diff, err := diffGen.Generate(ctx, string(old), newContent, path)
			if err != nil || diff == "" {
				return ""
			}
			return diff
		}
		return ""
	}
}
