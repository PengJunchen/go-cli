package tools_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type ordinaryReviewedRegistry struct{ tools.ToolRegistry }

type rejectingReviewedRegistry struct {
	tools.ToolRegistry
	registrationCalls int
}

func (r *rejectingReviewedRegistry) Register(ctx context.Context, definition tools.ToolDefinition) error {
	r.registrationCalls++
	return r.ToolRegistry.Register(ctx, definition)
}
func (*rejectingReviewedRegistry) RegisterReviewedTool(context.Context, tools.ToolDefinition, tools.ProjectTrustBinding) error {
	return tools.ErrProjectTrustBindingInvalid
}

func TestRegisterDefaultsOrdinaryRegistryPreservesGitWithoutProof(t *testing.T) {
	for _, layer := range []string{"ordinary", "middleware", "deferred-middleware"} {
		t.Run(layer, func(t *testing.T) {
			root := t.TempDir()
			out, err := exec.Command("git", "init", "-q", root).CombinedOutput()
			require.NoError(t, err, "%s", out)
			base := tools.NewDefaultToolRegistry()
			var registry tools.ToolRegistry = &ordinaryReviewedRegistry{ToolRegistry: base}
			if layer == "middleware" {
				registry = tools.NewMiddlewareToolRegistry(registry)
			}
			if layer == "deferred-middleware" {
				registry = tools.NewMiddlewareToolRegistry(tools.NewDeferredToolRegistryAdapter(registry))
			}
			require.NoError(t, tools.RegisterDefaults(t.Context(), registry, tools.WithRegisteredGitTool(tools.NewDefaultGitTool(root)), tools.WithRegisteredBuiltinWhitelist([]string{"git_status"})))
			// The outer optional adapters may require snapshot support for Get;
			// registration must still reach the ordinary underlying registry.
			definition, err := base.Get(t.Context(), "git_status")
			require.NoError(t, err)
			result, err := definition.Execute(t.Context(), tools.ToolCall{Name: "git_status", Args: map[string]any{}})
			require.NoError(t, err)
			require.NotNil(t, result)
			// Inspect the actual underlying registration: fallback cannot mint
			// completion authority through a wrapper's optional interface.
			inspector := tools.NewMiddlewareToolRegistry(base)
			require.NoError(t, inspector.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
				return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
			}))
			require.NoError(t, inspector.SetPreparedToolStages(tools.PreparedToolStages{}))
			prepared, err := inspector.PrepareToolCall(t.Context(), tools.ToolCall{Name: "git_status", Args: map[string]any{}})
			require.NoError(t, err)
			supported, err := tools.PreparedCompletionSupported(t.Context(), prepared, nil)
			require.NoError(t, err)
			require.False(t, supported)
			require.Nil(t, prepared.CompletionSignal().Done())
		})
	}
}

func TestRegisterDefaultsDoesNotFallbackAfterReviewedRegistrationRejection(t *testing.T) {
	registry := &rejectingReviewedRegistry{ToolRegistry: tools.NewDefaultToolRegistry()}
	err := tools.RegisterDefaults(t.Context(), registry, tools.WithRegisteredGitTool(tools.NewDefaultGitTool(t.TempDir())), tools.WithRegisteredBuiltinWhitelist([]string{"git_status"}))
	require.ErrorIs(t, err, tools.ErrProjectTrustBindingInvalid)
	require.Zero(t, registry.registrationCalls)
}
