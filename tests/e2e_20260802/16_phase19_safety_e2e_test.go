// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 19 safety baseline wiring: approval gate and
// mutation serialization through the MiddlewareToolRegistry decorator.
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// =============================================================================
// Phase 19 E2E: Safety Baseline Wiring
// =============================================================================

// TestE2E_Phase19_ApprovalGateBlocksBash verifies that the prepared approval
// policy denies bash tool calls before the captured executor runs.
func TestE2E_Phase19_ApprovalGateBlocksBash(t *testing.T) {
	inner := &recordingTool{name: "bash"}
	base, wrapped, _ := newPreparedApprovalRuntime(t, approval.NewSafetyPolicyClassifier([]string{"bash"}), false, nil, nil)
	require.NoError(t, base.Register(context.Background(), inner))

	def, err := wrapped.Get(context.Background(), "bash")
	require.NoError(t, err)

	_, execErr := def.Execute(context.Background(), toolCall("bash"))
	require.ErrorIs(t, execErr, approval.ErrToolDenied)
	assert.False(t, inner.executed, "bash must not execute when denied")
	assert.Zero(t, inner.calls.Load(), "denied bash must have no executor side effects")
}

// TestE2E_Phase19_ApprovalAllowsRead verifies that an ordinary read-named
// registration requires an explicit approval even when the compatibility
// classifier's name list marks it as read-only.
func TestE2E_Phase19_ApprovalAllowsRead(t *testing.T) {
	inner := &recordingTool{name: "read"}
	callback := &fixedApprovalCallback{answer: approval.ApprovalAllow}
	base, wrapped, _ := newPreparedApprovalRuntime(t, approval.NewSafetyPolicyClassifier([]string{"bash"}), false, callback, nil)
	require.NoError(t, base.Register(context.Background(), inner))

	def, err := wrapped.Get(context.Background(), "read")
	require.NoError(t, err)

	res, execErr := def.Execute(context.Background(), toolCall("read"))
	require.NoError(t, execErr)
	require.NotNil(t, res)
	assert.Equal(t, "ok:read", res.Output)
	assert.True(t, inner.executed, "read must execute when allowed")
	assert.EqualValues(t, 1, inner.calls.Load())
	assert.EqualValues(t, 1, callback.calls.Load(), "an unknown read-name registration needs an explicit approval")
}

// TestE2E_Phase19_AutoApprovePassesAll verifies that with auto-approve enabled,
// tools that would normally be "Ask" are allowed through.
func TestE2E_Phase19_AutoApprovePassesAll(t *testing.T) {
	inner := &recordingTool{name: "bash"}
	base, wrapped, _ := newPreparedApprovalRuntime(t, approval.NewPlanClassifier(), true, nil, nil)
	require.NoError(t, base.Register(context.Background(), inner))

	def, err := wrapped.Get(context.Background(), "bash")
	require.NoError(t, err)

	res, execErr := def.Execute(context.Background(), toolCall("bash"))
	require.NoError(t, execErr)
	require.NotNil(t, res)
	assert.Equal(t, "ok:bash", res.Output)
	assert.True(t, inner.executed, "auto-approved tool must execute")
	assert.EqualValues(t, 1, inner.calls.Load())
}

// TestE2E_Phase19_MutationQueueSerializes verifies that the production prepared
// mutation queue serializes concurrent writes to the same file path.
func TestE2E_Phase19_MutationQueueSerializes(t *testing.T) {
	var concurrent int32
	var maxConcurrent int32
	queue := tools.NewDefaultFileMutationQueue()
	t.Cleanup(func() { require.NoError(t, queue.(*tools.DefaultFileMutationQueue).Close()) })

	inner := &recordingTool{name: "write"}
	inner.execute = func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		cur := atomic.AddInt32(&concurrent, 1)
		atomicMaxInt32(&maxConcurrent, cur)
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
		return &tools.ToolResult{Output: "ok:write"}, nil
	}

	base, wrapped, _ := newPreparedApprovalRuntime(t, approval.NewPlanClassifier(), true, nil, queue)
	require.NoError(t, base.Register(context.Background(), inner))
	def, err := wrapped.Get(context.Background(), "write")
	require.NoError(t, err)

	var wg sync.WaitGroup
	const calls = 3
	type callResult struct {
		result *tools.ToolResult
		err    error
	}
	results := make([]callResult, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index].result, results[index].err = def.Execute(context.Background(), tools.ToolCall{
				ID:   "write-" + strconv.Itoa(index),
				Name: "write",
				Args: map[string]any{
					"path":    "/tmp/e2e-phase19-serial.txt",
					"content": "x",
				},
			})
		}(i)
	}
	wg.Wait()
	for _, outcome := range results {
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.result)
		assert.Equal(t, "ok:write", outcome.result.Output)
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&maxConcurrent),
		"concurrent writes to the same file must be serialized")
	assert.EqualValues(t, calls, inner.calls.Load(), "each approved mutation must invoke the executor once")
}

// TestE2E_Phase19_CombinedApprovalAndMutation verifies that prepared approval
// completes before the mutation queue invokes the captured executor.
func TestE2E_Phase19_CombinedApprovalAndMutation(t *testing.T) {
	queue := tools.NewDefaultFileMutationQueue()
	t.Cleanup(func() { require.NoError(t, queue.(*tools.DefaultFileMutationQueue).Close()) })
	inner := &recordingTool{name: "write"}
	base, wrapped, _ := newPreparedApprovalRuntime(t, approval.NewSafetyPolicyClassifier([]string{"bash"}), true, nil, queue)
	require.NoError(t, base.Register(context.Background(), inner))

	// An explicit auto-approve resolves the unknown write tool's Ask decision;
	// the prepared queue then serializes the captured executor.
	def, err := wrapped.Get(context.Background(), "write")
	require.NoError(t, err)

	res, err := def.Execute(context.Background(), toolCallWithArgs("write", map[string]any{
		"path":    "/tmp/e2e-phase19-combined.txt",
		"content": "hello",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "ok:write", res.Output)
	assert.True(t, inner.executed)
	assert.EqualValues(t, 1, inner.calls.Load())

	// Verify a forbidden bash registration is denied and has no side effects.
	inner2 := &recordingTool{name: "bash"}
	require.NoError(t, base.Register(context.Background(), inner2))

	def2, err := wrapped.Get(context.Background(), "bash")
	require.NoError(t, err)

	_, err = def2.Execute(context.Background(), toolCall("bash"))
	require.ErrorIs(t, err, approval.ErrToolDenied)
	assert.False(t, inner2.executed, "bash must be denied before reaching mutation layer")
	assert.Zero(t, inner2.calls.Load(), "denied bash must have no executor side effects")
}

func newPreparedApprovalRuntime(
	t *testing.T,
	classifier approval.ApprovalClassifier,
	autoApprove bool,
	callback approval.ApprovalCallback,
	queue tools.FileMutationQueue,
) (*tools.DefaultToolRegistry, *tools.MiddlewareToolRegistry, *core.DefaultPlanModeController) {
	t.Helper()
	base, ok := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	require.True(t, ok, "prepared runtime requires the default registry")
	fence := base.DispatchFence()
	plan := core.NewDefaultPlanModeController(fence)
	options := []approval.Option{
		approval.WithAutoApprove(autoApprove),
		approval.WithDispatchFence(fence),
		approval.WithPlanModeController(plan),
	}
	if callback != nil {
		options = append(options, approval.WithCallback(callback))
	}
	middleware := approval.NewApprovalMiddleware(classifier, approval.NewInMemoryApprovalStore(), options...)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{MutationQueue: queue}))
	return base, registry, plan
}

func registerPreparedPlanControls(t *testing.T, base *tools.DefaultToolRegistry, plan *core.DefaultPlanModeController) {
	t.Helper()
	registrar, ok := any(base).(tools.TrustedControlRegistrar)
	require.True(t, ok, "default registry should expose its narrow trusted-control registration path")
	require.NoError(t, registrar.RegisterTrustedControl(context.Background(), tools.NewEnterPlanModeTool(plan)))
	require.NoError(t, registrar.RegisterTrustedControl(context.Background(), tools.NewExitPlanModeTool(plan)))
}

type fixedApprovalCallback struct {
	answer approval.ApprovalResult
	calls  atomic.Int32
}

func (c *fixedApprovalCallback) RequestApproval(context.Context, string, map[string]any) (approval.ApprovalResult, error) {
	c.calls.Add(1)
	return c.answer, nil
}

func atomicMaxInt32(maximum *int32, candidate int32) {
	for {
		current := atomic.LoadInt32(maximum)
		if candidate <= current || atomic.CompareAndSwapInt32(maximum, current, candidate) {
			return
		}
	}
}

// recordingTool is a test ToolDefinition that records whether Execute was called.
type recordingTool struct {
	name     string
	executed bool
	calls    atomic.Int32
	execute  func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)
}

func (t *recordingTool) Name() string        { return t.name }
func (t *recordingTool) Description() string { return "test " + t.name }
func (t *recordingTool) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	t.calls.Add(1)
	t.executed = true
	if t.execute != nil {
		return t.execute(ctx, call)
	}
	return &tools.ToolResult{Output: "ok:" + call.Name}, nil
}
