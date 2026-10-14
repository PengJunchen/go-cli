package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// This fake-provider integration test covers the real durable REPL, prepared
// Bash registration, production DefaultBashSandbox, process identity observer,
// and session journal. The fake response sequence is test evidence only; live
// provider acceptance is tracked separately.
func TestDurableREPLBashPolicyRejectionIsKnownAndNextTurnContinues(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)

	workspace := t.TempDir()
	marker := filepath.Join(workspace, "python-must-not-start")
	command := fmt.Sprintf("python3 -c 'from pathlib import Path; Path(%q).write_text(\"started\")'", marker)
	arguments, err := json.Marshal(map[string]string{"command": command})
	require.NoError(t, err)
	toolCallResponse, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "call_python_policy_reject", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": string(arguments)},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	})
	require.NoError(t, err)
	knownErrorResponse, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"index":         0,
		"message":       map[string]any{"role": "assistant", "content": "The denied command did not run; I can continue."},
		"finish_reason": "stop",
	}}})
	require.NoError(t, err)
	secondTurnResponse, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"index":         0,
		"message":       map[string]any{"role": "assistant", "content": "The independent second request completed."},
		"finish_reason": "stop",
	}}})
	require.NoError(t, err)
	server, script := newDurablePromptServer(t, string(toolCallResponse), string(knownErrorResponse), string(secondTurnResponse))
	defer server.Close()

	storeDirectory := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(server.URL, storeDirectory)
	cfg.Git.WorkDir = workspace
	lineEditor := &scriptedLineEditor{results: []lineReadResult{
		{line: "try the blocked interpreter"},
		{line: "send an independent second request"},
		{line: exitCommand},
	}}
	var output bytes.Buffer
	cmd := newInteractiveCmd(nil, &output)
	cmd.lineEditor = lineEditor
	repl := &REPLSession{
		cmd:                  cmd,
		out:                  &output,
		lineEditor:           lineEditor,
		extraAssembleOptions: []AssembleOption{WithApproveMode(ApproveAuto)},
	}
	runCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, repl.Run(runCtx, cfg, []string{"--no-onboarding"}), "a known pre-start sandbox error must not poison the next REPL turn")
	assert.Contains(t, output.String(), "The denied command did not run; I can continue.")
	assert.Contains(t, output.String(), "The independent second request completed.")

	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "the blacklisted interpreter must not create its marker")
	physicalID := physicalSessionIDFromAssembly(t, repl.assembly)
	require.NotEmpty(t, physicalID)
	readback, err := newDurableCLISession(t.Context(), storeDirectory, "policy-rejection-readback", physicalID, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readback.requestClose(context.Background())) })
	snapshot, err := readback.snapshotSelected()
	require.NoError(t, err)

	var runID, actionID string
	var bashIntents, bashExecutions, bashReceipts, localResolutions int
	var modelRequestedCommands []string
	terminalOutcomes := map[string]core.RunOutcome{}
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeCheckpoint && row.CheckpointKind == session.CheckpointRunTerminal {
			terminalOutcomes[row.RunID] = row.RunOutcome
		}
		if row.Type == session.EntryTypeAssistant {
			for _, toolCall := range row.ToolCalls {
				if toolCall.ID != "call_python_policy_reject" {
					continue
				}
				toolArgs, ok := toolCall.Args.(map[string]any)
				require.True(t, ok)
				commandValue, ok := toolArgs["command"].(string)
				require.True(t, ok)
				modelRequestedCommands = append(modelRequestedCommands, commandValue)
			}
		}
		if row.Type != session.EntryTypeTool || row.ToolName != "bash" {
			continue
		}
		switch row.ActionState {
		case session.ActionStateIntent:
			bashIntents++
			runID, actionID = row.RunID, row.ActionID
			require.Equal(t, "call_python_policy_reject", row.ToolCallID)
		case session.ActionStateExecution:
			bashExecutions++
			assert.Equal(t, tools.CompletionInlinePipelineJoined, row.CompletionProof)
			assert.Equal(t, core.RunExecutionQuiescent, row.ResourceState)
			assert.Equal(t, core.DispatchStateDispatched, row.DispatchState)
			assert.Nil(t, row.ExecutionIdentity, "a pre-validation denial must not publish a process identity")
		case "":
			bashReceipts++
			assert.Equal(t, core.ReceiptOutcomeError, row.ReceiptOutcome)
			assert.Contains(t, row.Content, "command blocked")
			assert.Contains(t, row.Content, "python3")
		}
	}
	require.NotEmpty(t, runID)
	require.NotEmpty(t, actionID)
	assert.Equal(t, 1, bashIntents, "a rejected call is never replayed as a second action")
	assert.Equal(t, []string{command}, modelRequestedCommands, "the durable assistant history contains one exact rejected command call")
	assert.Equal(t, 1, bashExecutions)
	assert.Equal(t, 1, bashReceipts)
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeCheckpoint && row.CheckpointKind == session.CheckpointExecutionResolution && row.RunID == runID &&
			row.ExecutionResolution != nil && row.ExecutionResolution.ActionID == actionID && row.ExecutionResolution.Evidence == core.ResolutionLocalResourcesStopped {
			localResolutions++
		}
	}
	assert.Equal(t, 1, localResolutions, "the exact rejected action must resolve from its concrete no-process proof")
	assert.Equal(t, core.RunOutcomeSuccess, terminalOutcomes[runID], "the model handled the known tool error and completed the run")
	assert.Len(t, terminalOutcomes, 2, "both submitted user turns must complete as distinct durable runs")
	assert.Contains(t, messageContents(snapshot), "send an independent second request")
	assert.Contains(t, messageContents(snapshot), "The independent second request completed.")

	var primaryBodies []string
	for _, body := range script.requestBodies() {
		if !strings.Contains(body, "Analyze the following conversation") {
			primaryBodies = append(primaryBodies, body)
		}
	}
	assert.Len(t, primaryBodies, 3, "one request asks for the rejected Bash action, one consumes the known error, and one handles the new user turn")
	if len(primaryBodies) == 3 {
		assert.Contains(t, primaryBodies[0], "try the blocked interpreter")
		assert.Contains(t, primaryBodies[1], "command blocked")
		assert.Contains(t, primaryBodies[2], "send an independent second request")
	}
}
