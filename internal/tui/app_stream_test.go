//go:build !headless

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

func TestIncrementalAssistantPreviewIsBoundedAndFinalMessageKeepsFullMarkdown(t *testing.T) {
	const (
		prefix = "FULL-START-MARKER\n"
		suffix = "\nFINAL-TAIL-MARKER"
	)
	source := prefix + strings.Repeat("visible words ", maxStreamingRenderBytes/len("visible words ")+1024) + suffix
	app := NewBubbleteaApp(nil, WithWidth(80))
	app.model.accordion.Add(entryFor(ContentTypeUser, "previous user message", false, ""))

	const chunkBytes = 37
	for start := 0; start < len(source); start += chunkBytes {
		end := min(start+chunkBytes, len(source))
		app.model.Update(agentEventMsg{event: AgentEvent{
			Type:        "message",
			ContentType: ContentTypeAssistant,
			Content:     source[start:end],
			Incremental: true,
		}})
		if start == 0 {
			// The stream row already exists; browsing an earlier row must remain
			// stable while later chunks revise only the live row.
			app.model.accordion.selected = 0
		}
	}

	preview := app.model.View()
	require.Contains(t, preview, "[earlier content hidden]\n", "the truncation notice must occupy its own line")
	require.NotContains(t, preview, "FULL-START-MARKER")
	require.Contains(t, preview, "FINAL-TAIL-MARKER")
	require.Equal(t, 0, app.model.accordion.selected, "stream updates must not move an existing selection anchor")
	require.Equal(t, source, app.model.streamingBuf.String(), "the bounded view must not discard the accumulated source")
	require.NotNil(t, app.model.streamingMD, "assistant chunks must use the incremental renderer")
	require.EqualValues(t, len(source), app.model.streamingMD.stats().inputBytes)
	require.EqualValues(t, len(source), app.model.streamingMD.stats().parserBytes)
	require.LessOrEqual(t, strings.Count(app.model.streamingEntry.Full, "\n")+1, 24)

	app.model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	require.Equal(t, 40, app.model.streamingMD.windowWidth, "resize must reflow only the retained live tail")
	require.Equal(t, 0, app.model.accordion.selected, "resize must preserve the user's existing selection anchor")
	resizedPreview := app.model.View()
	require.Contains(t, resizedPreview, "[earlier content hidden]\n", "the resized truncation notice must occupy its own line")
	require.Contains(t, resizedPreview, "FINAL-TAIL-MARKER")
	require.LessOrEqual(t, strings.Count(app.model.streamingEntry.Full, "\n")+1, 2, "the live preview must fit the terminal body rows")

	app.model.Update(agentEventMsg{event: AgentEvent{
		Type:        "message",
		ContentType: ContentTypeAssistant,
		Content:     source,
	}})
	entries := app.model.accordion.Entries()
	require.Len(t, entries, 2, "the final full-message event must not duplicate the streamed assistant row")
	require.Contains(t, entries[1].Full, "FULL-START-MARKER", "final markdown rendering must retain text hidden during the live preview")
	require.Contains(t, entries[1].Full, "FINAL-TAIL-MARKER")
	require.NotContains(t, entries[1].Full, "[earlier content hidden]")
	require.Empty(t, app.model.streamingBuf.String(), "completed source must be released after its full final render is stored")
}

func TestDoneFinalizesStreamingAssistantWithoutLosingFullText(t *testing.T) {
	source := "# Answer\n" + strings.Repeat("complete markdown source ", 4000)
	app := NewBubbleteaApp(nil, WithWidth(80))
	for start := 0; start < len(source); start += 64 {
		end := min(start+64, len(source))
		app.model.Update(agentEventMsg{event: AgentEvent{
			Type: "message", ContentType: ContentTypeAssistant,
			Content: source[start:end], Incremental: true,
		}})
	}

	app.model.Update(agentEventMsg{event: AgentEvent{Type: "done"}})
	entries := app.model.accordion.Entries()
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Full, "Answer")
	require.Contains(t, entries[0].Full, "complete markdown source")
	require.NotContains(t, entries[0].Full, "[earlier content hidden]")
	require.Nil(t, app.model.streamingEntry)
	require.Empty(t, app.model.streamingBuf.String())

	// The normal producer may follow done with the complete non-incremental
	// answer. It confirms tool calls but must not render a second assistant row.
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "message", ContentType: ContentTypeAssistant, Content: source,
	}})
	require.Len(t, app.model.accordion.Entries(), 1)
}

func TestFinalAssistantMessageDifferentFromStreamIsNotDropped(t *testing.T) {
	app := NewBubbleteaApp(nil, WithWidth(80))
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "message", ContentType: ContentTypeAssistant,
		Content: "provisional answer", Incremental: true,
	}})
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "message", ContentType: ContentTypeAssistant, Content: "corrected final answer",
	}})

	entries := app.model.accordion.Entries()
	require.Len(t, entries, 2)
	require.Contains(t, entries[0].Full, "provisional answer")
	require.Contains(t, entries[1].Full, "corrected final answer")
}

func TestDoneDigestDoesNotSuppressLaterIdenticalAssistantAfterInterveningEvent(t *testing.T) {
	const source = "same answer"
	app := NewBubbleteaApp(nil, WithWidth(80))
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "message", ContentType: ContentTypeAssistant,
		Content: source, Incremental: true,
	}})
	app.model.Update(agentEventMsg{event: AgentEvent{Type: "done", Content: source}})

	// The digest may only suppress the immediate complete-message duplicate
	// associated with this done. Once another event intervenes, an identical
	// later answer is a distinct assistant message and must remain visible.
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "tool_result", ContentType: ContentTypeToolResult,
		Content: "tool finished", ToolCallID: "tool-1",
	}})
	app.model.Update(agentEventMsg{event: AgentEvent{
		Type: "message", ContentType: ContentTypeAssistant, Content: source,
	}})

	entries := app.model.accordion.Entries()
	require.Len(t, entries, 3)
	require.Contains(t, entries[0].Full, source)
	require.Contains(t, entries[1].Full, "tool finished")
	require.Contains(t, entries[2].Full, source)
}
