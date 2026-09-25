package tui

import (
	"testing"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/stretchr/testify/require"
)

func TestCanonicalTranscriptSnapshotCloneIsDeepAndIndependent(t *testing.T) {
	args := map[string]any{"nested": map[string]any{"items": []any{"before"}}}
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "physical",
		BranchID:          "main",
		Rows: []CanonicalTranscriptRow{{
			Key:     DurableTranscriptKey{PhysicalSessionID: "physical", BranchID: "main", EntryID: "assistant-1"},
			Kind:    TranscriptRowAssistant,
			Message: core.AgentMessage{Role: "assistant", Usage: &llm.Usage{InputTokens: 2}, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read", Args: args}}},
			Calls: []ToolOccurrence{{
				Key:      ToolOccurrenceKey{RunID: "run-1", AssistantTranscriptOrdinal: 0, ToolCallID: "call-1"},
				ToolName: "read", JournalFacts: []ActionJournalFact{{EntryID: "intent-1", Stage: ActionJournalIntent}},
			}},
		}},
	}

	clone := snapshot.Clone()
	clone.Rows[0].Message.ToolCalls[0].Args.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0] = "clone-only"
	clone.Rows[0].Message.Usage.InputTokens = 99
	clone.Rows[0].Calls[0].JournalFacts[0].EntryID = "clone-entry"

	require.Equal(t, "before", snapshot.Rows[0].Message.ToolCalls[0].Args.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0])
	require.Equal(t, 2, snapshot.Rows[0].Message.Usage.InputTokens)
	require.Equal(t, "intent-1", snapshot.Rows[0].Calls[0].JournalFacts[0].EntryID)

	snapshot.Rows[0].Message.ToolCalls[0].Args.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0] = "source-only"
	require.Equal(t, "clone-only", clone.Rows[0].Message.ToolCalls[0].Args.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0])
}

func TestCanonicalTranscriptSnapshotValidateRejectsInconsistentRows(t *testing.T) {
	valid := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "physical",
		BranchID:          "main",
		Revision:          "revision-1",
		Rows: []CanonicalTranscriptRow{{
			Key:     DurableTranscriptKey{PhysicalSessionID: "physical", BranchID: "main", EntryID: "user-1"},
			Kind:    TranscriptRowUser,
			Message: core.AgentMessage{Role: "user"},
		}},
	}
	require.NoError(t, valid.Validate())

	tests := map[string]func(*CanonicalTranscriptSnapshot){
		"empty snapshot identity": func(value *CanonicalTranscriptSnapshot) { value.PhysicalSessionID = "" },
		"row identity mismatch":   func(value *CanonicalTranscriptSnapshot) { value.Rows[0].Key.BranchID = "other" },
		"role mismatch":           func(value *CanonicalTranscriptSnapshot) { value.Rows[0].Message.Role = "assistant" },
		"duplicate durable key": func(value *CanonicalTranscriptSnapshot) {
			value.Rows = append(value.Rows, value.Rows[0])
		},
		"compaction lacks fact": func(value *CanonicalTranscriptSnapshot) { value.Rows[0].Kind = TranscriptRowCompaction },
		"invalid kind":          func(value *CanonicalTranscriptSnapshot) { value.Rows[0].Kind = TranscriptRowKind(255) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := valid.Clone()
			mutate(&value)
			require.Error(t, value.Validate())
		})
	}
}
