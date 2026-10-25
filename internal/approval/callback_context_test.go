package approval

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type cancelOnApprovalRead struct {
	cancel context.CancelFunc
	reads  int
}

func (reader *cancelOnApprovalRead) Read(buffer []byte) (int, error) {
	reader.reads++
	reader.cancel()
	return copy(buffer, "a\n"), nil
}

func TestInteractiveApprovalRejectsCancellationBeforeAndDuringRead(t *testing.T) {
	for _, beforeRead := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &cancelOnApprovalRead{cancel: cancel}
		if beforeRead {
			cancel()
		}
		callback := NewInteractiveApprovalCallback(reader, io.Discard)
		result, choice, source, err := callback.requestApprovalWithChoice(ctx, "bash", nil)
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, ApprovalDeny, result)
		require.Empty(t, choice)
		require.Equal(t, "canceled", source)
		if beforeRead {
			require.Zero(t, reader.reads, "an already-cancelled request cannot consume terminal input")
		} else {
			require.Equal(t, 1, reader.reads)
		}
		cancel()
	}
}

func TestInteractiveApprovalLateAlwaysCannotLeavePreparedGrant(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callback := NewInteractiveApprovalCallback(&cancelOnApprovalRead{cancel: cancel}, io.Discard)
	cache := NewApprovalCache("")
	registry, _, _, store, targets := newPreparedRuntimeFixture(t, callback, cache, "target")
	call := tools.ToolCall{Name: "target", Args: map[string]any{"path": "same"}}
	_, err := registry.PrepareToolCall(ctx, call)
	require.ErrorIs(t, err, context.Canceled)
	cache.mu.RLock()
	cacheEntries := len(cache.entries)
	cache.mu.RUnlock()
	store.mu.RLock()
	storeEntries := len(store.decisions)
	store.mu.RUnlock()
	require.Zero(t, cacheEntries, "canceled input cannot publish a cache grant")
	require.Zero(t, storeEntries, "canceled input cannot publish a store grant")
	callback.stdin = strings.NewReader("n\n")
	_, err = registry.PrepareToolCall(context.Background(), call)
	require.ErrorIs(t, err, ErrToolDenied, "the same call cannot inherit a late always-allow grant")
	require.Zero(t, targets["target"].callCount())
}

func TestPreparedExplicitClassifierAndResolverDenyRemainAuthoritative(t *testing.T) {
	for _, useResolver := range []bool{false, true} {
		ctx := context.Background()
		base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
		target := &preparedRuntimeTool{name: "target"}
		require.NoError(t, base.Register(ctx, target))
		fence := base.DispatchFence()
		callback := &MockApprovalCallback{result: ApprovalAlwaysAllow}
		options := []Option{WithDispatchFence(fence), WithPlanModeController(core.NewDefaultPlanModeController(fence)), WithCallback(callback)}
		var classifier ApprovalClassifier = &DenyAllClassifier{}
		if useResolver {
			classifier = &AllowAllClassifier{}
			options = append(options, WithPermissionModeResolver(&customResolver{auto: &DenyAllClassifier{}}), WithPermissionMode(PermissionAuto))
		}
		middleware := NewApprovalMiddleware(classifier, NewInMemoryApprovalStore(), options...)
		registry := tools.NewMiddlewareToolRegistry(base)
		require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
		require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
		_, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "target"})
		require.ErrorIs(t, err, ErrToolDenied)
		require.Zero(t, callback.callCount())
		require.Zero(t, target.callCount())
	}
}
