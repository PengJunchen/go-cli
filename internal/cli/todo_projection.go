package cli

import (
	"sync"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
)

type assemblyTodoState struct {
	store   *tools.TodoStore
	mu      sync.RWMutex
	handler tools.TodoWriteChangeHandler
}

func newAssemblyTodoState(store *tools.TodoStore) *assemblyTodoState {
	return &assemblyTodoState{store: store}
}

func (s *assemblyTodoState) snapshot() tools.TodoSnapshot {
	if s == nil || s.store == nil {
		return tools.TodoSnapshot{}
	}
	return s.store.Snapshot()
}

func (s *assemblyTodoState) setHandler(handler tools.TodoWriteChangeHandler) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.handler = handler
	s.mu.Unlock()
}

func (s *assemblyTodoState) notify(snapshot tools.TodoSnapshot) error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	handler := s.handler
	s.mu.RUnlock()
	if handler == nil {
		return nil
	}
	return handler(snapshot)
}

func (a *AgentAssembly) todoSnapshot() (tools.TodoSnapshot, bool) {
	if a == nil || a.todoState == nil {
		return tools.TodoSnapshot{}, false
	}
	return a.todoState.snapshot(), true
}

func (a *AgentAssembly) bindTodoUpdateHandler(handler tools.TodoWriteChangeHandler) {
	if a != nil && a.todoState != nil {
		a.todoState.setHandler(handler)
	}
}

func (a *AgentAssembly) unbindTodoUpdateHandler() {
	a.bindTodoUpdateHandler(nil)
}

func todoUpdateMsg(snapshot tools.TodoSnapshot) tui.TodoUpdateMsg {
	items := make([]tui.TodoItem, len(snapshot.Items))
	for i, item := range snapshot.Items {
		items[i] = tui.TodoItem{
			ID:       item.ID,
			Content:  item.Content,
			Status:   item.Status,
			Priority: item.Priority,
		}
	}
	return tui.TodoUpdateMsg{Revision: snapshot.Revision, Items: items}
}

func (s *REPLSession) bindTodoUpdates() {
	if s == nil || s.assembly == nil || s.app == nil || s.uiCtx == nil {
		return
	}
	app := s.app
	uiCtx := s.uiCtx
	handler := func(snapshot tools.TodoSnapshot) error {
		return app.CriticalSendContext(uiCtx, todoUpdateMsg(snapshot))
	}
	s.assembly.bindTodoUpdateHandler(handler)
	// renderTUI seeds the app before this handler is installed. Re-read after
	// binding so a mutation in that interval is not lost; revision checks make
	// this safe if a concurrent callback has already delivered a newer snapshot.
	if snapshot, ok := s.assembly.todoSnapshot(); ok {
		if err := handler(snapshot); err != nil && s.logger != nil {
			s.logger.Warn("cli_todo_initial_snapshot_send_failed", "err", err)
		}
	}
}

func (s *REPLSession) unbindTodoUpdates() {
	if s != nil && s.assembly != nil {
		s.assembly.unbindTodoUpdateHandler()
	}
}
