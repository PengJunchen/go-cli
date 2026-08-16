package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRegistry_InterfaceSegregation verifies that *DefaultRegistry is
// assignable to every sub-interface produced by the ISP split. If a method is
// accidentally removed or renamed, the corresponding assignment fails to
// compile.
func TestRegistry_InterfaceSegregation(t *testing.T) {
	dr := NewRegistry().(*DefaultRegistry)

	// *DefaultRegistry must satisfy each sub-interface individually.
	var _ CoreRegistry = dr
	var _ SessionRegistry = dr
	var _ CompactorRegistry = dr
	var _ ToolRegistryAccessor = dr
	var _ ModelProviderRegistry = dr
	var _ ApprovalRegistry = dr
	var _ TracingRegistry = dr
	var _ PluginRegistry = dr

	// The Registry return value (interface) must also be assignable to each
	// sub-interface, proving Registry embeds them all.
	r := NewRegistry()
	var _ CoreRegistry = r
	var _ SessionRegistry = r
	var _ CompactorRegistry = r
	var _ ToolRegistryAccessor = r
	var _ ModelProviderRegistry = r
	var _ ApprovalRegistry = r
	var _ TracingRegistry = r
	var _ PluginRegistry = r

	t.Log("DefaultRegistry and Registry satisfy all sub-interfaces")
}

// TestRegistry_TopLevelInterfaceCompatible verifies that the top-level
// Registry interface embeds all sub-interfaces and remains backward
// compatible — all 16 getters are still directly accessible on the composite
// interface.
func TestRegistry_TopLevelInterfaceCompatible(t *testing.T) {
	r := NewRegistry()

	// Registry embeds every sub-interface, so a Registry value can be passed
	// wherever a narrower sub-interface is expected.
	assertCoreRegistry(t, r)
	assertSessionRegistry(t, r)
	assertCompactorRegistry(t, r)
	assertToolRegistryAccessor(t, r)
	assertModelProviderRegistry(t, r)
	assertApprovalRegistry(t, r)
	assertTracingRegistry(t, r)
	assertPluginRegistry(t, r)

	// All 16 getters remain directly accessible on the composite interface.
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

// TestRegistry_SubInterfaceUsage demonstrates that a consumer depending on a
// narrow sub-interface can operate on a *DefaultRegistry without needing the
// full Registry. This is the core benefit of interface segregation.
func TestRegistry_SubInterfaceUsage(t *testing.T) {
	dr := NewRegistry().(*DefaultRegistry)

	// A consumer that only needs model-provider access.
	assert.True(t, hasModelProvider(dr))

	// A consumer that only needs tool-registry access.
	assert.True(t, hasToolRegistry(dr))

	// A consumer that only needs session-store access.
	assert.True(t, hasSessionStore(dr))
}

// --- narrow consumers accepting sub-interfaces ---

func assertCoreRegistry(t *testing.T, r CoreRegistry) {
	t.Helper()
	assert.NotNil(t, r.AgentLoop())
	assert.NotNil(t, r.Agent())
	assert.NotNil(t, r.Harness())
	assert.NotNil(t, r.TurnRunner())
}

func assertSessionRegistry(t *testing.T, r SessionRegistry) {
	t.Helper()
	assert.NotNil(t, r.SessionStore())
	assert.NotNil(t, r.SessionTree())
	assert.NotNil(t, r.ContextManager())
}

func assertCompactorRegistry(t *testing.T, r CompactorRegistry) {
	t.Helper()
	assert.NotNil(t, r.Compactor())
	assert.NotNil(t, r.TokenEstimator())
}

func assertToolRegistryAccessor(t *testing.T, r ToolRegistryAccessor) {
	t.Helper()
	assert.NotNil(t, r.ToolRegistry())
}

func assertModelProviderRegistry(t *testing.T, r ModelProviderRegistry) {
	t.Helper()
	assert.NotNil(t, r.ModelProvider())
}

func assertApprovalRegistry(t *testing.T, r ApprovalRegistry) {
	t.Helper()
	assert.NotNil(t, r.ApprovalClassifier())
	assert.NotNil(t, r.ApprovalStore())
}

func assertTracingRegistry(t *testing.T, r TracingRegistry) {
	t.Helper()
	assert.NotNil(t, r.TraceExporter())
}

func assertPluginRegistry(t *testing.T, r PluginRegistry) {
	t.Helper()
	assert.NotNil(t, r.ConfigProvider())
	assert.NotNil(t, r.PluginLoader())
}

func hasModelProvider(r ModelProviderRegistry) bool {
	return r.ModelProvider() != nil
}

func hasToolRegistry(r ToolRegistryAccessor) bool {
	return r.ToolRegistry() != nil
}

func hasSessionStore(r SessionRegistry) bool {
	return r.SessionStore() != nil
}
