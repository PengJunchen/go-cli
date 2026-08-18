package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccordionAddAndSelect(t *testing.T) {
	m := NewAccordionModel()
	require.Equal(t, -1, m.Selected())

	m.Add(&AccordionEntry{ContentType: ContentTypeAssistant, Summary: "hello", Full: "hello"})
	m.Add(&AccordionEntry{ContentType: ContentTypeToolCall, Summary: "[tool] bash", Full: "[tool] bash ls"})
	m.Add(&AccordionEntry{ContentType: ContentTypeAssistant, Summary: "done", Full: "done"})

	assert.Equal(t, 3, m.Len())
	assert.Equal(t, 2, m.Selected())

	m.Select(-1)
	assert.Equal(t, 1, m.Selected())

	m.Select(-5) // clamp to 0
	assert.Equal(t, 0, m.Selected())

	m.Select(10) // clamp to 2
	assert.Equal(t, 2, m.Selected())
}

func TestAccordionToggle(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{ContentType: ContentTypeToolCall, Collapsed: true, Summary: "s", Full: "f"})

	m.Toggle()
	assert.False(t, m.Entries()[0].Collapsed)

	m.Toggle()
	assert.True(t, m.Entries()[0].Collapsed)
}

func TestAccordionToolCallResultGrouping(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{ContentType: ContentTypeToolCall, Summary: "[tool] ls", Full: "[tool] ls"})
	m.Add(&AccordionEntry{ContentType: ContentTypeToolResult, Summary: "[result] file.go", Full: "[result] file.go\nmain.go"})

	// tool_result should be grouped as a child of tool_call, not a new entry.
	assert.Equal(t, 1, m.Len())
	entries := m.Entries()
	require.Len(t, entries[0].Children, 1)
	assert.Equal(t, ContentTypeToolResult, entries[0].Children[0].ContentType)
}

func TestAccordionRenderCollapsed(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{
		ContentType: ContentTypeToolCall,
		Summary:     "[tool] bash",
		Full:        "[tool] bash ls -la\nlong output",
		Collapsed:   true,
	})

	out := m.Render()
	assert.Contains(t, out, "[tool] bash")
	assert.NotContains(t, out, "long output")
}

func TestAccordionRenderExpanded(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{
		ContentType: ContentTypeToolCall,
		Summary:     "[tool] bash",
		Full:        "[tool] bash ls -la\nlong output",
		Collapsed:   false,
	})

	out := m.Render()
	assert.Contains(t, out, "[tool] bash ls -la")
	assert.Contains(t, out, "long output")
}

func TestAccordionRenderGroupExpanded(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{
		ContentType: ContentTypeToolCall,
		Summary:     "[tool] ls",
		Full:        "[tool] ls",
		Collapsed:   false,
	})
	m.Add(&AccordionEntry{
		ContentType: ContentTypeToolResult,
		Summary:     "[result] file.go",
		Full:        "[result] file.go\nmain.go",
		Collapsed:   false,
	})

	out := m.Render()
	assert.Contains(t, out, "[tool] ls")
	assert.Contains(t, out, "[result] file.go")
	assert.Contains(t, out, "main.go")
}

func TestAccordionExpandAllCollapseAll(t *testing.T) {
	m := NewAccordionModel()
	m.Add(&AccordionEntry{ContentType: ContentTypeToolCall, Collapsed: true, Summary: "s1", Full: "f1"})
	m.Add(&AccordionEntry{ContentType: ContentTypeThinking, Collapsed: true, Summary: "s2", Full: "f2"})

	m.ExpandAll()
	for _, e := range m.Entries() {
		assert.False(t, e.Collapsed)
	}

	m.CollapseAll()
	for _, e := range m.Entries() {
		assert.True(t, e.Collapsed)
	}
}

func TestAccordionRenderEmpty(t *testing.T) {
	m := NewAccordionModel()
	assert.Equal(t, "", m.Render())
}

func TestDefaultCollapsed(t *testing.T) {
	assert.True(t, defaultCollapsed(ContentTypeToolCall))
	assert.True(t, defaultCollapsed(ContentTypeToolResult))
	// Thinking entries default to expanded; they are auto-collapsed to a
	// duration summary when the next non-thinking event arrives.
	assert.False(t, defaultCollapsed(ContentTypeThinking))
	assert.False(t, defaultCollapsed(ContentTypeAssistant))
	assert.False(t, defaultCollapsed(ContentTypeUser))
	assert.False(t, defaultCollapsed(ContentTypeStatus))
}

func TestSummarizeFirstLine(t *testing.T) {
	assert.Equal(t, "hello", summarizeFirstLine("hello", 80))
	assert.Equal(t, "hello", summarizeFirstLine("hello\nworld", 80))
	assert.Equal(t, "wor…", summarizeFirstLine("world", 3))
	assert.Equal(t, "…", summarizeFirstLine("", 80))
}

func TestStripANSIPlain(t *testing.T) {
	// \x1b[31mred\x1b[0m → "red"
	assert.Equal(t, "red", stripANSIPlain("\x1b[31mred\x1b[0m"))
	assert.Equal(t, "plain text", stripANSIPlain("plain text"))
}
