package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// ToolStatus represents the lifecycle state of a tool_call entry.
type ToolStatus int

const (
	// ToolStatusPending means the tool call has been sent but execution has
	// not started yet. Rendered as ⏳.
	ToolStatusPending ToolStatus = iota
	// ToolStatusRunning means the tool is currently executing. Rendered as a
	// spinner glyph (⠋).
	ToolStatusRunning
	// ToolStatusCompleted means the tool finished successfully. Rendered as ✓.
	ToolStatusCompleted
	// ToolStatusError means the tool returned an error. Rendered as ✗.
	ToolStatusError
)

// toolStatusIcon returns the glyph for a tool status. Running uses the entry's
// spinner frame to produce an animated effect.
func toolStatusIcon(status ToolStatus, spinnerFrame int) string {
	switch status {
	case ToolStatusPending:
		return "⏳"
	case ToolStatusRunning:
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		return frames[spinnerFrame%len(frames)]
	case ToolStatusCompleted:
		return "✓"
	case ToolStatusError:
		return "✗"
	default:
		return " "
	}
}

// maxResultLines is the default number of lines to show for a tool result
// before truncating with a "showing N of M lines" notice.
const maxResultLines = 20

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
	// (false) is displayed. tool_call and tool_result entries default to
	// collapsed; thinking entries default to expanded (auto-collapsed on
	// completion); assistant/user messages default to expanded.
	Collapsed bool
	// Timestamp records when the entry was created (for debug/logging).
	Timestamp time.Time
	// Children holds nested entries for grouped content (e.g. a tool_call
	// followed by its tool_result). When non-empty the parent is rendered as
	// a group header and children are shown indented beneath it when
	// expanded.
	Children []*AccordionEntry
	// ToolStatus is the lifecycle state of a tool_call entry. Only meaningful
	// for ContentTypeToolCall entries; other entry types leave it at zero.
	ToolStatus ToolStatus
	// ToolStartTime records when the tool call began (for duration display).
	ToolStartTime time.Time
	// ToolDuration is the total execution time, set when the tool completes.
	ToolDuration time.Duration
	// SpinnerFrame is the current animation frame for a running tool entry.
	// Advanced by the global spinner tick.
	SpinnerFrame int
	// MaxResultLines caps the number of lines shown for a tool_result entry.
	// 0 means use the default (maxResultLines). Set to -1 to disable truncation.
	MaxResultLines int
	// ToolCallID is the unique identifier from the model's tool call. Used to
	// match tool_output and tool_result events to the originating tool_call
	// entry, replacing position-based matching.
	ToolCallID string
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

// Add appends a top-level entry and auto-selects it. If the new entry is a
// tool_result, it is grouped as a child of the originating tool_call entry
// (matched by ToolCallID, with position-based fallback) instead of becoming a
// new top-level item.
func (m *AccordionModel) Add(entry *AccordionEntry) {
	if entry == nil {
		return
	}
	// Group tool_result with the originating tool_call entry.
	if entry.ContentType == ContentTypeToolResult {
		// Match by ToolCallID first.
		if entry.ToolCallID != "" {
			if parent := m.FindByToolCallID(entry.ToolCallID); parent != nil {
				parent.Children = append(parent.Children, entry)
				m.selected = len(m.entries) - 1
				return
			}
		}
		// Fallback: group with the last tool_call entry (position-based).
		if len(m.entries) > 0 {
			last := m.entries[len(m.entries)-1]
			if last.ContentType == ContentTypeToolCall {
				last.Children = append(last.Children, entry)
				m.selected = len(m.entries) - 1
				return
			}
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

// FindByToolCallID returns the top-level tool_call entry whose ToolCallID
// matches id, or nil if no match is found. Only top-level entries are
// searched because tool_call entries are always added at the top level
// (tool_result entries become children).
func (m *AccordionModel) FindByToolCallID(id string) *AccordionEntry {
	if id == "" {
		return nil
	}
	for _, e := range m.entries {
		if e.ContentType == ContentTypeToolCall && e.ToolCallID == id {
			return e
		}
	}
	return nil
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
// based on its content type. tool_call and tool_result entries are collapsed
// by default; thinking entries are expanded by default (auto-collapsed to a
// duration summary when the next non-thinking event arrives).
func defaultCollapsed(contentType string) bool {
	switch contentType {
	case ContentTypeToolCall, ContentTypeToolResult:
		return true
	default:
		return false
	}
}

// Render produces the full terminal output for the accordion. Collapsed entries
// show only their summary; expanded entries show their full content plus any
// expanded children (indented). The selected entry is prefixed with "▶ " when
// collapsed or "▼ " when expanded; unselected entries are prefixed with "  ".
// Tool_call entries are prefixed with a status icon (⏳/⠋/✓/✗) when collapsed.
// Tool_result children are truncated to MaxResultLines lines (default 20).
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
		// Prepend a status icon for tool_call entries.
		statusPrefix := ""
		if e.ContentType == ContentTypeToolCall {
			statusPrefix = toolStatusIcon(e.ToolStatus, e.SpinnerFrame) + " "
		}
		if e.Collapsed {
			sb.WriteString(marker + statusPrefix + e.Summary + "\n")
		} else {
			sb.WriteString(marker + statusPrefix + e.Full + "\n")
			for _, child := range e.Children {
				cMarker := "  "
				if child.Collapsed {
					sb.WriteString(cMarker + "  " + child.Summary + "\n")
				} else {
					rendered := truncateToolResult(child)
					for _, line := range strings.Split(rendered, "\n") {
						sb.WriteString(cMarker + "  " + line + "\n")
					}
				}
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// truncateToolResult truncates a tool_result entry's Full content to
// MaxResultLines lines (default maxResultLines), appending a "showing N of M
// lines" notice when truncated. Set MaxResultLines to -1 to disable.
func truncateToolResult(e *AccordionEntry) string {
	if e.ContentType != ContentTypeToolResult {
		return e.Full
	}
	limit := e.MaxResultLines
	if limit == 0 {
		limit = maxResultLines
	}
	if limit < 0 {
		return e.Full
	}
	lines := strings.Split(e.Full, "\n")
	if len(lines) <= limit {
		return e.Full
	}
	shown := strings.Join(lines[:limit], "\n")
	return shown + fmt.Sprintf("\n... showing %d of %d lines", limit, len(lines))
}

// Len returns the number of top-level entries.
func (m *AccordionModel) Len() int { return len(m.entries) }

// ---------- collapsible tool call rendering ----------

// Package-level lipgloss styles for the collapsible tool call renderer. The
// tool name is painted cyan and the result payload gray.
var (
	toolNameStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#00D9FF"))
	toolResultStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
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
	sb.WriteString(toolNameStyle.Render("[tool] " + toolName))
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
	sb.WriteString(toolNameStyle.Render("[tool] " + toolName))
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
	sb.WriteString(toolResultStyle.Render("result:"))
	for _, line := range strings.Split(result, "\n") {
		sb.WriteString("\n    ")
		sb.WriteString(line)
	}
	return sb.String()
}
