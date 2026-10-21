//go:build !headless

package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestCanonicalToolProgressTracksExactOccurrences(t *testing.T) {
	const runID = "run-progress"
	calls := []ToolOccurrenceKey{
		{RunID: runID, AssistantTranscriptOrdinal: 4, ToolCallID: "call-a"},
		{RunID: runID, AssistantTranscriptOrdinal: 4, ToolCallID: "call-b"},
	}
	app := newCanonicalToolProgressTestApp(t, calls...)
	indexByRun := map[string]uint64{runID: 1}

	require.NotContains(t, app.model.View(), "working...", "assistant ToolCalls are proposals, not evidence that execution started")
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, calls[0], indexByRun[runID])})
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, calls[1], indexByRun[runID])})
	require.Contains(t, app.model.View(), "working...", "an exact canonical tool_call starts the progress indicator")
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolOutput, calls[0], indexByRun[runID])})
	require.Contains(t, app.model.View(), "working...", "tool output is not a completion receipt")

	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolResult, calls[0], indexByRun[runID])})
	require.Contains(t, app.model.View(), "working...", "one receipt must leave the other parallel call active")
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolResult, calls[1], indexByRun[runID])})
	require.NotContains(t, app.model.View(), "working...", "all matching receipts clear the progress indicator")

	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, calls[1], indexByRun[runID])})
	require.NotContains(t, app.model.View(), "working...", "a duplicate call event cannot reopen a completed occurrence")
}

func TestCanonicalToolProgressStartsBeforeDurableSnapshotRefresh(t *testing.T) {
	priorCall := ToolOccurrenceKey{RunID: "run-prior", AssistantTranscriptOrdinal: 1, ToolCallID: "call-prior"}
	app := newCanonicalToolProgressTestApp(t, priorCall)
	const runID = "run-live-before-refresh"
	index := uint64(7)
	ordinal := uint64(3)
	call := ToolOccurrenceKey{RunID: runID, AssistantTranscriptOrdinal: ordinal, ToolCallID: "call-live"}

	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "message", ContentType: ContentTypeAssistant, RunID: runID,
		CanonicalTranscriptIndex: &index, Content: "I will run the requested tool.",
	}})
	require.NotContains(t, app.model.View(), "working...", "assistant tool-call proposals do not imply execution")
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, call, index)})
	require.Contains(t, app.model.View(), "working...", "an exact live tool event starts progress before the durable snapshot catches up")

	wrongOrdinal := call
	wrongOrdinal.AssistantTranscriptOrdinal++
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolResult, wrongOrdinal, index)})
	require.Contains(t, app.model.View(), "working...", "a receipt at a different assistant ordinal cannot clear live progress")
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolResult, call, index)})
	require.NotContains(t, app.model.View(), "working...", "the exact live receipt clears progress")

	// A provider may reuse a call ID in a later assistant occurrence. The full
	// occurrence identity and the new canonical index keep it independent.
	nextIndex := index + 1
	nextOrdinal := ordinal + 1
	reusedCallID := ToolOccurrenceKey{RunID: runID, AssistantTranscriptOrdinal: nextOrdinal, ToolCallID: call.ToolCallID}
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "message", ContentType: ContentTypeAssistant, RunID: runID,
		CanonicalTranscriptIndex: &nextIndex, Content: "I will use the same call ID in a later occurrence.",
	}})
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, reusedCallID, nextIndex)})
	require.Contains(t, app.model.View(), "working...", "a reused call ID at a new ordinal and index remains a distinct live occurrence")
}

func TestCanonicalToolProgressUsesFullOccurrenceAndTranscriptIndex(t *testing.T) {
	callA := ToolOccurrenceKey{RunID: "run-a", AssistantTranscriptOrdinal: 2, ToolCallID: "shared-call-id"}
	callB := ToolOccurrenceKey{RunID: "run-b", AssistantTranscriptOrdinal: 2, ToolCallID: "shared-call-id"}
	app := newCanonicalToolProgressTestApp(t, callA, callB)
	indexByRun := map[string]uint64{"run-a": 1, "run-b": 2}

	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, callA, indexByRun[callA.RunID]+1)})
	require.NotContains(t, app.model.View(), "working...", "a known call paired with the wrong canonical transcript index is not treated as a new call")
	wrongOrdinalCall := callA
	wrongOrdinalCall.AssistantTranscriptOrdinal++
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, wrongOrdinalCall, indexByRun[callA.RunID])})
	require.NotContains(t, app.model.View(), "working...", "a known assistant coordinate rejects a tool call at the wrong ordinal")
	wrongCallID := callA
	wrongCallID.ToolCallID = "unknown-call-id"
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, wrongCallID, indexByRun[callA.RunID])})
	require.NotContains(t, app.model.View(), "working...", "a known assistant coordinate rejects an unknown call ID")
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, callA, indexByRun[callA.RunID])})
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolResult, callA, indexByRun[callA.RunID]+1)})
	require.Contains(t, app.model.View(), "working...", "a matching call ID at the wrong canonical index cannot clear a running occurrence")
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolResult, callB, indexByRun[callB.RunID])})
	require.Contains(t, app.model.View(), "working...", "a same-name/call-ID receipt from another run cannot clear this occurrence")
	wrongOrdinal := callA
	wrongOrdinal.AssistantTranscriptOrdinal--
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolResult, wrongOrdinal, indexByRun[callA.RunID])})
	require.Contains(t, app.model.View(), "working...", "a reused call ID from another assistant occurrence cannot clear this occurrence")

	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolResult, callA, indexByRun[callA.RunID])})
	require.NotContains(t, app.model.View(), "working...", "only the full exact occurrence key clears the indicator")
}

func TestCanonicalToolProgressQuiescentCancellationClosesOnlyItsRun(t *testing.T) {
	callA := ToolOccurrenceKey{RunID: "run-cancelled", AssistantTranscriptOrdinal: 1, ToolCallID: "call-a"}
	callB := ToolOccurrenceKey{RunID: "run-still-running", AssistantTranscriptOrdinal: 1, ToolCallID: "call-b"}
	app := newCanonicalToolProgressTestApp(t, callA, callB)
	indexByRun := map[string]uint64{"run-cancelled": 1, "run-still-running": 2}

	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, callA, indexByRun[callA.RunID])})
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, callB, indexByRun[callB.RunID])})
	require.Contains(t, app.model.View(), "working...")

	refresh := newTranscriptRefreshRequest(1, TranscriptRefresh{
		Snapshot: app.model.transcriptSnapshot.Clone(),
		Observation: RunObservation{
			RunID: "run-cancelled", OwnerGeneration: "owner-cancelled",
			Outcome: core.RunOutcomeCancelled, ExecutionState: core.RunExecutionQuiescent,
			TerminalEntryID: "terminal-cancelled", TerminalOutcome: core.RunOutcomeCancelled,
		},
		Publication: core.DurablePublication{
			State: core.DurablePublicationSynced, RunID: "run-cancelled",
			SessionID: "session-tool-progress", OwnerGeneration: "owner-cancelled",
		},
	})
	applyAndQueueTranscriptRefreshView(t, app.model, refresh)
	require.Contains(t, app.model.View(), "working...", "quiescence clears only the canceled run while another exact run remains active")

	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, callA, indexByRun[callA.RunID])})
	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolResult, callB, indexByRun[callB.RunID])})
	require.NotContains(t, app.model.View(), "working...", "terminal and completed occurrences cannot reopen the indicator")
}

func TestCanonicalToolProgressRejectsEventsOutsideBoundRun(t *testing.T) {
	call := ToolOccurrenceKey{RunID: "stale-run", AssistantTranscriptOrdinal: 1, ToolCallID: "call-stale"}
	app := newCanonicalToolProgressTestApp(t, call)
	app.model.expectedRunID = "current-run"
	incomplete := canonicalToolProgressEvent(ContentTypeToolCall, ToolOccurrenceKey{
		RunID: "current-run", AssistantTranscriptOrdinal: 1, ToolCallID: "call-incomplete",
	}, 1)
	incomplete.AssistantTranscriptOrdinal = nil
	app.model.Update(agentEventMsg{event: incomplete})
	require.NotContains(t, app.model.View(), "working...", "an event without a complete occurrence identity cannot start progress")

	app.model.Update(agentEventMsg{event: canonicalToolProgressEvent(ContentTypeToolCall, call, 1)})
	require.NotContains(t, app.model.View(), "working...", "a stale run cannot reopen progress for the active owner")
}

func newCanonicalToolProgressTestApp(t *testing.T, occurrences ...ToolOccurrenceKey) *BubbleteaApp {
	t.Helper()
	const sessionID = "session-tool-progress"
	const branchID = "branch-tool-progress"
	rows := make([]CanonicalTranscriptRow, 0, len(occurrences))
	indexByRun := make(map[string]uint64)
	for _, occurrence := range occurrences {
		if _, exists := indexByRun[occurrence.RunID]; exists {
			continue
		}
		index := uint64(len(indexByRun) + 1)
		indexByRun[occurrence.RunID] = index
		assistantOccurrence := AssistantOccurrenceKey{RunID: occurrence.RunID, CanonicalTranscriptIndex: index}
		messageCalls := make([]llm.ToolCall, 0)
		callFacts := make([]ToolOccurrence, 0)
		for _, candidate := range occurrences {
			if candidate.RunID != occurrence.RunID {
				continue
			}
			messageCalls = append(messageCalls, llm.ToolCall{ID: candidate.ToolCallID, Name: "read"})
			callFacts = append(callFacts, ToolOccurrence{Key: candidate, ToolName: "read"})
		}
		rows = append(rows, CanonicalTranscriptRow{
			Key:       DurableTranscriptKey{PhysicalSessionID: sessionID, BranchID: branchID, EntryID: "assistant-" + occurrence.RunID},
			Kind:      TranscriptRowAssistant,
			Message:   core.AgentMessage{Role: "assistant", Content: "read fixture", ToolCalls: messageCalls},
			SourceSeq: uint64(len(rows) + 1), AssistantOccurrence: &assistantOccurrence, Calls: callFacts,
		})
	}
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: sessionID,
		BranchID:          branchID,
		Revision:          "revision-tool-progress",
		Rows:              rows,
	}
	app, err := NewBubbleteaAppWithTranscript(nil, snapshot, WithWidth(80))
	require.NoError(t, err)
	app.model.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	return app
}

func canonicalToolProgressEvent(contentType string, occurrence ToolOccurrenceKey, transcriptIndex uint64) AgentEvent {
	index := transcriptIndex
	ordinal := occurrence.AssistantTranscriptOrdinal
	event := AgentEvent{
		Type:                       contentType,
		ContentType:                contentType,
		RunID:                      occurrence.RunID,
		CanonicalTranscriptIndex:   &index,
		AssistantTranscriptOrdinal: &ordinal,
		ToolCallID:                 occurrence.ToolCallID,
	}
	if contentType == ContentTypeToolResult {
		event.ReceiptOutcome = "success"
	}
	return event
}
