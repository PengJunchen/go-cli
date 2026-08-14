package core

import (
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// DefaultRegistry is the default implementation of Registry. It binds a
// default implementation for every subsystem interface and lets callers
// replace any of them via the RegisterXxx methods. All fields are interface
// types so the runtime always depends on abstractions rather than concrete
// implementations.
type DefaultRegistry struct {
	mu sync.RWMutex

	// Core layer.
	agentLoop  AgentLoop
	agent      Agent
	harness    Harness
	turnRunner TurnRunner

	// Service layer.
	sessionStore       SessionStore
	sessionTree        SessionTree
	contextManager     ContextManager
	compactor          Compactor
	tokenEstimator     TokenEstimator
	toolRegistry       tools.ToolRegistry
	modelProvider      llm.ModelProvider
	approvalClassifier ApprovalClassifier
	approvalStore      ApprovalStore
	traceExporter      tracing.TraceExporter

	// Extension layer.
	configProvider extension.ConfigProvider
	pluginLoader   PluginLoader

	// Disposers collected from RegisterXxxWithDisposer calls, invoked in
	// reverse order (LIFO) by DisposeAll.
	disposers []Disposer
}

// Compile-time guarantee that DefaultRegistry satisfies the Registry
// interface (DIP / dependency-inversion).
var _ Registry = (*DefaultRegistry)(nil)

// Compile-time guarantee that DefaultRegistry satisfies DisposableRegistry.
var _ DisposableRegistry = (*DefaultRegistry)(nil)

// NewRegistry returns a Registry whose every field is bound to a default stub
// implementation. No field is left as a nil interface.
func NewRegistry() Registry {
	return &DefaultRegistry{
		agentLoop:          &LoopAgent{},
		agent:              &AgentImpl{},
		harness:            &HarnessImpl{},
		turnRunner:         &EinoTurnRunner{},
		sessionStore:       &SessionStoreImpl{},
		sessionTree:        &SessionTreeImpl{},
		contextManager:     &ContextManagerImpl{},
		compactor:          &UnifiedCompactor{},
		tokenEstimator:     &HeuristicTokenEstimator{},
		toolRegistry:       &DefaultToolRegistry{},
		modelProvider:      &DefaultModelProvider{},
		approvalClassifier: &SafetyPolicyClassifier{},
		approvalStore:      &InMemoryApprovalStore{},
		traceExporter:      &NoopTraceExporter{},
		configProvider:     &DefaultConfigProvider{},
		pluginLoader:       &PluginLoaderImpl{},
	}
}

// replace swaps field under lock, panics on a nil next, and returns the
// previous value. It logs when an implementation is replaced.
func replace[T any](r Registry, field *T, name string, next T) (prev T) {
	if any(next) == nil {
		panic("registry: nil " + name)
	}
	dr, ok := r.(*DefaultRegistry)
	if !ok {
		panic("registry: expected *DefaultRegistry")
	}
	dr.mu.Lock()
	defer dr.mu.Unlock()
	prev = *field
	slog.Info("core.registry.replace", "component", name)
	*field = next
	return prev
}

// get returns the current value of field under a read lock.
func get[T any](r Registry, field *T) T {
	dr, ok := r.(*DefaultRegistry)
	if !ok {
		panic("registry: expected *DefaultRegistry")
	}
	dr.mu.RLock()
	defer dr.mu.RUnlock()
	return *field
}

// AgentLoop returns the bound AgentLoop implementation.
func (r *DefaultRegistry) AgentLoop() AgentLoop { return get(r, &r.agentLoop) }

// RegisterAgentLoop replaces the AgentLoop implementation and returns the old
// one. It panics if n is nil.
func (r *DefaultRegistry) RegisterAgentLoop(n AgentLoop) AgentLoop {
	return replace(r, &r.agentLoop, "AgentLoop", n)
}

// Agent returns the bound Agent implementation.
func (r *DefaultRegistry) Agent() Agent { return get(r, &r.agent) }

// RegisterAgent replaces the Agent implementation and returns the old one. It
// panics if n is nil.
func (r *DefaultRegistry) RegisterAgent(n Agent) Agent {
	return replace(r, &r.agent, "Agent", n)
}

// Harness returns the bound Harness implementation.
func (r *DefaultRegistry) Harness() Harness { return get(r, &r.harness) }

// RegisterHarness replaces the Harness implementation and returns the old one.
// It panics if n is nil.
func (r *DefaultRegistry) RegisterHarness(n Harness) Harness {
	return replace(r, &r.harness, "Harness", n)
}

// TurnRunner returns the bound TurnRunner implementation.
func (r *DefaultRegistry) TurnRunner() TurnRunner { return get(r, &r.turnRunner) }

// RegisterTurnRunner replaces the TurnRunner implementation and returns the
// old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterTurnRunner(n TurnRunner) TurnRunner {
	return replace(r, &r.turnRunner, "TurnRunner", n)
}

// SessionStore returns the bound SessionStore implementation.
func (r *DefaultRegistry) SessionStore() SessionStore { return get(r, &r.sessionStore) }

// RegisterSessionStore replaces the SessionStore implementation and returns
// the old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterSessionStore(n SessionStore) SessionStore {
	return replace(r, &r.sessionStore, "SessionStore", n)
}

// SessionTree returns the bound SessionTree implementation.
func (r *DefaultRegistry) SessionTree() SessionTree { return get(r, &r.sessionTree) }

// RegisterSessionTree replaces the SessionTree implementation and returns the
// old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterSessionTree(n SessionTree) SessionTree {
	return replace(r, &r.sessionTree, "SessionTree", n)
}

// ContextManager returns the bound ContextManager implementation.
func (r *DefaultRegistry) ContextManager() ContextManager { return get(r, &r.contextManager) }

// RegisterContextManager replaces the ContextManager implementation and
// returns the old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterContextManager(n ContextManager) ContextManager {
	return replace(r, &r.contextManager, "ContextManager", n)
}

// Compactor returns the bound Compactor implementation.
func (r *DefaultRegistry) Compactor() Compactor { return get(r, &r.compactor) }

// RegisterCompactor replaces the Compactor implementation and returns the old
// one. It panics if n is nil.
func (r *DefaultRegistry) RegisterCompactor(n Compactor) Compactor {
	return replace(r, &r.compactor, "Compactor", n)
}

// TokenEstimator returns the bound TokenEstimator implementation.
func (r *DefaultRegistry) TokenEstimator() TokenEstimator { return get(r, &r.tokenEstimator) }

// RegisterTokenEstimator replaces the TokenEstimator implementation and
// returns the old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterTokenEstimator(n TokenEstimator) TokenEstimator {
	return replace(r, &r.tokenEstimator, "TokenEstimator", n)
}

// ToolRegistry returns the bound ToolRegistry implementation.
func (r *DefaultRegistry) ToolRegistry() tools.ToolRegistry { return get(r, &r.toolRegistry) }

// RegisterToolRegistry replaces the ToolRegistry implementation and returns
// the old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterToolRegistry(n tools.ToolRegistry) tools.ToolRegistry {
	return replace(r, &r.toolRegistry, "ToolRegistry", n)
}

// ModelProvider returns the bound ModelProvider implementation.
func (r *DefaultRegistry) ModelProvider() llm.ModelProvider { return get(r, &r.modelProvider) }

// RegisterModelProvider replaces the ModelProvider implementation and returns
// the old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterModelProvider(n llm.ModelProvider) llm.ModelProvider {
	return replace(r, &r.modelProvider, "ModelProvider", n)
}

// ApprovalClassifier returns the bound ApprovalClassifier implementation.
func (r *DefaultRegistry) ApprovalClassifier() ApprovalClassifier {
	return get(r, &r.approvalClassifier)
}

// RegisterApprovalClassifier replaces the ApprovalClassifier implementation
// and returns the old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterApprovalClassifier(n ApprovalClassifier) ApprovalClassifier {
	return replace(r, &r.approvalClassifier, "ApprovalClassifier", n)
}

// ApprovalStore returns the bound ApprovalStore implementation.
func (r *DefaultRegistry) ApprovalStore() ApprovalStore { return get(r, &r.approvalStore) }

// RegisterApprovalStore replaces the ApprovalStore implementation and returns
// the old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterApprovalStore(n ApprovalStore) ApprovalStore {
	return replace(r, &r.approvalStore, "ApprovalStore", n)
}

// TraceExporter returns the bound TraceExporter implementation.
func (r *DefaultRegistry) TraceExporter() tracing.TraceExporter { return get(r, &r.traceExporter) }

// RegisterTraceExporter replaces the TraceExporter implementation and returns
// the old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterTraceExporter(n tracing.TraceExporter) tracing.TraceExporter {
	return replace(r, &r.traceExporter, "TraceExporter", n)
}

// ConfigProvider returns the bound ConfigProvider implementation.
func (r *DefaultRegistry) ConfigProvider() extension.ConfigProvider { return get(r, &r.configProvider) }

// RegisterConfigProvider replaces the ConfigProvider implementation and
// returns the old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterConfigProvider(n extension.ConfigProvider) extension.ConfigProvider {
	return replace(r, &r.configProvider, "ConfigProvider", n)
}

// PluginLoader returns the bound PluginLoader implementation.
func (r *DefaultRegistry) PluginLoader() PluginLoader { return get(r, &r.pluginLoader) }

// RegisterPluginLoader replaces the PluginLoader implementation and returns
// the old one. It panics if n is nil.
func (r *DefaultRegistry) RegisterPluginLoader(n PluginLoader) PluginLoader {
	return replace(r, &r.pluginLoader, "PluginLoader", n)
}

// ---------------------------------------------------------------------------
// Reversible registration (disposer pattern)
// ---------------------------------------------------------------------------

// RegisterAgentLoopWithDisposer replaces the AgentLoop implementation,
// returns the old one, and registers a disposer that restores the previous
// implementation when called.
func (r *DefaultRegistry) RegisterAgentLoopWithDisposer(n AgentLoop) (AgentLoop, Disposer) {
	prev := replace(r, &r.agentLoop, "AgentLoop", n)
	d := Disposer(func() {
		slog.Info("core.registry.dispose", "component", "AgentLoop")
		replace(r, &r.agentLoop, "AgentLoop", prev)
	})
	r.mu.Lock()
	r.disposers = append(r.disposers, d)
	r.mu.Unlock()
	return prev, d
}

// RegisterAgentWithDisposer replaces the Agent implementation, returns the
// old one, and registers a disposer that restores the previous implementation
// when called.
func (r *DefaultRegistry) RegisterAgentWithDisposer(n Agent) (Agent, Disposer) {
	prev := replace(r, &r.agent, "Agent", n)
	d := Disposer(func() {
		slog.Info("core.registry.dispose", "component", "Agent")
		replace(r, &r.agent, "Agent", prev)
	})
	r.mu.Lock()
	r.disposers = append(r.disposers, d)
	r.mu.Unlock()
	return prev, d
}

// RegisterHarnessWithDisposer replaces the Harness implementation, returns
// the old one, and registers a disposer that restores the previous
// implementation when called.
func (r *DefaultRegistry) RegisterHarnessWithDisposer(n Harness) (Harness, Disposer) {
	prev := replace(r, &r.harness, "Harness", n)
	d := Disposer(func() {
		slog.Info("core.registry.dispose", "component", "Harness")
		replace(r, &r.harness, "Harness", prev)
	})
	r.mu.Lock()
	r.disposers = append(r.disposers, d)
	r.mu.Unlock()
	return prev, d
}

// RegisterToolRegistryWithDisposer replaces the ToolRegistry implementation,
// returns the old one, and registers a disposer that restores the previous
// implementation when called.
func (r *DefaultRegistry) RegisterToolRegistryWithDisposer(n tools.ToolRegistry) (tools.ToolRegistry, Disposer) {
	prev := replace(r, &r.toolRegistry, "ToolRegistry", n)
	d := Disposer(func() {
		slog.Info("core.registry.dispose", "component", "ToolRegistry")
		replace(r, &r.toolRegistry, "ToolRegistry", prev)
	})
	r.mu.Lock()
	r.disposers = append(r.disposers, d)
	r.mu.Unlock()
	return prev, d
}

// RegisterModelProviderWithDisposer replaces the ModelProvider implementation,
// returns the old one, and registers a disposer that restores the previous
// implementation when called.
func (r *DefaultRegistry) RegisterModelProviderWithDisposer(n llm.ModelProvider) (llm.ModelProvider, Disposer) {
	prev := replace(r, &r.modelProvider, "ModelProvider", n)
	d := Disposer(func() {
		slog.Info("core.registry.dispose", "component", "ModelProvider")
		replace(r, &r.modelProvider, "ModelProvider", prev)
	})
	r.mu.Lock()
	r.disposers = append(r.disposers, d)
	r.mu.Unlock()
	return prev, d
}

// DisposeAll calls all stored disposers in reverse registration order (LIFO)
// and clears the internal slice. It is safe to call multiple times;
// subsequent calls are no-ops.
//
// The disposers slice is copied and the lock is released before invoking
// disposers, because each disposer calls replace() which acquires the same
// mutex — Go's sync.Mutex is not reentrant.
func (r *DefaultRegistry) DisposeAll() {
	r.mu.Lock()
	disposers := r.disposers
	r.disposers = nil
	r.mu.Unlock()
	for i := len(disposers) - 1; i >= 0; i-- {
		disposers[i]()
	}
}
