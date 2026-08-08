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
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/tui"
)

// Interactive command constants. They live in one place so the scanner does not
// flag them as scattered hardcoded values.
const (
	// spanInteractiveRun is the top-level span for an interactive session.
	spanInteractiveRun = "interactive.run"
	// spanInteractiveTurn is the span for a single turn in the session.
	spanInteractiveTurn = "interactive.turn"
	// exitCommand is the user input that terminates the interactive session.
	exitCommand = "exit"
)

// interactiveCmd implements Command and runs an interactive multi-turn agent
// session with TUI rendering, MCP tool support, skill execution, and automatic
// session compaction.
type interactiveCmd struct {
	out        io.Writer
	in         io.Reader
	lineEditor LineEditor
}

// newInteractiveCmd creates an interactive command reading from in and writing
// to out.
func newInteractiveCmd(in io.Reader, out io.Writer) *interactiveCmd {
	return &interactiveCmd{out: out, in: in}
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
		if rc != nil && rc.Session.GitAwareBranch && assembly.GitTool != nil {
			if dt, ok := sessionTree.(*session.DefaultSessionTree); ok {
				dt.SetGitBranchSwitcher(assembly.GitTool)
			}
		}
		sessionHandler = session.NewSessionSlashHandler(sessionTree, assembly.SessionStore)
		sessionHandler.OnResume = func(ctx context.Context, entries []session.SessionEntry) error {
			// Convert SessionEntry to AgentMessage and replace agent history.
			msgs := make([]core.AgentMessage, 0, len(entries))
			for _, e := range entries {
				var role string
				switch e.Type {
				case session.EntryTypeUser:
					role = "user"
				case session.EntryTypeAssistant:
					role = "assistant"
				case session.EntryTypeSystem:
					role = "system"
				default:
					continue // skip tool/compaction entries
				}
				msgs = append(msgs, core.AgentMessage{Role: role, Content: e.Content, ContentBlocks: e.ContentBlocks})
			}
			assembly.Agent.SetHistory(msgs)
			return nil
		}
	}
	slashCtx := slashContext{
		agent:          assembly.Agent,
		costTracker:    assembly.CostTracker,
		statsRegistry:  assembly.StatsRegistry,
		sessionID:      assembly.SessionID,
		toolRegistry:   assembly.ToolRegistry,
		modelName:      modelName,
		sessionHandler: sessionHandler,
		out:            c.out,
		config:         rc,
		sessionStore:   assembly.SessionStore,
		fileTracker:    assembly.FileTracker,
		diffGenerator:  assembly.DiffGenerator,
		planCtrl:       assembly.PlanCtrl,
		memoryStore:    assembly.MemoryStore,
		contextWindow:  assembly.ContextWindow,
	}

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
		dle.SetCompleter(NewCompositeCompleter(
			NewSlashCommandCompleter(slashCommandNames()),
			NewFilePathCompleter(),
		))

		// Load persisted history on startup (best-effort).
		if hs := dle.HistoryStore(); hs != nil {
			if err := hs.Load(); err != nil {
				logger.Warn("cli_interactive_history_load_failed", "err", err)
			}
		}
		le = dle
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
			continue
		}
		if strings.EqualFold(line, exitCommand) {
			logger.Info("cli_interactive_exit", "op", "cli.interactive.exit")
			break
		}

		turnSpan, turnCtx := tracing.SpanFromContext(spanCtx, spanInteractiveTurn, tracing.SpanKindInternal)
		turnSpan.SetAttributes(tracing.SensitiveAttribute("user_message", line))

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
		// the turn finishes.
		stream := core.NewEventStream(64)
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
		tuiEvents := tui.BridgeEvents(turnCtx, stream)
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
		// TUIConfig: theme, word wrap, diff style.
		if rc != nil {
			appOpts = append(appOpts,
				tui.WithThemeConfig(rc.TUI.Theme),
				tui.WithWordWrap(rc.TUI.WordWrap),
				tui.WithDiffStyle(rc.TUI.DiffStyle),
			)
		}
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
	}

	fmt.Fprintln(c.out, "Session ended.") //nolint:errcheck
	return nil
}

// estimateTurnTokens sums the estimated token counts of all turn items.
func estimateTurnTokens(items []compaction.TurnItem, estimator compaction.TokenEstimator) int {
	total := 0
	for _, it := range items {
		if it.Content != "" {
			n, _ := estimator.Estimate(it.Content) //nolint:errcheck
			total += n
		}
		if it.ToolResult != "" {
			n, _ := estimator.Estimate(it.ToolResult) //nolint:errcheck
			total += n
		}
	}
	return total
}

// RegisterMCPToolsFromClients connects the given MCP clients and registers
// their tools. This is the public API for programmatic MCP tool registration.
func RegisterMCPToolsFromClients(ctx context.Context, clients []mcp.MCPClient, tr tools.ToolRegistry) error {
	for _, client := range clients {
		if err := client.Connect(ctx); err != nil {
			slog.Warn("cli_interactive_mcp_connect_failed", "server", client.Name(), "err", err)
			continue
		}
		mcpTools, err := client.ListTools(ctx)
		if err != nil {
			slog.Warn("cli_interactive_mcp_list_tools_failed", "server", client.Name(), "err", err)
			continue
		}
		for _, tool := range mcpTools {
			adapter := mcp.NewMCPToolAdapter(client, tool)
			if regErr := tr.Register(ctx, adapter); regErr != nil {
				slog.Warn("cli_interactive_mcp_register_failed",
					"server", client.Name(),
					"tool", tool.Name,
					"err", regErr,
				)
			}
		}
		slog.Info("cli_interactive_mcp_registered",
			"server", client.Name(),
			"tools", len(mcpTools),
		)
	}
	return nil
}

// RegisterSkillToolsFromDir loads skills from the given directory and registers
// them as tools. This is the public API for programmatic skill registration.
func RegisterSkillToolsFromDir(ctx context.Context, dir string, tr tools.ToolRegistry) error {
	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(ctx, dir)
	if err != nil {
		return fmt.Errorf("interactive: load skills from %s: %w", dir, err)
	}
	reg := skill.NewDefaultSkillRegistry()
	for _, def := range defs {
		if def == nil {
			continue
		}
		if regErr := reg.Register(ctx, *def); regErr != nil {
			slog.Warn("cli_interactive_skill_register_failed",
				"skill", (*def).Name(),
				"err", regErr,
			)
		}
	}
	skills := reg.List(ctx)
	for _, s := range skills {
		adapter := skill.NewSkillAdapter(s)
		if regErr := tr.Register(ctx, adapter); regErr != nil {
			slog.Warn("cli_interactive_skill_adapter_failed",
				"skill", s.Name(),
				"err", regErr,
			)
		}
	}
	slog.Info("cli_interactive_skills_registered", "count", len(skills))
	return nil
}

// messagesToTurnItems converts core.AgentMessage slice to compaction.TurnItem
// slice for the compaction pipeline.
func messagesToTurnItems(msgs []core.AgentMessage) []compaction.TurnItem {
	items := make([]compaction.TurnItem, len(msgs))
	for i, m := range msgs {
		items[i] = compaction.TurnItem{
			ID:      fmt.Sprintf("msg-%d", i),
			Role:    m.Role,
			Content: m.Content,
		}
	}
	return items
}

// turnItemsToMessages converts compaction.TurnItem slice back to
// core.AgentMessage slice.
func turnItemsToMessages(items []compaction.TurnItem) []core.AgentMessage {
	msgs := make([]core.AgentMessage, len(items))
	for i, it := range items {
		content := it.Content
		if it.IsCompaction && it.Content == "" {
			content = "[compacted]"
		}
		msgs[i] = core.AgentMessage{
			Role:    it.Role,
			Content: content,
		}
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
			})
		}
	}
	return messages, scanner.Err()
}

// slashCommandNames returns the canonical names of all registered slash
// commands plus "exit", for use by the tab completer.
func slashCommandNames() []string {
	return append(defaultSlashReg.Names(), "exit")
}

// emitTokenUsageEvent estimates the total token usage from the agent's message
// history and the accumulated cost from the CostTracker, then sends a
// token_usage event to the stream so the TUI status bar can update.
func emitTokenUsageEvent(stream *core.EventStreamImpl, assembly *AgentAssembly) {
	if assembly.Estimator == nil || assembly.Agent == nil {
		return
	}
	var inputTokens, outputTokens int
	for _, msg := range assembly.Agent.Messages() {
		n, _ := assembly.Estimator.Estimate(msg.Content) //nolint:errcheck
		if msg.Role == "assistant" {
			outputTokens += n
		} else {
			inputTokens += n
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

var _ Command = (*interactiveCmd)(nil)

// cliHITLEmitter implements core.HITLQuestionEmitter for the interactive CLI.
// It prints the question to the output writer and reads the answer from stdin.
type cliHITLEmitter struct {
	out io.Writer
}

func (e *cliHITLEmitter) Emit(ctx context.Context, event core.HITLQuestionEvent) error {
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
