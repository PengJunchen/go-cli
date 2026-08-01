package approval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func ctx() context.Context { return context.Background() }

func call(name string) tools.ToolCall {
	return tools.ToolCall{ID: "1", Name: name, Args: map[string]any{"k": "v"}}
}

func TestAllowAllClassifierAlwaysAllows(t *testing.T) {
	c := &AllowAllClassifier{}
	require.Equal(t, "allow_all", c.Name())
	assert.Equal(t, Allow, c.Classify(ctx(), call("read_file")))
	assert.Equal(t, Allow, c.Classify(ctx(), call("dangerous")))
}

func TestDenyAllClassifierAlwaysDenies(t *testing.T) {
	c := &DenyAllClassifier{}
	require.Equal(t, "deny_all", c.Name())
	assert.Equal(t, Deny, c.Classify(ctx(), call("read_file")))
	assert.Equal(t, Deny, c.Classify(ctx(), call("harmless")))
}

func TestStaticClassifierAllowlist(t *testing.T) {
	c := NewStaticClassifier([]string{"read_file", "edit"}, nil)
	assert.Equal(t, "static", c.Name())
	assert.Equal(t, Allow, c.Classify(ctx(), call("read_file")))
	assert.Equal(t, Allow, c.Classify(ctx(), call("edit")))
}

func TestStaticClassifierDenyWinsOverAllow(t *testing.T) {
	c := NewStaticClassifier([]string{"read_file"}, []string{"read_file"})
	assert.Equal(t, Deny, c.Classify(ctx(), call("read_file")))
}

func TestStaticClassifierDenyByDefault(t *testing.T) {
	c := NewStaticClassifier([]string{"read_file"}, []string{"delete"})
	assert.Equal(t, Allow, c.Classify(ctx(), call("read_file")))
	assert.Equal(t, Deny, c.Classify(ctx(), call("delete")))
	assert.Equal(t, Deny, c.Classify(ctx(), call("unknown_tool")))
}

func TestSafetyPolicyClassifierDeniesForbidden(t *testing.T) {
	c := NewSafetyPolicyClassifier([]string{"delete", "rm_remote"})
	assert.Equal(t, "safety_policy", c.Name())
	assert.Equal(t, Deny, c.Classify(ctx(), call("delete")))
	assert.Equal(t, Deny, c.Classify(ctx(), call("rm_remote")))
	assert.Equal(t, Allow, c.Classify(ctx(), call("read_file")))
	assert.Equal(t, Allow, c.Classify(ctx(), call("search")))
}

func TestClassifiersSatisfyInterface(t *testing.T) {
	var _ ApprovalClassifier = (*AllowAllClassifier)(nil)
	var _ ApprovalClassifier = (*DenyAllClassifier)(nil)
	var _ ApprovalClassifier = (*StaticClassifier)(nil)
	var _ ApprovalClassifier = (*SafetyPolicyClassifier)(nil)
}
