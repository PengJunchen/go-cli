package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/session"
)

// escSequenceTimeout is the window within which a following byte must arrive
// after an ESC (0x1B) for the sequence to be treated as a CSI escape rather
// than a standalone Esc keypress.
const escSequenceTimeout = 50 * time.Millisecond

// InterruptHandler monitors for user interrupt signals during agent execution.
// In non-TTY mode (the default for test/CI environments) it catches SIGINT
// (Ctrl+C) and cancels the in-progress turn. In TTY mode, Esc-key detection
// is handled by the TUI keyboardLoop (keyboard.go) which calls
// cancelCallback -> cancelTurn. The monitorEsc implementation in this handler
// provides an alternative Esc detection path for non-TUI scenarios; it is
// activated when Start is called with a non-nil io.Reader.
//
// Steering messages are enqueued into a session.SubmissionQueue and drained
// into steerCh by a background goroutine. This decouples the producer
// (SendSteer, called from the TUI keyboard loop) from the consumer (the REPL
// select loop), allowing multiple steer messages to be buffered without loss.
type InterruptHandler struct {
	cancelFn  context.CancelFunc
	queue     session.SubmissionQueue
	steerCh   chan string
	notifyCh  chan struct{}
	done      chan struct{}
	drainCh   chan struct{}
	stopCh    chan struct{}
	stopOnce  sync.Once
	escDone   chan struct{}
	escActive bool
}

// NewInterruptHandler creates a new InterruptHandler bound to the given cancel
// function. When an interrupt signal is received, cancelFn is invoked to cancel
// the in-progress agent turn.
func NewInterruptHandler(cancelFn context.CancelFunc) *InterruptHandler {
	return &InterruptHandler{
		cancelFn: cancelFn,
		queue:    session.NewDefaultSubmissionQueue(),
		steerCh:  make(chan string, 16),
		notifyCh: make(chan struct{}, 1),
		done:     make(chan struct{}),
		drainCh:  make(chan struct{}),
		stopCh:   make(chan struct{}),
	}
}

// Start begins monitoring for interrupt input in a goroutine. It always
// catches SIGINT via signal.Notify and drains the SubmissionQueue into
// steerCh. When r is non-nil it also launches a monitorEsc goroutine that
// watches for standalone Esc keypresses and cancels the current turn.
// In production TTY mode, nil is passed because the TUI keyboardLoop
// already handles Esc detection via its own 50ms timeout logic.
func (h *InterruptHandler) Start(r io.Reader) {
	go h.monitor()
	go h.drainQueue()
	if r != nil {
		h.escDone = make(chan struct{})
		h.escActive = true
		go h.monitorEsc(r)
	}
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

// drainQueue runs in its own goroutine. It waits for notifications from
// notifyCh (sent by SendSteer) and drains all pending items from the
// SubmissionQueue into steerCh. It exits when stopCh is closed.
func (h *InterruptHandler) drainQueue() {
	defer close(h.drainCh)
	for {
		select {
		case <-h.stopCh:
			return
		case <-h.notifyCh:
			// Drain all pending steering messages from the queue.
			for {
				item, ok := h.queue.Dequeue(session.QueueSteering)
				if !ok {
					break
				}
				select {
				case h.steerCh <- item.Content:
				case <-h.stopCh:
					return
				}
			}
		}
	}
}

// Stop stops monitoring and cleans up the signal handler. It blocks until the
// monitor, drain, and (if active) Esc-monitor goroutines have fully exited,
// preventing goroutine leaks. Safe to call multiple times.
func (h *InterruptHandler) Stop() {
	h.stopOnce.Do(func() {
		close(h.stopCh)
		<-h.done
		<-h.drainCh
		if h.escActive {
			<-h.escDone
		}
	})
}

// monitorEsc reads bytes from r and watches for standalone Esc keypresses
// (0x1B). Terminal escape sequences (e.g. arrow keys) begin with ESC followed
// by additional bytes that arrive quickly; a standalone Esc has no following
// bytes within escSequenceTimeout. When a standalone Esc is detected,
// cancelFn is invoked to cancel the in-progress turn.
//
// A persistent reader goroutine feeds bytes into a channel so the main loop
// can always select on stopCh, allowing Stop to interrupt the monitor even
// when blocked waiting for input.
func (h *InterruptHandler) monitorEsc(r io.Reader) {
	defer close(h.escDone)

	type readResult struct {
		b   byte
		err error
	}
	readCh := make(chan readResult, 1)
	done := make(chan struct{})

	// Persistent reader goroutine: reads bytes one at a time and sends them
	// to readCh. Exits when the reader returns an error (e.g. EOF) or when
	// done is closed (monitorEsc is returning).
	go func() {
		for {
			b := make([]byte, 1)
			n, err := r.Read(b)
			if n > 0 {
				select {
				case readCh <- readResult{b[0], nil}:
				case <-done:
					return
				}
			}
			if err != nil {
				select {
				case readCh <- readResult{0, err}:
				case <-done:
					return
				}
				return
			}
		}
	}()

	defer close(done)

	for {
		select {
		case res := <-readCh:
			if res.err != nil {
				return
			}
			if res.b != 0x1B {
				continue
			}
			// Got ESC. Wait briefly to distinguish standalone Esc from a
			// CSI sequence. If no more bytes arrive within
			// escSequenceTimeout, it is a standalone Esc.
			timer := time.NewTimer(escSequenceTimeout)
			select {
			case res := <-readCh:
				timer.Stop()
				if res.err != nil {
					return
				}
				// Another byte arrived quickly - it's a CSI sequence (e.g.
				// ESC [ A = up arrow). Consume and continue.
			case <-timer.C:
				// Timeout - standalone Esc, trigger cancel.
				h.cancelFn()
			case <-h.stopCh:
				timer.Stop()
				return
			}
		case <-h.stopCh:
			return
		}
	}
}

// SteerChannel returns a receive-only channel for steer messages. The REPL
// select loop receives steer messages from this channel and forwards them to
// the TurnRunner via Steer().
func (h *InterruptHandler) SteerChannel() <-chan string {
	return h.steerCh
}

// SendSteer enqueues a steering instruction into the SubmissionQueue. A
// notification is sent to drainQueue so the message is forwarded to steerCh.
// This is non-blocking: if the queue is full the message is still enqueued
// (the queue is unbounded), and the notification is best-effort (buffered
// cap 1, dropped if already pending - which is fine because drainQueue drains
// all items on each notification).
func (h *InterruptHandler) SendSteer(msg string) error {
	if err := h.queue.Enqueue(session.QueueSteering, session.QueuedSubmission{
		Content: msg,
	}); err != nil {
		return err
	}
	select {
	case h.notifyCh <- struct{}{}:
	default:
	}
	return nil
}

// QueueLen returns the number of pending steering messages in the queue.
func (h *InterruptHandler) QueueLen() int {
	return h.queue.Len(session.QueueSteering)
}
