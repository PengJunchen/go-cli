package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/tui"
)

// newEntryID generates a unique entry ID using crypto/rand. The resulting ID
// has the form "entry-<16 hex chars>" which avoids collisions with existing
// entries even when resuming a session that contains sequentially-numbered
// IDs.
func newEntryID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback to timestamp-based ID if crypto/rand fails.
		return fmt.Sprintf("entry-%d", time.Now().UnixNano())
	}
	return "entry-" + hex.EncodeToString(b[:])
}

// REPLSession holds the state of an interactive read-eval-print loop session.
// It is constructed by interactiveCmd.Run and encapsulates all session-level
// and turn-level state that was previously spread across local variables in
// the monolithic Run method.
type REPLSession struct {
	// cmd is the owning interactiveCmd, kept for handleSlashCommand.
	cmd *interactiveCmd

	// extraAssembleOptions supplies focused dependency overrides to REPL tests.
	// The interactive command itself does not expose these as user flags.
	extraAssembleOptions []AssembleOption

	// unknownCancelRecovery is set only after an explicit TUI cancellation has
	// joined a quiescent owner and its exact unknown terminal checkpoint.
	unknownCancelRecovery *durableUnknownCancelRecovery
	historyAdmissionError error

	// I/O
	out io.Writer
	in  io.Reader

	// Configuration
	cfg  Config
	args []string
	rc   *config.Config

	// Flags
	modelName        string
	modelFlag        string
	providerName     string
	maxTokens        int
	thinkingLevel    llm.ThinkingLevel
	thinkingFlag     string
	verboseFlag      bool
	resumeFlag       bool
	resumeSessionID  string
	noSandboxFlag    bool
	noOnboardingFlag bool
	approveMode      ApproveMode

	// Agent assembly
	assembly        *AgentAssembly
	approvalCh      chan tui.ApprovalRequest
	memoryCtx       context.Context
	memoryCtxCancel context.CancelFunc

	// Slash command
	slashCtx slashContext
	slashReg *SlashCommandRegistry

	// Session
	sessionTree    session.SessionTree
	sessionHandler *session.SessionSlashHandler
	sharedThemeMgr ThemeController

	// pendingWrites buffers session entries for batched persistence,
	// reducing syscall overhead by flushing multiple entries in a single
	// pass instead of writing each one individually.
	pendingWrites *session.PendingSessionWrites

	// flushTicker periodically flushes pending writes as a safety net.
	flushMu      sync.Mutex
	flushTicker  *time.Ticker
	flushDone    chan struct{}
	flushExited  chan struct{}
	flushStarted bool
	flushStopped bool

	// Editor/expander
	lineEditor      LineEditor
	mentionExpander *MentionExpander

	// Counters
	entryCounter int
	turnCounter  int

	// Ctrl+C graded semantics: records the time of the last Ctrl+C on an
	// empty line. A second Ctrl+C within ctrlCDoublePressWindow exits.
	lastInterruptTime time.Time

	// Logging
	logger *slog.Logger

	// Tracing (session-level)
	span    tracing.TraceSpan
	spanCtx context.Context

	// Turn-level state (shared across executeTurn sub-methods)
	line              string
	turnSpan          tracing.TraceSpan
	turnCtx           context.Context
	cancelTurn        context.CancelFunc
	interrupter       *InterruptHandler
	stream            *core.EventStreamImpl
	turnResult        core.Result
	turnErr           error
	turnDone          chan struct{}
	uiCancelRequested atomic.Bool
	isTTY             bool
	app               *replApp
	eventSource       *tui.OwnedAgentEventStream
	uiCtx             context.Context
	cancelUI          context.CancelFunc
	appStarted        bool
}

const durableOwnerJoinWait = 2 * time.Second

type durableRunBindingError struct {
	cause error
}

func (err *durableRunBindingError) Error() string {
	if err == nil || err.cause == nil {
		return "interactive: durable run binding failed"
	}
	return fmt.Sprintf("interactive: durable run binding failed: %v", err.cause)
}

func (err *durableRunBindingError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// Run starts the session: it parses flags, assembles the agent, enters the
// REPL loop, and cleans up on exit.
func (s *REPLSession) Run(ctx context.Context, cfg Config, args []string) (runErr error) {
	s.cfg = cfg
	s.args = args
	sessionStarted := false
	defer func() {
		if sessionStarted && runErr == nil && ctx.Err() == nil && s.assembly != nil {
			s.assembly.DrainMemory(ctx)
		}
		closeErr := closeAssemblyForCommand(context.Background(), s.assembly, "interactive")
		if sessionStarted {
			s.cleanup()
		} else {
			s.stopFlushTicker()
		}
		runErr = errors.Join(runErr, closeErr)
		if s.span == nil {
			return
		}
		if runErr != nil {
			s.span.SetStatus(tracing.SpanStatusError, runErr.Error())
		} else {
			s.span.SetStatus(tracing.SpanStatusOK, "")
		}
		s.span.End()
	}()
	if err := s.start(ctx); err != nil {
		return err
	}
	sessionStarted = true
	return s.readInput()
}

// start performs all one-time session setup: flag parsing, onboarding, agent
// assembly, slash-context wiring, and editor/mention-expander initialization.
func (s *REPLSession) start(ctx context.Context) error {
	if err := s.parseFlags(); err != nil {
		return err
	}
	// Run first-run onboarding wizard after flag parsing so that invalid
	// flags produce a UsageError before the wizard runs. Skipped via
	// --no-onboarding flag or GO_CLI_NO_ONBOARDING env, and when the API
	// key is already configured.
	if s.rc != nil && !s.noOnboardingFlag {
		in := s.in
		if in == nil {
			in = os.Stdin
		}
		if err := runOnboardingWithModelOverride(ctx, s.rc, s.providerName, s.modelFlag, in, s.out); err != nil {
			return err
		}
		if s.modelFlag == "" {
			s.modelName = resolveModelName("", s.rc)
		}
	}
	if err := s.setupAgent(ctx); err != nil {
		return err
	}
	if err := s.setupSlashContext(); err != nil {
		return err
	}
	s.setupEditors()
	return nil
}

// parseFlags parses the interactive command-line flags and resolves the
// model/provider/config from the environment.
func (s *REPLSession) parseFlags() error {
	fs := flag.NewFlagSet("interactive", flag.ContinueOnError)
	fs.SetOutput(s.out)

	var (
		modelFlag        string
		providerFlag     string
		maxTokensFlag    int
		verboseFlag      bool
		resumeFlag       bool
		resumeSessionID  string
		thinkingFlag     string
		noSandboxFlag    bool
		noOnboardingFlag bool
		approveMode      ApproveMode
	)
	fs.StringVar(&modelFlag, "model", "", "model name to use")
	fs.StringVar(&providerFlag, "provider", "", "provider name to use")
	fs.IntVar(&maxTokensFlag, "max-tokens", defaultMaxTokens, "compaction token budget")
	fs.BoolVar(&verboseFlag, "verbose", false, "enable verbose output")
	fs.BoolVar(&resumeFlag, "resume", false, "resume previous session from store path")
	fs.StringVar(&resumeSessionID, "resume-session", "", "resume an exact durable physical session ID")
	fs.StringVar(&thinkingFlag, "thinking", "medium", "thinking level: none|minimal|low|medium|high|max")
	fs.BoolVar(&noSandboxFlag, "no-sandbox", false, "disable bash sandbox enforcement")
	fs.BoolVar(&noOnboardingFlag, "no-onboarding", false, "skip first-run onboarding wizard")
	fs.Var(&approveMode, "approve", "approval mode: ask|auto|deny (flag > approval.mode > ask)")
	if err := fs.Parse(s.args); err != nil {
		return newUsageError("interactive: %v", err)
	}

	thinkingLevel, err := llm.ParseThinkingLevel(thinkingFlag)
	if err != nil {
		return newUsageError("interactive: %v", err)
	}

	if v, ok := s.cfg.(*config.Config); ok {
		s.rc = v
	}
	approveExplicit := false
	fs.Visit(func(option *flag.Flag) {
		if option.Name == "approve" {
			approveExplicit = true
		}
	})
	resolvedApproval, err := resolveApprovalMode(s.rc, approveMode, approveExplicit)
	if err != nil {
		return err
	}
	s.approveMode = resolvedApproval
	if resumeFlag && resumeSessionID != "" {
		return newUsageError("interactive: choose --resume or --resume-session, not both")
	}
	if resumeSessionID != "" && (s.rc == nil || s.rc.Session.StorePath == "") {
		return newUsageError("interactive: --resume-session requires session.store_path")
	}

	// Build the dynamic slash command registry: built-in commands plus any
	// custom Markdown commands from .go-cli/commands/. Built-in commands take
	// priority over custom commands with the same name.
	s.slashReg = buildDynamicRegistry(s.rc)
	if s.cmd != nil {
		s.cmd.slashReg = s.slashReg
	}

	s.modelName = resolveModelName(modelFlag, s.rc)
	s.modelFlag = modelFlag
	s.providerName = resolveProviderName(providerFlag, s.rc)
	if maxTokensFlag <= 0 {
		maxTokensFlag = defaultMaxTokens
	}
	s.maxTokens = maxTokensFlag
	s.thinkingLevel = thinkingLevel
	s.thinkingFlag = thinkingFlag
	s.verboseFlag = verboseFlag
	s.resumeFlag = resumeFlag
	s.resumeSessionID = resumeSessionID
	s.noSandboxFlag = noSandboxFlag
	s.noOnboardingFlag = noOnboardingFlag
	return nil
}

// setupAgent creates the tracing span, logger, and assembles the full agent
// runtime with all production wiring.
func (s *REPLSession) setupAgent(ctx context.Context) error {
	s.span, s.spanCtx = tracing.SpanFromContext(ctx, spanInteractiveRun, tracing.SpanKindInternal)
	s.span.SetAttributes(
		tracing.Attribute{Key: "command", Value: s.cmd.Name()},
		tracing.Attribute{Key: "model", Value: s.modelName},
		tracing.Attribute{Key: "provider", Value: s.providerName},
		tracing.Attribute{Key: "max_tokens", Value: s.maxTokens},
		tracing.Attribute{Key: "thinking", Value: s.thinkingFlag},
	)

	s.logger = slog.Default()
	s.logger.Info("cli_interactive_run",
		"op", "cli.interactive.run",
		"provider", s.providerName,
		"model", s.modelName,
		"max_tokens", s.maxTokens,
		"verbose", s.verboseFlag,
		"thinking", s.thinkingFlag,
	)

	// Assemble the full agent runtime with all production wiring (model
	// wrapping, tools, approval gates, retry/cost tracking, output guards,
	// subagent, session persistence, compaction).
	//
	// When running in an interactive TTY, create a shared approval channel so
	// the TUI can render approval prompts inline (TeaApprovalCallback) instead
	// of blocking on stdin readline. The same channel is passed to each
	// per-turn BubbleteaApp via tui.WithApprovalChannel.
	var approvalCh chan tui.ApprovalRequest
	var durableSession *durableCLISession
	if s.rc != nil && s.rc.Session.StorePath != "" {
		var err error
		if s.resumeFlag {
			s.resumeSessionID, err = session.LatestPhysicalSession(s.spanCtx, s.rc.Session.StorePath)
			if err != nil {
				return newUsageError("interactive: %v", err)
			}
		}
		durableSession, err = newDurableCLISession(s.spanCtx, s.rc.Session.StorePath, "interactive", s.resumeSessionID, "")
		if err != nil {
			return newExecutionError("interactive: prepare durable session", err)
		}
	}
	assembleOpts := []AssembleOption{
		WithApproveMode(s.approveMode),
		WithMaxTokens(s.maxTokens),
		WithResume(s.resumeFlag),
		WithSessionPersistence(true),
		WithAgentName("interactive"),
		WithThinkingLevel(s.thinkingLevel),
	}
	assembleOpts = append(assembleOpts, s.extraAssembleOptions...)
	if durableSession != nil {
		assembleOpts = append(assembleOpts, withDurableCLISession(durableSession))
	}
	if s.noSandboxFlag {
		assembleOpts = append(assembleOpts, WithNoSandbox())
	}
	if s.approveMode == ApproveAsk && interactiveApprovalsEnabled() {
		approvalCh = make(chan tui.ApprovalRequest, 32)
		assembleOpts = append(assembleOpts, WithApprovalChannel(approvalCh))
	}
	assembly, err := AssembleAgent(s.spanCtx, s.rc, s.providerName, s.modelName, s.out,
		assembleOpts...)
	if err != nil {
		return newExecutionError("interactive: assemble agent", err)
	}
	s.assembly = assembly
	if _, err := fmt.Fprintf(s.out, "Approval mode: %s\n", assembly.ApprovalMode.String()); err != nil {
		return errors.Join(newExecutionError("interactive: display approval mode", err), assembly.Close(context.Background()))
	}
	if err := assembly.setInteractiveCloseHooks(func() {
		s.stopFlushTicker()
	}, func(ctx context.Context) error {
		if assembly.durableSession != nil {
			return nil
		}
		if assembly.SessionStore == nil || s.pendingWrites == nil {
			return nil
		}
		flushErr := s.pendingWrites.Flush(ctx, assembly.SessionStore)
		saveErr := assembly.SessionStore.Save(ctx)
		return errors.Join(flushErr, saveErr)
	}, func() {
		if s.memoryCtxCancel != nil {
			s.memoryCtxCancel()
		}
	}); err != nil {
		return err
	}
	s.approvalCh = approvalCh
	return nil
}

// setupSlashContext builds the slash command context from the assembled
// components and initializes the shared theme manager.
func (s *REPLSession) setupSlashContext() error {
	// Build slash command context from the assembled components.
	if s.assembly.durableSession != nil {
		snapshot, err := s.assembly.durableSession.snapshotSelected()
		if err != nil {
			return fmt.Errorf("interactive: snapshot durable session for slash commands: %w", err)
		}
		s.sessionTree, err = session.NewDefaultSessionTreeBuilder().BuildFromSnapshot(s.spanCtx, snapshot)
		if err != nil {
			return fmt.Errorf("interactive: build durable session tree: %w", err)
		}
		s.assembly.Agent.SetHistory(snapshot.PublishedView.Messages)
		s.sessionHandler = session.NewDurableReadOnlySessionSlashHandler(s.sessionTree)
	} else if s.assembly.SessionStore != nil {
		treeBuilder := session.NewDefaultSessionTreeBuilder()
		var err error
		s.sessionTree, err = treeBuilder.BuildFromStore(s.spanCtx, s.assembly.SessionStore)
		if err != nil {
			// fallback to empty tree on error
			s.sessionTree = session.NewDefaultSessionTree()
		}
		// Wire Git-aware branch linkage when enabled in config.
		if s.rc != nil && s.rc.Session.GitAwareBranch != nil && *s.rc.Session.GitAwareBranch && s.assembly.GitTool != nil {
			if dt, ok := s.sessionTree.(*session.DefaultSessionTree); ok {
				dt.SetGitBranchSwitcher(s.assembly.GitTool)
			}
		}
		s.sessionHandler = session.NewSessionSlashHandler(s.sessionTree, s.assembly.SessionStore)
		s.sessionHandler.OnResume = func(ctx context.Context, entries []session.SessionEntry) error {
			s.assembly.Agent.SetHistory(session.EntriesToAgentMessages(entries))
			return nil
		}
		// In directory mode, configure the session ID on the store. When
		// resuming, keep the existing session file; otherwise create a new
		// per-session file.
		if s.assembly.SessionStore.DirMode() {
			if err := s.assembly.SessionStore.SetSessionID(s.assembly.SessionID, !s.resumeFlag); err != nil {
				s.logger.Warn("cli_interactive_session_set_id_failed", "err", err)
			}
		}
	}
	if s.slashReg != nil {
		if registered, ok := s.slashReg.Lookup("session"); ok {
			if handler, ok := registered.(*SessionHandler); ok {
				switch {
				case s.assembly.durableSession != nil:
					handler.mode = sessionCommandModeDurable
					handler.historyAvailable = true
					handler.durableSession = s.assembly.durableSession
					handler.runSlotGuard = s.assembly.runSlotGuard
				case s.assembly.SessionStore != nil:
					handler.mode = sessionCommandModeLegacy
				default:
					handler.mode = sessionCommandModeUnavailable
				}
			}
		}
	}
	s.slashCtx = slashContext{
		agent:                  s.assembly.Agent,
		costTracker:            s.assembly.CostTracker,
		statsRegistry:          s.assembly.StatsRegistry,
		sessionID:              s.assembly.SessionID,
		durableMode:            s.assembly.durableSession != nil,
		toolRegistry:           s.assembly.ToolRegistry,
		modelName:              s.modelName,
		sessionHandler:         s.sessionHandler,
		out:                    s.out,
		config:                 s.rc,
		sessionStore:           s.assembly.SessionStore,
		fileTracker:            s.assembly.FileTracker,
		diffGenerator:          s.assembly.DiffGenerator,
		planCtrl:               s.assembly.PlanCtrl,
		memoryStore:            s.assembly.MemoryStore,
		contextWindow:          s.assembly.ContextWindow,
		localCompactionCeiling: s.assembly.MaxTokens,
		worktreeManager:        s.assembly.WorktreeManager,
		snapshotManager:        s.assembly.SnapshotMgr,
		modelSelector:          s.assembly.ModelSelector,
		estimator:              s.assembly.Estimator,
		promptBuilder:          s.assembly.PromptBuilder,
		contextLoader:          s.assembly.ContextLoader,
		mcpResources:           s.assembly.mcpResources,
		capabilities:           s.assembly,
	}
	if s.assembly.PhysicalSessionID != "" {
		s.slashCtx.sessionID = s.assembly.PhysicalSessionID
	}
	if s.assembly.durableSession != nil {
		s.slashCtx.historyCommands = &durableHistoryCommands{repl: s}
	}

	// Create a shared ThemeManager so /theme can switch themes at runtime
	// without restarting. The initial theme is set from config; subsequent
	// switches via /theme persist across turns because the same manager is
	// passed to every BubbleteaApp via WithThemeManager.
	s.sharedThemeMgr = newREPLThemeManager()
	if s.rc != nil && s.sharedThemeMgr != nil {
		themeName := strings.TrimSpace(strings.ToLower(s.rc.TUI.Theme))
		if themeName == "" || themeName == "auto" {
			themeName = "dark"
		}
		if err := s.sharedThemeMgr.Set(themeName); err != nil {
			s.logger.Warn("cli_interactive_theme_init", "theme", themeName, "err", err)
		}
	}
	s.slashCtx.themeMgr = s.sharedThemeMgr

	s.entryCounter = len(s.assembly.Agent.Messages())
	s.turnCounter = 0

	// Initialize batched session persistence. PendingSessionWrites buffers
	// entry writes and flushes them in batches, reducing syscall overhead.
	if s.assembly.SessionStore != nil {
		s.pendingWrites = session.NewPendingSessionWrites()
		s.startFlushTicker()
	}
	return nil
}

// setupEditors initializes the input reader, line editor (with history and
// tab-completion), and the @-mention expander.
func (s *REPLSession) setupEditors() {
	// Default to os.Stdin when no input reader was provided at registration
	// time (the common path when RunWithRegistry creates the command).
	in := s.in
	if in == nil {
		in = os.Stdin
	}

	fmt.Fprintln(s.out, "Interactive session started. Type 'exit' to quit.") //nolint:errcheck

	le := s.lineEditor
	if le == nil {
		// Resolve history file path: use the configured path or fall back
		// to ~/.go-cli/history.jsonl so history persists across sessions.
		historyPath := ""
		if s.rc != nil && s.rc.History.Path != "" {
			historyPath = s.rc.History.Path
		} else if home, err := os.UserHomeDir(); err == nil && home != "" {
			historyPath = filepath.Join(home, ".go-cli", "history.jsonl")
		}

		historyMaxLen := 1000
		if s.rc != nil && s.rc.History.MaxLen > 0 {
			historyMaxLen = s.rc.History.MaxLen
		}

		dle := NewDefaultLineEditor(in, s.out,
			WithHistoryPath(historyPath),
			WithHistoryMaxLen(historyMaxLen),
		)
		// Register completers in priority order: slash commands first,
		// then file paths, and finally LSP code completion (when an LSP
		// server is configured). CompositeCompleter uses first-non-empty-
		// wins fall-through, so nil children are silently skipped.
		var lspCompleter Completer
		if s.assembly.LSPClient != nil {
			lspCompleter = NewLSPCompleter(s.assembly.LSPClient, s.assembly.LSPWorkspaceRoot)
		}
		dle.SetCompleter(NewCompositeCompleter(
			NewSlashCommandCompleterFromRegistry(s.slashReg),
			NewFilePathCompleter(),
			lspCompleter,
		))

		// Load persisted history on startup (best-effort).
		if hs := dle.HistoryStore(); hs != nil {
			if err := hs.Load(); err != nil {
				s.logger.Warn("cli_interactive_history_load_failed", "err", err)
			}
		}
		le = dle
	}
	s.lineEditor = le

	// Initialize the @-mention expander using the current working directory
	// so relative @path tokens resolve correctly.
	if s.mentionExpander == nil {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		s.mentionExpander = NewMentionExpander(cwd, 0)
		// Wire typed mention resolvers from the assembled components.
		s.mentionExpander.SetResolver("symbol", NewSymbolMentionResolver(s.assembly.LSPClient, s.assembly.LSPWorkspaceRoot, cwd))
		s.mentionExpander.SetResolver("url", NewURLMentionResolver())
		if s.assembly.durableSession != nil {
			s.mentionExpander.SetResolver("session", &selectedSessionMentionResolver{snapshot: func() (session.PreparedSessionSnapshot, error) {
				return s.assembly.durableSession.snapshotSelected()
			}})
		} else if s.assembly.SessionStore != nil {
			s.mentionExpander.SetResolver("session", NewSessionMentionResolver(s.assembly.SessionStore))
		}
	}
}

// ctrlCDoublePressWindow is the time window within which a second Ctrl+C on
// an empty line exits the REPL.
const ctrlCDoublePressWindow = 1500 * time.Millisecond

// pendingWritesFlushInterval is how often the background goroutine flushes
// buffered session writes as a safety net against data loss on crash.
const pendingWritesFlushInterval = 1 * time.Second

// startFlushTicker launches a background goroutine that periodically flushes
// pending session writes to the store. It is stopped by stopFlushTicker.
func (s *REPLSession) startFlushTicker() {
	ticker := time.NewTicker(pendingWritesFlushInterval)
	done := make(chan struct{})
	exited := make(chan struct{})
	s.flushMu.Lock()
	if s.flushStopped || s.flushStarted {
		s.flushMu.Unlock()
		ticker.Stop()
		return
	}
	s.flushTicker = ticker
	s.flushDone = done
	s.flushExited = exited
	s.flushStarted = true
	// Snapshot fields to avoid concurrent access from the ticker goroutine.
	pw := s.pendingWrites
	var store session.SessionStore
	if s.assembly != nil {
		store = s.assembly.SessionStore
	}
	ctx := s.spanCtx
	logger := s.logger
	s.flushMu.Unlock()
	go func() {
		defer close(exited)
		for {
			select {
			case <-ticker.C:
				if pw == nil || store == nil {
					continue
				}
				if err := pw.Flush(ctx, store); err != nil {
					logger.Warn("cli_interactive_pending_writes_flush_failed", "err", err)
				}
			case <-done:
				return
			}
		}
	}()
}

// stopFlushTicker stops the background flush goroutine. It is safe to call
// when the ticker was never started.
func (s *REPLSession) stopFlushTicker() {
	s.flushMu.Lock()
	if !s.flushStopped {
		s.flushStopped = true
		if s.flushTicker != nil {
			s.flushTicker.Stop()
		}
		if s.flushDone != nil {
			close(s.flushDone)
		}
	}
	started := s.flushStarted
	exited := s.flushExited
	s.flushMu.Unlock()
	if started && exited != nil {
		<-exited
	}
}

// ctrlCAction represents the action to take when Ctrl+C is pressed.
type ctrlCAction int

const (
	ctrlCClearLine      ctrlCAction = iota // clear current input and re-prompt
	ctrlCShowExitPrompt                    // show "press again to exit" message
	ctrlCExit                              // exit the REPL
)

// evaluateCtrlC determines the graded Ctrl+C action based on whether the
// input buffer had content and the timing of the previous empty-line Ctrl+C.
// It returns the action to take and the new lastInterrupt timestamp to store.
func evaluateCtrlC(hadContent bool, lastInterrupt, now time.Time, window time.Duration) (ctrlCAction, time.Time) {
	if hadContent {
		// Non-empty input: clear the line and reset the double-press timer.
		return ctrlCClearLine, time.Time{}
	}
	// Empty input: check for double-press within the window.
	if !lastInterrupt.IsZero() && now.Sub(lastInterrupt) <= window {
		return ctrlCExit, time.Time{}
	}
	// First press on empty line (or window expired): show exit prompt.
	return ctrlCShowExitPrompt, now
}

// readInput is the REPL for loop. It reads user input, routes slash commands,
// expands @-mentions, and delegates each turn to executeTurn.
func (s *REPLSession) readInput() error {
	for {
		line, err := s.lineEditor.ReadLine(s.spanCtx, "> ")
		if err != nil {
			if errors.Is(err, errInterrupted) {
				if s.handleInterrupt(err) {
					break
				}
				continue
			}
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				break
			}
			return err
		}
		// Successful input resets the double-press timer.
		s.lastInterruptTime = time.Time{}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Slash command routing.
		if strings.HasPrefix(line, "/") {
			cmd, ok := session.ParseSlashCommand(line)
			if !ok {
				fmt.Fprintln(s.out, "Invalid command. Type /help for available commands.") //nolint:errcheck
				continue
			}
			if cmd.Name == "exit" {
				break
			}
			if s.unknownCancelRecovery != nil && !allowedDuringUnknownCancellation(cmd, s.slashReg) {
				printUnknownCancellationFence(s.out)
				continue
			}
			// Custom Markdown command handlers may return a pendingInput to
			// inject a prompt template that the REPL loop processes as user
			// input.
			pendingInput := s.cmd.handleSlashCommand(s.spanCtx, cmd, &s.slashCtx)
			if pendingInput != "" {
				line = pendingInput
				// Fall through to process as a normal user message.
			} else {
				continue
			}
		}
		if strings.EqualFold(line, exitCommand) {
			s.logger.Info("cli_interactive_exit", "op", "cli.interactive.exit")
			break
		}
		if s.unknownCancelRecovery != nil {
			printUnknownCancellationFence(s.out)
			continue
		}

		s.line = line
		if err := s.executeTurn(); err != nil {
			return err
		}
	}
	return nil
}

// handleInterrupt processes a Ctrl+C interrupt (errInterrupted) and returns
// true if the REPL should exit. It implements the graded Ctrl+C semantics:
// non-empty input clears the line; empty input shows an exit prompt; a second
// Ctrl+C within ctrlCDoublePressWindow on an empty line exits the REPL.
func (s *REPLSession) handleInterrupt(err error) bool {
	var ie *interruptedError
	hadContent := errors.As(err, &ie) && ie.hadContent
	action, newTime := evaluateCtrlC(hadContent, s.lastInterruptTime, time.Now(), ctrlCDoublePressWindow)
	s.lastInterruptTime = newTime
	switch action {
	case ctrlCClearLine:
		return false
	case ctrlCShowExitPrompt:
		fmt.Fprintln(s.out, "(press Ctrl+C again to exit)") //nolint:errcheck
		return false
	case ctrlCExit:
		s.logger.Info("cli_interactive_exit_interrupt", "op", "cli.interactive.exit.interrupt")
		return true
	}
	return false
}

// executeTurn orchestrates a single agent turn: it sets up the turn context,
// launches the agent goroutine, renders the TUI, waits for completion, then
// persists the session and extracts memories.
func (s *REPLSession) executeTurn() error {
	if s.historyAdmissionError != nil {
		return s.historyAdmissionError
	}
	s.setupTurn()
	s.assembly.RefreshMemoryContext(s.turnCtx)
	runCtx, preparedSnapshot, initialTranscript, runSink, err := s.prepareDurableTurn()
	if err != nil {
		s.finishTurnSetupFailure(err)
		return err
	}
	if err := s.renderTUI(initialTranscript); err != nil {
		if runSink != nil {
			runSink.CloseRunCommit()
		}
		s.finishTurnSetupFailure(err)
		return err
	}
	s.bindTodoUpdates()
	s.runAgentGoroutine(runCtx)
	waitErr := s.waitForTurnCompletion()

	// Clear the HITL program reference now that the turn is complete.
	// The bubbletea program from this turn has exited; leaving a stale
	// reference would cause the next turn to send to a dead program.
	if s.assembly.HITLEmitter != nil {
		s.assembly.HITLEmitter.clearProgram()
	}

	// Wait for RunTurn to finish so turnResult and turnErr are populated.
	<-s.turnDone
	var bindingErr *durableRunBindingError
	bindingFailed := errors.As(s.turnErr, &bindingErr)

	if bindingFailed {
		s.finishTurnSpan(bindingErr)
		s.cancelTurn()
		s.interrupter.Stop()
		s.stopTUI()
		return bindingErr
	}
	if waitErr != nil {
		s.finishTurnSpan(waitErr)
		s.cancelTurn()
		s.interrupter.Stop()
		s.stopTUI()
		return waitErr
	}

	if s.assembly.durableSession != nil {
		if err := s.applyDurableTurnRefresh(preparedSnapshot); err != nil {
			s.finishTurnSpan(err)
			s.cancelTurn()
			s.interrupter.Stop()
			s.stopTUI()
			return err
		}
	}
	interrupted := s.turnCtx.Err() != nil
	s.cancelTurn()
	s.interrupter.Stop()
	s.stopTUI()
	if s.unknownCancelRecovery != nil {
		s.finishTurnSpan(context.Canceled)
		fmt.Fprintln(s.out, "[interrupted] The run outcome remains unknown; this is not a rollback. External or workspace side effects may be partial.")              //nolint:errcheck
		fmt.Fprintln(s.out, "The old run will not be retried. Use /session pending to inspect unresolved actions, then /session new to continue in a fresh session.") //nolint:errcheck
		return nil
	}

	if err := s.finishTurnSpanAfterCompletion(interrupted); err != nil {
		return err
	}

	if s.turnErr != nil && !interrupted {
		if ufe := classifyError(s.turnErr); ufe != nil {
			if ufe.DiagnosticCode != "" {
				s.logger.ErrorContext(s.spanCtx, "cli.turn.failed", "op", "cli.turn.failed", "error_type", ufe.DiagnosticCode)
			}
			fmt.Fprintf(s.out, "Error: %s\n", ufe.Action) //nolint:errcheck
			fmt.Fprintf(s.out, "Hint: %s\n", ufe.Hint)    //nolint:errcheck
		} else {
			fmt.Fprintf(s.out, "Error: %v\n", s.turnErr) //nolint:errcheck
		}
		return nil
	}

	if interrupted {
		fmt.Fprintln(s.out, "[interrupted]") //nolint:errcheck
	}

	// In TTY mode bubbletea streamed the accordion view in real time on
	// stderr. Only print the final assistant message in non-TTY mode
	// (where bubbletea's renderer is disabled), and only when the turn
	// was not interrupted.
	if s.turnResult.Message != "" && !s.isTTY && !interrupted {
		fmt.Fprintf(s.out, "AI: %s\n", s.turnResult.Message) //nolint:errcheck
	}

	if !interrupted && s.turnErr == nil {
		s.assembly.RecordCompletedMemory(s.spanCtx, s.turnResult, preparedSnapshot, s.line)
	}
	if s.assembly.durableSession != nil {
		s.extractMemory()
	} else {
		s.persistSession()
	}

	s.logger.Info("cli_interactive_turn_complete",
		"op", "cli.interactive.turn_complete",
	)
	return nil
}

func (s *REPLSession) prepareDurableTurn() (
	context.Context,
	*session.PreparedSessionSnapshot,
	*tui.CanonicalTranscriptSnapshot,
	*session.SessionRunCommitSink,
	error,
) {
	if s.assembly == nil || s.assembly.durableSession == nil {
		return s.turnCtx, nil, nil, nil, nil
	}
	runCtx, sink, err := s.assembly.durableSession.bindRun(s.turnCtx)
	if err != nil {
		return nil, nil, nil, nil, &durableRunBindingError{cause: err}
	}
	if err := hydrateDurableRunHistory(s.assembly.Agent, sink); err != nil {
		sink.CloseRunCommit()
		return nil, nil, nil, nil, &durableRunBindingError{cause: err}
	}
	prepared, err := s.assembly.durableSession.snapshotSelected()
	if err != nil {
		sink.CloseRunCommit()
		return nil, nil, nil, nil, fmt.Errorf("interactive: prepare initial durable transcript: %w", err)
	}
	transcript, err := ProjectPreparedTranscript(s.spanCtx, prepared)
	if err != nil {
		sink.CloseRunCommit()
		return nil, nil, nil, nil, fmt.Errorf("interactive: project initial durable transcript: %w", err)
	}
	return runCtx, &prepared, &transcript, sink, nil
}

func (s *REPLSession) finishTurnSetupFailure(err error) {
	if s.cancelTurn != nil {
		s.cancelTurn()
	}
	if s.interrupter != nil {
		s.interrupter.Stop()
	}
	if s.turnSpan != nil {
		s.turnSpan.SetStatus(tracing.SpanStatusError, err.Error())
		s.turnSpan.End()
	}
	s.stopTUI()
}

func (s *REPLSession) stopTUI() {
	s.unbindTodoUpdates()
	if s.cancelTurn != nil {
		s.cancelTurn()
	}
	if s.app != nil {
		s.app.Quit()
		if s.appStarted {
			<-s.app.Done()
		}
		if s.cancelUI != nil {
			s.cancelUI()
		}
	}
	if s.eventSource != nil {
		<-s.eventSource.Done()
	}
}

func (s *REPLSession) finishTurnSpan(err error) {
	if s.turnSpan == nil {
		return
	}
	if err != nil {
		s.turnSpan.SetStatus(tracing.SpanStatusError, err.Error())
	} else {
		s.turnSpan.SetStatus(tracing.SpanStatusOK, "")
	}
	s.turnSpan.End()
}

func (s *REPLSession) finishTurnSpanAfterCompletion(interrupted bool) error {
	var completionErr error
	if s.assembly.durableSession != nil && !durableTurnIsJoinable(s.turnResult) {
		status := s.turnResult
		if status.StatusHandle != nil {
			fresh := status.StatusHandle.Snapshot()
			status.Outcome = fresh.Outcome
			status.ExecutionState = fresh.ExecutionState
		}
		completionErr = fmt.Errorf("interactive: durable run did not reach a known joined terminal state (outcome=%s execution=%s)", status.Outcome, status.ExecutionState)
	} else if s.assembly.durableSession != nil && s.turnResult.DurablePublication.State == core.DurablePublicationFailed {
		completionErr = errors.New("interactive: durable run finished without positive transcript publication")
	}
	if completionErr != nil {
		completionErr = errors.Join(completionErr, s.turnErr)
	}

	spanErr := completionErr
	if spanErr == nil {
		if s.turnErr != nil {
			spanErr = s.turnErr
		} else if interrupted {
			spanErr = context.Canceled
		}
	}
	s.finishTurnSpan(spanErr)
	return completionErr
}

func durableTurnIsJoinable(result core.Result) bool {
	if result.StatusHandle != nil {
		status := result.StatusHandle.Snapshot()
		result.Outcome = status.Outcome
		result.ExecutionState = status.ExecutionState
	}
	return result.ExecutionState == core.RunExecutionQuiescent && result.Outcome != core.RunOutcomeUnknown
}

func (s *REPLSession) applyDurableTurnRefresh(initial *session.PreparedSessionSnapshot) (refreshErr error) {
	defer func() {
		if refreshErr != nil {
			// Publication failures remain fatal. Keep the run's typed cause so
			// callers can distinguish denial, timeout, and persistence failures.
			refreshErr = errors.Join(refreshErr, s.turnErr)
		}
	}()
	if initial == nil || s.app == nil || s.uiCtx == nil {
		return errors.New("interactive: durable transcript refresh requires its initial snapshot and active TUI")
	}
	result := s.turnResult
	publication := result.DurablePublication
	if publication.State == "" {
		publication.State = core.DurablePublicationUnbound
	}
	var status core.RunStatus
	var waitErr error
	if result.StatusHandle != nil {
		waiter, ok := result.StatusHandle.(core.RunStatusWaiter)
		if !ok {
			status = result.StatusHandle.Snapshot()
			waitErr = errors.New("interactive: durable run owner cannot be joined through its status handle")
		} else {
			waitCtx, cancel := context.WithTimeout(s.spanCtx, durableOwnerJoinWait)
			status, publication, waitErr = waiter.WaitForCompletion(waitCtx)
			cancel()
		}
	} else if publication.State != core.DurablePublicationUnbound {
		waitErr = errors.New("interactive: durable publication has no owner status handle")
	}
	if waitErr == nil && publication.State == core.DurablePublicationSynced {
		recovery, ok, err := s.unknownCancellationRecovery(initial, status, publication)
		if err != nil {
			return err
		}
		if ok {
			handler := s.durableSessionCommandHandler()
			if handler == nil {
				return errors.New("interactive: durable unknown cancellation recovery requires the /session command")
			}
			s.unknownCancelRecovery = recovery
			handler.recoveryNewSession = s.startNewSessionAfterUnknownCancellation
			return nil
		}
	}

	var sourceSnapshot session.PreparedSessionSnapshot
	var transcript tui.CanonicalTranscriptSnapshot
	if waitErr == nil && publication.State == core.DurablePublicationSynced {
		if status.ExecutionState != core.RunExecutionQuiescent || status.Outcome == core.RunOutcomeUnknown ||
			publication.RunID == "" || publication.RunID != result.RunID ||
			publication.SessionID != initial.SessionID || publication.OwnerGeneration == "" {
			return fmt.Errorf(
				"interactive: owner publication did not match the joined durable run (quiescent=%t outcome_known=%t run_matches=%t session_matches=%t owner_generation_present=%t)",
				status.ExecutionState == core.RunExecutionQuiescent,
				status.Outcome != core.RunOutcomeUnknown,
				publication.RunID != "" && publication.RunID == result.RunID,
				publication.SessionID == initial.SessionID,
				publication.OwnerGeneration != "",
			)
		}
		var err error
		sourceSnapshot, err = s.assembly.durableSession.snapshotSelected()
		if err != nil {
			return fmt.Errorf("interactive: refresh durable session after joined run: %w", err)
		}
		if err := validateSamePreparedTranscript(*initial, sourceSnapshot); err != nil {
			return err
		}
		transcript, err = ProjectPreparedTranscript(s.spanCtx, sourceSnapshot)
		if err != nil {
			return fmt.Errorf("interactive: project joined durable transcript: %w", err)
		}
	} else {
		if publication.State != core.DurablePublicationUnbound &&
			(publication.RunID == "" || publication.SessionID != initial.SessionID || publication.OwnerGeneration == "") {
			return errors.New("interactive: pending durable publication has an incomplete run binding")
		}
		sourceSnapshot = initial.Clone()
		var err error
		transcript, err = ProjectPreparedTranscript(s.spanCtx, sourceSnapshot)
		if err != nil {
			return fmt.Errorf("interactive: retain initial durable transcript while publication is pending: %w", err)
		}
	}

	refresh := tui.TranscriptRefresh{Snapshot: transcript, Publication: publication}
	if result.StatusHandle != nil {
		resultRunID := publication.RunID
		if resultRunID == "" {
			resultRunID = result.RunID
		}
		observation, err := ProjectRunObservation(s.spanCtx, sourceSnapshot, result.StatusHandle, resultRunID)
		if err != nil {
			return fmt.Errorf("interactive: observe durable run owner: %w", err)
		}
		observation.OwnerGeneration = publication.OwnerGeneration
		refresh.Observation = observation
	}
	if err := s.app.ApplyTranscriptRefresh(s.uiCtx, refresh); err != nil {
		return fmt.Errorf("interactive: wait for canonical transcript apply acknowledgement: %w", err)
	}

	if publication.State == core.DurablePublicationSynced && waitErr == nil {
		if err := s.applyDurableSessionSnapshot(sourceSnapshot); err != nil {
			return err
		}
		return nil
	}
	if waitErr != nil {
		return fmt.Errorf("interactive: durable owner remains unjoined; publication is %s: %w", publication.State, waitErr)
	}
	if publication.State == core.DurablePublicationFailed {
		return errors.New("interactive: durable run finished without positive transcript publication")
	}
	return fmt.Errorf("interactive: durable run publication is %s; the selected transcript was not advanced", publication.State)
}

func validateSamePreparedTranscript(initial, refreshed session.PreparedSessionSnapshot) error {
	if initial.SessionID == "" || initial.ActiveBranchID == "" ||
		refreshed.SessionID != initial.SessionID || refreshed.ActiveBranchID != initial.ActiveBranchID {
		return errors.New("interactive: durable refresh changed the prepared physical session or branch")
	}
	return nil
}

func (s *REPLSession) applyDurableSessionSnapshot(snapshot session.PreparedSessionSnapshot) error {
	tree, err := session.NewDefaultSessionTreeBuilder().BuildFromSnapshot(s.spanCtx, snapshot)
	if err != nil {
		return fmt.Errorf("interactive: rebuild durable session tree after turn: %w", err)
	}
	s.sessionTree = tree
	if s.assembly.durableSession != nil {
		s.sessionHandler = session.NewDurableReadOnlySessionSlashHandler(tree)
		s.slashCtx.sessionHandler = s.sessionHandler
	}
	s.assembly.Agent.SetHistory(snapshot.PublishedView.Messages)
	s.entryCounter = len(s.assembly.Agent.Messages())
	return nil
}

func (s *REPLSession) unknownCancellationRecovery(
	initial *session.PreparedSessionSnapshot,
	joined core.RunStatus,
	publication core.DurablePublication,
) (*durableUnknownCancelRecovery, bool, error) {
	if initial == nil || s == nil || s.spanCtx == nil || s.turnCtx == nil ||
		s.assembly == nil || s.assembly.durableSession == nil || s.assembly.durableSession.manager == nil {
		return nil, false, nil
	}
	result := s.turnResult
	guard, ok := s.assembly.runSlotGuard.(core.RunSlotWrapperRegistrar)
	if !ok {
		return nil, false, nil
	}
	_, runSlotActive := guard.ActiveRunStatus()
	evidence := unknownCancellationOwnerEvidence{
		explicitUICancel: s.uiCancelRequested.Load(),
		parentContextErr: s.spanCtx.Err(), turnContextErr: s.turnCtx.Err(),
		// applyDurableTurnRefresh only reaches this method after WaitForCompletion returns nil.
		joinErr: nil, statusHandle: result.StatusHandle != nil,
		resultRunID: result.RunID, initialSessionID: initial.SessionID,
		joinedStatus: joined, publication: publication, runSlotActive: runSlotActive,
	}
	if !validUnknownCancellationOwnerEvidence(evidence) {
		return nil, false, nil
	}
	snapshot, err := s.assembly.durableSession.snapshotSelected()
	if err != nil {
		return nil, false, fmt.Errorf("interactive: inspect unknown canceled durable run: %w", err)
	}
	if err := validateSamePreparedTranscript(*initial, snapshot); err != nil {
		return nil, false, err
	}
	observation, err := ProjectRunObservation(s.spanCtx, snapshot, result.StatusHandle, result.RunID)
	if err != nil {
		return nil, false, fmt.Errorf("interactive: inspect unknown canceled run terminal: %w", err)
	}
	if observation.RunID != joined.RunID || observation.ExecutionState != core.RunExecutionQuiescent ||
		observation.Outcome != core.RunOutcomeUnknown || observation.TerminalEntryID == "" ||
		observation.TerminalOutcome != core.RunOutcomeUnknown {
		return nil, false, nil
	}
	recovery := &durableUnknownCancelRecovery{
		sessionID: initial.SessionID, activeBranchID: initial.ActiveBranchID, runID: result.RunID,
		ownerGeneration: publication.OwnerGeneration, terminalEntryID: observation.TerminalEntryID,
	}
	if err := validateUnknownCancellationRecoverySelection(recovery, result.RunID, joined, publication, snapshot, observation); err != nil {
		return nil, false, nil
	}
	return recovery, true, nil
}

func (s *REPLSession) durableSessionCommandHandler() *SessionHandler {
	if s.slashReg == nil {
		return nil
	}
	registered, ok := s.slashReg.Lookup("session")
	if !ok {
		return nil
	}
	handler, _ := registered.(*SessionHandler)
	return handler
}

func (s *REPLSession) startNewSessionAfterUnknownCancellation(ctx context.Context) (string, error) {
	recovery := s.unknownCancelRecovery
	if recovery == nil || s.assembly == nil || s.assembly.durableSession == nil || s.assembly.durableSession.manager == nil {
		return "", errors.New("/session new is available only after a quiescent unknown cancellation")
	}
	guard, ok := s.assembly.runSlotGuard.(core.RunSlotWrapperRegistrar)
	if !ok {
		return "", errors.New("cannot verify that the previous run slot is idle")
	}
	if _, active := guard.ActiveRunStatus(); active {
		return "", errors.New("cannot start a new session while a run owner is active")
	}
	if s.turnResult.StatusHandle == nil || s.turnResult.RunID != recovery.runID {
		return "", errors.New("the interrupted run no longer matches the saved recovery binding")
	}
	waiter, ok := s.turnResult.StatusHandle.(core.RunStatusWaiter)
	if !ok {
		return "", errors.New("cannot revalidate the joined owner publication for /session new")
	}
	waitCtx, cancel := context.WithTimeout(normalizeSessionContext(ctx), durableOwnerJoinWait)
	status, publication, waitErr := waiter.WaitForCompletion(waitCtx)
	cancel()
	if waitErr != nil {
		return "", fmt.Errorf("rejoin the interrupted run before starting a new session: %w", waitErr)
	}
	selected, err := s.assembly.durableSession.snapshotSelected()
	if err != nil {
		return "", fmt.Errorf("inspect the selected durable session before starting a new one: %w", err)
	}
	observation, err := ProjectRunObservation(ctx, selected, s.turnResult.StatusHandle, recovery.runID)
	if err != nil {
		return "", fmt.Errorf("revalidate the unknown terminal checkpoint before starting a new session: %w", err)
	}
	if err := validateUnknownCancellationRecoverySelection(recovery, s.turnResult.RunID, status, publication, selected, observation); err != nil {
		return "", err
	}
	if err := s.assembly.durableSession.manager.prepareNew(ctx, "interactive after unknown cancellation"); err != nil {
		return "", fmt.Errorf("prepare a new durable session after unknown cancellation: %w", err)
	}
	if _, active := guard.ActiveRunStatus(); active {
		return "", errors.New("a run owner became active while starting a new session")
	}
	snapshot, err := s.assembly.durableSession.snapshotSelected()
	if err != nil {
		return "", fmt.Errorf("inspect the newly selected durable session: %w", err)
	}
	if snapshot.SessionID == recovery.sessionID || len(snapshot.PublishedView.Messages) != 0 {
		return "", errors.New("new durable session did not select a fresh empty physical session")
	}
	tree, err := session.NewDefaultSessionTreeBuilder().BuildFromSnapshot(ctx, snapshot)
	if err != nil {
		return "", fmt.Errorf("build the new durable session tree: %w", err)
	}
	s.sessionTree = tree
	s.sessionHandler = session.NewDurableReadOnlySessionSlashHandler(tree)
	s.slashCtx.sessionHandler = s.sessionHandler
	s.assembly.Agent.SetHistory(snapshot.PublishedView.Messages)
	s.assembly.PhysicalSessionID = snapshot.SessionID
	s.slashCtx.sessionID = snapshot.SessionID
	s.entryCounter = len(s.assembly.Agent.Messages())
	s.unknownCancelRecovery = nil
	if handler := s.durableSessionCommandHandler(); handler != nil {
		handler.recoveryNewSession = nil
	}
	return snapshot.SessionID, nil
}

func allowedDuringUnknownCancellation(command session.SlashCommand, registry *SlashCommandRegistry) bool {
	if registry == nil {
		return false
	}
	registered, ok := registry.Lookup(command.Name)
	if !ok {
		return false
	}
	switch handler := registered.(type) {
	case *HelpHandler:
		return handler != nil
	case *SessionHandler:
		if handler == nil || handler.mode != sessionCommandModeDurable || len(command.Args) != 1 {
			return false
		}
		switch command.Args[0] {
		case "pending":
			return true
		case "new":
			return handler.recoveryNewSession != nil
		default:
			return false
		}
	default:
		return false
	}
}

func printUnknownCancellationFence(out io.Writer) {
	fmt.Fprintln(out, "The interrupted run is unresolved; no new model request was sent. Use /session pending, /help, /session new, or exit.") //nolint:errcheck
}

// setupTurn creates the cancelable turn context, interrupt handler, and
// event stream, and expands @-mentions in the user input.
func (s *REPLSession) setupTurn() {
	s.uiCancelRequested.Store(false)
	s.turnSpan, s.turnCtx = tracing.SpanFromContext(s.spanCtx, spanInteractiveTurn, tracing.SpanKindInternal)
	s.turnSpan.SetAttributes(
		tracing.SensitiveAttribute("user_message", s.line),
		tracing.Attribute{Key: "user_message_length", Value: len(s.line)},
	)

	// Expand @-mentions before processing. The SensitiveAttribute above
	// already captured the original user input; Expand modifies line so
	// the inlined file content reaches the agent via RunTurn.
	if s.mentionExpander != nil {
		mentionSpan, mentionCtx := tracing.SpanFromContext(s.turnCtx, spanInteractiveMentionExpand, tracing.SpanKindInternal)
		expanded, files, totalBytes, mErr := s.mentionExpander.Expand(mentionCtx, s.line)
		if mErr == nil {
			mentionSpan.SetAttributes(
				tracing.Attribute{Key: "mention.count", Value: len(files)},
				tracing.Attribute{Key: "mention.files", Value: strings.Join(files, ",")},
				tracing.Attribute{Key: "mention.total_bytes", Value: totalBytes},
			)
			if expanded != s.line {
				s.line = expanded
			}
		}
		mentionSpan.End()
	}

	// Wrap the turn context in a cancellable context so the user can
	// interrupt the in-progress agent turn via Ctrl+C (non-TTY) or Esc
	// (TTY). The InterruptHandler monitors for SIGINT and invokes
	// cancelTurn when a signal arrives. Esc-key detection is handled by
	// the TUI keyboardLoop (keyboard.go), which uses a 50ms timeout to
	// distinguish standalone Esc from CSI sequences and calls
	// cancelCallback -> cancelTurn. nil is passed to Start() because the
	// TUI already owns stdin in raw mode; a second reader would steal
	// bytes from the keyboardLoop.
	s.turnCtx, s.cancelTurn = context.WithCancel(s.turnCtx)
	s.interrupter = NewInterruptHandler(s.cancelTurn)
	s.interrupter.Start(nil)

	// Create an EventStream for this turn and wire it to the TurnRunner
	// so events are streamed in real time to the TUI. RunTurn is
	// blocking, so it runs in a goroutine that closes the stream when
	// the turn finishes. DiscardOldest prevents goroutine leaks if the
	// TUI consumer falls behind: old events are evicted rather than
	// blocking the agent loop indefinitely. Buffer is 256 so that
	// 200+ events (including all tool_result events) fit without
	// being discarded under normal load.
	s.stream = core.NewEventStream(256, core.WithEventDiscardPolicy(core.DiscardOldest))
	s.assembly.TurnRunner.SetStream(s.stream)
}

// runAgentGoroutine launches the RunTurn goroutine that processes the user
// submission and streams events.
func (s *REPLSession) runAgentGoroutine(runCtx context.Context) {
	s.turnDone = make(chan struct{})
	go func() {
		defer close(s.turnDone)
		s.turnResult, s.turnErr = s.assembly.TurnRunner.RunTurn(runCtx, core.Submission{
			Type:    core.SubmissionUserMessage,
			Content: s.line,
		})

		// Emit a token_usage event from CostTracker and estimator data
		// so the TUI status bar updates before the stream closes.
		emitTokenUsageEvent(s.stream, s.assembly)

		// Set the result and send a done event, matching the harness
		// behavior so downstream consumers (bridge, TUI) observe
		// completion correctly.
		s.stream.SetResult(core.AgentMessage{Role: "assistant", Content: s.turnResult.Message}, s.turnErr)
		if s.turnErr == nil {
			_ = s.stream.Send(core.AgentEvent{Kind: "done", Content: s.turnResult.Message, Timestamp: time.Now()}) //nolint:errcheck
		}
		s.stream.Close()
	}()
}

// waitForTurnCompletion waits for the render loop to finish, forwarding any
// steer messages that arrive during the wait.
func (s *REPLSession) waitForTurnCompletion() error {
	durableApp := s.assembly != nil && s.assembly.durableSession != nil
	turnDone := s.turnDone
	var bridgeDone <-chan struct{}
	if s.eventSource != nil {
		bridgeDone = s.eventSource.Done()
	}
	var eventSourceDone <-chan struct{}
	var appDone <-chan struct{}
	if s.app != nil {
		eventSourceDone = s.app.EventSourceDone()
		appDone = s.app.Done()
	}
	if s.waitForTUICompletion(turnDone, bridgeDone, eventSourceDone, appDone, durableApp) {
		return errors.New("interactive: TUI exited before its event source and durable refresh completed")
	}
	if durableApp && s.app != nil {
		select {
		case <-s.app.Done():
			return errors.New("interactive: TUI exited before its durable refresh completed")
		default:
		}
	}
	return nil
}

// waitForTUICompletion joins the turn and bridge, and waits for the model event
// source while the TUI can still acknowledge it. After the app exits, it drops
// that UI-owned acknowledgment barrier, but still joins the core turn and
// bridge. It returns true for an unexpected early exit.
func (s *REPLSession) waitForTUICompletion(turnDone, bridgeDone, eventSourceDone, appDone <-chan struct{}, durableApp bool) bool {
	turnCtxDone := s.turnCtx.Done()
	var steerMessages <-chan string
	if s.interrupter != nil {
		steerMessages = s.interrupter.SteerChannel()
	}
	var appExitedEarly bool
	var sourceClosed bool
	for turnDone != nil || bridgeDone != nil || eventSourceDone != nil || (!durableApp && appDone != nil) {
		select {
		case <-turnDone:
			turnDone = nil
		case <-bridgeDone:
			bridgeDone = nil
		case <-eventSourceDone:
			eventSourceDone = nil
			sourceClosed = true
		case <-appDone:
			appDone = nil
			if !sourceClosed && eventSourceDone != nil {
				select {
				case <-eventSourceDone:
					eventSourceDone = nil
					sourceClosed = true
				default:
				}
			}
			if durableApp || !sourceClosed {
				// The app can be stopped by the parent invocation context. In a
				// legacy non-durable REPL that is a normal shutdown path; in the
				// durable path it still forbids a refresh because no live model is
				// available to acknowledge it.
				parentCanceledUI := s.uiCtx != nil && s.uiCtx.Err() != nil
				cancelRequested := s.turnCtx.Err() != nil
				// Escape cancels the turn and closes the legacy TUI. Its app-done
				// notification may arrive before the owned bridge and RunTurn have
				// observed cancellation. That is an expected UI exit, but the loop
				// below must still join those owners before executeTurn continues.
				// A durable app remains an error here because its transcript refresh
				// has not been acknowledged.
				legacyCancelExit := !durableApp && cancelRequested
				appExitedEarly = (!parentCanceledUI || durableApp) && !legacyCancelExit
				eventSourceDone = nil
				if s.cancelTurn != nil {
					s.cancelTurn()
				}
			}
		case <-turnCtxDone:
			turnCtxDone = nil
		case steerMsg, ok := <-steerMessages:
			if !ok {
				steerMessages = nil
			} else if s.turnCtx.Err() == nil {
				s.handleSteer(steerMsg)
			}
		}
	}
	return appExitedEarly
}

// handleSteer forwards a steer message to the TurnRunner or the shared
// steer channel.
func (s *REPLSession) handleSteer(steerMsg string) {
	s.logger.Info("cli_interactive_steer",
		"op", "cli.interactive.steer",
		"message", steerMsg,
	)
	turnID := s.assembly.TurnRunner.RunningTurnID()
	if turnID != "" {
		if steerErr := s.assembly.TurnRunner.Steer(s.turnCtx, turnID, steerMsg); steerErr != nil {
			s.logger.Warn("cli_interactive_steer_failed", "err", steerErr)
		} else {
			s.logger.Info("cli_interactive_steer_forwarded", "message", steerMsg)
		}
	} else if s.assembly.SteerChannel != nil {
		select {
		case s.assembly.SteerChannel <- steerMsg:
			s.logger.Info("cli_interactive_steer_forwarded", "message", steerMsg)
		default:
			s.logger.Warn("cli_interactive_steer_channel_full")
		}
	}
}

// persistSession appends user and assistant entries to the session store and
// tree, saves the store, and invokes an explicitly configured memory extractor
// when present. The default CLI assembly leaves that extractor unset. Entries
// are buffered through PendingSessionWrites and flushed in a single batch,
// reducing the number of individual store writes.
func (s *REPLSession) persistSession() {
	// Persist to session store (even on interruption to preserve history).
	// Use spanCtx, not turnCtx, because turnCtx may be canceled by the
	// interrupt handler.
	if s.assembly.SessionStore == nil {
		return
	}
	parentID := ""
	if s.sessionTree != nil {
		parentID = s.sessionTree.CurrentLeaf()
	}
	userEntry := &session.SessionEntry{
		ID:        newEntryID(),
		ParentID:  parentID,
		Type:      session.EntryTypeUser,
		Content:   s.line,
		Timestamp: time.Now(),
	}
	// Buffer the write through PendingSessionWrites instead of appending
	// directly to the store.
	s.pendingWrites.Enqueue(session.SessionWrite{
		SessionID: s.assembly.SessionID,
		Entry:     *userEntry,
	})
	if s.sessionTree != nil {
		if treeErr := s.sessionTree.Append(s.spanCtx, userEntry); treeErr != nil {
			s.logger.Warn("cli_interactive_session_tree_append_failed", "err", treeErr)
		}
	}
	if s.turnResult.Message != "" {
		parentID := ""
		if s.sessionTree != nil {
			parentID = s.sessionTree.CurrentLeaf()
		}
		assistantEntry := &session.SessionEntry{
			ID:        newEntryID(),
			ParentID:  parentID,
			Type:      session.EntryTypeAssistant,
			Content:   s.turnResult.Message,
			Timestamp: time.Now(),
		}
		s.pendingWrites.Enqueue(session.SessionWrite{
			SessionID: s.assembly.SessionID,
			Entry:     *assistantEntry,
		})
		if s.sessionTree != nil {
			if treeErr := s.sessionTree.Append(s.spanCtx, assistantEntry); treeErr != nil {
				s.logger.Warn("cli_interactive_session_tree_append_failed", "err", treeErr)
			}
		}
	}
	// Flush all buffered entries to the store in a single batch.
	if s.pendingWrites != nil {
		if flushErr := s.pendingWrites.Flush(s.spanCtx, s.assembly.SessionStore); flushErr != nil {
			s.logger.Warn("cli_interactive_session_save_failed", "err", flushErr)
		}
	}
	_ = s.assembly.SessionStore.Save(s.spanCtx) //nolint:errcheck

	s.extractMemory()
}

// extractMemory runs an explicitly configured extractor asynchronously. The
// default CLI does not supply one, so conversation content is not extracted or
// persisted automatically.
func (s *REPLSession) extractMemory() {
	// An injected extractor uses a stored cancellable context (derived from
	// context.Background so it survives turn cancellation) that Cleanup can
	// cancel to promptly release goroutines. Errors are logged only and do not
	// block the main interaction loop.
	if s.assembly.MemoryExtractor == nil || s.assembly.MemoryStore == nil {
		return
	}
	// Lazily initialize the cancellable memory context on first use.
	if s.memoryCtxCancel == nil {
		s.memoryCtx, s.memoryCtxCancel = context.WithCancel(context.Background())
	}
	agentMsgs := s.assembly.Agent.Messages()
	msgs := make([]llm.Message, 0, len(agentMsgs))
	for _, m := range agentMsgs {
		msgs = append(msgs, llm.Message{
			Role:    llm.Role(m.Role),
			Content: m.Content,
		})
	}
	memStore := s.assembly.MemoryStore
	extractor := s.assembly.MemoryExtractor
	memCtx := s.memoryCtx
	s.assembly.MemoryWG.Add(1)
	go func() {
		defer s.assembly.MemoryWG.Done()
		ctx, cancel := context.WithTimeout(memCtx, 30*time.Second)
		defer cancel()
		extracted, err := extractor.Extract(ctx, msgs)
		if err != nil {
			s.logger.Warn("cli_interactive_memory_extract_failed", "err", err)
			return
		}
		for _, mem := range extracted {
			if err := memStore.Add(ctx, mem); err != nil {
				s.logger.Warn("cli_interactive_memory_store_failed", "err", err)
			}
		}
	}()
}

// cleanup saves the line editor history, stops the editor, and prints the
// session-ended message.
func (s *REPLSession) cleanup() {
	// Stop the background flush goroutine first to prevent concurrent
	// flushes during the final drain.
	s.stopFlushTicker()

	// Save history on exit (covers EOF, /exit, and exit text paths).
	if dle, ok := s.lineEditor.(*DefaultLineEditor); ok {
		if hs := dle.HistoryStore(); hs != nil {
			_ = hs.Save()
		}
		dle.Stop()
	}

	fmt.Fprintln(s.out, "Session ended.") //nolint:errcheck
}
