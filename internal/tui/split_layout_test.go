package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
)

// TestIsToolEntry verifies that tool_call and tool_result entries are
// classified as tool entries, while all other content types are not.
func TestIsToolEntry(t *testing.T) {
	assert.True(t, isToolEntry(&AccordionEntry{ContentType: ContentTypeToolCall}))
	assert.True(t, isToolEntry(&AccordionEntry{ContentType: ContentTypeToolResult}))
	assert.False(t, isToolEntry(&AccordionEntry{ContentType: ContentTypeAssistant}))
	assert.False(t, isToolEntry(&AccordionEntry{ContentType: ContentTypeUser}))
	assert.False(t, isToolEntry(&AccordionEntry{ContentType: ContentTypeThinking}))
	assert.False(t, isToolEntry(&AccordionEntry{ContentType: ContentTypeStatus}))
}

// TestRenderView_ToolOnly verifies that RenderView with toolOnly=true
// renders only tool_call entries (and their children), excluding
// conversation entries.
func TestRenderView_ToolOnly(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{ContentType: ContentTypeUser, Summary: "hello", Full: "hello"})
	m.Add(&AccordionEntry{
		ContentType: ContentTypeToolCall,
		Summary:     "bash(ls)",
		Full:        "bash(ls)",
		Collapsed:   true,
		ToolCallID:  "tc-1",
	})
	m.Add(&AccordionEntry{
		ContentType: ContentTypeToolResult,
		Summary:     "result",
		Full:        "result",
		Collapsed:   true,
		ToolCallID:  "tc-1",
	})

	out := m.RenderView(80, 0, true)
	assert.Contains(t, out, "bash(ls)")
	assert.NotContains(t, out, "hello")
}

// TestRenderView_ConversationOnly verifies that RenderView with
// toolOnly=false renders only non-tool entries.
func TestRenderView_ConversationOnly(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{ContentType: ContentTypeUser, Summary: "hello", Full: "hello"})
	m.Add(&AccordionEntry{
		ContentType: ContentTypeToolCall,
		Summary:     "bash(ls)",
		Full:        "bash(ls)",
		Collapsed:   true,
	})

	out := m.RenderView(80, 0, false)
	assert.Contains(t, out, "hello")
	assert.NotContains(t, out, "bash(ls)")
}

// TestRenderView_HeightClipping verifies that RenderView clips to the
// last `height` lines when the content exceeds it.
func TestRenderView_HeightClipping(t *testing.T) {
	m := NewAccordionModel()
	for i := 0; i < 10; i++ {
		m.Add(&AccordionEntry{
			ContentType: ContentTypeStatus,
			Summary:     "line" + string(rune('A'+i)),
			Full:        "line" + string(rune('A'+i)),
		})
	}

	out := m.RenderView(80, 3, false)
	lines := strings.Split(out, "\n")
	assert.Len(t, lines, 3)
	// Should keep the last 3 entries (H, I, J).
	assert.Contains(t, out, "lineH")
	assert.Contains(t, out, "lineI")
	assert.Contains(t, out, "lineJ")
	assert.NotContains(t, out, "lineA")
}

// TestRenderView_EmptyPanels verifies that RenderView returns "" when
// no entries match the filter.
func TestRenderView_EmptyPanels(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{ContentType: ContentTypeUser, Summary: "hi", Full: "hi"})

	// No tool entries → toolOnly=true returns "".
	assert.Equal(t, "", m.RenderView(80, 0, true))

	// No entries at all → returns "".
	empty := NewAccordionModel()
	assert.Equal(t, "", empty.RenderView(80, 0, true))
	assert.Equal(t, "", empty.RenderView(80, 0, false))
}

// TestTruncateLine_PlainText verifies basic truncation of plain text.
func TestTruncateLine_PlainText(t *testing.T) {
	// No truncation needed.
	assert.Equal(t, "short", truncateLine("short", 80))
	// Width <= 0 → no truncation.
	assert.Equal(t, "unlimited", truncateLine("unlimited", 0))
	// Truncation needed.
	truncated := truncateLine("0123456789ABCDEF", 10)
	assert.True(t, lipglossWidth(truncated) <= 10)
}

// TestTruncateLine_ANSI verifies that ANSI escape sequences are handled
// correctly — the display width is measured, not the raw byte/rune count.
func TestTruncateLine_ANSI(t *testing.T) {
	// "\x1b[31m" is a red color escape (5 chars, 0 display width).
	// The visible text "hi" is 2 display columns.
	colored := "\x1b[31mhi\x1b[0m"
	// Width 80 is more than enough — no truncation.
	assert.Equal(t, colored, truncateLine(colored, 80))
}

// TestRenderViewLocked_SplitLayout verifies that the split layout is
// activated when width >= 120 and tool entries exist.
func TestRenderViewLocked_SplitLayout(t *testing.T) {
	m := &teaModel{
		width:     140,
		height:    40,
		accordion: NewAccordionModel(),
	}
	m.accordion.Add(&AccordionEntry{
		ContentType: ContentTypeUser,
		Summary:     "user message",
		Full:        "user message",
	})
	m.accordion.Add(&AccordionEntry{
		ContentType: ContentTypeToolCall,
		Summary:     "bash(ls)",
		Full:        "bash(ls)",
		Collapsed:   true,
	})

	out := m.renderViewLocked()
	// Both panels should be present.
	assert.Contains(t, out, "user message")
	assert.Contains(t, out, "bash(ls)")
	// The separator should be present.
	assert.Contains(t, out, "│")
}

// TestRenderViewLocked_SingleColumn_NarrowWidth verifies that the
// single-column layout is used when width < 120.
func TestRenderViewLocked_SingleColumn_NarrowWidth(t *testing.T) {
	m := &teaModel{
		width:     80,
		height:    40,
		accordion: NewAccordionModel(),
	}
	m.accordion.Add(&AccordionEntry{
		ContentType: ContentTypeUser,
		Summary:     "hello",
		Full:        "hello",
	})
	m.accordion.Add(&AccordionEntry{
		ContentType: ContentTypeToolCall,
		Summary:     "bash(ls)",
		Full:        "bash(ls)",
		Collapsed:   true,
	})

	out := m.renderViewLocked()
	// Both entries present, but no separator (single column).
	assert.Contains(t, out, "hello")
	assert.Contains(t, out, "bash(ls)")
	assert.NotContains(t, out, "│")
}

// TestRenderViewLocked_SingleColumn_NoToolEntries verifies that even
// when width >= 120, the layout stays single-column if there are no
// tool entries (conversation gets full width).
func TestRenderViewLocked_SingleColumn_NoToolEntries(t *testing.T) {
	m := &teaModel{
		width:     140,
		height:    40,
		accordion: NewAccordionModel(),
	}
	m.accordion.Add(&AccordionEntry{
		ContentType: ContentTypeUser,
		Summary:     "just conversation",
		Full:        "just conversation",
	})

	out := m.renderViewLocked()
	assert.Contains(t, out, "just conversation")
	// No separator — single column.
	assert.NotContains(t, out, "│")
}

// lipglossWidth is a test helper that returns the display width of a
// string, accounting for ANSI escape sequences.
func lipglossWidth(s string) int {
	return lipgloss.Width(s)
}
