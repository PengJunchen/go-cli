package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

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

func (h *HelpHandler) Handle(_ context.Context, _ []string, deps Dependencies) (string, error) {
	fmt.Fprintln(deps.Out(), "Available commands:") //nolint:errcheck
	if h != nil && h.reg != nil {
		for _, cmd := range h.reg.List() {
			fmt.Fprintf(deps.Out(), "  /%-8s %s\n", cmd.Name(), cmd.Description()) //nolint:errcheck
		}
	}
	fmt.Fprintln(deps.Out(), "  exit      Exit the interactive session") //nolint:errcheck
	return "", nil
}

// CostHandler prints the accumulated cost, call count, and per-session
// statistics when available.
type CostHandler struct{}

var _ SlashCommandHandler = (*CostHandler)(nil)

func (h *CostHandler) Name() string        { return "cost" }
func (h *CostHandler) Description() string { return "Show accumulated cost and usage statistics" }

func (h *CostHandler) Handle(_ context.Context, _ []string, deps Dependencies) (string, error) {
	if deps.CostTracker() != nil {
		fmt.Fprintf(deps.Out(), "Total cost: $%.4f\n", deps.CostTracker().Total()) //nolint:errcheck
		fmt.Fprintf(deps.Out(), "Total calls: %d\n", deps.CostTracker().Calls())   //nolint:errcheck

		// Sub-agent cost breakdown.
		subTotal := deps.CostTracker().SubagentTotal()
		subCalls := deps.CostTracker().SubagentCalls()
		if subCalls > 0 {
			fmt.Fprintf(deps.Out(), "\nSub-agent costs:\n")                 //nolint:errcheck
			fmt.Fprintf(deps.Out(), "  Sub-agent total: $%.4f\n", subTotal) //nolint:errcheck
			fmt.Fprintf(deps.Out(), "  Sub-agent calls: %d\n", subCalls)    //nolint:errcheck
			for _, rec := range deps.CostTracker().SubagentCostSnapshot() { //nolint:errcheck
				fmt.Fprintf(deps.Out(), "    %s: $%.4f (%d calls, %d in / %d out tokens)\n", //nolint:errcheck
					rec.TaskID, rec.Cost, rec.Calls, rec.TokensIn, rec.TokensOut)
			}
		}
	} else {
		fmt.Fprintln(deps.Out(), "Cost tracking not configured.") //nolint:errcheck
	}
	if deps.StatsRegistry() != nil && deps.SessionID() != "" {
		if stats, ok := deps.StatsRegistry().GetSessionStats(deps.SessionID()); ok {
			fmt.Fprintf(deps.Out(), "Session stats:\n")                    //nolint:errcheck
			fmt.Fprintf(deps.Out(), "  Turns:     %d\n", stats.Turns)      //nolint:errcheck
			fmt.Fprintf(deps.Out(), "  Tool calls: %d\n", stats.ToolCalls) //nolint:errcheck
			fmt.Fprintf(deps.Out(), "  Tokens in:  %d\n", stats.TokensIn)  //nolint:errcheck
			fmt.Fprintf(deps.Out(), "  Tokens out: %d\n", stats.TokensOut) //nolint:errcheck
		}
	}
	if deps.ContextWindow() > 0 {
		totalTokens := 0
		if deps.StatsRegistry() != nil && deps.SessionID() != "" {
			if stats, ok := deps.StatsRegistry().GetSessionStats(deps.SessionID()); ok {
				totalTokens = stats.TokensIn + stats.TokensOut
			}
		}
		pct := totalTokens * 100 / deps.ContextWindow()
		fmt.Fprintf(deps.Out(), "  Context:   %d/%d (%d%%)\n", totalTokens, deps.ContextWindow(), pct) //nolint:errcheck
	}
	return "", nil
}

// CompactHandler manually triggers the compaction hook on the agent's history
// and reports the before/after message counts.
type CompactHandler struct{}

var _ SlashCommandHandler = (*CompactHandler)(nil)

func (h *CompactHandler) Name() string        { return "compact" }
func (h *CompactHandler) Description() string { return "Manually compact conversation history" }

func (h *CompactHandler) Handle(ctx context.Context, _ []string, deps Dependencies) (string, error) {
	if deps.Agent() == nil {
		fmt.Fprintln(deps.Out(), "Agent not configured.") //nolint:errcheck
		return "", nil
	}
	before := len(deps.Agent().Messages())
	if err := deps.Agent().Compact(ctx); err != nil {
		fmt.Fprintf(deps.Out(), "Compaction failed: %v\n", err) //nolint:errcheck
		return "", nil
	}
	after := len(deps.Agent().Messages())
	fmt.Fprintf(deps.Out(), "Compacted history: %d -> %d messages\n", before, after) //nolint:errcheck
	return "", nil
}

// ClearHandler clears the agent's conversation history and confirms the action.
type ClearHandler struct{}

var _ SlashCommandHandler = (*ClearHandler)(nil)

func (h *ClearHandler) Name() string        { return "clear" }
func (h *ClearHandler) Description() string { return "Clear conversation history" }

func (h *ClearHandler) Handle(_ context.Context, _ []string, deps Dependencies) (string, error) {
	if deps.Agent() == nil {
		fmt.Fprintln(deps.Out(), "Agent not configured.") //nolint:errcheck
		return "", nil
	}
	deps.Agent().ClearHistory()
	fmt.Fprintln(deps.Out(), "Conversation history cleared.") //nolint:errcheck
	return "", nil
}

// ToolsHandler lists all registered tools with their names and descriptions.
type ToolsHandler struct{}

var _ SlashCommandHandler = (*ToolsHandler)(nil)

func (h *ToolsHandler) Name() string        { return "tools" }
func (h *ToolsHandler) Description() string { return "List registered tools" }

func (h *ToolsHandler) Handle(ctx context.Context, _ []string, deps Dependencies) (string, error) {
	if deps.ToolRegistry() == nil {
		fmt.Fprintln(deps.Out(), "Tool registry not configured.") //nolint:errcheck
		return "", nil
	}
	defs, err := deps.ToolRegistry().List(ctx)
	if err != nil {
		fmt.Fprintf(deps.Out(), "Error listing tools: %v\n", err) //nolint:errcheck
		return "", nil
	}
	if len(defs) == 0 {
		fmt.Fprintln(deps.Out(), "No tools registered.") //nolint:errcheck
		return "", nil
	}
	fmt.Fprintf(deps.Out(), "Registered tools (%d):\n", len(defs)) //nolint:errcheck
	for _, def := range defs {
		fmt.Fprintf(deps.Out(), "  %s: %s\n", def.Name(), def.Description()) //nolint:errcheck
	}
	return "", nil
}

// ModelHandler shows or switches the current model. With no arguments it lists
// available models from the provider, marking the active one. With a model name
// argument it switches to that model at runtime (no restart needed).
type ModelHandler struct{}

var _ SlashCommandHandler = (*ModelHandler)(nil)

func (h *ModelHandler) Name() string { return "model" }
func (h *ModelHandler) Description() string {
	return "Show or switch the current model (/model [name])"
}

func (h *ModelHandler) Handle(ctx context.Context, args []string, deps Dependencies) (string, error) {
	sel := deps.ModelSelector()

	if len(args) == 0 {
		// List available models.
		if sel == nil {
			fmt.Fprintf(deps.Out(), "Current model: %s\n", deps.ModelName()) //nolint:errcheck
			return "", nil
		}
		current := sel.PrimaryModelName()
		models := sel.AvailableModels()
		if len(models) == 0 {
			fmt.Fprintf(deps.Out(), "Current model: %s\n", current) //nolint:errcheck
			return "", nil
		}
		fmt.Fprintln(deps.Out(), "Available models:") //nolint:errcheck
		for _, m := range models {
			marker := "  "
			if m.Name == current {
				marker = "* "
			}
			fmt.Fprintf(deps.Out(), "%s%s", marker, m.Name) //nolint:errcheck
			if m.Description != "" {
				fmt.Fprintf(deps.Out(), " - %s", m.Description) //nolint:errcheck
			}
			fmt.Fprintln(deps.Out()) //nolint:errcheck
		}
		fmt.Fprintf(deps.Out(), "\nCurrent: %s\n", current) //nolint:errcheck
		return "", nil
	}

	// Switch model.
	modelName := args[0]
	if sel == nil {
		return "", fmt.Errorf("model switching not available")
	}
	if err := sel.SwitchModel(ctx, modelName); err != nil {
		return "", fmt.Errorf("switch model: %w", err)
	}
	fmt.Fprintf(deps.Out(), "Switched to model: %s\n", modelName) //nolint:errcheck
	return "", nil
}

// SessionHandler delegates to the SessionSlashHandler for session tree
// operations (/tree, /fork, /resume). Without sub-arguments it defaults to
// showing the tree.
type SessionHandler struct{}

var _ SlashCommandHandler = (*SessionHandler)(nil)

func (h *SessionHandler) Name() string { return "session" }
func (h *SessionHandler) Description() string {
	return "Session operations (subcommands: tree, fork, resume, branches, clone, switch)"
}

// Subcommands returns the subcommands supported by /session for tab completion.
func (h *SessionHandler) Subcommands() []Subcommand {
	return []Subcommand{
		{Name: "tree", Description: "Show session tree (default)"},
		{Name: "fork", Description: "Fork the current session"},
		{Name: "resume", Description: "Resume a previous session"},
		{Name: "branches", Description: "List session branches"},
		{Name: "clone", Description: "Clone the current session"},
		{Name: "switch", Description: "Switch to a different session"},
	}
}

func (h *SessionHandler) Handle(ctx context.Context, args []string, deps Dependencies) (string, error) {
	if deps.SessionHandler() == nil {
		fmt.Fprintln(deps.Out(), "Session tree not configured.") //nolint:errcheck
		return "", nil
	}
	// Map /session to /tree when no subcommand is given; otherwise use the
	// first argument as the subcommand name (e.g. /session fork name -> fork).
	subCmd := session.SlashCommand{Name: "tree"}
	if len(args) > 0 {
		subCmd.Name = args[0]
		subCmd.Args = args[1:]
	}
	output, err := deps.SessionHandler().Handle(ctx, subCmd)
	if err != nil {
		fmt.Fprintf(deps.Out(), "Error: %v\n", err) //nolint:errcheck
		return "", nil
	}
	fmt.Fprint(deps.Out(), output) //nolint:errcheck
	return "", nil
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

func (h *UndoHandler) Handle(_ context.Context, _ []string, deps Dependencies) (string, error) {
	if deps.FileTracker() == nil {
		fmt.Fprintln(deps.Out(), "File tracking not configured.") //nolint:errcheck
		return "", nil
	}
	checkpoints := deps.FileTracker().ListCheckpoints()
	if len(checkpoints) == 0 {
		fmt.Fprintln(deps.Out(), "No checkpoints to undo.") //nolint:errcheck
		return "", nil
	}
	latest := checkpoints[len(checkpoints)-1]
	if err := deps.FileTracker().Restore(latest.ID); err != nil {
		fmt.Fprintf(deps.Out(), "Undo failed: %v\n", err) //nolint:errcheck
		return "", nil
	}
	fmt.Fprintf(deps.Out(), "Restored %s to checkpoint %s.\n", latest.Path, latest.ID) //nolint:errcheck
	return "", nil
}

// DiffHandler shows a unified diff of the most recent file change recorded by
// the FileTracker, using the DiffGenerator when available.
type DiffHandler struct{}

var _ SlashCommandHandler = (*DiffHandler)(nil)

func (h *DiffHandler) Name() string        { return "diff" }
func (h *DiffHandler) Description() string { return "Show the diff of the most recent file change" }

func (h *DiffHandler) Handle(ctx context.Context, _ []string, deps Dependencies) (string, error) {
	if deps.FileTracker() == nil {
		fmt.Fprintln(deps.Out(), "File tracking not configured.") //nolint:errcheck
		return "", nil
	}
	checkpoints := deps.FileTracker().ListCheckpoints()
	if len(checkpoints) == 0 {
		fmt.Fprintln(deps.Out(), "No file changes recorded.") //nolint:errcheck
		return "", nil
	}
	latest := checkpoints[len(checkpoints)-1]

	if deps.DiffGenerator() == nil {
		fmt.Fprintf(deps.Out(), "Recent change: %s (checkpoint %s)\n", latest.Path, latest.ID) //nolint:errcheck
		return "", nil
	}

	// Old content comes from the backup checkpoint; new content is the file's
	// current state on disk.
	oldContent := ""
	if backup, ok := deps.FileTracker().BackupContent(latest.ID); ok {
		oldContent = string(backup)
	}
	newBytes, err := os.ReadFile(latest.Path)
	if err != nil {
		fmt.Fprintf(deps.Out(), "Recent change: %s (checkpoint %s, current content unreadable: %v)\n", latest.Path, latest.ID, err) //nolint:errcheck
		return "", nil
	}

	diff, genErr := deps.DiffGenerator().Generate(ctx, oldContent, string(newBytes), latest.Path)
	if genErr != nil {
		fmt.Fprintf(deps.Out(), "Diff failed: %v\n", genErr) //nolint:errcheck
		return "", nil
	}
	if diff == "" {
		fmt.Fprintf(deps.Out(), "No changes for %s.\n", latest.Path) //nolint:errcheck
		return "", nil
	}
	fmt.Fprint(deps.Out(), diff) //nolint:errcheck
	return "", nil
}

// PlanHandler enters or exits plan mode. With no arguments it toggles the
// current state; "/plan enter [reason]" and "/plan exit [summary]" set it
// explicitly.
type PlanHandler struct{}

var _ SlashCommandHandler = (*PlanHandler)(nil)

func (h *PlanHandler) Name() string        { return "plan" }
func (h *PlanHandler) Description() string { return "Enter or exit plan mode (read-only exploration)" }

// Subcommands returns the subcommands supported by /plan for tab completion.
func (h *PlanHandler) Subcommands() []Subcommand {
	return []Subcommand{
		{Name: "enter", Description: "Enter plan mode"},
		{Name: "exit", Description: "Exit plan mode"},
	}
}

func (h *PlanHandler) Handle(ctx context.Context, args []string, deps Dependencies) (string, error) {
	if deps.PlanCtrl() == nil {
		fmt.Fprintln(deps.Out(), "Plan mode not configured.") //nolint:errcheck
		return "", nil
	}

	if len(args) > 0 && args[0] == "exit" {
		summary := strings.Join(args[1:], " ")
		if err := deps.PlanCtrl().Exit(ctx, summary); err != nil {
			fmt.Fprintf(deps.Out(), "Failed to exit plan mode: %v\n", err) //nolint:errcheck
			return "", nil
		}
		fmt.Fprintln(deps.Out(), "Exited plan mode.") //nolint:errcheck
		return "", nil
	}
	if len(args) > 0 && args[0] == "enter" {
		reason := strings.Join(args[1:], " ")
		if err := deps.PlanCtrl().Enter(ctx, reason); err != nil {
			fmt.Fprintf(deps.Out(), "Failed to enter plan mode: %v\n", err) //nolint:errcheck
			return "", nil
		}
		fmt.Fprintf(deps.Out(), "Entered plan mode: %s\n", reason) //nolint:errcheck
		return "", nil
	}

	// No explicit subcommand: toggle.
	if deps.PlanCtrl().IsActive() {
		if err := deps.PlanCtrl().Exit(ctx, ""); err != nil {
			fmt.Fprintf(deps.Out(), "Failed to exit plan mode: %v\n", err) //nolint:errcheck
			return "", nil
		}
		fmt.Fprintln(deps.Out(), "Exited plan mode.") //nolint:errcheck
		return "", nil
	}
	if err := deps.PlanCtrl().Enter(ctx, "user requested"); err != nil {
		fmt.Fprintf(deps.Out(), "Failed to enter plan mode: %v\n", err) //nolint:errcheck
		return "", nil
	}
	fmt.Fprintln(deps.Out(), "Entered plan mode.") //nolint:errcheck
	return "", nil
}

// ConfigHandler prints a summary of the current application configuration.
type ConfigHandler struct{}

var _ SlashCommandHandler = (*ConfigHandler)(nil)

func (h *ConfigHandler) Name() string        { return "config" }
func (h *ConfigHandler) Description() string { return "Display the current configuration summary" }

func (h *ConfigHandler) Handle(_ context.Context, _ []string, deps Dependencies) (string, error) {
	if deps.Config() == nil {
		fmt.Fprintln(deps.Out(), "Config not available.") //nolint:errcheck
		return "", nil
	}
	cfg := deps.Config()
	fmt.Fprintln(deps.Out(), "Configuration summary:")                         //nolint:errcheck
	fmt.Fprintf(deps.Out(), "  Provider:   %s\n", nonEmpty(cfg.Provider.Name)) //nolint:errcheck
	fmt.Fprintf(deps.Out(), "  Model:      %s\n", nonEmpty(cfg.Model.Name))    //nolint:errcheck
	if cfg.Agent.MaxIterations != 0 {
		fmt.Fprintf(deps.Out(), "  Max iters:  %d\n", cfg.Agent.MaxIterations) //nolint:errcheck
	}
	if cfg.Approval.Mode != "" {
		fmt.Fprintf(deps.Out(), "  Approval:   %s\n", cfg.Approval.Mode) //nolint:errcheck
	}
	if cfg.Session.StorePath != "" {
		fmt.Fprintf(deps.Out(), "  Session:    %s\n", cfg.Session.StorePath) //nolint:errcheck
	}
	if cfg.Compaction.Strategy != "" {
		fmt.Fprintf(deps.Out(), "  Compaction: %s\n", cfg.Compaction.Strategy) //nolint:errcheck
	}
	return "", nil
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

func (h *HistoryHandler) Handle(_ context.Context, _ []string, deps Dependencies) (string, error) {
	if deps.Agent() == nil {
		fmt.Fprintln(deps.Out(), "Agent not configured.") //nolint:errcheck
		return "", nil
	}
	msgs := deps.Agent().Messages()
	if len(msgs) == 0 {
		fmt.Fprintln(deps.Out(), "No conversation history.") //nolint:errcheck
		return "", nil
	}
	fmt.Fprintf(deps.Out(), "Conversation history (%d messages):\n", len(msgs)) //nolint:errcheck
	for i, m := range msgs {
		fmt.Fprintf(deps.Out(), "  %d. [%s] %s\n", i+1, m.Role, truncatePreview(m.Content)) //nolint:errcheck
	}
	return "", nil
}

// SaveHandler explicitly flushes the session store to disk.
type SaveHandler struct{}

var _ SlashCommandHandler = (*SaveHandler)(nil)

func (h *SaveHandler) Name() string        { return "save" }
func (h *SaveHandler) Description() string { return "Explicitly save the current session" }

func (h *SaveHandler) Handle(ctx context.Context, _ []string, deps Dependencies) (string, error) {
	if deps.SessionStore() == nil {
		fmt.Fprintln(deps.Out(), "Session store not configured.") //nolint:errcheck
		return "", nil
	}
	if err := deps.SessionStore().Save(ctx); err != nil {
		fmt.Fprintf(deps.Out(), "Save failed: %v\n", err) //nolint:errcheck
		return "", nil
	}
	fmt.Fprintf(deps.Out(), "Session saved to %s.\n", deps.SessionStore().FilePath()) //nolint:errcheck
	return "", nil
}

// LoadHandler reads the persisted session entries from the store and prints a
// summary of the stored session.
type LoadHandler struct{}

var _ SlashCommandHandler = (*LoadHandler)(nil)

func (h *LoadHandler) Name() string        { return "load" }
func (h *LoadHandler) Description() string { return "Load and summarize the stored session" }

func (h *LoadHandler) Handle(ctx context.Context, _ []string, deps Dependencies) (string, error) {
	if deps.SessionStore() == nil {
		fmt.Fprintln(deps.Out(), "Session store not configured.") //nolint:errcheck
		return "", nil
	}
	entries, err := deps.SessionStore().List(ctx)
	if err != nil {
		fmt.Fprintf(deps.Out(), "Load failed: %v\n", err) //nolint:errcheck
		return "", nil
	}
	if len(entries) == 0 {
		fmt.Fprintln(deps.Out(), "No stored session entries.") //nolint:errcheck
		return "", nil
	}
	fmt.Fprintf(deps.Out(), "Loaded %d session entries from %s:\n", len(entries), deps.SessionStore().FilePath()) //nolint:errcheck
	for _, e := range entries {
		fmt.Fprintf(deps.Out(), "  [%s] %s: %s\n", e.Type, e.ID, truncatePreview(e.Content)) //nolint:errcheck
	}
	return "", nil
}

// truncatePreview shortens content to at most maxHistoryPreview runes,
// appending an ellipsis when truncated. Rune-based slicing avoids splitting
// multi-byte UTF-8 characters (e.g. CJK text) that byte-slicing would corrupt.
func truncatePreview(content string) string {
	if utf8.RuneCountInString(content) <= maxHistoryPreview {
		return content
	}
	runes := []rune(content)
	return string(runes[:maxHistoryPreview]) + "..."
}

// ----------------------------------------------------------------------------
// TUI display commands
// ----------------------------------------------------------------------------

// ThinkingHandler implements the /thinking command, which controls how
// thinking-chain entries are displayed in the TUI. Sub-commands:
//
//	/thinking show     — expand all thinking entries (default)
//	/thinking collapse — fold thinking entries to a one-line summary
//	/thinking hide     — suppress thinking entries entirely
//
// With no argument, /thinking reports the current mode.
type ThinkingHandler struct{}

var _ SlashCommandHandler = (*ThinkingHandler)(nil)

func (h *ThinkingHandler) Name() string { return "thinking" }
func (h *ThinkingHandler) Description() string {
	return "Control thinking-chain display: show, collapse, or hide"
}

// Subcommands returns the subcommands supported by /thinking for tab completion.
func (h *ThinkingHandler) Subcommands() []Subcommand {
	return []Subcommand{
		{Name: "show", Description: "Expand all thinking entries (default)"},
		{Name: "collapse", Description: "Fold thinking entries to a one-line summary"},
		{Name: "hide", Description: "Suppress thinking entries entirely"},
	}
}

func (h *ThinkingHandler) Handle(_ context.Context, args []string, deps Dependencies) (string, error) {
	if len(args) == 0 {
		mode := deps.ThinkingVisibility()
		if mode == "" {
			mode = "show"
		}
		fmt.Fprintf(deps.Out(), "Thinking display mode: %s\n", mode)
		return "", nil
	}
	switch strings.ToLower(args[0]) {
	case "show":
		deps.SetThinkingVisibility("show")
		fmt.Fprintln(deps.Out(), "Thinking entries will be expanded.")
	case "collapse":
		deps.SetThinkingVisibility("collapse")
		fmt.Fprintln(deps.Out(), "Thinking entries will be collapsed to a summary.")
	case "hide":
		deps.SetThinkingVisibility("hide")
		fmt.Fprintln(deps.Out(), "Thinking entries will be hidden.")
	default:
		return "", fmt.Errorf("unknown sub-command %q: use show, collapse, or hide", args[0])
	}
	return "", nil
}

// ThemeHandler implements the /theme command, which lists or switches TUI
// themes at runtime. Sub-commands:
//
//	/theme           — list available themes and mark the active one
//	/theme <name>    — switch to the named theme immediately
//
// When themeMgr is nil (headless mode), the handler prints a friendly message
// instead of crashing.
type ThemeHandler struct{}

var _ SlashCommandHandler = (*ThemeHandler)(nil)

func (h *ThemeHandler) Name() string { return "theme" }
func (h *ThemeHandler) Description() string {
	return "Switch or list TUI themes: dark, light, monokai, solarized"
}

// Subcommands returns the built-in theme names for tab completion.
func (h *ThemeHandler) Subcommands() []Subcommand {
	return []Subcommand{
		{Name: "dark", Description: "Dark theme (default)"},
		{Name: "light", Description: "Light theme"},
		{Name: "monokai", Description: "Monokai theme"},
		{Name: "solarized", Description: "Solarized theme"},
	}
}

func (h *ThemeHandler) Handle(_ context.Context, args []string, deps Dependencies) (string, error) {
	if deps.ThemeMgr() == nil {
		fmt.Fprintln(deps.Out(), "Theme switching is only available in interactive TUI mode.")
		return "", nil
	}
	if len(args) == 0 {
		current := deps.ThemeMgr().CurrentName()
		fmt.Fprintln(deps.Out(), "Available themes:")
		for _, name := range deps.ThemeMgr().Names() {
			marker := ""
			if name == current {
				marker = " (active)"
			}
			fmt.Fprintf(deps.Out(), "  %s%s\n", name, marker)
		}
		return "", nil
	}
	name := strings.TrimSpace(strings.ToLower(args[0]))
	if err := deps.ThemeMgr().Set(name); err != nil {
		fmt.Fprintf(deps.Out(), "Error: %v\nAvailable themes: %s\n", err, strings.Join(deps.ThemeMgr().Names(), ", "))
		return "", nil
	}
	fmt.Fprintf(deps.Out(), "Theme switched to: %s\n", name)
	return "", nil
}
