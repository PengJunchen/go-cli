package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
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
	"github.com/pengjunchen/go-cli/internal/tui"
)

// Interactive command constants. They live in one place so the scanner does not
// flag them as scattered hardcoded values.
const (
	// interactiveDefaultMaxTokens is the default compaction token budget.
	interactiveDefaultMaxTokens = 8000
	// spanInteractiveRun is the top-level span for an interactive session.
	spanInteractiveRun = "interactive.run"
	// spanInteractiveTurn is the span for a single turn in the session.
	spanInteractiveTurn = "interactive.turn"
	// spanInteractiveCompact is the span for auto-compaction between turns.
	spanInteractiveCompact = "interactive.compact"
	// exitCommand is the user input that terminates the interactive session.
	exitCommand = "exit"
)

// interactiveCmd implements Command and runs an interactive multi-turn agent
// session with TUI rendering, MCP tool support, skill execution, and automatic
// session compaction.
type interactiveCmd struct {
	out io.Writer
	in  io.Reader
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
	fs.IntVar(&maxTokensFlag, "max-tokens", interactiveDefaultMaxTokens, "compaction token budget")
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
		maxTokensFlag = interactiveDefaultMaxTokens
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

	model, cleanup, err := c.buildModel(spanCtx, rc, providerName, modelName)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return newExecutionError("interactive: build model", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	tr := tools.NewDefaultToolRegistry()
	if registerErr := tools.RegisterDefaults(spanCtx, tr); registerErr != nil {
		span.SetStatus(tracing.SpanStatusError, registerErr.Error())
		return newExecutionError("interactive: register tools", registerErr)
	}

	// Register MCP tools.
	if mcpErr := c.registerMCPTools(spanCtx, rc, tr); mcpErr != nil {
		logger.Warn("cli_interactive_mcp_failed", "err", mcpErr)
	}

	// Register skill tools.
	if skillErr := c.registerSkillTools(spanCtx, rc, tr); skillErr != nil {
		logger.Warn("cli_interactive_skill_failed", "err", skillErr)
	}

	// Wire approval + mutation middleware via decorator pattern.
	// Approval: SafetyPolicyClassifier denies dangerous tools (bash), allows all others.
	// Mutation: serializes concurrent write/edit to the same file path.
	approvalMW := approval.NewApprovalMiddleware(
		approval.NewSafetyPolicyClassifier([]string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
	)
	tr = tools.NewMiddlewareToolRegistry(tr, approvalMW.WrapToolCall, tools.NewMutationWrapper())

	// Wire production resilience (retry + cost tracking).
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
	)

	// Build loop agent with configurable max iterations.
	loopOpts := []core.LoopOption{
		core.WithLLM(model),
		core.WithTools(tr),
		core.WithModelWrapper(newModelWrapper(pw, nil)),
	}
	if rc != nil && rc.Agent.MaxIterations != 0 {
		loopOpts = append(loopOpts, core.WithMaxIterations(rc.Agent.MaxIterations))
	}
	loop := core.NewLoopAgent(loopOpts...)

	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	midTurn := compaction.NewMidTurnCompact()

	// Wire session persistence.
	var sessionStore *session.JSONLSessionStore
	if rc != nil && rc.Session.StorePath != "" {
		sessionStore = session.NewJSONLSessionStore(rc.Session.StorePath)
		if openErr := sessionStore.Open(spanCtx); openErr != nil {
			logger.Warn("cli_interactive_session_open_failed", "err", openErr)
			sessionStore = nil
		}
	}
	if sessionStore != nil {
		defer sessionStore.Close()
	}

	// Resume history from session store if requested.
	var restoredHistory []core.AgentMessage
	if resumeFlag && sessionStore != nil {
		restoredHistory, _ = loadSessionHistory(sessionStore.FilePath())
		if len(restoredHistory) > 0 {
			logger.Info("cli_interactive_session_resumed", "messages", len(restoredHistory))
		}
	}

	agentOpts := []core.AgentOption{
		core.WithCompactionHook(newCompactionHook(compactor, estimator, maxTokensFlag)),
	}
	if len(restoredHistory) > 0 {
		agentOpts = append(agentOpts, core.WithHistory(restoredHistory))
	}
	agent := core.NewAgentImpl("interactive", loop, agentOpts...)
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(64))

	var turnItems []compaction.TurnItem
	entryCounter := len(restoredHistory)

	// Default to os.Stdin when no input reader was provided at registration
	// time (the common path when RunWithRegistry creates the command).
	in := c.in
	if in == nil {
		in = os.Stdin
	}

	fmt.Fprintln(c.out, "Interactive session started. Type 'exit' to quit.")

	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprint(c.out, "> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.EqualFold(line, exitCommand) {
			logger.Info("cli_interactive_exit", "op", "cli.interactive.exit")
			break
		}

		turnSpan, turnCtx := tracing.SpanFromContext(spanCtx, spanInteractiveTurn, tracing.SpanKindInternal)
		turnSpan.SetAttributes(tracing.Attribute{Key: "user_message", Value: line})

		turnItems = append(turnItems, compaction.TurnItem{
			ID:      fmt.Sprintf("msg-%d", len(turnItems)),
			Role:    compaction.RoleUser,
			Content: line,
		})

		// Auto-compaction check before submitting.
		turnItems = c.autoCompact(turnCtx, turnItems, maxTokensFlag, compactor, estimator, midTurn, logger)

		stream, err := h.Submit(turnCtx, line)
		if err != nil {
			turnSpan.SetStatus(tracing.SpanStatusError, err.Error())
			turnSpan.End()
			fmt.Fprintf(c.out, "Error: %v\n", err)
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
		var app *tui.BubbleteaApp
		app = tui.NewBubbleteaApp(tuiEvents,
			tui.WithWidth(80),
			tui.WithoutKeyboardNavigation(),
			tui.WithOnUpdate(func(view string) {
				if isTTY {
					if lastLineCount > 0 {
						fmt.Fprintf(c.out, "\033[%dA", lastLineCount)
					}
					fmt.Fprint(c.out, "\033[J")
					// In raw mode \n only moves down; \r\n returns to column 0.
					fmt.Fprint(c.out, strings.ReplaceAll(view, "\n", "\r\n"))
					fmt.Fprint(c.out, "\r\n")
					lastLineCount = strings.Count(view, "\n") + 1
				} else {
					fmt.Fprintln(c.out, view)
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
		// EventStream — by which point SetResult has already been invoked.
		// stream.Result() is non-blocking, so it must be called after the
		// stream is known to be closed, otherwise it returns errNoResult.
		select {
		case <-app.Done():
		case <-turnCtx.Done():
		}

		result, streamErr := stream.Result()
		turnSpan.SetStatus(tracing.SpanStatusOK, "")
		turnSpan.End()

		app.Quit()

		if streamErr != nil {
			fmt.Fprintf(c.out, "Error: %v\n", streamErr)
			continue
		}

		// The accordion view was streamed in real time via onUpdate.
		// Only print the final assistant message in non-TTY mode (where the
		// TUI's real-time repaint isn't active).
		if result.Content != "" {
			if !isTTY {
				fmt.Fprintf(c.out, "AI: %s\n", result.Content)
			}
			turnItems = append(turnItems, compaction.TurnItem{
				ID:      fmt.Sprintf("msg-%d", len(turnItems)),
				Role:    compaction.RoleAssistant,
				Content: result.Content,
			})
		}

		// Persist to session store.
		if sessionStore != nil {
			if appendErr := sessionStore.Append(turnCtx, &session.SessionEntry{
				ID:        fmt.Sprintf("entry-%d", entryCounter),
				Type:      session.EntryTypeUser,
				Content:   line,
				Timestamp: time.Now(),
			}); appendErr != nil {
				logger.Warn("cli_interactive_session_save_failed", "err", appendErr)
			}
			entryCounter++
			if result.Content != "" {
				if appendErr := sessionStore.Append(turnCtx, &session.SessionEntry{
					ID:        fmt.Sprintf("entry-%d", entryCounter),
					Type:      session.EntryTypeAssistant,
					Content:   result.Content,
					Timestamp: time.Now(),
				}); appendErr != nil {
					logger.Warn("cli_interactive_session_save_failed", "err", appendErr)
				}
				entryCounter++
			}
			_ = sessionStore.Save(turnCtx)
		}

		logger.Info("cli_interactive_turn_complete",
			"op", "cli.interactive.turn_complete",
			"turn_items", len(turnItems),
		)
	}

	if scanErr := scanner.Err(); scanErr != nil {
		logger.Warn("cli_interactive_scanner_error", "err", scanErr)
	}

	fmt.Fprintln(c.out, "Session ended.")
	return nil
}

// buildModel resolves an llm.BaseChatModel. Reuses the same logic as promptCmd.
func (c *interactiveCmd) buildModel(ctx context.Context, rc *config.Config, providerName, modelName string) (llm.BaseChatModel, func(), error) {
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
func (c *interactiveCmd) registerMCPTools(ctx context.Context, rc *config.Config, tr tools.ToolRegistry) error {
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
			slog.Warn("cli_interactive_mcp_connect_failed", "server", srv.Name, "err", err)
			continue
		}

		mcpTools, err := client.ListTools(ctx)
		if err != nil {
			slog.Warn("cli_interactive_mcp_list_failed", "server", srv.Name, "err", err)
			continue
		}

		for _, t := range mcpTools {
			if regErr := tr.Register(ctx, mcp.NewMCPToolAdapter(client, t)); regErr != nil {
				slog.Warn("cli_interactive_mcp_register_failed", "tool", t.Name, "err", regErr)
			}
		}
		slog.Info("cli_interactive_mcp_registered", "server", srv.Name, "tools", len(mcpTools))
	}
	return nil
}

// registerSkillTools loads skills from the configured directory and registers
// them as tools. It supports two directory layouts:
//   - Flat: {dir}/{name}.md
//   - Nested: {dir}/{name}/SKILL.md
//
// Skills are loaded with YAMLSkillLoader for proper frontmatter parsing and
// wrapped with SkillAdapter so the agent loop can execute them.
func (c *interactiveCmd) registerSkillTools(ctx context.Context, rc *config.Config, tr tools.ToolRegistry) error {
	if rc == nil || rc.Skill.Dir == "" {
		return nil
	}

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(ctx, rc.Skill.Dir)
	if err != nil {
		slog.Warn("cli_interactive_skill_load_failed", "dir", rc.Skill.Dir, "err", err)
		return nil
	}

	count := 0
	for _, def := range defs {
		if def == nil {
			continue
		}
		adapter := skill.NewSkillAdapter(*def)
		if regErr := tr.Register(ctx, adapter); regErr != nil {
			slog.Warn("cli_interactive_skill_register_failed", "skill", (*def).Name(), "err", regErr)
			continue
		}
		count++
	}
	if count > 0 {
		slog.Info("cli_interactive_skills_registered", "dir", rc.Skill.Dir, "count", count)
	}
	return nil
}

// autoCompact checks whether the conversation turn items exceed the token
// budget and triggers compaction when necessary. It returns the (possibly
// compacted) items.
func (c *interactiveCmd) autoCompact(
	ctx context.Context,
	items []compaction.TurnItem,
	maxTokens int,
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	midTurn *compaction.MidTurnCompact,
	logger *slog.Logger,
) []compaction.TurnItem {
	compactSpan, compactCtx := tracing.SpanFromContext(ctx, spanInteractiveCompact, tracing.SpanKindInternal)
	current := estimateTurnTokens(items, estimator)
	compactSpan.SetAttributes(
		tracing.Attribute{Key: "current_tokens", Value: current},
		tracing.Attribute{Key: "max_tokens", Value: maxTokens},
	)

	// CompactIfNeeded uses a threshold ratio (default 0.8) to decide whether
	// compaction should run. It returns items unchanged when below threshold.
	result, compactResult, err := midTurn.CompactIfNeeded(compactCtx, items, maxTokens, estimator, compactor)

	compactSpan.SetAttributes(
		tracing.Attribute{Key: "triggered", Value: compactResult.Triggered},
		tracing.Attribute{Key: "reason", Value: compactResult.Reason.String()},
	)

	if err != nil {
		compactSpan.SetStatus(tracing.SpanStatusError, err.Error())
		compactSpan.End()
		logger.Warn("cli_interactive_compact_failed", "err", err)
		return items
	}

	compactSpan.SetStatus(tracing.SpanStatusOK, "")
	compactSpan.End()

	if compactResult.Triggered {
		logger.Info("cli_interactive_compacted",
			"op", "cli.interactive.compacted",
			"reason", compactResult.Reason.String(),
			"items_in", len(items),
			"items_out", len(result),
		)
		return result
	}
	return items
}

// estimateTurnTokens sums the estimated token counts of all turn items.
func estimateTurnTokens(items []compaction.TurnItem, estimator compaction.TokenEstimator) int {
	total := 0
	for _, it := range items {
		if it.Content != "" {
			n, _ := estimator.Estimate(it.Content)
			total += n
		}
		if it.ToolResult != "" {
			n, _ := estimator.Estimate(it.ToolResult)
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
	defer file.Close()

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

var _ Command = (*interactiveCmd)(nil)
