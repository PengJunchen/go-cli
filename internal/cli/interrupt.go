package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
)

// InterruptHandler monitors for user interrupt signals during agent execution.
// In non-TTY mode (the default for test/CI environments) it catches SIGINT
// (Ctrl+C) and cancels the in-progress turn. In TTY mode it would monitor
// stdin for the Esc key (future work).
type InterruptHandler struct {
	cancelFn context.CancelFunc
	steerCh  chan string
	done     chan struct{}
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewInterruptHandler creates a new InterruptHandler bound to the given cancel
// function. When an interrupt signal is received, cancelFn is invoked to cancel
// the in-progress agent turn.
func NewInterruptHandler(cancelFn context.CancelFunc) *InterruptHandler {
	return &InterruptHandler{
		cancelFn: cancelFn,
		steerCh:  make(chan string, 1),
		done:     make(chan struct{}),
		stopCh:   make(chan struct{}),
	}
}

// Start begins monitoring for interrupt input in a goroutine. In non-TTY mode
// it catches SIGINT via signal.Notify. The stdin parameter is reserved for
// TTY-mode Esc-key monitoring (future work) and is currently unused.
func (h *InterruptHandler) Start(_ io.Reader) {
	go h.monitor()
}

// monitor runs in its own goroutine. It registers for SIGINT and waits for
// either an interrupt signal or a stop request. On SIGINT it calls cancelFn
// to cancel the current turn. The signal handler is always cleaned up on exit.
func (h *InterruptHandler) monitor() {
	defer close(h.done)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	select {
	case <-sigCh:
		slog.Info("cli_interrupt_received", "op", "cli.interrupt.signal")
		h.cancelFn()
	case <-h.stopCh:
	}
}

// Stop stops monitoring and cleans up the signal handler. It blocks until the
// monitor goroutine has fully exited, preventing goroutine leaks. Safe to call
// multiple times.
func (h *InterruptHandler) Stop() {
	h.stopOnce.Do(func() {
		close(h.stopCh)
		<-h.done
	})
}

// SteerChannel returns a receive-only channel for steer messages. In TTY mode
// (future work) the monitor goroutine would send steering instructions here
// when the user presses Esc and types a new instruction; the REPL loop selects
// on this channel during turn execution to inject the steer.
func (h *InterruptHandler) SteerChannel() <-chan string {
	return h.steerCh
}
