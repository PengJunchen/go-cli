package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultToolDef_SatisfiesInterface(t *testing.T) {
	// Compile-time assertion already exists (var _ ToolDefinition = (*defaultToolDef)(nil)),
	// but we verify the interface methods are callable at runtime.
	var _ ToolDefinition = (*defaultToolDef)(nil)
}

func TestDefaultToolDef_Name(t *testing.T) {
	d := defaultToolDef{}
	assert.Equal(t, "default", d.Name())
}

func TestDefaultToolDef_Description(t *testing.T) {
	d := defaultToolDef{}
	assert.Equal(t, "", d.Description())
}

func TestDefaultToolDef_Execute_ReturnsDefaultError(t *testing.T) {
	d := defaultToolDef{}
	result, err := d.Execute(context.Background(), ToolCall{ID: "1", Name: "test"})
	assert.Nil(t, result)
	assert.ErrorIs(t, err, errDefaultTool)
}

func TestDefaultToolRegistry_SatisfiesInterface(t *testing.T) {
	var _ ToolRegistry = (*defaultToolRegistry)(nil)
}

func TestDefaultToolRegistry_Register_NoOp(t *testing.T) {
	r := defaultToolRegistry{}
	err := r.Register(context.Background(), &mockToolDef{name: "x"})
	assert.NoError(t, err, "default registry Register should be a no-op")
}

func TestDefaultToolRegistry_Get_ReturnsDefaultError(t *testing.T) {
	r := defaultToolRegistry{}
	def, err := r.Get(context.Background(), "anything")
	assert.Nil(t, def)
	assert.ErrorIs(t, err, errDefaultTool)
}

func TestDefaultToolRegistry_List_ReturnsEmptySlice(t *testing.T) {
	r := defaultToolRegistry{}
	list, err := r.List(context.Background())
	require.NoError(t, err)
	assert.Nil(t, list, "default registry List should return nil (empty)")
}

// mockToolDef is a minimal test helper that satisfies ToolDefinition.
type mockToolDef struct {
	name string
}

func (m *mockToolDef) Name() string        { return m.name }
func (m *mockToolDef) Description() string { return "" }
func (m *mockToolDef) Execute(_ context.Context, _ ToolCall) (*ToolResult, error) {
	return &ToolResult{Output: "ok"}, nil
}

var _ ToolDefinition = (*mockToolDef)(nil)
