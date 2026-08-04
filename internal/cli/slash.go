package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/pengjunchen/go-cli/internal/config"
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
	costTracker    *production.CostTracker
	statsRegistry  *production.StatsRegistry
	sessionID      string
	toolRegistry   tools.ToolRegistry
	modelName      string
	sessionHandler *session.SessionSlashHandler
	out            io.Writer

	// Dependencies for the extended slash commands. Any of these may be nil
	// when the corresponding subsystem is not wired; handlers print a helpful
	// diagnostic in that case.
	fileTracker   *tools.FileTracker
	diffGenerator tools.DiffGenerator
	planCtrl      core.PlanModeController
	config        *config.Config
	sessionStore  *session.JSONLSessionStore
}

// defaultSlashReg is the fully populated registry shared by all interactive
// sessions. It is built once at package initialization; the handlers it
// contains are stateless and operate on the slashContext passed to each
// invocation, so sharing is safe.
var defaultSlashReg = buildSlashCommandRegistry()

// handleSlashCommand dispatches a parsed slash command to the appropriate
// handler via the registry. It emits a tracing span so command invocations are
// observable.
func (c *interactiveCmd) handleSlashCommand(ctx context.Context, cmd session.SlashCommand, sc *slashContext) {
	span, spanCtx := tracing.SpanFromContext(ctx, "slash.command", tracing.SpanKindInternal)
	span.SetAttributes(tracing.Attribute{Key: "command_name", Value: cmd.Name})
	defer span.End()

	handler, ok := defaultSlashReg.Lookup(cmd.Name)
	if !ok {
		fmt.Fprintf(sc.out, "Unknown command: /%s. Type /help for available commands.\n", cmd.Name)
		return
	}
	if err := handler.Handle(spanCtx, cmd.Args, sc); err != nil {
		fmt.Fprintf(sc.out, "Error: %v\n", err)
	}
}

// buildSlashCommandRegistry creates a SlashCommandRegistry populated with every
// built-in slash command handler and the standard aliases. It is the single
// place to add a new slash command.
func buildSlashCommandRegistry() *SlashCommandRegistry {
	reg := NewSlashCommandRegistry()
	// add registers a handler, panicking only on a duplicate name, which would
	// be a programming error in this hardcoded list.
	add := func(h SlashCommandHandler) {
		if err := reg.Register(h); err != nil {
			panic(fmt.Sprintf("slash: build registry: %v", err))
		}
	}

	// Existing commands (migrated from the former switch statement).
	add(&HelpHandler{reg: reg})
	add(&CostHandler{})
	add(&CompactHandler{})
	add(&ClearHandler{})
	add(&ToolsHandler{})
	add(&ModelHandler{})
	add(&SessionHandler{})

	// New commands.
	add(&UndoHandler{})
	add(&DiffHandler{})
	add(&PlanHandler{})
	add(&ConfigHandler{})
	add(&HistoryHandler{})
	add(&SaveHandler{})
	add(&LoadHandler{})

	// Aliases.
	reg.RegisterAlias("h", "help")
	reg.RegisterAlias("c", "cost")

	return reg
}
