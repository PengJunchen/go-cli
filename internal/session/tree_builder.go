package session

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// EntryLister is an optional interface implemented by stores that can enumerate
// all stored entries. The tree builder uses it to reconstruct a SessionTree from
// persistent storage.
type EntryLister interface {
	List(ctx context.Context) ([]*SessionEntry, error)
}

// SessionTreeBuilder builds a SessionTree from a SessionStore.
type SessionTreeBuilder interface {
	BuildFromStore(ctx context.Context, store SessionStore) (SessionTree, error)
}

// DefaultSessionTreeBuilder implements SessionTreeBuilder by reading all entries
// from the store and linking them via ParentID.
type DefaultSessionTreeBuilder struct{}

var _ SessionTreeBuilder = (*DefaultSessionTreeBuilder)(nil)

// NewDefaultSessionTreeBuilder returns a DefaultSessionTreeBuilder.
func NewDefaultSessionTreeBuilder() *DefaultSessionTreeBuilder {
	return &DefaultSessionTreeBuilder{}
}

// BuildFromStore reads all entries from the store and builds a SessionTree by
// linking entries via ParentID. The current leaf is set to the entry with the
// latest timestamp (i.e. the most recently appended entry). If the store is
// empty or the backing file does not exist, an empty tree is returned.
// Corrupted or orphaned entries are skipped so the rest of the tree is still
// rebuilt.
func (b *DefaultSessionTreeBuilder) BuildFromStore(ctx context.Context, store SessionStore) (SessionTree, error) {
	tree := NewDefaultSessionTree()
	if store == nil {
		return tree, nil
	}

	lister, ok := store.(EntryLister)
	if !ok {
		return tree, fmt.Errorf("session: store %T does not implement EntryLister", store)
	}

	entries, err := lister.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("session: list entries from store: %w", err)
	}

	if len(entries) == 0 {
		return tree, nil
	}

	// Build a lookup map, skipping invalid entries.
	entryMap := make(map[string]*SessionEntry, len(entries))
	for _, e := range entries {
		if e == nil || e.ID == "" || e.Type == "" {
			continue
		}
		if _, exists := entryMap[e.ID]; !exists {
			entryMap[e.ID] = e
		}
	}

	if len(entryMap) == 0 {
		return tree, nil
	}

	// Topologically order entries so parents are appended before children.
	// This guarantees tree.Append (which validates parent existence) succeeds
	// for well-formed data even when timestamps are identical.
	ordered := topoSortEntries(entryMap)

	var latestID string
	var latestTs time.Time

	for _, e := range ordered {
		if err := tree.Append(ctx, e); err != nil {
			// Skip entries that cannot be appended (e.g. parent missing due to
			// corruption). Continue building the rest of the tree.
			continue
		}
		if e.Timestamp.After(latestTs) {
			latestTs = e.Timestamp
			latestID = e.ID
		}
	}

	// Set the current leaf to the most recently appended entry.
	if latestID != "" {
		if err := tree.MoveTo(ctx, latestID); err != nil {
			return nil, fmt.Errorf("session: set current leaf %q: %w", latestID, err)
		}
	}

	return tree, nil
}

// topoSortEntries returns entries ordered so that every parent appears before
// its children. Within that constraint, entries are ordered by timestamp for
// deterministic output. The function does not recurse more deeply than the
// number of entries (guarded by the visited set).
func topoSortEntries(entryMap map[string]*SessionEntry) []*SessionEntry {
	var result []*SessionEntry
	visited := make(map[string]bool)

	// Collect and sort IDs for deterministic traversal order.
	ids := make([]string, 0, len(entryMap))
	for id := range entryMap {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var visit func(id string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		e := entryMap[id]
		if e.ParentID != "" {
			if _, ok := entryMap[e.ParentID]; ok {
				visit(e.ParentID)
			}
		}
		result = append(result, e)
	}

	for _, id := range ids {
		visit(id)
	}
	return result
}
