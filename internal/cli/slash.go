package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/memory"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/tui"
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
	memoryStore   memory.MemoryStore
	contextWindow int
	// thinkingVisibility controls how thinking entries are displayed in the TUI.
	// "show" (default) expands them, "collapse" folds them to a summary, "hide"
	// suppresses them entirely. Set by the /thinking slash command.
	thinkingVisibility string
	// worktreeManager manages git worktrees for parallel session isolation.
	// It is nil when worktree isolation is not enabled in config.
	worktreeManager *tools.WorktreeManager
	// themeMgr enables runtime theme switching via the /theme slash command.
	// It is nil in headless mode; the ThemeHandler degrades gracefully.
	themeMgr *tui.ThemeManager
	// pendingInput, when set by a slash command handler, is picked up by the
	// REPL loop as the next user message instead of reading from stdin. This
	// enables custom Markdown commands to inject prompt templates.
	pendingInput string
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

	handler, ok := c.slashReg.Lookup(cmd.Name)
	if !ok {
		fmt.Fprintf(sc.out, "Unknown command: /%s. Type /help for available commands.\n", cmd.Name) //nolint:errcheck
		return
	}
	if err := handler.Handle(spanCtx, cmd.Args, sc); err != nil {
		fmt.Fprintf(sc.out, "Error: %v\n", err) //nolint:errcheck
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
	add(&MemoryHandler{})

	// TUI display commands.
	add(&ThinkingHandler{})
	add(&ThemeHandler{})

	// Git worktree management.
	add(&WorktreeHandler{})

	// Aliases.
	reg.RegisterAlias("h", "help")
	reg.RegisterAlias("c", "cost")

	return reg
}

// buildDynamicRegistry creates a SlashCommandRegistry that starts with all
// built-in commands (from buildSlashCommandRegistry) and then loads custom
// Markdown commands from .go-cli/commands/ (or rc.Commands.Dir). Built-in
// commands take priority — custom commands with conflicting names are skipped.
// When rc is nil or no commands directory is found, the result is identical
// to buildSlashCommandRegistry().
func buildDynamicRegistry(rc *config.Config) *SlashCommandRegistry {
	reg := buildSlashCommandRegistry()

	cmdDir := ""
	if rc != nil && rc.Commands.Dir != "" {
		cmdDir = rc.Commands.Dir
	} else {
		cmdDir = discoverCommandDir()
	}
	if cmdDir == "" {
		return reg
	}

	loader := &MarkdownCommandLoader{}
	cmds, err := loader.LoadDir(context.Background(), cmdDir)
	if err != nil {
		slog.Warn("slash_dynamic_load_failed", "dir", cmdDir, "err", err)
		return reg
	}

	count := 0
	for _, cmd := range cmds {
		// Skip custom commands that conflict with built-in names.
		if _, exists := reg.Lookup(cmd.name); exists {
			slog.Warn("slash_dynamic_skip_conflict", "name", cmd.name, "dir", cmdDir)
			continue
		}
		h := &MarkdownCommandHandler{cmd: cmd}
		if err := reg.Register(h); err != nil {
			slog.Warn("slash_dynamic_register_failed", "name", cmd.name, "err", err)
			continue
		}
		count++
	}
	if count > 0 {
		slog.Info("slash_dynamic_registered", "dir", cmdDir, "count", count)
	}
	return reg
}

// discoverCommandDir probes default custom command directories and returns the
// first one that exists. The search order is:
//  1. .go-cli/commands (project-local, conventional location)
//  2. ~/.config/go-cli/commands (global user-level)
func discoverCommandDir() string {
	candidates := []string{".go-cli/commands"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "go-cli", "commands"))
	}
	for _, dir := range candidates {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			slog.Info("slash_dynamic_dir_discovered", "dir", dir)
			return dir
		}
	}
	return ""
}
