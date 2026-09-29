package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type approvalAuditAssemblyTool struct {
	name                  string
	output                string
	auditDir              string
	auditFile             string
	widenAuditPermissions bool
	calls                 int
}

func (t *approvalAuditAssemblyTool) Name() string        { return t.name }
func (t *approvalAuditAssemblyTool) Description() string { return "approval audit assembly test tool" }
func (t *approvalAuditAssemblyTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	t.calls++
	if t.widenAuditPermissions {
		if err := os.Chmod(t.auditFile, 0o644); err != nil {
			return nil, err
		}
		if err := os.Chmod(t.auditDir, 0o755); err != nil {
			_ = os.Chmod(t.auditFile, 0o600)
			return nil, err
		}
	}
	return &tools.ToolResult{Output: t.output}, nil
}

func assembleApprovalAuditTestTool(t *testing.T, tool *approvalAuditAssemblyTool) (*AgentAssembly, string) {
	t.Helper()
	ctx := context.Background()
	cfg := newAssembleTestConfig(t)
	assembly, err := AssembleAgent(ctx, cfg, cfg.Provider.Name, "approval-audit-assembly", io.Discard,
		WithApproveMode(ApproveAuto),
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	auditDir := filepath.Join(cfg.Git.WorkDir, ".go-cli", "audit")
	tool.auditDir = auditDir
	tool.auditFile = filepath.Join(auditDir, time.Now().UTC().Format("2006-01-02")+".jsonl")
	require.NoError(t, assembly.ToolRegistry.Register(ctx, tool))
	return assembly, auditDir
}

func TestAssembleAgentPersistsPreparedApprovalToolOutcome(t *testing.T) {
	ctx := context.Background()
	tool := &approvalAuditAssemblyTool{
		name:   "assembled_audit_outcome",
		output: "raw-assembled-output-secret-sentinel",
	}
	assembly, auditDir := assembleApprovalAuditTestTool(t, tool)
	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok)

	invocation, err := preparer.PrepareToolCall(ctx, tools.ToolCall{
		ID:   "assembled-call-secret-sentinel",
		Name: tool.name,
		Args: map[string]any{"prompt": "raw-assembled-prompt-secret-sentinel"},
	})
	require.NoError(t, err)
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, tool.output, result.Output)
	assert.Equal(t, 1, tool.calls)
	assert.Equal(t, approval.AuditTrailHealth{Enabled: true, Healthy: true}, assembly.ApprovalAuditHealth())

	entries, err := approval.NewAuditTrail(auditDir).ReadEntries(time.Now())
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, "classifier_proposal", entries[0].Stage)
	assert.Equal(t, "approval_resolution", entries[1].Stage)
	assert.Equal(t, "auto_approve", entries[1].Resolution)
	assert.Equal(t, "tool_outcome", entries[2].Stage)
	assert.Equal(t, "allow", entries[2].Decision)
	assert.Equal(t, "auto_approve", entries[2].Resolution)
	assert.Equal(t, tools.PreparedTerminalErrorSuccess, entries[2].ToolOutcome)
	assert.True(t, entries[2].DispatchCommitted)
	assert.True(t, entries[2].ResultPresent)
	assert.False(t, entries[2].ResultIsError)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, entries[2].ToolCallID)

	data, err := os.ReadFile(filepath.Join(auditDir, time.Now().UTC().Format("2006-01-02")+".jsonl"))
	require.NoError(t, err)
	for _, sentinel := range []string{
		"assembled-call-secret-sentinel",
		"raw-assembled-prompt-secret-sentinel",
		"raw-assembled-output-secret-sentinel",
	} {
		assert.NotContains(t, string(data), sentinel)
	}
}

func TestAssembleAgentObserverFailurePreservesResultAndExposesHealth(t *testing.T) {
	ctx := context.Background()
	tool := &approvalAuditAssemblyTool{
		name:                  "assembled_audit_failure",
		output:                "raw-assembled-output-secret-sentinel",
		widenAuditPermissions: true,
	}
	assembly, auditDir := assembleApprovalAuditTestTool(t, tool)
	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok)

	invocation, err := preparer.PrepareToolCall(ctx, tools.ToolCall{
		ID:   "assembled-call-secret-sentinel",
		Name: tool.name,
		Args: map[string]any{"prompt": "raw-assembled-prompt-secret-sentinel"},
	})
	require.NoError(t, err)
	require.FileExists(t, tool.auditFile, "approval proposal and resolution must be durable before dispatch")

	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err, "a post-dispatch audit append failure must not replace the completed tool result")
	require.NotNil(t, result)
	assert.Equal(t, tool.output, result.Output)
	assert.Equal(t, 1, tool.calls, "observer failure must not replay a dispatched tool")

	assert.Equal(t, approval.AuditTrailHealth{
		Enabled:    true,
		Healthy:    false,
		ErrorClass: approval.AuditTrailErrorClassLockUnavailable,
	}, assembly.ApprovalAuditHealth())

	// Restore the isolated test audit files so the independent reader can verify
	// the original chain and t.TempDir cleanup can remove the directory safely.
	require.NoError(t, os.Chmod(tool.auditFile, 0o600))
	require.NoError(t, os.Chmod(auditDir, 0o700))
	entries, err := approval.NewAuditTrail(auditDir).ReadEntries(time.Now())
	require.NoError(t, err)
	require.Len(t, entries, 2, "the failed outcome append must leave the prior chain intact")
	assert.Equal(t, "classifier_proposal", entries[0].Stage)
	assert.Equal(t, "approval_resolution", entries[1].Stage)
	assert.Equal(t, "auto_approve", entries[1].Resolution)
	data, err := os.ReadFile(tool.auditFile)
	require.NoError(t, err)
	for _, sentinel := range []string{
		"assembled-call-secret-sentinel",
		"raw-assembled-prompt-secret-sentinel",
		"raw-assembled-output-secret-sentinel",
	} {
		assert.NotContains(t, string(data), sentinel)
	}
}
