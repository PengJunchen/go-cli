package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pengjunchen/go-cli/internal/session"
)

// maxHistoryPreview is the maximum number of characters shown for a single
// message or entry when summarizing history.
const maxHistoryPreview = 60

// ----------------------------------------------------------------------------
// Existing commands (migrated from the switch in slash.go)
// ----------------------------------------------------------------------------

// HelpHandler lists all registered slash commands. It reads the handler list
// dynamically from the registry so newly registered commands appear
// automatically.
type HelpHandler struct {
	reg *SlashCommandRegistry
}

var _ SlashCommandHandler = (*HelpHandler)(nil)

func (h *HelpHandler) Name() string        { return "help" }
func (h *HelpHandler) Description() string { return "Show available slash commands" }

func (h *HelpHandler) Handle(_ context.Context, _ []string, sc *slashContext) error {
	fmt.Fprintln(sc.out, "Available commands:")
	if h != nil && h.reg != nil {
		for _, cmd := range h.reg.List() {
			fmt.Fprintf(sc.out, "  /%-8s %s\n", cmd.Name(), cmd.Description())
		}
	}
	fmt.Fprintln(sc.out, "  exit      Exit the interactive session")
	return nil
}

// CostHandler prints the accumulated cost, call count, and per-session
// statistics when available.
type CostHandler struct{}

var _ SlashCommandHandler = (*CostHandler)(nil)

func (h *CostHandler) Name() string        { return "cost" }
func (h *CostHandler) Description() string { return "Show accumulated cost and usage statistics" }

func (h *CostHandler) Handle(_ context.Context, _ []string, sc *slashContext) error {
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
	return nil
}

// CompactHandler manually triggers the compaction hook on the agent's history
// and reports the before/after message counts.
type CompactHandler struct{}

var _ SlashCommandHandler = (*CompactHandler)(nil)

func (h *CompactHandler) Name() string        { return "compact" }
func (h *CompactHandler) Description() string { return "Manually compact conversation history" }

func (h *CompactHandler) Handle(ctx context.Context, _ []string, sc *slashContext) error {
	if sc.agent == nil {
		fmt.Fprintln(sc.out, "Agent not configured.")
		return nil
	}
	before := len(sc.agent.Messages())
	if err := sc.agent.Compact(ctx); err != nil {
		fmt.Fprintf(sc.out, "Compaction failed: %v\n", err)
		return nil
	}
	after := len(sc.agent.Messages())
	fmt.Fprintf(sc.out, "Compacted history: %d -> %d messages\n", before, after)
	return nil
}

// ClearHandler clears the agent's conversation history and confirms the action.
type ClearHandler struct{}

var _ SlashCommandHandler = (*ClearHandler)(nil)

func (h *ClearHandler) Name() string        { return "clear" }
func (h *ClearHandler) Description() string { return "Clear conversation history" }

func (h *ClearHandler) Handle(_ context.Context, _ []string, sc *slashContext) error {
	if sc.agent == nil {
		fmt.Fprintln(sc.out, "Agent not configured.")
		return nil
	}
	sc.agent.ClearHistory()
	fmt.Fprintln(sc.out, "Conversation history cleared.")
	return nil
}

// ToolsHandler lists all registered tools with their names and descriptions.
type ToolsHandler struct{}

var _ SlashCommandHandler = (*ToolsHandler)(nil)

func (h *ToolsHandler) Name() string        { return "tools" }
func (h *ToolsHandler) Description() string { return "List registered tools" }

func (h *ToolsHandler) Handle(ctx context.Context, _ []string, sc *slashContext) error {
	if sc.toolRegistry == nil {
		fmt.Fprintln(sc.out, "Tool registry not configured.")
		return nil
	}
	defs, err := sc.toolRegistry.List(ctx)
	if err != nil {
		fmt.Fprintf(sc.out, "Error listing tools: %v\n", err)
		return nil
	}
	if len(defs) == 0 {
		fmt.Fprintln(sc.out, "No tools registered.")
		return nil
	}
	fmt.Fprintf(sc.out, "Registered tools (%d):\n", len(defs))
	for _, def := range defs {
		fmt.Fprintf(sc.out, "  %s: %s\n", def.Name(), def.Description())
	}
	return nil
}

// ModelHandler prints the name of the model currently in use.
type ModelHandler struct{}

var _ SlashCommandHandler = (*ModelHandler)(nil)

func (h *ModelHandler) Name() string        { return "model" }
func (h *ModelHandler) Description() string { return "Show the current model name" }

func (h *ModelHandler) Handle(_ context.Context, _ []string, sc *slashContext) error {
	fmt.Fprintf(sc.out, "Current model: %s\n", sc.modelName)
	return nil
}

// SessionHandler delegates to the SessionSlashHandler for session tree
// operations (/tree, /fork, /resume). Without sub-arguments it defaults to
// showing the tree.
type SessionHandler struct{}

var _ SlashCommandHandler = (*SessionHandler)(nil)

func (h *SessionHandler) Name() string        { return "session" }
func (h *SessionHandler) Description() string { return "Session operations (subcommands: tree, fork, resume)" }

func (h *SessionHandler) Handle(ctx context.Context, args []string, sc *slashContext) error {
	if sc.sessionHandler == nil {
		fmt.Fprintln(sc.out, "Session tree not configured.")
		return nil
	}
	// Map /session to /tree when no subcommand is given; otherwise use the
	// first argument as the subcommand name (e.g. /session fork name -> fork).
	subCmd := session.SlashCommand{Name: "tree"}
	if len(args) > 0 {
		subCmd.Name = args[0]
		subCmd.Args = args[1:]
	}
	output, err := sc.sessionHandler.Handle(ctx, subCmd)
	if err != nil {
		fmt.Fprintf(sc.out, "Error: %v\n", err)
		return nil
	}
	fmt.Fprint(sc.out, output)
	return nil
}

// ----------------------------------------------------------------------------
// New commands
// ----------------------------------------------------------------------------

// UndoHandler restores the most recent file checkpoint recorded by the
// FileTracker, reverting the last write/edit.
type UndoHandler struct{}

var _ SlashCommandHandler = (*UndoHandler)(nil)

func (h *UndoHandler) Name() string        { return "undo" }
func (h *UndoHandler) Description() string { return "Restore the most recent file checkpoint" }

func (h *UndoHandler) Handle(_ context.Context, _ []string, sc *slashContext) error {
	if sc.fileTracker == nil {
		fmt.Fprintln(sc.out, "File tracking not configured.")
		return nil
	}
	checkpoints := sc.fileTracker.ListCheckpoints()
	if len(checkpoints) == 0 {
		fmt.Fprintln(sc.out, "No checkpoints to undo.")
		return nil
	}
	latest := checkpoints[len(checkpoints)-1]
	if err := sc.fileTracker.Restore(latest.ID); err != nil {
		fmt.Fprintf(sc.out, "Undo failed: %v\n", err)
		return nil
	}
	fmt.Fprintf(sc.out, "Restored %s to checkpoint %s.\n", latest.Path, latest.ID)
	return nil
}

// DiffHandler shows a unified diff of the most recent file change recorded by
// the FileTracker, using the DiffGenerator when available.
type DiffHandler struct{}

var _ SlashCommandHandler = (*DiffHandler)(nil)

func (h *DiffHandler) Name() string        { return "diff" }
func (h *DiffHandler) Description() string { return "Show the diff of the most recent file change" }

func (h *DiffHandler) Handle(_ context.Context, _ []string, sc *slashContext) error {
	if sc.fileTracker == nil {
		fmt.Fprintln(sc.out, "File tracking not configured.")
		return nil
	}
	checkpoints := sc.fileTracker.ListCheckpoints()
	if len(checkpoints) == 0 {
		fmt.Fprintln(sc.out, "No file changes recorded.")
		return nil
	}
	latest := checkpoints[len(checkpoints)-1]

	if sc.diffGenerator == nil {
		fmt.Fprintf(sc.out, "Recent change: %s (checkpoint %s)\n", latest.Path, latest.ID)
		return nil
	}

	// Old content comes from the backup checkpoint; new content is the file's
	// current state on disk.
	oldContent := ""
	if backup, ok := sc.fileTracker.BackupContent(latest.ID); ok {
		oldContent = string(backup)
	}
	newBytes, err := os.ReadFile(latest.Path)
	if err != nil {
		fmt.Fprintf(sc.out, "Recent change: %s (checkpoint %s, current content unreadable: %v)\n", latest.Path, latest.ID, err)
		return nil
	}

	diff, genErr := sc.diffGenerator.Generate(oldContent, string(newBytes), latest.Path)
	if genErr != nil {
		fmt.Fprintf(sc.out, "Diff failed: %v\n", genErr)
		return nil
	}
	if diff == "" {
		fmt.Fprintf(sc.out, "No changes for %s.\n", latest.Path)
		return nil
	}
	fmt.Fprint(sc.out, diff)
	return nil
}

// PlanHandler enters or exits plan mode. With no arguments it toggles the
// current state; "/plan enter [reason]" and "/plan exit [summary]" set it
// explicitly.
type PlanHandler struct{}

var _ SlashCommandHandler = (*PlanHandler)(nil)

func (h *PlanHandler) Name() string        { return "plan" }
func (h *PlanHandler) Description() string { return "Enter or exit plan mode (read-only exploration)" }

func (h *PlanHandler) Handle(ctx context.Context, args []string, sc *slashContext) error {
	if sc.planCtrl == nil {
		fmt.Fprintln(sc.out, "Plan mode not configured.")
		return nil
	}

	if len(args) > 0 && args[0] == "exit" {
		summary := strings.Join(args[1:], " ")
		if err := sc.planCtrl.Exit(ctx, summary); err != nil {
			fmt.Fprintf(sc.out, "Failed to exit plan mode: %v\n", err)
			return nil
		}
		fmt.Fprintln(sc.out, "Exited plan mode.")
		return nil
	}
	if len(args) > 0 && args[0] == "enter" {
		reason := strings.Join(args[1:], " ")
		if err := sc.planCtrl.Enter(ctx, reason); err != nil {
			fmt.Fprintf(sc.out, "Failed to enter plan mode: %v\n", err)
			return nil
		}
		fmt.Fprintf(sc.out, "Entered plan mode: %s\n", reason)
		return nil
	}

	// No explicit subcommand: toggle.
	if sc.planCtrl.IsActive() {
		if err := sc.planCtrl.Exit(ctx, ""); err != nil {
			fmt.Fprintf(sc.out, "Failed to exit plan mode: %v\n", err)
			return nil
		}
		fmt.Fprintln(sc.out, "Exited plan mode.")
		return nil
	}
	if err := sc.planCtrl.Enter(ctx, "user requested"); err != nil {
		fmt.Fprintf(sc.out, "Failed to enter plan mode: %v\n", err)
		return nil
	}
	fmt.Fprintln(sc.out, "Entered plan mode.")
	return nil
}

// ConfigHandler prints a summary of the current application configuration.
type ConfigHandler struct{}

var _ SlashCommandHandler = (*ConfigHandler)(nil)

func (h *ConfigHandler) Name() string        { return "config" }
func (h *ConfigHandler) Description() string { return "Display the current configuration summary" }

func (h *ConfigHandler) Handle(_ context.Context, _ []string, sc *slashContext) error {
	if sc.config == nil {
		fmt.Fprintln(sc.out, "Config not available.")
		return nil
	}
	cfg := sc.config
	fmt.Fprintln(sc.out, "Configuration summary:")
	fmt.Fprintf(sc.out, "  Provider:   %s\n", nonEmpty(cfg.Provider.Name))
	fmt.Fprintf(sc.out, "  Model:      %s\n", nonEmpty(cfg.Model.Name))
	if cfg.Agent.MaxIterations != 0 {
		fmt.Fprintf(sc.out, "  Max iters:  %d\n", cfg.Agent.MaxIterations)
	}
	if cfg.Approval.Mode != "" {
		fmt.Fprintf(sc.out, "  Approval:   %s\n", cfg.Approval.Mode)
	}
	if cfg.Session.StorePath != "" {
		fmt.Fprintf(sc.out, "  Session:    %s\n", cfg.Session.StorePath)
	}
	if cfg.Compaction.Strategy != "" {
		fmt.Fprintf(sc.out, "  Compaction: %s\n", cfg.Compaction.Strategy)
	}
	return nil
}

// nonEmpty returns value, or "(none)" when empty, for display.
func nonEmpty(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

// HistoryHandler prints a summary of the agent's in-memory conversation
// history.
type HistoryHandler struct{}

var _ SlashCommandHandler = (*HistoryHandler)(nil)

func (h *HistoryHandler) Name() string        { return "history" }
func (h *HistoryHandler) Description() string { return "Show conversation history summary" }

func (h *HistoryHandler) Handle(_ context.Context, _ []string, sc *slashContext) error {
	if sc.agent == nil {
		fmt.Fprintln(sc.out, "Agent not configured.")
		return nil
	}
	msgs := sc.agent.Messages()
	if len(msgs) == 0 {
		fmt.Fprintln(sc.out, "No conversation history.")
		return nil
	}
	fmt.Fprintf(sc.out, "Conversation history (%d messages):\n", len(msgs))
	for i, m := range msgs {
		fmt.Fprintf(sc.out, "  %d. [%s] %s\n", i+1, m.Role, truncatePreview(m.Content))
	}
	return nil
}

// SaveHandler explicitly flushes the session store to disk.
type SaveHandler struct{}

var _ SlashCommandHandler = (*SaveHandler)(nil)

func (h *SaveHandler) Name() string        { return "save" }
func (h *SaveHandler) Description() string { return "Explicitly save the current session" }

func (h *SaveHandler) Handle(ctx context.Context, _ []string, sc *slashContext) error {
	if sc.sessionStore == nil {
		fmt.Fprintln(sc.out, "Session store not configured.")
		return nil
	}
	if err := sc.sessionStore.Save(ctx); err != nil {
		fmt.Fprintf(sc.out, "Save failed: %v\n", err)
		return nil
	}
	fmt.Fprintf(sc.out, "Session saved to %s.\n", sc.sessionStore.FilePath())
	return nil
}

// LoadHandler reads the persisted session entries from the store and prints a
// summary of the stored session.
type LoadHandler struct{}

var _ SlashCommandHandler = (*LoadHandler)(nil)

func (h *LoadHandler) Name() string        { return "load" }
func (h *LoadHandler) Description() string { return "Load and summarize the stored session" }

func (h *LoadHandler) Handle(ctx context.Context, _ []string, sc *slashContext) error {
	if sc.sessionStore == nil {
		fmt.Fprintln(sc.out, "Session store not configured.")
		return nil
	}
	entries, err := sc.sessionStore.List(ctx)
	if err != nil {
		fmt.Fprintf(sc.out, "Load failed: %v\n", err)
		return nil
	}
	if len(entries) == 0 {
		fmt.Fprintln(sc.out, "No stored session entries.")
		return nil
	}
	fmt.Fprintf(sc.out, "Loaded %d session entries from %s:\n", len(entries), sc.sessionStore.FilePath())
	for _, e := range entries {
		fmt.Fprintf(sc.out, "  [%s] %s: %s\n", e.Type, e.ID, truncatePreview(e.Content))
	}
	return nil
}

// truncatePreview shortens content to at most maxHistoryPreview characters,
// appending an ellipsis when truncated.
func truncatePreview(content string) string {
	if len(content) <= maxHistoryPreview {
		return content
	}
	return content[:maxHistoryPreview] + "..."
}
