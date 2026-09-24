package approval

import (
	"context"
	"sync"
	"testing"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type preparedRuntimeAskClassifier struct{}

func (preparedRuntimeAskClassifier) Name() string { return "prepared_runtime_ask" }
func (preparedRuntimeAskClassifier) Classify(context.Context, tools.ToolCall) Classification {
	return Ask
}

type preparedRuntimeCallback struct {
	mu      sync.Mutex
	results map[string][]ApprovalResult
	calls   map[string]int
}

type preparedConcurrentAlwaysCallback struct {
	arrived chan<- string
	release <-chan struct{}
}

func (c preparedConcurrentAlwaysCallback) RequestApproval(ctx context.Context, toolName string, _ map[string]any) (ApprovalResult, error) {
	select {
	case c.arrived <- toolName:
	case <-ctx.Done():
		return ApprovalDeny, ctx.Err()
	}
	select {
	case <-c.release:
		return ApprovalAlwaysAllow, nil
	case <-ctx.Done():
		return ApprovalDeny, ctx.Err()
	}
}

func (c *preparedRuntimeCallback) RequestApproval(_ context.Context, toolName string, _ map[string]any) (ApprovalResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[toolName]++
	results := c.results[toolName]
	if len(results) == 0 {
		return ApprovalDeny, nil
	}
	result := results[0]
	c.results[toolName] = results[1:]
	return result, nil
}

func (c *preparedRuntimeCallback) count(toolName string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[toolName]
}

type preparedRuntimeTool struct {
	name  string
	mu    sync.Mutex
	calls int
}

func (t *preparedRuntimeTool) Name() string        { return t.name }
func (t *preparedRuntimeTool) Description() string { return "prepared runtime test tool" }
func (t *preparedRuntimeTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return &tools.ToolResult{Output: t.name}, nil
}

func (t *preparedRuntimeTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func newPreparedRuntimeFixture(t *testing.T, callback ApprovalCallback, cache *ApprovalCache, toolNames ...string) (*tools.MiddlewareToolRegistry, *ApprovalMiddleware, *core.DefaultPlanModeController, *InMemoryApprovalStore, map[string]*preparedRuntimeTool) {
	return newPreparedRuntimeFixtureWithOptions(t, callback, cache, nil, toolNames...)
}

func newPreparedRuntimeFixtureWithOptions(t *testing.T, callback ApprovalCallback, cache *ApprovalCache, extraOptions []Option, toolNames ...string) (*tools.MiddlewareToolRegistry, *ApprovalMiddleware, *core.DefaultPlanModeController, *InMemoryApprovalStore, map[string]*preparedRuntimeTool) {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	registered := make(map[string]*preparedRuntimeTool, len(toolNames))
	for _, name := range toolNames {
		tool := &preparedRuntimeTool{name: name}
		require.NoError(t, base.Register(ctx, tool))
		registered[name] = tool
	}
	fence := base.DispatchFence()
	controller := core.NewDefaultPlanModeController(fence)
	store := NewInMemoryApprovalStore()
	options := []Option{WithDispatchFence(fence), WithPlanModeController(controller), WithCallback(callback)}
	if cache != nil {
		options = append(options, WithCache(cache))
	}
	options = append(options, extraOptions...)
	middleware := NewApprovalMiddleware(preparedRuntimeAskClassifier{}, store, options...)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	return registry, middleware, controller, store, registered
}

func TestPreparedApprovalUserDenyInvalidatesSiblingPermit(t *testing.T) {
	ctx := context.Background()
	callback := &preparedRuntimeCallback{
		results: map[string][]ApprovalResult{
			"write_a": {ApprovalAlwaysAllow},
			"write_b": {ApprovalDeny},
		},
		calls: make(map[string]int),
	}
	registry, _, _, _, toolsByName := newPreparedRuntimeFixture(t, callback, NewApprovalCache(""), "write_a", "write_b")

	permitA, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "write_a", Args: map[string]any{"path": "a"}})
	require.NoError(t, err)
	_, err = registry.PrepareToolCall(ctx, tools.ToolCall{Name: "write_b", Args: map[string]any{"path": "b"}})
	require.ErrorIs(t, err, ErrToolDenied)

	_, err = permitA.Dispatch(ctx)
	require.ErrorIs(t, err, tools.ErrDispatchFenceChanged, "an explicit user denial revokes previously prepared permits")
	assert.Zero(t, toolsByName["write_a"].callCount())
	assert.Zero(t, toolsByName["write_b"].callCount())
}

func TestPreparedPermitStalesOnModePlanAndRegistryChange(t *testing.T) {
	for _, mutation := range []string{"mode", "plan", "registry"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			callback := &preparedRuntimeCallback{
				results: map[string][]ApprovalResult{"target": {ApprovalAlwaysAllow}},
				calls:   make(map[string]int),
			}
			registry, middleware, controller, _, toolByName := newPreparedRuntimeFixtureWithOptions(
				t,
				callback,
				NewApprovalCache(""),
				[]Option{WithPermissionModeResolver(NewDefaultPermissionModeResolver())},
				"target",
			)
			invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "target", Args: map[string]any{"path": "value"}})
			require.NoError(t, err)

			switch mutation {
			case "mode":
				middleware.SetPermissionMode(PermissionAuto)
			case "plan":
				require.NoError(t, controller.Enter(ctx, "invalidate prepared invocation"))
			case "registry":
				require.NoError(t, registry.Register(ctx, &preparedRuntimeTool{name: "target"}))
			}

			_, err = invocation.Dispatch(ctx)
			require.ErrorIs(t, err, tools.ErrDispatchFenceChanged)
			assert.Zero(t, toolByName["target"].callCount())
		})
	}
}

func TestPreparedApprovalPolicyReplacementDoesNotReusePriorGrant(t *testing.T) {
	ctx := context.Background()
	callback := &preparedRuntimeCallback{
		results: map[string][]ApprovalResult{"target": {ApprovalAlwaysAllow, ApprovalDeny}},
		calls:   make(map[string]int),
	}
	registry, middleware, _, _, toolByName := newPreparedRuntimeFixture(t, callback, NewApprovalCache(""), "target")

	first, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "target", Args: map[string]any{"path": "file"}})
	require.NoError(t, err)
	_, err = first.Dispatch(ctx)
	require.NoError(t, err)

	// Replacing the installed prepared policy advances the shared approval
	// generation. The same normalized call must therefore ask again instead of
	// reusing the previous AlwaysAllow grant.
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	_, err = registry.PrepareToolCall(ctx, tools.ToolCall{Name: "target", Args: map[string]any{"path": "file"}})
	require.ErrorIs(t, err, ErrToolDenied)
	assert.Equal(t, 2, callback.count("target"))
	assert.Equal(t, 1, toolByName["target"].callCount())
}

func TestPreparedDefaultAndAutoReadNameNeedsExplicitApproval(t *testing.T) {
	for _, mode := range []PermissionMode{PermissionDefault, PermissionAuto} {
		t.Run(mode.String(), func(t *testing.T) {
			ctx := context.Background()
			callback := &preparedRuntimeCallback{
				results: map[string][]ApprovalResult{"read": {ApprovalAllow}},
				calls:   make(map[string]int),
			}
			base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
			read := &preparedRuntimeTool{name: "read"}
			require.NoError(t, base.Register(ctx, read))
			fence := base.DispatchFence()
			controller := core.NewDefaultPlanModeController(fence)
			middleware := NewApprovalMiddleware(&DenyAllClassifier{}, NewInMemoryApprovalStore(),
				WithPermissionModeResolver(NewDefaultPermissionModeResolver()),
				WithPermissionMode(mode),
				WithCallback(callback),
				WithDispatchFence(fence),
				WithPlanModeController(controller),
			)
			registry := tools.NewMiddlewareToolRegistry(base)
			require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
			require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

			invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "read", Args: map[string]any{"path": "outside-scope"}})
			require.NoError(t, err, "a user may approve an otherwise unknown read explicitly")
			assert.Equal(t, 1, callback.count("read"), "name-only classifier allow must resolve through approval")
			_, err = invocation.Dispatch(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1, read.callCount())
		})
	}
}

func TestPreparedAutoFullRetainsExplicitOperatorAuthorization(t *testing.T) {
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	read := &preparedRuntimeTool{name: "read"}
	require.NoError(t, base.Register(ctx, read))
	fence := base.DispatchFence()
	controller := core.NewDefaultPlanModeController(fence)
	middleware := NewApprovalMiddleware(&DenyAllClassifier{}, NewInMemoryApprovalStore(),
		WithPermissionModeResolver(NewDefaultPermissionModeResolver()),
		WithPermissionMode(PermissionAutoFull),
		WithDispatchFence(fence),
		WithPlanModeController(controller),
	)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "read"})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err, "AutoFull is an explicit operator-selected authorization mode")
	assert.Equal(t, 1, read.callCount())
}

func TestPreparedClassifierDenyDoesNotPublishExplicitRevocation(t *testing.T) {
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	tool := &preparedRuntimeTool{name: "denied"}
	require.NoError(t, base.Register(ctx, tool))
	fence := base.DispatchFence()
	controller := core.NewDefaultPlanModeController(fence)
	middleware := NewApprovalMiddleware(&DenyAllClassifier{}, NewInMemoryApprovalStore(),
		WithDispatchFence(fence), WithPlanModeController(controller))
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

	before := fence.Snapshot()
	_, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "denied"})
	require.ErrorIs(t, err, ErrToolDenied)
	assert.Equal(t, before, fence.Snapshot(), "classifier denial is local policy evaluation, not a user revocation")
	assert.Zero(t, tool.callCount())
}

func TestPreparedParallelAlwaysAllowGrantsBothDispatch(t *testing.T) {
	ctx := context.Background()
	arrived := make(chan string, 2)
	release := make(chan struct{})
	callback := preparedConcurrentAlwaysCallback{arrived: arrived, release: release}
	registry, _, _, _, toolsByName := newPreparedRuntimeFixture(t, callback, NewApprovalCache(""), "write_a", "write_b")

	type result struct {
		name       string
		invocation tools.PreparedInvocation
		err        error
	}
	results := make(chan result, 2)
	for _, name := range []string{"write_a", "write_b"} {
		go func(name string) {
			invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: name, Args: map[string]any{"path": name}})
			results <- result{name: name, invocation: invocation, err: err}
		}(name)
	}
	arrivedNames := []string{<-arrived, <-arrived}
	assert.ElementsMatch(t, []string{"write_a", "write_b"}, arrivedNames)
	close(release)

	prepared := make([]result, 0, 2)
	for range 2 {
		prepared = append(prepared, <-results)
	}
	for _, outcome := range prepared {
		require.NoError(t, outcome.err, "%s should retain its positive permit", outcome.name)
		require.NotNil(t, outcome.invocation)
	}
	for _, outcome := range prepared {
		_, err := outcome.invocation.Dispatch(ctx)
		require.NoError(t, err, "%s must remain dispatchable after the sibling AlwaysAllow grant", outcome.name)
		assert.Equal(t, 1, toolsByName[outcome.name].callCount())
	}
}

func TestPreparedStoreGrantRejectsSnapshotRevokedBeforePersistence(t *testing.T) {
	fence := tools.NewDispatchFence()
	store := NewInMemoryApprovalStore()
	require.NoError(t, store.bindFence(fence))
	expected := fence.Snapshot()
	_, err := fence.Publish(tools.DispatchFenceApproval, nil, func() {})
	require.NoError(t, err)

	err = store.setPreparedGrant(expected, "prepared:stale")
	require.ErrorIs(t, err, tools.ErrDispatchFenceChanged)
	_, found, getErr := store.Get(context.Background(), "prepared:stale")
	require.NoError(t, getErr)
	assert.False(t, found, "a stale callback may not rebase and publish its grant under a new generation")
}
