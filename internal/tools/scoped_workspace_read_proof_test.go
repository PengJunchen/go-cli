package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type scopedReadImpostor struct{}

func canonicalScopedReadTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return root
}

func (*scopedReadImpostor) Name() string        { return "read" }
func (*scopedReadImpostor) Description() string { return "same-name test tool" }
func (*scopedReadImpostor) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required":             []any{"path"},
		"additionalProperties": false,
	}
}
func (*scopedReadImpostor) Execute(context.Context, ToolCall) (*ToolResult, error) {
	return &ToolResult{Output: "impostor"}, nil
}

func newScopedReadProofRegistry(t *testing.T, root string, options ...RegisterDefaultsOption) (*DefaultToolRegistry, *MiddlewareToolRegistry) {
	t.Helper()
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	registry := NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolStages(PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	registerOptions := append([]RegisterDefaultsOption{WithRegisteredCanonicalWorkspaceRoot(root)}, options...)
	require.NoError(t, RegisterDefaults(ctx, registry, registerOptions...))
	return base, registry
}

func prepareScopedReadProofInvocation(t *testing.T, registry *MiddlewareToolRegistry, name, callID, path string) PreparedInvocation {
	t.Helper()
	invocation, err := registry.PrepareToolCall(context.Background(), ToolCall{
		ID: callID, Name: name, Args: map[string]any{"path": path},
	})
	require.NoError(t, err)
	return invocation
}

func TestScopedWorkspaceReadProofIsOpaqueAndBoundToPreparedInvocation(t *testing.T) {
	for _, name := range []string{"read", "ls"} {
		t.Run(name, func(t *testing.T) {
			root := canonicalScopedReadTestRoot(t)
			_, registry := newScopedReadProofRegistry(t, root)
			invocation := prepareScopedReadProofInvocation(t, registry, name, "call-one", "README.md")
			proof, ok := ScopedWorkspaceReadProofFor(invocation, root)
			require.True(t, ok, "attested built-in reader should receive a scoped proof")
			require.True(t, proof.MatchesAction(
				invocation.ToolIdentity(), invocation.EffectSet(), invocation.SchemaDigest(),
				invocation.ArgumentDigest(), invocation.NormalizedCall().ID, root,
			))

			encoded, err := json.Marshal(proof)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), root, "proof JSON must not reveal its workspace path")
			require.NotContains(t, string(encoded), "workspace_read", "proof JSON must not serialize authorization facts")

			changedArgs := prepareScopedReadProofInvocation(t, registry, name, "call-one", "different.txt")
			require.False(t, proof.MatchesAction(
				changedArgs.ToolIdentity(), changedArgs.EffectSet(), changedArgs.SchemaDigest(),
				changedArgs.ArgumentDigest(), changedArgs.NormalizedCall().ID, root,
			), "a proof from another normalized argument digest must not match")
			otherCall := prepareScopedReadProofInvocation(t, registry, name, "call-two", "README.md")
			require.False(t, proof.MatchesAction(
				otherCall.ToolIdentity(), otherCall.EffectSet(), otherCall.SchemaDigest(),
				otherCall.ArgumentDigest(), otherCall.NormalizedCall().ID, root,
			), "a proof from another tool-call ID must not match")
			require.False(t, proof.MatchesAction(
				invocation.ToolIdentity(), invocation.EffectSet(), invocation.SchemaDigest(),
				invocation.ArgumentDigest(), invocation.NormalizedCall().ID, canonicalScopedReadTestRoot(t),
			), "a proof must remain bound to its canonical workspace root")

			_, mintedAgain := ScopedWorkspaceReadProofFor(invocation, root)
			require.False(t, mintedAgain, "one prepared invocation must not mint a second proof")
		})
	}
}

func TestScopedWorkspaceReadProofRejectsUntrustedOrUnscopedRegistration(t *testing.T) {
	t.Run("same-name custom reader", func(t *testing.T) {
		ctx := context.Background()
		root := canonicalScopedReadTestRoot(t)
		base := NewDefaultToolRegistry().(*DefaultToolRegistry)
		registry := NewMiddlewareToolRegistry(base)
		require.NoError(t, registry.SetPreparedToolStages(PreparedToolStages{}))
		require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
			return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
		}))
		require.NoError(t, base.Register(ctx, &scopedReadImpostor{}))
		invocation := prepareScopedReadProofInvocation(t, registry, "read", "custom-call", "README.md")
		_, ok := ScopedWorkspaceReadProofFor(invocation, root)
		require.False(t, ok, "a public same-name registration must not mint a scoped proof")
	})

	t.Run("unrestricted builtin reader", func(t *testing.T) {
		ctx := context.Background()
		root := canonicalScopedReadTestRoot(t)
		base := NewDefaultToolRegistry().(*DefaultToolRegistry)
		registry := NewMiddlewareToolRegistry(base)
		require.NoError(t, registry.SetPreparedToolStages(PreparedToolStages{}))
		require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
			return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
		}))
		tool := NewReadTool()
		tool.canonicalWorkspaceRoot = root
		require.NoError(t, base.registerTrustedScopedWorkspaceRead(ctx, tool))
		invocation := prepareScopedReadProofInvocation(t, registry, "read", "unrestricted-call", "README.md")
		_, ok := ScopedWorkspaceReadProofFor(invocation, root)
		require.False(t, ok, "an unrestricted whitelist must fail closed for quarantine admission")
	})

	t.Run("configured path outside workspace", func(t *testing.T) {
		root := canonicalScopedReadTestRoot(t)
		outside := canonicalScopedReadTestRoot(t)
		base, registry := newScopedReadProofRegistry(t, root, WithRegisteredPathWhitelist([]string{outside}))
		require.Equal(t, []string{outside}, base.tools["read"].(*ReadTool).whitelist.paths, "configured executor scope must stay authoritative")
		invocation := prepareScopedReadProofInvocation(t, registry, "read", "outside-call", "README.md")
		_, ok := ScopedWorkspaceReadProofFor(invocation, root)
		require.False(t, ok, "a configured whitelist outside the captured workspace must not mint a proof")
	})

	t.Run("invalid whitelist", func(t *testing.T) {
		ctx := context.Background()
		root := canonicalScopedReadTestRoot(t)
		base := NewDefaultToolRegistry().(*DefaultToolRegistry)
		registry := NewMiddlewareToolRegistry(base)
		require.NoError(t, registry.SetPreparedToolStages(PreparedToolStages{}))
		require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
			return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
		}))
		tool := NewReadTool()
		tool.canonicalWorkspaceRoot = root
		tool.whitelist = PathWhitelist{invalid: true}
		require.NoError(t, base.registerTrustedScopedWorkspaceRead(ctx, tool))
		invocation := prepareScopedReadProofInvocation(t, registry, "read", "invalid-call", "README.md")
		_, ok := ScopedWorkspaceReadProofFor(invocation, root)
		require.False(t, ok, "an invalid whitelist must not mint a proof")
	})

	t.Run("unreviewed execution wrapper", func(t *testing.T) {
		root := canonicalScopedReadTestRoot(t)
		base, registry := newScopedReadProofRegistry(t, root)
		_ = base
		wrapper := ToolExecutorWrapper(func(next func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
			return func(ctx context.Context, call ToolCall) (*ToolResult, error) { return next(ctx, call) }
		})
		require.NoError(t, registry.SetPreparedToolStages(PreparedToolStages{ExecutionWrappers: []ToolExecutorWrapper{wrapper}}))
		invocation := prepareScopedReadProofInvocation(t, registry, "read", "wrapped-call", "README.md")
		_, ok := ScopedWorkspaceReadProofFor(invocation, root)
		require.False(t, ok, "an unreviewed wrapper must disable the completion proof needed for admission")
	})
}

func TestDurableCanonicalRootNarrowsOnlyDefaultBuiltinReadAndLS(t *testing.T) {
	root := canonicalScopedReadTestRoot(t)
	base, registry := newScopedReadProofRegistry(t, root)
	for _, name := range []string{"read", "ls"} {
		var whitelist PathWhitelist
		switch tool := base.tools[name].(type) {
		case *ReadTool:
			whitelist = tool.whitelist
		case *LSTool:
			whitelist = tool.whitelist
		default:
			t.Fatalf("registered %s tool has type %T", name, base.tools[name])
		}
		require.False(t, whitelist.invalid)
		require.Equal(t, []string{root}, whitelist.paths, "%s must enforce the durable captured workspace root", name)
	}
	for _, name := range []string{"grep", "find"} {
		var whitelist PathWhitelist
		switch tool := base.tools[name].(type) {
		case *GrepTool:
			whitelist = tool.whitelist
		case *FindTool:
			whitelist = tool.whitelist
		default:
			t.Fatalf("registered %s tool has type %T", name, base.tools[name])
		}
		require.Empty(t, whitelist.paths, "%s keeps its existing default behavior", name)
	}
	read := prepareScopedReadProofInvocation(t, registry, "read", "configured-inside-call", filepath.Join(root, "README.md"))
	_, ok := ScopedWorkspaceReadProofFor(read, root)
	require.True(t, ok, "only an eligible builtin read under the durable root receives proof")
}

func TestRegisterDefaultsWithoutDurableRootKeepsUnrestrictedReadBehavior(t *testing.T) {
	ctx := context.Background()
	root := canonicalScopedReadTestRoot(t)
	outsidePath := filepath.Join(root, "outside-durable-workspace.txt")
	require.NoError(t, os.WriteFile(outsidePath, []byte("ordinary default read\n"), 0o600))
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	require.NoError(t, RegisterDefaults(ctx, base))
	reader := base.tools["read"].(*ReadTool)
	require.Empty(t, reader.whitelist.paths, "without a durable root or sandbox option the existing empty whitelist remains unrestricted")
	result, err := reader.Execute(ctx, ToolCall{Args: map[string]any{"path": outsidePath}})
	require.NoError(t, err)
	require.Equal(t, "ordinary default read\n", result.Output)
}

func TestScopedWorkspaceReadProofRechecksCurrentGeneration(t *testing.T) {
	root := canonicalScopedReadTestRoot(t)
	_, registry := newScopedReadProofRegistry(t, root)
	invocation := prepareScopedReadProofInvocation(t, registry, "read", "stale-call", "README.md")
	proof, ok := ScopedWorkspaceReadProofFor(invocation, root)
	require.True(t, ok)

	require.NoError(t, registry.SetPreparedToolStages(PreparedToolStages{}))
	require.False(t, proof.MatchesAction(
		invocation.ToolIdentity(), invocation.EffectSet(), invocation.SchemaDigest(),
		invocation.ArgumentDigest(), invocation.NormalizedCall().ID, root,
	), "a proof must become unusable when a captured generation changes")
	_, ok = ScopedWorkspaceReadProofFor(invocation, root)
	require.False(t, ok, "a stale invocation must not mint a replacement proof")

	encoded, err := json.Marshal(proof)
	require.NoError(t, err)
	for _, secret := range []string{root, "stale-call", invocation.ToolIdentity().ToolName()} {
		require.False(t, strings.Contains(string(encoded), secret), "proof JSON leaked %q: %s", secret, encoded)
	}
}
