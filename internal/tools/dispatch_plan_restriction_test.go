package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParentPlanRestrictionRevokesQueuedPreparedMutation(t *testing.T) {
	ctx := t.Context()
	parent := NewDispatchFence()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	require.NoError(t, base.DispatchFence().RestrictPlanBy(parent))
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"write"})))
	registry := NewMiddlewareToolRegistry(base)
	blocked, release := make(chan struct{}), make(chan struct{})
	queue := NewDefaultFileMutationQueue(WithMutationHandler(func(context.Context, FileMutation) (*ToolResult, error) {
		close(blocked)
		<-release
		return &ToolResult{Output: "released"}, nil
	})).(*DefaultFileMutationQueue)
	t.Cleanup(func() { require.NoError(t, queue.Close()) })
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})
	path := filepath.Join(t.TempDir(), "queued-child")
	first, err := queue.Enqueue(ctx, FileMutation{FilePath: path, ToolName: "hold", Operation: "hold"})
	require.NoError(t, err)
	<-blocked
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{ID: "queued", Name: "write", Args: map[string]any{"path": path, "content": "forbidden"}})
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() { _, err := invocation.Dispatch(ctx); result <- err }()
	prepared := invocation.(*preparedInvocation)
	deadline := time.After(5 * time.Second)
	for !prepared.consumed.Load() {
		select {
		case <-deadline:
			t.Fatal("prepared mutation did not reach queue")
		case <-time.After(time.Millisecond):
		}
	}
	_, err = parent.Publish(DispatchFencePlan, nil, func() {})
	require.NoError(t, err)
	_, err = parent.Publish(DispatchFencePlan, nil, func() {})
	require.NoError(t, err)
	releaseOnce.Do(func() { close(release) })
	<-first
	require.ErrorIs(t, <-result, ErrDispatchFenceChanged)
	require.False(t, invocation.DispatchCommitted())
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
}

func TestParentPlanRestrictionGuardsActualEffectsAfterDispatchCommit(t *testing.T) {
	for _, name := range []string{"write", "edit", "bash"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			parent := NewDispatchFence()
			base := NewDefaultToolRegistry().(*DefaultToolRegistry)
			require.NoError(t, base.DispatchFence().RestrictPlanBy(parent))
			require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{name}), WithRegisteredBashSandbox(AllowAllSandbox{})))
			registry := NewMiddlewareToolRegistry(base)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			wrapper := ToolExecutorWrapper(func(next func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
				return func(ctx context.Context, call ToolCall) (*ToolResult, error) {
					close(started)
					<-release
					return next(ctx, call)
				}
			})
			queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue)
			t.Cleanup(func() { require.NoError(t, queue.Close()) })
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{ExecutionWrappers: []ToolExecutorWrapper{wrapper}, SynchronousExecutionWrappers: true, MutationQueue: queue})
			path := filepath.Join(t.TempDir(), "final-effect")
			args := map[string]any{"path": path, "content": "forbidden"}
			if name == "edit" {
				require.NoError(t, os.WriteFile(path, []byte("before"), 0600))
				args = map[string]any{"file_path": path, "old_string": "before", "new_string": "forbidden"}
			}
			if name == "bash" {
				args = map[string]any{"command": fmt.Sprintf("printf forbidden > %q", path)}
			}
			invocation, err := registry.PrepareToolCall(ctx, ToolCall{ID: "committed-child", Name: name, Args: args})
			require.NoError(t, err)
			result := make(chan error, 1)
			go func() { _, err := invocation.Dispatch(ctx); result <- err }()
			<-started
			require.True(t, invocation.DispatchCommitted())
			_, err = parent.Publish(DispatchFencePlan, nil, func() {})
			require.NoError(t, err)
			once.Do(func() { close(release) })
			require.ErrorIs(t, <-result, ErrDispatchFenceChanged)
			if name == "edit" {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, "before", string(data))
			} else {
				_, err = os.Stat(path)
				require.True(t, os.IsNotExist(err))
			}
		})
	}
}

func TestParentPlanRestrictionGuardsBashGateReleaseAfterIdentitySync(t *testing.T) {
	parent := NewDispatchFence()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	require.NoError(t, base.DispatchFence().RestrictPlanBy(parent))
	require.NoError(t, RegisterDefaults(t.Context(), base, WithRegisteredBuiltinWhitelist([]string{"bash"}), WithRegisteredBashSandbox(AllowAllSandbox{})))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	observed, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	ctx := WithProcessExecutionIdentityObserver(t.Context(), func(context.Context, ProcessExecutionIdentity) error { close(observed); <-release; return nil })
	path := filepath.Join(t.TempDir(), "blocked-bootstrap")
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{ID: "bootstrap-child", Name: "bash", Args: map[string]any{"command": fmt.Sprintf("printf forbidden > %q", path)}})
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() { _, err := invocation.Dispatch(ctx); result <- err }()
	<-observed
	_, err = parent.Publish(DispatchFencePlan, nil, func() {})
	require.NoError(t, err)
	once.Do(func() { close(release) })
	require.ErrorIs(t, <-result, ErrDispatchFenceChanged)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
}

func TestPlanRestrictionCannotCycleAndOnlyImportsPlanEpoch(t *testing.T) {
	parent, child, grandchild := NewDispatchFence(), NewDispatchFence(), NewDispatchFence()
	require.NoError(t, child.RestrictPlanBy(parent))
	require.NoError(t, grandchild.RestrictPlanBy(child))
	require.Error(t, parent.RestrictPlanBy(grandchild))
	require.Error(t, child.RestrictPlanBy(NewDispatchFence()))
	initial := grandchild.Snapshot()
	_, err := parent.Publish(DispatchFenceRegistry, nil, func() {})
	require.NoError(t, err)
	require.Equal(t, initial, grandchild.Snapshot(), "parent registry does not become child execution authority")
	_, err = parent.Publish(DispatchFencePlan, nil, func() {})
	require.NoError(t, err)
	require.NotEqual(t, initial, grandchild.Snapshot())
	require.ErrorIs(t, grandchild.commit(initial, func() { t.Fatal("ancestor revoked permit committed") }), ErrDispatchFenceChanged)
}

func TestPlanRestrictionCheckedPublicationIncludesAncestorEpoch(t *testing.T) {
	parent, child := NewDispatchFence(), NewDispatchFence()
	require.NoError(t, child.RestrictPlanBy(parent))
	old := child.Snapshot()
	_, err := parent.Publish(DispatchFencePlan, nil, func() {})
	require.NoError(t, err)
	_, err = child.publishChecked(DispatchFenceRegistry, &old, func() error { t.Fatal("stale publication callback ran"); return nil })
	require.ErrorIs(t, err, ErrDispatchFenceChanged)
	current := child.Snapshot()
	published, err := child.publishChecked(DispatchFenceRegistry, &current, func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, child.Snapshot(), published)
	require.Equal(t, current.PlanRestriction, published.PlanRestriction)
	require.Equal(t, current.Plan, published.Plan, "an ancestor restriction does not become child Plan authority")
}

func TestPlanRestrictionCommitLinearizesWithParentEnterExit(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		parent, child := NewDispatchFence(), NewDispatchFence()
		require.NoError(t, child.RestrictPlanBy(parent))
		captured := child.Snapshot()
		begin := make(chan struct{})
		var active, markedWhileActive atomic.Bool
		committed := make(chan error, 1)
		transitioned := make(chan error, 1)
		go func() {
			<-begin
			committed <- child.commit(captured, func() { markedWhileActive.Store(active.Load()) })
		}()
		go func() {
			<-begin
			_, err := parent.Publish(DispatchFencePlan, nil, func() { active.Store(true) })
			if err == nil {
				_, err = parent.Publish(DispatchFencePlan, nil, func() { active.Store(false) })
			}
			transitioned <- err
		}()
		close(begin)
		err := <-committed
		require.True(t, err == nil || err == ErrDispatchFenceChanged)
		require.NoError(t, <-transitioned)
		require.False(t, markedWhileActive.Load(), "parent activation and child start must have one atomic ordering")
	}
}
