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
	default:
		return "", fmt.Errorf("session: unknown slash command %q", cmd.Name)
	}
}

// handleTree displays the session tree by listing the current branch.
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
	sb.WriteString(fmt.Sprintf("Session tree (current leaf: %s, %d entries):\n", leaf, len(branch)))
	for _, e := range branch {
		sb.WriteString(fmt.Sprintf("  [%s] %s: %s\n", e.Type, e.ID, truncateForDisplay(e.Content, 60)))
	}
	return sb.String(), nil
}

// handleFork creates a new branch from the current position. The first argument
// is used as the branch name; when absent a default name is generated.
func (h *SessionSlashHandler) handleFork(ctx context.Context, args []string) (string, error) {
	if h.tree == nil {
		return "", fmt.Errorf("session: no session tree configured")
	}
	leaf := h.tree.CurrentLeaf()
	if leaf == "" {
		return "", fmt.Errorf("session: cannot fork from an empty tree")
	}
	branchName := "fork-" + leaf
	if len(args) > 0 && args[0] != "" {
		branchName = args[0]
	}
	if err := h.tree.Branch(ctx, leaf, WithBranchID(branchName)); err != nil {
		return "", fmt.Errorf("session: fork failed: %w", err)
	}
	msg := fmt.Sprintf("Forked branch %q from %q", branchName, leaf)
	slog.Info("session.slash.fork", "branch", branchName, "from", leaf)
	return msg, nil
}

// handleResume resumes a previous session by moving the current leaf to the
// given session/entry id.
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
	msg := fmt.Sprintf("Resumed session at %q", target)
	slog.Info("session.slash.resume", "target", target)
	return msg, nil
}

// truncateForDisplay shortens content to at most maxLen characters, appending an
// ellipsis when truncated.
func truncateForDisplay(content string, maxLen int) string {
	if len(content) <= maxLen {
		return content
	}
	return content[:maxLen] + "..."
}
