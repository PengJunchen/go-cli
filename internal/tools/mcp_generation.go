package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	// RegistrationSourceMCP identifies definitions published by a selected MCP
	// configuration source. It does not confer any trusted tool effects.
	RegistrationSourceMCP = "mcp_server"
)

var (
	// ErrMCPGenerationUnavailable reports that the registry cannot atomically
	// publish source-owned MCP generations.
	ErrMCPGenerationUnavailable = errors.New("tools: MCP generation publication is unavailable")
	// ErrMCPNameReserved reports that another registration owns an MCP name.
	ErrMCPNameReserved = errors.New("tools: MCP tool name is reserved")
	// ErrMCPGenerationStale reports that a deferred MCP entry no longer belongs
	// to the active generation for its source.
	ErrMCPGenerationStale = errors.New("tools: MCP tool generation is stale")
	// ErrMCPGenerationTransactionClosed reports an operation on a consumed update.
	ErrMCPGenerationTransactionClosed = errors.New("tools: MCP generation transaction is closed")
)

// PreparedDispatchLease pins one exact MCP client generation for a committed
// prepared invocation. Acquire and its release function must not take DTR,
// owner, or DispatchFence locks.
type PreparedDispatchLease interface {
	Acquire() (release func(), err error)
}

// MCPToolCandidate is one fully built tool in an unpublished MCP generation.
// The caller owns the definition and lease until Publish or Abort completes.
type MCPToolCandidate struct {
	Definition ToolDefinition
	Lease      PreparedDispatchLease
}

type mcpGenerationOwner struct {
	sourceID   string
	generation uint64
}

type mcpMaterializedOwner struct {
	mcpGenerationOwner
	lease PreparedDispatchLease
}

type mcpSourceState struct {
	generation uint64
	names      map[string]struct{}
}

type mcpDeferredEntry struct {
	owner       mcpGenerationOwner
	registry    *DefaultToolRegistry
	definition  ToolDefinition
	lease       PreparedDispatchLease
	metadata    toolRegistrationMetadata
	fingerprint [32]byte
}

type mcpGenerationCandidate struct {
	name        string
	definition  ToolDefinition
	lease       PreparedDispatchLease
	metadata    toolRegistrationMetadata
	fingerprint [32]byte
}

// MCPGenerationUpdate owns the DTR write lock until Publish, Disable, or Abort.
// It stages only local metadata; client construction and network work must be
// complete before BeginMCPGenerationUpdate is called.
type MCPGenerationUpdate struct {
	dtr        *DeferredToolRegistryAdapter
	registry   *DefaultToolRegistry
	sourceID   string
	generation uint64
	oldNames   map[string]struct{}
	candidates []mcpGenerationCandidate
	active     bool
}

// BeginMCPGenerationUpdate validates and freezes the complete candidate, then
// takes and retains the DTR lock until the update is consumed.
func (a *DeferredToolRegistryAdapter) BeginMCPGenerationUpdate(
	ctx context.Context,
	sourceID string,
	generation uint64,
	candidates []MCPToolCandidate,
) (*MCPGenerationUpdate, error) {
	if a == nil || a.DefaultDeferredToolRegistry == nil {
		return nil, ErrMCPGenerationUnavailable
	}
	if ctx == nil {
		return nil, errors.New("tools: MCP generation context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sourceID) == "" || generation == 0 {
		return nil, errors.New("tools: MCP generation requires a source and positive generation")
	}
	registry, ok := mcpRegistryFrom(a.underlying)
	if !ok || registry == nil {
		return nil, ErrMCPGenerationUnavailable
	}

	prepared := make([]mcpGenerationCandidate, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		entry, err := prepareMCPGenerationCandidate(candidate)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[entry.name]; duplicate {
			return nil, fmt.Errorf("tools: duplicate MCP tool name %q", entry.name)
		}
		seen[entry.name] = struct{}{}
		prepared = append(prepared, entry)
	}
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].name < prepared[j].name })

	a.mu.Lock()
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if current, exists := a.mcpSources[sourceID]; exists && generation <= current.generation {
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: source %q current generation is %d, requested %d", ErrMCPGenerationStale, sourceID, current.generation, generation)
	}
	for _, candidate := range prepared {
		if owner, reserved := a.mcpReservations[candidate.name]; reserved && owner.sourceID != sourceID {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: %q is reserved by MCP source %q", ErrMCPNameReserved, candidate.name, owner.sourceID)
		}
		if _, deferred := a.loaders[candidate.name]; deferred {
			owned, sourceEntry := a.mcpEntries[candidate.name]
			if !sourceEntry || owned.owner.sourceID != sourceID {
				a.mu.Unlock()
				return nil, fmt.Errorf("%w: %q already has a deferred registration", ErrMCPNameReserved, candidate.name)
			}
		}
	}
	oldNames := make(map[string]struct{})
	if current, exists := a.mcpSources[sourceID]; exists {
		for name := range current.names {
			oldNames[name] = struct{}{}
		}
	}
	return &MCPGenerationUpdate{
		dtr:        a,
		registry:   registry,
		sourceID:   sourceID,
		generation: generation,
		oldNames:   oldNames,
		candidates: prepared,
		active:     true,
	}, nil
}

// Publish atomically replaces this source's names, definitions, and deferred
// loaders, advancing the Registry fence once. A checked publication error
// leaves the transaction lock held so the caller may Disable or Abort.
func (u *MCPGenerationUpdate) Publish() error {
	if !u.isActive() {
		return ErrMCPGenerationTransactionClosed
	}
	err := u.registry.publishMCPGeneration(u, false)
	if err != nil {
		return err
	}
	u.finish()
	return nil
}

// Disable removes only this source's current names and definitions, advances
// the Registry fence once, and consumes the transaction.
func (u *MCPGenerationUpdate) Disable() error {
	if !u.isActive() {
		return ErrMCPGenerationTransactionClosed
	}
	if err := u.registry.publishMCPGeneration(u, true); err != nil {
		return err
	}
	u.finish()
	return nil
}

// Abort releases an unpublished generation transaction without changing state.
func (u *MCPGenerationUpdate) Abort() {
	if !u.isActive() {
		return
	}
	u.finish()
}

func (u *MCPGenerationUpdate) isActive() bool {
	return u != nil && u.active && u.dtr != nil && u.registry != nil
}

func (u *MCPGenerationUpdate) finish() {
	u.active = false
	u.dtr.mu.Unlock()
}

func prepareMCPGenerationCandidate(candidate MCPToolCandidate) (mcpGenerationCandidate, error) {
	if candidate.Definition == nil {
		return mcpGenerationCandidate{}, errors.New("tools: MCP generation contains a nil tool")
	}
	name := candidate.Definition.Name()
	if strings.TrimSpace(name) == "" {
		return mcpGenerationCandidate{}, errors.New("tools: MCP generation contains a tool with an empty name")
	}
	metadata := toolRegistrationMetadata{
		description: candidate.Definition.Description(),
		effects:     unknownTrustedEffects(),
	}
	parameterized, ok := candidate.Definition.(Parameterized)
	if !ok {
		return mcpGenerationCandidate{}, fmt.Errorf("%w: MCP tool %q has no input schema", ErrPreparedSchemaInvalid, name)
	}
	metadata.hasParameters = true
	schema, err := freezeToolSchema(parameterized.Parameters())
	if err != nil {
		return mcpGenerationCandidate{}, fmt.Errorf("tools: snapshot MCP schema for %q: %w", name, err)
	}
	if !schema.Present() {
		return mcpGenerationCandidate{}, fmt.Errorf("%w: MCP tool %q has no input schema", ErrPreparedSchemaInvalid, name)
	}
	if err := validatePreparedSchemaNode(schema.Schema(), true); err != nil {
		return mcpGenerationCandidate{}, fmt.Errorf("tools: validate MCP schema for %q: %w", name, err)
	}
	metadata.schema = schema
	if guideliner, ok := candidate.Definition.(PromptGuideliner); ok {
		metadata.hasPromptGuidelines = true
		metadata.promptGuidelines = append([]string(nil), guideliner.PromptGuidelines()...)
	}
	_, metadata.hasStreaming = candidate.Definition.(StreamingBashTool)
	_, metadata.hasTopTools = candidate.Definition.(TopToolsProvider)

	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return mcpGenerationCandidate{}, fmt.Errorf("tools: create MCP executor fingerprint: %w", err)
	}
	return mcpGenerationCandidate{
		name:        name,
		definition:  candidate.Definition,
		lease:       candidate.Lease,
		metadata:    metadata,
		fingerprint: sha256.Sum256(nonce[:]),
	}, nil
}

type mcpGenerationRegistryProvider interface {
	mcpGenerationRegistry() *DefaultToolRegistry
}

func mcpRegistryFrom(registry ToolRegistry) (*DefaultToolRegistry, bool) {
	if provider, ok := registry.(mcpGenerationRegistryProvider); ok {
		return provider.mcpGenerationRegistry(), true
	}
	return nil, false
}

func (r *DefaultToolRegistry) mcpGenerationRegistry() *DefaultToolRegistry { return r }

func (r *DefaultToolRegistry) publishMCPGeneration(update *MCPGenerationUpdate, disable bool) error {
	if r == nil || r.fence == nil {
		return ErrDispatchFenceUnavailable
	}
	_, err := r.fence.publishChecked(DispatchFenceRegistry, nil, func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ensureMCPMapsLocked()

		if !disable {
			for _, candidate := range update.candidates {
				if owner, reserved := r.mcpReservations[candidate.name]; reserved && owner.sourceID != update.sourceID {
					return fmt.Errorf("%w: %q is reserved by MCP source %q", ErrMCPNameReserved, candidate.name, owner.sourceID)
				}
				if _, registered := r.tools[candidate.name]; registered {
					owner, sourceOwned := r.mcpMaterialized[candidate.name]
					metadata, hasMetadata := r.metadata[candidate.name]
					if !sourceOwned || owner.sourceID != update.sourceID || !hasMetadata || metadata.identity.RegistrationSource() != RegistrationSourceMCP {
						return fmt.Errorf("%w: %q is owned by another registration", ErrMCPNameReserved, candidate.name)
					}
				}
			}
		}

		for name := range update.oldNames {
			if owner, exists := r.mcpMaterialized[name]; exists && owner.sourceID == update.sourceID {
				if metadata, ok := r.metadata[name]; ok && metadata.identity.RegistrationSource() == RegistrationSourceMCP {
					delete(r.tools, name)
					delete(r.metadata, name)
					delete(r.mcpMaterialized, name)
					removeRegistryName(&r.order, name)
				}
			}
			if owner, exists := r.mcpReservations[name]; exists && owner.sourceID == update.sourceID {
				delete(r.mcpReservations, name)
			}
		}

		newNames := make(map[string]struct{}, len(update.candidates))
		if !disable {
			for _, candidate := range update.candidates {
				owner := mcpGenerationOwner{sourceID: update.sourceID, generation: update.generation}
				r.mcpReservations[candidate.name] = owner
				newNames[candidate.name] = struct{}{}
			}
		}
		r.version++
		update.applyDTRState(disable, newNames)
		return nil
	})
	return err
}

func (u *MCPGenerationUpdate) applyDTRState(disable bool, newNames map[string]struct{}) {
	for name := range u.oldNames {
		if entry, exists := u.dtr.mcpEntries[name]; exists && entry.owner.sourceID == u.sourceID {
			delete(u.dtr.loaders, name)
			delete(u.dtr.loaded, name)
			delete(u.dtr.mcpEntries, name)
		}
		if owner, exists := u.dtr.mcpReservations[name]; exists && owner.sourceID == u.sourceID {
			delete(u.dtr.mcpReservations, name)
		}
	}
	if !disable {
		owner := mcpGenerationOwner{sourceID: u.sourceID, generation: u.generation}
		for _, candidate := range u.candidates {
			name := candidate.name
			entry := mcpDeferredEntry{
				owner:       owner,
				registry:    u.registry,
				definition:  candidate.definition,
				lease:       candidate.lease,
				metadata:    candidate.metadata,
				fingerprint: candidate.fingerprint,
			}
			u.dtr.mcpEntries[name] = entry
			u.dtr.mcpReservations[name] = owner
			definition := candidate.definition
			u.dtr.loaders[name] = func() (ToolDefinition, error) { return definition, nil }
		}
	}
	u.dtr.mcpSources[u.sourceID] = mcpSourceState{generation: u.generation, names: newNames}
	u.dtr.revision++
}

func (r *DefaultToolRegistry) registerMCPMaterialization(ctx context.Context, entry mcpDeferredEntry, def ToolDefinition) error {
	if ctx == nil {
		return errors.New("tools: MCP materialization context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if def == nil || def.Name() == "" {
		return errors.New("tools: cannot materialize an empty MCP definition")
	}
	if r == nil || r.fence == nil {
		return ErrDispatchFenceUnavailable
	}
	_, err := r.fence.publishChecked(DispatchFenceRegistry, nil, func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ensureMCPMapsLocked()
		if owner, reserved := r.mcpReservations[def.Name()]; !reserved || owner != entry.owner {
			return ErrMCPGenerationStale
		}
		if _, registered := r.tools[def.Name()]; registered {
			return fmt.Errorf("%w: %q is already materialized", ErrMCPNameReserved, def.Name())
		}
		metadata := entry.metadata
		metadata.identity = newTrustedToolIdentity(RegistrationSourceMCP, def.Name(), entry.owner.generation, entry.fingerprint, TrustedToolScope{})
		if r.tools == nil {
			r.tools = make(map[string]ToolDefinition)
		}
		if r.metadata == nil {
			r.metadata = make(map[string]toolRegistrationMetadata)
		}
		r.order = append(r.order, def.Name())
		r.tools[def.Name()] = def
		r.metadata[def.Name()] = metadata
		r.mcpMaterialized[def.Name()] = mcpMaterializedOwner{mcpGenerationOwner: entry.owner, lease: entry.lease}
		r.version++
		return nil
	})
	return err
}

func (r *DefaultToolRegistry) ensureMCPMapsLocked() {
	if r.tools == nil {
		r.tools = make(map[string]ToolDefinition)
	}
	if r.metadata == nil {
		r.metadata = make(map[string]toolRegistrationMetadata)
	}
	if r.mcpReservations == nil {
		r.mcpReservations = make(map[string]mcpGenerationOwner)
	}
	if r.mcpMaterialized == nil {
		r.mcpMaterialized = make(map[string]mcpMaterializedOwner)
	}
}

func removeRegistryName(order *[]string, name string) {
	for i, current := range *order {
		if current == name {
			*order = append((*order)[:i], (*order)[i+1:]...)
			return
		}
	}
}

func (r *DefaultToolRegistry) registerDefaultToolGuarded(ctx context.Context, def ToolDefinition) error {
	metadata, fingerprint, err := prepareRegistrationMetadata(def, unknownTrustedEffects(), "")
	if err != nil {
		return err
	}
	name := def.Name()
	if r == nil || r.fence == nil {
		return ErrDispatchFenceUnavailable
	}
	_, err = r.fence.publishChecked(DispatchFenceRegistry, nil, func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ensureMCPMapsLocked()
		if owner, reserved := r.mcpReservations[name]; reserved {
			return fmt.Errorf("%w: %q is reserved by MCP source %q generation %d", ErrMCPNameReserved, name, owner.sourceID, owner.generation)
		}
		generation := uint64(1)
		if previous, exists := r.metadata[name]; exists {
			generation = previous.identity.RegistrationGeneration() + 1
		}
		metadata.identity = newTrustedToolIdentity(RegistrationSourceToolRegistry, name, generation, fingerprint, TrustedToolScope{})
		if _, exists := r.tools[name]; !exists {
			r.order = append(r.order, name)
		}
		r.tools[name] = def
		r.metadata[name] = metadata
		delete(r.mcpMaterialized, name)
		r.version++
		return nil
	})
	return err
}

func prepareRegistrationMetadata(def ToolDefinition, effects TrustedEffectSet, completionProof PreparedCompletionProof) (toolRegistrationMetadata, [32]byte, error) {
	if def == nil {
		return toolRegistrationMetadata{}, [32]byte{}, errors.New("tools: cannot register a nil tool definition")
	}
	name := def.Name()
	if name == "" {
		return toolRegistrationMetadata{}, [32]byte{}, errors.New("tools: cannot register a tool with an empty name")
	}
	metadata := toolRegistrationMetadata{
		description:     def.Description(),
		effects:         newTrustedEffectSet(effects.Values()...),
		completionProof: completionProof,
	}
	if parameterized, ok := def.(Parameterized); ok {
		metadata.hasParameters = true
		schema, err := freezeToolSchema(parameterized.Parameters())
		if err != nil {
			return toolRegistrationMetadata{}, [32]byte{}, fmt.Errorf("tools: snapshot schema for %q: %w", name, err)
		}
		metadata.schema = schema
	}
	if guideliner, ok := def.(PromptGuideliner); ok {
		metadata.hasPromptGuidelines = true
		metadata.promptGuidelines = append([]string(nil), guideliner.PromptGuidelines()...)
	}
	_, metadata.hasStreaming = def.(StreamingBashTool)
	_, metadata.hasTopTools = def.(TopToolsProvider)

	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return toolRegistrationMetadata{}, [32]byte{}, fmt.Errorf("tools: create executor fingerprint: %w", err)
	}
	return metadata, sha256.Sum256(nonce[:]), nil
}

func (f *DispatchFence) publishChecked(domain DispatchFenceDomain, expected *DispatchFenceSnapshot, update func() error) (DispatchFenceSnapshot, error) {
	if f == nil {
		return DispatchFenceSnapshot{}, ErrDispatchFenceUnavailable
	}
	if update == nil {
		return DispatchFenceSnapshot{}, errors.New("tools: dispatch fence update is nil")
	}
	if domain != DispatchFenceRegistry && domain != DispatchFenceApproval && domain != DispatchFencePlan {
		return DispatchFenceSnapshot{}, ErrInvalidDispatchFenceDomain
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if expected != nil && f.generations != *expected {
		return f.generations, ErrDispatchFenceChanged
	}
	if err := update(); err != nil {
		return f.generations, err
	}
	switch domain {
	case DispatchFenceRegistry:
		f.generations.Registry++
	case DispatchFenceApproval:
		f.generations.Approval++
	case DispatchFencePlan:
		f.generations.Plan++
	}
	return f.generations, nil
}
