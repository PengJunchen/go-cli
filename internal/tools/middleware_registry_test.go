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

// TestMiddlewareToolRegistry_BlocksDeniedTool verifies that a deny classifier
// short-circuits execution: the inner tool's Execute is never called and the
// middleware surfaces ErrToolDenied.
func TestMiddlewareToolRegistry_BlocksDeniedTool(t *testing.T) {
	inner := &recordingToolDef{name: "read", desc: "read tool"}
	tr := newRegistryWithTool(t, inner)

	mw := approval.NewApprovalMiddleware(
		&approval.DenyAllClassifier{},
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(context.Background(), "read")
	require.NoError(t, err)
	require.NotNil(t, def)

	res, execErr := def.Execute(context.Background(), tools.ToolCall{ID: "1", Name: "read"})
	require.Error(t, execErr)
	assert.ErrorIs(t, execErr, approval.ErrToolDenied)
	assert.Nil(t, res)
	assert.False(t, inner.executed, "denied tool must not invoke the inner Execute")
}

// TestMiddlewareToolRegistry_AllowsSafeTool verifies that a SafetyPolicyClassifier
// which only denies "bash" lets a "read" tool execute normally through the wrapper.
func TestMiddlewareToolRegistry_AllowsSafeTool(t *testing.T) {
	inner := &recordingToolDef{name: "read", desc: "read tool"}
	tr := newRegistryWithTool(t, inner)

	mw := approval.NewApprovalMiddleware(
		approval.NewSafetyPolicyClassifier([]string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(context.Background(), "read")
	require.NoError(t, err)
	require.NotNil(t, def)

	res, execErr := def.Execute(context.Background(), tools.ToolCall{ID: "1", Name: "read"})
	require.NoError(t, execErr)
	require.NotNil(t, res)
	assert.Equal(t, "executed:read", res.Output)
	assert.True(t, inner.executed, "safe tool must invoke the inner Execute")
}

// TestMiddlewareToolRegistry_AutoApprove verifies that an Ask classification
// (from PlanClassifier) is resolved to Allow when auto-approve is enabled,
// allowing the call to reach the inner tool.
func TestMiddlewareToolRegistry_AutoApprove(t *testing.T) {
	inner := &recordingToolDef{name: "write", desc: "write tool"}
	tr := newRegistryWithTool(t, inner)

	mw := approval.NewApprovalMiddleware(
		approval.NewPlanClassifier(),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(true),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(context.Background(), "write")
	require.NoError(t, err)
	require.NotNil(t, def)

	res, execErr := def.Execute(context.Background(), tools.ToolCall{ID: "1", Name: "write"})
	require.NoError(t, execErr)
	require.NotNil(t, res)
	assert.Equal(t, "executed:write", res.Output)
	assert.True(t, inner.executed, "auto-approved Ask must invoke the inner Execute")
}

// TestMiddlewareToolRegistry_PassthroughNoWrappers verifies that when no
// wrappers are configured, Get returns the original ToolDefinition instance
// unchanged (no wrapping adapter is applied).
func TestMiddlewareToolRegistry_PassthroughNoWrappers(t *testing.T) {
	inner := &recordingToolDef{name: "read", desc: "read tool"}
	tr := newRegistryWithTool(t, inner)

	wrapped := tools.NewMiddlewareToolRegistry(tr)

	def, err := wrapped.Get(context.Background(), "read")
	require.NoError(t, err)
	require.NotNil(t, def)

	// With no wrappers, Get must return the exact original instance, not a
	// wrapping adapter.
	got, ok := def.(*recordingToolDef)
	require.True(t, ok, "passthrough should return the original ToolDefinition type")
	assert.Same(t, inner, got, "no wrappers should return the original ToolDefinition instance")

	res, execErr := def.Execute(context.Background(), tools.ToolCall{ID: "1", Name: "read"})
	require.NoError(t, execErr)
	require.NotNil(t, res)
	assert.Equal(t, "executed:read", res.Output)
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
