package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestReadWindowNativeWireEditAndDurableReopen(t *testing.T) {
	configureDurableCLISessionTest(t)
	root := t.TempDir()
	t.Chdir(root)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	path := filepath.Join(root, "fixture.go")
	original := "package fixture\n// before\n// undisclosed\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	server, script := newDurablePromptServer(t,
		durableApprovalToolResponse(t, "window-read", "read", map[string]any{"path": "fixture.go", "offset": 2, "limit": 1}),
		durableApprovalToolResponse(t, "window-edit", "edit", map[string]any{"file_path": "fixture.go", "old_string": "// before\n", "new_string": "// after\n"}),
		`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`,
	)
	defer server.Close()
	store := filepath.Join(root, "history")
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Git.WorkDir = root
	var output bytes.Buffer
	require.NoError(t, newPromptCmd(&output).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "Change before to after in fixture.go."}))
	result, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "package fixture\n// after\n// undisclosed\n", string(result))
	bodies := script.requestBodies()
	require.Len(t, bodies, 3)
	type wireMessage struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
	}
	type wireTool struct {
		Function struct {
			Name       string         `json:"name"`
			Parameters map[string]any `json:"parameters"`
		} `json:"function"`
	}
	var first struct {
		Tools []wireTool `json:"tools"`
	}
	require.NoError(t, json.Unmarshal([]byte(bodies[0]), &first))
	var readSchema map[string]any
	for _, tool := range first.Tools {
		if tool.Function.Name == "read" {
			readSchema = tool.Function.Parameters
		}
	}
	require.NotNil(t, readSchema)
	props, ok := readSchema["properties"].(map[string]any)
	require.True(t, ok)
	for _, name := range []string{"offset", "limit"} {
		field, ok := props[name].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "integer", field["type"])
		require.Equal(t, float64(1), field["minimum"])
		require.Equal(t, float64(2147483647), field["maximum"])
	}
	var second struct {
		Messages []wireMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(bodies[1]), &second))
	var windowContent string
	for _, message := range second.Messages {
		if message.Role == "tool" && message.ToolCallID == "window-read" {
			windowContent = message.Content
		}
	}
	require.Contains(t, windowContent, "not file content")
	require.Contains(t, windowContent, "lines=2-2/3")
	require.True(t, strings.HasSuffix(windowContent, "\n// before\n"))
	require.NotContains(t, windowContent, "package fixture")
	require.NotContains(t, windowContent, "undisclosed")
	id := physicalSessionIDFromOutput(t, output.String())
	reopened, err := newDurableCLISession(t.Context(), store, "inspect", id, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.requestClose(context.Background())) })
	snapshot, err := reopened.snapshotSelected()
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
	var intent, receipt, proof, resolution *session.SessionEntry
	var assistantArguments any
	for i := range snapshot.DurableEntries {
		row := &snapshot.DurableEntries[i]
		for _, call := range row.ToolCalls {
			if call.ID == "window-read" {
				assistantArguments = call.Args
			}
		}
		if row.ToolCallID != "window-read" {
			continue
		}
		switch {
		case row.ActionState == session.ActionStateIntent:
			intent = row
		case row.ActionState == session.ActionStateExecution && row.CompletionProof != "":
			proof = row
		case row.Type == session.EntryTypeTool && row.ActionState == "":
			receipt = row
		case row.CheckpointKind == session.CheckpointExecutionResolution && row.ExecutionResolution != nil:
			resolution = row
		}
	}
	require.NotNil(t, intent)
	require.NotNil(t, intent.ToolIdentity)
	require.Equal(t, tools.RegistrationSourceBuiltin, intent.ToolIdentity.RegistrationSource)
	require.NotEmpty(t, intent.ArgumentDigest)
	normalizedArgs, err := json.Marshal(map[string]any{"path": filepath.Join(cwd, "fixture.go"), "offset": 2, "limit": 1})
	require.NoError(t, err)
	argumentHash := sha256.Sum256(normalizedArgs)
	require.Equal(t, hex.EncodeToString(argumentHash[:]), intent.ArgumentDigest)
	encodedAssistantArgs, err := json.Marshal(assistantArguments)
	require.NoError(t, err)
	require.JSONEq(t, `{"path":"fixture.go","offset":2,"limit":1}`, string(encodedAssistantArgs))
	require.NotEmpty(t, intent.SchemaDigest)
	require.NotNil(t, proof)
	require.Equal(t, tools.CompletionInlinePipelineJoined, proof.CompletionProof)
	require.NotNil(t, receipt)
	require.Equal(t, string(core.ReceiptOutcomeSuccess), receipt.ReceiptOutcome)
	require.Equal(t, windowContent, receipt.Content)
	require.Contains(t, snapshot.PublishedView.SourceEntryIDs, receipt.ID)
	require.NotNil(t, resolution)
	require.Equal(t, intent.ActionID, proof.ActionID)
	require.Equal(t, intent.ActionID, receipt.ActionID)
	require.Equal(t, intent.ActionID, resolution.ExecutionResolution.ActionID)
	require.Equal(t, proof.ID, resolution.ExecutionResolution.EvidenceRef)
}
