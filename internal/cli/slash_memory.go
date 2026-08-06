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

func (h *MemoryHandler) Handle(ctx context.Context, args []string, sc *slashContext) error {
	if sc.memoryStore == nil {
		fmt.Fprintln(sc.out, "Memory store not configured.") //nolint:errcheck
		return nil
	}

	subCmd := "list"
	if len(args) > 0 {
		subCmd = args[0]
		args = args[1:]
	}

	switch subCmd {
	case "list":
		return h.handleMemoryList(ctx, sc)
	case "add":
		return h.handleMemoryAdd(ctx, args, sc)
	case "delete":
		return h.handleMemoryDelete(ctx, args, sc)
	case "search":
		return h.handleMemorySearch(ctx, args, sc)
	case "clear":
		return h.handleMemoryClear(ctx, args, sc)
	default:
		fmt.Fprintf(sc.out, "Unknown subcommand: %s\n", subCmd)             //nolint:errcheck
		fmt.Fprintln(sc.out, "Usage: /memory [list|add|delete|search|clear]") //nolint:errcheck
		return nil
	}
}

// handleMemoryList prints all stored memories with ID, category, and a content
// preview.
func (h *MemoryHandler) handleMemoryList(ctx context.Context, sc *slashContext) error {
	memories, err := sc.memoryStore.List(ctx)
	if err != nil {
		fmt.Fprintf(sc.out, "Error listing memories: %v\n", err) //nolint:errcheck
		return nil
	}
	if len(memories) == 0 {
		fmt.Fprintln(sc.out, "No memories stored.") //nolint:errcheck
		return nil
	}
	fmt.Fprintf(sc.out, "Memories (%d):\n", len(memories)) //nolint:errcheck
	for _, m := range memories {
		fmt.Fprintf(sc.out, "  %s  [%s]  %s\n", m.ID, m.Category, truncatePreview(m.Content)) //nolint:errcheck
	}
	return nil
}

// handleMemoryAdd stores a manual memory with category="manual" and
// source="manual".
func (h *MemoryHandler) handleMemoryAdd(ctx context.Context, args []string, sc *slashContext) error {
	if len(args) == 0 {
		fmt.Fprintln(sc.out, "Usage: /memory add <text>") //nolint:errcheck
		return nil
	}
	content := strings.Join(args, " ")
	mem := memory.Memory{
		Content:  content,
		Category: "manual",
		Source:   "manual",
	}
	if err := sc.memoryStore.Add(ctx, mem); err != nil {
		fmt.Fprintf(sc.out, "Error adding memory: %v\n", err) //nolint:errcheck
		return nil
	}
	// Add takes Memory by value, so the generated ID is discovered via List.
	memories, _ := sc.memoryStore.List(ctx)
	for _, m := range memories {
		if m.Content == content && m.Source == "manual" {
			fmt.Fprintf(sc.out, "Added memory %s: %s\n", m.ID, truncatePreview(content)) //nolint:errcheck
			return nil
		}
	}
	fmt.Fprintf(sc.out, "Added memory: %s\n", truncatePreview(content)) //nolint:errcheck
	return nil
}

// handleMemoryDelete removes a memory by its ID.
func (h *MemoryHandler) handleMemoryDelete(ctx context.Context, args []string, sc *slashContext) error {
	if len(args) == 0 {
		fmt.Fprintln(sc.out, "Usage: /memory delete <id>") //nolint:errcheck
		return nil
	}
	id := args[0]
	if err := sc.memoryStore.Delete(ctx, id); err != nil {
		fmt.Fprintf(sc.out, "Error deleting memory: %v\n", err) //nolint:errcheck
		return nil
	}
	fmt.Fprintf(sc.out, "Deleted memory %s.\n", id) //nolint:errcheck
	return nil
}

// handleMemorySearch searches memories by keyword using the store's TF-IDF
// search and prints up to 10 results.
func (h *MemoryHandler) handleMemorySearch(ctx context.Context, args []string, sc *slashContext) error {
	if len(args) == 0 {
		fmt.Fprintln(sc.out, "Usage: /memory search <query>") //nolint:errcheck
		return nil
	}
	query := strings.Join(args, " ")
	results, err := sc.memoryStore.Search(ctx, query, 10)
	if err != nil {
		fmt.Fprintf(sc.out, "Error searching memories: %v\n", err) //nolint:errcheck
		return nil
	}
	if len(results) == 0 {
		fmt.Fprintln(sc.out, "No matching memories.") //nolint:errcheck
		return nil
	}
	fmt.Fprintf(sc.out, "Search results (%d):\n", len(results)) //nolint:errcheck
	for _, m := range results {
		fmt.Fprintf(sc.out, "  %s  [%s]  %s\n", m.ID, m.Category, truncatePreview(m.Content)) //nolint:errcheck
	}
	return nil
}

// handleMemoryClear deletes all memories. It requires a "confirm" argument to
// prevent accidental data loss.
func (h *MemoryHandler) handleMemoryClear(ctx context.Context, args []string, sc *slashContext) error {
	if len(args) == 0 || args[0] != "confirm" {
		fmt.Fprintln(sc.out, "This will delete ALL memories. Type /memory clear confirm to proceed.") //nolint:errcheck
		return nil
	}
	memories, err := sc.memoryStore.List(ctx)
	if err != nil {
		fmt.Fprintf(sc.out, "Error listing memories: %v\n", err) //nolint:errcheck
		return nil
	}
	for _, m := range memories {
		if delErr := sc.memoryStore.Delete(ctx, m.ID); delErr != nil {
			fmt.Fprintf(sc.out, "Error deleting memory %s: %v\n", m.ID, delErr) //nolint:errcheck
		}
	}
	fmt.Fprintf(sc.out, "Cleared %d memories.\n", len(memories)) //nolint:errcheck
	return nil
}
