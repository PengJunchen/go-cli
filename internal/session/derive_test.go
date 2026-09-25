package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// TestDeriveMessages_NormalFlow verifies that when all entries are visible (or
// unmarked), DeriveMessages returns them all in order.
func TestDeriveMessages_NormalFlow(t *testing.T) {
	entries := []SessionEntry{
		{ID: "a", Type: EntryTypeUser, Content: "hello", Seq: 1},
		{ID: "b", Type: EntryTypeAssistant, Content: "hi there", Seq: 2},
		{ID: "c", Type: EntryTypeUser, Content: "bye", Seq: 3},
	}

	result := DeriveMessages(entries)

	require.Len(t, result, 3)
	assert.Equal(t, "a", result[0].ID)
	assert.Equal(t, "b", result[1].ID)
	assert.Equal(t, "c", result[2].ID)
	assert.Equal(t, "hello", result[0].Content)
	assert.Equal(t, "hi there", result[1].Content)
	assert.Equal(t, "bye", result[2].Content)
}

// TestDeriveMessages_HiddenFiltered verifies that entries marked
// SurfaceOpHidden are excluded from the derived message stream.
func TestDeriveMessages_HiddenFiltered(t *testing.T) {
	entries := []SessionEntry{
		{ID: "a", Type: EntryTypeUser, Content: "visible", Seq: 1},
		{ID: "b", Type: EntryTypeUser, Content: "hidden", Seq: 2, SurfaceOp: SurfaceOpHidden},
		{ID: "c", Type: EntryTypeAssistant, Content: "response", Seq: 3},
	}

	result := DeriveMessages(entries)

	require.Len(t, result, 2)
	assert.Equal(t, "a", result[0].ID)
	assert.Equal(t, "c", result[1].ID)
}

// TestDeriveMessages_CompactedReplaced verifies that entries before the last
// compaction point are replaced by the compaction summary, and entries marked
// SurfaceOpCompacted are skipped.
func TestDeriveMessages_CompactedReplaced(t *testing.T) {
	entries := []SessionEntry{
		{ID: "old1", Type: EntryTypeUser, Content: "old message 1", Seq: 1, SurfaceOp: SurfaceOpCompacted},
		{ID: "old2", Type: EntryTypeAssistant, Content: "old message 2", Seq: 2, SurfaceOp: SurfaceOpCompacted},
		{ID: "comp", Type: EntryTypeCompaction, Summary: "summarized history", Seq: 3},
		{ID: "new", Type: EntryTypeUser, Content: "new message", Seq: 4},
	}

	result := DeriveMessages(entries)

	require.Len(t, result, 2)
	assert.Equal(t, EntryTypeCompaction, result[0].Type)
	assert.Equal(t, "summarized history", result[0].Content)
	assert.Equal(t, "comp", result[0].ID)
	assert.Equal(t, "new", result[1].ID)
	assert.Equal(t, "new message", result[1].Content)
}

// TestDeriveMessages_EmptyEntries verifies that empty input produces empty
// output.
func TestDeriveMessages_EmptyEntries(t *testing.T) {
	result := DeriveMessages(nil)
	assert.Empty(t, result)

	result = DeriveMessages([]SessionEntry{})
	assert.Empty(t, result)
}

func TestDeriveMessagesReturnsDefensiveCopies(t *testing.T) {
	input := []SessionEntry{{
		ID:   "assistant",
		Type: EntryTypeAssistant,
		ToolCalls: []llm.ToolCall{{
			ID: "provider-call-id", Name: "inspect",
			Args: map[string]any{"nested": []any{map[string]any{"value": "canonical"}}},
		}},
		ContentBlocks: []llm.ContentBlock{{Type: "text", Text: "canonical block"}},
		Usage:         &llm.Usage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
	}}

	derived := DeriveMessages(input)
	require.Len(t, derived, 1)
	derived[0].ToolCalls[0].Args.(map[string]any)["nested"].([]any)[0].(map[string]any)["value"] = "derived mutation"
	derived[0].ContentBlocks[0].Text = "derived mutation"
	derived[0].Usage.InputTokens = 99

	assert.Equal(t, "canonical", input[0].ToolCalls[0].Args.(map[string]any)["nested"].([]any)[0].(map[string]any)["value"])
	assert.Equal(t, "canonical block", input[0].ContentBlocks[0].Text)
	assert.Equal(t, 4, input[0].Usage.InputTokens)
}

// TestDeriveMessages_SeqPreserved verifies that Seq numbers are preserved on
// entries in the derived output.
func TestDeriveMessages_SeqPreserved(t *testing.T) {
	entries := []SessionEntry{
		{ID: "a", Type: EntryTypeUser, Content: "first", Seq: 10},
		{ID: "b", Type: EntryTypeAssistant, Content: "second", Seq: 20},
		{ID: "c", Type: EntryTypeUser, Content: "third", Seq: 30},
	}

	result := DeriveMessages(entries)

	require.Len(t, result, 3)
	assert.Equal(t, uint64(10), result[0].Seq)
	assert.Equal(t, uint64(20), result[1].Seq)
	assert.Equal(t, uint64(30), result[2].Seq)
}

// TestDeriveMessages_CompactedAfterCompactionPoint verifies that entries
// marked SurfaceOpCompacted after the compaction point are also skipped.
func TestDeriveMessages_CompactedAfterCompactionPoint(t *testing.T) {
	entries := []SessionEntry{
		{ID: "a", Type: EntryTypeUser, Content: "visible", Seq: 1},
		{ID: "b", Type: EntryTypeUser, Content: "compacted", Seq: 2, SurfaceOp: SurfaceOpCompacted},
		{ID: "c", Type: EntryTypeAssistant, Content: "visible too", Seq: 3},
	}

	result := DeriveMessages(entries)

	require.Len(t, result, 2)
	assert.Equal(t, "a", result[0].ID)
	assert.Equal(t, "c", result[1].ID)
}

// TestDeriveMessages_SurfaceVisibleMethod verifies the SurfaceVisible method
// behaviour for all SurfaceOp values.
func TestDeriveMessages_SurfaceVisibleMethod(t *testing.T) {
	assert.True(t, SessionEntry{}.SurfaceVisible(), "empty SurfaceOp should be visible")
	assert.True(t, SessionEntry{SurfaceOp: SurfaceOpVisible}.SurfaceVisible())
	assert.False(t, SessionEntry{SurfaceOp: SurfaceOpHidden}.SurfaceVisible())
	assert.False(t, SessionEntry{SurfaceOp: SurfaceOpCompacted}.SurfaceVisible())
}

// TestDeriveAgentMessages verifies that DeriveAgentMessages first applies
// DeriveMessages (filtering hidden/compacted entries) and then converts the
// result to core.AgentMessage via EntriesToAgentMessages (skipping tool and
// compaction entries).
func TestDeriveAgentMessages(t *testing.T) {
	entries := []SessionEntry{
		{ID: "a", Type: EntryTypeUser, Content: "hello", Seq: 1},
		{ID: "b", Type: EntryTypeAssistant, Content: "hi", Seq: 2, ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "bash"}}, ToolCallID: "tc1", ToolName: "bash"},
		{ID: "c", Type: EntryTypeUser, Content: "hidden", Seq: 3, SurfaceOp: SurfaceOpHidden},
		{ID: "d", Type: EntryTypeTool, Content: "tool result", Seq: 4},
		{ID: "e", Type: EntryTypeSystem, Content: "system note", Seq: 5},
	}

	msgs := DeriveAgentMessages(entries)

	// "c" is hidden (filtered by DeriveMessages).
	// "d" is a tool entry (skipped by EntriesToAgentMessages).
	// Remaining: a (user), b (assistant), e (system).
	require.Len(t, msgs, 3)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "hello", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "hi", msgs[1].Content)
	assert.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "tc1", msgs[1].ToolCallID)
	assert.Equal(t, "bash", msgs[1].ToolName)
	assert.Equal(t, "system", msgs[2].Role)
	assert.Equal(t, "system note", msgs[2].Content)
}

// TestDeriveAgentMessages_Empty verifies that DeriveAgentMessages handles
// empty input gracefully.
func TestDeriveAgentMessages_Empty(t *testing.T) {
	msgs := DeriveAgentMessages(nil)
	assert.Empty(t, msgs)
}

func TestSessionViewPreservesSourceEntryIDs(t *testing.T) {
	ordinal := uint64(0)
	assistantIndex := uint64(1)
	receiptIndex := uint64(2)
	workspaceID := "workspace-2"
	entries := []SessionEntry{
		*newR2UserEntry("covered", "", 1, 0),
		{
			ID:                         "assistant-call",
			ParentID:                   "covered",
			Type:                       EntryTypeAssistant,
			Content:                    "running tool",
			ToolCalls:                  []llm.ToolCall{{ID: "provider-call-id", Name: "bash", Args: map[string]any{"cmd": "pwd"}}},
			Usage:                      &llm.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10},
			RecordVersion:              1,
			SessionID:                  "physical-session",
			RunID:                      "run-2",
			WorkspaceID:                workspaceID,
			AssistantTranscriptOrdinal: &ordinal,
			TranscriptIndex:            &assistantIndex,
			Seq:                        2,
		},
		{
			ID:                         "tool-intent",
			ParentID:                   "assistant-call",
			Type:                       EntryTypeTool,
			ToolCallID:                 "provider-call-id",
			ToolName:                   "bash",
			ActionState:                ActionStateIntent,
			ActionBatchID:              "batch-2",
			ActionID:                   "action-2",
			RecordVersion:              1,
			SessionID:                  "physical-session",
			RunID:                      "run-2",
			WorkspaceID:                workspaceID,
			AssistantTranscriptOrdinal: &ordinal,
			ArgumentDigest:             strings.Repeat("a", 64),
			SchemaDigest:               strings.Repeat("b", 64),
			ToolIdentity: &tools.TrustedToolIdentitySnapshot{
				RegistrationSource: "builtin", ToolName: "bash", RegistrationGeneration: 1,
				ExecutorFingerprint: strings.Repeat("c", 64), ScopeFingerprint: strings.Repeat("d", 64),
			},
			EffectSet: []tools.TrustedToolEffect{tools.EffectUnknown},
			Seq:       3,
		},
		{
			ID:                         "tool-receipt",
			ParentID:                   "tool-intent",
			Type:                       EntryTypeTool,
			Content:                    "ok",
			ToolCallID:                 "provider-call-id",
			ToolName:                   "bash",
			ReceiptOutcome:             core.ReceiptOutcomeSuccess,
			DispatchState:              DispatchStateDispatched,
			ActionID:                   "action-2",
			ActionBatchID:              "batch-2",
			RecordVersion:              1,
			SessionID:                  "physical-session",
			RunID:                      "run-2",
			WorkspaceID:                workspaceID,
			AssistantTranscriptOrdinal: &ordinal,
			TranscriptIndex:            &receiptIndex,
			Seq:                        4,
		},
		{
			ID:                      "compaction-marker",
			ParentID:                "tool-receipt",
			Type:                    EntryTypeCompaction,
			Summary:                 "prior tool interaction summary",
			RecordVersion:           1,
			SessionID:               "physical-session",
			CompactedThroughEntryID: "covered",
			Seq:                     5,
		},
	}

	view, err := SessionViewFromEntries("physical-session", "main", "revision-4", entries)
	require.NoError(t, err)
	require.NoError(t, view.Validate())
	require.Equal(t, []string{"compaction-marker", "assistant-call", "tool-receipt"}, view.SourceEntryIDs)
	require.Len(t, view.Messages, 3)
	assert.Equal(t, "user", view.Messages[0].Role)
	assert.Empty(t, view.Messages[0].InputKind)
	assert.Empty(t, view.Messages[0].ToolCalls)
	assert.Empty(t, view.Messages[0].ToolCallID)
	assert.Equal(t, "assistant", view.Messages[1].Role)
	assert.Equal(t, 10, view.Messages[1].Usage.TotalTokens)
	assert.Equal(t, "provider-call-id", view.Messages[1].ToolCalls[0].ID)
	assert.Equal(t, "tool", view.Messages[2].Role)
	assert.Equal(t, "provider-call-id", view.Messages[2].ToolCallID)
	assert.Equal(t, "action-2", entries[2].ActionID, "projection must preserve, not rewrite, action identity")
}
