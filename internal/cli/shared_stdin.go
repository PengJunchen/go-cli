package cli

import (
	"io"
	"sync"
	"sync/atomic"
)

// SharedStdin multiplexes a single io.Reader to multiple subscribers. Each
// subscriber receives a copy of every byte read from the underlying reader.
// This allows multiple consumers (e.g., the Esc-key monitor and the TUI
// keyboard loop) to share a single stdin stream without byte stealing.
//
// The read loop reads batches from the underlying reader and distributes each
// batch to all active subscribers via buffered channels. Subscribers read from
// their channel via SharedStdinReader.Read, which returns as many bytes as
// available in a single call, enabling bufio.Reader.Buffered() to work
// correctly for Esc-sequence disambiguation.
type SharedStdin struct {
	mu          sync.Mutex
	subscribers map[uint64]*stdinSubscriber
	nextID      uint64
	stopCh      chan struct{}
	stopped     atomic.Bool
}

type stdinSubscriber struct {
	ch     chan []byte
	closed bool
}

// NewSharedStdin creates a SharedStdin that reads from r. The read loop runs
// in a background goroutine until Stop is called or r returns an error.
func NewSharedStdin(r io.Reader) *SharedStdin {
	s := &SharedStdin{
		subscribers: make(map[uint64]*stdinSubscriber),
		stopCh:      make(chan struct{}),
	}
	go s.readLoop(r)
	return s
}

// Subscribe returns a new io.Reader that receives a copy of every byte from
// the underlying reader. The caller should call Close on the returned reader
// when done, though Stop on the SharedStdin also closes all subscribers.
func (s *SharedStdin) Subscribe() *SharedStdinReader {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	sub := &stdinSubscriber{ch: make(chan []byte, 64)}
	s.subscribers[id] = sub
	return &SharedStdinReader{parent: s, id: id, sub: sub}
}

// Stop shuts down the read loop and closes all subscriber channels. It is
// safe to call multiple times.
func (s *SharedStdin) Stop() {
	if s.stopped.CompareAndSwap(false, true) {
		close(s.stopCh)
		s.mu.Lock()
		for _, sub := range s.subscribers {
			if !sub.closed {
				close(sub.ch)
				sub.closed = true
			}
		}
		s.subscribers = make(map[uint64]*stdinSubscriber)
		s.mu.Unlock()
	}
}

func (s *SharedStdin) readLoop(r io.Reader) {
	buf := make([]byte, 256)
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		n, err := r.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			s.mu.Lock()
			for _, sub := range s.subscribers {
				if !sub.closed {
					select {
					case sub.ch <- data:
					default:
						// subscriber buffer full, drop batch
					}
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			s.Stop()
			return
		}
	}
}

// SharedStdinReader is a subscriber to SharedStdin. It implements io.Reader.
// Read blocks until at least one byte is available, then returns as many
// bytes as possible (up to len(p)) from the current batch. This allows
// callers using bufio.Reader to detect whether follow-up bytes are already
// buffered after reading a leading byte (e.g., for Esc-sequence timing).
type SharedStdinReader struct {
	parent   *SharedStdin
	id       uint64
	sub      *stdinSubscriber
	leftover []byte
}

// Read implements io.Reader. It blocks until at least one byte is available
// or the subscription is closed (returns io.EOF).
func (r *SharedStdinReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.leftover) > 0 {
		n := copy(p, r.leftover)
		r.leftover = r.leftover[n:]
		return n, nil
	}
	batch, ok := <-r.sub.ch
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, batch)
	if n < len(batch) {
		r.leftover = batch[n:]
	}
	return n, nil
}

// Close unsubscribes from the SharedStdin.
func (r *SharedStdinReader) Close() error {
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	if sub, ok := r.parent.subscribers[r.id]; ok && !sub.closed {
		close(sub.ch)
		sub.closed = true
		delete(r.parent.subscribers, r.id)
	}
	return nil
}
