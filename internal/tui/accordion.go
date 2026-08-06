package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// AccordionEntry is a single renderable item in the accordion view. Each entry
// tracks its own collapsed state and carries both a one-line summary (shown
// when collapsed) and the full rendered content (shown when expanded).
type AccordionEntry struct {
	// ContentType is the renderer content type of the source event.
	ContentType string
	// Summary is the single-line text shown when collapsed.
	Summary string
	// Full is the complete rendered text shown when expanded.
	Full string
	// Collapsed controls whether only the summary (true) or the full content
	// (false) is displayed. tool_call, tool_result and thinking entries
	// default to collapsed; assistant/user messages default to expanded.
	Collapsed bool
	// Timestamp records when the entry was created (for debug/logging).
	Timestamp time.Time
	// Children holds nested entries for grouped content (e.g. a tool_call
	// followed by its tool_result). When non-empty the parent is rendered as
	// a group header and children are shown indented beneath it when
	// expanded.
	Children []*AccordionEntry
}

// AccordionModel holds the ordered list of entries and the index of the
// currently selected (highlighted) entry. It is the backing store for the
// interactive accordion view.
type AccordionModel struct {
	entries  []*AccordionEntry
	selected int
}

// NewAccordionModel returns an empty accordion model.
func NewAccordionModel() *AccordionModel {
	return &AccordionModel{selected: -1}
}

// Add appends a top-level entry and auto-selects it. If the previous entry is
// a tool_call and the new entry is a tool_result, they are grouped: the
// tool_result becomes a child of the tool_call entry instead of a new
// top-level item.
func (m *AccordionModel) Add(entry *AccordionEntry) {
	if entry == nil {
		return
	}
	// Group tool_result with the preceding tool_call.
	if len(m.entries) > 0 && entry.ContentType == ContentTypeToolResult {
		last := m.entries[len(m.entries)-1]
		if last.ContentType == ContentTypeToolCall {
			last.Children = append(last.Children, entry)
			m.selected = len(m.entries) - 1
			return
		}
	}
	m.entries = append(m.entries, entry)
	m.selected = len(m.entries) - 1
}

// Entries returns a snapshot of the top-level entries.
func (m *AccordionModel) Entries() []*AccordionEntry {
	out := make([]*AccordionEntry, len(m.entries))
	copy(out, m.entries)
	return out
}

// Selected returns the index of the currently selected entry, or -1 if empty.
func (m *AccordionModel) Selected() int { return m.selected }

// Select moves the selection by delta, clamping to valid bounds.
func (m *AccordionModel) Select(delta int) {
	if len(m.entries) == 0 {
		m.selected = -1
		return
	}
	m.selected += delta
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(m.entries) {
		m.selected = len(m.entries) - 1
	}
}

// Toggle collapses or expands the selected entry.
func (m *AccordionModel) Toggle() {
	if m.selected < 0 || m.selected >= len(m.entries) {
		return
	}
	m.entries[m.selected].Collapsed = !m.entries[m.selected].Collapsed
}

// ExpandAll expands every entry (including children).
func (m *AccordionModel) ExpandAll() {
	for _, e := range m.entries {
		e.Collapsed = false
		for _, c := range e.Children {
			c.Collapsed = false
		}
	}
}

// CollapseAll collapses every entry (including children).
func (m *AccordionModel) CollapseAll() {
	for _, e := range m.entries {
		e.Collapsed = true
		for _, c := range e.Children {
			c.Collapsed = true
		}
	}
}

// defaultCollapsed reports whether an entry should be collapsed by default
// based on its content type.
func defaultCollapsed(contentType string) bool {
	switch contentType {
	case ContentTypeToolCall, ContentTypeToolResult, ContentTypeThinking:
		return true
	default:
		return false
	}
}

// Render produces the full terminal output for the accordion. Collapsed entries
// show only their summary; expanded entries show their full content plus any
// expanded children (indented). The selected entry is prefixed with "▶ " when
// collapsed or "▼ " when expanded; unselected entries are prefixed with "  ".
func (m *AccordionModel) Render() string {
	if len(m.entries) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, e := range m.entries {
		marker := "  "
		if i == m.selected {
			if e.Collapsed {
				marker = "▶ "
			} else {
				marker = "▼ "
			}
		}
		if e.Collapsed {
			sb.WriteString(marker + e.Summary + "\n")
		} else {
			sb.WriteString(marker + e.Full + "\n")
			for _, child := range e.Children {
				cMarker := "  "
				if child.Collapsed {
					sb.WriteString(cMarker + "  " + child.Summary + "\n")
				} else {
					for _, line := range strings.Split(child.Full, "\n") {
						sb.WriteString(cMarker + "  " + line + "\n")
					}
				}
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Len returns the number of top-level entries.
func (m *AccordionModel) Len() int { return len(m.entries) }

// ---------- collapsible tool call rendering ----------

// ANSI escape sequences for the collapsible tool call renderer.
const (
	tcCyan  = "\033[36m"
	tcGray  = "\033[90m"
	tcReset = "\033[0m"
)

// sortedArgKeys returns the keys of args in sorted order for deterministic
// output.
func sortedArgKeys(args map[string]any) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RenderCollapsed renders a tool call as a single-line summary:
// [tool] tool_name(arg1=val1, arg2=val2). The tool name is colored cyan.
func (r *ToolCallRenderer) RenderCollapsed(toolName string, args map[string]any) string {
	var sb strings.Builder
	sb.WriteString(tcCyan)
	sb.WriteString("[tool] ")
	sb.WriteString(toolName)
	sb.WriteString(tcReset)
	sb.WriteString("(")
	keys := sortedArgKeys(args)
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(fmt.Sprintf("%v", args[k]))
	}
	sb.WriteString(")")
	return sb.String()
}

// RenderExpanded renders a tool call with full args and result across multiple
// lines. The tool name is colored cyan and the result is colored gray.
func (r *ToolCallRenderer) RenderExpanded(toolName string, args map[string]any, result string) string {
	var sb strings.Builder
	sb.WriteString(tcCyan)
	sb.WriteString("[tool] ")
	sb.WriteString(toolName)
	sb.WriteString(tcReset)
	sb.WriteString("\n  args:")
	keys := sortedArgKeys(args)
	if len(keys) == 0 {
		sb.WriteString(" (none)")
	} else {
		for _, k := range keys {
			sb.WriteString("\n    ")
			sb.WriteString(k)
			sb.WriteString(": ")
			sb.WriteString(fmt.Sprintf("%v", args[k]))
		}
	}
	sb.WriteString("\n  ")
	sb.WriteString(tcGray)
	sb.WriteString("result:")
	for _, line := range strings.Split(result, "\n") {
		sb.WriteString("\n    ")
		sb.WriteString(line)
	}
	sb.WriteString(tcReset)
	return sb.String()
}
