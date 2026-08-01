// Package session provides the conversational session storage layer: an
// append-only entry store, a branching session tree, and JSONL persistence. It
// implements the richer Phase 2 session interfaces (SessionStore and
// SessionTree) that build on top of the core stubs.
package session

import (
	"context"
	"time"
)

// EntryType categorizes a single immutable session entry.
type EntryType string

const (
	// EntryTypeUser is a message authored by the human user.
	EntryTypeUser EntryType = "user"
	// EntryTypeAssistant is a message produced by the assistant.
	EntryTypeAssistant EntryType = "assistant"
	// EntryTypeTool is a tool invocation or its result.
	EntryTypeTool EntryType = "tool"
	// EntryTypeCompaction is a summarized replacement for older history.
	EntryTypeCompaction EntryType = "compaction"
	// EntryTypeSystem is a system-level informational entry.
	EntryTypeSystem EntryType = "system"
)

// SessionEntry is a single immutable record in a session. Entries are linked
// into a tree via ParentID and are never mutated once appended.
type SessionEntry struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Type      EntryType `json:"type"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
	Summary   string    `json:"summary,omitempty"`
}

// clone returns a defensive copy of the entry so callers cannot mutate the
// stored immutable record through the returned pointer.
func (e *SessionEntry) clone() *SessionEntry {
	if e == nil {
		return nil
	}
	cp := *e
	return &cp
}

// SessionContext is the effective, replayable context for a session branch.
type SessionContext struct {
	LeafID     string         `json:"leaf_id"`
	Messages   []SessionEntry `json:"messages"`
	EntryCount int            `json:"entry_count"`
	LastUpdate time.Time      `json:"last_update,omitempty"`
}

// SessionStore persists and loads immutable session entries.
type SessionStore interface {
	// Append inserts an immutable entry without overwriting an existing id.
	Append(ctx context.Context, entry *SessionEntry) error
	// Get returns a defensive copy of the entry with the given id.
	Get(ctx context.Context, id string) (*SessionEntry, error)
	// Save flushes any buffered state (no-op for in-memory stores).
	Save(ctx context.Context) error
}

// SessionTree manages the append-only branching structure of a session.
type SessionTree interface {
	// Append adds an immutable entry to the tree.
	Append(ctx context.Context, entry *SessionEntry) error
	// MoveTo sets the current leaf pointer to the given entry id.
	MoveTo(ctx context.Context, leafID string) error
	// GetBranch returns the ordered entries from root to leafID.
	GetBranch(ctx context.Context, leafID string) ([]*SessionEntry, error)
	// BuildContext reconstructs the effective context for a branch.
	BuildContext(ctx context.Context, leafID string) (*SessionContext, error)
	// CurrentLeaf returns the id of the current leaf, or "" when empty.
	CurrentLeaf() string
}
