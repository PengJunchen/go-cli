package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestToolStatusIcon_Pending verifies the pending icon is ⏳.
func TestToolStatusIcon_Pending(t *testing.T) {
	assert.Equal(t, "⏳", toolStatusIcon(ToolStatusPending, 0))
}

// TestToolStatusIcon_Running verifies the running icon is a braille spinner
// frame that changes with the frame index.
func TestToolStatusIcon_Running(t *testing.T) {
	frame0 := toolStatusIcon(ToolStatusRunning, 0)
	frame1 := toolStatusIcon(ToolStatusRunning, 1)
	assert.Equal(t, "⠋", frame0)
	assert.Equal(t, "⠙", frame1)
	assert.NotEqual(t, frame0, frame1)
}

// TestToolStatusIcon_Completed verifies the completed icon is ✓.
func TestToolStatusIcon_Completed(t *testing.T) {
	assert.Equal(t, "✓", toolStatusIcon(ToolStatusCompleted, 0))
}

// TestToolStatusIcon_Error verifies the error icon is ✗.
func TestToolStatusIcon_Error(t *testing.T) {
	assert.Equal(t, "✗", toolStatusIcon(ToolStatusError, 0))
}

// TestAccordionRender_ToolStatusIcon verifies the accordion Render prepends
// the status icon to tool_call entries.
func TestAccordionRender_ToolStatusIcon(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{
		ContentType: ContentTypeToolCall,
		Summary:     "write(/tmp/test)",
		Full:        "write(/tmp/test)",
		Collapsed:   true,
		ToolStatus:  ToolStatusCompleted,
	})
	out := m.Render()
	assert.Contains(t, out, "✓")
	assert.Contains(t, out, "write(/tmp/test)")
}

// TestAccordionRender_ToolStatusRunning shows a spinner glyph.
func TestAccordionRender_ToolStatusRunning(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{
		ContentType:  ContentTypeToolCall,
		Summary:      "bash(ls)",
		Full:         "bash(ls)",
		Collapsed:    true,
		ToolStatus:   ToolStatusRunning,
		SpinnerFrame: 0,
	})
	out := m.Render()
	assert.Contains(t, out, "⠋")
}

// TestAccordionRender_ToolStatusError shows ✗ for error status.
func TestAccordionRender_ToolStatusError(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{
		ContentType: ContentTypeToolCall,
		Summary:     "bash(rm)",
		Full:        "bash(rm)",
		Collapsed:   true,
		ToolStatus:  ToolStatusError,
	})
	out := m.Render()
	assert.Contains(t, out, "✗")
}

// TestTruncateToolResult_Truncated verifies that tool results exceeding the
// limit are truncated with a "showing N of M lines" notice.
func TestTruncateToolResult_Truncated(t *testing.T) {
	lines := make([]string, 25)
	for i := range lines {
		lines[i] = "line " + string(rune('a'+i))
	}
	e := &AccordionEntry{
		ContentType: ContentTypeToolResult,
		Full:        strings.Join(lines, "\n"),
	}
	out := truncateToolResult(e)
	assert.Contains(t, out, "showing 20 of 25 lines")
	// The 21st line should NOT be present.
	assert.NotContains(t, out, "line u")
}

// TestTruncateToolResult_NotTruncated verifies short results are not truncated.
func TestTruncateToolResult_NotTruncated(t *testing.T) {
	e := &AccordionEntry{
		ContentType: ContentTypeToolResult,
		Full:        "line1\nline2\nline3",
	}
	out := truncateToolResult(e)
	assert.Equal(t, "line1\nline2\nline3", out)
	assert.NotContains(t, out, "showing")
}

// TestTruncateToolResult_Disabled verifies MaxResultLines=-1 disables truncation.
func TestTruncateToolResult_Disabled(t *testing.T) {
	lines := make([]string, 50)
	for i := range lines {
		lines[i] = "line"
	}
	e := &AccordionEntry{
		ContentType:    ContentTypeToolResult,
		Full:           strings.Join(lines, "\n"),
		MaxResultLines: -1,
	}
	out := truncateToolResult(e)
	assert.NotContains(t, out, "showing")
}

// TestTruncateToolResult_CustomLimit verifies a custom MaxResultLines is honored.
func TestTruncateToolResult_CustomLimit(t *testing.T) {
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "line"
	}
	e := &AccordionEntry{
		ContentType:    ContentTypeToolResult,
		Full:           strings.Join(lines, "\n"),
		MaxResultLines: 5,
	}
	out := truncateToolResult(e)
	assert.Contains(t, out, "showing 5 of 10 lines")
}

// TestTruncateToolResult_NonToolResult verifies non-tool_result entries are
// not truncated.
func TestTruncateToolResult_NonToolResult(t *testing.T) {
	e := &AccordionEntry{
		ContentType: ContentTypeAssistant,
		Full:        strings.Repeat("line\n", 50),
	}
	out := truncateToolResult(e)
	assert.NotContains(t, out, "showing")
}

// TestFinalizeThinking_AutoCollapse verifies that finalizeThinking collapses
// an expanded thinking entry and sets a duration summary.
func TestFinalizeThinking_AutoCollapse(t *testing.T) {
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
	}
	entry := &AccordionEntry{
		ContentType:   ContentTypeThinking,
		Summary:       "thinking...",
		Full:          "Let me consider the options...",
		Collapsed:     false,
		ToolStartTime: time.Now().Add(-100 * time.Millisecond),
	}
	m.accordion.Add(entry)

	m.finalizeThinking()

	assert.True(t, entry.Collapsed)
	assert.Contains(t, entry.Summary, "thought for")
	assert.Contains(t, entry.Summary, "s")
}

// TestFinalizeThinking_NoThinkingEntry verifies finalizeThinking is a no-op
// when there is no thinking entry.
func TestFinalizeThinking_NoThinkingEntry(t *testing.T) {
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
	}
	m.accordion.Add(&AccordionEntry{
		ContentType: ContentTypeAssistant,
		Summary:     "hello",
		Full:        "hello",
		Collapsed:   false,
	})
	// Should not panic and should not modify the entry.
	m.finalizeThinking()
}

// TestFinalizeThinking_AlreadyCollapsed verifies finalizeThinking skips
// entries that are already collapsed.
func TestFinalizeThinking_AlreadyCollapsed(t *testing.T) {
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
	}
	entry := &AccordionEntry{
		ContentType:   ContentTypeThinking,
		Summary:       "already done",
		Full:          "done thinking",
		Collapsed:     true,
		ToolStartTime: time.Now().Add(-100 * time.Millisecond),
	}
	m.accordion.Add(entry)

	m.finalizeThinking()

	// Should remain collapsed with the original summary.
	assert.True(t, entry.Collapsed)
	assert.Equal(t, "already done", entry.Summary)
}

// TestIsToolError verifies the structured error detection via the IsError
// marker on tool_result events, replacing the former string-matching heuristic.
func TestIsToolError(t *testing.T) {
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
	}

	// Add a running tool_call entry.
	m.accordion.Add(&AccordionEntry{
		ContentType:   ContentTypeToolCall,
		Summary:       "bash(test)",
		Full:          "bash(test)",
		ToolStatus:    ToolStatusRunning,
		ToolStartTime: time.Now(),
	})

	// isError=true marks the result as an error.
	m.updateToolResultStatusLocked("", true)
	entries := m.accordion.Entries()
	assert.Equal(t, ToolStatusError, entries[0].ToolStatus)

	// Reset to running for the next case.
	entries[0].ToolStatus = ToolStatusRunning

	// isError=false marks the result as completed.
	m.updateToolResultStatusLocked("", false)
	assert.Equal(t, ToolStatusCompleted, entries[0].ToolStatus)
}

// TestFindByToolCallID verifies that FindByToolCallID returns the correct
// entry by ToolCallID, and returns nil for empty/unknown IDs.
func TestFindByToolCallID(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{ContentType: ContentTypeToolCall, ToolCallID: "tc-1", Summary: "tool A"})
	m.Add(&AccordionEntry{ContentType: ContentTypeToolCall, ToolCallID: "tc-2", Summary: "tool B"})
	m.Add(&AccordionEntry{ContentType: ContentTypeThinking, Summary: "thinking"})

	// Exact match.
	found := m.FindByToolCallID("tc-2")
	assert.NotNil(t, found)
	assert.Equal(t, "tool B", found.Summary)

	// Non-tool_call entry type is ignored even if ToolCallID matches.
	m.Add(&AccordionEntry{ContentType: ContentTypeThinking, ToolCallID: "tc-3"})
	assert.Nil(t, m.FindByToolCallID("tc-3"))

	// Unknown ID.
	assert.Nil(t, m.FindByToolCallID("nonexistent"))

	// Empty ID returns nil.
	assert.Nil(t, m.FindByToolCallID(""))
}

// TestParallelToolResultGroupingByToolCallID verifies that when tool_results
// arrive out of order (as in parallel mode), they are correctly grouped under
// the originating tool_call entry by ToolCallID, not by position.
func TestParallelToolResultGroupingByToolCallID(t *testing.T) {
	m := NewAccordionModel()
	// Two tool_call entries issued in parallel.
	m.Add(&AccordionEntry{ContentType: ContentTypeToolCall, ToolCallID: "tc-A", Summary: "tool A"})
	m.Add(&AccordionEntry{ContentType: ContentTypeToolCall, ToolCallID: "tc-B", Summary: "tool B"})

	// tool_result for B arrives first (out-of-order completion).
	m.Add(&AccordionEntry{ContentType: ContentTypeToolResult, ToolCallID: "tc-B", Summary: "result B"})

	// tool_result for A arrives second.
	m.Add(&AccordionEntry{ContentType: ContentTypeToolResult, ToolCallID: "tc-A", Summary: "result A"})

	entries := m.Entries()
	assert.Len(t, entries, 2, "should have 2 top-level tool_call entries")

	// Verify grouping: each tool_call should have its own result as a child.
	entryA := m.FindByToolCallID("tc-A")
	assert.NotNil(t, entryA)
	assert.Len(t, entryA.Children, 1)
	assert.Equal(t, "result A", entryA.Children[0].Summary)

	entryB := m.FindByToolCallID("tc-B")
	assert.NotNil(t, entryB)
	assert.Len(t, entryB.Children, 1)
	assert.Equal(t, "result B", entryB.Children[0].Summary)
}

// TestUpdateToolResultStatusByToolCallID verifies that
// updateToolResultStatusLocked matches by ToolCallID to update the correct
// tool_call entry, even when multiple tool_calls exist.
func TestUpdateToolResultStatusByToolCallID(t *testing.T) {
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
	}
	m.accordion.Add(&AccordionEntry{
		ContentType:   ContentTypeToolCall,
		ToolCallID:    "tc-A",
		Summary:       "tool A",
		ToolStatus:    ToolStatusRunning,
		ToolStartTime: time.Now(),
	})
	m.accordion.Add(&AccordionEntry{
		ContentType:   ContentTypeToolCall,
		ToolCallID:    "tc-B",
		Summary:       "tool B",
		ToolStatus:    ToolStatusRunning,
		ToolStartTime: time.Now(),
	})

	// Mark tc-A as completed while tc-B is still running.
	m.updateToolResultStatusLocked("tc-A", false)

	entryA := m.accordion.FindByToolCallID("tc-A")
	entryB := m.accordion.FindByToolCallID("tc-B")
	assert.Equal(t, ToolStatusCompleted, entryA.ToolStatus)
	assert.Equal(t, ToolStatusRunning, entryB.ToolStatus, "tc-B should still be running")

	// Now mark tc-B as error.
	m.updateToolResultStatusLocked("tc-B", true)
	assert.Equal(t, ToolStatusError, entryB.ToolStatus)
	assert.Equal(t, ToolStatusCompleted, entryA.ToolStatus, "tc-A should remain completed")
}

// TestThinkingVisibility_Hide verifies that thinking entries are skipped when
// thinkingVisibility is "hide".
func TestThinkingVisibility_Hide(t *testing.T) {
	m := &teaModel{
		accordion:          NewAccordionModel(),
		msgCh:              make(chan Msg, 1),
		reg:                NewDefaultRegistry(),
		thinkingVisibility: "hide",
	}
	ev := AgentEvent{
		Type:        "thinking",
		Content:     "Let me think...",
		ContentType: ContentTypeThinking,
	}
	cmd := m.handleEvent(ev)
	assert.Nil(t, cmd)
	// No entry should have been added.
	assert.Equal(t, 0, m.accordion.Len())
}

// TestThinkingVisibility_Collapse verifies that thinking entries are collapsed
// when thinkingVisibility is "collapse".
func TestThinkingVisibility_Collapse(t *testing.T) {
	m := &teaModel{
		accordion:          NewAccordionModel(),
		msgCh:              make(chan Msg, 1),
		reg:                NewDefaultRegistry(),
		thinkingVisibility: "collapse",
	}
	ev := AgentEvent{
		Type:        "thinking",
		Content:     "Let me think...",
		ContentType: ContentTypeThinking,
	}
	m.handleEvent(ev)
	assert.Equal(t, 1, m.accordion.Len())
	entries := m.accordion.Entries()
	assert.True(t, entries[0].Collapsed)
}

// TestThinkingVisibility_Show verifies thinking entries are expanded by default.
func TestThinkingVisibility_Show(t *testing.T) {
	m := &teaModel{
		accordion:          NewAccordionModel(),
		msgCh:              make(chan Msg, 1),
		reg:                NewDefaultRegistry(),
		thinkingVisibility: "show",
	}
	ev := AgentEvent{
		Type:        "thinking",
		Content:     "Let me think...",
		ContentType: ContentTypeThinking,
	}
	m.handleEvent(ev)
	assert.Equal(t, 1, m.accordion.Len())
	entries := m.accordion.Entries()
	assert.False(t, entries[0].Collapsed)
}
