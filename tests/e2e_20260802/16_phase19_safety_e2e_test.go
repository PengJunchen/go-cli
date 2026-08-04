// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 19 safety baseline wiring: approval gate and
// mutation serialization through the MiddlewareToolRegistry decorator.
package e2e_20260802

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// =============================================================================
// Phase 19 E2E: Safety Baseline Wiring
// =============================================================================

// TestE2E_Phase19_ApprovalGateBlocksBash verifies that the MiddlewareToolRegistry
// decorator with ApprovalMiddleware denies bash tool calls.
func TestE2E_Phase19_ApprovalGateBlocksBash(t *testing.T) {
	inner := &recordingTool{name: "bash"}
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), inner))

	mw := approval.NewApprovalMiddleware(
		approval.NewSafetyPolicyClassifier([]string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(context.Background(), "bash")
	require.NoError(t, err)

	_, execErr := def.Execute(context.Background(), toolCall("bash"))
	require.ErrorIs(t, execErr, approval.ErrToolDenied)
	assert.False(t, inner.executed, "bash must not execute when denied")
}

// TestE2E_Phase19_ApprovalAllowsRead verifies that read tool passes through
// the approval gate when using SafetyPolicyClassifier.
func TestE2E_Phase19_ApprovalAllowsRead(t *testing.T) {
	inner := &recordingTool{name: "read"}
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), inner))

	mw := approval.NewApprovalMiddleware(
		approval.NewSafetyPolicyClassifier([]string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(context.Background(), "read")
	require.NoError(t, err)

	res, execErr := def.Execute(context.Background(), toolCall("read"))
	require.NoError(t, execErr)
	assert.Equal(t, "ok:read", res.Output)
	assert.True(t, inner.executed, "read must execute when allowed")
}

// TestE2E_Phase19_AutoApprovePassesAll verifies that with auto-approve enabled,
// tools that would normally be "Ask" are allowed through.
func TestE2E_Phase19_AutoApprovePassesAll(t *testing.T) {
	inner := &recordingTool{name: "bash"}
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), inner))

	// DenyAll with auto-approve still denies (Deny is final, only Ask is auto-resolved).
	// Use StaticClassifier with no allowlist to get "Ask" equivalent (deny-by-default).
	// Actually, StaticClassifier returns Deny for unknown tools, not Ask.
	// Use PlanClassifier which returns Ask for all tools.
	mw := approval.NewApprovalMiddleware(
		approval.NewPlanClassifier(),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(true),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(context.Background(), "bash")
	require.NoError(t, err)

	res, execErr := def.Execute(context.Background(), toolCall("bash"))
	require.NoError(t, execErr)
	assert.Equal(t, "ok:bash", res.Output)
	assert.True(t, inner.executed, "auto-approved tool must execute")
}

// TestE2E_Phase19_MutationQueueSerializes verifies that the mutation wrapper
// serializes concurrent writes to the same file path.
func TestE2E_Phase19_MutationQueueSerializes(t *testing.T) {
	var concurrent int32
	var maxConcurrent int32

	inner := &recordingTool{name: "write"}
	inner.execute = func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		cur := atomic.AddInt32(&concurrent, 1)
		if cur > atomic.LoadInt32(&maxConcurrent) {
			atomic.StoreInt32(&maxConcurrent, cur)
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
		return &tools.ToolResult{Output: "ok:write"}, nil
	}

	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), inner))

	wrapped := tools.NewMiddlewareToolRegistry(tr, tools.NewMutationWrapper())
	def, err := wrapped.Get(context.Background(), "write")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = def.Execute(context.Background(), toolCallWithArgs("write", map[string]any{
				"path":    "/tmp/e2e-phase19-serial.txt",
				"content": "x",
			}))
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&maxConcurrent),
		"concurrent writes to the same file must be serialized")
}

// TestE2E_Phase19_CombinedApprovalAndMutation verifies that both wrappers
// work together in the decorator chain: approval runs first (outermost),
// mutation second.
func TestE2E_Phase19_CombinedApprovalAndMutation(t *testing.T) {
	inner := &recordingTool{name: "write"}
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), inner))

	mw := approval.NewApprovalMiddleware(
		approval.NewSafetyPolicyClassifier([]string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
	)
	// Order: approval first (outermost), mutation second (innermost).
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall, tools.NewMutationWrapper())

	// write should pass approval (not in forbidden list) and then be serialized.
	def, err := wrapped.Get(context.Background(), "write")
	require.NoError(t, err)

	res, err := def.Execute(context.Background(), toolCallWithArgs("write", map[string]any{
		"path":    "/tmp/e2e-phase19-combined.txt",
		"content": "hello",
	}))
	require.NoError(t, err)
	assert.Equal(t, "ok:write", res.Output)
	assert.True(t, inner.executed)

	// Reset and verify bash is denied even with mutation wrapper present.
	inner2 := &recordingTool{name: "bash"}
	require.NoError(t, tr.Register(context.Background(), inner2))

	def2, err := wrapped.Get(context.Background(), "bash")
	require.NoError(t, err)

	_, err = def2.Execute(context.Background(), toolCall("bash"))
	require.ErrorIs(t, err, approval.ErrToolDenied)
	assert.False(t, inner2.executed, "bash must be denied before reaching mutation layer")
}

// recordingTool is a test ToolDefinition that records whether Execute was called.
type recordingTool struct {
	name     string
	executed bool
	execute  func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)
}

func (t *recordingTool) Name() string        { return t.name }
func (t *recordingTool) Description() string { return "test " + t.name }
func (t *recordingTool) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	t.executed = true
	if t.execute != nil {
		return t.execute(ctx, call)
	}
	return &tools.ToolResult{Output: "ok:" + call.Name}, nil
}
