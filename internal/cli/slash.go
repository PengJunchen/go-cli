package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// slashContext holds the references that slash command handlers need. It is
// built once after the agent and harness are created and passed to each
// handler invocation.
type slashContext struct {
	agent          *core.AgentImpl
	harness        *core.HarnessImpl
	costTracker    *production.CostTracker
	statsRegistry  *production.StatsRegistry
	sessionID      string
	toolRegistry   tools.ToolRegistry
	modelName      string
	compactor      compaction.Compactor
	estimator      compaction.TokenEstimator
	maxTokens      int
	sessionHandler *session.SessionSlashHandler
	out            io.Writer
}

// handleSlashCommand dispatches a parsed slash command to the appropriate
// handler. It emits a tracing span so command invocations are observable.
func (c *interactiveCmd) handleSlashCommand(ctx context.Context, cmd session.SlashCommand, sc *slashContext) {
	span, spanCtx := tracing.SpanFromContext(ctx, "slash.command", tracing.SpanKindInternal)
	span.SetAttributes(tracing.Attribute{Key: "command_name", Value: cmd.Name})
	defer span.End()

	switch cmd.Name {
	case "help":
		c.slashHelp(sc)
	case "cost":
		c.slashCost(sc)
	case "compact":
		c.slashCompact(spanCtx, sc)
	case "clear":
		c.slashClear(sc)
	case "tools":
		c.slashTools(spanCtx, sc)
	case "model":
		c.slashModel(sc)
	case "session":
		c.slashSession(spanCtx, cmd, sc)
	default:
		fmt.Fprintf(sc.out, "Unknown command: /%s. Type /help for available commands.\n", cmd.Name)
	}
}

// slashHelp prints the list of available slash commands.
func (c *interactiveCmd) slashHelp(sc *slashContext) {
	fmt.Fprintln(sc.out, "Available commands:")
	fmt.Fprintln(sc.out, "  /help     Show this help message")
	fmt.Fprintln(sc.out, "  /cost     Show accumulated cost and usage statistics")
	fmt.Fprintln(sc.out, "  /compact  Manually compact conversation history")
	fmt.Fprintln(sc.out, "  /clear    Clear conversation history")
	fmt.Fprintln(sc.out, "  /tools    List registered tools")
	fmt.Fprintln(sc.out, "  /model    Show the current model name")
	fmt.Fprintln(sc.out, "  /session  Show session tree (subcommands: tree, fork, resume)")
	fmt.Fprintln(sc.out, "  /tree     Show session tree")
	fmt.Fprintln(sc.out, "  /fork     Fork the current session branch")
	fmt.Fprintln(sc.out, "  /resume   Resume a previous session")
	fmt.Fprintln(sc.out, "  exit      Exit the interactive session")
}

// slashCost prints the accumulated cost, call count, and per-session statistics
// when available.
func (c *interactiveCmd) slashCost(sc *slashContext) {
	if sc.costTracker != nil {
		fmt.Fprintf(sc.out, "Total cost: $%.4f\n", sc.costTracker.Total())
		fmt.Fprintf(sc.out, "Total calls: %d\n", sc.costTracker.Calls())
	} else {
		fmt.Fprintln(sc.out, "Cost tracking not configured.")
	}
	if sc.statsRegistry != nil && sc.sessionID != "" {
		if stats, ok := sc.statsRegistry.GetSessionStats(sc.sessionID); ok {
			fmt.Fprintf(sc.out, "Session stats:\n")
			fmt.Fprintf(sc.out, "  Turns:     %d\n", stats.Turns)
			fmt.Fprintf(sc.out, "  Tool calls: %d\n", stats.ToolCalls)
			fmt.Fprintf(sc.out, "  Tokens in:  %d\n", stats.TokensIn)
			fmt.Fprintf(sc.out, "  Tokens out: %d\n", stats.TokensOut)
		}
	}
}

// slashCompact manually triggers the compaction hook on the agent's history
// and reports the before/after message counts.
func (c *interactiveCmd) slashCompact(ctx context.Context, sc *slashContext) {
	if sc.agent == nil {
		fmt.Fprintln(sc.out, "Agent not configured.")
		return
	}
	before := len(sc.agent.Messages())
	if err := sc.agent.Compact(ctx); err != nil {
		fmt.Fprintf(sc.out, "Compaction failed: %v\n", err)
		return
	}
	after := len(sc.agent.Messages())
	fmt.Fprintf(sc.out, "Compacted history: %d → %d messages\n", before, after)
}

// slashClear clears the agent's conversation history and confirms the action.
func (c *interactiveCmd) slashClear(sc *slashContext) {
	if sc.agent == nil {
		fmt.Fprintln(sc.out, "Agent not configured.")
		return
	}
	sc.agent.ClearHistory()
	fmt.Fprintln(sc.out, "Conversation history cleared.")
}

// slashTools lists all registered tools with their names and descriptions.
func (c *interactiveCmd) slashTools(ctx context.Context, sc *slashContext) {
	if sc.toolRegistry == nil {
		fmt.Fprintln(sc.out, "Tool registry not configured.")
		return
	}
	defs, err := sc.toolRegistry.List(ctx)
	if err != nil {
		fmt.Fprintf(sc.out, "Error listing tools: %v\n", err)
		return
	}
	if len(defs) == 0 {
		fmt.Fprintln(sc.out, "No tools registered.")
		return
	}
	fmt.Fprintf(sc.out, "Registered tools (%d):\n", len(defs))
	for _, def := range defs {
		fmt.Fprintf(sc.out, "  %s: %s\n", def.Name(), def.Description())
	}
}

// slashModel prints the name of the model currently in use.
func (c *interactiveCmd) slashModel(sc *slashContext) {
	fmt.Fprintf(sc.out, "Current model: %s\n", sc.modelName)
}

// slashSession delegates to the SessionSlashHandler for session tree operations
// (/tree, /fork, /resume). When no session handler is configured it prints a
// diagnostic message. Without sub-arguments it defaults to showing the tree.
func (c *interactiveCmd) slashSession(ctx context.Context, cmd session.SlashCommand, sc *slashContext) {
	if sc.sessionHandler == nil {
		fmt.Fprintln(sc.out, "Session tree not configured.")
		return
	}
	// Map /session to /tree when no subcommand is given; otherwise use the
	// first argument as the subcommand name (e.g. /session fork name → fork).
	subCmd := cmd
	if len(cmd.Args) > 0 {
		subCmd.Name = cmd.Args[0]
		subCmd.Args = cmd.Args[1:]
	} else {
		subCmd.Name = "tree"
	}
	output, err := sc.sessionHandler.Handle(ctx, subCmd)
	if err != nil {
		fmt.Fprintf(sc.out, "Error: %v\n", err)
		return
	}
	fmt.Fprint(sc.out, output)
}
