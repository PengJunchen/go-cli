package approval

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
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

type preparedBlockingRuntimeTool struct {
	name    string
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func (t *preparedBlockingRuntimeTool) Name() string      { return t.name }
func (*preparedBlockingRuntimeTool) Description() string { return "prepared blocking project tool" }
func (t *preparedBlockingRuntimeTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	t.once.Do(func() { close(t.started) })
	<-t.release
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return &tools.ToolResult{Output: "completed"}, nil
}

func (t *preparedBlockingRuntimeTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
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
	name          string
	resultOutput  string
	resultIsError bool
	mu            sync.Mutex
	calls         int
}

func TestProjectTrustRevokeDuringAskDoesNotHoldLockOrDispatch(t *testing.T) {
	const (
		storePathEnv = "GO_CLI_TRUST_ASK_HELPER_STORE"
		rootPathEnv  = "GO_CLI_TRUST_ASK_HELPER_ROOT"
	)
	if storePath := os.Getenv(storePathEnv); storePath != "" {
		manager := NewDefaultTrustManager(NewFileTrustStore(storePath)).(*DefaultTrustManager)
		if err := manager.RevokeTrust(context.Background(), os.Getenv(rootPathEnv)); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(2)
		}
		os.Exit(0)
	}

	ctx := context.Background()
	storePath := privateTrustStorePath(t)
	root := t.TempDir()
	snapshot := testProjectSnapshot(t, root, []byte("ask-time revocation"), nil, nil)
	manager := NewDefaultTrustManager(NewFileTrustStore(storePath)).(*DefaultTrustManager)
	require.NoError(t, manager.TrustProjectSourceSnapshot(ctx, snapshot))
	binding, err := manager.CheckProjectSourceSnapshot(ctx, snapshot)
	require.NoError(t, err)

	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	target := &preparedRuntimeTool{name: "ask_target"}
	registrar := tools.ProjectSourcedToolRegistrar(base)
	require.NoError(t, registrar.RegisterProjectSourced(ctx, target, binding))
	fence := base.DispatchFence()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	middleware := NewApprovalMiddleware(preparedRuntimeAskClassifier{}, NewInMemoryApprovalStore(),
		WithCallback(preparedWaitingApprovalCallback{entered: entered, release: release}),
		WithDispatchFence(fence),
		WithPlanModeController(core.NewDefaultPlanModeController(fence)),
	)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	type preparedResult struct {
		invocation tools.PreparedInvocation
		err        error
	}
	prepareDone := make(chan preparedResult, 1)
	go func() {
		invocation, prepareErr := registry.PrepareToolCall(ctx, tools.ToolCall{Name: target.Name(), Args: map[string]any{"value": "x"}})
		prepareDone <- preparedResult{invocation: invocation, err: prepareErr}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("approval callback did not block before the trust revocation")
	}

	command := exec.Command(os.Args[0], "-test.run=^TestProjectTrustRevokeDuringAskDoesNotHoldLockOrDispatch$")
	command.Env = append(os.Environ(), storePathEnv+"="+storePath, rootPathEnv+"="+snapshot.CanonicalRoot())
	if err := command.Start(); err != nil {
		close(release)
		t.Fatal(err)
	}
	revokeDone := make(chan error, 1)
	go func() { revokeDone <- command.Wait() }()
	select {
	case err := <-revokeDone:
		require.NoError(t, err, "a second process must revoke while approval is waiting")
	case <-time.After(5 * time.Second):
		close(release)
		_ = <-revokeDone
		t.Fatal("cross-process revoke was blocked while the approval callback was waiting")
	}

	close(release)
	select {
	case prepared := <-prepareDone:
		require.NoError(t, prepared.err, "the Ask callback may approve without a trust lock held")
		require.NotNil(t, prepared.invocation)
		_, err := prepared.invocation.Dispatch(ctx)
		require.Error(t, err, "a permit approved before revoke must fail its final generation commit")
		assert.False(t, prepared.invocation.DispatchCommitted())
	case <-time.After(3 * time.Second):
		t.Fatal("prepared call did not return after approval was released")
	}
	assert.Zero(t, target.callCount(), "revoked tool execution must not start")
}

func TestProjectTrustCommitWinsBeforeLateRevoke(t *testing.T) {
	ctx := context.Background()
	storePath := privateTrustStorePath(t)
	root := t.TempDir()
	snapshot := testProjectSnapshot(t, root, []byte("commit-before-revoke"), nil, nil)
	first := NewDefaultTrustManager(NewFileTrustStore(storePath)).(*DefaultTrustManager)
	second := NewDefaultTrustManager(NewFileTrustStore(storePath)).(*DefaultTrustManager)
	require.NoError(t, first.TrustProjectSourceSnapshot(ctx, snapshot))
	binding, err := first.CheckProjectSourceSnapshot(ctx, snapshot)
	require.NoError(t, err)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseExecutor := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseExecutor)
	target := &preparedBlockingRuntimeTool{name: "late_revoke", started: make(chan struct{}), release: release}
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, base.RegisterProjectSourced(ctx, target, binding))
	fence := base.DispatchFence()
	callback := &preparedRuntimeCallback{
		results: map[string][]ApprovalResult{"late_revoke": {ApprovalAllow}},
		calls:   make(map[string]int),
	}
	middleware := NewApprovalMiddleware(preparedRuntimeAskClassifier{}, NewInMemoryApprovalStore(),
		WithCallback(callback),
		WithDispatchFence(fence),
		WithPlanModeController(core.NewDefaultPlanModeController(fence)),
	)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: target.Name(), Args: map[string]any{"value": "x"}})
	require.NoError(t, err)
	dispatchDone := make(chan struct {
		result *tools.ToolResult
		err    error
	}, 1)
	go func() {
		result, dispatchErr := invocation.Dispatch(ctx)
		dispatchDone <- struct {
			result *tools.ToolResult
			err    error
		}{result: result, err: dispatchErr}
	}()
	select {
	case <-target.started:
	case <-time.After(3 * time.Second):
		releaseExecutor()
		t.Fatal("project executor did not start after the final trust commit")
	}
	assert.True(t, invocation.DispatchCommitted())
	require.NoError(t, second.RevokeTrust(ctx, snapshot.CanonicalRoot()))
	releaseExecutor()
	select {
	case completed := <-dispatchDone:
		require.NoError(t, completed.err, "a revoke after commit cannot reclassify an active execution")
		require.NotNil(t, completed.result)
		assert.Equal(t, "completed", completed.result.Output)
	case <-time.After(3 * time.Second):
		t.Fatal("committed project call did not finish after late revoke")
	}
	assert.True(t, invocation.DispatchCommitted())
	assert.Equal(t, 1, target.callCount())
}

func (t *preparedRuntimeTool) Name() string        { return t.name }
func (t *preparedRuntimeTool) Description() string { return "prepared runtime test tool" }
func (t *preparedRuntimeTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	output := t.resultOutput
	if output == "" {
		output = t.name
	}
	return &tools.ToolResult{Output: output, IsError: t.resultIsError}, nil
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
	fence, err := tools.DispatchFenceFromRegistry(registry)
	require.NoError(t, err)
	before := fence.Snapshot()
	_, err = registry.PrepareToolCall(ctx, tools.ToolCall{Name: "write_b", Args: map[string]any{"path": "b"}})
	require.ErrorIs(t, err, ErrToolDenied)
	after := fence.Snapshot()
	require.Equal(t, before.Approval, after.Approval, "declining one call does not replace the permission policy")
	require.Equal(t, before.Plan, after.Plan)
	require.Equal(t, before.Decision+1, after.Decision, "every older prepared decision is still invalidated")

	_, err = permitA.Dispatch(ctx)
	require.ErrorIs(t, err, tools.ErrDispatchFenceChanged, "an explicit user denial revokes previously prepared permits")
	assert.Zero(t, toolsByName["write_a"].callCount())
	assert.Zero(t, toolsByName["write_b"].callCount())
}

func TestPreparedApprovalAllowOnceAsksAgainForIdenticalCall(t *testing.T) {
	ctx := context.Background()
	callback := &preparedRuntimeCallback{
		results: map[string][]ApprovalResult{"target": {ApprovalAllow, ApprovalAllow}},
		calls:   make(map[string]int),
	}
	registry, _, _, _, toolsByName := newPreparedRuntimeFixture(t, callback, nil, "target")
	call := tools.ToolCall{Name: "target", Args: map[string]any{"path": "same-file"}}

	for attempt := 1; attempt <= 2; attempt++ {
		invocation, err := registry.PrepareToolCall(ctx, call)
		require.NoError(t, err)
		require.NotNil(t, invocation)
		_, err = invocation.Dispatch(ctx)
		require.NoError(t, err)
		assert.Equal(t, attempt, callback.count("target"), "ApprovalAllow is a one-call decision, not a reusable grant")
		assert.Equal(t, attempt, toolsByName["target"].callCount())
	}
}

func TestApprovalAuditRecordsFinalChoice(t *testing.T) {
	ctx := context.Background()
	dir := privateAuditTestDir(t)
	trail := NewAuditTrail(dir)
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	target := &preparedRuntimeTool{name: "audit_target", resultOutput: "raw-output-secret-sentinel"}
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
		ID:   "call-id-secret-sentinel",
		Name: "audit_target",
		Args: map[string]any{"content": "prompt-secret-sentinel"},
	})
	require.NoError(t, err)
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "raw-output-secret-sentinel", result.Output)
	assert.False(t, result.IsError)
	assert.Equal(t, 1, target.callCount())

	reopened := NewAuditTrail(dir)
	entries, err := reopened.ReadEntries(time.Now())
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, "classifier_proposal", entries[0].Stage)
	assert.Equal(t, "ask", entries[0].Decision)
	assert.Equal(t, "approval_resolution", entries[1].Stage)
	assert.Equal(t, "ask", entries[1].Decision)
	assert.Equal(t, "allow_always", entries[1].UserChoice)
	assert.Equal(t, "user", entries[1].Resolution)
	assert.Equal(t, "tool_outcome", entries[2].Stage)
	assert.Equal(t, "ask", entries[2].Decision)
	assert.Equal(t, "allow_always", entries[2].UserChoice)
	assert.Equal(t, "user", entries[2].Resolution)
	assert.Equal(t, tools.PreparedTerminalErrorSuccess, entries[2].ToolOutcome)
	assert.True(t, entries[2].DispatchCommitted)
	assert.True(t, entries[2].ResultPresent)
	assert.False(t, entries[2].ResultIsError)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, entries[2].ToolCallID)

	raw, err := os.ReadFile(filepath.Join(dir, time.Now().UTC().Format("2006-01-02")+".jsonl"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "prompt-secret-sentinel")
	assert.NotContains(t, string(raw), "call-id-secret-sentinel")
	assert.NotContains(t, string(raw), "raw-output-secret-sentinel")
}

func TestPreparedTerminalAuditFailurePreservesToolResultAndMarksHealth(t *testing.T) {
	ctx := context.Background()
	dir := privateAuditTestDir(t)
	trail := NewAuditTrail(dir)
	syncErr := errors.New("raw-sync-secret-sentinel")
	syncCounter := &auditSyncCounter{}
	trail.openFile = func(path string, flag int, perm os.FileMode) (auditTrailFile, error) {
		file, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
		return &auditNthSyncFailureFile{File: file, counter: syncCounter, failAt: 3, syncErr: syncErr}, nil
	}
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	target := &preparedRuntimeTool{
		name:          "audit_target",
		resultOutput:  "raw-output-secret-sentinel",
		resultIsError: true,
	}
	require.NoError(t, base.Register(ctx, target))
	fence := base.DispatchFence()
	controller := core.NewDefaultPlanModeController(fence)
	classifier := NewAuditClassifier(preparedRuntimeAskClassifier{}, trail, PermissionDefault, "session-1")
	middleware := NewApprovalMiddleware(classifier, NewInMemoryApprovalStore(),
		WithCallback(NewInteractiveApprovalCallback(strings.NewReader("y\n"), io.Discard)),
		WithDispatchFence(fence),
		WithPlanModeController(controller),
	)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{
		ID:   "call-id-secret-sentinel",
		Name: "audit_target",
		Args: map[string]any{"content": "prompt-secret-sentinel"},
	})
	require.NoError(t, err)
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err, "a post-dispatch audit failure cannot rewrite the completed tool result")
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Equal(t, "raw-output-secret-sentinel", result.Output)
	assert.Equal(t, 1, target.callCount(), "audit observer failure must not retry or replay dispatch")

	health := trail.Health()
	assert.True(t, health.Enabled)
	assert.False(t, health.Healthy)
	assert.Equal(t, AuditTrailErrorClassSyncFailed, health.ErrorClass)
	assert.Equal(t, health, middleware.AuditHealth(), "the middleware exposes the sticky observer failure independently of Dispatch")
	entries, err := trail.ReadEntries(time.Now())
	require.NoError(t, err)
	require.Len(t, entries, 2, "the failed outcome append is rolled back while earlier audit stages remain durable")
	assert.Equal(t, "approval_resolution", entries[1].Stage)
	assert.Equal(t, "allow_once", entries[1].UserChoice)

	raw, err := os.ReadFile(filepath.Join(dir, time.Now().UTC().Format("2006-01-02")+".jsonl"))
	require.NoError(t, err)
	for _, secret := range []string{
		"raw-output-secret-sentinel",
		"prompt-secret-sentinel",
		"call-id-secret-sentinel",
		"raw-sync-secret-sentinel",
	} {
		assert.NotContains(t, string(raw), secret)
	}
}

func TestPreparedApprovalAuditWriteFailureStopsDispatch(t *testing.T) {
	ctx := context.Background()
	dir := privateAuditTestDir(t)
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
	dir := privateAuditTestDir(t)
	trail := NewAuditTrail(dir)
	syncErr := errors.New("resolution sync sentinel")
	syncCounter := &auditSyncCounter{}
	trail.openFile = func(path string, flag int, perm os.FileMode) (auditTrailFile, error) {
		file, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
		return &auditNthSyncFailureFile{File: file, counter: syncCounter, failAt: 2, syncErr: syncErr}, nil
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

type auditNthSyncFailureFile struct {
	*os.File
	counter *auditSyncCounter
	failAt  int
	syncErr error
}

func (f *auditNthSyncFailureFile) Sync() error {
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
