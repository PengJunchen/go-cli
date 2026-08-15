package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// TodoItem represents a single todo/task item in the persistent progress panel.
type TodoItem struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// TodoUpdateMsg is a tea.Msg that carries an updated todo list snapshot. It is
// pushed via Send to refresh the progress panel without going through the event
// stream.
type TodoUpdateMsg struct {
	Items []TodoItem
}

var todoCheckboxStyle = lipgloss.NewStyle().Bold(true)

// todoHeaderStyle renders the panel header including the progress summary.
var todoHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00D9FF"))

var todoDoneStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))

var todoCancelledStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B6B"))

// todoCheckbox returns the styled checkbox glyph for the given status.
func todoCheckbox(status string) string {
	switch status {
	case "completed":
		return todoCheckboxStyle.Render("☑")
	case "in_progress":
		return todoCheckboxStyle.Render("◐")
	case "cancelled":
		return todoCheckboxStyle.Render("⊘")
	default:
		return todoCheckboxStyle.Render("☐")
	}
}

// parseTodoItems decodes a JSON array of todo items from the event content.
// An empty string yields nil (no items).
func parseTodoItems(content string) ([]TodoItem, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, nil
	}
	var items []TodoItem
	if err := json.Unmarshal([]byte(content), &items); err != nil {
		return nil, err
	}
	return items, nil
}

// renderTodoPanelLocked builds the persistent todo progress panel. The caller
// must hold m.mu. Returns an empty string when there are no todos.
func (m *teaModel) renderTodoPanelLocked() string {
	if len(m.todoSnapshot) == 0 {
		return ""
	}
	completed := 0
	for _, item := range m.todoSnapshot {
		if item.Status == "completed" {
			completed++
		}
	}
	total := len(m.todoSnapshot)
	var lines []string
	header := fmt.Sprintf("Todos [%d/%d completed]", completed, total)
	lines = append(lines, todoHeaderStyle.Render(header))
	for _, item := range m.todoSnapshot {
		checkbox := todoCheckbox(item.Status)
		text := item.Content
		switch item.Status {
		case "completed":
			text = todoDoneStyle.Render(text)
		case "cancelled":
			text = todoCancelledStyle.Render(text)
		}
		lines = append(lines, fmt.Sprintf("  %s %s", checkbox, text))
	}
	return strings.Join(lines, "\n")
}
