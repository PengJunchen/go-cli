package tui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func completionContractSnapshot(proof string) CanonicalTranscriptSnapshot {
	return CanonicalTranscriptSnapshot{PhysicalSessionID: "physical", BranchID: "main", Revision: "revision", Rows: []CanonicalTranscriptRow{{
		Key: DurableTranscriptKey{PhysicalSessionID: "physical", BranchID: "main", EntryID: "assistant"}, Kind: TranscriptRowAssistant,
		Message:             core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call", Name: "actual_tool", Args: map[string]any{}}}},
		AssistantOccurrence: &AssistantOccurrenceKey{RunID: "run"}, Calls: []ToolOccurrence{{
			Key: ToolOccurrenceKey{RunID: "run", ToolCallID: "call"}, ToolName: "actual_tool", JournalFacts: []ActionJournalFact{{
				EntryID: "execution", ActionBatchID: "batch", ActionID: "action", Stage: ActionJournalExecution,
				DispatchState: core.DispatchStateDispatched, ResourceState: core.RunExecutionQuiescent, CompletionProof: proof,
			}},
		}},
	}}}
}

func TestCanonicalTranscriptCompletionProofClosedSet(t *testing.T) {
	for _, proof := range []tools.PreparedCompletionProof{tools.CompletionInlinePipelineJoined, tools.CompletionQueuedJobJoined, tools.CompletionOwnedProcessGroupJoined, tools.CompletionMCPRequestJoined, tools.CompletionWorkerRequestJoined} {
		t.Run(string(proof), func(t *testing.T) {
			require.NoError(t, completionContractSnapshot(string(proof)).Validate())
		})
	}
	for _, proof := range []string{"forged-proof", "none", "pending", "INLINE_PIPELINE_JOINED", "inline_pipeline_joined ", "scoped_search_joined", "local_process_or_prestart_no_process_joined"} {
		t.Run("reject-"+proof, func(t *testing.T) {
			require.Error(t, completionContractSnapshot(proof).Validate())
		})
	}
	t.Run("absence-is-only-observation", func(t *testing.T) {
		snapshot := completionContractSnapshot("")
		snapshot.Rows[0].Calls[0].JournalFacts[0].ResourceState = core.RunExecutionCleanupPending
		require.NoError(t, snapshot.Validate())
		require.Empty(t, snapshot.Rows[0].Calls[0].JournalFacts[0].CompletionProof)
	})
	t.Run("positive-proof-on-intent-is-invalid", func(t *testing.T) {
		snapshot := completionContractSnapshot(string(tools.CompletionWorkerRequestJoined))
		snapshot.Rows[0].Calls[0].JournalFacts[0].Stage = ActionJournalIntent
		require.Error(t, snapshot.Validate())
	})
}
