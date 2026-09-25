package tools_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// recordingToolDef is a mock ToolDefinition that records whether Execute was
// invoked and returns a deterministic result derived from the call name.
type recordingToolDef struct {
	name     string
	desc     string
	executed bool
}

func (d *recordingToolDef) Name() string        { return d.name }
func (d *recordingToolDef) Description() string { return d.desc }
func (d *recordingToolDef) Execute(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	d.executed = true
	return &tools.ToolResult{Output: "executed:" + call.Name, ToolCallID: call.ID}, nil
}

// newRegistryWithTool builds a DefaultToolRegistry containing a single tool.
func newRegistryWithTool(t *testing.T, def tools.ToolDefinition) tools.ToolRegistry {
	t.Helper()
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), def))
	return tr
}

// TestMiddlewareToolRegistryRejectsOpaqueApprovalWrapper verifies that an
// unreviewed legacy wrapper cannot enter the protected registry dispatch path.
func TestMiddlewareToolRegistryRejectsOpaqueApprovalWrapper(t *testing.T) {
	inner := &recordingToolDef{name: "read", desc: "read tool"}
	tr := newRegistryWithTool(t, inner)

	mw := approval.NewApprovalMiddleware(
		&approval.DenyAllClassifier{},
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(context.Background(), "read")
	require.Nil(t, def)
	require.ErrorIs(t, err, tools.ErrPreparedOpaqueWrapperUnsupported)
	assert.False(t, inner.executed, "denied tool must not invoke the inner Execute")
}

// TestMiddlewareToolRegistryRejectsOpaqueSafetyWrapper verifies that an
// opaque safety wrapper cannot create a legacy prepared-dispatch bypass.
func TestMiddlewareToolRegistryRejectsOpaqueSafetyWrapper(t *testing.T) {
	inner := &recordingToolDef{name: "read", desc: "read tool"}
	tr := newRegistryWithTool(t, inner)

	mw := approval.NewApprovalMiddleware(
		approval.NewSafetyPolicyClassifier([]string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(context.Background(), "read")
	require.Nil(t, def)
	require.ErrorIs(t, err, tools.ErrPreparedOpaqueWrapperUnsupported)
	assert.False(t, inner.executed, "opaque wrappers must not create an unprepared execution path")
}

// TestMiddlewareToolRegistryRejectsOpaquePlanWrapper verifies that a legacy
// Plan wrapper is not silently treated as a prepared authorization policy.
func TestMiddlewareToolRegistryRejectsOpaquePlanWrapper(t *testing.T) {
	inner := &recordingToolDef{name: "write", desc: "write tool"}
	tr := newRegistryWithTool(t, inner)

	mw := approval.NewApprovalMiddleware(
		approval.NewPlanClassifier(),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(true),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(context.Background(), "write")
	require.Nil(t, def)
	require.ErrorIs(t, err, tools.ErrPreparedOpaqueWrapperUnsupported)
	assert.False(t, inner.executed, "opaque approval wrappers must not create an unprepared execution path")
}

// TestMiddlewareToolRegistry_PassthroughNoWrappers verifies that a proxy is
// unavailable until both prepared policy and stages are configured.
func TestMiddlewareToolRegistry_PassthroughNoWrappers(t *testing.T) {
	inner := &recordingToolDef{name: "read", desc: "read tool"}
	tr := newRegistryWithTool(t, inner)

	wrapped := tools.NewMiddlewareToolRegistry(tr)

	def, err := wrapped.Get(context.Background(), "read")
	require.Nil(t, def)
	require.ErrorIs(t, err, tools.ErrPreparedToolPolicyUnavailable)
	assert.False(t, inner.executed, "a proxy without a complete prepared policy must not execute")
}

// TestMiddlewareToolRegistry_VersionDelegates verifies that Version() forwards
// to the inner registry's Version(), so the tool-definition cache in the agent
// loop can detect hot-registered tools through the middleware decorator.
func TestMiddlewareToolRegistry_VersionDelegates(t *testing.T) {
	inner := &recordingToolDef{name: "read", desc: "read tool"}
	tr := newRegistryWithTool(t, inner)

	wrapped := tools.NewMiddlewareToolRegistry(tr)

	// Version must reflect the inner registry's version (1 registration so far).
	v0 := wrapped.Version()
	assert.Equal(t, 1, v0, "Version() should forward inner registry's version")

	// Register a second tool through the wrapper (delegates to inner).
	require.NoError(t, wrapped.Register(context.Background(), &recordingToolDef{name: "write", desc: "write tool"}))

	v1 := wrapped.Version()
	assert.Equal(t, 2, v1, "Version() should increment after Register")

	// The type assertion used by the loop's cache must succeed.
	cacher, ok := interface{}(wrapped).(interface{ Version() int })
	require.True(t, ok, "MiddlewareToolRegistry must satisfy interface{ Version() int }")
	assert.Equal(t, v1, cacher.Version())
}

// TestMiddlewareToolRegistry_HotRegistrationVisible verifies that a tool
// registered at runtime through the wrapper is immediately visible via Get
// and List, and that Version() reflects the change.
func TestMiddlewareToolRegistry_HotRegistrationVisible(t *testing.T) {
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), &recordingToolDef{name: "read", desc: "read tool"}))

	wrapped := tools.NewMiddlewareToolRegistry(tr)
	requirePreparedAllowPolicy(t, wrapped)
	vBefore := wrapped.Version()

	// Hot-register a new tool through the wrapper.
	require.NoError(t, wrapped.Register(context.Background(), &recordingToolDef{name: "write", desc: "write tool"}))

	// Version must have incremented.
	assert.Greater(t, wrapped.Version(), vBefore, "Version() must increase after hot-registration")

	// The new tool must be visible through the wrapper.
	def, err := wrapped.Get(context.Background(), "write")
	require.NoError(t, err)
	require.NotNil(t, def)
	assert.Equal(t, "write", def.Name())

	// And appear in List.
	list, err := wrapped.List(context.Background())
	require.NoError(t, err)
	var names []string
	for _, d := range list {
		names = append(names, d.Name())
	}
	assert.Contains(t, names, "write")
	assert.Contains(t, names, "read")
}

func requirePreparedAllowPolicy(t *testing.T, registry *tools.MiddlewareToolRegistry) {
	t.Helper()
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
}
