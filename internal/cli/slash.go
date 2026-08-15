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
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/memory"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/tui"
)

// ---------------------------------------------------------------------------
// Domain accessor interfaces
// ---------------------------------------------------------------------------

// AgentAccessor provides access to agent runtime.
type AgentAccessor interface {
	Agent() *core.AgentImpl
	CostTracker() *production.CostTracker
	StatsRegistry() *production.StatsRegistry
	ContextWindow() int
	ModelName() string
}

// SessionAccessor provides access to session state.
type SessionAccessor interface {
	SessionID() string
	SessionStore() *session.JSONLSessionStore
	SessionHandler() *session.SessionSlashHandler
}

// ToolAccessor provides access to tools and file operations.
type ToolAccessor interface {
	ToolRegistry() tools.ToolRegistry
	FileTracker() *tools.FileTracker
	DiffGenerator() tools.DiffGenerator
	PlanCtrl() core.PlanModeController
	WorktreeManager() *tools.WorktreeManager
	SnapshotManager() *tools.SnapshotManager
}

// DisplayAccessor provides access to output and TUI.
type DisplayAccessor interface {
	Out() io.Writer
	ThemeMgr() *tui.ThemeManager
	ThinkingVisibility() string
	SetThinkingVisibility(v string)
}

// MemoryAccessor provides access to the memory store.
type MemoryAccessor interface {
	MemoryStore() memory.MemoryStore
}

// ConfigAccessor provides access to configuration.
type ConfigAccessor interface {
	Config() *config.Config
}

// ModelAccessor provides access to the model selector for runtime model
// switching via the /model slash command.
type ModelAccessor interface {
	ModelSelector() *llm.DefaultModelSelector
}

// Dependencies combines all accessor interfaces. Slash handlers receive
// this composite interface and use only the parts they need.
type Dependencies interface {
	AgentAccessor
	SessionAccessor
	ToolAccessor
	DisplayAccessor
	MemoryAccessor
	ConfigAccessor
	ModelAccessor
}

// slashContext holds the references that slash command handlers need. It is
// built once after the agent and harness are created and passed to each
// handler invocation. It implements the Dependencies interface via getter
// methods so handlers never access its fields directly.
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
	// snapshotManager captures git working-tree snapshots before file
	// mutations so files can be reverted to a previous state via /revert.
	// It is nil when not in a git repository.
	snapshotManager *tools.SnapshotManager
	// themeMgr enables runtime theme switching via the /theme slash command.
	// It is nil in headless mode; the ThemeHandler degrades gracefully.
	themeMgr *tui.ThemeManager
	// modelSelector enables runtime model switching via the /model slash
	// command. It is nil when the selector was not wired (e.g. in tests).
	modelSelector *llm.DefaultModelSelector
}

// Compile-time assertion that *slashContext satisfies Dependencies.
var _ Dependencies = (*slashContext)(nil)

// --- AgentAccessor ---

func (sc *slashContext) Agent() *core.AgentImpl                   { return sc.agent }
func (sc *slashContext) CostTracker() *production.CostTracker     { return sc.costTracker }
func (sc *slashContext) StatsRegistry() *production.StatsRegistry { return sc.statsRegistry }
func (sc *slashContext) ContextWindow() int                       { return sc.contextWindow }
func (sc *slashContext) ModelName() string {
	if sc.modelSelector != nil {
		return sc.modelSelector.PrimaryModelName()
	}
	return sc.modelName
}

// --- SessionAccessor ---

func (sc *slashContext) SessionID() string                            { return sc.sessionID }
func (sc *slashContext) SessionStore() *session.JSONLSessionStore     { return sc.sessionStore }
func (sc *slashContext) SessionHandler() *session.SessionSlashHandler { return sc.sessionHandler }

// --- ToolAccessor ---

func (sc *slashContext) ToolRegistry() tools.ToolRegistry        { return sc.toolRegistry }
func (sc *slashContext) FileTracker() *tools.FileTracker         { return sc.fileTracker }
func (sc *slashContext) DiffGenerator() tools.DiffGenerator      { return sc.diffGenerator }
func (sc *slashContext) PlanCtrl() core.PlanModeController       { return sc.planCtrl }
func (sc *slashContext) WorktreeManager() *tools.WorktreeManager { return sc.worktreeManager }
func (sc *slashContext) SnapshotManager() *tools.SnapshotManager { return sc.snapshotManager }

// --- DisplayAccessor ---

func (sc *slashContext) Out() io.Writer                 { return sc.out }
func (sc *slashContext) ThemeMgr() *tui.ThemeManager    { return sc.themeMgr }
func (sc *slashContext) ThinkingVisibility() string     { return sc.thinkingVisibility }
func (sc *slashContext) SetThinkingVisibility(v string) { sc.thinkingVisibility = v }

// --- MemoryAccessor ---

func (sc *slashContext) MemoryStore() memory.MemoryStore { return sc.memoryStore }

// --- ConfigAccessor ---

func (sc *slashContext) Config() *config.Config { return sc.config }

// --- ModelAccessor ---

func (sc *slashContext) ModelSelector() *llm.DefaultModelSelector { return sc.modelSelector }

// defaultSlashReg is the fully populated registry shared by all interactive
// sessions. It is built once at package initialization; the handlers it
// contains are stateless and operate on the slashContext passed to each
// invocation, so sharing is safe.
var defaultSlashReg = buildSlashCommandRegistry()

// handleSlashCommand dispatches a parsed slash command to the appropriate
// handler via the registry. It emits a tracing span so command invocations are
// observable. It returns the pendingInput string (non-empty when a custom
// Markdown command injects a prompt template for the REPL loop to process).
func (c *interactiveCmd) handleSlashCommand(ctx context.Context, cmd session.SlashCommand, deps Dependencies) string {
	span, spanCtx := tracing.SpanFromContext(ctx, "slash.command", tracing.SpanKindInternal)
	span.SetAttributes(tracing.Attribute{Key: "command_name", Value: cmd.Name})
	defer span.End()

	handler, ok := c.slashReg.Lookup(cmd.Name)
	if !ok {
		fmt.Fprintf(deps.Out(), "Unknown command: /%s. Type /help for available commands.\n", cmd.Name) //nolint:errcheck
		return ""
	}
	pendingInput, err := handler.Handle(spanCtx, cmd.Args, deps)
	if err != nil {
		fmt.Fprintf(deps.Out(), "Error: %v\n", err) //nolint:errcheck
	}
	return pendingInput
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

	// Snapshot revert for file state rollback.
	add(&RevertHandler{})

	// Retry regenerates the last assistant response.
	add(&RetryHandler{})

	// Edit opens an external editor for composing a message.
	add(&EditHandler{})

	// Aliases.
	reg.RegisterAlias("h", "help")
	reg.RegisterAlias("c", "cost")
	reg.RegisterAlias("r", "retry")
	reg.RegisterAlias("vim", "edit")

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
