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
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
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
	)
	fs.StringVar(&modelFlag, "model", "", "model name to use")
	fs.StringVar(&providerFlag, "provider", "", "provider name to use")
	fs.IntVar(&maxTokensFlag, "max-tokens", defaultMaxTokens, "compaction token budget")
	fs.BoolVar(&verboseFlag, "verbose", false, "enable verbose output")
	fs.BoolVar(&resumeFlag, "resume", false, "resume previous session from store path")
	if err := fs.Parse(args); err != nil {
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
	)

	// Assemble the full agent runtime with all production wiring (model
	// wrapping, tools, approval gates, retry/cost tracking, output guards,
	// subagent, session persistence, compaction).
	assembly, err := AssembleAgent(spanCtx, rc, providerName, modelName, c.out,
		WithMaxTokens(maxTokensFlag),
		WithResume(resumeFlag),
		WithSessionPersistence(true),
		WithAgentName("interactive"),
	)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return newExecutionError("interactive: assemble agent", err)
	}
	defer assembly.Cleanup()

	// Build slash command context from the assembled components.
	var sessionHandler *session.SessionSlashHandler
	if assembly.SessionStore != nil {
		treeBuilder := session.NewDefaultSessionTreeBuilder()
		sessionTree, err := treeBuilder.BuildFromStore(spanCtx, assembly.SessionStore)
		if err != nil {
			// fallback to empty tree on error
			sessionTree = session.NewDefaultSessionTree()
		}
		sessionHandler = session.NewSessionSlashHandler(sessionTree, assembly.SessionStore)
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
	}

	entryCounter := len(assembly.Agent.Messages())

	// Default to os.Stdin when no input reader was provided at registration
	// time (the common path when RunWithRegistry creates the command).
	in := c.in
	if in == nil {
		in = os.Stdin
	}

	fmt.Fprintln(c.out, "Interactive session started. Type 'exit' to quit.") //nolint:errcheck

	le := c.lineEditor
	if le == nil {
		le = NewDefaultLineEditor(in, c.out)
		le.SetCompleter(NewSlashCommandCompleter(slashCommandNames()))
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
		turnSpan.SetAttributes(tracing.Attribute{Key: "user_message", Value: line})

		// Wrap the turn context in a cancellable context so the user can
		// interrupt the in-progress agent turn via Ctrl+C (non-TTY) or Esc
		// (TTY, future work). The InterruptHandler monitors for SIGINT and
		// invokes cancelTurn when a signal arrives.
		turnCtx, cancelTurn := context.WithCancel(turnCtx)
		interrupter := NewInterruptHandler(cancelTurn)
		interrupter.Start(in)

		stream, err := assembly.Harness.Submit(turnCtx, line)
		if err != nil {
			cancelTurn()
			interrupter.Stop()
			turnSpan.SetStatus(tracing.SpanStatusError, err.Error())
			turnSpan.End()
			fmt.Fprintf(c.out, "Error: %v\n", err) //nolint:errcheck
			continue
		}

		// Bridge core events to TUI events and render. The BubbleteaApp is the
		// sole consumer of the tuiEvents channel; when the bridge channel
		// closes the app's Run loop returns and closes its Done channel.
		tuiEvents := tui.BridgeEvents(turnCtx, stream)

		// Streaming output: each view mutation re-renders to the terminal.
		// In TTY mode we use ANSI cursor reset + clear-line to repaint in
		// place; in pipe mode we emit the latest view line-by-line.
		var lastLineCount int
		isTTY := tui.IsTerminal()
		tsp := tui.NewDefaultTerminalSizeProvider()
		termWidth := tsp.Width()
		app := tui.NewBubbleteaApp(tuiEvents,
			tui.WithWidth(termWidth),
			tui.WithOnUpdate(func(view string) {
				if isTTY {
					if lastLineCount > 0 {
						fmt.Fprintf(c.out, "\033[%dA", lastLineCount) //nolint:errcheck
					}
					fmt.Fprint(c.out, "\033[J") //nolint:errcheck
					// In raw mode \n only moves down; \r\n returns to column 0.
					fmt.Fprint(c.out, strings.ReplaceAll(view, "\n", "\r\n")) //nolint:errcheck
					fmt.Fprint(c.out, "\r\n")                                 //nolint:errcheck
					lastLineCount = countViewVisualLines(view, termWidth)
				} else {
					fmt.Fprintln(c.out, view) //nolint:errcheck
				}
			}),
		)

		go func() {
			if runErr := app.Run(turnCtx); runErr != nil && runErr != context.Canceled {
				logger.Debug("cli_interactive_tui_error", "err", runErr)
			}
		}()

		// Wait for the render loop to finish. The app exits when the bridge
		// channel closes, which only happens after the harness closes the
		// EventStream - by which point SetResult has already been invoked.
		// stream.Result() is non-blocking, so it must be called after the
		// stream is known to be closed, otherwise it returns errNoResult.
		// If the context is canceled (user interrupt), we additionally wait
		// for app.Done() so the stream is fully closed and Result() is ready.
		// Steer messages arriving on the interrupter's channel are forwarded
		// to the assembly's shared SteerChannel, which the LoopAgent drains
		// between LLM iterations. Steering can only happen between iterations,
		// not during generation, because the LLM call is a synchronous
		// blocking operation.
		turnComplete := false
		for !turnComplete {
			select {
			case <-app.Done():
				turnComplete = true
			case <-turnCtx.Done():
				// Context canceled (user interrupt). Wait for the app to
				// finish cleaning up so the stream closes and Result() is
				// available.
				<-app.Done()
				turnComplete = true
			case steerMsg := <-interrupter.SteerChannel():
				logger.Info("cli_interactive_steer",
					"op", "cli.interactive.steer",
					"message", steerMsg,
				)
				// Forward the steer message to the assembly's shared
				// SteerChannel so the LoopAgent picks it up between
				// LLM iterations.
				if assembly.SteerChannel != nil {
					select {
					case assembly.SteerChannel <- steerMsg:
						logger.Info("cli_interactive_steer_forwarded", "message", steerMsg)
					default:
						logger.Warn("cli_interactive_steer_channel_full")
					}
				}
			}
		}

		result, streamErr := stream.Result()

		interrupted := turnCtx.Err() != nil
		cancelTurn()
		interrupter.Stop()
		turnSpan.SetStatus(tracing.SpanStatusOK, "")
		turnSpan.End()

		app.Quit()

		if streamErr != nil && !interrupted {
			fmt.Fprintf(c.out, "Error: %v\n", streamErr) //nolint:errcheck
			continue
		}

		if interrupted {
			fmt.Fprintln(c.out, "[interrupted]") //nolint:errcheck
		}

		// The accordion view was streamed in real time via onUpdate.
		// Only print the final assistant message in non-TTY mode (where the
		// TUI's real-time repaint isn't active), and only when the turn was
		// not interrupted.
		if result.Content != "" && !isTTY && !interrupted {
			fmt.Fprintf(c.out, "AI: %s\n", result.Content) //nolint:errcheck
		}

		// Persist to session store (even on interruption to preserve history).
		// Use spanCtx, not turnCtx, because turnCtx may be canceled by the
		// interrupt handler.
		if assembly.SessionStore != nil {
			if appendErr := assembly.SessionStore.Append(spanCtx, &session.SessionEntry{
				ID:        fmt.Sprintf("entry-%d", entryCounter),
				Type:      session.EntryTypeUser,
				Content:   line,
				Timestamp: time.Now(),
			}); appendErr != nil {
				logger.Warn("cli_interactive_session_save_failed", "err", appendErr)
			}
			entryCounter++
			if result.Content != "" {
				if appendErr := assembly.SessionStore.Append(spanCtx, &session.SessionEntry{
					ID:        fmt.Sprintf("entry-%d", entryCounter),
					Type:      session.EntryTypeAssistant,
					Content:   result.Content,
					Timestamp: time.Now(),
				}); appendErr != nil {
					logger.Warn("cli_interactive_session_save_failed", "err", appendErr)
				}
				entryCounter++
			}
			_ = assembly.SessionStore.Save(spanCtx) //nolint:errcheck
		}

		logger.Info("cli_interactive_turn_complete",
			"op", "cli.interactive.turn_complete",
		)
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
				Role:    string(entry.Type),
				Content: entry.Content,
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
