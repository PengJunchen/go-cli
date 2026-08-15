package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/tui"
)

// REPLSession holds the state of an interactive read-eval-print loop session.
// It is constructed by interactiveCmd.Run and encapsulates all session-level
// and turn-level state that was previously spread across local variables in
// the monolithic Run method.
type REPLSession struct {
	// cmd is the owning interactiveCmd, kept for handleSlashCommand.
	cmd *interactiveCmd

	// I/O
	out io.Writer
	in  io.Reader

	// Configuration
	cfg  Config
	args []string
	rc   *config.Config

	// Flags
	modelName     string
	providerName  string
	maxTokens     int
	thinkingLevel llm.ThinkingLevel
	thinkingFlag  string
	verboseFlag   bool
	resumeFlag    bool
	noSandboxFlag bool

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
	sharedThemeMgr *tui.ThemeManager

	// pendingWrites buffers session entries for batched persistence,
	// reducing syscall overhead by flushing multiple entries in a single
	// pass instead of writing each one individually.
	pendingWrites *session.PendingSessionWrites

	// flushTicker periodically flushes pending writes as a safety net.
	flushTicker *time.Ticker
	flushDone   chan struct{}

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
	line        string
	turnSpan    tracing.TraceSpan
	turnCtx     context.Context
	cancelTurn  context.CancelFunc
	interrupter *InterruptHandler
	stream      *core.EventStreamImpl
	turnResult  core.Result
	turnErr     error
	turnDone    chan struct{}
	isTTY       bool
	app         *tui.BubbleteaApp
}

// Run starts the session: it parses flags, assembles the agent, enters the
// REPL loop, and cleans up on exit.
func (s *REPLSession) Run(ctx context.Context, cfg Config, args []string) error {
	s.cfg = cfg
	s.args = args
	if err := s.start(ctx); err != nil {
		return err
	}
	defer func() {
		s.span.SetStatus(tracing.SpanStatusOK, "")
		s.span.End()
	}()
	defer s.assembly.Cleanup()
	s.readInput()
	s.cleanup()
	return nil
}

// start performs all one-time session setup: flag parsing, agent assembly,
// slash-context wiring, and editor/mention-expander initialization.
func (s *REPLSession) start(ctx context.Context) error {
	if err := s.parseFlags(); err != nil {
		return err
	}
	if err := s.setupAgent(ctx); err != nil {
		return err
	}
	s.setupSlashContext()
	s.setupEditors()
	return nil
}

// parseFlags parses the interactive command-line flags and resolves the
// model/provider/config from the environment.
func (s *REPLSession) parseFlags() error {
	fs := flag.NewFlagSet("interactive", flag.ContinueOnError)
	fs.SetOutput(s.out)

	var (
		modelFlag     string
		providerFlag  string
		maxTokensFlag int
		verboseFlag   bool
		resumeFlag    bool
		thinkingFlag  string
		noSandboxFlag bool
	)
	fs.StringVar(&modelFlag, "model", "", "model name to use")
	fs.StringVar(&providerFlag, "provider", "", "provider name to use")
	fs.IntVar(&maxTokensFlag, "max-tokens", defaultMaxTokens, "compaction token budget")
	fs.BoolVar(&verboseFlag, "verbose", false, "enable verbose output")
	fs.BoolVar(&resumeFlag, "resume", false, "resume previous session from store path")
	fs.StringVar(&thinkingFlag, "thinking", "medium", "thinking level: none|minimal|low|medium|high|max")
	fs.BoolVar(&noSandboxFlag, "no-sandbox", false, "disable bash sandbox enforcement")
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

	// Build the dynamic slash command registry: built-in commands plus any
	// custom Markdown commands from .go-cli/commands/. Built-in commands take
	// priority over custom commands with the same name.
	s.slashReg = buildDynamicRegistry(s.rc)
	if s.cmd != nil {
		s.cmd.slashReg = s.slashReg
	}

	s.modelName = resolveModelName(modelFlag, s.rc)
	s.providerName = resolveProviderName(providerFlag, s.rc)
	if maxTokensFlag <= 0 {
		maxTokensFlag = defaultMaxTokens
	}
	s.maxTokens = maxTokensFlag
	s.thinkingLevel = thinkingLevel
	s.thinkingFlag = thinkingFlag
	s.verboseFlag = verboseFlag
	s.resumeFlag = resumeFlag
	s.noSandboxFlag = noSandboxFlag
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
	assembleOpts := []AssembleOption{
		WithMaxTokens(s.maxTokens),
		WithResume(s.resumeFlag),
		WithSessionPersistence(true),
		WithAgentName("interactive"),
		WithThinkingLevel(s.thinkingLevel),
	}
	if s.noSandboxFlag {
		assembleOpts = append(assembleOpts, WithNoSandbox())
	}
	if tui.IsTerminal() {
		approvalCh = make(chan tui.ApprovalRequest, 32)
		assembleOpts = append(assembleOpts, WithApprovalChannel(approvalCh))
	}
	assembly, err := AssembleAgent(s.spanCtx, s.rc, s.providerName, s.modelName, s.out,
		assembleOpts...)
	if err != nil {
		s.span.SetStatus(tracing.SpanStatusError, err.Error())
		s.span.End()
		return newExecutionError("interactive: assemble agent", err)
	}
	s.assembly = assembly
	s.approvalCh = approvalCh
	return nil
}

// setupSlashContext builds the slash command context from the assembled
// components and initializes the shared theme manager.
func (s *REPLSession) setupSlashContext() {
	// Build slash command context from the assembled components.
	if s.assembly.SessionStore != nil {
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
	}
	s.slashCtx = slashContext{
		agent:           s.assembly.Agent,
		costTracker:     s.assembly.CostTracker,
		statsRegistry:   s.assembly.StatsRegistry,
		sessionID:       s.assembly.SessionID,
		toolRegistry:    s.assembly.ToolRegistry,
		modelName:       s.modelName,
		sessionHandler:  s.sessionHandler,
		out:             s.out,
		config:          s.rc,
		sessionStore:    s.assembly.SessionStore,
		fileTracker:     s.assembly.FileTracker,
		diffGenerator:   s.assembly.DiffGenerator,
		planCtrl:        s.assembly.PlanCtrl,
		memoryStore:     s.assembly.MemoryStore,
		contextWindow:   s.assembly.ContextWindow,
		worktreeManager: s.assembly.WorktreeManager,
		snapshotManager: s.assembly.SnapshotMgr,
		modelSelector:   s.assembly.ModelSelector,
		estimator:       s.assembly.Estimator,
		promptBuilder:   s.assembly.PromptBuilder,
		contextLoader:   s.assembly.ContextLoader,
	}

	// Create a shared ThemeManager so /theme can switch themes at runtime
	// without restarting. The initial theme is set from config; subsequent
	// switches via /theme persist across turns because the same manager is
	// passed to every BubbleteaApp via WithThemeManager.
	s.sharedThemeMgr = tui.NewThemeManager()
	if s.rc != nil {
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
		if s.assembly.SessionStore != nil {
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
	s.flushTicker = ticker
	s.flushDone = done
	// Snapshot fields to avoid concurrent access from the ticker goroutine.
	pw := s.pendingWrites
	store := s.assembly.SessionStore
	ctx := s.spanCtx
	logger := s.logger
	go func() {
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
	if s.flushTicker != nil {
		s.flushTicker.Stop()
	}
	if s.flushDone != nil {
		close(s.flushDone)
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
func (s *REPLSession) readInput() {
	for {
		line, err := s.lineEditor.ReadLine(s.spanCtx, "> ")
		if err != nil {
			if errors.Is(err, errInterrupted) {
				if s.handleInterrupt(err) {
					break
				}
				continue
			}
			if !errors.Is(err, io.EOF) {
				s.logger.Warn("cli_interactive_readline_error", "err", err)
			}
			break
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

		s.line = line
		s.executeTurn()
	}
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
func (s *REPLSession) executeTurn() {
	s.setupTurn()
	s.runAgentGoroutine()
	s.renderTUI()
	s.waitForTurnCompletion()

	// Clear the HITL program reference now that the turn is complete.
	// The bubbletea program from this turn has exited; leaving a stale
	// reference would cause the next turn to send to a dead program.
	if s.assembly.HITLEmitter != nil {
		s.assembly.HITLEmitter.SetProgram(nil)
	}

	// Wait for RunTurn to finish so turnResult and turnErr are populated.
	<-s.turnDone

	interrupted := s.turnCtx.Err() != nil
	s.cancelTurn()
	s.interrupter.Stop()
	s.turnSpan.SetStatus(tracing.SpanStatusOK, "")
	s.turnSpan.End()

	s.app.Quit()

	if s.turnErr != nil && !interrupted {
		fmt.Fprintf(s.out, "Error: %v\n", s.turnErr) //nolint:errcheck
		return
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

	s.persistSession()

	s.logger.Info("cli_interactive_turn_complete",
		"op", "cli.interactive.turn_complete",
	)
}

// setupTurn creates the cancelable turn context, interrupt handler, and
// event stream, and expands @-mentions in the user input.
func (s *REPLSession) setupTurn() {
	s.turnSpan, s.turnCtx = tracing.SpanFromContext(s.spanCtx, spanInteractiveTurn, tracing.SpanKindInternal)
	s.turnSpan.SetAttributes(tracing.SensitiveAttribute("user_message", s.line))

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
func (s *REPLSession) runAgentGoroutine() {
	s.turnDone = make(chan struct{})
	go func() {
		defer close(s.turnDone)
		s.turnResult, s.turnErr = s.assembly.TurnRunner.RunTurn(s.turnCtx, core.Submission{
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

// renderTUI builds the TUI options, creates the BubbleteaApp, starts the
// render goroutine, and wires HITL through the bubbletea program.
func (s *REPLSession) renderTUI() {
	// Bridge core events to TUI events and render. Bubbletea owns terminal
	// rendering in TTY mode (raw mode, diff-based repaint on stderr), so
	// the legacy onUpdate manual redraw and ANSI cursor logic has been
	// removed.
	var tuiEvents <-chan tui.AgentEvent
	if s.rc != nil && s.rc.TUI.Mode == "remote" && s.rc.TUI.RemoteURL != "" {
		tuiEvents = tui.NewACPStreamAdapter(s.rc.TUI.RemoteURL).Stream(s.turnCtx)
	} else {
		tuiEvents = tui.BridgeEvents(s.turnCtx, s.stream)
	}
	s.turnCounter++
	s.isTTY = tui.IsTerminal()
	tsp := tui.NewDefaultTerminalSizeProvider()
	appOpts := []tui.AppOption{
		tui.WithWidth(tsp.Width()),
		tui.WithModelInfo(s.slashCtx.ModelName()),
		tui.WithTurnCount(s.turnCounter),
		tui.WithSessionInfo(s.assembly.SessionID),
	}
	// Mode label: show "plan" when plan mode is active, otherwise "chat".
	if s.assembly.PlanCtrl != nil && s.assembly.PlanCtrl.IsActive() {
		appOpts = append(appOpts, tui.WithModeLabel("plan"))
	} else {
		appOpts = append(appOpts, tui.WithModeLabel("chat"))
	}
	// TUIConfig: word wrap, diff style. The shared theme manager is
	// passed separately so runtime /theme switches persist across turns.
	if s.rc != nil {
		appOpts = append(appOpts,
			tui.WithWordWrap(s.rc.TUI.WordWrap),
			tui.WithDiffStyle(s.rc.TUI.DiffStyle),
		)
	}
	appOpts = append(appOpts, tui.WithThemeManager(s.sharedThemeMgr))
	if s.assembly.ApprovalChannel != nil {
		appOpts = append(appOpts, tui.WithApprovalChannel(s.assembly.ApprovalChannel))
	}
	if s.slashCtx.thinkingVisibility != "" {
		appOpts = append(appOpts, tui.WithThinkingVisibility(s.slashCtx.thinkingVisibility))
	}
	appOpts = append(appOpts, s.buildCallbackOptions()...)
	s.app = tui.NewBubbleteaApp(tuiEvents, appOpts...)

	go func() {
		if runErr := s.app.Run(s.turnCtx); runErr != nil && runErr != context.Canceled {
			s.logger.Debug("cli_interactive_tui_error", "err", runErr)
		}
	}()

	s.wireHITL()
}

// buildCallbackOptions constructs the steer, follow-up, cancel, pause, and
// resume callback options for the BubbleteaApp.
func (s *REPLSession) buildCallbackOptions() []tui.AppOption {
	return []tui.AppOption{
		tui.WithSteerCallback(func(input string) {
			turnID := s.assembly.TurnRunner.RunningTurnID()
			if turnID != "" {
				if steerErr := s.assembly.TurnRunner.Steer(s.turnCtx, turnID, input); steerErr != nil {
					s.logger.Warn("cli_interactive_steer_callback_failed", "err", steerErr)
				}
			} else if s.assembly.SteerChannel != nil {
				select {
				case s.assembly.SteerChannel <- input:
				default:
				}
			}
		}),
		tui.WithFollowUpCallback(func(input string) {
			turnID := s.assembly.TurnRunner.RunningTurnID()
			if turnID != "" {
				if err := s.assembly.TurnRunner.FollowUp(s.turnCtx, turnID, input); err != nil {
					s.logger.Warn("cli_interactive_followup_callback_failed", "err", err)
				}
			} else if s.assembly.FollowUpChannel != nil {
				select {
				case s.assembly.FollowUpChannel <- input:
				default:
					s.logger.Warn("cli_interactive_followup_channel_full")
				}
			}
		}),
		tui.WithCancelCallback(func() {
			s.cancelTurn()
		}),
		tui.WithPauseCallback(func() {
			if s.assembly.LoopAgent != nil {
				s.assembly.LoopAgent.Pause()
			}
		}),
		tui.WithResumeCallback(func() {
			if s.assembly.LoopAgent != nil {
				s.assembly.LoopAgent.Resume()
			}
		}),
	}
}

// wireHITL waits for the bubbletea program to be initialized so HITL can
// route through it.
func (s *REPLSession) wireHITL() {
	// Wait for the tea.Program to be initialized so HITL can route
	// through it. The program is created inside Run (which runs in the
	// goroutine above); instead of polling, we wait on the
	// ProgramReady channel which closes once the program is stored.
	// In TTY mode this routes HITL questions through bubbletea instead
	// of corrupting stdout.
	if s.assembly.HITLEmitter != nil {
		select {
		case <-s.app.ProgramReady():
			if prog := s.app.Program(); prog != nil {
				s.assembly.HITLEmitter.SetProgram(prog)
			}
		case <-time.After(5 * time.Second):
			s.logger.Warn("cli_interactive_hitl_program_timeout",
				"op", "cli.interactive.hitl.program_timeout",
			)
		case <-s.turnCtx.Done():
		}
	}
}

// waitForTurnCompletion waits for the render loop to finish, forwarding any
// steer messages that arrive during the wait.
func (s *REPLSession) waitForTurnCompletion() {
	// Wait for the render loop to finish. The app exits when the bridge
	// channel closes (stream closed by the RunTurn goroutine). Steer
	// messages arriving on the interrupter's channel are forwarded to
	// the TurnRunner via Steer(), which delivers them to the running
	// loop between LLM iterations.
	turnComplete := false
	for !turnComplete {
		select {
		case <-s.app.Done():
			turnComplete = true
		case <-s.turnCtx.Done():
			<-s.app.Done()
			turnComplete = true
		case steerMsg := <-s.interrupter.SteerChannel():
			s.handleSteer(steerMsg)
		}
	}
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
// tree, saves the store, and triggers memory extraction. Entries are buffered
// through PendingSessionWrites and flushed in a single batch, reducing the
// number of individual store writes.
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
		ID:        fmt.Sprintf("entry-%d", s.entryCounter),
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
	s.entryCounter++
	if s.turnResult.Message != "" {
		parentID := ""
		if s.sessionTree != nil {
			parentID = s.sessionTree.CurrentLeaf()
		}
		assistantEntry := &session.SessionEntry{
			ID:        fmt.Sprintf("entry-%d", s.entryCounter),
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
		s.entryCounter++
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

// extractMemory asynchronously extracts memories from the conversation for
// cross-session context continuity.
func (s *REPLSession) extractMemory() {
	// Asynchronously extract memories from the conversation for
	// cross-session context continuity. Uses a stored cancellable context
	// (derived from context.Background so it survives turn cancellation)
	// that Cleanup can cancel to promptly release goroutines. Errors are
	// logged only and do not block the main interaction loop.
	if s.assembly.MemoryExtractor == nil || s.assembly.MemoryStore == nil {
		return
	}
	// Lazily initialise the cancellable memory context on first use.
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

	// Flush any remaining buffered session entries and sync the store
	// before tearing down shared resources.
	if s.assembly != nil && s.assembly.SessionStore != nil && s.pendingWrites != nil {
		if flushErr := s.pendingWrites.Flush(s.spanCtx, s.assembly.SessionStore); flushErr != nil {
			s.logger.Warn("cli_interactive_session_flush_final", "err", flushErr)
		}
		_ = s.assembly.SessionStore.Save(s.spanCtx) //nolint:errcheck
	}

	// Wait for in-flight memory extraction goroutines to finish before
	// closing shared resources (MemoryStore, etc.). This ensures the
	// extraction completes and writes memories before the store is closed.
	// After the wait, cancel the memory context to release any remaining
	// resources held by the context (no-op if extraction already finished).
	if s.assembly != nil && s.assembly.MemoryWG != nil {
		s.assembly.MemoryWG.Wait()
	}
	if s.memoryCtxCancel != nil {
		s.memoryCtxCancel()
	}

	// Save history on exit (covers EOF, /exit, and exit text paths).
	if dle, ok := s.lineEditor.(*DefaultLineEditor); ok {
		if hs := dle.HistoryStore(); hs != nil {
			_ = hs.Save()
		}
		dle.Stop()
	}

	fmt.Fprintln(s.out, "Session ended.") //nolint:errcheck
}
