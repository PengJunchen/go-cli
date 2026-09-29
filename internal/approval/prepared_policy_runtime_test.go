package approval

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
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

type preparedWaitingApprovalCallback struct {
	entered chan<- struct{}
	release <-chan struct{}
}

type preparedMutatingApprovalCallback struct {
	start    <-chan struct{}
	finished chan<- struct{}
}

type preparedArgumentCaptureTool struct {
	mutationFinished <-chan struct{}
	mu               sync.Mutex
	args             map[string]any
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

func (c preparedWaitingApprovalCallback) RequestApproval(ctx context.Context, _ string, _ map[string]any) (ApprovalResult, error) {
	select {
	case c.entered <- struct{}{}:
	case <-ctx.Done():
		return ApprovalDeny, ctx.Err()
	}
	select {
	case <-c.release:
		return ApprovalAllow, nil
	case <-ctx.Done():
		return ApprovalDeny, ctx.Err()
	}
}

func (c preparedMutatingApprovalCallback) RequestApproval(_ context.Context, _ string, args map[string]any) (ApprovalResult, error) {
	nested := args["nested"].(map[string]any)
	tags := nested["tags"].([]any)
	go func() {
		<-c.start
		for i := 0; i < 10000; i++ {
			args["path"] = "callback-mutated"
			nested["value"] = i
			tags[0] = i
			runtime.Gosched()
		}
		close(c.finished)
	}()
	return ApprovalAllow, nil
}

func (*preparedArgumentCaptureTool) Name() string { return "capture_args" }
func (*preparedArgumentCaptureTool) Description() string {
	return "prepared argument capture test tool"
}
func (t *preparedArgumentCaptureTool) Execute(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	for i := 0; i < 10000; i++ {
		_ = call.Args["path"]
		if nested, ok := call.Args["nested"].(map[string]any); ok {
			_ = nested["value"]
			if tags, ok := nested["tags"].([]any); ok && len(tags) > 0 {
				_ = tags[0]
			}
		}
		runtime.Gosched()
	}
	<-t.mutationFinished
	nested := call.Args["nested"].(map[string]any)
	tags := nested["tags"].([]any)
	t.mu.Lock()
	t.args = map[string]any{
		"path":  call.Args["path"],
		"value": nested["value"],
		"tag":   tags[0],
	}
	t.mu.Unlock()
	return &tools.ToolResult{Output: "captured"}, nil
}

func (t *preparedArgumentCaptureTool) capturedArgs() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.args
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

func TestApprovalAuditRecordsFinalChoice(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	trail := NewAuditTrail(dir)
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	target := &preparedRuntimeTool{name: "audit_target"}
	require.NoError(t, base.Register(ctx, target))
	fence := base.DispatchFence()
	controller := core.NewDefaultPlanModeController(fence)
	classifier := NewAuditClassifier(preparedRuntimeAskClassifier{}, trail, PermissionDefault, "session-1")
	callback := NewInteractiveApprovalCallback(strings.NewReader("a\n"), io.Discard)
	middleware := NewApprovalMiddleware(classifier, NewInMemoryApprovalStore(),
		WithCallback(callback),
		WithDispatchFence(fence),
		WithPlanModeController(controller),
	)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{
		Name: "audit_target",
		Args: map[string]any{"content": "prompt-secret-sentinel"},
	})
	require.NoError(t, err)
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, target.callCount())

	entries, err := trail.ReadEntries(time.Now())
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "classifier_proposal", entries[0].Stage)
	assert.Equal(t, "ask", entries[0].Decision)
	assert.Equal(t, "approval_resolution", entries[1].Stage)
	assert.Equal(t, "ask", entries[1].Decision)
	assert.Equal(t, "allow_always", entries[1].UserChoice)
	assert.Equal(t, "user", entries[1].Resolution)

	raw, err := os.ReadFile(filepath.Join(dir, time.Now().UTC().Format("2006-01-02")+".jsonl"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "prompt-secret-sentinel")
}

func TestPreparedApprovalAuditWriteFailureStopsDispatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	trail := NewAuditTrail(dir)
	syncErr := errors.New("sync sentinel")
	rollbackErr := errors.New("truncate sentinel")
	trail.openFile = func(path string, flag int, perm os.FileMode) (auditTrailFile, error) {
		file, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
		return &auditTrailFaultFile{File: file, syncErr: syncErr, truncateErr: rollbackErr}, nil
	}
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	target := &preparedRuntimeTool{name: "audit_target"}
	require.NoError(t, base.Register(ctx, target))
	fence := base.DispatchFence()
	controller := core.NewDefaultPlanModeController(fence)
	callback := &preparedRuntimeCallback{results: map[string][]ApprovalResult{"audit_target": {ApprovalAlwaysAllow}}, calls: make(map[string]int)}
	classifier := NewAuditClassifier(preparedRuntimeAskClassifier{}, trail, PermissionDefault, "session-1")
	middleware := NewApprovalMiddleware(classifier, NewInMemoryApprovalStore(),
		WithCallback(callback),
		WithDispatchFence(fence),
		WithPlanModeController(controller),
	)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

	_, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "audit_target"})
	assert.ErrorIs(t, err, ErrAuditTrailSync)
	assert.ErrorIs(t, err, ErrAuditTrailRollback)
	assert.ErrorIs(t, err, syncErr)
	assert.ErrorIs(t, err, rollbackErr)
	assert.Zero(t, callback.count("audit_target"), "approval must not be requested after a failed classifier audit")
	assert.Zero(t, target.callCount(), "tool dispatch must stop when its classifier audit cannot be committed")
}

func TestPreparedApprovalResolutionAuditFailureStopsDispatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	trail := NewAuditTrail(dir)
	syncErr := errors.New("resolution sync sentinel")
	syncCounter := &auditSyncCounter{}
	trail.openFile = func(path string, flag int, perm os.FileMode) (auditTrailFile, error) {
		file, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
		return &auditSecondSyncFailureFile{File: file, counter: syncCounter, failAt: 2, syncErr: syncErr}, nil
	}
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	target := &preparedRuntimeTool{name: "audit_target"}
	require.NoError(t, base.Register(ctx, target))
	fence := base.DispatchFence()
	controller := core.NewDefaultPlanModeController(fence)
	callback := &preparedRuntimeCallback{results: map[string][]ApprovalResult{"audit_target": {ApprovalAlwaysAllow}}, calls: make(map[string]int)}
	classifier := NewAuditClassifier(preparedRuntimeAskClassifier{}, trail, PermissionDefault, "session-1")
	middleware := NewApprovalMiddleware(classifier, NewInMemoryApprovalStore(),
		WithCallback(callback),
		WithDispatchFence(fence),
		WithPlanModeController(controller),
	)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

	_, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "audit_target"})
	assert.ErrorIs(t, err, ErrAuditTrailSync)
	assert.ErrorIs(t, err, syncErr)
	assert.Equal(t, 1, callback.count("audit_target"))
	assert.Zero(t, target.callCount(), "an undurable final approval choice must not publish a permit")
	entries, readErr := trail.ReadEntries(time.Now())
	require.NoError(t, readErr)
	require.Len(t, entries, 1, "the failed resolution append must roll back without damaging the prior proposal")
	assert.Equal(t, "classifier_proposal", entries[0].Stage)
}

type auditSyncCounter struct {
	mu    sync.Mutex
	calls int
}

func (c *auditSyncCounter) next() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.calls
}

type auditSecondSyncFailureFile struct {
	*os.File
	counter *auditSyncCounter
	failAt  int
	syncErr error
}

func (f *auditSecondSyncFailureFile) Sync() error {
	if f.counter.next() == f.failAt {
		return f.syncErr
	}
	return f.File.Sync()
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

func TestPreparedWritersDuringApprovalWaitInvalidatePermit(t *testing.T) {
	for _, writer := range []string{"mode", "plan", "registry"} {
		t.Run(writer, func(t *testing.T) {
			ctx := context.Background()
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseCallback()
			callback := preparedWaitingApprovalCallback{entered: entered, release: release}
			registry, middleware, controller, _, toolsByName := newPreparedRuntimeFixtureWithOptions(
				t,
				callback,
				NewApprovalCache(""),
				[]Option{WithPermissionModeResolver(NewDefaultPermissionModeResolver())},
				"target",
			)
			type result struct {
				invocation tools.PreparedInvocation
				err        error
			}
			prepared := make(chan result, 1)
			go func() {
				invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "target", Args: map[string]any{"path": "original"}})
				prepared <- result{invocation: invocation, err: err}
			}()

			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("approval callback did not enter")
			}
			writerDone := make(chan error, 1)
			go func() {
				switch writer {
				case "mode":
					middleware.SetPermissionMode(PermissionAuto)
					writerDone <- nil
				case "plan":
					writerDone <- controller.Enter(ctx, "stale user approval")
				case "registry":
					writerDone <- registry.Register(ctx, &preparedRuntimeTool{name: "target"})
				}
			}()
			select {
			case err := <-writerDone:
				require.NoError(t, err, "fence writer should complete while the approval callback waits")
			case <-time.After(2 * time.Second):
				releaseCallback()
				select {
				case <-writerDone:
				case <-time.After(2 * time.Second):
					t.Fatal("fence writer remained blocked after releasing the approval callback")
				}
				t.Fatal("fence writer blocked while the approval callback waited")
			}
			releaseCallback()

			var outcome result
			select {
			case outcome = <-prepared:
			case <-time.After(5 * time.Second):
				t.Fatal("prepared callback did not return")
			}
			require.ErrorIs(t, outcome.err, tools.ErrDispatchFenceChanged)
			assert.Nil(t, outcome.invocation)
			assert.Zero(t, toolsByName["target"].callCount(), "a state writer during user wait must prevent dispatch")
		})
	}
}

func TestPreparedCallbackArgumentMutationCannotChangeFrozenDispatch(t *testing.T) {
	ctx := context.Background()
	startMutation := make(chan struct{})
	mutationFinished := make(chan struct{})
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	target := &preparedArgumentCaptureTool{mutationFinished: mutationFinished}
	require.NoError(t, base.Register(ctx, target))
	fence := base.DispatchFence()
	controller := core.NewDefaultPlanModeController(fence)
	callback := preparedMutatingApprovalCallback{start: startMutation, finished: mutationFinished}
	middleware := NewApprovalMiddleware(preparedRuntimeAskClassifier{}, NewInMemoryApprovalStore(),
		WithCallback(callback), WithDispatchFence(fence), WithPlanModeController(controller))
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{
		Name: "capture_args",
		Args: map[string]any{
			"path": "original-path",
			"nested": map[string]any{
				"value": "original-value",
				"tags":  []any{"original-tag"},
			},
		},
	})
	require.NoError(t, err)
	close(startMutation)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"path":  "original-path",
		"value": "original-value",
		"tag":   "original-tag",
	}, target.capturedArgs(), "callback argument edits must not affect the frozen tool call")
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
