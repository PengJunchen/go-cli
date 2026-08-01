package approval

import (
	"context"
	"log/slog"
	"sync"
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
	mu        sync.RWMutex
	decisions map[string]Classification
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
	s.mu.Lock()
	defer s.mu.Unlock()
	slog.Info("approval.store.set", "key", key, "classification", classification.String())
	s.decisions[key] = classification
	return nil
}

// Delete removes the classification under the given key.
func (s *InMemoryApprovalStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	slog.Info("approval.store.delete", "key", key)
	delete(s.decisions, key)
	return nil
}
