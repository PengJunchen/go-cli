//go:build !headless

package tui

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestCanonicalToolOnlyAssistantHidesBodyWithoutLosingReceiptIdentity(t *testing.T) {
	for _, width := range []int{80, 143} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			assistant := AssistantOccurrenceKey{RunID: "tool-only-run", CanonicalTranscriptIndex: 1}
			call := ToolOccurrenceKey{RunID: assistant.RunID, AssistantTranscriptOrdinal: 1, ToolCallID: "read-proposal"}
			key := func(id string) DurableTranscriptKey {
				return DurableTranscriptKey{PhysicalSessionID: "tool-only-session", BranchID: "main", EntryID: id}
			}
			snapshot := CanonicalTranscriptSnapshot{
				PhysicalSessionID: "tool-only-session", BranchID: "main", Revision: "first",
				Rows: []CanonicalTranscriptRow{
					{Key: key("proposal"), Kind: TranscriptRowAssistant,
						Message:             core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: call.ToolCallID, Name: "read"}}},
						AssistantOccurrence: &assistant, Calls: []ToolOccurrence{{Key: call, ToolName: "read"}}},
					{Key: key("receipt"), Kind: TranscriptRowTool,
						Message: core.AgentMessage{Role: "tool", ToolCallID: call.ToolCallID, ToolName: "read", Content: "visible result", ReceiptOutcome: "unknown"}, Occurrence: &call},
					{Key: key("answer"), Kind: TranscriptRowAssistant, Message: core.AgentMessage{Role: "assistant", Content: "visible answer"}},
					{Key: key("empty-result"), Kind: TranscriptRowAssistant, Message: core.AgentMessage{Role: "assistant"}},
				},
			}
			original := snapshot.Clone()
			app, err := NewBubbleteaAppWithTranscript(nil, snapshot, WithWidth(width))
			require.NoError(t, err)
			app.model.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			view := app.View()
			require.Equal(t, 2, strings.Count(view, "[assistant]"), "a tool-only proposal has no body header; a genuine empty result remains visible")
			require.Contains(t, view, "visible answer")
			require.Contains(t, view, "visible result")
			require.Contains(t, view, "[receipt: unknown]")
			require.Contains(t, app.model.canonicalAssistantKeys, assistant)
			occurrence := liveToolOccurrence{Key: call, CanonicalTranscriptIndex: assistant.CanonicalTranscriptIndex}
			require.Equal(t, "receipt", app.model.canonicalToolEntryKeys[occurrence])
			require.Equal(t, original, app.model.transcriptSnapshot, "presentation must not remove canonical proposals or their call coordinates")

			truncated := true
			ordinal := call.AssistantTranscriptOrdinal
			app.model.Update(agentEventMsg{event: AgentEvent{Type: "tool_result", ContentType: ContentTypeToolResult,
				RunID: call.RunID, CanonicalTranscriptIndex: &assistant.CanonicalTranscriptIndex, AssistantTranscriptOrdinal: &ordinal,
				ToolCallID: call.ToolCallID, ReceiptOutcome: "error", SourceTruncated: &truncated}})
			refresh := newTranscriptRefreshRequest(1, TranscriptRefresh{Snapshot: snapshot.Clone(), Publication: core.DurablePublication{State: core.DurablePublicationUnbound}})
			applyAndQueueTranscriptRefreshView(t, app.model, refresh)
			view = app.View()
			require.Contains(t, view, "[receipt: error]")
			require.Contains(t, view, "[source output truncated]")
			require.NotContains(t, view, "[receipt: unknown]")
			require.Equal(t, 2, strings.Count(view, "[assistant]"))
			require.Equal(t, "receipt", app.model.toolTranscriptViewport.rows[0].key, "receipt metadata follows its durable key after a body row is hidden")
		})
	}
}

func TestLiveToolOnlyAssistantHidesConfirmedBodyAndKeepsExecutionReceipt(t *testing.T) {
	app, err := NewBubbleteaAppWithTranscript(nil, CanonicalTranscriptSnapshot{PhysicalSessionID: "live-tool-only", BranchID: "main", Revision: "empty"}, WithWidth(143))
	require.NoError(t, err)
	app.model.Update(tea.WindowSizeMsg{Width: 143, Height: 24})
	index, ordinal := uint64(1), uint64(1)
	message := AgentEvent{Type: "message", ContentType: ContentTypeAssistant, RunID: "live-tool-only-run", CanonicalTranscriptIndex: &index,
		ToolCalls: []ToolCallData{{ID: "live-read", Name: "read"}}}
	message.Incremental = true
	app.model.Update(agentEventMsg{event: message})
	require.Contains(t, app.View(), "[assistant · live]", "an unconfirmed empty stream remains a pending placeholder")
	message.Incremental = false
	app.model.Update(agentEventMsg{event: message})
	require.NotContains(t, app.View(), "[assistant · live]", "confirmed tool-only messages remove only the empty body placeholder")
	require.Empty(t, app.model.liveAssistantRows)

	toolEvent := AgentEvent{Type: "tool_call", ContentType: ContentTypeToolCall, Content: "read", RunID: message.RunID,
		CanonicalTranscriptIndex: &index, AssistantTranscriptOrdinal: &ordinal, ToolCallID: "live-read"}
	app.model.Update(agentEventMsg{event: toolEvent})
	require.Contains(t, app.View(), "[tool live-read]")
	require.True(t, app.model.spinnerActive, "hiding a proposal body does not mark execution complete")
	toolEvent.Type, toolEvent.ContentType = "tool_result", ContentTypeToolResult
	toolEvent.ReceiptOutcome = "unknown"
	app.model.Update(agentEventMsg{event: toolEvent})
	require.Contains(t, app.View(), "[receipt: unknown]")
	require.NotContains(t, app.View(), "[assistant · live]")
}

func TestLiveToolOnlyAssistantDoesNotHideErrorOrEarlierText(t *testing.T) {
	for _, earlierText := range []string{"", "previous streamed text"} {
		app, err := NewBubbleteaAppWithTranscript(nil, CanonicalTranscriptSnapshot{PhysicalSessionID: "visible-live", BranchID: "main", Revision: "empty"}, WithWidth(80))
		require.NoError(t, err)
		app.model.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
		index := uint64(1)
		message := AgentEvent{Type: "message", ContentType: ContentTypeAssistant, RunID: "visible-run", CanonicalTranscriptIndex: &index,
			ToolCalls: []ToolCallData{{ID: "visible-read", Name: "read"}}}
		if earlierText != "" {
			message.Incremental, message.Content = true, earlierText
			app.model.Update(agentEventMsg{event: message})
		}
		message.Incremental, message.Content = false, ""
		message.IsError = earlierText == ""
		app.model.Update(agentEventMsg{event: message})
		require.Contains(t, app.View(), "[assistant · live]")
		if earlierText != "" {
			require.Contains(t, app.View(), earlierText)
		}
	}
}
