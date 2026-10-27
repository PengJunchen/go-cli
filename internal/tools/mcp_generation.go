package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
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
	// ErrMCPGenerationCandidateInvalid reports an MCP candidate without frozen
	// metadata or a generation-scoped execution handle.
	ErrMCPGenerationCandidateInvalid = errors.New("tools: MCP generation candidate is invalid")
)

// PreparedDispatchLease pins one exact MCP client generation for a committed
// prepared invocation. Acquire and its release function must not take DTR,
// owner, or DispatchFence locks.
type PreparedDispatchLease interface {
	Acquire() (release func(), err error)
}

// MCPGenerationClient executes one remote MCP tool through a source-owned
// client generation. Implementations must remain bound to that generation;
// callers must not reconnect or reconfigure a client in place across sources.
type MCPGenerationClient interface {
	ExecuteMCPTool(ctx context.Context, remoteToolName string, call ToolCall) (*ToolResult, error)
}

// MCPGenerationClientHandle is an opaque reference to one MCP client
// generation. Its zero value is invalid.
type MCPGenerationClientHandle struct {
	client MCPGenerationClient
	owned  *ownedMCPRequestExecutor
}

// ownedMCPRequestExecutor is minted only by the explicit host-owned constructor.
// Capture the exact receiver's method before publication, so embedding a handle
// cannot replace its executor while retaining request-completion authority.
type ownedMCPRequestExecutor struct {
	execute func(context.Context, string, ToolCall) (*ToolResult, error)
}

// NewMCPGenerationClientHandle creates a generic generation handle. Optional
// interfaces, names and embedded owned generations never grant completion proof.
func NewMCPGenerationClientHandle(client MCPGenerationClient) (MCPGenerationClientHandle, error) {
	if isNilMCPGenerationClient(client) {
		return MCPGenerationClientHandle{}, fmt.Errorf("%w: missing client generation", ErrMCPGenerationCandidateInvalid)
	}
	return MCPGenerationClientHandle{client: client}, nil
}

// NewOwnedMCPGenerationClientHandle explicitly attests a host-reviewed request
// adapter's local synchronous lifecycle. The host must supply its exact owned
// generation, not a remote descriptor or a user-provided wrapper. This captures
// only completion authority; tool effects and approval remain unchanged.
func NewOwnedMCPGenerationClientHandle(client MCPGenerationClient) (MCPGenerationClientHandle, error) {
	handle, err := NewMCPGenerationClientHandle(client)
	if err != nil {
		return MCPGenerationClientHandle{}, err
	}
	handle.owned = &ownedMCPRequestExecutor{execute: client.ExecuteMCPTool}
	return handle, nil
}

func (h MCPGenerationClientHandle) valid() bool {
	return !isNilMCPGenerationClient(h.client)
}

func (h MCPGenerationClientHandle) execute(ctx context.Context, remoteToolName string, call ToolCall) (*ToolResult, error) {
	if !h.valid() {
		return nil, fmt.Errorf("%w: missing client generation", ErrMCPGenerationCandidateInvalid)
	}
	if h.owned != nil {
		return h.owned.execute(ctx, remoteToolName, call)
	}
	return h.client.ExecuteMCPTool(ctx, remoteToolName, call)
}

func isNilMCPGenerationClient(client MCPGenerationClient) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return value.IsNil()
	default:
		return false
	}
}

func isNilPreparedDispatchLease(lease PreparedDispatchLease) bool {
	if lease == nil {
		return true
	}
	value := reflect.ValueOf(lease)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return value.IsNil()
	default:
		return false
	}
}

// MCPToolCandidate is one immutable tool descriptor in an unpublished MCP
// generation. Its fields are private so callers cannot replace the frozen
// metadata or omit its typed client-generation handle.
type MCPToolCandidate struct {
	name                string
	remoteName          string
	description         string
	schema              ToolSchemaSnapshot
	client              MCPGenerationClientHandle
	lease               PreparedDispatchLease
	projectTrustBinding ProjectTrustBinding
}

// NewMCPToolCandidate snapshots one remote MCP tool's public descriptor and
// binds it to an exact client-generation handle.
func NewMCPToolCandidate(
	registryName string,
	remoteToolName string,
	description string,
	schema any,
	client MCPGenerationClientHandle,
	lease PreparedDispatchLease,
) (MCPToolCandidate, error) {
	if strings.TrimSpace(registryName) == "" || strings.TrimSpace(remoteToolName) == "" || !client.valid() || isNilPreparedDispatchLease(lease) {
		return MCPToolCandidate{}, fmt.Errorf("%w: missing tool name, client generation, or dispatch lease", ErrMCPGenerationCandidateInvalid)
	}
	frozenSchema, err := freezeToolSchema(schema)
	if err != nil {
		return MCPToolCandidate{}, fmt.Errorf("%w: snapshot MCP schema: %w", ErrMCPGenerationCandidateInvalid, err)
	}
	if !frozenSchema.Present() {
		return MCPToolCandidate{}, fmt.Errorf("%w: MCP tool %q has no input schema", ErrMCPGenerationCandidateInvalid, registryName)
	}
	if err := validatePreparedSchemaNode(frozenSchema.Schema(), true); err != nil {
		return MCPToolCandidate{}, fmt.Errorf("%w: validate MCP schema for %q: %w", ErrMCPGenerationCandidateInvalid, registryName, err)
	}
	return MCPToolCandidate{
		name:        registryName,
		remoteName:  remoteToolName,
		description: description,
		schema:      frozenSchema,
		client:      client,
		lease:       lease,
	}, nil
}

// WithProjectTrustBinding returns a copy bound to one verified project
// generation. Candidates without this binding remain eligible only for
// non-project MCP sources.
func (candidate MCPToolCandidate) WithProjectTrustBinding(binding ProjectTrustBinding) (MCPToolCandidate, error) {
	if !validProjectTrustBinding(binding) {
		return MCPToolCandidate{}, ErrProjectTrustBindingInvalid
	}
	candidate.projectTrustBinding = binding
	return candidate, nil
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
	appendOnly  bool
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

type frozenMCPGenerationDefinition struct {
	name        string
	description string
	remoteName  string
	schema      ToolSchemaSnapshot
	client      MCPGenerationClientHandle
}

func (d *frozenMCPGenerationDefinition) Name() string        { return d.name }
func (d *frozenMCPGenerationDefinition) Description() string { return d.description }
func (d *frozenMCPGenerationDefinition) Parameters() any     { return d.schema.Schema() }
func (d *frozenMCPGenerationDefinition) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if d == nil || !d.client.valid() {
		return nil, ErrPreparedInvocationUnavailable
	}
	if call.Name != d.name {
		return nil, ErrPreparedProxyNameMismatch
	}
	return d.client.execute(ctx, d.remoteName, call)
}

// MCPGenerationUpdate owns the DTR write lock until Publish, Disable, or Abort.
// It stages only local metadata; client construction and network work must be
// complete before BeginMCPGenerationUpdate is called.
type MCPGenerationUpdate struct {
	dtr         *DeferredToolRegistryAdapter
	registry    *DefaultToolRegistry
	sourceID    string
	generation  uint64
	oldNames    map[string]struct{}
	candidates  []mcpGenerationCandidate
	appendOnly  bool
	materialize bool
	active      bool
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

// BeginMCPGenerationAddition requests additive disclosure for a source never
// published before. Existing/revoked sources retain the normal replacement fence.
// Names are revalidated atomically under the dispatch fence before publication.
func (a *DeferredToolRegistryAdapter) BeginMCPGenerationAddition(ctx context.Context, sourceID string, generation uint64, candidates []MCPToolCandidate) (*MCPGenerationUpdate, error) {
	update, err := a.BeginMCPGenerationUpdate(ctx, sourceID, generation, candidates)
	if err != nil {
		return nil, err
	}
	_, exists := a.mcpSources[sourceID]
	update.appendOnly = !exists
	return update, nil
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

// PublishMaterialized publishes frozen definitions and exact executors in one
// checked transaction. No deferred loader or network work remains after commit.
func (u *MCPGenerationUpdate) PublishMaterialized() error {
	if !u.isActive() {
		return ErrMCPGenerationTransactionClosed
	}
	u.materialize = true
	return u.Publish()
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
	if strings.TrimSpace(candidate.name) == "" || strings.TrimSpace(candidate.remoteName) == "" || !candidate.client.valid() || isNilPreparedDispatchLease(candidate.lease) {
		return mcpGenerationCandidate{}, fmt.Errorf("%w: missing tool name, client generation, or dispatch lease", ErrMCPGenerationCandidateInvalid)
	}
	if !candidate.schema.Present() {
		return mcpGenerationCandidate{}, fmt.Errorf("%w: MCP tool %q has no input schema", ErrMCPGenerationCandidateInvalid, candidate.name)
	}
	if err := validatePreparedSchemaNode(candidate.schema.Schema(), true); err != nil {
		return mcpGenerationCandidate{}, fmt.Errorf("%w: validate MCP schema for %q: %w", ErrMCPGenerationCandidateInvalid, candidate.name, err)
	}
	metadata := toolRegistrationMetadata{
		description:   candidate.description,
		effects:       unknownTrustedEffects(),
		dispatchLease: candidate.lease,
		hasParameters: true,
		schema:        candidate.schema,
	}
	if candidate.client.owned != nil {
		metadata.completionProof = CompletionMCPRequestJoined
	}
	if candidate.projectTrustBinding != nil {
		if !validProjectTrustBinding(candidate.projectTrustBinding) {
			return mcpGenerationCandidate{}, ErrProjectTrustBindingInvalid
		}
		metadata.projectTrustBinding = candidate.projectTrustBinding
	}
	frozenDefinition := &frozenMCPGenerationDefinition{
		name:        candidate.name,
		description: candidate.description,
		remoteName:  candidate.remoteName,
		schema:      candidate.schema,
		client:      candidate.client,
	}

	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return mcpGenerationCandidate{}, fmt.Errorf("tools: create MCP executor fingerprint: %w", err)
	}
	return mcpGenerationCandidate{
		name:        candidate.name,
		definition:  frozenDefinition,
		lease:       candidate.lease,
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
	if update.appendOnly && !disable {
		published, err := r.publishMCPAddition(update)
		if published || err != nil {
			return err
		}
		update.appendOnly = false
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

// publishMCPAddition preserves the authority fence only when no previously
// visible registration or source can change. The caller holds the DTR lock.
func (r *DefaultToolRegistry) publishMCPAddition(update *MCPGenerationUpdate) (bool, error) {
	r.fence.mu.Lock()
	defer r.fence.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureMCPMapsLocked()
	if _, exists := update.dtr.mcpSources[update.sourceID]; exists || len(update.oldNames) > 0 {
		return false, nil
	}
	for _, candidate := range update.candidates {
		if _, exists := r.tools[candidate.name]; exists {
			return false, nil
		}
		if _, reserved := r.mcpReservations[candidate.name]; reserved {
			return false, nil
		}
	}
	names := make(map[string]struct{}, len(update.candidates))
	for _, candidate := range update.candidates {
		r.mcpReservations[candidate.name] = mcpGenerationOwner{sourceID: update.sourceID, generation: update.generation}
		names[candidate.name] = struct{}{}
	}
	r.version++
	update.applyDTRState(false, names)
	return true, nil
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
				appendOnly:  u.appendOnly,
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
			if u.materialize {
				metadata := candidate.metadata
				metadata.identity = newTrustedToolIdentity(RegistrationSourceMCP, name, u.generation, candidate.fingerprint, TrustedToolScope{})
				u.registry.tools[name] = definition
				u.registry.metadata[name] = metadata
				u.registry.order = append(u.registry.order, name)
				u.registry.mcpMaterialized[name] = mcpMaterializedOwner{mcpGenerationOwner: owner, lease: candidate.lease}
				u.dtr.loaded[name] = definition
			}
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
	publish := func() error {
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
	}
	if entry.appendOnly {
		r.fence.mu.Lock()
		defer r.fence.mu.Unlock()
		return publish()
	}
	_, err := r.fence.publishChecked(DispatchFenceRegistry, nil, publish)
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
	return r.registerDefaultToolGuardedWithBinding(ctx, def, nil)
}

func (r *DefaultToolRegistry) registerDefaultToolGuardedWithBinding(ctx context.Context, def ToolDefinition, binding ProjectTrustBinding) error {
	proof := PreparedCompletionProof("")
	if discovery, ok := def.(*MCPDiscoveryTool); ok && discovery != nil && discovery.owned != nil {
		proof = CompletionMCPRequestJoined
	}
	metadata, fingerprint, err := prepareRegistrationMetadata(def, unknownTrustedEffects(), proof)
	if err != nil {
		return err
	}
	metadata.projectTrustBinding = binding
	name := def.Name()
	if r == nil || r.fence == nil {
		return ErrDispatchFenceUnavailable
	}
	source := RegistrationSourceToolRegistry
	if binding != nil {
		source = RegistrationSourceProjectSourced
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
		metadata.identity = newTrustedToolIdentity(source, name, generation, fingerprint, TrustedToolScope{})
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
	var snapshot DispatchFenceSnapshot
	err := f.withPlanRestriction(func(parentPlan uint64) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		snapshot = f.snapshotWithParentPlan(parentPlan)
		if expected != nil && snapshot != *expected {
			return ErrDispatchFenceChanged
		}
		if err := update(); err != nil {
			return err
		}
		switch domain {
		case DispatchFenceRegistry:
			f.generations.Registry++
		case DispatchFenceApproval:
			f.generations.Approval++
		case DispatchFencePlan:
			f.generations.Plan++
		}
		snapshot = f.snapshotWithParentPlan(parentPlan)
		return nil
	})
	return snapshot, err
}

// PreviewDefinition returns detached model metadata for candidate budget/plan
// compilation. It intentionally cannot invoke the unpublished executor.
func (candidate MCPToolCandidate) PreviewDefinition() ToolDefinition {
	return &mcpCandidatePreview{name: candidate.name, description: candidate.description, schema: candidate.schema}
}

type mcpCandidatePreview struct {
	name, description string
	schema            ToolSchemaSnapshot
}

func (p *mcpCandidatePreview) Name() string        { return p.name }
func (p *mcpCandidatePreview) Description() string { return p.description }
func (p *mcpCandidatePreview) Parameters() any     { return p.schema.Schema() }
func (p *mcpCandidatePreview) Execute(context.Context, ToolCall) (*ToolResult, error) {
	return nil, ErrPreparedInvocationUnavailable
}
