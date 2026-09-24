package approval

import (
	"context"
	"testing"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type preparedUnfencedDenyStore struct{}

func (preparedUnfencedDenyStore) Get(context.Context, string) (Classification, bool, error) {
	return Deny, true, nil
}

func (preparedUnfencedDenyStore) Set(context.Context, string, Classification) error { return nil }
func (preparedUnfencedDenyStore) Delete(context.Context, string) error              { return nil }

func TestPreparedPolicyFailsClosedWithUnfencedApprovalStore(t *testing.T) {
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	target := &preparedRuntimeTool{name: "target"}
	require.NoError(t, base.Register(ctx, target))
	fence := base.DispatchFence()
	controller := core.NewDefaultPlanModeController(fence)
	callback := &preparedRuntimeCallback{
		results: map[string][]ApprovalResult{"target": {ApprovalAllow}},
		calls:   make(map[string]int),
	}
	middleware := NewApprovalMiddleware(&AllowAllClassifier{}, preparedUnfencedDenyStore{},
		WithDispatchFence(fence), WithPlanModeController(controller), WithCallback(callback))
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

	_, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "target", Args: map[string]any{"id": "1"}})
	assert.ErrorIs(t, err, ErrPreparedPolicyUnavailable, "prepared dispatch must not ignore an ApprovalStore it cannot fence")
	assert.ErrorIs(t, err, ErrPreparedApprovalStoreUnsupported, "the failure should identify the missing fenced-store adapter")
	assert.Zero(t, callback.count("target"), "unfenced store state must fail before prompting")
	assert.Zero(t, target.callCount(), "an unfenced store deny must never be bypassed by a classifier allow")
}

func TestPreparedPolicyValidatesExplicitModeWithoutResolver(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		setMode bool
	}{
		{name: "WithPermissionMode"},
		{name: "SetPermissionMode", setMode: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
			target := &preparedRuntimeTool{name: "target"}
			require.NoError(t, base.Register(ctx, target))
			fence := base.DispatchFence()
			controller := core.NewDefaultPlanModeController(fence)
			options := []Option{
				WithDispatchFence(fence),
				WithPlanModeController(controller),
			}
			if !testCase.setMode {
				options = append(options, WithPermissionMode(PermissionMode(97)))
			}
			middleware := NewApprovalMiddleware(&AllowAllClassifier{}, NewInMemoryApprovalStore(), options...)
			if testCase.setMode {
				middleware.SetPermissionMode(PermissionMode(97))
			}
			registry := tools.NewMiddlewareToolRegistry(base)
			require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
			require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

			_, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "target"})
			assert.ErrorIs(t, err, ErrInvalidPermissionMode, "default classifier semantics must not mask an invalid stored mode")
			assert.Zero(t, target.callCount())
		})
	}
}
