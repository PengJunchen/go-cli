package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// testLoop is a minimal AgentLoop used to verify registry replacement.
type testLoop struct{ tag string }

func (t testLoop) Run(_ context.Context, _ Submission) ([]AgentEvent, error) {
	return []AgentEvent{}, nil
}

func TestNewRegistryAllDefaultsBound(t *testing.T) {
	r := NewRegistry()
	assert.NotNil(t, r.AgentLoop())
	assert.NotNil(t, r.Agent())
	assert.NotNil(t, r.Harness())
	assert.NotNil(t, r.TurnRunner())
	assert.NotNil(t, r.SessionStore())
	assert.NotNil(t, r.SessionTree())
	assert.NotNil(t, r.ContextManager())
	assert.NotNil(t, r.Compactor())
	assert.NotNil(t, r.TokenEstimator())
	assert.NotNil(t, r.ToolRegistry())
	assert.NotNil(t, r.ModelProvider())
	assert.NotNil(t, r.ApprovalClassifier())
	assert.NotNil(t, r.ApprovalStore())
	assert.NotNil(t, r.TraceExporter())
	assert.NotNil(t, r.ConfigProvider())
	assert.NotNil(t, r.PluginLoader())
}

func TestRegisterAgentLoopReturnsPreviousImpl(t *testing.T) {
	r := NewRegistry()
	prev := r.AgentLoop()
	assert.NotNil(t, prev)

	gotPrev := r.RegisterAgentLoop(&testLoop{tag: "a"})
	assert.Equal(t, prev, gotPrev)

	// Getter reflects the new impl.
	got, ok := r.AgentLoop().(*testLoop)
	assert.True(t, ok)
	assert.Equal(t, "a", got.tag)
}

func TestSecondRegisterReturnsPrevious(t *testing.T) {
	r := NewRegistry()
	first := &testLoop{tag: "first"}
	r.RegisterAgentLoop(first)
	second := &testLoop{tag: "second"}
	got := r.RegisterAgentLoop(second)
	assert.Equal(t, first, got)
	cur, ok := r.AgentLoop().(*testLoop)
	assert.True(t, ok)
	assert.Equal(t, "second", cur.tag)
}

func TestRegisterNilPanics(t *testing.T) {
	r := NewRegistry()
	assert.PanicsWithValue(t, "registry: nil AgentLoop", func() {
		r.RegisterAgentLoop(nil)
	})

	// After the panic the original impl is still in place.
	assert.NotNil(t, r.AgentLoop())
}

func TestRegisterComponentReturnsPrevious(t *testing.T) {
	r := NewRegistry()

	oldStore := r.SessionStore()
	got := r.RegisterSessionStore(&SessionStoreImpl{})
	assert.Equal(t, oldStore, got)

	oldTools := r.ToolRegistry()
	gotTools := r.RegisterToolRegistry(&DefaultToolRegistry{})
	assert.Equal(t, oldTools, gotTools)

	oldModel := r.ModelProvider()
	gotModel := r.RegisterModelProvider(&DefaultModelProvider{})
	assert.Equal(t, oldModel, gotModel)
}

func TestRegistryConcurrencySafety(t *testing.T) {
	r := NewRegistry()

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.AgentLoop()
			r.SessionStore()
			r.ToolRegistry()
			r.ModelProvider()
			r.RegisterAgentLoop(&testLoop{})
			r.RegisterCompactor(&UnifiedCompactor{})
		}()
	}
	wg.Wait()

	assert.NotNil(t, r.AgentLoop())
}

// TestInterfaceStubAssertions ensures the compile-time interface guards are
// present. It fails to compile if a stub no longer satisfies its interface.
func TestInterfaceStubAssertions(t *testing.T) {
	var _ AgentLoop = (*LoopAgent)(nil)
	var _ Agent = (*AgentImpl)(nil)
	var _ Harness = (*HarnessImpl)(nil)
	var _ TurnRunner = (*EinoTurnRunner)(nil)
	var _ EventStream = (*EventStreamImpl)(nil)
	var _ SessionStore = (*SessionStoreImpl)(nil)
	var _ SessionTree = (*SessionTreeImpl)(nil)
	var _ ContextManager = (*ContextManagerImpl)(nil)
	var _ Compactor = (*UnifiedCompactor)(nil)
	var _ TokenEstimator = (*HeuristicTokenEstimator)(nil)
	var _ ApprovalClassifier = (*SafetyPolicyClassifier)(nil)
	var _ ApprovalStore = (*InMemoryApprovalStore)(nil)
	var _ PluginLoader = (*PluginLoaderImpl)(nil)
	var _ Extension = (*ExtensionImpl)(nil)
	var _ ExtensionRegistry = (*ExtensionRegistryImpl)(nil)
	var _ Hook = (*HookImpl)(nil)
	var _ Middleware = (*MiddlewareImpl)(nil)
	t.Log("all interface stub assertions present")
}
