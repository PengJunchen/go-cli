package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pengjunchen/go-cli/internal/skill"
)

// CapabilityBackend is the terminal-facing capability Interface.
type CapabilityBackend interface {
	CapabilityStatus(context.Context) (CapabilityStatus, error)
	ReloadCapabilities(context.Context) error
	LoadMCPServer(context.Context, string) error
}

func (sc *slashContext) CapabilityBackend() CapabilityBackend { return sc.capabilities }

type capabilityHandler struct{ name string }

func (h *capabilityHandler) Name() string { return h.name }
func (h *capabilityHandler) Description() string {
	return "Inspect, search or refresh progressive skills and MCP capabilities"
}
func (h *capabilityHandler) Handle(ctx context.Context, args []string, deps Dependencies) (string, error) {
	provider, ok := deps.(interface{ CapabilityBackend() CapabilityBackend })
	if !ok || provider.CapabilityBackend() == nil {
		return "", errors.New("cli: capability commands are unavailable")
	}
	backend := provider.CapabilityBackend()
	if h.name == "reload" || (len(args) > 0 && args[0] == "reload") {
		if err := backend.ReloadCapabilities(ctx); err != nil {
			return "", err
		}
		if _, err := fmt.Fprintln(deps.Out(), "Skill, prompt and hook plans refreshed for the next run. MCP server configuration is indexed; use /mcp reload <server> to replace an active server."); err != nil {
			return "", err
		}
		return "", nil
	} //nolint:errcheck
	if h.name == "skills" && len(args) == 2 && args[0] == "load" {
		if loader, ok := backend.(interface {
			LoadSkillInstructions(context.Context, string) (string, error)
		}); ok {
			return loader.LoadSkillInstructions(ctx, args[1])
		}
	}
	status, err := backend.CapabilityStatus(ctx)
	if err != nil {
		return "", err
	}
	if h.name == "skills" {
		query := strings.Join(args, " ")
		if len(args) > 0 && args[0] == "search" {
			query = strings.Join(args[1:], " ")
		}
		entries := status.Skills
		if search, ok := backend.(interface {
			SearchSkills(context.Context, string) ([]skill.Descriptor, error)
		}); ok {
			entries, err = search.SearchSkills(ctx, query)
			if err != nil {
				return "", err
			}
		}
		for _, entry := range entries {
			if query == "" || strings.Contains(strings.ToLower(entry.Name+" "+entry.Description), strings.ToLower(query)) {
				if _, err := fmt.Fprintf(deps.Out(), "%s — %s\n", entry.Name, entry.Description); err != nil {
					return "", err
				}
			}
		} //nolint:errcheck
		return "", nil
	}
	fmt.Fprintln(deps.Out(), formatCapabilityStatus(status)) //nolint:errcheck
	return "", nil
}
