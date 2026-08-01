package core

import (
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// Registry is the interface hub of the runtime. It binds a default
// implementation for every subsystem interface and lets callers replace any
// of them via the RegisterXxx methods. All fields are interface types so the
// runtime always depends on abstractions rather than concrete implementations
// (SCAN-013).
type Registry struct {
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
}

// NewRegistry returns a Registry whose every field is bound to a default stub
// implementation. No field is left as a nil interface.
func NewRegistry() *Registry {
	return &Registry{
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
func replace[T any](r *Registry, field *T, name string, next T) (prev T) {
	if any(next) == nil {
		panic("registry: nil " + name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev = *field
	slog.Info("core.registry.replace", "component", name)
	*field = next
	return prev
}

// get returns the current value of field under a read lock.
func get[T any](r *Registry, field *T) T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return *field
}

// AgentLoop returns the bound AgentLoop implementation.
func (r *Registry) AgentLoop() AgentLoop { return get(r, &r.agentLoop) }

// RegisterAgentLoop replaces the AgentLoop implementation and returns the old
// one. It panics if n is nil.
func (r *Registry) RegisterAgentLoop(n AgentLoop) AgentLoop {
	return replace(r, &r.agentLoop, "AgentLoop", n)
}

// Agent returns the bound Agent implementation.
func (r *Registry) Agent() Agent { return get(r, &r.agent) }

// RegisterAgent replaces the Agent implementation and returns the old one. It
// panics if n is nil.
func (r *Registry) RegisterAgent(n Agent) Agent {
	return replace(r, &r.agent, "Agent", n)
}

// Harness returns the bound Harness implementation.
func (r *Registry) Harness() Harness { return get(r, &r.harness) }

// RegisterHarness replaces the Harness implementation and returns the old one.
// It panics if n is nil.
func (r *Registry) RegisterHarness(n Harness) Harness {
	return replace(r, &r.harness, "Harness", n)
}

// TurnRunner returns the bound TurnRunner implementation.
func (r *Registry) TurnRunner() TurnRunner { return get(r, &r.turnRunner) }

// RegisterTurnRunner replaces the TurnRunner implementation and returns the
// old one. It panics if n is nil.
func (r *Registry) RegisterTurnRunner(n TurnRunner) TurnRunner {
	return replace(r, &r.turnRunner, "TurnRunner", n)
}

// SessionStore returns the bound SessionStore implementation.
func (r *Registry) SessionStore() SessionStore { return get(r, &r.sessionStore) }

// RegisterSessionStore replaces the SessionStore implementation and returns
// the old one. It panics if n is nil.
func (r *Registry) RegisterSessionStore(n SessionStore) SessionStore {
	return replace(r, &r.sessionStore, "SessionStore", n)
}

// SessionTree returns the bound SessionTree implementation.
func (r *Registry) SessionTree() SessionTree { return get(r, &r.sessionTree) }

// RegisterSessionTree replaces the SessionTree implementation and returns the
// old one. It panics if n is nil.
func (r *Registry) RegisterSessionTree(n SessionTree) SessionTree {
	return replace(r, &r.sessionTree, "SessionTree", n)
}

// ContextManager returns the bound ContextManager implementation.
func (r *Registry) ContextManager() ContextManager { return get(r, &r.contextManager) }

// RegisterContextManager replaces the ContextManager implementation and
// returns the old one. It panics if n is nil.
func (r *Registry) RegisterContextManager(n ContextManager) ContextManager {
	return replace(r, &r.contextManager, "ContextManager", n)
}

// Compactor returns the bound Compactor implementation.
func (r *Registry) Compactor() Compactor { return get(r, &r.compactor) }

// RegisterCompactor replaces the Compactor implementation and returns the old
// one. It panics if n is nil.
func (r *Registry) RegisterCompactor(n Compactor) Compactor {
	return replace(r, &r.compactor, "Compactor", n)
}

// TokenEstimator returns the bound TokenEstimator implementation.
func (r *Registry) TokenEstimator() TokenEstimator { return get(r, &r.tokenEstimator) }

// RegisterTokenEstimator replaces the TokenEstimator implementation and
// returns the old one. It panics if n is nil.
func (r *Registry) RegisterTokenEstimator(n TokenEstimator) TokenEstimator {
	return replace(r, &r.tokenEstimator, "TokenEstimator", n)
}

// ToolRegistry returns the bound ToolRegistry implementation.
func (r *Registry) ToolRegistry() tools.ToolRegistry { return get(r, &r.toolRegistry) }

// RegisterToolRegistry replaces the ToolRegistry implementation and returns
// the old one. It panics if n is nil.
func (r *Registry) RegisterToolRegistry(n tools.ToolRegistry) tools.ToolRegistry {
	return replace(r, &r.toolRegistry, "ToolRegistry", n)
}

// ModelProvider returns the bound ModelProvider implementation.
func (r *Registry) ModelProvider() llm.ModelProvider { return get(r, &r.modelProvider) }

// RegisterModelProvider replaces the ModelProvider implementation and returns
// the old one. It panics if n is nil.
func (r *Registry) RegisterModelProvider(n llm.ModelProvider) llm.ModelProvider {
	return replace(r, &r.modelProvider, "ModelProvider", n)
}

// ApprovalClassifier returns the bound ApprovalClassifier implementation.
func (r *Registry) ApprovalClassifier() ApprovalClassifier { return get(r, &r.approvalClassifier) }

// RegisterApprovalClassifier replaces the ApprovalClassifier implementation
// and returns the old one. It panics if n is nil.
func (r *Registry) RegisterApprovalClassifier(n ApprovalClassifier) ApprovalClassifier {
	return replace(r, &r.approvalClassifier, "ApprovalClassifier", n)
}

// ApprovalStore returns the bound ApprovalStore implementation.
func (r *Registry) ApprovalStore() ApprovalStore { return get(r, &r.approvalStore) }

// RegisterApprovalStore replaces the ApprovalStore implementation and returns
// the old one. It panics if n is nil.
func (r *Registry) RegisterApprovalStore(n ApprovalStore) ApprovalStore {
	return replace(r, &r.approvalStore, "ApprovalStore", n)
}

// TraceExporter returns the bound TraceExporter implementation.
func (r *Registry) TraceExporter() tracing.TraceExporter { return get(r, &r.traceExporter) }

// RegisterTraceExporter replaces the TraceExporter implementation and returns
// the old one. It panics if n is nil.
func (r *Registry) RegisterTraceExporter(n tracing.TraceExporter) tracing.TraceExporter {
	return replace(r, &r.traceExporter, "TraceExporter", n)
}

// ConfigProvider returns the bound ConfigProvider implementation.
func (r *Registry) ConfigProvider() extension.ConfigProvider { return get(r, &r.configProvider) }

// RegisterConfigProvider replaces the ConfigProvider implementation and
// returns the old one. It panics if n is nil.
func (r *Registry) RegisterConfigProvider(n extension.ConfigProvider) extension.ConfigProvider {
	return replace(r, &r.configProvider, "ConfigProvider", n)
}

// PluginLoader returns the bound PluginLoader implementation.
func (r *Registry) PluginLoader() PluginLoader { return get(r, &r.pluginLoader) }

// RegisterPluginLoader replaces the PluginLoader implementation and returns
// the old one. It panics if n is nil.
func (r *Registry) RegisterPluginLoader(n PluginLoader) PluginLoader {
	return replace(r, &r.pluginLoader, "PluginLoader", n)
}
