package capability

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrCloseInProgress prevents a finalizer from waiting for itself.
var ErrCloseInProgress = errors.New("capability: scope close in progress")

// ErrScopeClosed rejects new owners after shutdown starts.
var ErrScopeClosed = errors.New("capability: scope is closing")

type finalizer struct {
	id    string
	close func(context.Context) error
}

// Scope owns resources and closes each registered resource once in reverse order.
type Scope struct {
	mu         sync.Mutex
	finalizers []finalizer
	state      uint8
	done       chan struct{}
	err        error
}

// NewScope creates an open resource owner.
func NewScope() *Scope { return &Scope{done: make(chan struct{})} }

// Defer registers ownership before starting the resource's initialization.
func (s *Scope) Defer(id string, closeResource func(context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		s.done = make(chan struct{})
	}
	if s.state != 0 {
		return ErrScopeClosed
	}
	if id == "" || closeResource == nil {
		return errors.New("capability: invalid resource finalizer")
	}
	for _, f := range s.finalizers {
		if f.id == id {
			return fmt.Errorf("capability: duplicate owner %q", id)
		}
	}
	s.finalizers = append(s.finalizers, finalizer{id, closeResource})
	return nil
}

// Close starts cleanup and waits within ctx. Reentrant/concurrent calls return immediately.
func (s *Scope) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.done == nil {
		s.done = make(chan struct{})
	}
	if s.state == 2 {
		err := s.err
		s.mu.Unlock()
		return err
	}
	if s.state == 1 {
		s.mu.Unlock()
		return ErrCloseInProgress
	}
	s.state = 1
	owners := s.finalizers
	s.finalizers = nil
	s.mu.Unlock()
	go func() {
		var errs []error
		for i := len(owners) - 1; i >= 0; i-- {
			if err := closeOwner(owners[i]); err != nil {
				errs = append(errs, err)
			}
		}
		s.mu.Lock()
		s.err = errors.Join(errs...)
		s.state = 2
		close(s.done)
		s.mu.Unlock()
	}()
	return s.WaitClosed(ctx)
}

// WaitClosed waits without invoking finalizers and is safe outside finalizer callbacks.
func (s *Scope) WaitClosed(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.done == nil {
		s.done = make(chan struct{})
	}
	done := s.done
	s.mu.Unlock()
	select {
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func closeOwner(f finalizer) (err error) {
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("capability: owner %q finalizer panicked", f.id)
		}
	}()
	if err = f.close(context.Background()); err != nil {
		return fmt.Errorf("capability: close owner %q: %w", f.id, err)
	}
	return nil
}
