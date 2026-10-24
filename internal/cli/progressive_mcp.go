package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/capability"
	"github.com/pengjunchen/go-cli/internal/mcp"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// progressiveMCP owns lazy disclosure for a fixed source configuration. New
// servers are additive within a run; refreshing source configuration is an idle
// cutover. No generic call tool bypasses prepared dispatch or MCP leases.
type progressiveMCP struct {
	mu                sync.Mutex
	ctx               context.Context
	stale             map[string]bool
	watchers          map[string]context.CancelFunc
	source            *mcpServerSource
	servers           []config.MCPServerConfig
	loaded            map[string]bool
	diagnostics       map[string]string
	registry          *tools.DeferredToolRegistryAdapter
	owner             *assemblyMCPResources
	limit             int
	validateCandidate func(context.Context, []tools.ToolDefinition) error
}

// MCPServerStatus contains safe metadata; it never includes command/env/URL.
type MCPServerStatus struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

func (s *assembleState) registerConfiguredMCP(dtr *tools.DeferredToolRegistryAdapter) error {
	lazy := s.rc != nil && (s.rc.MCP.Loading == "lazy" || s.rc.Runtime.Profile == "edge")
	if !lazy {
		if dynamicRuntimeEnabled(s.rc) {
			return registerMCPSourceWithReload(s.ctx, s.rc, dtr, s.mcpResources, false)
		}
		return registerMCPToolsOwned(s.ctx, s.rc, dtr, s.mcpResources)
	}
	source, err := selectMCPServerSource(s.ctx, s.rc, nil)
	if err != nil {
		return err
	}
	if source == nil {
		source = &mcpServerSource{kind: mcpServerSourceNone}
	}
	servers, _, err := source.loadCurrent(s.ctx)
	if err != nil {
		return err
	}
	if err := validateMCPServerSet(servers); err != nil {
		return err
	}
	limit := 16
	if s.rc.Runtime.MaxMCPServers > 0 {
		limit = s.rc.Runtime.MaxMCPServers
	}
	p := &progressiveMCP{ctx: s.lifetimeCtx, stale: map[string]bool{}, watchers: map[string]context.CancelFunc{}, source: source, servers: cloneMCPServerConfigs(servers), registry: dtr, owner: s.mcpResources, limit: limit, loaded: map[string]bool{}, diagnostics: map[string]string{}}
	s.progressiveMCP = p
	s.cleanupList = append(s.cleanupList, p.stopWatchers)
	return dtr.Register(s.ctx, &mcpDiscoveryTool{owner: p})
}

func (p *progressiveMCP) List(query string) []MCPServerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listLocked(query)
}
func (p *progressiveMCP) listLocked(query string) []MCPServerStatus {
	var result []MCPServerStatus
	for _, server := range p.servers {
		if query != "" && !strings.Contains(strings.ToLower(server.Name), strings.ToLower(query)) {
			continue
		}
		state := "discovered"
		if p.loaded[server.Name] {
			state = "loaded"
		}
		if p.stale[server.Name] {
			state = "refresh-required"
		}
		if p.diagnostics[server.Name] != "" {
			state = "failed"
		}
		result = append(result, MCPServerStatus{Name: server.Name, State: state, Error: p.diagnostics[server.Name]})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (p *progressiveMCP) Activate(ctx context.Context, name string) error {
	// The short server set is bounded. Serial construction also makes connection
	// setup single-flight and prevents unbounded process creation on discovery.
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activateLocked(ctx, name, false)
}

func (p *progressiveMCP) activateLocked(ctx context.Context, name string, refresh bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.loaded[name] && !refresh {
		return nil
	}
	if !p.loaded[name] && len(p.loaded) >= p.limit {
		return errors.New("cli: MCP active server budget exhausted")
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	candidate, update, _, err := p.prepareActivationLocked(callCtx, name, p.source, p.servers, refresh)
	if err != nil {
		p.diagnostics[name] = mcpGenerationErrorClass(err)
		return fmt.Errorf("cli: MCP discovery: %w", err)
	}
	if err := p.owner.publishMaterializedMCPGeneration(candidate, update); err != nil {
		return errors.Join(err, p.owner.cleanupMCPCandidate(candidate))
	}
	p.finishActivationLocked(name, candidate.sourceID)
	return nil
}

func (p *progressiveMCP) finishActivationLocked(name, sourceID string) {
	p.loaded[name] = true
	delete(p.diagnostics, name)
	delete(p.stale, name)
	p.watchListChangesLocked(name, sourceID)
}

// prepareActivationLocked leaves all public registries and the current owner
// untouched. A successful return retains the DTR transaction until commit/abort.
func (p *progressiveMCP) prepareActivationLocked(ctx context.Context, name string, source *mcpServerSource, servers []config.MCPServerConfig, refresh bool) (*assemblyMCPGeneration, *tools.MCPGenerationUpdate, []tools.ToolDefinition, error) {
	var selected *config.MCPServerConfig
	for index := range servers {
		if servers[index].Name == name {
			selected = &servers[index]
			break
		}
	}
	if selected == nil {
		return nil, nil, nil, errors.New("cli: unknown MCP server")
	}
	child := mcpServerSource{kind: source.kind, id: source.id + "/server/" + name, path: source.path, projectRoot: source.projectRoot, manager: source.manager, trustLatched: true}
	var binding tools.ProjectTrustBinding
	if source.projectOwned() {
		verified, ok := source.latchedProjectBinding()
		if !ok {
			return nil, nil, nil, errMCPSourceUnverified
		}
		child.trustBinding = verified
		binding = verified
	}
	candidate, candidates, err := buildMCPGeneration(ctx, child.id, p.owner.nextMCPGeneration(child.id), []config.MCPServerConfig{*selected}, p.owner, binding)
	if err != nil {
		return nil, nil, nil, err
	}
	definitions, err := p.projectDefinitions(ctx, child.id, candidates)
	if err == nil {
		if p.validateCandidate == nil {
			err = errors.New("cli: MCP candidate validator is unavailable")
		} else {
			err = p.validateCandidate(ctx, definitions)
		}
	}
	if err != nil {
		return nil, nil, nil, errors.Join(err, p.owner.cleanupMCPCandidate(candidate))
	}
	begin := p.registry.BeginMCPGenerationAddition
	if refresh {
		begin = p.registry.BeginMCPGenerationUpdate
	}
	update, err := begin(ctx, child.id, candidate.generation, candidates)
	if err != nil {
		return nil, nil, nil, errors.Join(err, p.owner.cleanupMCPCandidate(candidate))
	}
	return candidate, update, definitions, nil
}

func (p *progressiveMCP) projectDefinitions(ctx context.Context, sourceID string, candidates []tools.MCPToolCandidate) ([]tools.ToolDefinition, error) {
	oldNames := map[string]bool{}
	for _, name := range p.owner.mcpToolNames(sourceID) {
		oldNames[name] = true
	}
	current, err := p.registry.List(ctx)
	if err != nil {
		return nil, err
	}
	definitions := make([]tools.ToolDefinition, 0, len(current)+len(candidates))
	names := map[string]bool{}
	for _, definition := range current {
		if !oldNames[definition.Name()] {
			definitions = append(definitions, definition)
			names[definition.Name()] = true
		}
	}
	for _, candidate := range candidates {
		definition := candidate.PreviewDefinition()
		if names[definition.Name()] {
			return nil, fmt.Errorf("%w: %s", tools.ErrMCPNameReserved, definition.Name())
		}
		names[definition.Name()] = true
		definitions = append(definitions, definition)
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name() < definitions[j].Name() })
	return definitions, nil
}

type mcpDiscoveryTool struct{ owner *progressiveMCP }

func (t *mcpDiscoveryTool) Name() string { return "mcp_discover" }
func (t *mcpDiscoveryTool) Description() string {
	return "List configured MCP server names without connecting. Set server to connect only that server and activate its tool schemas for the next model request. Calls to discovered tools still require normal approval."
}
func (t *mcpDiscoveryTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{"server": map[string]any{"type": "string"}, "query": map[string]any{"type": "string"}}, "additionalProperties": false}
}
func (t *mcpDiscoveryTool) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	name, _ := call.Args["server"].(string)
	query, _ := call.Args["query"].(string)
	if name != "" {
		if err := t.owner.Activate(ctx, name); err != nil {
			return nil, err
		}
	}
	data, err := json.Marshal(t.owner.List(query))
	if err != nil {
		return nil, err
	}
	return &tools.ToolResult{Output: string(data)}, nil
}

func (p *progressiveMCP) refreshServer(ctx context.Context, name string, fresh *config.Config, runtime *dynamicRuntime) error {
	source, err := selectMCPServerSource(ctx, fresh, nil)
	if err != nil {
		return err
	}
	if source == nil {
		return errors.New("cli: MCP source unavailable")
	}
	servers, _, err := source.loadCurrent(ctx)
	if err != nil {
		return err
	}
	if err := validateMCPServerSet(servers); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.validateSourceLocked(source); err != nil {
		return err
	}
	if err := p.validateMetadataLocked(servers, name); err != nil {
		return err
	}
	if !p.loaded[name] && len(p.loaded) >= p.limit {
		return errors.New("cli: MCP active server budget exhausted")
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	candidate, update, definitions, err := p.prepareActivationLocked(callCtx, name, source, servers, true)
	if err != nil {
		return err
	}
	defer update.Abort()
	snapshot, plan, err := runtime.prepare(callCtx, runtime.state.skillCatalog, runtime.state.skillBinding, runtimePreparation{definitions: definitions, servers: servers, overrideMCP: true})
	if err == nil {
		err = runtime.manager.ReloadWithCommit(callCtx, func(context.Context, *capability.Scope) (capability.Snapshot, *runtimePlan, error) {
			return snapshot, plan, nil
		}, func() error {
			return p.owner.publishMaterializedMCPGeneration(candidate, update)
		})
	}
	if err != nil {
		update.Abort()
		return errors.Join(err, p.owner.cleanupMCPCandidate(candidate))
	}
	p.source = source
	p.servers = cloneMCPServerConfigs(servers)
	p.finishActivationLocked(name, candidate.sourceID)
	return nil
}

func (p *progressiveMCP) validateSourceLocked(next *mcpServerSource) error {
	if len(p.loaded) > 0 && p.source.id != next.id {
		return capability.ErrRestartRequired
	}
	return nil
}

func (p *progressiveMCP) validateMetadataLocked(servers []config.MCPServerConfig, except string) error {
	byName := map[string]config.MCPServerConfig{}
	for _, server := range servers {
		byName[server.Name] = server
	}
	for _, server := range p.servers {
		if p.loaded[server.Name] && server.Name != except {
			next, ok := byName[server.Name]
			if !ok || !reflect.DeepEqual(server, next) {
				return fmt.Errorf("cli: MCP server %q changed: refresh that server individually or restart", server.Name)
			}
		}
	}
	return nil
}

func (p *progressiveMCP) watchListChangesLocked(name, sourceID string) {
	if cancel := p.watchers[name]; cancel != nil {
		cancel()
	}
	p.owner.mu.Lock()
	state := p.owner.sources[sourceID]
	var clients []mcp.MCPClient
	if state != nil && state.current != nil {
		clients = append(clients, state.current.clients...)
	}
	p.owner.mu.Unlock()
	for _, client := range clients {
		notifier, ok := client.(interface{ Notifications() <-chan json.RawMessage })
		if !ok {
			continue
		}
		notifications := notifier.Notifications()
		if notifications == nil {
			continue
		}
		stop := make(chan struct{})
		var stopOnce sync.Once
		p.watchers[name] = func() { stopOnce.Do(func() { close(stop) }) }
		go func() {
			for {
				select {
				case <-p.ctx.Done():
					return
				case <-stop:
					return
				case data, ok := <-notifications:
					if !ok {
						return
					}
					var notification struct {
						Method string `json:"method"`
					}
					if json.Unmarshal(data, &notification) == nil && notification.Method == "notifications/tools/list_changed" {
						p.mu.Lock()
						p.stale[name] = true
						p.mu.Unlock()
					}
				}
			}
		}()
	}
}

func (p *progressiveMCP) stopWatchers(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, stop := range p.watchers {
		stop()
	}
	return nil
}
