package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

var (
	// ErrSendQueueFull identifies a rejected message because the bounded TUI
	// queue has no available capacity.
	ErrSendQueueFull = errors.New("tui: message queue is full")
	// ErrAppClosed identifies a rejected message after the app has been asked
	// to stop or the active run has ended.
	ErrAppClosed = errors.New("tui: app is closed")
	// ErrNilSendContext identifies a critical-send request without a context.
	ErrNilSendContext = errors.New("tui: critical send requires a context")
)

// SendQueueFullError reports a message rejected because the local TUI queue
// reached Capacity. It unwraps to ErrSendQueueFull for errors.Is checks.
type SendQueueFullError struct {
	Capacity int
}

// Error returns the queue-capacity failure description.
func (e *SendQueueFullError) Error() string {
	return fmt.Sprintf("%s (capacity %d)", ErrSendQueueFull, e.Capacity)
}

// Unwrap returns the queue-full sentinel.
func (e *SendQueueFullError) Unwrap() error { return ErrSendQueueFull }

// AppClosedError reports a message rejected because the TUI app is stopping.
// It unwraps to ErrAppClosed for errors.Is checks.
type AppClosedError struct{}

// Error returns the app-closed description.
func (AppClosedError) Error() string { return ErrAppClosed.Error() }

// Unwrap returns the app-closed sentinel.
func (AppClosedError) Unwrap() error { return ErrAppClosed }

// Send enqueues a message for the render loop without blocking. Messages may
// be queued before Run starts, but a full queue or stopped app is reported to
// the caller and counted as a local TUI delivery gap.
func (a *BubbleteaApp) Send(msg Msg) error {
	a.sendMu.Lock()
	if a.quitRequested || runHasEnded(a.activeRunDone) {
		a.sendMu.Unlock()
		return a.recordSendGap(&AppClosedError{})
	}
	select {
	case a.model.msgCh <- msg:
		a.sendMu.Unlock()
		return nil
	default:
		a.sendMu.Unlock()
		return a.recordSendGap(&SendQueueFullError{Capacity: cap(a.model.msgCh)})
	}
}

// CriticalSendContext enqueues a critical message and waits for queue space
// while a Run is active. Before Run starts, a full queue is returned
// immediately because no consumer can make progress. Context cancellation,
// Quit, and completion of the active Run each return an inspectable error.
func (a *BubbleteaApp) CriticalSendContext(ctx context.Context, msg Msg) error {
	if ctx == nil {
		return ErrNilSendContext
	}

	var waitRunDone <-chan struct{}
	for {
		if err := ctx.Err(); err != nil {
			return a.recordSendGap(fmt.Errorf("tui: critical send canceled: %w", err))
		}
		a.sendMu.Lock()
		if err := ctx.Err(); err != nil {
			a.sendMu.Unlock()
			return a.recordSendGap(fmt.Errorf("tui: critical send canceled: %w", err))
		}
		if a.quitRequested {
			a.sendMu.Unlock()
			return a.recordSendGap(&AppClosedError{})
		}
		if waitRunDone == nil && runHasEnded(a.activeRunDone) {
			a.sendMu.Unlock()
			return a.recordSendGap(&AppClosedError{})
		}
		if waitRunDone != nil && runHasEnded(waitRunDone) {
			a.sendMu.Unlock()
			return a.recordSendGap(&AppClosedError{})
		}
		select {
		case a.model.msgCh <- msg:
			a.sendMu.Unlock()
			return nil
		default:
		}
		if waitRunDone == nil {
			if a.activeRunDone == nil || !a.running.Load() {
				a.sendMu.Unlock()
				return a.recordSendGap(&SendQueueFullError{Capacity: cap(a.model.msgCh)})
			}
			waitRunDone = a.activeRunDone
		}
		a.sendMu.Unlock()

		select {
		case <-ctx.Done():
			return a.recordSendGap(fmt.Errorf("tui: critical send canceled: %w", ctx.Err()))
		case <-waitRunDone:
			return a.recordSendGap(&AppClosedError{})
		case <-a.quitCh:
			return a.recordSendGap(&AppClosedError{})
		case <-a.queueSpace:
		}
	}
}

func (a *BubbleteaApp) recordSendGap(err error) error {
	count := a.model.localSendGapCount.Add(1)
	slog.Warn("tui.app.send_gap", "local_gap_count", count, "error", err)
	return err
}

func runHasEnded(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}
