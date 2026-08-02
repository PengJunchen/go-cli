package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRegistryEveryComponentDefaultsBound(t *testing.T) {
	r := NewRegistry()
	// Each getter must be non-nil, proving no field is left as a nil interface.
	comps := []any{
		r.AgentLoop(), r.Agent(), r.Harness(), r.TurnRunner(),
		r.SessionStore(), r.SessionTree(), r.ContextManager(), r.Compactor(),
		r.TokenEstimator(), r.ToolRegistry(), r.ModelProvider(),
		r.ApprovalClassifier(), r.ApprovalStore(), r.TraceExporter(),
		r.ConfigProvider(), r.PluginLoader(),
	}
	for i, c := range comps {
		assert.NotNilf(t, c, "component %d is nil", i)
	}
}

func TestRegistryDefaultConcreteTypes(t *testing.T) {
	r := NewRegistry()
	assert.IsType(t, &LoopAgent{}, r.AgentLoop())
	assert.IsType(t, &AgentImpl{}, r.Agent())
	assert.IsType(t, &HarnessImpl{}, r.Harness())
	assert.IsType(t, &EinoTurnRunner{}, r.TurnRunner())
	assert.IsType(t, &SessionStoreImpl{}, r.SessionStore())
	assert.IsType(t, &SessionTreeImpl{}, r.SessionTree())
	assert.IsType(t, &ContextManagerImpl{}, r.ContextManager())
	assert.IsType(t, &UnifiedCompactor{}, r.Compactor())
	assert.IsType(t, &HeuristicTokenEstimator{}, r.TokenEstimator())
	assert.IsType(t, &DefaultToolRegistry{}, r.ToolRegistry())
	assert.IsType(t, &DefaultModelProvider{}, r.ModelProvider())
	assert.IsType(t, &SafetyPolicyClassifier{}, r.ApprovalClassifier())
	assert.IsType(t, &InMemoryApprovalStore{}, r.ApprovalStore())
	assert.IsType(t, &NoopTraceExporter{}, r.TraceExporter())
	assert.IsType(t, &DefaultConfigProvider{}, r.ConfigProvider())
	assert.IsType(t, &PluginLoaderImpl{}, r.PluginLoader())
}

// stub implementations for the service-layer interfaces.
type stubSessionStore struct{}

func (stubSessionStore) Save(context.Context, Session) error           { return nil }
func (stubSessionStore) Load(context.Context, string) (Session, error) { return Session{}, nil }
func (stubSessionStore) List(context.Context) ([]Session, error)       { return nil, nil }

type stubSessionTree struct{}

func (stubSessionTree) Create(context.Context, string) error         { return nil }
func (stubSessionTree) Branch(context.Context, string, string) error { return nil }
func (stubSessionTree) Leaves(context.Context, string) ([]string, error) {
	return nil, nil
}

type stubContextManager struct{}

func (stubContextManager) Build(context.Context, string) ([]AgentMessage, error) { return nil, nil }

type stubCompactor struct{}

func (stubCompactor) Compact(context.Context, []AgentMessage, int) ([]AgentMessage, error) {
	return nil, nil
}

type stubTokenEstimator struct{}

func (stubTokenEstimator) Estimate(string) int                 { return 0 }
func (stubTokenEstimator) EstimateMessages([]AgentMessage) int { return 0 }

// TestRegistryReplaceEveryComponent replaces each subsystem with a fresh
// default and confirms the previous value is returned and the getter reflects
// the change.
func TestRegistryReplaceEveryComponent(t *testing.T) {
	r := NewRegistry()

	prevC := r.RegisterCompactor(&UnifiedCompactor{})
	assert.NotNil(t, prevC)
	assert.IsType(t, &UnifiedCompactor{}, r.Compactor())

	prevT := r.RegisterTokenEstimator(&HeuristicTokenEstimator{})
	assert.NotNil(t, prevT)
	assert.IsType(t, &HeuristicTokenEstimator{}, r.TokenEstimator())

	prevCM := r.RegisterContextManager(&ContextManagerImpl{})
	assert.NotNil(t, prevCM)
	assert.IsType(t, &ContextManagerImpl{}, r.ContextManager())

	prevST := r.RegisterSessionTree(&SessionTreeImpl{})
	assert.NotNil(t, prevST)
	assert.IsType(t, &SessionTreeImpl{}, r.SessionTree())

	prevAS := r.RegisterApprovalStore(&InMemoryApprovalStore{})
	assert.NotNil(t, prevAS)
	assert.IsType(t, &InMemoryApprovalStore{}, r.ApprovalStore())

	prevTE := r.RegisterTraceExporter(&NoopTraceExporter{})
	assert.NotNil(t, prevTE)
	assert.IsType(t, &NoopTraceExporter{}, r.TraceExporter())

	prevCfg := r.RegisterConfigProvider(&DefaultConfigProvider{})
	assert.NotNil(t, prevCfg)
	assert.IsType(t, &DefaultConfigProvider{}, r.ConfigProvider())

	prevPlug := r.RegisterPluginLoader(&PluginLoaderImpl{})
	assert.NotNil(t, prevPlug)
	assert.IsType(t, &PluginLoaderImpl{}, r.PluginLoader())
}

func TestRegistryReplaceWithStubTypes(t *testing.T) {
	r := NewRegistry()

	r.RegisterSessionStore(stubSessionStore{})
	assert.IsType(t, stubSessionStore{}, r.SessionStore())

	r.RegisterSessionTree(stubSessionTree{})
	assert.IsType(t, stubSessionTree{}, r.SessionTree())

	r.RegisterContextManager(stubContextManager{})
	assert.IsType(t, stubContextManager{}, r.ContextManager())

	r.RegisterCompactor(stubCompactor{})
	assert.IsType(t, stubCompactor{}, r.Compactor())

	r.RegisterTokenEstimator(stubTokenEstimator{})
	assert.IsType(t, stubTokenEstimator{}, r.TokenEstimator())
}

func TestRegistryNilReplacementPanicsTable(t *testing.T) {
	r := NewRegistry()

	panicCases := []struct {
		name string
		reg  func()
	}{
		{"AgentLoop", func() { r.RegisterAgentLoop(nil) }},
		{"Agent", func() { r.RegisterAgent(nil) }},
		{"Harness", func() { r.RegisterHarness(nil) }},
		{"TurnRunner", func() { r.RegisterTurnRunner(nil) }},
		{"SessionStore", func() { r.RegisterSessionStore(nil) }},
		{"SessionTree", func() { r.RegisterSessionTree(nil) }},
		{"ContextManager", func() { r.RegisterContextManager(nil) }},
		{"Compactor", func() { r.RegisterCompactor(nil) }},
		{"TokenEstimator", func() { r.RegisterTokenEstimator(nil) }},
		{"ToolRegistry", func() { r.RegisterToolRegistry(nil) }},
		{"ModelProvider", func() { r.RegisterModelProvider(nil) }},
		{"ApprovalClassifier", func() { r.RegisterApprovalClassifier(nil) }},
		{"ApprovalStore", func() { r.RegisterApprovalStore(nil) }},
		{"TraceExporter", func() { r.RegisterTraceExporter(nil) }},
		{"ConfigProvider", func() { r.RegisterConfigProvider(nil) }},
		{"PluginLoader", func() { r.RegisterPluginLoader(nil) }},
	}
	for _, tc := range panicCases {
		assert.PanicsWithValuef(t, "registry: nil "+tc.name, tc.reg, "component %s", tc.name)
	}
	// After all panics the registry is still fully bound.
	assert.NotNil(t, r.AgentLoop())
}

func TestRegistryRegisterAndGetRoundTrip(t *testing.T) {
	r := NewRegistry()

	prevAgent := r.RegisterAgent(&fakeEventStreamAgent{})
	assert.NotNil(t, prevAgent)
	assert.IsType(t, &fakeEventStreamAgent{}, r.Agent())

	prevHarness := r.RegisterHarness(&HarnessImpl{})
	assert.NotNil(t, prevHarness)
	assert.IsType(t, &HarnessImpl{}, r.Harness())

	prevTurn := r.RegisterTurnRunner(&EinoTurnRunner{})
	assert.NotNil(t, prevTurn)
	assert.IsType(t, &EinoTurnRunner{}, r.TurnRunner())

	prevTools := r.RegisterToolRegistry(&DefaultToolRegistry{})
	assert.NotNil(t, prevTools)
	assert.IsType(t, &DefaultToolRegistry{}, r.ToolRegistry())

	prevModel := r.RegisterModelProvider(&DefaultModelProvider{})
	assert.NotNil(t, prevModel)
	assert.IsType(t, &DefaultModelProvider{}, r.ModelProvider())

	prevCls := r.RegisterApprovalClassifier(&SafetyPolicyClassifier{})
	assert.NotNil(t, prevCls)
	assert.IsType(t, &SafetyPolicyClassifier{}, r.ApprovalClassifier())
}
