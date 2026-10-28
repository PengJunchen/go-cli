package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
)

type projectionStatusHandle struct{ status core.RunStatus }

func (h projectionStatusHandle) Snapshot() core.RunStatus { return h.status }

func TestTranscriptProjectionUsesPublishedSourceIDs(t *testing.T) {
	snapshot := validReceiptProjectionSnapshot(t)

	got, err := ProjectPreparedTranscript(context.Background(), snapshot)
	require.NoError(t, err)
	require.Equal(t, "physical-1", got.PhysicalSessionID)
	require.Equal(t, "main", got.BranchID)
	require.Equal(t, []string{"receipt-user", "receipt-assistant", "receipt-tool"}, projectedEntryIDs(got.Rows))
	require.Equal(t, &tui.AssistantOccurrenceKey{RunID: "run-1", CanonicalTranscriptIndex: 1}, got.Rows[1].AssistantOccurrence)
	require.Equal(t, uint64(0), got.Rows[1].Calls[0].Key.AssistantTranscriptOrdinal)
	require.Equal(t, []string{"receipt-intent", "receipt-execution"}, journalEntryIDs(got.Rows[1].Calls[0].JournalFacts))
	require.Equal(t, &tui.ToolOccurrenceKey{RunID: "run-1", AssistantTranscriptOrdinal: 0, ToolCallID: "call-1"}, got.Rows[2].Occurrence)
	require.Nil(t, got.Rows[2].SourceTruncated, "receipt text alone does not prove source truncation metadata")

	legacy := snapshot.Clone()
	legacy.DurableEntries[1].RecordVersion = 0
	legacy.DurableEntries[1].TranscriptIndex = nil
	legacy.DurableEntries[1].RunID = ""
	legacy.DurableEntries[1] = canonicalProjectionFixtureEntry(t, legacy.DurableEntries[1])
	legacyProjection, err := ProjectPreparedTranscript(context.Background(), legacy)
	require.NoError(t, err)
	require.Nil(t, legacyProjection.Rows[1].AssistantOccurrence, "legacy rows without producer coordinates stay uncorrelated")

	badSource := snapshot.Clone()
	badSource.PublishedView.SourceEntryIDs[1] = "missing-source"
	_, err = ProjectPreparedTranscript(context.Background(), badSource)
	require.Error(t, err, "a missing positional source ID must reject the whole projection")

	duplicateSource := snapshot.Clone()
	duplicateSource.PublishedView.SourceEntryIDs[2] = "receipt-assistant"
	_, err = ProjectPreparedTranscript(context.Background(), duplicateSource)
	require.ErrorContains(t, err, "view source entry ID \"receipt-assistant\" is repeated", "a repeated source ID must reject the whole projection")
}

func TestTranscriptProjectionAfterDurableReopenKeepsSourceTruncationUnknown(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	workspaceID := "workspace-source-truncated-reopen"
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	storeDir := filepath.Join(root, "durable")
	prepared, err := session.NewDurableJSONLSessionPreparer(storeDir, index).PrepareNew(ctx, "source truncation reopen")
	require.NoError(t, err)
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			require.NoError(t, prepared.Close())
		}
	})
	sessionID := prepared.View().SessionID
	sink, err := session.NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)

	readDir := filepath.Join(root, "read-input")
	require.NoError(t, os.Mkdir(readDir, 0o700))
	for item := 0; item < 1005; item++ {
		name := filepath.Join(readDir, fmt.Sprintf("entry-%04d.txt", item))
		require.NoError(t, os.WriteFile(name, nil, 0o600))
	}
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(ctx, deferred, tools.WithRegisteredBuiltinWhitelist([]string{"read"})))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("source-truncated-reopen", "source truncation reopen",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "read-truncated-reopen", Name: "read", Args: map[string]any{"path": readDir},
		}}},
		mock.ConversationTurn{AssistantContent: "directory inspected"},
	))
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry))
	harness := core.NewHarnessImpl(core.NewAgentImpl("source-truncated-reopen", loop))
	stream, err := harness.Submit(core.WithRunCommit(ctx, sink, core.RunCommitBinding{
		SessionID: sessionID, WorkspaceID: workspaceID,
	}), "inspect the directory")
	require.NoError(t, err)
	var sourceTruncatedEvent *core.AgentEvent
	for event := range stream.Events() {
		if event.Kind == "tool_result" && event.ToolCallID == "read-truncated-reopen" {
			eventCopy := event
			sourceTruncatedEvent = &eventCopy
		}
	}
	_, err = stream.Result()
	require.NoError(t, err)
	require.NotNil(t, sourceTruncatedEvent)
	require.NotNil(t, sourceTruncatedEvent.SourceTruncated)
	require.True(t, *sourceTruncatedEvent.SourceTruncated, "live event carries the exact read tool's structured truncation fact")

	beforeClose, err := prepared.Snapshot()
	require.NoError(t, err)
	liveProjection, err := ProjectPreparedTranscript(ctx, beforeClose)
	require.NoError(t, err)
	var liveTool *tui.CanonicalTranscriptRow
	for row := range liveProjection.Rows {
		if liveProjection.Rows[row].Kind == tui.TranscriptRowTool {
			liveTool = &liveProjection.Rows[row]
		}
	}
	require.NotNil(t, liveTool)
	require.Nil(t, liveTool.SourceTruncated, "durable projection never upgrades an absent stored fact from live event metadata")

	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(storeDir, index).PrepareResume(ctx, sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	afterRestart, err := reopened.Snapshot()
	require.NoError(t, err)
	reopenedProjection, err := ProjectPreparedTranscript(ctx, afterRestart)
	require.NoError(t, err)
	var reopenedTool *tui.CanonicalTranscriptRow
	for row := range reopenedProjection.Rows {
		if reopenedProjection.Rows[row].Kind == tui.TranscriptRowTool {
			reopenedTool = &reopenedProjection.Rows[row]
		}
	}
	require.NotNil(t, reopenedTool)
	require.Nil(t, reopenedTool.SourceTruncated, "reopened canonical rows remain unknown because this runtime-only fact is not durable schema")
}

func TestTranscriptProjectionKeepsCompactionTyped(t *testing.T) {
	entry := session.SessionEntry{
		ID: "compaction-source", Type: session.EntryTypeCompaction,
		CompactedThroughEntryID: "older-user", Summary: "condensed history", Content: "stored content",
	}
	snapshot := projectionSnapshot([]session.SessionEntry{entry}, []core.AgentMessage{{
		Role: "user", Content: compaction.UntrustedSummaryContext("condensed history"),
	}}, []string{"compaction-source"}, "compaction-source")

	got, err := ProjectPreparedTranscript(context.Background(), snapshot)
	require.NoError(t, err)
	require.Len(t, got.Rows, 1)
	require.Equal(t, tui.TranscriptRowCompaction, got.Rows[0].Kind)
	require.Equal(t, "user", got.Rows[0].Message.Role)
	require.Equal(t, &tui.CompactionFact{CompactedThroughEntryID: "older-user", Summary: "condensed history"}, got.Rows[0].Compaction)
}

func TestTranscriptProjectionRejectsSourceMessageMismatch(t *testing.T) {
	tests := map[string]func(*session.PreparedSessionSnapshot){
		"role": func(snapshot *session.PreparedSessionSnapshot) { snapshot.PublishedView.Messages[2].Role = "assistant" },
		"assistant call ID": func(snapshot *session.PreparedSessionSnapshot) {
			snapshot.PublishedView.Messages[1].ToolCalls[0].ID = "different-call"
		},
		"assistant call name": func(snapshot *session.PreparedSessionSnapshot) {
			snapshot.PublishedView.Messages[1].ToolCalls[0].Name = "different-tool"
		},
		"tool call ID": func(snapshot *session.PreparedSessionSnapshot) {
			snapshot.PublishedView.Messages[2].ToolCallID = "other-call"
		},
		"tool name": func(snapshot *session.PreparedSessionSnapshot) {
			snapshot.PublishedView.Messages[2].ToolName = "other-tool"
		},
		"receipt outcome": func(snapshot *session.PreparedSessionSnapshot) {
			snapshot.PublishedView.Messages[2].ReceiptOutcome = string(core.ReceiptOutcomeError)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snapshot := validReceiptProjectionSnapshot(t)
			mutate(&snapshot)
			_, err := ProjectPreparedTranscript(context.Background(), snapshot)
			require.Error(t, err)
		})
	}

	compactionSnapshot := projectionSnapshot([]session.SessionEntry{{
		ID: "compaction-source", Type: session.EntryTypeCompaction, Summary: "summary",
	}}, []core.AgentMessage{{Role: "user", Content: compaction.UntrustedSummaryContext("summary")}}, []string{"compaction-source"}, "compaction-source")
	got, err := ProjectPreparedTranscript(context.Background(), compactionSnapshot)
	require.NoError(t, err, "compaction is a typed row with its user-role provider representation")
	require.Equal(t, tui.TranscriptRowCompaction, got.Rows[0].Kind)
}

func TestTranscriptProjectionJoinsActionFactsByExactOccurrenceAndEntryID(t *testing.T) {
	entries := []session.SessionEntry{
		{ID: "user-1", Type: session.EntryTypeUser, Content: "run"},
		{ID: "assistant-1", ParentID: "user-1", Type: session.EntryTypeAssistant, RunID: "run-1", AssistantTranscriptOrdinal: uint64Pointer(0),
			ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{ID: "intent-1", ParentID: "assistant-1", Type: session.EntryTypeTool, RunID: "run-1", AssistantTranscriptOrdinal: uint64Pointer(0),
			ActionBatchID: "batch-1", ActionID: "action-1", ActionState: session.ActionStateIntent, ToolCallID: "call-1", ToolName: "read"},
		{ID: "execution-1", ParentID: "intent-1", Type: session.EntryTypeTool, RunID: "run-1", AssistantTranscriptOrdinal: uint64Pointer(0),
			ActionBatchID: "batch-1", ActionID: "action-1", ActionState: session.ActionStateExecution, DispatchState: core.DispatchStateDispatched,
			ResourceState: core.RunExecutionQuiescent, CompletionProof: tools.CompletionInlinePipelineJoined, ToolCallID: "call-1", ToolName: "read"},
		{ID: "assistant-2", ParentID: "execution-1", Type: session.EntryTypeAssistant, RunID: "run-2", AssistantTranscriptOrdinal: uint64Pointer(0),
			ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{ID: "intent-2", ParentID: "assistant-2", Type: session.EntryTypeTool, RunID: "run-2", AssistantTranscriptOrdinal: uint64Pointer(0),
			ActionBatchID: "batch-2", ActionID: "action-2", ActionState: session.ActionStateIntent, ToolCallID: "call-1", ToolName: "read"},
		{ID: "incomplete-intent", ParentID: "intent-2", Type: session.EntryTypeTool, RunID: "run-2", ActionState: session.ActionStateIntent,
			ActionBatchID: "batch-2", ActionID: "action-2", ToolCallID: "call-1", ToolName: "read"},
		{ID: "assistant-3", ParentID: "incomplete-intent", Type: session.EntryTypeAssistant, RunID: "run-1", AssistantTranscriptOrdinal: uint64Pointer(1),
			ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{ID: "intent-3", ParentID: "assistant-3", Type: session.EntryTypeTool, RunID: "run-1", AssistantTranscriptOrdinal: uint64Pointer(1),
			ActionBatchID: "batch-3", ActionID: "action-3", ActionState: session.ActionStateIntent, ToolCallID: "call-1", ToolName: "read"},
		// This exact-looking journal fact belongs to a sibling path and must not join.
		{ID: "sibling-intent", ParentID: "user-1", Type: session.EntryTypeTool, RunID: "run-1", AssistantTranscriptOrdinal: uint64Pointer(0),
			ActionBatchID: "sibling-batch", ActionID: "sibling-action", ActionState: session.ActionStateIntent, ToolCallID: "call-1", ToolName: "read"},
	}
	messages := []core.AgentMessage{
		{Role: "user", Content: "run"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
	}
	snapshot := projectionSnapshot(entries, messages, []string{"user-1", "assistant-1", "assistant-2", "assistant-3"}, "intent-3")

	got, err := ProjectPreparedTranscript(context.Background(), snapshot)
	require.NoError(t, err)
	require.Len(t, got.Rows[1].Calls, 1)
	require.Equal(t, "run-1", got.Rows[1].Calls[0].Key.RunID)
	require.Equal(t, []string{"intent-1", "execution-1"}, journalEntryIDs(got.Rows[1].Calls[0].JournalFacts))
	require.Equal(t, []string{"intent-2"}, journalEntryIDs(got.Rows[2].Calls[0].JournalFacts), "missing occurrence ordinal must not join")
	require.Equal(t, uint64(1), got.Rows[3].Calls[0].Key.AssistantTranscriptOrdinal)
	require.Equal(t, []string{"intent-3"}, journalEntryIDs(got.Rows[3].Calls[0].JournalFacts), "same call ID in another ordinal remains distinct")

	conflictingName := snapshot.Clone()
	for index := range conflictingName.DurableEntries {
		if conflictingName.DurableEntries[index].ID == "intent-1" {
			conflictingName.DurableEntries[index].ToolName = ""
		}
	}
	_, err = ProjectPreparedTranscript(context.Background(), conflictingName)
	require.Error(t, err, "an exact occurrence with a missing tool name is contradictory, not an omitted join")
}

func TestTranscriptOpenJournalDoesNotReplacePublishedView(t *testing.T) {
	ctx := context.Background()
	prepared, err := session.NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "open group projection")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	zero, one := uint64(0), uint64(1)
	user, err := prepared.DurableWriter().AppendAndSync(ctx, session.SessionEntry{
		ID: "open-user", Type: session.EntryTypeUser, RunID: "run-open", TranscriptIndex: &zero, Content: "before tool call",
	})
	require.NoError(t, err)
	ordinal := uint64(0)
	_, err = prepared.DurableWriter().AppendAndSync(ctx,
		session.SessionEntry{ID: "open-assistant", ParentID: user[0].ID, Type: session.EntryTypeAssistant, RunID: "run-open",
			AssistantTranscriptOrdinal: &ordinal, TranscriptIndex: &one, ToolCalls: []llm.ToolCall{{ID: "open-call", Name: "read"}}},
		session.SessionEntry{ID: "open-intent", ParentID: "open-assistant", Type: session.EntryTypeTool, RunID: "run-open", WorkspaceID: "workspace-open",
			ActionBatchID: "batch-open", ActionID: "action-open", OwnerGeneration: "owner-open", ActionState: session.ActionStateIntent,
			AssistantTranscriptOrdinal: &ordinal, ToolCallID: "open-call", ToolName: "read", ArgumentDigest: strings.Repeat("a", 64), SchemaDigest: strings.Repeat("b", 64),
			ToolIdentity: &tools.TrustedToolIdentitySnapshot{RegistrationSource: "builtin", ToolName: "read", RegistrationGeneration: 1,
				ExecutorFingerprint: strings.Repeat("c", 64), ScopeFingerprint: strings.Repeat("d", 64)},
			EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceRead}},
		session.SessionEntry{ID: "open-execution", ParentID: "open-intent", Type: session.EntryTypeTool, RunID: "run-open", WorkspaceID: "workspace-open",
			ActionBatchID: "batch-open", ActionID: "action-open", OwnerGeneration: "owner-open", ActionState: session.ActionStateExecution,
			AssistantTranscriptOrdinal: &ordinal, ToolCallID: "open-call", ToolName: "read", DispatchState: core.DispatchStateDispatched, ResourceState: core.RunExecutionRunning},
	)
	require.NoError(t, err)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.DurableEntries, 4)
	require.Equal(t, []string{"open-user"}, snapshot.PublishedView.SourceEntryIDs)

	got, err := ProjectPreparedTranscript(ctx, snapshot)
	require.NoError(t, err)
	require.Len(t, got.Rows, 1)
	require.Equal(t, []string{"open-user"}, projectedEntryIDs(got.Rows))
}

func TestTranscriptProjectionDefensiveCopyNestedToolArgs(t *testing.T) {
	args := map[string]any{"nested": map[string]any{"items": []any{"original"}}}
	sourceCalls := []llm.ToolCall{{ID: "call-copy", Name: "read", Args: args}}
	viewCalls := core.CloneAgentMessages([]core.AgentMessage{{Role: "assistant", ToolCalls: sourceCalls}})[0].ToolCalls
	snapshot := projectionSnapshot([]session.SessionEntry{{
		ID: "assistant-copy", Type: session.EntryTypeAssistant, RunID: "run-copy", AssistantTranscriptOrdinal: uint64Pointer(0),
		ToolCalls: sourceCalls, Usage: &llm.Usage{InputTokens: 7},
	}}, []core.AgentMessage{{Role: "assistant", ToolCalls: viewCalls, Usage: &llm.Usage{InputTokens: 7}}}, []string{"assistant-copy"}, "assistant-copy")

	projected, err := ProjectPreparedTranscript(context.Background(), snapshot)
	require.NoError(t, err)
	projected.Rows[0].Message.ToolCalls[0].Args.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0] = "projection mutation"
	projected.Rows[0].Message.Usage.InputTokens = 88
	require.Equal(t, "original", snapshot.PublishedView.Messages[0].ToolCalls[0].Args.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0])
	require.Equal(t, "original", snapshot.DurableEntries[0].ToolCalls[0].Args.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0])
	require.Equal(t, 7, snapshot.PublishedView.Messages[0].Usage.InputTokens)

	cloned := projected.Clone()
	cloned.Rows[0].Message.ToolCalls[0].Args.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0] = "clone mutation"
	require.Equal(t, "projection mutation", projected.Rows[0].Message.ToolCalls[0].Args.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0])
}

func TestTranscriptRunObservationRequiresExactTerminalCheckpoint(t *testing.T) {
	snapshot := projectionSnapshot([]session.SessionEntry{
		{ID: "terminal-user", Type: session.EntryTypeUser, Content: "hello"},
		{ID: "terminal-checkpoint", ParentID: "terminal-user", Type: session.EntryTypeCheckpoint, RunID: "run-1",
			CheckpointKind: session.CheckpointRunTerminal, RunOutcome: core.RunOutcomeSuccess},
	}, []core.AgentMessage{{Role: "user", Content: "hello"}}, []string{"terminal-user"}, "terminal-checkpoint")
	handle := projectionStatusHandle{status: core.RunStatus{RunID: "run-1", Outcome: core.RunOutcomeSuccess, ExecutionState: core.RunExecutionQuiescent}}
	got, err := ProjectRunObservation(context.Background(), snapshot, handle, "run-1")
	require.NoError(t, err)
	require.Equal(t, "run-1", got.RunID)
	require.Equal(t, core.RunOutcomeSuccess, got.Outcome)
	require.Equal(t, core.RunExecutionQuiescent, got.ExecutionState)
	require.Equal(t, "terminal-checkpoint", got.TerminalEntryID)
	require.Equal(t, core.RunOutcomeSuccess, got.TerminalOutcome)

	wrongRun := snapshot.Clone()
	wrongRun.DurableEntries[1].RunID = "another-run"
	got, err = ProjectRunObservation(context.Background(), wrongRun, handle, "")
	require.NoError(t, err)
	require.Empty(t, got.TerminalEntryID)

	_, err = ProjectRunObservation(context.Background(), snapshot, handle, "different-result")
	require.Error(t, err, "a supplied result run ID must agree with the exact owner handle")
}

func TestTranscriptRunObservationKeepsOwnerFailureDespiteTerminalRow(t *testing.T) {
	snapshot := projectionSnapshot([]session.SessionEntry{
		{ID: "terminal-user", Type: session.EntryTypeUser, Content: "hello"},
		{ID: "terminal-checkpoint", ParentID: "terminal-user", Type: session.EntryTypeCheckpoint, RunID: "run-unknown",
			CheckpointKind: session.CheckpointRunTerminal, RunOutcome: core.RunOutcomeSuccess},
	}, []core.AgentMessage{{Role: "user", Content: "hello"}}, []string{"terminal-user"}, "terminal-checkpoint")
	handle := projectionStatusHandle{status: core.RunStatus{
		RunID: "run-unknown", Outcome: core.RunOutcomeUnknown, ExecutionState: core.RunExecutionQuarantined, EventGapCount: 2,
	}}
	got, err := ProjectRunObservation(context.Background(), snapshot, handle, "run-unknown")
	require.NoError(t, err)
	require.Equal(t, core.RunOutcomeUnknown, got.Outcome)
	require.Equal(t, core.RunExecutionQuarantined, got.ExecutionState)
	require.Equal(t, uint64(2), got.EventGapCount)
	require.Equal(t, "terminal-checkpoint", got.TerminalEntryID, "checkpoint is reported only as an observed row")
	require.Equal(t, core.RunOutcomeSuccess, got.TerminalOutcome)
}

func TestTranscriptProjectionDoesNotCachePriorSnapshot(t *testing.T) {
	first := projectionSnapshot([]session.SessionEntry{{ID: "first-user", Type: session.EntryTypeUser, Content: "first"}},
		[]core.AgentMessage{{Role: "user", Content: "first"}}, []string{"first-user"}, "first-user")
	firstProjection, err := ProjectPreparedTranscript(context.Background(), first)
	require.NoError(t, err)

	second := projectionSnapshot([]session.SessionEntry{{ID: "second-user", Type: session.EntryTypeUser, Content: "second"}},
		[]core.AgentMessage{{Role: "assistant", Content: "mismatched"}}, []string{"second-user"}, "second-user")
	secondProjection, err := ProjectPreparedTranscript(context.Background(), second)
	require.Error(t, err)
	require.Empty(t, secondProjection.Rows)
	require.Equal(t, []string{"first-user"}, projectedEntryIDs(firstProjection.Rows))
}

func projectionSnapshot(entries []session.SessionEntry, messages []core.AgentMessage, sourceIDs []string, activeLeaf string) session.PreparedSessionSnapshot {
	for index := range entries {
		if entries[index].Seq == 0 {
			entries[index].Seq = uint64(index + 1)
		}
		if entries[index].BranchID == "" {
			entries[index].BranchID = "main"
		}
	}
	return session.PreparedSessionSnapshot{
		SessionID: "physical-1", ActiveBranchID: "main", ActiveLeafEntryID: activeLeaf, MaxSeq: uint64(len(entries)),
		DurableEntries: entries,
		PublishedView: session.SessionView{
			SessionID: "physical-1", BranchID: "main", Revision: "revision-1",
			Messages: core.CloneAgentMessages(messages), SourceEntryIDs: append([]string(nil), sourceIDs...),
		},
	}
}

func validReceiptProjectionSnapshot(t *testing.T) session.PreparedSessionSnapshot {
	t.Helper()
	zero, one, two := uint64(0), uint64(1), uint64(2)
	ordinal := uint64(0)
	entries := []session.SessionEntry{
		{ID: "receipt-user", Type: session.EntryTypeUser, RunID: "run-1", TranscriptIndex: &zero, Content: "read"},
		{ID: "receipt-assistant", ParentID: "receipt-user", Type: session.EntryTypeAssistant, RunID: "run-1",
			AssistantTranscriptOrdinal: &ordinal, TranscriptIndex: &one, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read", Args: map[string]any{"path": "README.md"}}}},
		{ID: "receipt-intent", ParentID: "receipt-assistant", Type: session.EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
			ActionBatchID: "batch-1", ActionID: "action-1", OwnerGeneration: "owner-1", ActionState: session.ActionStateIntent,
			AssistantTranscriptOrdinal: &ordinal, ToolCallID: "call-1", ToolName: "read", ArgumentDigest: strings.Repeat("a", 64), SchemaDigest: strings.Repeat("b", 64),
			ToolIdentity: &tools.TrustedToolIdentitySnapshot{RegistrationSource: "builtin", ToolName: "read", RegistrationGeneration: 1,
				ExecutorFingerprint: strings.Repeat("c", 64), ScopeFingerprint: strings.Repeat("d", 64)},
			EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceRead}},
		{ID: "receipt-execution", ParentID: "receipt-intent", Type: session.EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
			ActionBatchID: "batch-1", ActionID: "action-1", OwnerGeneration: "owner-1", ActionState: session.ActionStateExecution,
			AssistantTranscriptOrdinal: &ordinal, ToolCallID: "call-1", ToolName: "read", DispatchState: core.DispatchStateDispatched,
			ResourceState: core.RunExecutionQuiescent, CompletionProof: tools.CompletionInlinePipelineJoined},
		{ID: "receipt-tool", ParentID: "receipt-execution", Type: session.EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
			ActionBatchID: "batch-1", ActionID: "action-1", OwnerGeneration: "owner-1", AssistantTranscriptOrdinal: &ordinal,
			TranscriptIndex: &two, DispatchState: core.DispatchStateDispatched, ToolCallID: "call-1", ToolName: "read",
			ReceiptOutcome: string(core.ReceiptOutcomeSuccess), Content: "ok"},
	}
	for index := range entries {
		entries[index].RecordVersion = 1
		entries[index].SessionID = "physical-1"
		entries[index].BranchID = "main"
		entries[index].Seq = uint64(index + 1)
		entries[index] = canonicalProjectionFixtureEntry(t, entries[index])
	}
	messages := []core.AgentMessage{
		{Role: "user", Content: "read"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read", Args: map[string]any{"path": "README.md"}}}},
		{Role: "tool", Content: "ok", ToolCallID: "call-1", ToolName: "read", ReceiptOutcome: string(core.ReceiptOutcomeSuccess)},
	}
	return projectionSnapshot(entries, messages, []string{"receipt-user", "receipt-assistant", "receipt-tool"}, "receipt-tool")
}

func uint64Pointer(value uint64) *uint64 { return &value }

func projectedEntryIDs(rows []tui.CanonicalTranscriptRow) []string {
	ids := make([]string, len(rows))
	for index := range rows {
		ids[index] = rows[index].Key.EntryID
	}
	return ids
}

func journalEntryIDs(facts []tui.ActionJournalFact) []string {
	ids := make([]string, len(facts))
	for index := range facts {
		ids[index] = facts[index].EntryID
	}
	return ids
}

// Match the durable writer's canonical payload: Seq and PayloadDigest are
// excluded from the JSON hash. Fixtures still pass the strict snapshot builder;
// changing a row's content later without recomputing this digest remains invalid.
func canonicalProjectionFixtureEntry(t *testing.T, entry session.SessionEntry) session.SessionEntry {
	t.Helper()
	seq := entry.Seq
	entry.Seq, entry.PayloadDigest = 0, ""
	encoded, err := json.Marshal(entry)
	require.NoError(t, err)
	entry.PayloadDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
	entry.Seq = seq
	return entry
}
