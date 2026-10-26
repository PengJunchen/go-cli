package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	mcpListCommand   = "list"
	mcpLoadCommand   = "load"
	mcpReloadCommand = "reload"
)

// MCPHandler lists configured lazy servers, activates one server, or refreshes
// one lazy server at an idle boundary. Bare reload preserves the existing eager
// hot-reloader operation when that compatible Assembly owns reloaders.
type MCPHandler struct{}

var _ SlashCommandHandler = (*MCPHandler)(nil)
var _ SubcommandProvider = (*MCPHandler)(nil)

type mcpSubcommand struct {
	Subcommand
	arguments string
	handle    func(context.Context, []string, Dependencies) error
}

// mcpSubcommands is the canonical routing, help, and completion registration.
func mcpSubcommands() []mcpSubcommand {
	return []mcpSubcommand{
		{Subcommand: Subcommand{Name: mcpListCommand, Description: "List configured server disclosure states"}, handle: handleMCPList},
		{Subcommand: Subcommand{Name: mcpLoadCommand, Description: "Activate one configured lazy server"}, arguments: " <server>", handle: handleMCPLoad},
		{Subcommand: Subcommand{Name: mcpReloadCommand, Description: "Refresh one lazy server, or trigger owned eager reloaders"}, arguments: " [server]", handle: handleMCPReload},
	}
}

func (h *MCPHandler) Name() string        { return "mcp" }
func (h *MCPHandler) Description() string { return "List, activate, or explicitly refresh MCP servers" }

// Subcommands exposes the same registrations consumed by Handle.
func (h *MCPHandler) Subcommands() []Subcommand {
	entries := mcpSubcommands()
	result := make([]Subcommand, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Subcommand)
	}
	return result
}

func mcpUsage() string {
	entries := mcpSubcommands()
	forms := make([]string, 0, len(entries))
	for _, entry := range entries {
		forms = append(forms, entry.Name+entry.arguments)
	}
	return "Usage: /mcp " + strings.Join(forms, " | ")
}

func (h *MCPHandler) Handle(ctx context.Context, args []string, deps Dependencies) (string, error) {
	name := mcpListCommand
	if len(args) == 0 {
		if mcpCapabilityBackend(deps) == nil {
			_, err := fmt.Fprintln(deps.Out(), mcpUsage())
			return "", err
		}
	} else {
		name, args = args[0], args[1:]
	}
	for _, entry := range mcpSubcommands() {
		if entry.Name == name {
			return "", entry.handle(ctx, args, deps)
		}
	}
	_, err := fmt.Fprintf(deps.Out(), "Unknown subcommand: /mcp %s\n%s\n", name, mcpUsage())
	return "", err
}

func mcpCapabilityBackend(deps Dependencies) CapabilityBackend {
	if provider, ok := deps.(interface{ CapabilityBackend() CapabilityBackend }); ok {
		return provider.CapabilityBackend()
	}
	return nil
}

func handleMCPList(ctx context.Context, args []string, deps Dependencies) error {
	if len(args) != 0 {
		return errors.New(mcpUsage())
	}
	backend := mcpCapabilityBackend(deps)
	if backend == nil {
		return errors.New("cli: lazy MCP capability commands are unavailable")
	}
	status, err := backend.CapabilityStatus(ctx)
	if err != nil {
		return err
	}
	for _, server := range status.MCP {
		if _, err := fmt.Fprintf(deps.Out(), "%s: %s\n", server.Name, server.State); err != nil {
			return err
		}
	}
	return nil
}

func handleMCPLoad(ctx context.Context, args []string, deps Dependencies) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: /mcp %s <server>", mcpLoadCommand)
	}
	backend := mcpCapabilityBackend(deps)
	if backend == nil {
		return errors.New("cli: lazy MCP capability commands are unavailable")
	}
	if err := backend.LoadMCPServer(ctx, args[0]); err != nil {
		return err
	}
	return handleMCPList(ctx, nil, deps)
}

func handleMCPReload(ctx context.Context, args []string, deps Dependencies) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: /mcp %s [server]", mcpReloadCommand)
	}
	if len(args) == 1 {
		refresher, ok := mcpCapabilityBackend(deps).(interface {
			RefreshMCPServer(context.Context, string) error
		})
		if !ok {
			return errors.New("cli: named MCP refresh is unavailable; restart or enable lazy loading")
		}
		if err := refresher.RefreshMCPServer(ctx, args[0]); err != nil {
			return err
		}
		_, err := fmt.Fprintln(deps.Out(), "MCP server refreshed for the next run.")
		return err
	}
	reloader, ok := deps.(interface {
		ReloadMCP(context.Context) (int, error)
	})
	if !ok {
		_, err := fmt.Fprintln(deps.Out(), "No active MCP hot reloaders to reload.")
		return err
	}
	count, err := reloader.ReloadMCP(ctx)
	if err != nil {
		return err
	}
	if count == 0 {
		_, err := fmt.Fprintln(deps.Out(), "No active MCP hot reloaders to reload.")
		return err
	}
	_, err = fmt.Fprintf(deps.Out(), "Triggered reload on %d MCP hot reloader(s).\n", count)
	return err
}
