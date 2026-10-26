//go:build headless

package cli

import (
	"context"
	"fmt"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tui"
)

// replApp is the event consumer for the portable build. The same REPL owns
// agent execution, approval policy, session commits, and cancellation. This
// consumer retains only the current durable snapshot and todo state; it never
// accumulates streamed terminal output.
type replApp struct {
	events     <-chan tui.AgentEvent
	durable    bool
	ready      chan struct{}
	done       chan struct{}
	sourceDone chan struct{}
	quit       chan struct{}
	quitOnce   sync.Once
	mu         sync.Mutex
	uiDone     <-chan struct{}
	snapshot   *tui.CanonicalTranscriptSnapshot
	todo       tui.TodoUpdateMsg
}

func newREPLThemeManager() ThemeController { return nil }

// Headless approval uses the normal synchronous approval middleware rather
// than starting a modal interaction that has no terminal renderer.
func interactiveApprovalsEnabled() bool { return false }

func (s *REPLSession) renderTUI(initial *tui.CanonicalTranscriptSnapshot) error {
	app := &replApp{
		durable: s.assembly.durableSession != nil,
		ready:   make(chan struct{}), done: make(chan struct{}),
		sourceDone: make(chan struct{}), quit: make(chan struct{}),
	}
	if initial != nil {
		if err := initial.Validate(); err != nil {
			return fmt.Errorf("interactive: validate headless transcript: %w", err)
		}
		snapshot := initial.Clone()
		app.snapshot = &snapshot
	}
	s.eventSource = tui.BridgeEventsOwned(s.turnCtx, s.stream)
	app.events = s.eventSource.Events()
	s.turnCounter++
	s.isTTY = false // The shared REPL prints the completed assistant response.
	s.uiCtx, s.cancelUI = context.WithCancel(s.spanCtx)
	s.app, s.appStarted = app, true
	go func() {
		if err := app.Run(s.uiCtx); err != nil && err != context.Canceled {
			s.logger.Debug("cli_interactive_headless_error", "err", err)
		}
	}()
	return nil
}

func (a *replApp) Run(ctx context.Context) error {
	a.mu.Lock()
	a.uiDone = ctx.Done()
	a.mu.Unlock()
	close(a.ready)
	defer close(a.done)
	defer func() {
		select {
		case <-a.sourceDone:
		default:
			close(a.sourceDone)
		}
	}()
	for {
		select {
		case _, ok := <-a.events:
			if !ok {
				close(a.sourceDone)
				if !a.durable {
					return nil
				}
				// Keep accepting the final canonical refresh until the REPL
				// has joined the run owner and installed that publication.
				select {
				case <-a.quit:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		case <-a.quit:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *replApp) Done() <-chan struct{}            { return a.done }
func (a *replApp) EventSourceDone() <-chan struct{} { return a.sourceDone }
func (a *replApp) Quit()                            { a.quitOnce.Do(func() { close(a.quit) }) }

func (a *replApp) ApplyTranscriptRefresh(ctx context.Context, refresh tui.TranscriptRefresh) error {
	if ctx == nil {
		return context.Canceled
	}
	select {
	case <-a.ready:
	case <-ctx.Done():
		return ctx.Err()
	case <-a.done:
		return tui.ErrTranscriptRefreshClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.uiDone == nil || ctx.Done() != a.uiDone {
		return fmt.Errorf("interactive: transcript refresh requires the active UI context")
	}
	select {
	case <-a.quit:
		return tui.ErrTranscriptRefreshClosed
	case <-a.done:
		return tui.ErrTranscriptRefreshClosed
	default:
	}
	var sessionID, branchID string
	if a.snapshot != nil {
		sessionID, branchID = a.snapshot.PhysicalSessionID, a.snapshot.BranchID
	}
	if err := tui.ValidateTranscriptRefresh(a.snapshot != nil, sessionID, branchID, "", "", refresh); err != nil {
		return err
	}
	snapshot := refresh.Snapshot.Clone()
	a.snapshot = &snapshot
	return nil // ACK follows installation in the headless consumer.
}

func (a *replApp) CriticalSendContext(ctx context.Context, message tui.Msg) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-a.quit:
		return tui.ErrTranscriptRefreshClosed
	case <-a.done:
		return tui.ErrTranscriptRefreshClosed
	default:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if update, ok := message.(tui.TodoUpdateMsg); ok {
		if update.Revision >= a.todo.Revision {
			a.todo = update
			a.todo.Items = append([]tui.TodoItem(nil), update.Items...)
		}
		return nil
	}
	return fmt.Errorf("interactive: unsupported headless control message %T", message)
}
