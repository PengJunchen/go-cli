package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/tui"

	tea "github.com/charmbracelet/bubbletea"
)

// Interactive command constants. They live in one place so the scanner does not
// flag them as scattered hardcoded values.
const (
	// spanInteractiveRun is the top-level span for an interactive session.
	spanInteractiveRun = "interactive.run"
	// spanInteractiveTurn is the span for a single turn in the session.
	spanInteractiveTurn = "interactive.turn"
	// spanInteractiveMentionExpand is the span for @-mention file expansion
	// within a single turn.
	spanInteractiveMentionExpand = "interactive.mention_expand"
	// exitCommand is the user input that terminates the interactive session.
	exitCommand = "exit"
)

// interactiveCmd implements Command and runs an interactive multi-turn agent
// session with TUI rendering, MCP tool support, skill execution, and automatic
// session compaction.
type interactiveCmd struct {
	out             io.Writer
	in              io.Reader
	lineEditor      LineEditor
	mentionExpander *MentionExpander
	slashReg        *SlashCommandRegistry
}

// newInteractiveCmd creates an interactive command reading from in and writing
// to out.
func newInteractiveCmd(in io.Reader, out io.Writer) *interactiveCmd {
	return &interactiveCmd{out: out, in: in, slashReg: defaultSlashReg}
}

// Name implements Command.
func (c *interactiveCmd) Name() string { return "interactive" }

// Synopsis implements Command.
func (c *interactiveCmd) Synopsis() string {
	return "Start an interactive multi-turn agent session with TUI"
}

// Run implements Command. It parses the -model, -provider, and -max-tokens
// flags, then enters a read-eval-print loop that submits each user message
// through the agent harness, renders the output via the TUI, and triggers
// compaction when the token estimate exceeds the budget.
func (c *interactiveCmd) Run(ctx context.Context, cfg Config, args []string) error {
	fs := flag.NewFlagSet("interactive", flag.ContinueOnError)
	fs.SetOutput(c.out)

	var (
		modelFlag     string
		providerFlag  string
		maxTokensFlag int
		verboseFlag   bool
		resumeFlag    bool
		thinkingFlag  string
	)
	fs.StringVar(&modelFlag, "model", "", "model name to use")
	fs.StringVar(&providerFlag, "provider", "", "provider name to use")
	fs.IntVar(&maxTokensFlag, "max-tokens", defaultMaxTokens, "compaction token budget")
	fs.BoolVar(&verboseFlag, "verbose", false, "enable verbose output")
	fs.BoolVar(&resumeFlag, "resume", false, "resume previous session from store path")
	fs.StringVar(&thinkingFlag, "thinking", "medium", "thinking level: none|minimal|low|medium|high|max")
	if err := fs.Parse(args); err != nil {
		return newUsageError("interactive: %v", err)
	}

	thinkingLevel, err := llm.ParseThinkingLevel(thinkingFlag)
	if err != nil {
		return newUsageError("interactive: %v", err)
	}

	var rc *config.Config
	if v, ok := cfg.(*config.Config); ok {
		rc = v
	}

	// Build the dynamic slash command registry: built-in commands plus any
	// custom Markdown commands from .go-cli/commands/. Built-in commands take
	// priority over custom commands with the same name.
	c.slashReg = buildDynamicRegistry(rc)

	modelName := resolveModelName(modelFlag, rc)
	providerName := resolveProviderName(providerFlag, rc)
	if maxTokensFlag <= 0 {
		maxTokensFlag = defaultMaxTokens
	}

	span, spanCtx := tracing.SpanFromContext(ctx, spanInteractiveRun, tracing.SpanKindInternal)
	span.SetAttributes(
		tracing.Attribute{Key: "command", Value: c.Name()},
		tracing.Attribute{Key: "model", Value: modelName},
		tracing.Attribute{Key: "provider", Value: providerName},
		tracing.Attribute{Key: "max_tokens", Value: maxTokensFlag},
		tracing.Attribute{Key: "thinking", Value: thinkingFlag},
	)
	defer func() {
		span.SetStatus(tracing.SpanStatusOK, "")
		span.End()
	}()

	logger := slog.Default()
	logger.Info("cli_interactive_run",
		"op", "cli.interactive.run",
		"provider", providerName,
		"model", modelName,
		"max_tokens", maxTokensFlag,
		"verbose", verboseFlag,
		"thinking", thinkingFlag,
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
		WithMaxTokens(maxTokensFlag),
		WithResume(resumeFlag),
		WithSessionPersistence(true),
		WithAgentName("interactive"),
		WithThinkingLevel(thinkingLevel),
	}
	if tui.IsTerminal() {
		approvalCh = make(chan tui.ApprovalRequest, 8)
		assembleOpts = append(assembleOpts, WithApprovalChannel(approvalCh))
	}
	assembly, err := AssembleAgent(spanCtx, rc, providerName, modelName, c.out,
		assembleOpts...)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return newExecutionError("interactive: assemble agent", err)
	}
	defer assembly.Cleanup()

	// Build slash command context from the assembled components.
	var sessionTree session.SessionTree
	var sessionHandler *session.SessionSlashHandler
	if assembly.SessionStore != nil {
		treeBuilder := session.NewDefaultSessionTreeBuilder()
		sessionTree, err = treeBuilder.BuildFromStore(spanCtx, assembly.SessionStore)
		if err != nil {
			// fallback to empty tree on error
			sessionTree = session.NewDefaultSessionTree()
		}
		// Wire Git-aware branch linkage when enabled in config.
		if rc != nil && rc.Session.GitAwareBranch != nil && *rc.Session.GitAwareBranch && assembly.GitTool != nil {
			if dt, ok := sessionTree.(*session.DefaultSessionTree); ok {
				dt.SetGitBranchSwitcher(assembly.GitTool)
			}
		}
		sessionHandler = session.NewSessionSlashHandler(sessionTree, assembly.SessionStore)
		sessionHandler.OnResume = func(ctx context.Context, entries []session.SessionEntry) error {
			assembly.Agent.SetHistory(session.EntriesToAgentMessages(entries))
			return nil
		}
	}
	slashCtx := slashContext{
		agent:           assembly.Agent,
		costTracker:     assembly.CostTracker,
		statsRegistry:   assembly.StatsRegistry,
		sessionID:       assembly.SessionID,
		toolRegistry:    assembly.ToolRegistry,
		modelName:       modelName,
		sessionHandler:  sessionHandler,
		out:             c.out,
		config:          rc,
		sessionStore:    assembly.SessionStore,
		fileTracker:     assembly.FileTracker,
		diffGenerator:   assembly.DiffGenerator,
		planCtrl:        assembly.PlanCtrl,
		memoryStore:     assembly.MemoryStore,
		contextWindow:   assembly.ContextWindow,
		worktreeManager: assembly.WorktreeManager,
	}

	// Create a shared ThemeManager so /theme can switch themes at runtime
	// without restarting. The initial theme is set from config; subsequent
	// switches via /theme persist across turns because the same manager is
	// passed to every BubbleteaApp via WithThemeManager.
	sharedThemeMgr := tui.NewThemeManager()
	if rc != nil {
		themeName := strings.TrimSpace(strings.ToLower(rc.TUI.Theme))
		if themeName == "" || themeName == "auto" {
			themeName = "dark"
		}
		if err := sharedThemeMgr.Set(themeName); err != nil {
			logger.Warn("cli_interactive_theme_init", "theme", themeName, "err", err)
		}
	}
	slashCtx.themeMgr = sharedThemeMgr

	entryCounter := len(assembly.Agent.Messages())
	turnCounter := 0

	// Default to os.Stdin when no input reader was provided at registration
	// time (the common path when RunWithRegistry creates the command).
	in := c.in
	if in == nil {
		in = os.Stdin
	}

	fmt.Fprintln(c.out, "Interactive session started. Type 'exit' to quit.") //nolint:errcheck

	le := c.lineEditor
	if le == nil {
		// Resolve history file path: use the configured path or fall back
		// to ~/.go-cli/history.jsonl so history persists across sessions.
		historyPath := ""
		if rc != nil && rc.History.Path != "" {
			historyPath = rc.History.Path
		} else if home, err := os.UserHomeDir(); err == nil && home != "" {
			historyPath = filepath.Join(home, ".go-cli", "history.jsonl")
		}

		historyMaxLen := 1000
		if rc != nil && rc.History.MaxLen > 0 {
			historyMaxLen = rc.History.MaxLen
		}

		dle := NewDefaultLineEditor(in, c.out,
			WithHistoryPath(historyPath),
			WithHistoryMaxLen(historyMaxLen),
		)
		// Register completers in priority order: slash commands first,
		// then file paths, and finally LSP code completion (when an LSP
		// server is configured). CompositeCompleter uses first-non-empty-
		// wins fall-through, so nil children are silently skipped.
		var lspCompleter Completer
		if assembly.LSPClient != nil {
			lspCompleter = NewLSPCompleter(assembly.LSPClient, assembly.LSPWorkspaceRoot)
		}
		dle.SetCompleter(NewCompositeCompleter(
			NewSlashCommandCompleterFromRegistry(c.slashReg),
			NewFilePathCompleter(),
			lspCompleter,
		))

		// Load persisted history on startup (best-effort).
		if hs := dle.HistoryStore(); hs != nil {
			if err := hs.Load(); err != nil {
				logger.Warn("cli_interactive_history_load_failed", "err", err)
			}
		}
		le = dle
	}

	// Initialize the @-mention expander using the current working directory
	// so relative @path tokens resolve correctly.
	if c.mentionExpander == nil {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		c.mentionExpander = NewMentionExpander(cwd, 0)
	}

	for {
		line, err := le.ReadLine(ctx, "> ")
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logger.Warn("cli_interactive_readline_error", "err", err)
			}
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Slash command routing.
		if strings.HasPrefix(line, "/") {
			cmd, ok := session.ParseSlashCommand(line)
			if !ok {
				fmt.Fprintln(c.out, "Invalid command. Type /help for available commands.") //nolint:errcheck
				continue
			}
			if cmd.Name == "exit" {
				break
			}
			c.handleSlashCommand(spanCtx, cmd, &slashCtx)
			// Custom Markdown command handlers may set pendingInput to inject
			// a prompt template that the REPL loop processes as user input.
			if slashCtx.pendingInput != "" {
				line = slashCtx.pendingInput
				slashCtx.pendingInput = ""
				// Fall through to process as a normal user message.
			} else {
				continue
			}
		}
		if strings.EqualFold(line, exitCommand) {
			logger.Info("cli_interactive_exit", "op", "cli.interactive.exit")
			break
		}

		turnSpan, turnCtx := tracing.SpanFromContext(spanCtx, spanInteractiveTurn, tracing.SpanKindInternal)
		turnSpan.SetAttributes(tracing.SensitiveAttribute("user_message", line))

		// Expand @-mentions before processing. The SensitiveAttribute above
		// already captured the original user input; Expand modifies line so
		// the inlined file content reaches the agent via RunTurn.
		if c.mentionExpander != nil {
			mentionSpan, mentionCtx := tracing.SpanFromContext(turnCtx, spanInteractiveMentionExpand, tracing.SpanKindInternal)
			expanded, files, totalBytes, mErr := c.mentionExpander.Expand(mentionCtx, line)
			if mErr == nil {
				mentionSpan.SetAttributes(
					tracing.Attribute{Key: "mention.count", Value: len(files)},
					tracing.Attribute{Key: "mention.files", Value: strings.Join(files, ",")},
					tracing.Attribute{Key: "mention.total_bytes", Value: totalBytes},
				)
				if expanded != line {
					line = expanded
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
		turnCtx, cancelTurn := context.WithCancel(turnCtx)
		interrupter := NewInterruptHandler(cancelTurn)
		interrupter.Start(nil)

		// Create an EventStream for this turn and wire it to the TurnRunner
		// so events are streamed in real time to the TUI. RunTurn is
		// blocking, so it runs in a goroutine that closes the stream when
		// the turn finishes. DiscardOldest prevents goroutine leaks if the
		// TUI consumer falls behind: old events are evicted rather than
		// blocking the agent loop indefinitely.
		stream := core.NewEventStream(64, core.WithEventDiscardPolicy(core.DiscardOldest))
		assembly.TurnRunner.SetStream(stream)

		var turnResult core.Result
		var turnErr error
		turnDone := make(chan struct{})
		go func() {
			defer close(turnDone)
			turnResult, turnErr = assembly.TurnRunner.RunTurn(turnCtx, core.Submission{
				Type:    core.SubmissionUserMessage,
				Content: line,
			})

			// Emit a token_usage event from CostTracker and estimator data
			// so the TUI status bar updates before the stream closes.
			emitTokenUsageEvent(stream, assembly)

			// Set the result and send a done event, matching the harness
			// behavior so downstream consumers (bridge, TUI) observe
			// completion correctly.
			stream.SetResult(core.AgentMessage{Role: "assistant", Content: turnResult.Message}, turnErr)
			if turnErr == nil {
				_ = stream.Send(core.AgentEvent{Kind: "done", Content: turnResult.Message, Timestamp: time.Now()}) //nolint:errcheck
			}
			stream.Close()
		}()

		// Bridge core events to TUI events and render. Bubbletea owns terminal
		// rendering in TTY mode (raw mode, diff-based repaint on stderr), so
		// the legacy onUpdate manual redraw and ANSI cursor logic has been
		// removed.
		var tuiEvents <-chan tui.AgentEvent
		if rc != nil && rc.TUI.Mode == "remote" && rc.TUI.RemoteURL != "" {
			tuiEvents = tui.NewACPStreamAdapter(rc.TUI.RemoteURL).Stream(turnCtx)
		} else {
			tuiEvents = tui.BridgeEvents(turnCtx, stream)
		}
		turnCounter++
		isTTY := tui.IsTerminal()
		tsp := tui.NewDefaultTerminalSizeProvider()
		termWidth := tsp.Width()
		appOpts := []tui.AppOption{
			tui.WithWidth(termWidth),
			tui.WithModelInfo(modelName),
			tui.WithTurnCount(turnCounter),
			tui.WithSessionInfo(assembly.SessionID),
		}
		// Mode label: show "plan" when plan mode is active, otherwise "chat".
		if assembly.PlanCtrl != nil && assembly.PlanCtrl.IsActive() {
			appOpts = append(appOpts, tui.WithModeLabel("plan"))
		} else {
			appOpts = append(appOpts, tui.WithModeLabel("chat"))
		}
		// TUIConfig: word wrap, diff style. The shared theme manager is
		// passed separately so runtime /theme switches persist across turns.
		if rc != nil {
			appOpts = append(appOpts,
				tui.WithWordWrap(rc.TUI.WordWrap),
				tui.WithDiffStyle(rc.TUI.DiffStyle),
			)
		}
		appOpts = append(appOpts, tui.WithThemeManager(sharedThemeMgr))
		if assembly.ApprovalChannel != nil {
			appOpts = append(appOpts, tui.WithApprovalChannel(assembly.ApprovalChannel))
		}
		if slashCtx.thinkingVisibility != "" {
			appOpts = append(appOpts, tui.WithThinkingVisibility(slashCtx.thinkingVisibility))
		}
		appOpts = append(appOpts,
			tui.WithSteerCallback(func(input string) {
				turnID := assembly.TurnRunner.RunningTurnID()
				if turnID != "" {
					if steerErr := assembly.TurnRunner.Steer(turnCtx, turnID, input); steerErr != nil {
						logger.Warn("cli_interactive_steer_callback_failed", "err", steerErr)
					}
				} else if assembly.SteerChannel != nil {
					select {
					case assembly.SteerChannel <- input:
					default:
					}
				}
			}),
			tui.WithFollowUpCallback(func(input string) {
				turnID := assembly.TurnRunner.RunningTurnID()
				if turnID != "" {
					if err := assembly.TurnRunner.FollowUp(turnCtx, turnID, input); err != nil {
						logger.Warn("cli_interactive_followup_callback_failed", "err", err)
					}
				} else if assembly.FollowUpChannel != nil {
					select {
					case assembly.FollowUpChannel <- input:
					default:
						logger.Warn("cli_interactive_followup_channel_full")
					}
				}
			}),
			tui.WithCancelCallback(func() {
				cancelTurn()
			}),
			tui.WithPauseCallback(func() {
				if assembly.LoopAgent != nil {
					assembly.LoopAgent.Pause()
				}
			}),
			tui.WithResumeCallback(func() {
				if assembly.LoopAgent != nil {
					assembly.LoopAgent.Resume()
				}
			}),
		)
		app := tui.NewBubbleteaApp(tuiEvents, appOpts...)

		go func() {
			if runErr := app.Run(turnCtx); runErr != nil && runErr != context.Canceled {
				logger.Debug("cli_interactive_tui_error", "err", runErr)
			}
		}()

		// Wait for the tea.Program to be initialized so HITL can route
		// through it. The program is created inside Run (which runs in the
		// goroutine above), so we poll app.Program() until it is non-nil.
		// In TTY mode this routes HITL questions through bubbletea instead
		// of corrupting stdout.
		if assembly.HITLEmitter != nil {
			for i := 0; i < 100; i++ {
				if prog := app.Program(); prog != nil {
					assembly.HITLEmitter.program = prog
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}

		// Wait for the render loop to finish. The app exits when the bridge
		// channel closes (stream closed by the RunTurn goroutine). Steer
		// messages arriving on the interrupter's channel are forwarded to
		// the TurnRunner via Steer(), which delivers them to the running
		// loop between LLM iterations.
		turnComplete := false
		for !turnComplete {
			select {
			case <-app.Done():
				turnComplete = true
			case <-turnCtx.Done():
				<-app.Done()
				turnComplete = true
			case steerMsg := <-interrupter.SteerChannel():
				logger.Info("cli_interactive_steer",
					"op", "cli.interactive.steer",
					"message", steerMsg,
				)
				turnID := assembly.TurnRunner.RunningTurnID()
				if turnID != "" {
					if steerErr := assembly.TurnRunner.Steer(turnCtx, turnID, steerMsg); steerErr != nil {
						logger.Warn("cli_interactive_steer_failed", "err", steerErr)
					} else {
						logger.Info("cli_interactive_steer_forwarded", "message", steerMsg)
					}
				} else if assembly.SteerChannel != nil {
					select {
					case assembly.SteerChannel <- steerMsg:
						logger.Info("cli_interactive_steer_forwarded", "message", steerMsg)
					default:
						logger.Warn("cli_interactive_steer_channel_full")
					}
				}
			}
		}

		// Clear the HITL program reference now that the turn is complete.
		// The bubbletea program from this turn has exited; leaving a stale
		// reference would cause the next turn to send to a dead program.
		if assembly.HITLEmitter != nil {
			assembly.HITLEmitter.program = nil
		}

		// Wait for RunTurn to finish so turnResult and turnErr are populated.
		<-turnDone

		interrupted := turnCtx.Err() != nil
		cancelTurn()
		interrupter.Stop()
		turnSpan.SetStatus(tracing.SpanStatusOK, "")
		turnSpan.End()

		app.Quit()

		if turnErr != nil && !interrupted {
			fmt.Fprintf(c.out, "Error: %v\n", turnErr) //nolint:errcheck
			continue
		}

		if interrupted {
			fmt.Fprintln(c.out, "[interrupted]") //nolint:errcheck
		}

		// In TTY mode bubbletea streamed the accordion view in real time on
		// stderr. Only print the final assistant message in non-TTY mode
		// (where bubbletea's renderer is disabled), and only when the turn
		// was not interrupted.
		if turnResult.Message != "" && !isTTY && !interrupted {
			fmt.Fprintf(c.out, "AI: %s\n", turnResult.Message) //nolint:errcheck
		}

		// Persist to session store (even on interruption to preserve history).
		// Use spanCtx, not turnCtx, because turnCtx may be canceled by the
		// interrupt handler.
		if assembly.SessionStore != nil {
			parentID := ""
			if sessionTree != nil {
				parentID = sessionTree.CurrentLeaf()
			}
			userEntry := &session.SessionEntry{
				ID:        fmt.Sprintf("entry-%d", entryCounter),
				ParentID:  parentID,
				Type:      session.EntryTypeUser,
				Content:   line,
				Timestamp: time.Now(),
			}
			if appendErr := assembly.SessionStore.Append(spanCtx, userEntry); appendErr != nil {
				logger.Warn("cli_interactive_session_save_failed", "err", appendErr)
			}
			if sessionTree != nil {
				if treeErr := sessionTree.Append(spanCtx, userEntry); treeErr != nil {
					logger.Warn("cli_interactive_session_tree_append_failed", "err", treeErr)
				}
			}
			entryCounter++
			if turnResult.Message != "" {
				parentID := ""
				if sessionTree != nil {
					parentID = sessionTree.CurrentLeaf()
				}
				assistantEntry := &session.SessionEntry{
					ID:        fmt.Sprintf("entry-%d", entryCounter),
					ParentID:  parentID,
					Type:      session.EntryTypeAssistant,
					Content:   turnResult.Message,
					Timestamp: time.Now(),
				}
				if appendErr := assembly.SessionStore.Append(spanCtx, assistantEntry); appendErr != nil {
					logger.Warn("cli_interactive_session_save_failed", "err", appendErr)
				}
				if sessionTree != nil {
					if treeErr := sessionTree.Append(spanCtx, assistantEntry); treeErr != nil {
						logger.Warn("cli_interactive_session_tree_append_failed", "err", treeErr)
					}
				}
				entryCounter++
			}
			_ = assembly.SessionStore.Save(spanCtx) //nolint:errcheck

			// Asynchronously extract memories from the conversation for
			// cross-session context continuity. Uses a detached context
			// so it survives turn cancellation. Errors are logged only
			// and do not block the main interaction loop.
			if assembly.MemoryExtractor != nil && assembly.MemoryStore != nil {
				agentMsgs := assembly.Agent.Messages()
				msgs := make([]llm.Message, 0, len(agentMsgs))
				for _, m := range agentMsgs {
					msgs = append(msgs, llm.Message{
						Role:    llm.Role(m.Role),
						Content: m.Content,
					})
				}
				memStore := assembly.MemoryStore
				extractor := assembly.MemoryExtractor
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					extracted, err := extractor.Extract(ctx, msgs)
					if err != nil {
						logger.Warn("cli_interactive_memory_extract_failed", "err", err)
						return
					}
					for _, mem := range extracted {
						if err := memStore.Add(ctx, mem); err != nil {
							logger.Warn("cli_interactive_memory_store_failed", "err", err)
						}
					}
				}()
			}
		}

		logger.Info("cli_interactive_turn_complete",
			"op", "cli.interactive.turn_complete",
		)
	}

	// Save history on exit (covers EOF, /exit, and exit text paths).
	if dle, ok := le.(*DefaultLineEditor); ok {
		if hs := dle.HistoryStore(); hs != nil {
			_ = hs.Save()
		}
		dle.Stop()
	}

	fmt.Fprintln(c.out, "Session ended.") //nolint:errcheck
	return nil
}

// messagesToTurnItems converts core.AgentMessage slice to compaction.TurnItem
// slice for the compaction pipeline.
func messagesToTurnItems(msgs []core.AgentMessage) []compaction.TurnItem {
	items := make([]compaction.TurnItem, len(msgs))
	for i, m := range msgs {
		item := compaction.TurnItem{
			ID:            fmt.Sprintf("msg-%d", i),
			Role:          m.Role,
			ContentBlocks: m.ContentBlocks,
			ToolCalls:     m.ToolCalls,
			ToolCallID:    m.ToolCallID,
			ToolName:      m.ToolName,
		}
		if m.Role == compaction.RoleTool {
			item.ToolResult = m.Content
		} else {
			item.Content = m.Content
		}
		items[i] = item
	}
	return items
}

// turnItemsToMessages converts compaction.TurnItem slice back to
// core.AgentMessage slice.
func turnItemsToMessages(items []compaction.TurnItem) []core.AgentMessage {
	msgs := make([]core.AgentMessage, len(items))
	for i, it := range items {
		msg := core.AgentMessage{
			Role:          it.Role,
			ContentBlocks: it.ContentBlocks,
			ToolCalls:     it.ToolCalls,
			ToolCallID:    it.ToolCallID,
			ToolName:      it.ToolName,
		}
		if it.Role == compaction.RoleTool {
			msg.Content = it.ToolResult
		} else {
			msg.Content = it.Content
			if it.IsCompaction && it.Content == "" {
				msg.Content = "[compacted]"
			}
		}
		msgs[i] = msg
	}
	return msgs
}

// loadSessionHistory reads a JSONL session file and reconstructs the message
// history as []core.AgentMessage. Only user and assistant entries are included.
func loadSessionHistory(path string) ([]core.AgentMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck

	var messages []core.AgentMessage
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var entry session.SessionEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type == session.EntryTypeUser || entry.Type == session.EntryTypeAssistant {
			messages = append(messages, core.AgentMessage{
				Role:          string(entry.Type),
				Content:       entry.Content,
				ContentBlocks: entry.ContentBlocks,
				ToolCalls:     entry.ToolCalls,
				ToolCallID:    entry.ToolCallID,
				ToolName:      entry.ToolName,
			})
		}
	}
	return messages, scanner.Err()
}

// emitTokenUsageEvent estimates the total token usage from the agent's message
// history and the accumulated cost from the CostTracker, then sends a
// token_usage event to the stream so the TUI status bar can update.
//
// When the last assistant message carries API-reported Usage (captured during
// streaming), those values are preferred over the local estimation because
// they reflect the actual token consumption billed by the provider. When no
// API usage is available, the function falls back to estimating tokens from
// message content via the configured TokenEstimator.
func emitTokenUsageEvent(stream *core.EventStreamImpl, assembly *AgentAssembly) {
	if assembly.Agent == nil {
		return
	}
	messages := assembly.Agent.Messages()

	var inputTokens, outputTokens int

	// Prefer API-reported usage from the last assistant message that has it.
	// API usage reflects the actual token consumption for the full
	// conversation (input = prompt tokens, output = completion tokens).
	if usage := lastAssistantAPIUsage(messages); usage != nil {
		inputTokens = usage.InputTokens
		outputTokens = usage.OutputTokens
	} else {
		// Fall back to local estimation when the API did not report usage.
		if assembly.Estimator == nil {
			return
		}
		for _, msg := range messages {
			n, _ := assembly.Estimator.Estimate(msg.Content) //nolint:errcheck
			if msg.Role == "assistant" {
				outputTokens += n
			} else {
				inputTokens += n
			}
		}
	}

	cost := 0.0
	if assembly.CostTracker != nil {
		cost = assembly.CostTracker.Total()
	}
	_ = stream.Send(core.AgentEvent{ //nolint:errcheck
		Kind:      "token_usage",
		Timestamp: time.Now(),
		TokenUsage: &core.TokenUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			MaxTokens:    assembly.ContextWindow,
			Cost:         cost,
		},
	})
}

// lastAssistantAPIUsage scans messages from the end and returns the Usage from
// the last assistant message that carries non-nil API-reported usage, or nil
// when none is found.
func lastAssistantAPIUsage(messages []core.AgentMessage) *llm.Usage {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && messages[i].Usage != nil {
			return messages[i].Usage
		}
	}
	return nil
}

var _ Command = (*interactiveCmd)(nil)

// cliHITLEmitter implements core.HITLQuestionEmitter for the interactive CLI.
// It prints the question to the output writer and reads the answer from stdin.
//
// When program is non-nil (TUI mode), Emit routes the question through the
// bubbletea program via program.Send(HITLMessage{...}) instead of writing
// directly to stdout. Writing to stdout while bubbletea owns the terminal
// corrupts the display, so the program path must be used whenever a TUI is
// running. When program is nil (plain CLI mode), Emit falls back to direct
// stdout output.
type cliHITLEmitter struct {
	out     io.Writer
	program *tea.Program
}

// HITLMessage is a bubbletea message carrying a HITL question to the TUI. The
// TUI renders the question, collects the user's answer, and sends a single
// HITLResponse on ResponseCh so the emitter can deliver it to the agent.
type HITLMessage struct {
	QuestionID string
	Question   string
	Options    []string
	ResponseCh chan HITLResponse
}

// HITLResponse is the user's answer to a HITLMessage, sent back by the TUI.
type HITLResponse struct {
	Answer string
	Err    error
}

func (e *cliHITLEmitter) Emit(ctx context.Context, event core.HITLQuestionEvent) error {
	// TUI mode: route the question through the bubbletea program so it renders
	// inside the TUI instead of corrupting stdout.
	if e.program != nil {
		respCh := make(chan HITLResponse, 1)
		e.program.Send(HITLMessage{
			QuestionID: event.QuestionID,
			Question:   event.Question,
			Options:    event.Options,
			ResponseCh: respCh,
		})
		select {
		case resp := <-respCh:
			if resp.Err != nil {
				return resp.Err
			}
			select {
			case event.ResponseCh <- core.HITLAnswer{QuestionID: event.QuestionID, Answer: resp.Answer}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Plain CLI mode: write directly to stdout and read from stdin.
	fmt.Fprintf(e.out, "\n[ask_user] %s\n", event.Question) //nolint:errcheck
	for i, opt := range event.Options {
		fmt.Fprintf(e.out, "  %d. %s\n", i+1, opt) //nolint:errcheck
	}
	fmt.Fprint(e.out, "> ") //nolint:errcheck

	line, err := readLine(os.Stdin)
	if err != nil {
		return err
	}
	answer := strings.TrimSpace(line)

	select {
	case event.ResponseCh <- core.HITLAnswer{QuestionID: event.QuestionID, Answer: answer}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readLine reads a single line from r using a fresh bufio.Reader so it does
// not compete with the REPL scanner's internal buffer.
func readLine(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	return br.ReadString('\n')
}
