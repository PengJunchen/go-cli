//go:build !headless

package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestAppCanonicalTranscriptRendersThroughUpdateAndView(t *testing.T) {
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "session-visible",
		BranchID:          "branch-visible",
		Revision:          "revision-1",
		Rows: []CanonicalTranscriptRow{
			{
				Key:       DurableTranscriptKey{PhysicalSessionID: "session-visible", BranchID: "branch-visible", EntryID: "user-1"},
				Kind:      TranscriptRowUser,
				Message:   core.AgentMessage{Role: "user", Content: "canonical user question"},
				SourceSeq: 1,
			},
			{
				Key:       DurableTranscriptKey{PhysicalSessionID: "session-visible", BranchID: "branch-visible", EntryID: "assistant-1"},
				Kind:      TranscriptRowAssistant,
				Message:   core.AgentMessage{Role: "assistant", Content: "canonical assistant answer"},
				SourceSeq: 2,
			},
		},
	}

	app, err := NewBubbleteaAppWithTranscript(nil, snapshot, WithWidth(80))
	require.NoError(t, err)
	app.model.Update(tea.WindowSizeMsg{Width: 80, Height: 12})

	view := app.model.View()
	require.Contains(t, view, "session-visible")
	require.Contains(t, view, "branch-visible")
	require.Contains(t, view, "Loaded rows: 2")
	require.Contains(t, view, "canonical user question")
	require.Contains(t, view, "canonical assistant answer")
}

func TestAppRestoredUnknownReceiptSurvivesTranscriptRefresh(t *testing.T) {
	occurrence := ToolOccurrenceKey{RunID: "run-restored", AssistantTranscriptOrdinal: 1, ToolCallID: "call-restored"}
	assistantOccurrence := AssistantOccurrenceKey{RunID: "run-restored", CanonicalTranscriptIndex: 1}
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "session-restored",
		BranchID:          "branch-restored",
		Revision:          "revision-restored",
		Rows: []CanonicalTranscriptRow{
			{
				Key:                 DurableTranscriptKey{PhysicalSessionID: "session-restored", BranchID: "branch-restored", EntryID: "assistant-restored"},
				Kind:                TranscriptRowAssistant,
				Message:             core.AgentMessage{Role: "assistant", Content: "read the directory", ToolCalls: []llm.ToolCall{{ID: "call-restored", Name: "read"}}},
				AssistantOccurrence: &assistantOccurrence,
				Calls:               []ToolOccurrence{{Key: occurrence, ToolName: "read"}},
			},
			{
				Key:        DurableTranscriptKey{PhysicalSessionID: "session-restored", BranchID: "branch-restored", EntryID: "tool-restored"},
				Kind:       TranscriptRowTool,
				Message:    core.AgentMessage{Role: "tool", ToolName: "read", ToolCallID: "call-restored", Content: "directory result", ReceiptOutcome: "unknown"},
				Occurrence: &occurrence,
			},
		},
	}
	app, err := NewBubbleteaAppWithTranscript(nil, snapshot, WithWidth(80), WithRunPublicationIdentity("run-restored", "owner-restored"))
	require.NoError(t, err)
	app.model.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	require.Contains(t, app.model.View(), "[receipt: unknown]", "a restored receipt outcome is part of the durable transcript projection")

	refresh := newTranscriptRefreshRequest(1, TranscriptRefresh{
		Snapshot: snapshot.Clone(),
		Observation: RunObservation{
			RunID: "run-restored", OwnerGeneration: "owner-restored",
			Outcome: core.RunOutcomeError, ExecutionState: core.RunExecutionQuiescent,
			TerminalEntryID: "terminal-restored", TerminalOutcome: core.RunOutcomeError,
		},
		Publication: core.DurablePublication{
			State:           core.DurablePublicationSynced,
			RunID:           "run-restored",
			SessionID:       "session-restored",
			OwnerGeneration: "owner-restored",
		},
	})
	applyAndQueueTranscriptRefreshView(t, app.model, refresh)
	require.Contains(t, app.model.View(), "[receipt: unknown]", "same-source refresh must retain the persisted receipt status")
}

func TestAppCanonicalRefreshRetiresMatchedLiveRowsAndOccurrenceCaches(t *testing.T) {
	const runID = "run-live-handoff"
	const sessionID = "session-live-handoff"
	const branchID = "branch-live-handoff"
	index := uint64(2)
	ordinal := uint64(1)
	occurrenceKey := ToolOccurrenceKey{RunID: runID, AssistantTranscriptOrdinal: ordinal, ToolCallID: "call-live-handoff"}
	occurrence := liveToolOccurrence{Key: occurrenceKey, CanonicalTranscriptIndex: index}
	assistantOccurrence := AssistantOccurrenceKey{RunID: runID, CanonicalTranscriptIndex: index}
	app, err := NewBubbleteaAppWithTranscript(nil, CanonicalTranscriptSnapshot{
		PhysicalSessionID: sessionID, BranchID: branchID, Revision: "revision-empty",
	}, WithWidth(80))
	require.NoError(t, err)
	app.model.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "message", ContentType: ContentTypeAssistant, Content: "live assistant",
		Incremental: true, RunID: runID, CanonicalTranscriptIndex: &index,
	}})
	truncated := false
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "tool_call", ContentType: ContentTypeToolCall, Content: "read", ToolCallID: occurrenceKey.ToolCallID,
		RunID: runID, CanonicalTranscriptIndex: &index, AssistantTranscriptOrdinal: &ordinal,
	}})
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "tool_output", ContentType: ContentTypeToolOutput, Content: "live output", ToolCallID: occurrenceKey.ToolCallID,
		RunID: runID, CanonicalTranscriptIndex: &index, AssistantTranscriptOrdinal: &ordinal,
	}})
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "tool_result", ContentType: ContentTypeToolResult, Content: "live output", ToolCallID: occurrenceKey.ToolCallID,
		RunID: runID, CanonicalTranscriptIndex: &index, AssistantTranscriptOrdinal: &ordinal,
		ReceiptOutcome: "unknown", SourceTruncated: &truncated,
	}})
	require.NotEmpty(t, app.model.liveRowOrder)
	require.Contains(t, app.model.liveToolReceipts, occurrence)
	require.Contains(t, app.model.liveToolTruncation, occurrence)

	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: sessionID, BranchID: branchID, Revision: "revision-canonical", ActiveLeafEntryID: "tool-entry",
		Rows: []CanonicalTranscriptRow{
			{
				Key:                 DurableTranscriptKey{PhysicalSessionID: sessionID, BranchID: branchID, EntryID: "assistant-entry"},
				Kind:                TranscriptRowAssistant,
				Message:             core.AgentMessage{Role: "assistant", Content: "live assistant", ToolCalls: []llm.ToolCall{{ID: occurrenceKey.ToolCallID, Name: "read"}}},
				AssistantOccurrence: &assistantOccurrence,
				Calls:               []ToolOccurrence{{Key: occurrenceKey, ToolName: "read"}},
			},
			{
				Key:             DurableTranscriptKey{PhysicalSessionID: sessionID, BranchID: branchID, EntryID: "tool-entry"},
				Kind:            TranscriptRowTool,
				Message:         core.AgentMessage{Role: "tool", ToolName: "read", ToolCallID: occurrenceKey.ToolCallID, Content: "live output", ReceiptOutcome: "unknown"},
				Occurrence:      &occurrenceKey,
				SourceTruncated: &truncated,
			},
		},
	}
	for index := 1; index <= 2; index++ {
		refresh := newTranscriptRefreshRequest(uint64(index), TranscriptRefresh{
			Snapshot: snapshot.Clone(), Publication: core.DurablePublication{State: core.DurablePublicationUnbound},
		})
		applyAndQueueTranscriptRefreshView(t, app.model, refresh)
	}
	require.Empty(t, app.model.liveRowOrder)
	require.Empty(t, app.model.liveTranscriptRows)
	require.Empty(t, app.model.liveAssistantRows)
	require.Empty(t, app.model.liveAssistantRenderers)
	require.Empty(t, app.model.liveToolRows)
	require.Empty(t, app.model.liveToolRenderers)
	require.Empty(t, app.model.liveToolTruncation)
	require.Empty(t, app.model.liveToolReceipts)
	require.Equal(t, 1, len(app.model.toolTranscriptViewport.rows), "repeated canonical handoff must not retain a duplicate live tool projection")
	require.Contains(t, app.model.View(), "[receipt: unknown]")
}

func TestAppCanonicalTranscriptSearchUsesFullLoadedSource(t *testing.T) {
	content := "needle-beyond-preview\n" + strings.Repeat("long source line ", 8_000)
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "session-search",
		BranchID:          "branch-search",
		Revision:          "revision-search",
		Rows: []CanonicalTranscriptRow{{
			Key:     DurableTranscriptKey{PhysicalSessionID: "session-search", BranchID: "branch-search", EntryID: "large-row"},
			Kind:    TranscriptRowAssistant,
			Message: core.AgentMessage{Role: "assistant", Content: content},
		}},
	}
	app, err := NewBubbleteaAppWithTranscript(nil, snapshot, WithWidth(80))
	require.NoError(t, err)
	app.model.Update(tea.WindowSizeMsg{Width: 80, Height: 8})

	app.model.mu.Lock()
	key, found := app.model.searchTranscriptLocked("needle-beyond-preview")
	app.model.mu.Unlock()
	require.True(t, found)
	require.Equal(t, "large-row", key)
	view := app.model.View()
	require.Contains(t, view, "loaded projection only")
	require.Contains(t, view, "[earlier content hidden]")
	app.model.mu.Lock()
	app.model.transcriptViewport.End()
	app.model.mu.Unlock()
	view = app.model.View()
	require.Contains(t, view, "[view truncated]", "a bounded preview must label the hidden prefix")
}

func TestCanonicalTranscriptViewportSharesSourceBodyAcrossDisplayAndSearch(t *testing.T) {
	content := strings.Repeat("R8 shared source line\n", 400) + "R8 source final marker"
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "session-shared-source",
		BranchID:          "branch-shared-source",
		Revision:          "revision-shared-source",
		Rows: []CanonicalTranscriptRow{{
			Key:     DurableTranscriptKey{PhysicalSessionID: "session-shared-source", BranchID: "branch-shared-source", EntryID: "shared-source-row"},
			Kind:    TranscriptRowAssistant,
			Message: core.AgentMessage{Role: "assistant", Content: content},
		}},
	}

	rows := canonicalTranscriptViewportRows(snapshot)
	require.Len(t, rows, 1)
	require.Equal(t, content, rows[0].searchText, "search keeps the entire canonical source body")
	require.Equal(t, "[assistant]", rows[0].text, "display metadata must not concatenate a second body-sized string")
	require.Equal(t, "[assistant]", rows[0].baseText)
	require.Len(t, rows[0].lines, 402)
	require.Equal(t, content, strings.Join(rows[0].lines[1:], "\n"), "display lines retain the exact uncropped source content")

	app, err := NewBubbleteaAppWithTranscript(nil, snapshot, WithWidth(120))
	require.NoError(t, err)
	require.True(t, app.model.transcriptViewport.rowMetadata[0].renderOnDemand)
	require.Empty(t, app.model.transcriptViewport.rowMetadata[0].renderedLines, "canonical offscreen fragments are rendered only on demand")
	app.model.Update(tea.WindowSizeMsg{Width: 120, Height: 200})
	view := app.View()
	require.Contains(t, view, "R8 source final marker")
	app.model.mu.Lock()
	key, found := app.model.searchTranscriptLocked("R8 source final marker")
	app.model.mu.Unlock()
	require.True(t, found)
	require.Equal(t, "shared-source-row", key)
}

func TestCanonicalTranscriptViewportPreservesANSIStateAcrossSourceLines(t *testing.T) {
	content := "\x1b[31mred first line\nstill red on second\x1b[0m"
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "session-ansi-source",
		BranchID:          "branch-ansi-source",
		Revision:          "revision-ansi-source",
		Rows: []CanonicalTranscriptRow{{
			Key:     DurableTranscriptKey{PhysicalSessionID: "session-ansi-source", BranchID: "branch-ansi-source", EntryID: "ansi-source-row"},
			Kind:    TranscriptRowAssistant,
			Message: core.AgentMessage{Role: "assistant", Content: content},
		}},
	}
	rows := canonicalTranscriptViewportRows(snapshot)
	viewport := newTranscriptViewport(80, 0)
	require.NoError(t, viewport.SetRows(rows, "ansi-source"))
	viewport.Resize(80, 8)
	window := viewport.Window()
	displayed := make([]string, 0, len(window.lines))
	for _, line := range window.lines {
		displayed = append(displayed, line.text)
	}
	require.Contains(t, strings.Join(displayed, "\n"), content, "SGR state must survive a source newline")
}

func TestCanonicalTranscriptViewportPreservesInRowAnchorAcrossResize(t *testing.T) {
	contentLines := make([]string, 40)
	for index := range contentLines {
		contentLines[index] = fmt.Sprintf("source-line-%02d", index)
	}
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "session-row-anchor",
		BranchID:          "branch-row-anchor",
		Revision:          "revision-row-anchor",
		Rows: []CanonicalTranscriptRow{{
			Key:     DurableTranscriptKey{PhysicalSessionID: "session-row-anchor", BranchID: "branch-row-anchor", EntryID: "row-anchor"},
			Kind:    TranscriptRowAssistant,
			Message: core.AgentMessage{Role: "assistant", Content: strings.Join(contentLines, "\n")},
		}},
	}
	viewport := newTranscriptViewport(20, 0)
	require.NoError(t, viewport.SetRows(canonicalTranscriptViewportRows(snapshot), "row-anchor"))
	viewport.Resize(20, 5)
	viewport.ScrollLines(-12)
	anchorBeforeResize := viewport.Anchor()
	require.Greater(t, anchorBeforeResize.sourceOffset, len("[assistant]"), "scrolling within a canonical row anchors by body source offset")
	before := viewport.Window()
	require.NotEqual(t, "[assistant]", before.lines[0].text, "scrolling down into the canonical body must leave the row header")

	viewport.Resize(24, 5)
	after := viewport.Window()
	require.Equal(t, anchorBeforeResize.sourceOffset, viewport.Anchor().sourceOffset, "resize preserves the in-row source anchor")
	require.Equal(t, before.lines[0].text, after.lines[0].text, "unchanged-width-independent source lines remain anchored across resize")
}

func TestAppLiveAssistantChunkUpdatesViewportRowThroughUpdate(t *testing.T) {
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "session-live",
		BranchID:          "branch-live",
		Revision:          "revision-live",
	}
	app, err := NewBubbleteaAppWithTranscript(nil, snapshot, WithWidth(80))
	require.NoError(t, err)
	app.model.Update(tea.WindowSizeMsg{Width: 80, Height: 10})

	index := uint64(7)
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type:                     "message",
		ContentType:              ContentTypeAssistant,
		Content:                  "streamed assistant content",
		Incremental:              true,
		RunID:                    "run-live",
		CanonicalTranscriptIndex: &index,
	}})
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type:                     "message",
		ContentType:              ContentTypeAssistant,
		Content:                  " more",
		Incremental:              true,
		RunID:                    "run-live",
		CanonicalTranscriptIndex: &index,
	}})

	view := app.model.View()
	require.Contains(t, view, "streamed assistant content")
	require.Equal(t, uint64(1), app.model.transcriptViewport.updateRowCalls)
	require.Equal(t, uint64(1), app.model.transcriptViewport.appendRowCalls)
	require.Equal(t, uint64(1), app.model.transcriptViewport.setRowsCalls)
}

func TestAppToolResultSourceTruncationUsesExactStructuredOccurrence(t *testing.T) {
	occurrence := ToolOccurrenceKey{RunID: "run-tool", AssistantTranscriptOrdinal: 2, ToolCallID: "call-reused"}
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "session-tool",
		BranchID:          "branch-tool",
		Revision:          "revision-tool",
		Rows: []CanonicalTranscriptRow{
			{
				Key:                 DurableTranscriptKey{PhysicalSessionID: "session-tool", BranchID: "branch-tool", EntryID: "assistant-tool"},
				Kind:                TranscriptRowAssistant,
				Message:             core.AgentMessage{Role: "assistant", Content: "running tool", ToolCalls: []llm.ToolCall{{ID: "call-reused", Name: "bash"}}},
				AssistantOccurrence: &AssistantOccurrenceKey{RunID: "run-tool", CanonicalTranscriptIndex: 9},
				Calls:               []ToolOccurrence{{Key: occurrence, ToolName: "bash"}},
			},
			{
				Key:        DurableTranscriptKey{PhysicalSessionID: "session-tool", BranchID: "branch-tool", EntryID: "tool-result"},
				Kind:       TranscriptRowTool,
				Message:    core.AgentMessage{Role: "tool", ToolName: "bash", ToolCallID: "call-reused", Content: "partial output"},
				Occurrence: &occurrence,
			},
		},
	}
	app, err := NewBubbleteaAppWithTranscript(nil, snapshot, WithWidth(100))
	require.NoError(t, err)
	app.model.Update(tea.WindowSizeMsg{Width: 100, Height: 10})

	truncated := true
	app.model.Update(agentEventMsg{event: AgentEvent{
		ContentType:                ContentTypeToolResult,
		RunID:                      "run-tool",
		CanonicalTranscriptIndex:   uint64PointerTUI(9),
		AssistantTranscriptOrdinal: uint64PointerTUI(2),
		ToolCallID:                 "call-reused",
		ReceiptOutcome:             "success",
		SourceTruncated:            &truncated,
	}})
	require.Contains(t, app.model.View(), "[source output truncated]")
	require.Contains(t, app.model.View(), "[receipt: success]")
	require.Nil(t, app.model.transcriptSnapshot.Rows[1].SourceTruncated, "live metadata must not mutate the canonical snapshot")

	refresh := newTranscriptRefreshRequest(1, TranscriptRefresh{Snapshot: snapshot, Publication: core.DurablePublication{State: core.DurablePublicationUnbound}})
	applyAndQueueTranscriptRefreshView(t, app.model, refresh)
	require.Contains(t, app.model.View(), "[source output truncated]", "the exact live occurrence fact survives same-branch canonical replacement")
}

func applyAndQueueTranscriptRefreshView(t *testing.T, model *teaModel, request *transcriptRefreshRequest) {
	t.Helper()
	_, renderAck := model.Update(transcriptRefreshControlMsg{request: request})
	require.NotNil(t, renderAck)
	_ = model.View()
	viewQueuedMsg := renderAck()
	_, _ = model.Update(viewQueuedMsg)
	require.NoError(t, <-request.ack)
}

func TestAppTranscriptPaneLayoutAcrossSupportedWidths(t *testing.T) {
	snapshot := paneLayoutSnapshot()
	for _, width := range []int{60, 80, 119, 120, 200} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			app, err := NewBubbleteaAppWithTranscript(nil, snapshot, WithWidth(width))
			require.NoError(t, err)
			app.model.Update(tea.WindowSizeMsg{Width: width, Height: 16})
			view := app.model.View()

			require.Equal(t, 3, len(app.model.toolTranscriptViewport.rows))
			if width >= splitWidthThreshold {
				require.Equal(t, 6, len(app.model.transcriptViewport.rows), "wide chat pane excludes the three tool rows")
				require.Contains(t, view, "│", "wide terminals render two independent panes")
			} else {
				require.Equal(t, len(snapshot.Rows), len(app.model.transcriptViewport.rows), "the narrow pane retains tool rows alongside chat rows")
				require.NotContains(t, view, "│")
				require.Contains(t, view, "tool payload marker 2", "narrow terminals keep tool output visible in the single pane")
			}

			app.model.mu.Lock()
			key, found := app.model.searchTranscriptLocked("tool payload marker 2")
			app.model.mu.Unlock()
			require.True(t, found)
			require.Equal(t, fmt.Sprintf("tool-%d", 2), key)
			if width >= splitWidthThreshold {
				require.Same(t, app.model.toolTranscriptViewport, app.model.selectedTranscriptViewportLocked())
			} else {
				require.Same(t, app.model.transcriptViewport, app.model.selectedTranscriptViewportLocked())
			}
		})
	}
}

func TestAppResizePreservesSplitPaneAnchors(t *testing.T) {
	snapshot := paneLayoutSnapshot()
	app, err := NewBubbleteaAppWithTranscript(nil, snapshot, WithWidth(200))
	require.NoError(t, err)
	app.model.Update(tea.WindowSizeMsg{Width: 200, Height: 16})
	_ = app.model.View() // establish the actual pane widths and viewport heights

	require.True(t, app.model.transcriptViewport.Select("assistant-2"))
	require.True(t, app.model.toolTranscriptViewport.Select("tool-2"))
	chatAnchor := app.model.transcriptViewport.Anchor().rowKey
	toolAnchor := app.model.toolTranscriptViewport.Anchor().rowKey
	require.NotEmpty(t, chatAnchor)
	require.NotEmpty(t, toolAnchor)

	app.model.selectedTranscriptKey = "assistant-2"
	app.model.transcriptSearchAfter = "tool-2"
	app.model.transcriptViewport.SelectSearchHit("assistant-2")
	app.model.toolTranscriptViewport.SelectSearchHit("tool-2")
	app.model.steerInput = "continue"
	app.model.steerCursor = len(app.model.steerInput)

	for _, width := range []int{80, 200, 119, 120} {
		app.model.Update(tea.WindowSizeMsg{Width: width, Height: 16})
		_ = app.model.View()
		require.Equal(t, chatAnchor, app.model.transcriptViewport.Anchor().rowKey, "conversation anchor survives each threshold crossing")
		require.Equal(t, toolAnchor, app.model.toolTranscriptViewport.Anchor().rowKey, "tool pane anchor stays independent of the conversation pane")
		require.Equal(t, "assistant-2", app.model.selectedTranscriptKey)
		require.Equal(t, "tool-2", app.model.toolTranscriptViewport.SearchHitKey())
		require.Equal(t, "continue", app.model.steerInput)
		require.Equal(t, len(app.model.steerInput), app.model.steerCursor)
	}
}

func paneLayoutSnapshot() CanonicalTranscriptSnapshot {
	const sessionID = "session-panes"
	const branchID = "branch-panes"
	snapshot := CanonicalTranscriptSnapshot{PhysicalSessionID: sessionID, BranchID: branchID, Revision: "revision-panes"}
	for index := 0; index < 3; index++ {
		assistantIndex := uint64(len(snapshot.Rows) + 1)
		runID := fmt.Sprintf("run-%d", index)
		toolID := fmt.Sprintf("call-%d", index)
		ordinal := uint64(index + 1)
		occurrence := ToolOccurrenceKey{RunID: runID, AssistantTranscriptOrdinal: ordinal, ToolCallID: toolID}
		snapshot.Rows = append(snapshot.Rows,
			CanonicalTranscriptRow{
				Key:       DurableTranscriptKey{PhysicalSessionID: sessionID, BranchID: branchID, EntryID: fmt.Sprintf("user-%d", index)},
				Kind:      TranscriptRowUser,
				Message:   core.AgentMessage{Role: "user", Content: fmt.Sprintf("user turn %d", index)},
				SourceSeq: uint64(len(snapshot.Rows) + 1),
			},
			CanonicalTranscriptRow{
				Key:                 DurableTranscriptKey{PhysicalSessionID: sessionID, BranchID: branchID, EntryID: fmt.Sprintf("assistant-%d", index)},
				Kind:                TranscriptRowAssistant,
				Message:             core.AgentMessage{Role: "assistant", Content: fmt.Sprintf("assistant turn %d", index), ToolCalls: []llm.ToolCall{{ID: toolID, Name: "bash"}}},
				SourceSeq:           uint64(len(snapshot.Rows) + 2),
				AssistantOccurrence: &AssistantOccurrenceKey{RunID: runID, CanonicalTranscriptIndex: assistantIndex},
				Calls:               []ToolOccurrence{{Key: occurrence, ToolName: "bash"}},
			},
			CanonicalTranscriptRow{
				Key:        DurableTranscriptKey{PhysicalSessionID: sessionID, BranchID: branchID, EntryID: fmt.Sprintf("tool-%d", index)},
				Kind:       TranscriptRowTool,
				Message:    core.AgentMessage{Role: "tool", ToolName: "bash", ToolCallID: toolID, Content: fmt.Sprintf("tool payload marker %d", index)},
				SourceSeq:  uint64(len(snapshot.Rows) + 3),
				Occurrence: &occurrence,
			},
		)
	}
	return snapshot
}

func uint64PointerTUI(value uint64) *uint64 { return &value }
