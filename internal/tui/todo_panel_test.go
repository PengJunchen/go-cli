package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTodoCheckboxStates verifies each status maps to the correct glyph.
func TestTodoCheckboxStates(t *testing.T) {
	assert.Contains(t, todoCheckbox("pending"), "☐")
	assert.Contains(t, todoCheckbox("in_progress"), "◐")
	assert.Contains(t, todoCheckbox("completed"), "☑")
	assert.Contains(t, todoCheckbox("canceled"), "⊘")
	// Unknown status falls back to the pending checkbox.
	assert.Contains(t, todoCheckbox("unknown"), "☐")
	assert.Contains(t, todoCheckbox(""), "☐")
}

// TestRenderTodoPanelEmpty verifies the panel is hidden when no todos exist.
func TestRenderTodoPanelEmpty(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.mu.Lock()
	view := m.renderTodoPanelLocked()
	m.mu.Unlock()
	assert.Equal(t, "", view, "empty snapshot should produce no panel")
}

// TestRenderTodoPanelWithItems verifies the panel renders header and items.
func TestRenderTodoPanelWithItems(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.mu.Lock()
	m.todoSnapshot = []TodoItem{
		{ID: "1", Content: "Write tests", Status: "completed"},
		{ID: "2", Content: "Ship feature", Status: "in_progress"},
		{ID: "3", Content: "Refactor", Status: "pending"},
	}
	view := m.renderTodoPanelLocked()
	m.mu.Unlock()

	stripped := stripEscape(view)
	assert.Contains(t, stripped, "Todos [1/3 completed]")
	assert.Contains(t, stripped, "☑")
	assert.Contains(t, stripped, "Write tests")
	assert.Contains(t, stripped, "◐")
	assert.Contains(t, stripped, "Ship feature")
	assert.Contains(t, stripped, "☐")
	assert.Contains(t, stripped, "Refactor")
}

// TestTodoUpdateMsgViaUpdate verifies TodoUpdateMsg updates the snapshot and
// view when delivered through Update.
func TestTodoUpdateMsgViaUpdate(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.Update(TodoUpdateMsg{Items: []TodoItem{
		{ID: "1", Content: "Task A", Status: "pending"},
	}})

	view := stripEscape(m.View())
	assert.Contains(t, view, "Todos [0/1 completed]")
	assert.Contains(t, view, "Task A")
}

// TestTodoSnapshotReplacement verifies a second update replaces the old list.
func TestTodoSnapshotReplacement(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.Update(TodoUpdateMsg{Items: []TodoItem{
		{ID: "1", Content: "Old task", Status: "pending"},
	}})
	m.Update(TodoUpdateMsg{Items: []TodoItem{
		{ID: "2", Content: "New task", Status: "completed"},
	}})

	view := stripEscape(m.View())
	assert.NotContains(t, view, "Old task")
	assert.Contains(t, view, "New task")
	assert.Contains(t, view, "Todos [1/1 completed]")
}

// TestContentTypeTodoEvent verifies an AgentEvent with ContentTypeTodo updates
// the snapshot via the event stream path.
func TestContentTypeTodoEvent(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	items := []TodoItem{
		{ID: "1", Content: "Event task", Status: "in_progress"},
		{ID: "2", Content: "Done task", Status: "completed"},
	}
	content, err := json.Marshal(items)
	require.NoError(t, err)

	m.Update(agentEventMsg{event: AgentEvent{
		ContentType: ContentTypeTodo,
		Content:     string(content),
	}})

	view := stripEscape(m.View())
	assert.Contains(t, view, "Todos [1/2 completed]")
	assert.Contains(t, view, "Event task")
	assert.Contains(t, view, "Done task")
}

// TestContentTypeTodoInvalidJSON verifies invalid JSON does not panic and
// leaves the existing snapshot intact.
func TestContentTypeTodoInvalidJSON(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	// Seed an existing snapshot.
	m.Update(TodoUpdateMsg{Items: []TodoItem{
		{ID: "1", Content: "Existing", Status: "pending"},
	}})

	// Send invalid JSON via event stream.
	m.Update(agentEventMsg{event: AgentEvent{
		ContentType: ContentTypeTodo,
		Content:     "{not valid json",
	}})

	// The old snapshot should still be present.
	view := stripEscape(m.View())
	assert.Contains(t, view, "Existing")
}

// TestTodoPanelPositionAboveStatusBar verifies the todo panel appears before
// the status bar in the full view.
func TestTodoPanelPositionAboveStatusBar(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.mu.Lock()
	m.modelName = "test-model"
	m.tokenMax = 1000
	m.tokenCost = 0.5
	m.todoSnapshot = []TodoItem{
		{ID: "1", Content: "Positional check", Status: "pending"},
	}
	view := m.renderViewLocked()
	m.mu.Unlock()

	stripped := stripEscape(view)
	todoIdx := strings.Index(stripped, "Todos [")
	// If the status bar is present, the todo panel must come before it.
	statusIdx := strings.Index(stripped, "test-model")
	if statusIdx >= 0 {
		assert.Greater(t, statusIdx, todoIdx, "todo panel should appear above status bar")
	}
	assert.GreaterOrEqual(t, todoIdx, 0, "todo panel should be rendered")
}

// TestTodoPanelAllCompleted verifies progress shows full completion.
func TestTodoPanelAllCompleted(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.Update(TodoUpdateMsg{Items: []TodoItem{
		{ID: "1", Content: "A", Status: "completed"},
		{ID: "2", Content: "B", Status: "completed"},
		{ID: "3", Content: "C", Status: "completed"},
	}})

	view := stripEscape(m.View())
	assert.Contains(t, view, "Todos [3/3 completed]")
}

// TestTodoPanelCanceledNotCompleted verifies cancelled items are not counted
// as completed.
func TestTodoPanelCanceledNotCompleted(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.Update(TodoUpdateMsg{Items: []TodoItem{
		{ID: "1", Content: "Done", Status: "completed"},
		{ID: "2", Content: "Canceled", Status: "canceled"},
		{ID: "3", Content: "Pending", Status: "pending"},
	}})

	view := stripEscape(m.View())
	assert.Contains(t, view, "Todos [1/3 completed]")
	assert.Contains(t, view, "⊘")
	assert.Contains(t, view, "Canceled")
}

// TestParseTodoItemsEmpty verifies empty content returns nil.
func TestParseTodoItemsEmpty(t *testing.T) {
	items, err := parseTodoItems("")
	require.NoError(t, err)
	assert.Nil(t, items)

	items, err = parseTodoItems("   \n\t  ")
	require.NoError(t, err)
	assert.Nil(t, items)
}

// TestParseTodoItemsValid verifies valid JSON is parsed correctly.
func TestParseTodoItemsValid(t *testing.T) {
	input := `[{"id":"1","content":"test","status":"pending","priority":"high"}]`
	items, err := parseTodoItems(input)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "1", items[0].ID)
	assert.Equal(t, "test", items[0].Content)
	assert.Equal(t, "pending", items[0].Status)
	assert.Equal(t, "high", items[0].Priority)
}

// TestParseTodoItemsInvalid verifies invalid JSON returns an error.
func TestParseTodoItemsInvalid(t *testing.T) {
	_, err := parseTodoItems("{invalid")
	assert.Error(t, err)
}
