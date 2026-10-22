package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/capability"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type runtimePlan struct {
	catalog        *skill.Catalog
	binding        tools.ProjectTrustBinding
	bindings       []tools.ProjectTrustBinding
	registry       tools.ToolRegistry
	hooks          *core.HookChain
	skills         []core.SkillInfo
	promptOptions  core.SystemPromptOptions
	interceptor    core.ContextToolInterceptor
	promptBudget   int
	promptCompiler core.SystemPromptBuilder
	resources      config.RuntimeConfig
}
type runtimePlanKey struct{}

// dynamicRuntime coordinates explicit refresh at idle run boundaries. Server
// activation inside a generation is additive disclosure, tracked by registry
// Version; it never changes that generation's captured source configuration.
type dynamicRuntime struct {
	manager         *capability.Manager[*runtimePlan]
	gate            chan struct{}
	state           *assembleState
	secretNamespace string
	secretVersions  map[string]runtimeSecretVersion
}

type runtimeRunLease struct {
	once  sync.Once
	inner *capability.Lease[*runtimePlan]
	gate  chan struct{}
}

func (l *runtimeRunLease) Release() { l.once.Do(func() { l.inner.Release(); l.gate <- struct{}{} }) }

func (d *dynamicRuntime) AcquireRun(ctx context.Context) (context.Context, core.RunGenerationLease, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-d.gate:
	}
	lease, err := d.manager.Acquire(ctx)
	if err != nil {
		d.gate <- struct{}{}
		return nil, nil, err
	}
	plan := lease.Value()
	if err := checkCurrentProjectBindings(ctx, plan.bindings); err != nil {
		lease.Release()
		d.gate <- struct{}{}
		return nil, nil, err
	}
	snapshot := lease.Snapshot()
	ctx = context.WithValue(ctx, runtimePlanKey{}, plan)
	ctx = core.WithRunGenerationMetadata(ctx, core.RunGenerationMetadata{GenerationID: snapshot.ID(), PromptDigest: snapshot.PromptDigest(), CapabilityDigest: snapshot.CapabilityDigest(), PolicyEpoch: snapshot.PolicyEpoch(), TrustEpoch: snapshot.TrustEpoch()})
	ctx = core.WithRunHookChain(ctx, plan.hooks)
	ctx = core.WithRunContextToolInterceptor(ctx, plan.interceptor)
	ctx = core.WithRunToolRegistry(ctx, plan.registry)
	guard := &runtimeCapabilityGuard{lease: lease, bindings: plan.bindings, plan: plan}
	if fence, err := tools.DispatchFenceFromRegistry(plan.registry); err == nil {
		guard.policyFence = fence
		guard.policy = fence.Snapshot()
	}
	ctx = core.WithRunCapabilityGuard(ctx, guard)
	return ctx, &runtimeRunLease{inner: lease, gate: d.gate}, nil
}

func (s *assembleState) assembleDynamicRuntime() error {
	if !dynamicRuntimeEnabled(s.rc) {
		return nil
	}
	namespace := make([]byte, 16)
	if _, err := rand.Read(namespace); err != nil {
		return fmt.Errorf("cli: secret revision namespace: %w", err)
	}
	d := &dynamicRuntime{secretNamespace: hex.EncodeToString(namespace), secretVersions: map[string]runtimeSecretVersion{}, manager: capability.NewManager[*runtimePlan](), gate: make(chan struct{}, 1), state: s}
	d.gate <- struct{}{}
	if err := d.publish(s.ctx, s.skillCatalog, s.skillBinding); err != nil {
		return err
	}
	s.dynamicRuntime = d
	if s.progressiveMCP != nil {
		s.progressiveMCP.validateCandidate = d.validateMCPDefinitions
	}
	s.cleanupList = append(s.cleanupList, d.manager.Close)
	return nil
}

func dynamicRuntimeEnabled(cfg *config.Config) bool {
	return cfg != nil && (cfg.Runtime.Dynamic || cfg.Runtime.Profile == "edge" || cfg.Skill.Progressive || cfg.MCP.Loading == "lazy")
}

type runtimePreparation struct {
	definitions []tools.ToolDefinition
	servers     []config.MCPServerConfig
	overrideMCP bool
}

func (d *dynamicRuntime) publish(ctx context.Context, catalog *skill.Catalog, binding tools.ProjectTrustBinding) error {
	snapshot, plan, err := d.prepare(ctx, catalog, binding, runtimePreparation{})
	if err != nil {
		return err
	}
	return d.manager.Reload(ctx, func(context.Context, *capability.Scope) (capability.Snapshot, *runtimePlan, error) {
		return snapshot, plan, nil
	})
}

// prepare performs all data-dependent validation without publishing a registry,
// resource owner, or runtime generation. The caller holds the idle cutover gate.
func (d *dynamicRuntime) prepare(ctx context.Context, catalog *skill.Catalog, binding tools.ProjectTrustBinding, input runtimePreparation) (capability.Snapshot, *runtimePlan, error) {
	infos := []core.SkillInfo(nil)
	var sources []capability.Source
	var nodes []capability.Node
	extensionNodes, extensionSources, err := extensionPlanInputs(d.state.extensionManager)
	if err != nil {
		return capability.Snapshot{}, nil, err
	}
	nodes = append(nodes, extensionNodes...)
	sources = append(sources, extensionSources...)
	if catalog != nil {
		for index, entry := range catalog.Entries() {
			if index < 100 {
				infos = append(infos, core.SkillInfo{Name: entry.Name, Description: entry.Description, Category: entry.Category})
			}
			sources = append(sources, capability.Source{ID: "skill/" + entry.Name, Kind: "skill-metadata", Digest: entry.Digest})
			nodes = append(nodes, capability.Node{ID: "skill/" + entry.Name, Owner: "skills"})
		}
	}
	if input.overrideMCP {
		for _, server := range input.servers {
			nodes = append(nodes, capability.Node{ID: "mcp/" + server.Name, Owner: "mcp"})
		}
	} else if d.state.progressiveMCP != nil {
		for _, server := range d.state.progressiveMCP.List("") {
			nodes = append(nodes, capability.Node{ID: "mcp/" + server.Name, Owner: "mcp"})
		}
	}
	trustEpoch := uint64(0)
	if binding != nil {
		trustEpoch = binding.TrustGeneration()
	}
	if verified := d.state.rc.VerifiedProjectSources(); verified != nil {
		trustEpoch = verified.TrustGeneration()
		sources = append(sources, capability.Source{ID: "project-manifest", Kind: "verified-capture", Digest: verified.ManifestDigest()})
	}
	hookBytes, err := json.Marshal(d.state.rc.Hooks)
	if err != nil {
		return capability.Snapshot{}, nil, err
	}
	sources = append(sources, capability.Source{ID: "config-hooks", Kind: "hook-plan", Content: hookBytes})
	options := d.state.runtimePromptOptions
	options.Skills = infos
	definitions := input.definitions
	if definitions == nil {
		definitions, err = d.state.tr.List(ctx)
		if err != nil {
			return capability.Snapshot{}, nil, err
		}
	}
	options.Tools = definitions
	compiler := d.state.promptBuilder
	if bounded, ok := compiler.(*runtimePromptBuilder); ok {
		compiler = bounded.inner
	}
	prompt, err := validateRuntimeRequestBudget(ctx, compiler, options, d.state.rc.Runtime.MaxPromptBytes)
	if err != nil {
		return capability.Snapshot{}, nil, err
	}
	if d.state.extensionManager != nil {
		for _, item := range d.state.extensionManager.Prompts() {
			sources = append(sources, capability.Source{ID: "extension-prompt/" + item.Owner + "/" + item.Name, Kind: "prompt", Content: []byte(item.Text)})
		}
	}
	if p := d.state.progressiveMCP; p != nil {
		servers := input.servers
		if !input.overrideMCP {
			p.mu.Lock()
			servers = cloneMCPServerConfigs(p.servers)
			p.mu.Unlock()
		}
		for _, server := range servers {
			copy := server
			copy.Env = nil
			data, err := json.Marshal(copy)
			if err != nil {
				return capability.Snapshot{}, nil, err
			}
			sources = append(sources, capability.Source{ID: "mcp-config/" + server.Name, Kind: "server", Content: data})
			if len(server.Env) > 0 || d.secretVersions[server.Name].version > 0 {
				envBytes, err := json.Marshal(server.Env)
				if err != nil {
					return capability.Snapshot{}, nil, err
				}
				fingerprint := sha256.Sum256(envBytes)
				record := d.secretVersions[server.Name]
				if record.version == 0 || record.fingerprint != fingerprint {
					record.version++
					record.fingerprint = fingerprint
					d.secretVersions[server.Name] = record
				}
				sources = append(sources, capability.Source{ID: "mcp-env/" + server.Name, Kind: "process-secret-version", Content: []byte(fmt.Sprintf("%s:%d", d.secretNamespace, record.version))})
			}

		}
	}
	snapshot, err := capability.NewSnapshot(capability.Input{Sources: sources, Nodes: nodes, Prompt: prompt, TrustEpoch: trustEpoch})
	if err != nil {
		return capability.Snapshot{}, nil, err
	}
	var interceptor core.ContextToolInterceptor
	if d.state.hookMgr != nil {
		interceptor = planModePreToolShellHookInterceptor(d.state.planCtrl, d.state.shellHooks, d.state.hookMgr.PreToolUseContextInterceptor())
	}
	bindings := []tools.ProjectTrustBinding{}
	if binding != nil {
		bindings = append(bindings, binding)
	}
	if verified := d.state.rc.VerifiedProjectSources(); verified != nil {
		bindings = append(bindings, verified)
	}
	plan := &runtimePlan{bindings: bindings, catalog: catalog, binding: binding, registry: d.state.tr, hooks: d.state.hookChain, skills: infos, promptOptions: options, interceptor: interceptor, promptBudget: d.state.rc.Runtime.MaxPromptBytes, promptCompiler: compiler, resources: d.state.rc.Runtime}
	return snapshot, plan, nil
}

func (d *dynamicRuntime) Reload(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.gate:
	}
	defer func() { d.gate <- struct{}{} }()
	fresh := d.state.rc
	if loader, ok := d.state.ctx.Value(runtimeConfigLoaderKey{}).(func(context.Context) (*config.Config, error)); ok {
		var err error
		fresh, err = loader(ctx)
		if err != nil {
			return fmt.Errorf("cli: reload config: %w", err)
		}
		if fresh == nil {
			return errors.New("cli: reload returned no configuration")
		}
		fresh = effectiveRuntimeConfig(fresh)
	}
	if old := d.state.rc.CapturedProjectSourcesForGrant(); old != nil && d.state.rc.ProjectSourceTrustStatus == config.ProjectSourceTrustVerified {
		next := fresh.VerifiedProjectSources()
		if next == nil || !next.Valid() || next.CanonicalRoot() != old.CanonicalRoot() {
			return errors.New("cli: needs-trust: changed project sources require a new explicit grant")
		}
		if binding := d.state.extensionBinding; binding != nil && binding.TrustGeneration() != next.TrustGeneration() {
			return capability.ErrRestartRequired
		}
		if p := d.state.progressiveMCP; p != nil && p.source.projectOwned() {
			p.mu.Lock()
			hasActive := len(p.loaded) > 0
			p.mu.Unlock()
			if hasActive {
				binding, valid := p.source.latchedProjectBinding()
				if !valid || binding.TrustGeneration() != next.TrustGeneration() {
					return capability.ErrRestartRequired
				}
			}
		}
		for _, path := range d.state.rc.Extensions.PluginPaths {
			before, bok := d.state.rc.ProjectExtensionBytesForVerifiedPath(path)
			after, aok := fresh.ProjectExtensionBytesForVerifiedPath(path)
			if bok != aok || !bytes.Equal(before, after) {
				return capability.ErrRestartRequired
			}
		}
	}
	for path, before := range d.state.extensionFingerprints {
		after, err := extensionFingerprint(fresh, path)
		if err != nil {
			return err
		}
		if before != after {
			return capability.ErrRestartRequired
		}
	}
	if coldConfigurationChanged(d.state.rc, fresh) {
		return capability.ErrRestartRequired
	}
	if d.state.progressiveMCP == nil {
		if err := d.rejectEagerMCPRefresh(ctx, fresh); err != nil {
			return err
		}
	}
	catalog, binding, err := discoverProgressiveSkills(ctx, fresh)
	if err != nil {
		return err
	}
	temp := &assembleState{ctx: ctx, rc: fresh, gitCwd: d.state.gitCwd, logger: d.state.logger, planCtrl: d.state.planCtrl}
	if err := validateRuntimeHooks(fresh); err != nil {
		return err
	}
	temp.loadConfiguredShellHooks()
	if len(temp.unsupportedPostToolUseHooks) > 0 {
		return ErrPreparedPostToolUseHookUnsupported
	}
	hooks := []core.Hook{}
	if d.state.extensionManager != nil {
		for _, registration := range d.state.extensionManager.HookRegistrations() {
			adapter := newExtensionHookAdapter(registration)
			adapter.binding = d.state.extensionBinding
			hooks = append(hooks, adapter)
		}
	}
	if len(temp.shellHooks) > 0 {
		temp.hookMgr = core.NewHookManager(temp.shellHooks...)
		hooks = append(hooks, temp.hookMgr)
	}
	nextHooks := core.NewHookChain(hooks...)
	options := d.state.runtimePromptOptions
	options.CustomPrompt = fresh.Agent.SystemPrompt
	options.AppendPrompt = fresh.Agent.AppendSystemPrompt
	if d.state.extensionManager != nil {
		for _, prompt := range d.state.extensionManager.Prompts() {
			options.AppendPrompt += "\n" + prompt.Text
		}
	}
	var nextSource *mcpServerSource
	var nextServers []config.MCPServerConfig
	if p := d.state.progressiveMCP; p != nil {
		nextSource, err = selectMCPServerSource(ctx, fresh, nil)
		if err != nil {
			return err
		}
		if nextSource == nil {
			return errors.New("cli: MCP source unavailable during reload")
		}
		nextServers, _, err = nextSource.loadCurrent(ctx)
		if err != nil {
			return err
		}
		if err := validateMCPServerSet(nextServers); err != nil {
			return err
		}
		p.mu.Lock()
		validationErr := p.validateSourceLocked(nextSource)
		if validationErr == nil {
			validationErr = p.validateMetadataLocked(nextServers, "")
		}
		p.mu.Unlock()
		if validationErr != nil {
			return validationErr
		}
	}
	oldRC, oldOptions, oldHooks, oldManager, oldShell := d.state.rc, d.state.runtimePromptOptions, d.state.hookChain, d.state.hookMgr, d.state.shellHooks
	d.state.rc = fresh
	d.state.runtimePromptOptions = options
	d.state.hookChain = nextHooks
	d.state.hookMgr = temp.hookMgr
	d.state.shellHooks = temp.shellHooks
	var oldSource *mcpServerSource
	var oldServers []config.MCPServerConfig
	if p := d.state.progressiveMCP; p != nil {
		p.mu.Lock()
		oldSource, oldServers = p.source, p.servers
		p.source = nextSource
		p.servers = cloneMCPServerConfigs(nextServers)
		p.mu.Unlock()
	}
	if err := d.publish(ctx, catalog, binding); err != nil {
		d.state.rc = oldRC
		d.state.runtimePromptOptions = oldOptions
		d.state.hookChain = oldHooks
		d.state.hookMgr = oldManager
		d.state.shellHooks = oldShell
		if p := d.state.progressiveMCP; p != nil {
			p.mu.Lock()
			p.source = oldSource
			p.servers = oldServers
			p.mu.Unlock()
		}
		return err
	}
	d.state.skillCatalog = catalog
	d.state.skillBinding = binding
	return nil
}

// CapabilityLimits reports effective host budgets; zero prompt bytes means
// the model's request envelope remains the active bound.
type CapabilityLimits struct {
	MaxSkills      int `json:"max_skills"`
	MaxPromptBytes int `json:"max_prompt_bytes"`
	MaxMCPServers  int `json:"max_mcp_servers"`
}

// CapabilityStatus is a safe, bounded view for terminal clients.
type CapabilityStatus struct {
	Profile         string             `json:"profile"`
	Limits          CapabilityLimits   `json:"limits"`
	SourceKinds     []string           `json:"source_kinds,omitempty"`
	Effective       string             `json:"effective"`
	Generation      string             `json:"generation"`
	CatalogRevision int                `json:"catalog_revision"`
	Skills          []skill.Descriptor `json:"skills,omitempty"`
	MCP             []MCPServerStatus  `json:"mcp,omitempty"`
}

// CapabilityStatus returns the current published identity and disclosure state.
func (a *AgentAssembly) CapabilityStatus(ctx context.Context) (CapabilityStatus, error) {
	if a.dynamicRuntime == nil {
		return CapabilityStatus{}, errors.New("cli: dynamic capability runtime is not enabled")
	}
	lease, err := a.dynamicRuntime.manager.Acquire(ctx)
	if err != nil {
		return CapabilityStatus{}, err
	}
	defer lease.Release()
	status := CapabilityStatus{Generation: lease.Snapshot().ID(), Skills: lease.Value().catalog.Search("", 100)}
	resources := lease.Value().resources
	status.Profile = resources.Profile
	if status.Profile == "" {
		status.Profile = "default"
	}
	status.Limits = CapabilityLimits{MaxSkills: resources.MaxSkills, MaxPromptBytes: resources.MaxPromptBytes, MaxMCPServers: resources.MaxMCPServers}
	if status.Limits.MaxSkills == 0 {
		status.Limits.MaxSkills = 1024
	}
	if status.Limits.MaxMCPServers == 0 {
		status.Limits.MaxMCPServers = 16
	}
	status.Effective = "Configuration refresh applies to the next run; newly activated MCP tools appear in the next model request."
	kinds := map[string]bool{}
	for _, source := range lease.Snapshot().Sources() {
		kinds[source.Kind] = true
	}
	for kind := range kinds {
		status.SourceKinds = append(status.SourceKinds, kind)
	}
	sort.Strings(status.SourceKinds)

	if v, ok := lease.Value().registry.(interface{ Version() int }); ok {
		status.CatalogRevision = v.Version()
	}
	if a.progressiveMCP != nil {
		status.MCP = a.progressiveMCP.List("")
	}
	return status, nil
}

// ReloadCapabilities refreshes skill metadata at an idle run boundary.
func (a *AgentAssembly) ReloadCapabilities(ctx context.Context) error {
	if a.dynamicRuntime == nil {
		return errors.New("cli: dynamic capability runtime is not enabled")
	}
	return a.dynamicRuntime.Reload(ctx)
}

// LoadMCPServer activates one configured server without a global reconnect.
func (a *AgentAssembly) LoadMCPServer(ctx context.Context, name string) error {
	if a.progressiveMCP == nil || a.dynamicRuntime == nil {
		return errors.New("cli: lazy MCP loading is not enabled")
	}
	d := a.dynamicRuntime
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.gate:
	}
	defer func() { d.gate <- struct{}{} }()
	return a.progressiveMCP.Activate(ctx, name)
}

type runtimePromptBuilder struct {
	inner    core.SystemPromptBuilder
	maxBytes int
}

func (b *runtimePromptBuilder) Build(ctx context.Context, options core.SystemPromptOptions) string {
	compiler, maxBytes := b.inner, b.maxBytes
	if plan, ok := ctx.Value(runtimePlanKey{}).(*runtimePlan); ok {
		if err := checkCurrentProjectBindings(ctx, plan.bindings); err != nil {
			return "Project capability authorization is no longer current. Refresh trusted project sources before continuing."
		}
		toolsForRequest := options.Tools
		options = plan.promptOptions
		options.Tools = toolsForRequest
		options.Skills = plan.skills
		compiler, maxBytes = plan.promptCompiler, plan.promptBudget
	}
	result := compiler.Build(ctx, options)
	if maxBytes > 0 && len(result) > maxBytes {
		// Never truncate structured instructions mid-section. A clear refusal
		// prompt keeps the model from acting on a partial capability contract.
		return fmt.Sprintf("Capability prompt exceeds the configured %d byte budget. Ask the user to reduce selected context or increase runtime.max_prompt_bytes before acting.", maxBytes)
	}
	return result
}

func formatCapabilityStatus(status CapabilityStatus) string {
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return "capability status unavailable"
	}
	return string(data)
}

type runtimeCapabilityGuard struct {
	lease         *capability.Lease[*runtimePlan]
	bindings      []tools.ProjectTrustBinding
	plan          *runtimePlan
	budgetMu      sync.Mutex
	budgetVersion int
	budgetChecked bool
	budgetError   error
	policyFence   *tools.DispatchFence
	policy        tools.DispatchFenceSnapshot
}

func (g *runtimeCapabilityGuard) BeginCall(ctx context.Context, id string) (core.RunCapabilityInvocation, error) {
	if err := g.checkPolicy(); err != nil {
		return nil, err
	}
	if err := checkCurrentProjectBindings(ctx, g.bindings); err != nil {
		return nil, err
	}
	if err := g.checkBudget(ctx); err != nil {
		return nil, err
	}
	invocation, err := g.lease.BeginCall(ctx, id)
	if err != nil {
		return nil, err
	}
	return &runtimeInvocation{Invocation: invocation, bindings: g.bindings, guard: g}, nil
}
func (g *runtimeCapabilityGuard) Check() error {
	return g.CheckContext(context.Background())
}

// CheckContext preserves the active caller's cancellation during persisted
// authority checks. Check remains compatible with the original guard port.
func (g *runtimeCapabilityGuard) CheckContext(ctx context.Context) error {
	invocation, err := g.BeginCall(ctx, "completion-fence")
	if err != nil {
		return err
	}
	defer invocation.Release()
	return invocation.Complete()
}

type runtimeInvocation struct {
	guard *runtimeCapabilityGuard
	*capability.Invocation[*runtimePlan]
	bindings []tools.ProjectTrustBinding
}

func (i *runtimeInvocation) Complete() error {
	if i.guard != nil {
		if err := i.guard.checkPolicy(); err != nil {
			return err
		}
	}
	if err := i.Invocation.Complete(); err != nil {
		return err
	}
	if err := checkCurrentProjectBindings(i.Context(), i.bindings); err != nil {
		return fmt.Errorf("cli: project capability authorization changed; external outcome may be unknown: %w", err)
	}
	return nil
}

// checkCurrentProjectBindings reads the persisted source authority; Valid only
// checks the minted proof's shape. The no-op callback must never enter a
// registry/manager lock or run user code while the trust transaction is held.
func checkCurrentProjectBindings(ctx context.Context, bindings []tools.ProjectTrustBinding) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, binding := range bindings {
		if binding == nil || !binding.Valid() {
			return errors.New("cli: project capability trust proof is invalid")
		}
		if err := binding.CommitIfCurrent(ctx, func() error { return nil }); err != nil {
			return fmt.Errorf("cli: project capability trust is not current: %w", err)
		}
	}
	return nil
}

func effectiveRuntimeConfig(input *config.Config) *config.Config {
	if input == nil || input.Runtime.Profile != "edge" {
		return input
	}
	cfg := *input
	cfg.Runtime.Dynamic = true
	cfg.Skill.Progressive = true
	cfg.MCP.Loading = "lazy"
	if cfg.Runtime.MaxSkills == 0 {
		cfg.Runtime.MaxSkills = 128
	}
	if cfg.Runtime.MaxMCPServers == 0 {
		cfg.Runtime.MaxMCPServers = 2
	}
	if cfg.Runtime.MaxPromptBytes == 0 {
		cfg.Runtime.MaxPromptBytes = 128 << 10
	}
	return &cfg
}

// RefreshMCPServer replaces one server's catalog at an idle boundary. Its
// candidate is complete before publication; failures keep that server intact.
func (a *AgentAssembly) RefreshMCPServer(ctx context.Context, name string) error {
	if a.progressiveMCP == nil {
		return errors.New("cli: lazy MCP loading is not enabled")
	}
	if a.dynamicRuntime == nil {
		return errors.New("cli: dynamic capability runtime is not enabled")
	}
	d := a.dynamicRuntime
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.gate:
	}
	defer func() { d.gate <- struct{}{} }()
	fresh := d.state.rc
	if loader, ok := d.state.ctx.Value(runtimeConfigLoaderKey{}).(func(context.Context) (*config.Config, error)); ok {
		var err error
		fresh, err = loader(ctx)
		if err != nil {
			return err
		}
		if fresh == nil {
			return errors.New("cli: reload returned no configuration")
		}
		fresh = effectiveRuntimeConfig(fresh)
	}
	if d.state.rc.ProjectSourceTrustStatus == config.ProjectSourceTrustVerified {
		oldBinding := d.state.rc.VerifiedProjectSources()
		nextBinding := fresh.VerifiedProjectSources()
		if oldBinding == nil || nextBinding == nil || oldBinding.TrustGeneration() != nextBinding.TrustGeneration() || oldBinding.CanonicalRoot() != nextBinding.CanonicalRoot() {
			return capability.ErrRestartRequired
		}
	}
	if coldConfigurationChanged(d.state.rc, fresh) {
		return capability.ErrRestartRequired
	}
	return a.progressiveMCP.refreshServer(ctx, name, fresh, d)
}

type runtimeConfigLoaderKey struct{}

// WithRuntimeConfigLoader binds the entry point's original trusted config loader
// so refresh preserves its precedence and source verification contract.
func WithRuntimeConfigLoader(ctx context.Context, loader func(context.Context) (*config.Config, error)) context.Context {
	return context.WithValue(ctx, runtimeConfigLoaderKey{}, loader)
}

func coldConfigurationChanged(before, after *config.Config) bool {
	if before.Skill.Progressive != after.Skill.Progressive || before.MCP.Loading != after.MCP.Loading || before.Runtime.Dynamic != after.Runtime.Dynamic || before.Runtime.Profile != after.Runtime.Profile || before.Runtime.MaxMCPServers != after.Runtime.MaxMCPServers {
		return true
	}
	if before.MCP.Loading != "lazy" && before.Runtime.Profile != "edge" {
		left, _ := json.Marshal(before.MCP)
		right, _ := json.Marshal(after.MCP)
		if !bytes.Equal(left, right) {
			return true
		}
	}
	a, b := *before, *after
	a.Skill = config.SkillConfig{}
	b.Skill = config.SkillConfig{}
	a.MCP = config.MCPConfig{}
	b.MCP = config.MCPConfig{}
	a.Runtime = config.RuntimeConfig{}
	b.Runtime = config.RuntimeConfig{}
	a.Hooks = config.HooksConfig{}
	b.Hooks = config.HooksConfig{}
	a.Agent.SystemPrompt = ""
	b.Agent.SystemPrompt = ""
	a.Agent.AppendSystemPrompt = ""
	b.Agent.AppendSystemPrompt = ""
	a.ProjectSourceTrustStatus = ""
	b.ProjectSourceTrustStatus = ""
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return !bytes.Equal(left, right)
}

// Eager servers do not participate in dynamic publication. Conservatively
// require restart whenever either selected source or an existing owner is
// present, including a file that was removed after the owner was installed.
func (d *dynamicRuntime) rejectEagerMCPRefresh(ctx context.Context, next *config.Config) error {
	for _, cfg := range []*config.Config{d.state.rc, next} {
		source, err := selectMCPServerSource(ctx, cfg, nil)
		if err != nil {
			return err
		}
		if source != nil && source.kind != mcpServerSourceNone {
			return capability.ErrRestartRequired
		}
	}
	if owner := d.state.mcpResources; owner != nil {
		owner.mu.Lock()
		defer owner.mu.Unlock()
		for _, source := range owner.sources {
			if source != nil && source.current != nil {
				return capability.ErrRestartRequired
			}
		}
	}
	return nil
}

func validateRuntimeHooks(cfg *config.Config) error {
	declarations := append([]config.HookConfig(nil), cfg.Hooks.Hooks...)
	if verified := cfg.VerifiedProjectSources(); verified != nil {
		if data, ok := verified.SourceBytes(config.ProjectHooksSourceID); ok {
			project, err := parseShellHooksConfig(data)
			if err != nil {
				return fmt.Errorf("cli: hook configuration: %w", err)
			}
			declarations = append(declarations, project.Hooks...)
		}
	}
	for _, hook := range declarations {
		event, err := parseHookEvent(hook.Event)
		if err != nil {
			return err
		}
		if event == core.EventPostToolUse {
			return ErrPreparedPostToolUseHookUnsupported
		}
		if hook.Timeout != "" {
			duration, err := time.ParseDuration(hook.Timeout)
			if err != nil || duration <= 0 || duration > time.Minute {
				return errors.New("cli: hook timeout must be greater than zero and at most one minute")
			}
		}
	}
	return nil
}

func (g *runtimeCapabilityGuard) checkBudget(ctx context.Context) error {
	if g.plan == nil || g.plan.promptBudget <= 0 {
		return nil
	}
	version := 0
	if registry, ok := g.plan.registry.(interface{ Version() int }); ok {
		version = registry.Version()
	}
	g.budgetMu.Lock()
	defer g.budgetMu.Unlock()
	if g.budgetChecked && g.budgetVersion == version {
		return g.budgetError
	}
	definitions, err := g.plan.registry.List(ctx)
	if err != nil {
		return err
	}
	options := g.plan.promptOptions
	options.Tools = definitions
	_, g.budgetError = validateRuntimeRequestBudget(ctx, g.plan.promptCompiler, options, g.plan.promptBudget)
	g.budgetChecked = true
	g.budgetVersion = version
	return g.budgetError
}

func validateRuntimeRequestBudget(ctx context.Context, compiler core.SystemPromptBuilder, options core.SystemPromptOptions, limit int) (string, error) {
	if compiler == nil {
		return "", errors.New("cli: capability prompt compiler is unavailable")
	}
	prompt := compiler.Build(ctx, options)
	if limit <= 0 {
		return prompt, nil
	}
	size := len(prompt)
	for _, definition := range options.Tools {
		if parameterized, ok := definition.(tools.Parameterized); ok {
			schema, err := json.Marshal(parameterized.Parameters())
			if err != nil {
				return "", err
			}
			size += len(schema)
		}
	}
	if size > limit {
		return "", errors.New("cli: capability prompt and schema byte budget exhausted")
	}
	return prompt, nil
}

func (d *dynamicRuntime) validateMCPDefinitions(ctx context.Context, definitions []tools.ToolDefinition) error {
	plan, _ := ctx.Value(runtimePlanKey{}).(*runtimePlan)
	if plan == nil {
		lease, err := d.manager.Acquire(ctx)
		if err != nil {
			return err
		}
		defer lease.Release()
		plan = lease.Value()
	}
	options := plan.promptOptions
	options.Tools = definitions
	_, err := validateRuntimeRequestBudget(ctx, plan.promptCompiler, options, plan.promptBudget)
	return err
}

func (g *runtimeCapabilityGuard) checkPolicy() error {
	if g.policyFence == nil {
		return nil
	}
	latest := g.policyFence.Snapshot()
	if latest.Approval != g.policy.Approval || latest.Plan != g.policy.Plan {
		return errors.New("cli: runtime policy changed; result rejected and external outcome may be unknown")
	}
	return nil
}

// SearchSkills searches the entire catalog while keeping output bounded.
func (a *AgentAssembly) SearchSkills(ctx context.Context, query string) ([]skill.Descriptor, error) {
	if a.dynamicRuntime == nil {
		return nil, errors.New("cli: progressive skill catalog is unavailable")
	}
	lease, err := a.dynamicRuntime.manager.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	plan := lease.Value()
	if err := checkCurrentProjectBindings(ctx, plan.bindings); err != nil {
		return nil, err
	}
	entries := plan.catalog.Search(query, 100)
	if err := checkCurrentProjectBindings(ctx, plan.bindings); err != nil {
		return nil, err
	}
	return entries, nil
}

// LoadSkillInstructions materializes one skill for an explicit terminal command.
func (a *AgentAssembly) LoadSkillInstructions(ctx context.Context, name string) (string, error) {
	if a.dynamicRuntime == nil {
		return "", errors.New("cli: progressive skill catalog is unavailable")
	}
	lease, err := a.dynamicRuntime.manager.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer lease.Release()
	plan := lease.Value()
	if err := checkCurrentProjectBindings(ctx, plan.bindings); err != nil {
		return "", err
	}
	definition, err := plan.catalog.Load(ctx, name)
	if err != nil {
		return "", err
	}
	if err := checkCurrentProjectBindings(ctx, plan.bindings); err != nil {
		return "", err
	}
	return fmt.Sprintf("Use skill %s from %s. Follow these instructions:\n\n%s", name, plan.catalog.EntryPath(name), definition.Prompt()), nil
}

func extensionFingerprint(cfg *config.Config, path string) (string, error) {
	hash := sha256.New()
	if data, ok := cfg.ProjectExtensionBytesForVerifiedPath(path); ok {
		_, _ = hash.Write(data)
	} else {
		if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
			return "remote-restart-required", nil
		}
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("cli: extension artifact fingerprint: %w", err)
		}
		defer file.Close() //nolint:errcheck
		if _, err := io.Copy(hash, file); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// runtimeSecretVersion is never exported or logged. The public generation
// contains only a process-scoped opaque namespace and monotonic revision.
type runtimeSecretVersion struct {
	fingerprint [32]byte
	version     uint64
}
