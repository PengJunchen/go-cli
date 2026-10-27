package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManagedDelegationProofRequiresClosedOwnedRegistrationAndExactCall(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()
	for _, owned := range []bool{false, true} {
		base := NewDefaultToolRegistry().(*DefaultToolRegistry)
		registry := NewMiddlewareToolRegistry(base)
		configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
		var definition ToolDefinition = NewSubagentTool(&fakeToolDispatcher{})
		if owned {
			definition = NewOwnedSubagentTool(&fakeToolDispatcher{})
		}
		require.NoError(t, registry.RegisterTrustedInlineControl(ctx, definition))
		invocation, err := registry.PrepareToolCall(ctx, ToolCall{ID: "exact-call", Name: "dispatch_subagent", Args: map[string]any{"prompt": "task"}})
		require.NoError(t, err)
		proof, ok := ManagedDelegationProofFor(invocation, root)
		require.Equal(t, owned, ok, "the generic same-name dispatcher must not get a delegation proof")
		if !owned {
			require.Nil(t, proof)
			continue
		}
		require.True(t, proof.MatchesAction(invocation.ToolIdentity(), invocation.EffectSet(), invocation.SchemaDigest(), invocation.ArgumentDigest(), "exact-call", root))
		require.False(t, proof.MatchesAction(invocation.ToolIdentity(), invocation.EffectSet(), invocation.SchemaDigest(), [32]byte{}, "exact-call", root))
		require.False(t, proof.MatchesWorkspaceAction(invocation.ToolIdentity().Snapshot(), invocation.EffectSet().Values(), "different-call", root))
		_, issuedAgain := ManagedDelegationProofFor(invocation, root)
		require.False(t, issuedAgain)
		require.True(t, proof.ClaimWorkspaceAdmission(invocation.ToolIdentity().Snapshot(), invocation.EffectSet().Values(), "exact-call", root))
		require.False(t, proof.ClaimWorkspaceAdmission(invocation.ToolIdentity().Snapshot(), invocation.EffectSet().Values(), "exact-call", root))
	}
}
