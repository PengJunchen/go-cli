package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
)

type assemblyTodoState struct {
	store   *tools.TodoStore
	mu      sync.RWMutex
	handler func(context.Context, tools.TodoSnapshot) error
	cancel  context.CancelFunc
	done    chan struct{}
}

func newAssemblyTodoState(ctx context.Context, store *tools.TodoStore) *assemblyTodoState {
	observerCtx, cancel := context.WithCancel(ctx)
	state := &assemblyTodoState{store: store, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(state.done)
		for {
			select {
			case <-observerCtx.Done():
				return
			case snapshot := <-store.Updates():
				_ = state.notify(observerCtx, snapshot)
			}
		}
	}()
	return state
}

func (s *assemblyTodoState) close(ctx context.Context) error {
	s.cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("todo observer close pending: %w", ctx.Err())
	}
}

func (s *assemblyTodoState) snapshot() tools.TodoSnapshot {
	if s == nil || s.store == nil {
		return tools.TodoSnapshot{}
	}
	return s.store.Snapshot()
}

func (s *assemblyTodoState) setHandler(handler func(context.Context, tools.TodoSnapshot) error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.handler = handler
	s.mu.Unlock()
}

func (s *assemblyTodoState) notify(ctx context.Context, snapshot tools.TodoSnapshot) error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	handler := s.handler
	s.mu.RUnlock()
	if handler == nil {
		return nil
	}
	return handler(ctx, snapshot)
}

func (a *AgentAssembly) todoSnapshot() (tools.TodoSnapshot, bool) {
	if a == nil || a.todoState == nil {
		return tools.TodoSnapshot{}, false
	}
	return a.todoState.snapshot(), true
}

func (a *AgentAssembly) bindTodoUpdateHandler(handler func(context.Context, tools.TodoSnapshot) error) {
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
	handler := func(ctx context.Context, snapshot tools.TodoSnapshot) error {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		stop := context.AfterFunc(uiCtx, cancel)
		defer stop()
		return app.CriticalSendContext(bounded, todoUpdateMsg(snapshot))
	}
	s.assembly.bindTodoUpdateHandler(handler)
	// renderTUI seeds the app before this handler is installed. Re-read after
	// binding so a mutation in that interval is not lost; revision checks make
	// this safe if a concurrent callback has already delivered a newer snapshot.
	if snapshot, ok := s.assembly.todoSnapshot(); ok {
		if err := handler(uiCtx, snapshot); err != nil && s.logger != nil {
			s.logger.Warn("cli_todo_initial_snapshot_send_failed", "err", err)
		}
	}
}

func (s *REPLSession) unbindTodoUpdates() {
	if s != nil && s.assembly != nil {
		s.assembly.unbindTodoUpdateHandler()
	}
}
