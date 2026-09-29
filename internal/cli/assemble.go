//exempt:scan012
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
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

// ErrPreparedPostToolUseHookUnsupported identifies shell hooks that cannot
// safely run in the prepared tool-dispatch pipeline yet.
var ErrPreparedPostToolUseHookUnsupported = errors.New("cli: PostToolUse shell hooks are unsupported by prepared execution")

// UnsupportedPostToolUseHookError identifies the configured hook that cannot
// run in the prepared execution pipeline. It deliberately omits the command
// contents so user-facing errors can identify the configuration location
// without disclosing executable text.
type UnsupportedPostToolUseHookError struct {
	Path  string
	Index int
}

func (e *UnsupportedPostToolUseHookError) Error() string {
	return fmt.Sprintf("%v: %s hook %d", ErrPreparedPostToolUseHookUnsupported, e.Path, e.Index)
}

func (e *UnsupportedPostToolUseHookError) Unwrap() error {
	return ErrPreparedPostToolUseHookUnsupported
}

// cliDefaultOpenAICompatibleBaseURL mirrors EinoProvider's default so small
// model credential reuse can compare origins when provider.base_url is empty.
const cliDefaultOpenAICompatibleBaseURL = "https://api.openai.com/v1"

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
	// PhysicalSessionID is set when this Assembly uses the prepared durable CLI
	// session protocol. SessionID remains the configured logical label.
	PhysicalSessionID string
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

	closeOwner     *assemblyCloseOwner
	mcpResources   *assemblyMCPResources
	durableSession *durableCLISession
}

// Close stops admitting new runs and waits for the Assembly-owned resources to
// finish. The caller context bounds only this wait; a timed-out close owner
// retains the active resources and continues its serial shutdown.
func (a *AgentAssembly) Close(ctx context.Context) error {
	if a == nil || a.closeOwner == nil {
		return nil
	}
	return a.closeOwner.Close(ctx)
}

func (a *AgentAssembly) setInteractiveCloseHooks(
	stop func(),
	beforeSessionClose func(context.Context) error,
	afterMemoryJoin func(),
) error {
	if a == nil || a.closeOwner == nil {
		return ErrAssemblyRunAdmissionUnavailable
	}
	return a.closeOwner.setInteractiveCloseHooks(stop, beforeSessionClose, afterMemoryJoin)
}

func (a *AgentAssembly) setHTTPServer(server *acp.HTTPServer) error {
	if a == nil || a.closeOwner == nil {
		return nil
	}
	return a.closeOwner.setHTTPServer(server)
}

// AssembleOption configures AssembleAgent behavior.
type AssembleOption func(*assembleConfig)

type assembleConfig struct {
	maxTokens      int
	resumeFlag     bool
	enableSession  bool
	durableSession *durableCLISession
	agentName      string
	thinkingLevel  *llm.ThinkingLevel
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

func withDurableCLISession(durableSession *durableCLISession) AssembleOption {
	return func(c *assembleConfig) {
		c.durableSession = durableSession
		c.enableSession = durableSession != nil || c.enableSession
	}
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
	if s.durableSession != nil {
		snapshot, err := s.durableSession.snapshotSelected()
		if err != nil {
			return nil, errors.Join(fmt.Errorf("assemble: inspect selected durable session: %w", err), s.runCleanup())
		}
		s.initialSnapshot = snapshot.Clone()
	}
	s.loadConfiguredShellHooks()
	if len(s.unsupportedPostToolUseHooks) > 0 {
		hook := s.unsupportedPostToolUseHooks[0]
		return nil, errors.Join(&UnsupportedPostToolUseHookError{Path: hook.Path, Index: hook.Index}, s.runCleanup())
	}

	// Assemble subsystems in dependency order.
	if err := s.assembleModel(); err != nil {
		return nil, errors.Join(err, s.runCleanup())
	}
	s.assembleTracing()
	if err := s.assembleTools(); err != nil {
		return nil, errors.Join(err, s.runCleanup())
	}
	if err := s.assembleExtensions(); err != nil {
		return nil, errors.Join(err, s.runCleanup())
	}
	if err := s.assembleApproval(); err != nil {
		return nil, errors.Join(err, s.runCleanup())
	}
	if err := s.assembleProductionResilience(); err != nil {
		return nil, errors.Join(err, s.runCleanup())
	}
	s.assembleOutputGuards()
	s.assembleSubAgent()
	if err := s.assembleExtraTools(); err != nil {
		return nil, errors.Join(err, s.runCleanup())
	}
	s.assembleLoopAgent()
	s.assembleMiddleware()
	if err := s.assembleCompactor(); err != nil {
		return nil, errors.Join(err, s.runCleanup())
	}
	s.assembleSession()

	// Resume history from session store if requested.
	var restoredHistory []core.AgentMessage
	if s.durableSession != nil {
		restoredHistory = append([]core.AgentMessage(nil), s.initialSnapshot.PublishedView.Messages...)
	} else if s.ac.resumeFlag && s.sessionStore != nil {
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
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(256), core.WithHarnessTracer(s.tracer), core.WithRunSlotGuard(s.runSlotGuard), core.WithHarnessEventBus(s.eventBus), core.WithRunLifetimeContext(s.lifetimeCtx))

	// Build TurnRunner wired with the shared steering channel and agent.
	turnRunner := core.NewEinoTurnRunner(s.loop)
	turnRunner.SetSteerChannel(s.steerCh)
	turnRunner.SetFollowUpChannel(s.followUpCh)
	turnRunner.SetAgent(agent)
	turnRunner.SetHookChain(s.hookChain)
	turnRunner.SetRunSlotGuard(s.runSlotGuard)
	turnRunner.SetRunLifetimeContext(s.lifetimeCtx)
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
			SessionStore:      s.sessionStore,
			SessionID:         s.sessionID,
			PhysicalSessionID: s.initialSnapshot.SessionID,
			MemoryStore:       s.memStore,
			MemoryExtractor:   s.memExtractor,
			MemoryWG:          &s.memWG,
			Compactor:         s.compactor,
			Estimator:         s.estimator,
			MidTurn:           s.midTurn,
			MaxTokens:         s.ac.maxTokens,
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
		Registry:       s.reg,
		Cleanup:        func() { _ = s.runCleanup() },
		closeOwner:     s.closeOwner,
		mcpResources:   s.mcpResources,
		durableSession: s.durableSession,
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
	if ctx == nil {
		ctx = context.Background()
	}
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
	gitCwd := resolveGitCwd(rc)

	// Resolve session ID: config.Session.ID takes priority, fallback to agentName.
	sessionID := ac.agentName
	if rc != nil && rc.Session.ID != "" {
		sessionID = rc.Session.ID
	}

	lifetimeCtx, lifetimeCancel := context.WithCancel(ctx)
	s := &assembleState{
		ctx:            lifetimeCtx,
		rc:             rc,
		ac:             ac,
		logger:         logger,
		reg:            reg,
		sessionID:      sessionID,
		providerName:   providerName,
		modelName:      modelName,
		out:            out,
		thinkingLevel:  thinkingLevel,
		eventBus:       core.NewMemoryEventBus(),
		gitCwd:         gitCwd,
		lifetimeCtx:    lifetimeCtx,
		lifetimeCancel: lifetimeCancel,
		mcpResources:   &assemblyMCPResources{},
		durableSession: ac.durableSession,
	}
	s.closeOwner = newAssemblyCloseOwner(
		func() core.RunSlotGuard { return s.runSlotGuard },
		s.lifetimeCancel,
		s.requestStopSources,
		s.closeResources,
	)
	s.closeOwner.mcpResources = s.mcpResources
	return s, thinkingLevel
}

func resolveGitCwd(rc *config.Config) string {
	gitCwd, err := os.Getwd()
	if err != nil {
		gitCwd = "."
	}
	if rc != nil && rc.Git.WorkDir != "" {
		gitCwd = rc.Git.WorkDir
	}
	return gitCwd
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
	modelResolver *cliModelResolver
	modelCycler   *llm.ModelCycler
	modelRegistry llm.ModelRegistry
	smallModel    llm.BaseChatModel
	modelSelector *llm.DefaultModelSelector

	// Tracing section
	tracer *tracing.Tracer

	// Tools section
	tr               tools.ToolRegistry
	underlyingReg    tools.ToolRegistry
	fileTracker      *tools.FileTracker
	diffGen          tools.DiffGenerator
	mutationQueue    tools.FileMutationQueue
	gitTool          tools.GitTool
	gitCwd           string
	worktreeMgr      *tools.WorktreeManager
	snapshotMgr      *tools.SnapshotManager
	htmlConverter    *tools.DefaultHTMLConverter
	lspClientField   tools.LSPClient
	lspWorkspaceRoot string
	skillInfos       []core.SkillInfo

	// Extensions section
	extHooks                    []core.Hook
	extMiddleware               []core.Middleware
	hookChain                   *core.HookChain
	hookMgr                     *core.HookManager
	shellHooksLoaded            bool
	shellHooks                  []core.HookSystem
	unsupportedPostToolUseHooks []UnsupportedPostToolUseHookError

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
	sessionStore    *session.JSONLSessionStore
	durableSession  *durableCLISession
	initialSnapshot session.PreparedSessionSnapshot

	// Cleanup
	cleanupList []func(context.Context) error

	lifetimeCtx    context.Context
	lifetimeCancel context.CancelFunc
	closeOwner     *assemblyCloseOwner
	mcpResources   *assemblyMCPResources
	pluginManagers []*extension.PluginManager

	primaryModelCleanup func()
	modelDisposer       func() error
	smallModelCleanup   func()
	worktreeStopSignal  func()
	acpConnectErr       error
}

// runCleanup starts or joins the same close owner used by a returned Assembly.
func (s *assembleState) runCleanup() error {
	if s.closeOwner == nil {
		return nil
	}
	return s.closeOwner.Close(context.Background())
}

// assembleModel builds the primary model, ModelCycler, ModelRegistry, and small
// model. It registers the model provider and appends model cleanups.
func (s *assembleState) assembleModel() error {
	modelResolver, err := newCLIModelResolver(s.rc, s.providerName, true, s.modelName)
	if err != nil {
		return fmt.Errorf("assemble: resolve model providers: %w", err)
	}
	s.modelResolver = modelResolver
	model, modelInfo, modelCleanup, err := modelResolver.buildPrimary(s.ctx, s.rc, s.providerName, s.modelName)
	if err != nil {
		return fmt.Errorf("assemble: build model: %w", err)
	}
	s.reg.RegisterModelProvider(&chatModelProvider{name: s.modelName, model: model})
	s.model = model
	s.modelInfo = modelInfo
	if modelCleanup != nil {
		s.primaryModelCleanup = modelCleanup
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
		sm, _, smallCleanup, smErr := modelResolver.buildSmall(s.ctx, s.rc, s.providerName, smallProvider, s.rc.SmallModel.Model)
		if smErr != nil {
			s.logger.Warn("assemble_small_model_failed", "err", smErr, "model", s.rc.SmallModel.Model)
		} else {
			s.smallModel = sm
			if smallCleanup != nil {
				s.smallModelCleanup = smallCleanup
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
		WithPrimaryCleanup(s.primaryModelCleanup).
		WithModelNames(s.providerName, s.modelName, smallProvider, smallModelName).
		WithModelBuilder(func(ctx context.Context, name string) (llm.BaseChatModel, func(), error) {
			m, _, cleanup, err := modelResolver.buildPrimary(ctx, s.rc, s.providerName, name)
			return m, cleanup, err
		}).
		WithModelSpecBuilder(func(ctx context.Context, candidate llm.ModelSpec) (llm.BaseChatModel, func(), error) {
			m, _, cleanup, err := modelResolver.buildInteractive(ctx, s.rc, candidate)
			return m, cleanup, err
		}).
		WithModelLister(func() []llm.ModelInfo {
			return modelResolver.listAvailableModels(context.Background(), s.rc, s.providerName)
		}).
		WithModelSpecLister(modelResolver.listModelSpecs).
		WithModelSwitchCallback(func(m llm.BaseChatModel) {
			if s.loopAgent != nil {
				s.loopAgent.SetModel(m)
			}
		})

	return nil
}

// assembleTracing reuses the tracer owned by the outer invocation context.
func (s *assembleState) assembleTracing() {
	s.tracer = tracing.TracerFromContext(s.ctx)
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
	gitCwd := s.gitCwd
	if gitCwd == "" {
		gitCwd = resolveGitCwd(s.rc)
		s.gitCwd = gitCwd
	}
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
		wtSignalDone := make(chan struct{})
		go func() {
			defer close(wtSignalDone)
			select {
			case <-wtSigCh:
				go func() {
					if err := s.runCleanup(); err != nil {
						s.logger.Warn("assemble_worktree_cleanup_failed", "err", err)
					}
				}()
			case <-wtDone:
			}
		}()
		var stopWorktreeSignal sync.Once
		s.worktreeStopSignal = func() {
			stopWorktreeSignal.Do(func() {
				signal.Stop(wtSigCh)
				close(wtDone)
				<-wtSignalDone
			})
		}
		s.cleanupList = append(s.cleanupList, func(ctx context.Context) error {
			return wtMgr.Cleanup(ctx)
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
	if s.rc != nil && len(s.rc.Sandbox.AllowedPaths) > 0 {
		regOpts = append(regOpts, tools.WithRegisteredPathWhitelist(s.rc.Sandbox.AllowedPaths))
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
	if mcpErr := registerMCPToolsOwned(s.ctx, s.rc, dtr, s.mcpResources); mcpErr != nil {
		s.logger.Warn("assemble_mcp_failed", "err", mcpErr)
	}

	// Register LSP tool (if configured).
	if s.rc != nil && (len(s.rc.LSP.ServerCommand) > 0 || len(s.rc.LSP.Servers) > 0) {
		lspClient, lspStarted := buildLSPClient(s.ctx, s.rc, s.logger)
		if lspClient != nil && lspStarted {
			client := lspClient
			s.cleanupList = append(s.cleanupList, func(ctx context.Context) error {
				return client.Shutdown(ctx)
			})
			if regErr := s.tr.Register(s.ctx, tools.NewLSPTool(lspClient)); regErr != nil {
				s.logger.Warn("assemble_lsp_register_failed", "err", regErr)
			} else {
				s.lspClientField = lspClient
				s.lspWorkspaceRoot = resolveLSPWorkspaceRoot(s.rc)
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
func (s *assembleState) assembleExtensions() error {
	if s.rc != nil && s.rc.Extensions.Enabled != nil && *s.rc.Extensions.Enabled && len(s.rc.Extensions.PluginPaths) > 0 {
		pm := extension.NewPluginManager(extension.NewDefaultPluginLoader())
		s.pluginManagers = append(s.pluginManagers, pm)
		if err := pm.Load(s.ctx, s.rc.Extensions.PluginPaths); err != nil {
			return fmt.Errorf("assemble: load extensions: %w", err)
		}
		if err := pm.Init(s.ctx); err != nil {
			return fmt.Errorf("assemble: initialize extensions: %w", err)
		}
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
	}

	s.loadConfiguredShellHooks()
	if len(s.shellHooks) > 0 {
		s.hookMgr = core.NewHookManager(s.shellHooks...)
		s.extHooks = append(s.extHooks, s.hookMgr)
	}

	s.hookChain = core.NewHookChain(s.extHooks...)
	return nil
}

func (s *assembleState) loadConfiguredShellHooks() {
	if s.shellHooksLoaded {
		return
	}
	s.shellHooksLoaded = true
	hooksPath := filepath.Join(s.gitCwd, ".go-cli", "hooks.yaml")
	if _, statErr := os.Stat(hooksPath); statErr == nil {
		s.loadShellHooks(hooksPath)
	}
}

// loadShellHooks reads a hooks.yaml file, parses shell hook definitions, and
// stores shell hooks for later assembly into the HookManager.
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
	for i, hc := range hooksCfg.Hooks {
		event, eventErr := parseHookEvent(hc.Event)
		if eventErr != nil {
			s.logger.Warn("assemble_hook_event_unknown", "index", i, "event", hc.Event, "err", eventErr)
			continue
		}
		if event == core.EventPostToolUse {
			s.unsupportedPostToolUseHooks = append(s.unsupportedPostToolUseHooks, UnsupportedPostToolUseHookError{Path: path, Index: i})
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
		s.shellHooks = append(s.shellHooks, core.NewShellHook(name, event, hc.Command, timeout))
	}
	s.logger.Info("assemble_hooks_ready", "path", path, "count", len(s.shellHooks))
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

// assembleApproval installs the approval policy and mutation queue on the
// registry's prepared-dispatch pipeline.
func (s *assembleState) assembleApproval() error {
	fence, err := tools.DispatchFenceFromRegistry(s.tr)
	if err != nil {
		return fmt.Errorf("assemble: get prepared-dispatch fence: %w", err)
	}
	s.planCtrl = core.NewDefaultPlanModeController(fence)

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
	mwOpts = append(mwOpts,
		approval.WithDispatchFence(fence),
		approval.WithPlanModeController(s.planCtrl),
	)
	approvalMW := approval.NewApprovalMiddleware(classifier, approvalStore, mwOpts...)
	s.reg.RegisterApprovalClassifier(&approvalClassifierAdapter{inner: classifier})
	s.reg.RegisterApprovalStore(&approvalStoreAdapter{inner: approvalStore})
	s.mutationQueue = tools.NewDefaultFileMutationQueue(
		tools.WithMutationFileTracker(s.fileTracker),
		tools.WithMutationDiffGenerator(s.diffGen),
	)
	mq := s.mutationQueue
	s.cleanupList = append(s.cleanupList, func(context.Context) error {
		if cq, ok := mq.(*tools.DefaultFileMutationQueue); ok {
			return cq.Close()
		}
		return nil
	})
	preparedRegistry := tools.NewMiddlewareToolRegistry(s.tr)
	if err := preparedRegistry.SetPreparedToolPolicy(approvalMW.PreparedPolicy); err != nil {
		return fmt.Errorf("assemble: install prepared approval policy: %w", err)
	}
	s.tr = preparedRegistry
	s.reg.RegisterToolRegistry(s.tr)
	return nil
}

// assembleProductionResilience wires retry, cost tracking, circuit breaker,
// idempotent cache, audit log, and telemetry. It installs only the explicit
// prepared execution stages on the registry assembled by assembleApproval.
func (s *assembleState) assembleProductionResilience() error {
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
	if s.planCtrl == nil || s.mutationQueue == nil {
		return errors.New("assemble: prepared approval stages are unavailable")
	}
	pathNormalizer := tools.NewPathNormalizer("")
	resultMasker := tools.NewResultMasker(nil)
	promptInjectionGuard := production.NewPromptInjectionGuard()
	wrappers := []tools.ToolExecutorWrapper{
		production.NewPromptInjectionToolWrapper(promptInjectionGuard),
		newProductionToolWrapper(nil, s.auditLog, s.telemetry, s.sessionID),
		tools.NewResultMaskingWrapper(resultMasker),
	}
	// This attestation applies only to the exact wrappers constructed above.
	// The production wrapper has no result cache, and its sidecars must be the
	// synchronous built-in audit/telemetry implementations.
	synchronousWrappers := defaultToolExecutionSidecarsAreSynchronous(s.auditLog, s.telemetry)
	preparedRegistry, ok := s.tr.(*tools.MiddlewareToolRegistry)
	if !ok {
		return errors.New("assemble: prepared tool registry is unavailable")
	}
	if err := preparedRegistry.SetPreparedToolStages(tools.PreparedToolStages{
		ArgumentPreparers:            []tools.ArgumentPreparer{pathNormalizer},
		ExecutionWrappers:            wrappers,
		SynchronousExecutionWrappers: synchronousWrappers,
		MutationQueue:                s.mutationQueue,
	}); err != nil {
		return fmt.Errorf("assemble: install prepared tool stages: %w", err)
	}
	s.reg.RegisterToolRegistry(s.tr)
	return nil
}

// defaultToolExecutionSidecarsAreSynchronous only admits the sidecars used by
// the fixed wrapper chain above: audit may be absent or the built-in sync log,
// and telemetry must be the built-in in-memory collector. An injected
// replacement keeps the wrappers installed but suppresses completion proof.
func defaultToolExecutionSidecarsAreSynchronous(auditLog production.AuditLog, telemetry production.Telemetry) bool {
	defaultTelemetry, ok := telemetry.(*production.DefaultTelemetry)
	if !ok || defaultTelemetry == nil {
		return false
	}
	if auditLog == nil {
		return true
	}
	defaultAuditLog, ok := auditLog.(*production.DefaultAuditLog)
	return ok && defaultAuditLog != nil
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
	dispatcher := core.NewDefaultSubagentDispatcher(subAgentFactory)
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
				s.acpConnectErr = connErr
			} else {
				acpMW := acp.NewACPMiddleware("acp-bridge", s.acpClient)
				s.acpAdapter = acp.NewACPMiddlewareAdapter(acpMW, dispatcher, s.acpClient)
				s.logger.Info("assemble_acp_connected", "transport", s.rc.ACP.Transport)
			}
		}
	}
}

// assembleExtraTools registers remaining tools (todo, task, goal, web, plan_mode, etc.).
func (s *assembleState) assembleExtraTools() error {
	todoStore := tools.NewTodoStore()
	taskStore := tools.NewTaskStore()
	goalStore, _ := tools.NewDefaultGoalStore("") //nolint:errcheck
	s.hitlEmitter = &cliHITLEmitter{out: s.out}

	webSearchTool := tools.NewWebSearchTool()
	if s.rc != nil {
		switch s.rc.WebSearch.Provider {
		case "":
			// An unconfigured provider stays unavailable; never substitute sample results.
		case "mock":
			s.logger.Warn("assemble_web_search_mock_provider_disabled")
		case "brave":
			webSearchTool = tools.NewWebSearchTool(tools.WithSearchProvider(
				tools.NewBraveSearchProvider(s.rc.WebSearch.APIKey),
			))
		case "fetch":
			webSearchTool = tools.NewWebSearchTool(tools.WithSearchProvider(
				tools.NewFetchSearchProvider(),
			))
		default:
			s.logger.Warn("assemble_web_search_provider_unknown", "provider", s.rc.WebSearch.Provider)
		}
	}
	webFetchTool := tools.NewWebFetchTool(tools.WithHTMLConverter(s.htmlConverter))

	extraTools := []tools.ToolDefinition{
		tools.NewTodoWriteTool(todoStore),
		tools.NewTaskCreateTool(taskStore),
		tools.NewTaskUpdateTool(taskStore),
		tools.NewGoalCreateTool(goalStore),
		tools.NewGoalUpdateTool(goalStore),
		tools.NewGitPRCreateTool(s.gitCwd),
	}
	for _, t := range extraTools {
		if regErr := s.tr.Register(s.ctx, t); regErr != nil {
			s.logger.Warn("assemble_tool_register_failed", "tool", t.Name(), "err", regErr)
		}
	}
	trustedControls := []tools.ToolDefinition{
		tools.NewTaskGetTool(taskStore),
		tools.NewTaskListTool(taskStore),
		tools.NewGoalListTool(goalStore, taskStore),
		tools.NewGoalGetTool(goalStore, taskStore),
		core.NewAskUserQuestionTool(s.hitlEmitter, 30*time.Second),
		tools.NewEnterPlanModeTool(s.planCtrl),
		tools.NewExitPlanModeTool(s.planCtrl),
		tools.NewToolSearchTool(s.tr),
	}
	trustedNetworkReads := []tools.ToolDefinition{webFetchTool, webSearchTool}
	registrar, ok := s.tr.(tools.TrustedControlRegistrar)
	if !ok {
		return errors.New("assemble: trusted control registrar unavailable")
	}
	for _, t := range trustedControls {
		if regErr := registrar.RegisterTrustedControl(s.ctx, t); regErr != nil {
			return fmt.Errorf("assemble: register trusted control tool %q: %w", t.Name(), regErr)
		}
	}
	networkRegistrar, ok := s.tr.(tools.TrustedNetworkReadRegistrar)
	if !ok {
		return errors.New("assemble: trusted network read registrar unavailable")
	}
	for _, t := range trustedNetworkReads {
		if regErr := networkRegistrar.RegisterTrustedNetworkRead(s.ctx, t); regErr != nil {
			return fmt.Errorf("assemble: register trusted network read tool %q: %w", t.Name(), regErr)
		}
	}
	return nil
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

	// Bind this Assembly's pre-tool interceptors to its loop. Plan Mode runs
	// first so blocked calls retain the tool_canceled event semantics; shell
	// hooks then run only for calls allowed by Plan Mode. The executor wrapper
	// remains a final Plan Mode guard for calls outside the loop path.
	interceptors := []core.ToolInterceptor{core.NewPlanModeToolInterceptor(s.planCtrl)}
	if s.hookMgr != nil {
		interceptors = append(interceptors, s.hookMgr.PreToolUseInterceptor())
	}
	s.loopAgent.SetToolInterceptor(func(toolName, toolCallID string, args map[string]any) error {
		for _, interceptor := range interceptors {
			if err := interceptor(toolName, toolCallID, args); err != nil {
				return err
			}
		}
		return nil
	})

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
	if s.durableSession != nil {
		return
	}
	if s.ac.enableSession && s.rc != nil && s.rc.Session.StorePath != "" {
		s.sessionStore = session.NewJSONLSessionStore(s.rc.Session.StorePath)
		if openErr := s.sessionStore.Open(s.ctx); openErr != nil {
			s.logger.Warn("assemble_session_open_failed", "err", openErr)
			s.sessionStore = nil
		}
	}
}

// cliModelResolver owns one immutable provider snapshot for the primary model,
// configured small model, explicit /model switches, and model listing.
type cliModelResolver struct {
	resolver          *llm.ModelResolver
	authChain         *config.AuthChain
	primaryProviderID string
	primaryEndpoint   string
	smallProviderID   string
	smallEndpoint     string
	smallErr          error
}

type cliProviderCredential struct {
	providerID string
	endpoint   string
	apiKey     string
	optional   bool
}

// newCLIModelResolver builds the CLI's provider definitions before publishing
// one resolver snapshot. Credentials remain in the scoped AuthChain and are
// never stored in provider metadata or provider constructors.
func newCLIModelResolver(rc *config.Config, primaryProviderName string, includeSmall bool, primaryModelName ...string) (*cliModelResolver, error) {
	primaryProviderName = normalizeProviderName(primaryProviderName)
	if primaryProviderName == "" {
		return nil, fmt.Errorf("assemble: provider identity is required")
	}
	if isDisabledCLIProvider(primaryProviderName) {
		return nil, unsupportedCLIProviderError(primaryProviderName)
	}

	primary := cliProviderCredential{providerID: primaryProviderName}
	if rc != nil {
		configuredProviderName := resolveProviderName("", rc)
		if providerIdentityMatches(configuredProviderName, primaryProviderName) {
			primary.endpoint = rc.Provider.BaseURL
			primary.apiKey = rc.Provider.APIKey
		}
	}
	bindings := []cliProviderCredential{primary}
	modelNamesByProvider := make(map[string][]llm.ModelInfo)
	configuredPrimaryModel := ""
	if len(primaryModelName) != 0 {
		configuredPrimaryModel = strings.TrimSpace(primaryModelName[0])
	}
	if configuredPrimaryModel == "" && rc != nil {
		configuredPrimaryModel = resolveModelName("", rc)
	}
	appendConfiguredModel(modelNamesByProvider, primaryProviderName, configuredPrimaryModel)
	smallProviderName := ""
	smallEndpoint := ""
	var smallErr error
	if includeSmall && rc != nil {
		smallProviderName = normalizeProviderName(rc.SmallModel.Provider)
		if smallProviderName == "" {
			smallProviderName = primaryProviderName
		}
		endpoint, apiKey, err := resolveSmallModelProviderSettings(primaryProviderName, smallProviderName, rc)
		if err != nil {
			smallErr = err
		} else if isDisabledCLIProvider(smallProviderName) {
			smallErr = unsupportedCLIProviderError(smallProviderName)
		} else {
			smallEndpoint = endpoint
			appendConfiguredModel(modelNamesByProvider, smallProviderName, rc.SmallModel.Model)
			bindings = append(bindings, cliProviderCredential{providerID: smallProviderName, endpoint: endpoint, apiKey: apiKey, optional: true})
		}
	}

	defaultProviderOptions := make([]llm.ProviderOption, 0, 2)
	if models := modelNamesByProvider["eino"]; len(models) != 0 {
		defaultProviderOptions = append(defaultProviderOptions,
			llm.WithModels(models),
			llm.WithDefaultModel(promptDefaultModel),
		)
	}
	registry := llm.NewProviderRegistryWithDefaultProviderOptions(defaultProviderOptions...)
	registered := map[string]struct{}{"eino": {}}
	for _, binding := range bindings {
		providerID := normalizeProviderName(binding.providerID)
		if providerID == "" || isDisabledCLIProvider(providerID) {
			continue
		}
		if _, exists := registered[providerID]; exists {
			continue
		}
		provider, err := newCLICompatibleProvider(providerID, binding.endpoint, modelNamesByProvider[providerID]...)
		if err != nil {
			if binding.optional {
				smallErr = err
				continue
			}
			return nil, err
		}
		if err := registry.Register(provider); err != nil {
			return nil, fmt.Errorf("assemble: register provider %q: %w", providerID, err)
		}
		registered[providerID] = struct{}{}
	}

	// Publish provider definitions first, then bind config keys to the resolver's
	// canonical provider/protocol/origin. The AuthChain's other sources remain
	// scope-aware; legacy provider-only sources stay inert.
	resolver := registry.SnapshotResolver()
	authChain := config.NewAuthChain()
	configuredKeys := make(map[llm.CredentialRef]string)
	for _, binding := range bindings {
		if binding.apiKey == "" || isDisabledCLIProvider(binding.providerID) {
			continue
		}
		if _, registeredProvider := registered[normalizeProviderName(binding.providerID)]; !registeredProvider {
			continue
		}
		ref, err := resolver.CredentialRefFor(binding.providerID, binding.endpoint)
		if err != nil {
			if binding.optional {
				smallErr = err
				continue
			}
			return nil, fmt.Errorf("assemble: resolve configured credential scope for %q: %w", binding.providerID, err)
		}
		if prior, exists := configuredKeys[ref]; exists {
			if prior != binding.apiKey {
				conflictErr := fmt.Errorf("assemble: conflicting primary/small credentials configured for provider scope %q at %q", ref.ProviderID, ref.Origin)
				if binding.optional {
					smallErr = conflictErr
					continue
				}
				return nil, conflictErr
			}
			continue
		}
		configuredKeys[ref] = binding.apiKey
		authChain.WithScopedCLIFlag(config.AuthScope{
			ProviderID: ref.ProviderID,
			ProtocolID: string(ref.ProtocolID),
			Origin:     ref.Origin,
		}, binding.apiKey)
	}
	registry.SetCredentialResolver(authChain)
	return &cliModelResolver{
		resolver:          registry.SnapshotResolver(),
		authChain:         authChain,
		primaryProviderID: primaryProviderName,
		primaryEndpoint:   primary.endpoint,
		smallProviderID:   smallProviderName,
		smallEndpoint:     smallEndpoint,
		smallErr:          smallErr,
	}, nil
}

func appendConfiguredModel(modelsByProvider map[string][]llm.ModelInfo, providerID, modelID string) {
	providerID = normalizeProviderName(providerID)
	modelID = strings.TrimSpace(modelID)
	if providerID == "" || modelID == "" || isDisabledCLIProvider(providerID) {
		return
	}
	for _, existing := range modelsByProvider[providerID] {
		if existing.Name == modelID {
			return
		}
	}
	modelsByProvider[providerID] = append(modelsByProvider[providerID], llm.ModelInfo{Name: modelID})
}

func cliPrimaryCredentialScope(rc *config.Config, providerName string) (config.AuthScope, error) {
	modelResolver, err := newCLIModelResolver(rc, providerName, false)
	if err != nil {
		return config.AuthScope{}, err
	}
	ref, err := modelResolver.resolver.CredentialRefFor(modelResolver.primaryProviderID, modelResolver.primaryEndpoint)
	if err != nil {
		return config.AuthScope{}, err
	}
	return config.AuthScope{ProviderID: ref.ProviderID, ProtocolID: string(ref.ProtocolID), Origin: ref.Origin}, nil
}

func resolveCLIPrimaryCredential(ctx context.Context, rc *config.Config, providerName string) (config.AuthScope, string, bool, error) {
	modelResolver, err := newCLIModelResolver(rc, providerName, false)
	if err != nil {
		return config.AuthScope{}, "", false, err
	}
	ref, err := modelResolver.resolver.CredentialRefFor(modelResolver.primaryProviderID, modelResolver.primaryEndpoint)
	if err != nil {
		return config.AuthScope{}, "", false, err
	}
	scope := config.AuthScope{ProviderID: ref.ProviderID, ProtocolID: string(ref.ProtocolID), Origin: ref.Origin}
	key, found, err := modelResolver.authChain.ResolveCredential(ctx, scope.ProviderID, scope.ProtocolID, scope.Origin)
	return scope, key, found, err
}

func newCLICompatibleProvider(providerID, endpoint string, models ...llm.ModelInfo) (llm.ModelProvider, error) {
	if isDisabledCLIProvider(providerID) {
		return nil, unsupportedCLIProviderError(providerID)
	}
	if providerID == "eino" {
		return nil, fmt.Errorf("assemble: default provider %q is already registered", providerID)
	}
	if providerID != "openai" && strings.TrimSpace(endpoint) == "" {
		return nil, providerEndpointRequiredError(providerID)
	}
	options := []llm.ProviderOption{llm.WithProviderName(providerID)}
	if len(models) != 0 {
		options = append(options, llm.WithModels(models))
	}
	if strings.TrimSpace(endpoint) != "" {
		options = append(options, llm.WithBaseURL(endpoint))
	}
	return llm.NewEinoProvider(options...), nil
}

func providerEndpointRequiredError(providerID string) error {
	return fmt.Errorf("assemble: %w: provider %q requires an explicit base_url for OpenAI-compatible routing", llm.ErrProviderEndpointRequired, providerID)
}

func isDisabledCLIProvider(providerID string) bool {
	switch normalizeProviderName(providerID) {
	case "claude", "anthropic", "gemini", "google-gemini":
		return true
	default:
		return false
	}
}

func unsupportedCLIProviderError(providerID string) error {
	return fmt.Errorf("assemble: %w: %s; native protocol support is unavailable in the CLI; use a custom provider name only for an OpenAI-compatible BaseURL", llm.ErrUnsupportedProviderProtocol, normalizeProviderName(providerID))
}

// buildModel resolves an llm.BaseChatModel from a fresh immutable snapshot for
// direct callers. Assembly paths share one snapshot through cliModelResolver.
func buildModel(ctx context.Context, rc *config.Config, providerName, modelName string) (llm.BaseChatModel, llm.ModelInfo, func(), error) {
	modelResolver, err := newCLIModelResolver(rc, providerName, false, modelName)
	if err != nil {
		return nil, llm.ModelInfo{}, nil, err
	}
	return modelResolver.buildPrimary(ctx, rc, providerName, modelName)
}

func (r *cliModelResolver) buildPrimary(ctx context.Context, rc *config.Config, providerName, modelName string) (llm.BaseChatModel, llm.ModelInfo, func(), error) {
	request := llm.ModelRequest{ProviderID: providerName, ModelID: modelName}
	if providerIdentityMatches(r.primaryProviderID, providerName) {
		request.Endpoint = r.primaryEndpoint
	}
	if rc != nil {
		providerSampling := config.SamplingSettings{}
		if providerIdentityMatches(resolveProviderName("", rc), providerName) {
			providerSampling = rc.ProviderSampling()
		}
		applySamplingSettings(&request, rc.ModelSampling(), providerSampling)
	} else {
		request.MaxTokens = config.DefaultLLMMaxTokens
		request.MaxTokensSet = true
	}
	return r.build(ctx, request)
}

func (r *cliModelResolver) buildSmall(ctx context.Context, rc *config.Config, primaryProviderName, providerName, modelName string) (llm.BaseChatModel, llm.ModelInfo, func(), error) {
	if rc == nil {
		return nil, llm.ModelInfo{}, nil, fmt.Errorf("assemble: buildSmallModel: config is nil")
	}
	if r.smallErr != nil {
		return nil, llm.ModelInfo{}, nil, r.smallErr
	}
	providerName = normalizeProviderName(providerName)
	if providerName == "" {
		providerName = normalizeProviderName(primaryProviderName)
	}
	endpoint, _, err := resolveSmallModelProviderSettings(primaryProviderName, providerName, rc)
	if err != nil {
		return nil, llm.ModelInfo{}, nil, err
	}
	if providerName != "eino" && providerName != "openai" && strings.TrimSpace(endpoint) == "" {
		return nil, llm.ModelInfo{}, nil, providerEndpointRequiredError(providerName)
	}
	request := llm.ModelRequest{
		ProviderID: providerName,
		ModelID:    modelName,
		Endpoint:   endpoint,
	}
	providerSampling := config.SamplingSettings{}
	if providerIdentityMatches(resolveProviderName("", rc), providerName) {
		providerSampling = rc.ProviderSampling()
	}
	applySamplingSettings(&request, rc.SmallModelSampling(), providerSampling)
	return r.build(ctx, request)
}

func applySamplingSettings(request *llm.ModelRequest, model, provider config.SamplingSettings) {
	if model.TemperatureSet {
		request.Temperature = model.Temperature
		request.TemperatureSet = true
	} else if provider.TemperatureSet {
		request.Temperature = provider.Temperature
		request.TemperatureSet = true
	}

	if model.MaxTokensSet {
		request.MaxTokens = model.MaxTokens
		request.MaxTokensSet = true
	} else if provider.MaxTokensSet {
		request.MaxTokens = provider.MaxTokens
		request.MaxTokensSet = true
	} else {
		request.MaxTokens = config.DefaultLLMMaxTokens
		request.MaxTokensSet = true
	}
}

func (r *cliModelResolver) build(ctx context.Context, request llm.ModelRequest) (llm.BaseChatModel, llm.ModelInfo, func(), error) {
	if isDisabledCLIProvider(request.ProviderID) {
		return nil, llm.ModelInfo{}, nil, unsupportedCLIProviderError(request.ProviderID)
	}
	ref, err := r.resolver.CredentialRefFor(request.ProviderID, request.Endpoint)
	if err != nil {
		return nil, llm.ModelInfo{}, nil, err
	}
	request.Credential = ref
	resolved, err := r.resolver.ResolveModel(ctx, request)
	if err != nil {
		return nil, llm.ModelInfo{}, nil, err
	}
	model, cleanup, err := resolved.Build(ctx)
	if err != nil {
		return nil, llm.ModelInfo{}, cleanup, err
	}
	if resolved.Model.Known {
		return model, resolved.Model.Info, cleanup, nil
	}
	return model, llm.ModelInfo{}, cleanup, nil
}

func (r *cliModelResolver) buildInteractive(ctx context.Context, rc *config.Config, candidate llm.ModelSpec) (llm.BaseChatModel, llm.ModelInfo, func(), error) {
	providerName := normalizeProviderName(candidate.ProviderID)
	modelName := strings.TrimSpace(candidate.ModelID)
	if providerIdentityMatches(r.primaryProviderID, providerName) {
		return r.buildPrimary(ctx, rc, providerName, modelName)
	}

	request := llm.ModelRequest{ProviderID: providerName, ModelID: modelName}
	if providerIdentityMatches(r.smallProviderID, providerName) {
		request.Endpoint = r.smallEndpoint
	}
	providerSampling := config.SamplingSettings{}
	if rc != nil && providerIdentityMatches(resolveProviderName("", rc), providerName) {
		providerSampling = rc.ProviderSampling()
	}
	applySamplingSettings(&request, config.SamplingSettings{}, providerSampling)
	return r.build(ctx, request)
}

func (r *cliModelResolver) listModelSpecs() []llm.ModelSpec {
	if r == nil || r.resolver == nil {
		return nil
	}
	return r.resolver.ListModels()
}

// listAvailableModels returns only models for a provider request accepted by
// the same immutable resolver and exact-scope credential chain as generation.
func listAvailableModels(rc *config.Config, providerName string) []llm.ModelInfo {
	modelResolver, err := newCLIModelResolver(rc, providerName, true)
	if err != nil {
		return nil
	}
	return modelResolver.listAvailableModels(context.Background(), rc, providerName)
}

func (r *cliModelResolver) listAvailableModels(ctx context.Context, rc *config.Config, providerName string) []llm.ModelInfo {
	request := llm.ModelRequest{ProviderID: providerName}
	if providerIdentityMatches(r.primaryProviderID, providerName) {
		request.Endpoint = r.primaryEndpoint
	}
	if rc != nil {
		request.ModelID = resolveModelName("", rc)
		applySamplingSettings(&request, rc.ModelSampling(), config.SamplingSettings{})
	}
	resolved, err := r.resolve(ctx, request)
	if err != nil {
		return nil
	}
	models := r.listModelSpecs()
	result := make([]llm.ModelInfo, 0, len(models))
	for _, model := range models {
		if model.ProviderID == resolved.ProviderID {
			result = append(result, model.Info)
		}
	}
	return result
}

func (r *cliModelResolver) resolve(ctx context.Context, request llm.ModelRequest) (llm.ResolvedModel, error) {
	if isDisabledCLIProvider(request.ProviderID) {
		return llm.ResolvedModel{}, unsupportedCLIProviderError(request.ProviderID)
	}
	ref, err := r.resolver.CredentialRefFor(request.ProviderID, request.Endpoint)
	if err != nil {
		return llm.ResolvedModel{}, err
	}
	request.Credential = ref
	return r.resolver.ResolveModel(ctx, request)
}

// resolveCLIProvider resolves only provider protocols that the CLI can route
// without guessing. An explicit BaseURL declares an OpenAI-compatible custom
// endpoint, while API-key-only routing is accepted only for the named Eino or
// OpenAI-compatible defaults. Native Claude/Gemini identities are unavailable
// in the CLI until their request and tool-call contracts are complete.
func normalizeProviderName(providerName string) string {
	return strings.ToLower(strings.TrimSpace(providerName))
}

// buildSmallModel builds a lightweight model from the SmallModelConfig section.
// Primary credentials are reused only when provider identity and effective
// endpoint origin match; different identities or origins use small-model
// settings only.
func buildSmallModel(ctx context.Context, rc *config.Config, primaryProviderName, providerName, modelName string) (llm.BaseChatModel, llm.ModelInfo, func(), error) {
	if rc == nil {
		return nil, llm.ModelInfo{}, nil, fmt.Errorf("assemble: buildSmallModel: config is nil")
	}
	modelResolver, err := newCLIModelResolver(rc, primaryProviderName, true)
	if err != nil {
		return nil, llm.ModelInfo{}, nil, err
	}
	return modelResolver.buildSmall(ctx, rc, primaryProviderName, providerName, modelName)
}

// resolveSmallModelProviderSettings prevents credentials from crossing a
// provider identity or origin boundary. The small model inherits the primary
// endpoint and credential only when its provider identity matches. If it
// explicitly selects another endpoint, a primary key may be reused only when
// the normalized origins still match.
func resolveSmallModelProviderSettings(primaryProviderName, smallProviderName string, rc *config.Config) (baseURL, apiKey string, err error) {
	baseURL = rc.SmallModel.BaseURL
	apiKey = rc.SmallModel.APIKey
	if providerIdentityMatches(primaryProviderName, smallProviderName) {
		// The provider block belongs to its declared provider name. A CLI
		// override may use this endpoint/key only when it names the same
		// canonical identity; otherwise the selected provider has its own
		// endpoint and credential scope.
		primaryConfigMatches := providerIdentityMatches(resolveProviderName("", rc), primaryProviderName)
		if baseURL == "" {
			if primaryConfigMatches {
				baseURL = rc.Provider.BaseURL
				if apiKey == "" {
					apiKey = rc.Provider.APIKey
				}
			}
			return baseURL, apiKey, nil
		}

		if primaryConfigMatches && apiKey == "" && rc.Provider.APIKey != "" {
			primaryOrigin, primaryOK := effectiveCLIProviderOrigin(primaryProviderName, rc.Provider.BaseURL)
			smallOrigin, smallOK := effectiveCLIProviderOrigin(smallProviderName, baseURL)
			if primaryOK && smallOK && primaryOrigin == smallOrigin {
				apiKey = rc.Provider.APIKey
			}
		}
		return baseURL, apiKey, nil
	}

	if baseURL == "" && apiKey == "" {
		return "", "", fmt.Errorf("assemble: small provider %q differs from primary provider %q and has no own base_url or api_key", smallProviderName, primaryProviderName)
	}
	return baseURL, apiKey, nil
}

func providerIdentityMatches(primaryProviderName, smallProviderName string) bool {
	primaryName := normalizeProviderName(primaryProviderName)
	return primaryName != "" && primaryName == normalizeProviderName(smallProviderName)
}

func effectiveCLIProviderOrigin(providerName, baseURL string) (string, bool) {
	if baseURL == "" {
		switch normalizeProviderName(providerName) {
		case "eino", "openai":
			baseURL = cliDefaultOpenAICompatibleBaseURL
		default:
			return "", false
		}
	}

	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Hostname() == "" {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	port := parsed.Port()
	switch scheme {
	case "http":
		if port == "" {
			port = "80"
		}
	case "https":
		if port == "" {
			port = "443"
		}
	default:
		return "", false
	}

	return scheme + "://" + net.JoinHostPort(strings.ToLower(parsed.Hostname()), port), true
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
	return registerMCPToolsOwned(ctx, rc, tr, nil)
}

func registerMCPToolsOwned(ctx context.Context, rc *config.Config, tr tools.DeferredRegistry, owner *assemblyMCPResources) error {
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
				cfg.Transport = mcp.MCPTransportStreamableHTTP
			} else {
				return nil
			}

			var client mcp.MCPClient
			if cfg.Transport == mcp.MCPTransportStreamableHTTP {
				client = mcp.NewHTTPClientAdapter(cfg)
			} else {
				client = mcp.NewOfficialSDKAdapter(cfg)
			}
			if owner != nil {
				owner.addClient(client)
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
			if owner != nil {
				createAndStartHotReloader(ctx, configPath, client, tr, owner)
			}
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
func createAndStartHotReloader(ctx context.Context, configPath string, client mcp.MCPClient, tr tools.DeferredRegistry, owner *assemblyMCPResources) {
	clientRef := client
	registerFn := mcp.RegisterToolsFunc(func(mcpTools []mcp.MCPTool) {
		registerMCPToolBatch(context.Background(), tr, clientRef, mcpTools)
		slog.Info("assemble_mcp_hot_reload_reregistered", "server", clientRef.Name(), "tools", len(mcpTools))
	})
	hr := mcp.NewDefaultHotReloader(client, registerFn)
	if err := owner.addReloader(hr); err != nil {
		return
	}
	if watchErr := hr.Watch(ctx, configPath); watchErr != nil {
		slog.Warn("assemble_mcp_hot_reload_watch_failed", "server", client.Name(), "err", watchErr)
		if lifecycle, ok := hr.(mcp.HotReloadLifecycle); ok {
			lifecycle.RequestStop()
		}
		return
	}
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
var _ core.CanonicalMiddleware = (*loopDetectorMiddleware)(nil)

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

func (m *loopDetectorMiddleware) WrapCanonical(next core.CanonicalLoop) core.CanonicalLoop {
	return &canonicalLoopDetectorLoop{detector: m.detector, manager: m.manager, next: next}
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
	observeLoopDetector(ctx, l.detector, l.manager, events)
	return events, nil
}

type canonicalLoopDetectorLoop struct {
	detector production.LoopDetector
	manager  *core.DefaultSystemReminderManager
	next     core.CanonicalLoop
}

func (l *canonicalLoopDetectorLoop) RunCanonical(ctx context.Context, submission core.Submission, stream ...core.EventStream) (core.CanonicalRunResult, error) {
	result, err := l.next.RunCanonical(ctx, submission, stream...)
	if err != nil {
		return result, err
	}
	observeLoopDetector(ctx, l.detector, l.manager, result.Events)
	return result, nil
}

func observeLoopDetector(ctx context.Context, detector production.LoopDetector, manager *core.DefaultSystemReminderManager, events []core.AgentEvent) {
	for _, ev := range events {
		_ = detector.Observe(ctx, ev) //nolint:errcheck
	}

	res := detector.Check(ctx)
	if res.Detected {
		slog.WarnContext(ctx, "loop_detected",
			"dimension", res.Dimension,
			"count", res.Count,
			"threshold", res.Threshold,
			"message", res.Message,
		)
		manager.AddReminder(core.SystemReminder{
			ID:      "loop-detector",
			Content: fmt.Sprintf("Loop detected: %s. Please try a different approach.", res.Message),
		})
	}
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

			// Mutation tools bypass the cache so they always reach the
			// approval middleware. Caching their results would let a
			// duplicate call skip user approval.
			skipCache := noCacheTools[call.Name]
			cacheKey := ""
			if !skipCache && cache != nil {
				var cacheable bool
				cacheKey, cacheable = toolCacheKey(call.Name, call.Args)
				skipCache = !cacheable
			}

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

const toolCacheKeyDomain = "go-cli/tool-cache/v1\x00"

// toolCacheKey returns a bounded, deterministic key without retaining or
// exposing the serialized tool arguments. A failed encoding is not cacheable.
func toolCacheKey(name string, args map[string]any) (string, bool) {
	data, err := json.Marshal(args)
	if err != nil {
		return "", false
	}
	digestInput := make([]byte, 0, len(toolCacheKeyDomain)+len(name)+1+len(data))
	digestInput = append(digestInput, toolCacheKeyDomain...)
	digestInput = append(digestInput, name...)
	digestInput = append(digestInput, 0)
	digestInput = append(digestInput, data...)
	sum := sha256.Sum256(digestInput)
	return "tool-cache:v1:" + hex.EncodeToString(sum[:]), true
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
	toolClass := "other"
	switch toolName {
	case "read", "grep", "find", "ls", "image_read", "git_status", "git_diff", "git_log", "git_blame":
		toolClass = "read"
	case "write", "edit", "git_branch", "git_checkout", "git_commit", "git_create_branch", "git_fetch", "git_merge", "git_pr_create", "git_pull", "git_push", "git_reset", "git_revert", "git_stash", "git_stash_pop":
		toolClass = "mutation"
	case "bash", "remote_bash":
		toolClass = "process"
	case "web_fetch", "web_search":
		toolClass = "web"
	}
	_ = telemetry.Record(ctx, production.TelemetryMetric{ //nolint:errcheck
		Name:  "tool.call.count",
		Value: 1,
		Labels: map[string]string{
			"tool_class": toolClass,
			"cache_hit":  fmt.Sprintf("%v", cacheHit),
		},
	})
	_ = telemetry.Record(ctx, production.TelemetryMetric{ //nolint:errcheck
		Name:  "tool.call.duration_ms",
		Value: float64(duration.Milliseconds()),
		Labels: map[string]string{
			"tool_class": toolClass,
		},
	})
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
