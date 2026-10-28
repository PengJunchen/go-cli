package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestLegacySessionImportPreservesBranchesAndSource(t *testing.T) {
	source := filepath.Join(t.TempDir(), "old.jsonl")
	rows := []SessionEntry{{ID: "u", Type: EntryTypeUser, Content: "original question"}, {ID: "a", ParentID: "u", Type: EntryTypeAssistant, Content: "one answer"}, {ID: "other", ParentID: "u", Type: EntryTypeAssistant, Content: "other branch"}}
	var raw bytes.Buffer
	for _, row := range rows {
		require.NoError(t, json.NewEncoder(&raw).Encode(row))
	}
	require.NoError(t, os.WriteFile(source, raw.Bytes(), 0600))
	inspection, err := InspectLegacySession(t.Context(), source)
	require.NoError(t, err)
	require.Equal(t, 3, inspection.EntryCount)
	dir := t.TempDir()
	id, err := ImportLegacySession(t.Context(), source, dir, "imported-history")
	require.NoError(t, err)
	require.Equal(t, "imported-history", id)
	unchanged, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, raw.Bytes(), unchanged)
	prepared, err := NewDurableJSONLSessionPreparer(dir, nil).PrepareResume(t.Context(), id)
	require.NoError(t, err)
	defer prepared.Close()
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Equal(t, rows[0].ID, snapshot.DurableEntries[0].ID)
	require.Zero(t, snapshot.DurableEntries[0].RecordVersion)
	require.Contains(t, snapshot.DurableEntries[len(rows)].Content, inspection.SHA256)
	require.Equal(t, []string{"original question", "other branch"}, historyContents(snapshot.PublishedView))
	_, err = ImportLegacySession(t.Context(), source, dir, "imported-history")
	require.Error(t, err)
}

func TestLegacySessionImportRejectsCorruptionAndUnknownEvidence(t *testing.T) {
	for _, contents := range []string{
		"{\"id\":\"u\",\"type\":\"user\"}\n{broken}\n",
		"{\"id\":\"u\",\"type\":\"user\"}\n{\"id\":\"u\",\"type\":\"assistant\"}\n",
		"{\"id\":\"u\",\"type\":\"user\"}",
		"{\"id\":\"u\",\"type\":\"user\",\"unknown_evidence\":true}\n",
		"{\"id\":\"u\",\"type\":\"user\",\"action_id\":\"unknown-action\"}\n",
	} {
		source := filepath.Join(t.TempDir(), "old.jsonl")
		require.NoError(t, os.WriteFile(source, []byte(contents), 0600))
		dir := filepath.Join(t.TempDir(), "absent")
		_, err := ImportLegacySession(t.Context(), source, dir, "")
		require.Error(t, err)
		after, err := os.ReadFile(source)
		require.NoError(t, err)
		require.Equal(t, contents, string(after))
		_, err = os.Stat(dir)
		require.True(t, os.IsNotExist(err))
	}
}

func TestLegacySessionImportClosedToolGroup(t *testing.T) {
	rows := []SessionEntry{{ID: "u", Type: EntryTypeUser, Content: "read"}, {ID: "a", ParentID: "u", Type: EntryTypeAssistant, ToolCalls: []llm.ToolCall{{ID: "call", Name: "read"}}}, {ID: "t", ParentID: "a", Type: EntryTypeTool, ToolCallID: "call", ToolName: "read", Content: "historic result"}, {ID: "done", ParentID: "t", Type: EntryTypeAssistant, Content: "done"}}
	source := filepath.Join(t.TempDir(), "old.jsonl")
	var raw bytes.Buffer
	for _, row := range rows {
		require.NoError(t, json.NewEncoder(&raw).Encode(row))
	}
	require.NoError(t, os.WriteFile(source, raw.Bytes(), 0600))
	dir := t.TempDir()
	id, err := ImportLegacySession(t.Context(), source, dir, "")
	require.NoError(t, err)
	prepared, err := NewDurableJSONLSessionPreparer(dir, nil).PrepareResume(t.Context(), id)
	require.NoError(t, err)
	defer prepared.Close()
	require.Len(t, prepared.View().Messages, 4)
	require.Equal(t, "tool", prepared.View().Messages[2].Role)
}
