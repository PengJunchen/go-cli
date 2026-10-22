package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"sync"
)

const (
	// RegistrationSourceToolRegistry identifies ordinary registrations, which
	// receive no trusted effects by default.
	RegistrationSourceToolRegistry = "tool_registry"
	// RegistrationSourceTrustedControl identifies the narrow, explicit
	// first-party control registration path.
	RegistrationSourceTrustedControl = "builtin_control"
	// RegistrationSourceBuiltin identifies reviewed first-party tools.
	RegistrationSourceBuiltin = "builtin_tool"
	// RegistrationSourceTrustedNetworkRead identifies concrete first-party
	// network readers registered through the narrow reviewed path.
	RegistrationSourceTrustedNetworkRead = "builtin_network_read"
	// RegistrationSourceProjectSourced identifies a tool whose executable
	// definition was captured from a verified project source manifest.
	RegistrationSourceProjectSourced = "project_sourced"
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
	// ErrProjectSourcedRegistrationUnavailable indicates that a registry cannot
	// preserve the trust binding for a project-sourced tool.
	ErrProjectSourcedRegistrationUnavailable = errors.New("tools: project-sourced registration is unavailable")
	// ErrProjectTrustBindingInvalid indicates a missing or invalid project trust
	// capability at registration time.
	ErrProjectTrustBindingInvalid = errors.New("tools: project trust binding is invalid")
)

// TrustedControlRegistrar is the narrow registration capability used by
// first-party assembly for reviewed control-plane tools. Ordinary Register
// calls, including MCP, extension, and user-defined tools, remain unknown.
type TrustedControlRegistrar interface {
	RegisterTrustedControl(context.Context, ToolDefinition) error
}

// TrustedInlineControlRegistrar is a host-only declaration that a reviewed
// control tool owns no work after Execute returns. It must not be used for
// extension callbacks or controls that hand untracked work to another owner.
type TrustedInlineControlRegistrar interface {
	RegisterTrustedInlineControl(context.Context, ToolDefinition) error
}

// TrustedNetworkReadRegistrar exposes only the reviewed first-party network
// readers. It does not accept a caller-selected effect or scope.
type TrustedNetworkReadRegistrar interface {
	RegisterTrustedNetworkRead(context.Context, ToolDefinition) error
}

// ProjectTrustBinding is the narrow tools-layer view of a manager-minted
// verified project-source capability. Tools depend on this interface instead
// of importing approval.
type ProjectTrustBinding interface {
	CanonicalRoot() string
	TrustGeneration() uint64
	Valid() bool
	CommitIfCurrent(context.Context, func() error) error
}

// ProjectSourcedToolRegistrar registers a tool only when its verified project
// trust binding can be captured by prepared dispatch.
type ProjectSourcedToolRegistrar interface {
	RegisterProjectSourced(context.Context, ToolDefinition, ProjectTrustBinding) error
}

// ProjectSourcedDeferredToolRegistrar retains a verified project binding until
// a deferred project tool is materialized.
type ProjectSourcedDeferredToolRegistrar interface {
	RegisterProjectSourcedDeferred(context.Context, string, func() (ToolDefinition, error), ProjectTrustBinding) error
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
	identity                TrustedToolIdentity
	effects                 TrustedEffectSet
	completionProof         PreparedCompletionProof
	dispatchLease           PreparedDispatchLease
	canonicalWorkspaceRoot  string
	workspaceReadWhitelist  []string
	scopedReadProofEligible bool
	schema                  ToolSchemaSnapshot
	hasParameters           bool
	description             string
	hasPromptGuidelines     bool
	promptGuidelines        []string
	hasStreaming            bool
	hasTopTools             bool
	projectTrustBinding     ProjectTrustBinding
}

type registeredToolSnapshot struct {
	definition              ToolDefinition
	identity                TrustedToolIdentity
	effects                 TrustedEffectSet
	completionProof         PreparedCompletionProof
	dispatchLease           PreparedDispatchLease
	canonicalWorkspaceRoot  string
	workspaceReadWhitelist  []string
	scopedReadProofEligible bool
	schema                  ToolSchemaSnapshot
	hasParameters           bool
	description             string
	hasPromptGuidelines     bool
	promptGuidelines        []string
	hasStreaming            bool
	hasTopTools             bool
	projectTrustBinding     ProjectTrustBinding
	generations             DispatchFenceSnapshot
	pipeline                preparedToolPipelineSnapshot
}

type registrationSnapshotProvider interface {
	toolRegistrationSnapshot(context.Context, string) (registeredToolSnapshot, error)
}

func validProjectTrustBinding(binding ProjectTrustBinding) bool {
	if binding == nil {
		return false
	}
	value := reflect.ValueOf(binding)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return false
		}
	}
	return binding.Valid() && binding.CanonicalRoot() != "" && binding.TrustGeneration() > 0
}

// PreparedToolStages separates argument normalization from execution
// decoration and mutation scheduling. Effects and identity are intentionally
// absent: only registration provenance can mint those values. ExecutionWrappers
// are trusted, audited assembly decorators: they must not add tool-visible
// file, process, or network effects, escape asynchronously, or mutate the
// frozen call. The fixed CLI wrapper chain may also synchronously append its
// already-configured audit record and update in-memory telemetry. This narrow
// allowance does not attest arbitrary custom wrappers or extension hooks.
type PreparedToolStages struct {
	ArgumentPreparers []ArgumentPreparer
	ExecutionWrappers []ToolExecutorWrapper
	// PreparedAttemptObserver synchronously observes the authoritative frozen
	// identity and normalized argument digest after validation and before policy.
	// It cannot approve, dispatch, or mint a PreparedInvocation.
	PreparedAttemptObserver PreparedAttemptObserver
	// SynchronousExecutionWrappers is an explicit trusted-assembly attestation
	// that every configured execution wrapper returns only after all work it
	// starts has stopped and preserves the frozen call. The current CLI caller
	// sets it only for its fixed reviewed wrapper chain, with a nil result cache,
	// optional DefaultAuditLog, and DefaultTelemetry. That chain may perform its
	// configured synchronous audit append and in-memory telemetry update; this
	// is not permission to attest arbitrary side-effect wrappers or hooks.
	// Completion covers those synchronous calls returning, but does not wait for
	// tracing spans they emit to flush through the CLI-owned tracing exporter.
	// Pending diagnostic export is not evidence of a tool mutation still running.
	// Without it, non-empty wrappers suppress prepared completion proofs.
	SynchronousExecutionWrappers bool
	MutationQueue                FileMutationQueue
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
			ArgumentPreparers:            append([]ArgumentPreparer(nil), p.stages.ArgumentPreparers...),
			ExecutionWrappers:            append([]ToolExecutorWrapper(nil), p.stages.ExecutionWrappers...),
			PreparedAttemptObserver:      p.stages.PreparedAttemptObserver,
			SynchronousExecutionWrappers: p.stages.SynchronousExecutionWrappers,
			MutationQueue:                p.stages.MutationQueue,
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
		ArgumentPreparers:            append([]ArgumentPreparer(nil), stages.ArgumentPreparers...),
		ExecutionWrappers:            append([]ToolExecutorWrapper(nil), stages.ExecutionWrappers...),
		PreparedAttemptObserver:      stages.PreparedAttemptObserver,
		SynchronousExecutionWrappers: stages.SynchronousExecutionWrappers,
		MutationQueue:                stages.MutationQueue,
	}
	p.stagesConfigured = true
}

type preparedToolPipelineProvider interface {
	preparedToolPipeline() (*preparedToolPipelineState, *DispatchFence)
}

type trustedWorkspaceWriteRegistrar interface {
	registerTrustedWorkspaceWrite(context.Context, ToolDefinition) error
}

type trustedLocalProcessRegistrar interface {
	registerTrustedLocalProcess(context.Context, ToolDefinition) error
}

type trustedInlineLocalReaderRegistrar interface {
	registerTrustedInlineLocalReader(context.Context, ToolDefinition) error
}

type trustedScopedWorkspaceReadRegistrar interface {
	registerTrustedScopedWorkspaceRead(context.Context, ToolDefinition) error
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
	completionProof := PreparedCompletionProof("")
	if askUser, ok := def.(*AskUserQuestionTool); ok {
		if askUser == nil {
			return errors.New("tools: trusted ask_user control must be non-nil")
		}
		// AskUserQuestionTool returns only after the matching answer has been
		// delivered, and it owns no detached work after Execute returns. Grant
		// this proof only to that concrete control tool; other trusted controls
		// keep the default unproven completion behavior.
		completionProof = CompletionInlinePipelineJoined
	}
	return r.registerDefinitionWithCompletion(def, RegistrationSourceTrustedControl, newTrustedEffectSet(EffectControl), completionProof)
}

// RegisterTrustedInlineControl records an explicit host completion contract;
// ordinary Register and RegisterTrustedControl retain their existing proofs.
func (r *DefaultToolRegistry) RegisterTrustedInlineControl(_ context.Context, def ToolDefinition) error {
	return r.registerDefinitionWithCompletion(def, RegistrationSourceTrustedControl, newTrustedEffectSet(EffectControl), CompletionInlinePipelineJoined)
}

func (r *DefaultToolRegistry) RegisterTrustedNetworkRead(ctx context.Context, def ToolDefinition) error {
	switch def.(type) {
	case *WebSearchTool, *WebFetchTool:
		return r.registerDefinition(def, RegistrationSourceTrustedNetworkRead, newTrustedEffectSet(EffectNetworkRead))
	default:
		return ErrUnreviewedNetworkReadDefinition
	}
}

func (r *DefaultToolRegistry) RegisterProjectSourced(ctx context.Context, def ToolDefinition, binding ProjectTrustBinding) error {
	if !validProjectTrustBinding(binding) {
		return ErrProjectTrustBindingInvalid
	}
	return r.registerDefaultToolGuardedWithBinding(ctx, def, binding)
}

func (r *DefaultToolRegistry) registerTrustedWorkspaceWrite(ctx context.Context, def ToolDefinition) error {
	switch def.(type) {
	case *WriteTool, *EditFileTool:
		proof := PreparedCompletionProof("")
		if supportsInlineWorkspaceCompletion(def) {
			proof = CompletionInlinePipelineJoined
		} else if supportsQueuedWorkspaceCompletion(def) {
			proof = CompletionQueuedJobJoined
		}
		return r.registerDefinitionWithCompletion(def, RegistrationSourceBuiltin, newTrustedEffectSet(EffectWorkspaceWrite), proof)
	default:
		return errors.New("tools: workspace-write registration requires a reviewed concrete tool")
	}
}

func (r *DefaultToolRegistry) registerTrustedLocalProcess(ctx context.Context, def ToolDefinition) error {
	switch tool := def.(type) {
	case *BashTool:
		if tool == nil {
			return errors.New("tools: local-process registration requires a non-nil bash tool")
		}
		proof := PreparedCompletionProof("")
		if supportsLocalProcessCompletion(tool) {
			proof = completionLocalProcessOrPrestartNoProcessJoined
		}
		return r.registerDefinitionWithCompletion(def, RegistrationSourceBuiltin, newTrustedEffectSet(), proof)
	case *StreamingBashToolImpl:
		if tool == nil || tool.BashTool == nil {
			return errors.New("tools: streaming bash registration requires a non-nil base tool")
		}
		proof := PreparedCompletionProof("")
		if supportsLocalProcessCompletion(tool.BashTool) {
			proof = completionLocalProcessOrPrestartNoProcessJoined
		}
		return r.registerDefinitionWithCompletion(def, RegistrationSourceBuiltin, newTrustedEffectSet(), proof)
	default:
		return errors.New("tools: local-process completion requires a reviewed local bash tool")
	}
}

func (r *DefaultToolRegistry) registerTrustedInlineLocalReader(ctx context.Context, def ToolDefinition) error {
	switch tool := def.(type) {
	case *ReadTool:
		if tool == nil {
			return errors.New("tools: inline read registration requires a non-nil read tool")
		}
	case *LSTool:
		if tool == nil {
			return errors.New("tools: inline read registration requires a non-nil ls tool")
		}
	default:
		return errors.New("tools: inline local read registration requires a reviewed built-in reader")
	}
	return r.registerDefinitionWithCompletion(def, RegistrationSourceBuiltin, newTrustedEffectSet(), CompletionInlinePipelineJoined)
}

func (r *DefaultToolRegistry) registerTrustedScopedWorkspaceRead(ctx context.Context, def ToolDefinition) error {
	var whitelist PathWhitelist
	completionProof := PreparedCompletionProof("")
	canonicalRoot := ""
	proofEligibleReader := false
	completionOnlySearchReader := false
	switch tool := def.(type) {
	case *ReadTool:
		if tool == nil {
			return errors.New("tools: scoped workspace-read registration requires a non-nil read tool")
		}
		whitelist = tool.whitelist
		completionProof = CompletionInlinePipelineJoined
		canonicalRoot = tool.canonicalWorkspaceRoot
		proofEligibleReader = true
	case *LSTool:
		if tool == nil {
			return errors.New("tools: scoped workspace-read registration requires a non-nil ls tool")
		}
		whitelist = tool.whitelist
		completionProof = CompletionInlinePipelineJoined
		canonicalRoot = tool.canonicalWorkspaceRoot
		proofEligibleReader = true
	case *GrepTool:
		if tool == nil {
			return errors.New("tools: scoped workspace-read registration requires a non-nil grep tool")
		}
		whitelist = tool.whitelist
		completionProof = completionScopedSearchJoined
		completionOnlySearchReader = true
	case *FindTool:
		if tool == nil {
			return errors.New("tools: scoped workspace-read registration requires a non-nil find tool")
		}
		whitelist = tool.whitelist
		completionProof = completionScopedSearchJoined
		completionOnlySearchReader = true
	default:
		return errors.New("tools: scoped workspace-read registration requires a reviewed concrete reader")
	}

	if whitelist.invalid || len(whitelist.paths) == 0 {
		if completionProof != "" {
			source := RegistrationSourceBuiltin
			if completionOnlySearchReader {
				source = RegistrationSourceToolRegistry
			}
			return r.registerDefinitionWithScopeAndCompletion(def, source, newTrustedEffectSet(), TrustedToolScope{}, completionProof)
		}
		return r.Register(ctx, def)
	}

	scope := trustedWorkspaceReadScope(whitelist.paths)
	if scope.Fingerprint() == ([32]byte{}) {
		return errors.New("tools: scoped workspace-read registration requires a non-empty canonical scope")
	}
	if proofEligibleReader && canonicalRoot != "" {
		return r.registerDefinitionWithScopedWorkspaceRead(def, scope, canonicalRoot, whitelist.paths)
	}
	return r.registerDefinitionWithScopeAndCompletion(def, RegistrationSourceBuiltin, newTrustedEffectSet(EffectWorkspaceRead), scope, completionProof)
}

func trustedWorkspaceReadScope(paths []string) TrustedToolScope {
	if len(paths) == 0 {
		return TrustedToolScope{}
	}

	canonical := append([]string(nil), paths...)
	sort.Strings(canonical)
	unique := canonical[:0]
	for _, root := range canonical {
		if len(unique) == 0 || unique[len(unique)-1] != root {
			unique = append(unique, root)
		}
	}

	var encoded bytes.Buffer
	encoded.WriteString("go-cli/workspace-read-scope/v1\x00")
	var length [8]byte
	for _, root := range unique {
		binary.BigEndian.PutUint64(length[:], uint64(len(root)))
		_, _ = encoded.Write(length[:])
		_, _ = encoded.WriteString(root)
	}
	return newTrustedToolScope(encoded.String())
}

func supportsLocalProcessCompletion(tool *BashTool) bool {
	if tool == nil {
		return false
	}
	switch typed := tool.Sandbox.(type) {
	case AllowAllSandbox:
		return true
	case *AllowAllSandbox:
		return typed != nil
	case *DefaultBashSandbox:
		return typed != nil
	default:
		return false
	}
}

func supportsInlineWorkspaceCompletion(def ToolDefinition) bool {
	synchronousDiff := func(generator DiffGenerator) bool {
		if generator == nil {
			return true
		}
		_, ok := generator.(*UnifiedDiffGenerator)
		return ok
	}
	switch tool := def.(type) {
	case *WriteTool:
		return synchronousDiff(tool.diffGenerator)
	case *EditFileTool:
		return synchronousDiff(tool.diffGenerator)
	default:
		return false
	}
}

func supportsQueuedWorkspaceCompletion(def ToolDefinition) bool {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return false
	}
	managedGitDiff := func(generator DiffGenerator) bool {
		gitGenerator, ok := generator.(*GitDiffGenerator)
		if !ok || gitGenerator == nil {
			return false
		}
		gitTool, ok := gitGenerator.git.(*DefaultGitTool)
		if !ok || gitTool == nil {
			return false
		}
		fallback, ok := gitGenerator.fallback.(*UnifiedDiffGenerator)
		return ok && fallback != nil
	}
	switch tool := def.(type) {
	case *WriteTool:
		return tool != nil && managedGitDiff(tool.diffGenerator)
	case *EditFileTool:
		return tool != nil && managedGitDiff(tool.diffGenerator)
	default:
		return false
	}
}

func (r *DefaultToolRegistry) registerDefinition(def ToolDefinition, source string, effects TrustedEffectSet) error {
	return r.registerDefinitionWithCompletion(def, source, effects, "")
}

func (r *DefaultToolRegistry) registerDefinitionWithCompletion(def ToolDefinition, source string, effects TrustedEffectSet, completionProof PreparedCompletionProof) error {
	return r.registerDefinitionWithScopeAndCompletion(def, source, effects, TrustedToolScope{}, completionProof)
}

func (r *DefaultToolRegistry) registerDefinitionWithScopeAndCompletion(def ToolDefinition, source string, effects TrustedEffectSet, scope TrustedToolScope, completionProof PreparedCompletionProof) error {
	return r.registerDefinitionWithRuntimeReadScope(def, source, effects, scope, completionProof, "", nil, false)
}

func (r *DefaultToolRegistry) registerDefinitionWithScopedWorkspaceRead(def ToolDefinition, scope TrustedToolScope, canonicalRoot string, whitelist []string) error {
	return r.registerDefinitionWithRuntimeReadScope(
		def, RegistrationSourceBuiltin, newTrustedEffectSet(EffectWorkspaceRead), scope,
		CompletionInlinePipelineJoined, canonicalRoot, whitelist, true,
	)
}

func (r *DefaultToolRegistry) registerDefinitionWithRuntimeReadScope(
	def ToolDefinition,
	source string,
	effects TrustedEffectSet,
	scope TrustedToolScope,
	completionProof PreparedCompletionProof,
	canonicalRoot string,
	workspaceReadWhitelist []string,
	scopedReadProofEligible bool,
) error {
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
		description:             def.Description(),
		effects:                 newTrustedEffectSet(effects.Values()...),
		completionProof:         completionProof,
		canonicalWorkspaceRoot:  canonicalRoot,
		workspaceReadWhitelist:  append([]string(nil), workspaceReadWhitelist...),
		scopedReadProofEligible: scopedReadProofEligible,
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

	_, err := r.fence.publishChecked(DispatchFenceRegistry, nil, func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ensureMCPMapsLocked()
		if owner, reserved := r.mcpReservations[name]; reserved {
			return fmt.Errorf("%w: %q is reserved by MCP source %q generation %d", ErrMCPNameReserved, name, owner.sourceID, owner.generation)
		}

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
		metadata.identity = newTrustedToolIdentity(source, name, generation, fingerprint, scope)
		r.tools[name] = def
		r.metadata[name] = metadata
		r.version++
		return nil
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
			definition:              definition,
			identity:                metadata.identity,
			effects:                 newTrustedEffectSet(metadata.effects.Values()...),
			completionProof:         metadata.completionProof,
			dispatchLease:           metadata.dispatchLease,
			canonicalWorkspaceRoot:  metadata.canonicalWorkspaceRoot,
			workspaceReadWhitelist:  append([]string(nil), metadata.workspaceReadWhitelist...),
			scopedReadProofEligible: metadata.scopedReadProofEligible,
			schema:                  metadata.schema,
			hasParameters:           metadata.hasParameters,
			description:             metadata.description,
			hasPromptGuidelines:     metadata.hasPromptGuidelines,
			promptGuidelines:        append([]string(nil), metadata.promptGuidelines...),
			hasStreaming:            metadata.hasStreaming,
			hasTopTools:             metadata.hasTopTools,
			projectTrustBinding:     metadata.projectTrustBinding,
			generations:             generations,
			pipeline:                r.prepared.snapshot(),
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
