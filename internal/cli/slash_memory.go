//exempt:scan010
package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/pengjunchen/go-cli/internal/memory"
)

// MemoryHandler manages cross-session memories via /memory subcommands.
// Subcommands: list (default), add, delete, search, clear.
type MemoryHandler struct{}

var _ SlashCommandHandler = (*MemoryHandler)(nil)

func (h *MemoryHandler) Name() string { return "memory" }
func (h *MemoryHandler) Description() string {
	return "Manage cross-session memories: list, add, delete, search, clear"
}

// Subcommands returns the subcommands supported by /memory for tab completion.
func (h *MemoryHandler) Subcommands() []Subcommand {
	return []Subcommand{
		{Name: "list", Description: "List all stored memories"},
		{Name: "add", Description: "Add a manual memory"},
		{Name: "delete", Description: "Delete a memory by ID"},
		{Name: "search", Description: "Search memories by keyword"},
		{Name: "clear", Description: "Delete all memories (requires confirm)"},
	}
}

func (h *MemoryHandler) Handle(ctx context.Context, args []string, deps Dependencies) (string, error) {
	if deps.MemoryStore() == nil {
		fmt.Fprintln(deps.Out(), "Memory store not configured.") //nolint:errcheck
		return "", nil
	}

	subCmd := "list"
	if len(args) > 0 {
		subCmd = args[0]
		args = args[1:]
	}

	switch subCmd {
	case "list":
		return "", h.handleMemoryList(ctx, deps)
	case "add":
		return "", h.handleMemoryAdd(ctx, args, deps)
	case "delete":
		return "", h.handleMemoryDelete(ctx, args, deps)
	case "search":
		return "", h.handleMemorySearch(ctx, args, deps)
	case "clear":
		return "", h.handleMemoryClear(ctx, args, deps)
	default:
		fmt.Fprintf(deps.Out(), "Unknown subcommand: %s\n", subCmd)               //nolint:errcheck
		fmt.Fprintln(deps.Out(), "Usage: /memory [list|add|delete|search|clear]") //nolint:errcheck
		return "", nil
	}
}

// handleMemoryList prints all stored memories with ID, category, and a content
// preview.
func (h *MemoryHandler) handleMemoryList(ctx context.Context, deps Dependencies) error {
	memories, err := deps.MemoryStore().List(ctx)
	if err != nil {
		fmt.Fprintf(deps.Out(), "Error listing memories: %v\n", err) //nolint:errcheck
		return nil
	}
	if len(memories) == 0 {
		fmt.Fprintln(deps.Out(), "No memories stored.") //nolint:errcheck
		return nil
	}
	fmt.Fprintf(deps.Out(), "Memories (%d):\n", len(memories)) //nolint:errcheck
	for _, m := range memories {
		fmt.Fprintf(deps.Out(), "  %s  [%s]  %s\n", m.ID, m.Category, truncatePreview(m.Content)) //nolint:errcheck
	}
	return nil
}

// handleMemoryAdd stores a manual memory with category="manual" and
// source="manual".
func (h *MemoryHandler) handleMemoryAdd(ctx context.Context, args []string, deps Dependencies) error {
	if len(args) == 0 {
		fmt.Fprintln(deps.Out(), "Usage: /memory add <text>") //nolint:errcheck
		return nil
	}
	content := strings.Join(args, " ")
	mem := memory.Memory{
		Content:  content,
		Category: "manual",
		Source:   "manual",
	}
	if err := deps.MemoryStore().Add(ctx, mem); err != nil {
		fmt.Fprintf(deps.Out(), "Error adding memory: %v\n", err) //nolint:errcheck
		return nil
	}
	// Add takes Memory by value, so the generated ID is discovered via List.
	memories, _ := deps.MemoryStore().List(ctx)
	for _, m := range memories {
		if m.Content == content && m.Source == "manual" {
			fmt.Fprintf(deps.Out(), "Added memory %s: %s\n", m.ID, truncatePreview(content)) //nolint:errcheck
			return nil
		}
	}
	fmt.Fprintf(deps.Out(), "Added memory: %s\n", truncatePreview(content)) //nolint:errcheck
	return nil
}

// handleMemoryDelete removes a memory by its ID.
func (h *MemoryHandler) handleMemoryDelete(ctx context.Context, args []string, deps Dependencies) error {
	if len(args) == 0 {
		fmt.Fprintln(deps.Out(), "Usage: /memory delete <id>") //nolint:errcheck
		return nil
	}
	id := args[0]
	if err := deps.MemoryStore().Delete(ctx, id); err != nil {
		fmt.Fprintf(deps.Out(), "Error deleting memory: %v\n", err) //nolint:errcheck
		return nil
	}
	fmt.Fprintf(deps.Out(), "Deleted memory %s.\n", id) //nolint:errcheck
	return nil
}

// handleMemorySearch searches memories by keyword using the store's TF-IDF
// search and prints up to 10 results.
func (h *MemoryHandler) handleMemorySearch(ctx context.Context, args []string, deps Dependencies) error {
	if len(args) == 0 {
		fmt.Fprintln(deps.Out(), "Usage: /memory search <query>") //nolint:errcheck
		return nil
	}
	query := strings.Join(args, " ")
	results, err := deps.MemoryStore().Search(ctx, query, 10)
	if err != nil {
		fmt.Fprintf(deps.Out(), "Error searching memories: %v\n", err) //nolint:errcheck
		return nil
	}
	if len(results) == 0 {
		fmt.Fprintln(deps.Out(), "No matching memories.") //nolint:errcheck
		return nil
	}
	fmt.Fprintf(deps.Out(), "Search results (%d):\n", len(results)) //nolint:errcheck
	for _, m := range results {
		fmt.Fprintf(deps.Out(), "  %s  [%s]  %s\n", m.ID, m.Category, truncatePreview(m.Content)) //nolint:errcheck
	}
	return nil
}

// handleMemoryClear deletes all memories. It requires a "confirm" argument to
// prevent accidental data loss.
func (h *MemoryHandler) handleMemoryClear(ctx context.Context, args []string, deps Dependencies) error {
	if len(args) == 0 || args[0] != "confirm" {
		fmt.Fprintln(deps.Out(), "This will delete ALL memories. Type /memory clear confirm to proceed.") //nolint:errcheck
		return nil
	}
	memories, err := deps.MemoryStore().List(ctx)
	if err != nil {
		fmt.Fprintf(deps.Out(), "Error listing memories: %v\n", err) //nolint:errcheck
		return nil
	}
	for _, m := range memories {
		if delErr := deps.MemoryStore().Delete(ctx, m.ID); delErr != nil {
			fmt.Fprintf(deps.Out(), "Error deleting memory %s: %v\n", m.ID, delErr) //nolint:errcheck
		}
	}
	fmt.Fprintf(deps.Out(), "Cleared %d memories.\n", len(memories)) //nolint:errcheck
	return nil
}
