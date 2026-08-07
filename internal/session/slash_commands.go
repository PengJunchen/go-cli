package session

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// SlashCommand represents a parsed session slash command.
type SlashCommand struct {
	Name string
	Args []string
}

// ParseSlashCommand parses input like "/tree" or "/fork branch-1". It returns
// the parsed command and true when the input starts with "/"; otherwise it
// returns false indicating the input is not a slash command.
func ParseSlashCommand(input string) (SlashCommand, bool) {
	input = strings.TrimSpace(input)
	if input == "" || !strings.HasPrefix(input, "/") {
		return SlashCommand{}, false
	}
	// Strip the leading "/".
	body := input[1:]
	if body == "" {
		return SlashCommand{}, false
	}
	parts := strings.Fields(body)
	if len(parts) == 0 {
		return SlashCommand{}, false
	}
	return SlashCommand{
		Name: parts[0],
		Args: parts[1:],
	}, true
}

// String returns the canonical rendering of the command (e.g. "/fork name").
func (c SlashCommand) String() string {
	if len(c.Args) == 0 {
		return "/" + c.Name
	}
	return "/" + c.Name + " " + strings.Join(c.Args, " ")
}

// SessionSlashHandler handles session-related slash commands: /tree, /fork, and
// /resume. It operates on a SessionTree and SessionStore.
type SessionSlashHandler struct {
	tree  SessionTree
	store SessionStore
	// OnResume, when non-nil, is invoked after a successful /resume with the
	// rebuilt branch context so callers (e.g. the interactive REPL) can inject
	// the history into the agent.
	OnResume func(ctx context.Context, entries []SessionEntry) error
}

// Compile-time assertion that SessionSlashHandler is a usable type.
var _ = (*SessionSlashHandler)(nil)

// NewSessionSlashHandler returns a handler bound to the given tree and store.
func NewSessionSlashHandler(tree SessionTree, store SessionStore) *SessionSlashHandler {
	return &SessionSlashHandler{tree: tree, store: store}
}

// Handle executes the given slash command. It returns the textual output of the
// command and any error.
func (h *SessionSlashHandler) Handle(ctx context.Context, cmd SlashCommand) (string, error) {
	slog.Info("session.slash.handle", "command", cmd.Name, "args", cmd.Args)

	switch cmd.Name {
	case "tree":
		return h.handleTree(ctx)
	case "fork":
		return h.handleFork(ctx, cmd.Args)
	case "resume":
		return h.handleResume(ctx, cmd.Args)
	case "branches":
		return h.handleBranches(ctx)
	case "clone":
		return h.handleClone(ctx, cmd.Args)
	case "switch":
		return h.handleSwitch(ctx, cmd.Args)
	default:
		return "", fmt.Errorf("session: unknown slash command %q", cmd.Name)
	}
}

// handleTree displays the session tree by listing the current branch along with
// all recorded branches.
func (h *SessionSlashHandler) handleTree(ctx context.Context) (string, error) {
	if h.tree == nil {
		return "", fmt.Errorf("session: no session tree configured")
	}
	leaf := h.tree.CurrentLeaf()
	if leaf == "" {
		return "(empty session tree)", nil
	}
	branch, err := h.tree.GetBranch(ctx, leaf)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	branches := h.tree.ListBranches()
	sb.WriteString(fmt.Sprintf("Session tree (current leaf: %s, %d entries, %d branches):\n", leaf, len(branch), len(branches)))
	// Show branch list.
	for _, b := range branches {
		marker := ""
		if b.BaseLeafID == leaf {
			marker = " (current)"
		}
		sb.WriteString(fmt.Sprintf("  [branch] %s <- %s%s\n", b.BranchID, b.BaseLeafID, marker))
	}
	// Show entries.
	for _, e := range branch {
		sb.WriteString(fmt.Sprintf("  [%s] %s: %s\n", e.Type, e.ID, truncateForDisplay(e.Content, 60)))
	}
	return sb.String(), nil
}

// handleFork creates a new branch from the current position. The first
// non-flag argument is used as the branch name; when absent a default name is
// generated. The --git <branch> flag creates an associated git branch.
func (h *SessionSlashHandler) handleFork(ctx context.Context, args []string) (string, error) {
	if h.tree == nil {
		return "", fmt.Errorf("session: no session tree configured")
	}
	leaf := h.tree.CurrentLeaf()
	if leaf == "" {
		return "", fmt.Errorf("session: cannot fork from an empty tree")
	}

	// Parse flags: --git <branch> associates a git branch with the fork.
	var gitBranch string
	var positional []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--git" && i+1 < len(args) {
			gitBranch = args[i+1]
			i++ // skip the git branch name
		} else if strings.HasPrefix(args[i], "--git=") {
			gitBranch = strings.TrimPrefix(args[i], "--git=")
		} else {
			positional = append(positional, args[i])
		}
	}

	branchName := "fork-" + leaf
	if len(positional) > 0 && positional[0] != "" {
		branchName = positional[0]
	}

	var opts []BranchOption
	opts = append(opts, WithBranchID(branchName))
	if gitBranch != "" {
		opts = append(opts, WithGitBranch(gitBranch))
	}

	if err := h.tree.Branch(ctx, leaf, opts...); err != nil {
		return "", fmt.Errorf("session: fork failed: %w", err)
	}
	msg := fmt.Sprintf("Forked branch %q from %q", branchName, leaf)
	if gitBranch != "" {
		msg += fmt.Sprintf(" (git branch: %s)", gitBranch)
	}
	slog.Info("session.slash.fork", "branch", branchName, "from", leaf, "git_branch", gitBranch)
	return msg, nil
}

// handleResume resumes a previous session by moving the current leaf to the
// given session/entry id, rebuilding the branch context, and invoking the
// OnResume callback (if set) so the caller can restore agent history.
func (h *SessionSlashHandler) handleResume(ctx context.Context, args []string) (string, error) {
	if h.tree == nil {
		return "", fmt.Errorf("session: no session tree configured")
	}
	if len(args) == 0 || args[0] == "" {
		return "", fmt.Errorf("session: /resume requires a session id")
	}
	target := args[0]
	if err := h.tree.MoveTo(ctx, target); err != nil {
		return "", fmt.Errorf("session: resume failed: %w", err)
	}

	// Rebuild the agent context from the target branch.
	sessCtx, err := h.tree.BuildContext(ctx, target)
	if err != nil {
		return "", fmt.Errorf("session: resume context rebuild failed: %w", err)
	}

	// Invoke the OnResume callback so the caller can restore agent history.
	if h.OnResume != nil {
		if err := h.OnResume(ctx, sessCtx.Messages); err != nil {
			return "", fmt.Errorf("session: resume callback failed: %w", err)
		}
	}

	msg := fmt.Sprintf("Resumed session at %q (%d messages)", target, len(sessCtx.Messages))
	slog.Info("session.slash.resume", "target", target, "messages", len(sessCtx.Messages))
	return msg, nil
}

// handleBranches lists all branches recorded in the session tree, marking the
// branch whose base leaf matches the current leaf.
func (h *SessionSlashHandler) handleBranches(ctx context.Context) (string, error) {
	if h.tree == nil {
		return "", fmt.Errorf("session: no session tree configured")
	}
	branches := h.tree.ListBranches()
	if len(branches) == 0 {
		return "No branches in session tree.", nil
	}
	var sb strings.Builder
	currentLeaf := h.tree.CurrentLeaf()
	sb.WriteString(fmt.Sprintf("Branches (%d):\n", len(branches)))
	for _, b := range branches {
		marker := ""
		if b.BaseLeafID == currentLeaf {
			marker = " *"
		}
		sb.WriteString(fmt.Sprintf("  %s (base: %s, created: %s)%s\n", b.BranchID, b.BaseLeafID, b.CreatedAt.Format("2006-01-02 15:04:05"), marker))
	}
	return sb.String(), nil
}

// handleClone deep-copies entries from one branch into a new branch id. It
// requires exactly two arguments: <from-branch-id> <new-branch-id>.
func (h *SessionSlashHandler) handleClone(ctx context.Context, args []string) (string, error) {
	if h.tree == nil {
		return "", fmt.Errorf("session: no session tree configured")
	}
	if len(args) < 2 || args[0] == "" || args[1] == "" {
		return "", fmt.Errorf("session: /clone requires <from-branch-id> <new-branch-id>")
	}
	fromID, newID := args[0], args[1]
	if err := h.tree.Clone(ctx, fromID, newID); err != nil {
		return "", fmt.Errorf("session: clone failed: %w", err)
	}
	slog.Info("session.slash.clone", "from", fromID, "new", newID)
	return fmt.Sprintf("Cloned branch %q to %q", fromID, newID), nil
}

// handleSwitch moves the current leaf to the base leaf of the named branch,
// rebuilds the context, and invokes the OnResume callback (like /resume).
func (h *SessionSlashHandler) handleSwitch(ctx context.Context, args []string) (string, error) {
	if h.tree == nil {
		return "", fmt.Errorf("session: no session tree configured")
	}
	if len(args) == 0 || args[0] == "" {
		return "", fmt.Errorf("session: /switch requires a branch id")
	}
	branchID := args[0]
	// Look up the branch's base leaf id via the recorded branch metadata.
	var targetLeaf string
	for _, b := range h.tree.ListBranches() {
		if b.BranchID == branchID {
			targetLeaf = b.BaseLeafID
			break
		}
	}
	if targetLeaf == "" {
		return "", fmt.Errorf("session: branch %q not found", branchID)
	}
	if err := h.tree.MoveTo(ctx, targetLeaf); err != nil {
		return "", fmt.Errorf("session: switch failed: %w", err)
	}

	// Rebuild the agent context from the target branch.
	sessCtx, err := h.tree.BuildContext(ctx, targetLeaf)
	if err != nil {
		return "", fmt.Errorf("session: switch context rebuild failed: %w", err)
	}
	if h.OnResume != nil {
		if err := h.OnResume(ctx, sessCtx.Messages); err != nil {
			return "", fmt.Errorf("session: switch callback failed: %w", err)
		}
	}

	slog.Info("session.slash.switch", "branch", branchID, "leaf", targetLeaf)
	return fmt.Sprintf("Switched to branch %q (%d messages)", branchID, len(sessCtx.Messages)), nil
}

// truncateForDisplay shortens content to at most maxLen characters, appending an
// ellipsis when truncated.
func truncateForDisplay(content string, maxLen int) string {
	if len(content) <= maxLen {
		return content
	}
	return content[:maxLen] + "..."
}
