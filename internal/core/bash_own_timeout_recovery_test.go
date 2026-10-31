//go:build darwin || linux

package core_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// This is a business recovery contract, not a claim that timed-out mutations
// rolled back. Real process containment must be proved independently first.
// It intentionally uses exec so no orphan-reaping/PID1 difference is involved.
func TestRunCommitOwnBashTimeoutRetainsPartialOutputAndContinues(t *testing.T) {
	testRunCommitBashVisibleFailure(t, true, false, false, false)
}

func TestRunCommitOrdinaryBashExitRetainsPartialOutputAndContinues(t *testing.T) {
	testRunCommitBashVisibleFailure(t, false, false, false, false)
}

func TestRunCommitLargeBashFailureKeepsCauseAfterHeadTruncation(t *testing.T) {
	testRunCommitBashVisibleFailure(t, false, true, false, false)
}

func TestRunCommitSourceTruncatedBashFailureFitsDurableReceipt(t *testing.T) {
	testRunCommitBashVisibleFailure(t, false, true, true, false)
}

func TestRunCommitParallelFailureKeepsReceiptClippingMetadata(t *testing.T) {
	testRunCommitBashVisibleFailure(t, false, true, false, true)
}

func testRunCommitBashVisibleFailure(t *testing.T, ownTimeout, largePartial, captureOverflow, parallel bool) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("real managed process group contract requires Linux or Darwin")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	const workspaceID = "workspace-own-bash-timeout"
	_, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "own timeout recovery")
	defer prepared.Close()
	checkPath := filepath.Join(root, "read-check.txt")
	pidPath := filepath.Join(root, "bash-leader.pid")
	counterPath := filepath.Join(root, "one-effect.count")
	require.NoError(t, os.WriteFile(checkPath, []byte("read-after-timeout-value\n"), 0o600))

	parent, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	toolWindow := time.Second
	if largePartial {
		toolWindow = 5 * time.Second
	}
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(parent, deferred,
		tools.WithRegisteredBuiltinWhitelist([]string{"bash", "read"}),
		tools.WithRegisteredBashSandbox(tools.AllowAllSandbox{}),
		tools.WithRegisteredBashTimeout(toolWindow),
	))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	// Bash builtins followed by exec: the managed leader becomes sleep itself.
	// The PID file is startup evidence, not a resource-completion proof.
	quotedPID := "'" + strings.ReplaceAll(pidPath, "'", "'\\''") + "'"
	quotedCounter := "'" + strings.ReplaceAll(counterPath, "'", "'\\''") + "'"
	ending, failureMessage := "exec sleep 10", "timed out"
	if !ownTimeout {
		ending, failureMessage = "exit 7", "exit status 7"
		if largePartial {
			ending = "printf '%2048s\\n' {1..70}; exit 7"
			if captureOverflow {
				ending = "printf '%2048s\\n' {1..600}; exit 7"
			}
		}
	}
	command := fmt.Sprintf("printf 'effect\\n' >> %s; printf 'timeout-sentinel\\n'; printf '%%s' \"$$\" > %s; %s", quotedCounter, quotedPID, ending)
	initialCalls := []mock.ExpectedToolCall{{ID: "timeout-bash", Name: "bash", Args: map[string]any{"command": command}}}
	if parallel {
		initialCalls = append(initialCalls, mock.ExpectedToolCall{ID: "parallel-check-read", Name: "read", Args: map[string]any{"path": checkPath}})
	}
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("own-timeout", "own timeout recovery",
		mock.ConversationTurn{AssistantToolCalls: initialCalls},
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "after-timeout-read", Name: "read", Args: map[string]any{"path": checkPath}}}},
		mock.ConversationTurn{AssistantContent: "done after visible tool failure"},
	))
	options := []core.LoopOption{core.WithLLM(model), core.WithTools(registry)}
	if parallel {
		options = append(options, core.WithExecutionMode(core.ExecutionModeParallel))
	}
	agent := core.NewAgentImpl("own-timeout-recovery", core.NewLoopAgent(options...))
	harness := core.NewHarnessImpl(agent, core.WithHarnessEventBus(core.NewMemoryEventBus()))
	stream, err := harness.Submit(core.WithRunCommit(parent, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID}), "run the synthetic bounded command then inspect the check file")
	require.NoError(t, err)
	events := <-collectAgentEvents(stream)
	result, runErr := stream.Result()

	// Check real containment before asserting the desired new recovery semantics.
	// No timeout increase or external kill is used to make this check pass.
	require.NoError(t, parent.Err(), "the caller/run budget must still be alive")
	pidBytes, err := os.ReadFile(pidPath)
	require.NoError(t, err, "startup did not occur: classify as fixture-startup failure, not a recovery semantic RED")
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	require.NoError(t, err)
	require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH)
	require.ErrorIs(t, syscall.Kill(-pid, 0), syscall.ESRCH)
	effect, err := os.ReadFile(counterPath)
	require.NoError(t, err)
	require.Equal(t, "effect\n", string(effect), "exactly one pre-timeout side effect survives; no rollback or automatic replay")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var bashReceipt session.SessionEntry
	var proofRow session.SessionEntry
	var resolution session.SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.ToolCallID == "timeout-bash" && row.ReceiptOutcome != "" {
			bashReceipt = row
		}
		if row.ToolCallID == "timeout-bash" && row.CompletionProof == tools.CompletionOwnedProcessGroupJoined {
			proofRow = row
		}
		if row.CheckpointKind == session.CheckpointExecutionResolution && row.ExecutionResolution != nil && row.ActionID != "" {
			if row.ActionID == bashReceipt.ActionID || row.ActionID == proofRow.ActionID {
				resolution = row
			}
		}
	}
	require.NotEmpty(t, proofRow.ID, "must have actual owned-process-group join proof, not merely a receipt or ESRCH observation")
	require.NotEmpty(t, resolution.ID)
	require.Equal(t, core.ResolutionLocalResourcesStopped, resolution.ExecutionResolution.Evidence)
	require.Equal(t, proofRow.ID, resolution.ExecutionResolution.EvidenceRef)
	require.NotEmpty(t, bashReceipt.ID)
	require.NotEqual(t, core.ReceiptOutcomeSuccess, bashReceipt.ReceiptOutcome)
	require.Equal(t, core.ReceiptOutcomeError, bashReceipt.ReceiptOutcome, "a proved local own-budget failure should be visible to the model; it is neither success nor uncontained execution")
	require.Contains(t, bashReceipt.Content, "timeout-sentinel")
	require.Contains(t, bashReceipt.Content, failureMessage)
	if largePartial {
		guarded, guardErr := production.NewLengthGuard(100000).Check(parent, bashReceipt.Content)
		require.NoError(t, guardErr)
		require.LessOrEqual(t, len(bashReceipt.Content), 100000, "failed receipt is byte-bounded before durable JSON serialization")
		require.True(t, strings.Contains(guarded.Sanitized, failureMessage), "the existing head-only retention policy must keep the failure cause; this is an explicit downstream-retention test, not a claim the default model-response guard processes tool inputs")
		require.True(t, strings.HasPrefix(bashReceipt.Content, "Error:"), "tool wire text must identify failure even without a provider-specific IsError field")
		require.Contains(t, bashReceipt.Content, "[partial output truncated for receipt]")
	}
	require.NoError(t, runErr)
	require.Equal(t, "done after visible tool failure", result.Content)
	require.Equal(t, 3, model.CallCount(), "one Bash, one Read, final done; no tool replay")
	calls := model.CallLog()
	var sawBashResult, sawReadResult bool
	for _, message := range calls[1].Messages {
		if message.ToolCallID == "timeout-bash" {
			require.Contains(t, message.Content, "timeout-sentinel")
			require.Contains(t, message.Content, failureMessage)
			sawBashResult = true
		}
	}
	for _, message := range calls[2].Messages {
		if message.ToolCallID == "after-timeout-read" {
			require.Contains(t, message.Content, "read-after-timeout-value")
			sawReadResult = true
		}
	}
	require.True(t, sawBashResult)
	require.True(t, sawReadResult)
	var receipts int
	for _, event := range events {
		if event.Kind == "tool_result" && event.ToolCallID == "timeout-bash" {
			receipts++
			require.Equal(t, core.ReceiptOutcomeError, event.ReceiptOutcome)
			require.NotNil(t, event.SourceTruncated)
			require.Equal(t, captureOverflow, *event.SourceTruncated, "source capture metadata remains independent of receipt representation clipping")
			require.Equal(t, largePartial, event.ReceiptOutputTruncated, "receipt clipping has its own metadata and never rewrites source truncation")
		}
	}
	require.Equal(t, 1, receipts)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
	original, err := os.ReadFile(checkPath)
	require.NoError(t, err)
	require.Equal(t, "read-after-timeout-value\n", string(original))
}
