package memory

import "context"

// ScopedStore gives manual CRUD the same workspace attribution as automatic
// publication. Legacy unattributed records remain visible for explicit review,
// while prompt injection selects only positively scoped records.
type ScopedStore struct {
	Store       *FileMemoryStore
	WorkspaceID string
}

var _ MemoryStore = (*ScopedStore)(nil)

// Add attributes a manually entered fact to this workspace.
func (s *ScopedStore) Add(ctx context.Context, item Memory) error {
	item.WorkspaceID = s.WorkspaceID
	return s.Store.Add(ctx, item)
}

// Get returns a fact visible to this workspace or an unattributed legacy fact.
func (s *ScopedStore) Get(ctx context.Context, id string) (Memory, error) {
	item, err := s.Store.Get(ctx, id)
	if err == nil && item.WorkspaceID != "" && item.WorkspaceID != s.WorkspaceID {
		return Memory{}, ErrMemoryNotFound
	}
	return item, err
}

// List includes scoped facts and legacy records available for manual review.
func (s *ScopedStore) List(ctx context.Context) ([]Memory, error) {
	items, err := s.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	var result []Memory
	for _, item := range items {
		if item.WorkspaceID == "" || item.WorkspaceID == s.WorkspaceID {
			result = append(result, item)
		}
	}
	return result, nil
}

// Delete removes only a fact visible through this scoped interface.
func (s *ScopedStore) Delete(ctx context.Context, id string) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	return s.Store.Delete(ctx, id)
}

// Search filters ranked results to this workspace and legacy manual records.
func (s *ScopedStore) Search(ctx context.Context, query string, limit int) ([]Memory, error) {
	if limit <= 0 {
		return nil, nil
	}
	items, err := s.Store.Search(ctx, query, 100)
	if err != nil {
		return nil, err
	}
	var result []Memory
	for _, item := range items {
		if item.WorkspaceID == "" || item.WorkspaceID == s.WorkspaceID {
			result = append(result, item)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

// Close closes the underlying owned file handle.
func (s *ScopedStore) Close() error { return s.Store.Close() }
