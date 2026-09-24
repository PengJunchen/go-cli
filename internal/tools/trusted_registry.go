package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
)

const (
	// RegistrationSourceToolRegistry identifies ordinary registrations, which
	// receive no trusted effects by default.
	RegistrationSourceToolRegistry = "tool_registry"
	// RegistrationSourceTrustedControl identifies the narrow, explicit
	// first-party control registration path.
	RegistrationSourceTrustedControl = "builtin_control"
	// RegistrationSourceBuiltin identifies reviewed first-party mutation tools.
	RegistrationSourceBuiltin = "builtin_tool"
	// RegistrationSourceTrustedNetworkRead identifies concrete first-party
	// network readers registered through the narrow reviewed path.
	RegistrationSourceTrustedNetworkRead = "builtin_network_read"
)

var (
	// ErrTrustedControlRegistrationUnavailable indicates that the registry
	// does not expose the first-party control registration capability.
	ErrTrustedControlRegistrationUnavailable = errors.New("tools: trusted control registration is unavailable")
	// ErrPreparedRegistrationUnavailable indicates that no registry snapshot
	// provider can pin the requested definition and its generation.
	ErrPreparedRegistrationUnavailable = errors.New("tools: prepared registry snapshot is unavailable")
	// ErrTrustedNetworkReadRegistrationUnavailable indicates that the registry
	// does not expose the narrow first-party network-read capability.
	ErrTrustedNetworkReadRegistrationUnavailable = errors.New("tools: trusted network-read registration is unavailable")
	// ErrUnreviewedNetworkReadDefinition indicates that the supplied definition
	// is not one of the concrete reviewed first-party network readers.
	ErrUnreviewedNetworkReadDefinition = errors.New("tools: network-read registration requires a reviewed concrete tool")
)

// TrustedControlRegistrar is the narrow registration capability used by
// first-party assembly for reviewed control-plane tools. Ordinary Register
// calls, including MCP, extension, and user-defined tools, remain unknown.
type TrustedControlRegistrar interface {
	RegisterTrustedControl(context.Context, ToolDefinition) error
}

// TrustedNetworkReadRegistrar exposes only the reviewed first-party network
// readers. It does not accept a caller-selected effect or scope.
type TrustedNetworkReadRegistrar interface {
	RegisterTrustedNetworkRead(context.Context, ToolDefinition) error
}

// DispatchFenceProvider exposes the shared fence used by a registry and its
// decorators. Each decorator must forward the same pointer, not create a new
// fence.
type DispatchFenceProvider interface {
	DispatchFence() *DispatchFence
}

// DispatchFenceFromRegistry returns the shared fence exposed by reg.
func DispatchFenceFromRegistry(reg ToolRegistry) (*DispatchFence, error) {
	provider, ok := reg.(DispatchFenceProvider)
	if !ok || provider.DispatchFence() == nil {
		return nil, ErrDispatchFenceUnavailable
	}
	return provider.DispatchFence(), nil
}

type toolRegistrationMetadata struct {
	identity            TrustedToolIdentity
	effects             TrustedEffectSet
	schema              ToolSchemaSnapshot
	hasParameters       bool
	description         string
	hasPromptGuidelines bool
	promptGuidelines    []string
	hasStreaming        bool
	hasTopTools         bool
}

type registeredToolSnapshot struct {
	definition          ToolDefinition
	identity            TrustedToolIdentity
	effects             TrustedEffectSet
	schema              ToolSchemaSnapshot
	hasParameters       bool
	description         string
	hasPromptGuidelines bool
	promptGuidelines    []string
	hasStreaming        bool
	hasTopTools         bool
	generations         DispatchFenceSnapshot
	pipeline            preparedToolPipelineSnapshot
}

type registrationSnapshotProvider interface {
	toolRegistrationSnapshot(context.Context, string) (registeredToolSnapshot, error)
}

// PreparedToolStages separates argument normalization from execution
// decoration and mutation scheduling. Effects and identity are intentionally
// absent: only registration provenance can mint those values. ExecutionWrappers
// are trusted, audited assembly decorators: they must not add side effects,
// escape asynchronously, or mutate the frozen call. Extension hooks and
// arbitrary side-effect wrappers must remain outside this stage.
type PreparedToolStages struct {
	ArgumentPreparers []ArgumentPreparer
	ExecutionWrappers []ToolExecutorWrapper
	MutationQueue     FileMutationQueue
}

type preparedToolPipelineSnapshot struct {
	policy           PreparedToolPolicy
	stages           PreparedToolStages
	policyConfigured bool
	stagesConfigured bool
}

func (p preparedToolPipelineSnapshot) pipelineConfigured() bool {
	return p.policyConfigured && p.stagesConfigured && p.policy != nil
}

type preparedToolPipelineState struct {
	mu               sync.RWMutex
	policy           PreparedToolPolicy
	stages           PreparedToolStages
	policyConfigured bool
	stagesConfigured bool
}

func (p *preparedToolPipelineState) snapshot() preparedToolPipelineSnapshot {
	if p == nil {
		return preparedToolPipelineSnapshot{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return preparedToolPipelineSnapshot{
		policy: p.policy,
		stages: PreparedToolStages{
			ArgumentPreparers: append([]ArgumentPreparer(nil), p.stages.ArgumentPreparers...),
			ExecutionWrappers: append([]ToolExecutorWrapper(nil), p.stages.ExecutionWrappers...),
			MutationQueue:     p.stages.MutationQueue,
		},
		policyConfigured: p.policyConfigured,
		stagesConfigured: p.stagesConfigured,
	}
}

func (p *preparedToolPipelineState) setPolicy(policy PreparedToolPolicy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policy = policy
	p.policyConfigured = true
}

func (p *preparedToolPipelineState) setStages(stages PreparedToolStages) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stages = PreparedToolStages{
		ArgumentPreparers: append([]ArgumentPreparer(nil), stages.ArgumentPreparers...),
		ExecutionWrappers: append([]ToolExecutorWrapper(nil), stages.ExecutionWrappers...),
		MutationQueue:     stages.MutationQueue,
	}
	p.stagesConfigured = true
}

type preparedToolPipelineProvider interface {
	preparedToolPipeline() (*preparedToolPipelineState, *DispatchFence)
}

type trustedWorkspaceWriteRegistrar interface {
	registerTrustedWorkspaceWrite(context.Context, ToolDefinition) error
}

func (r *DefaultToolRegistry) DispatchFence() *DispatchFence {
	if r == nil {
		return nil
	}
	return r.fence
}

func (r *DefaultToolRegistry) preparedToolPipeline() (*preparedToolPipelineState, *DispatchFence) {
	if r == nil {
		return nil, nil
	}
	return r.prepared, r.fence
}

func (r *DefaultToolRegistry) RegisterTrustedControl(ctx context.Context, def ToolDefinition) error {
	return r.registerDefinition(def, RegistrationSourceTrustedControl, newTrustedEffectSet(EffectControl))
}

func (r *DefaultToolRegistry) RegisterTrustedNetworkRead(ctx context.Context, def ToolDefinition) error {
	switch def.(type) {
	case *WebSearchTool, *WebFetchTool:
		return r.registerDefinition(def, RegistrationSourceTrustedNetworkRead, newTrustedEffectSet(EffectNetworkRead))
	default:
		return ErrUnreviewedNetworkReadDefinition
	}
}

func (r *DefaultToolRegistry) registerTrustedWorkspaceWrite(ctx context.Context, def ToolDefinition) error {
	switch def.(type) {
	case *WriteTool, *EditFileTool:
		return r.registerDefinition(def, RegistrationSourceBuiltin, newTrustedEffectSet(EffectWorkspaceWrite))
	default:
		return errors.New("tools: workspace-write registration requires a reviewed concrete tool")
	}
}

func (r *DefaultToolRegistry) registerDefinition(def ToolDefinition, source string, effects TrustedEffectSet) error {
	if def == nil {
		return errors.New("tools: cannot register a nil tool definition")
	}
	name := def.Name()
	if name == "" {
		return errors.New("tools: cannot register a tool with an empty name")
	}
	if r == nil || r.fence == nil {
		return ErrDispatchFenceUnavailable
	}

	metadata := toolRegistrationMetadata{
		description: def.Description(),
		effects:     newTrustedEffectSet(effects.Values()...),
	}
	if parameterized, ok := def.(Parameterized); ok {
		metadata.hasParameters = true
		schema, err := freezeToolSchema(parameterized.Parameters())
		if err != nil {
			return fmt.Errorf("tools: snapshot schema for %q: %w", name, err)
		}
		metadata.schema = schema
	}
	if guideliner, ok := def.(PromptGuideliner); ok {
		metadata.hasPromptGuidelines = true
		metadata.promptGuidelines = append([]string(nil), guideliner.PromptGuidelines()...)
	}
	_, metadata.hasStreaming = def.(StreamingBashTool)
	_, metadata.hasTopTools = def.(TopToolsProvider)

	var fingerprint [32]byte
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("tools: create executor fingerprint: %w", err)
	}
	fingerprint = sha256.Sum256(nonce[:])

	_, err := r.fence.Publish(DispatchFenceRegistry, nil, func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		if r.tools == nil {
			r.tools = make(map[string]ToolDefinition)
		}
		if r.metadata == nil {
			r.metadata = make(map[string]toolRegistrationMetadata)
		}
		if _, exists := r.tools[name]; !exists {
			r.order = append(r.order, name)
		}

		generation := uint64(1)
		if previous, exists := r.metadata[name]; exists {
			generation = previous.identity.registrationGeneration + 1
		}
		metadata.identity = newTrustedToolIdentity(source, name, generation, fingerprint, TrustedToolScope{})
		r.tools[name] = def
		r.metadata[name] = metadata
		r.version++
	})
	return err
}

func (r *DefaultToolRegistry) toolRegistrationSnapshot(_ context.Context, name string) (registeredToolSnapshot, error) {
	if r == nil || r.fence == nil {
		return registeredToolSnapshot{}, ErrDispatchFenceUnavailable
	}
	var snapshot registeredToolSnapshot
	found := false
	err := r.fence.read(func(generations DispatchFenceSnapshot) {
		r.mu.RLock()
		defer r.mu.RUnlock()

		definition, ok := r.tools[name]
		if !ok {
			return
		}
		metadata, ok := r.metadata[name]
		if !ok {
			return
		}
		found = true
		snapshot = registeredToolSnapshot{
			definition:          definition,
			identity:            metadata.identity,
			effects:             newTrustedEffectSet(metadata.effects.Values()...),
			schema:              metadata.schema,
			hasParameters:       metadata.hasParameters,
			description:         metadata.description,
			hasPromptGuidelines: metadata.hasPromptGuidelines,
			promptGuidelines:    append([]string(nil), metadata.promptGuidelines...),
			hasStreaming:        metadata.hasStreaming,
			hasTopTools:         metadata.hasTopTools,
			generations:         generations,
			pipeline:            r.prepared.snapshot(),
		}
	})
	if err != nil {
		return registeredToolSnapshot{}, err
	}
	if !found {
		return registeredToolSnapshot{}, ErrToolNotFound
	}
	return snapshot, nil
}
