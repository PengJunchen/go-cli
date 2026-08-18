package cli

import (
	"context"
	"fmt"
)

// MCPHandler implements the /mcp slash command, which provides MCP server
// management subcommands. Currently supported subcommands:
//
//	/mcp reload — manually trigger a reconnect and tool re-registration on
//	               every active MCP HotReloader.
type MCPHandler struct{}

var _ SlashCommandHandler = (*MCPHandler)(nil)

func (h *MCPHandler) Name() string        { return "mcp" }
func (h *MCPHandler) Description() string { return "MCP server management (e.g. /mcp reload)" }

func (h *MCPHandler) Handle(ctx context.Context, args []string, deps Dependencies) (string, error) {
	out := deps.Out()

	if len(args) == 0 {
		fmt.Fprintln(out, "Usage: /mcp reload") //nolint:errcheck
		return "", nil
	}

	switch args[0] {
	case "reload":
		count := ReloadAllHotReloaders(ctx)
		if count == 0 {
			fmt.Fprintln(out, "No active MCP hot reloaders to reload.") //nolint:errcheck
			return "", nil
		}
		fmt.Fprintf(out, "Triggered reload on %d MCP hot reloader(s).\n", count) //nolint:errcheck
		return "", nil
	default:
		fmt.Fprintf(out, "Unknown subcommand: /mcp %s\nUsage: /mcp reload\n", args[0]) //nolint:errcheck
		return "", nil
	}
}
