package approval

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// ApprovalStore records and queries approval decisions keyed by a stable
// decision key (typically "tool_name:args_hash"). It provides the cross-session
// aggregation of decisions so repeated identical calls are decided once.
type ApprovalStore interface {
	// Get returns the stored classification and whether it was found.
	Get(ctx context.Context, key string) (Classification, bool, error)
	// Set stores a classification under the given key.
	Set(ctx context.Context, key string, classification Classification) error
	// Delete removes the classification under the given key.
	Delete(ctx context.Context, key string) error
}

// InMemoryApprovalStore is an in-memory, concurrency-safe ApprovalStore. It
// persists decisions for the lifetime of the process, giving cross-session
// (in-process) caching without external dependencies.
type InMemoryApprovalStore struct {
	mu          sync.RWMutex
	decisions   map[string]Classification
	fence       *tools.DispatchFence
	policyEpoch uint64
}

var _ ApprovalStore = (*InMemoryApprovalStore)(nil)

// NewInMemoryApprovalStore creates an empty in-memory approval store.
func NewInMemoryApprovalStore() *InMemoryApprovalStore {
	return &InMemoryApprovalStore{decisions: make(map[string]Classification)}
}

// Get returns the stored classification and whether it was found.
func (s *InMemoryApprovalStore) Get(_ context.Context, key string) (Classification, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	val, ok := s.decisions[key]
	return val, ok, nil
}

// Set stores a classification under the given key.
func (s *InMemoryApprovalStore) Set(_ context.Context, key string, classification Classification) error {
	update := func() {
		s.decisions[key] = classification
		if classification != Allow {
			s.policyEpoch++
		}
	}
	return s.publishUpdate("approval.store.set", classification == Allow, update)
}

// Delete removes the classification under the given key.
func (s *InMemoryApprovalStore) Delete(_ context.Context, key string) error {
	update := func() {
		delete(s.decisions, key)
		s.policyEpoch++
	}
	return s.publishUpdate("approval.store.delete", false, update)
}

// setPreparedGrant publishes a positive prepared-path decision only against
// the generation snapshot that produced it. It deliberately does not rebase
// on the current fence: an intervening revoke or policy replacement makes the
// original approval obsolete.
func (s *InMemoryApprovalStore) setPreparedGrant(expected tools.DispatchFenceSnapshot, key string) error {
	s.mu.RLock()
	fence := s.fence
	s.mu.RUnlock()
	if fence == nil {
		return tools.ErrDispatchFenceUnavailable
	}
	_, err := publishApprovalGrant(fence, expected, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.decisions[key] = Allow
	})
	if err != nil {
		return err
	}
	slog.Info("approval.store.set")
	return nil
}

func (s *InMemoryApprovalStore) bindFence(fence *tools.DispatchFence) error {
	if fence == nil {
		return tools.ErrDispatchFenceUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fence != nil && s.fence != fence {
		return errors.New("approval: store is bound to a different dispatch fence")
	}
	s.fence = fence
	return nil
}

func (s *InMemoryApprovalStore) publishUpdate(event string, monotonicGrant bool, update func()) error {
	s.mu.Lock()
	fence := s.fence
	if fence == nil {
		update()
		s.mu.Unlock()
		slog.Info(event)
		return nil
	}
	s.mu.Unlock()

	if monotonicGrant {
		if _, err := publishApprovalGrant(fence, fence.Snapshot(), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			update()
		}); err != nil {
			return err
		}
	} else if _, err := fence.Publish(tools.DispatchFenceApproval, nil, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		update()
	}); err != nil {
		return err
	}
	slog.Info(event)
	return nil
}

func (s *InMemoryApprovalStore) policyEpochValue() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.policyEpoch
}
