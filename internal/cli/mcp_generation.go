package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func registerMCPToolsOwned(ctx context.Context, rc *config.Config, registry tools.DeferredRegistry, owner *assemblyMCPResources) error {
	return registerMCPSource(ctx, rc, registry, owner)
}

func registerMCPSource(ctx context.Context, rc *config.Config, registry tools.DeferredRegistry, owner *assemblyMCPResources) error {
	dtr, ok := registry.(*tools.DeferredToolRegistryAdapter)
	if !ok || dtr == nil {
		return tools.ErrMCPGenerationUnavailable
	}
	source, err := selectMCPServerSource(ctx, rc, nil)
	if err != nil {
		return err
	}
	if source == nil || source.kind == mcpServerSourceNone {
		return nil
	}

	providedOwner := owner != nil
	if owner == nil {
		owner = &assemblyMCPResources{}
	}

	servers := cloneMCPServerConfigs(source.servers)
	revision := source.initialRevision
	initialErr := source.initialErr
	if initialErr == nil {
		servers, revision, initialErr = source.loadCurrent(ctx)
	}
	if revision != "" {
		source.initialRevision = revision
	}
	if initialErr != nil {
		slog.Warn("assemble_mcp_generation_initial_rejected", "error_class", mcpGenerationErrorClass(initialErr))
	} else if err := replaceMCPSourceGeneration(ctx, source, servers, dtr, owner); err != nil {
		slog.Warn("assemble_mcp_generation_initial_rejected", "error_class", mcpGenerationErrorClass(err))
	} else {
		slog.Info("assemble_mcp_generation_published", "servers", len(servers))
	}

	if !providedOwner || !source.reloadable() {
		return nil
	}
	reloader := mcp.NewConfigHotReloader(func(reloadCtx context.Context, _ string) error {
		servers, _, loadErr := source.loadCurrent(reloadCtx)
		if loadErr != nil {
			if source.projectOwned() && errors.Is(loadErr, errMCPSourceUnverified) {
				return disableMCPSourceGeneration(reloadCtx, source, dtr, owner, loadErr)
			}
			return loadErr
		}
		return replaceMCPSourceGeneration(reloadCtx, source, servers, dtr, owner)
	}, mcp.WithConfigSourceRevision(source.currentRevision, source.initialRevision))
	if err := owner.addReloader(reloader); err != nil {
		_ = reloader.Stop()
		return err
	}
	watchPath := source.path
	if watchPath == "" {
		watchPath = source.projectRoot
	}
	if err := reloader.Watch(ctx, watchPath); err != nil {
		reloader.RequestStop()
		_ = reloader.Wait(context.Background())
		return err
	}
	slog.Info("assemble_mcp_generation_watcher_started", "source", source.id)
	return nil
}

func replaceMCPSourceGeneration(
	ctx context.Context,
	source *mcpServerSource,
	servers []config.MCPServerConfig,
	dtr *tools.DeferredToolRegistryAdapter,
	owner *assemblyMCPResources,
) error {
	generationNumber := owner.nextMCPGeneration(source.id)
	candidate, definitions, err := buildMCPGeneration(ctx, source.id, generationNumber, servers, owner)
	if err != nil {
		return err
	}
	update, err := dtr.BeginMCPGenerationUpdate(ctx, source.id, generationNumber, definitions)
	if err != nil {
		cleanupErr := owner.cleanupMCPCandidate(candidate)
		if errors.Is(err, tools.ErrMCPNameReserved) {
			disableErr := disableMCPSourceAfterCandidateRejection(ctx, source, dtr, owner, generationNumber, err)
			return errors.Join(disableErr, cleanupErr)
		}
		return errors.Join(err, cleanupErr)
	}
	if err := owner.publishMCPGeneration(candidate, update); err != nil {
		cleanupErr := owner.cleanupMCPCandidate(candidate)
		return errors.Join(err, cleanupErr)
	}
	return nil
}

func disableMCPSourceGeneration(
	ctx context.Context,
	source *mcpServerSource,
	dtr *tools.DeferredToolRegistryAdapter,
	owner *assemblyMCPResources,
	cause error,
) error {
	generationNumber := owner.nextMCPGeneration(source.id)
	update, err := dtr.BeginMCPGenerationUpdate(ctx, source.id, generationNumber, nil)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("cli: begin fail-closed MCP source disable: %w", err))
	}
	if err := owner.disableMCPGeneration(source.id, generationNumber, update); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func disableMCPSourceAfterCandidateRejection(
	ctx context.Context,
	source *mcpServerSource,
	dtr *tools.DeferredToolRegistryAdapter,
	owner *assemblyMCPResources,
	generationNumber uint64,
	cause error,
) error {
	update, err := dtr.BeginMCPGenerationUpdate(ctx, source.id, generationNumber, nil)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("cli: begin fail-closed MCP collision disable: %w", err))
	}
	if err := owner.disableMCPGeneration(source.id, generationNumber, update); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func buildMCPGeneration(
	ctx context.Context,
	sourceID string,
	generationNumber uint64,
	servers []config.MCPServerConfig,
	owner *assemblyMCPResources,
) (*assemblyMCPGeneration, []tools.MCPToolCandidate, error) {
	if err := validateMCPServerSet(servers); err != nil {
		return nil, nil, err
	}
	candidate := newAssemblyMCPGeneration(sourceID, generationNumber)
	clients := make([]mcp.MCPClient, len(servers))
	clientGenerations := make([]*mcp.MCPClientGeneration, len(servers))
	for index, server := range servers {
		client, err := newMCPClientForAssembly(server)
		if err != nil {
			return nil, nil, errors.Join(err, owner.cleanupMCPCandidate(candidate))
		}
		generation, err := mcp.NewMCPClientGeneration(client)
		if err != nil {
			candidate.clients = append(candidate.clients, client)
			return nil, nil, errors.Join(err, owner.cleanupMCPCandidate(candidate))
		}
		clients[index] = client
		clientGenerations[index] = generation
		candidate.clients = append(candidate.clients, client)
		candidate.leases = append(candidate.leases, generation)
	}
	if err := owner.stageMCPGeneration(candidate); err != nil {
		return nil, nil, errors.Join(err, owner.cleanupMCPCandidate(candidate))
	}

	serverTools := make([][]tools.MCPToolCandidate, len(servers))
	g, groupCtx := errgroup.WithContext(ctx)
	for index := range servers {
		index := index
		g.Go(func() error {
			client := clients[index]
			if err := client.Connect(groupCtx); err != nil {
				return fmt.Errorf("cli: MCP candidate connect failed: %w", err)
			}
			remoteTools, err := client.ListTools(groupCtx)
			if err != nil {
				return fmt.Errorf("cli: MCP candidate tool listing failed: %w", err)
			}
			definitions := make([]tools.MCPToolCandidate, 0, len(remoteTools))
			for _, remoteTool := range remoteTools {
				if strings.TrimSpace(remoteTool.Name) == "" {
					return errors.Join(errMCPSourceMalformed, errors.New("cli: MCP tool name is empty"))
				}
				adapter := clientGenerations[index].NewToolAdapter(remoteTool)
				if adapter == nil {
					return mcp.ErrMCPClientGenerationRetired
				}
				toolCandidate, candidateErr := adapter.MCPGenerationCandidate()
				if candidateErr != nil {
					return fmt.Errorf("cli: MCP candidate schema rejected: %w", candidateErr)
				}
				definitions = append(definitions, toolCandidate)
			}
			serverTools[index] = definitions
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, errors.Join(err, owner.cleanupMCPCandidate(candidate))
	}
	definitions := make([]tools.MCPToolCandidate, 0)
	for _, group := range serverTools {
		definitions = append(definitions, group...)
	}
	return candidate, definitions, nil
}

func newMCPClientForAssembly(server config.MCPServerConfig) (mcp.MCPClient, error) {
	clientConfig := mcp.MCPServerConfig{Name: server.Name}
	switch {
	case strings.TrimSpace(server.Command) != "" && strings.TrimSpace(server.URL) == "":
		clientConfig.Transport = mcp.MCPTransportStdio
		clientConfig.Command = server.Command
		clientConfig.Args = append([]string(nil), server.Args...)
		keys := make([]string, 0, len(server.Env))
		for key := range server.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			clientConfig.Env = append(clientConfig.Env, key+"="+server.Env[key])
		}
		return mcp.NewOfficialSDKAdapter(clientConfig), nil
	case strings.TrimSpace(server.URL) != "" && strings.TrimSpace(server.Command) == "":
		parsed, err := url.ParseRequestURI(server.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, errMCPSourceMalformed
		}
		clientConfig.Transport = mcp.MCPTransportStreamableHTTP
		clientConfig.URL = server.URL
		return mcp.NewHTTPClientAdapter(clientConfig), nil
	default:
		return nil, errMCPSourceMalformed
	}
}

func validateMCPServerSet(servers []config.MCPServerConfig) error {
	seen := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		if strings.TrimSpace(server.Name) == "" {
			return errors.Join(errMCPSourceMalformed, errors.New("cli: MCP server name is empty"))
		}
		if _, exists := seen[server.Name]; exists {
			return errors.Join(errMCPSourceMalformed, errors.New("cli: duplicate MCP server name"))
		}
		seen[server.Name] = struct{}{}
		command := strings.TrimSpace(server.Command)
		serverURL := strings.TrimSpace(server.URL)
		if (command == "") == (serverURL == "") {
			return errMCPSourceMalformed
		}
		if serverURL != "" {
			parsed, err := url.ParseRequestURI(server.URL)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				return errMCPSourceMalformed
			}
		}
	}
	return nil
}

func mcpGenerationErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, errMCPSourceUnverified):
		return "project_source_unverified"
	case errors.Is(err, errMCPSourceMalformed):
		return "source_malformed"
	case errors.Is(err, errMCPSourceGlobalRejected):
		return "global_source_rejected"
	case errors.Is(err, tools.ErrMCPNameReserved):
		return "name_reserved"
	default:
		return "candidate_error"
	}
}
