package tui

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
	// Revision zero is the legacy unversioned form. Such updates are accepted,
	// but cannot participate in stale-message ordering.
	Revision uint64
	Items    []TodoItem
}
